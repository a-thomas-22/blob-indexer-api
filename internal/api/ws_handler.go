package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/gorilla/websocket"
	"github.com/lib/pq"
	"go.uber.org/zap"

	"github.com/a-thomas-22/blob-indexer-api/internal/config"
	"github.com/a-thomas-22/blob-indexer-api/internal/db/models"
	"github.com/a-thomas-22/blob-indexer-api/internal/logger"
)

// blockSnapshotDepth is how many recent blocks a newly connected client
// receives in its block_snapshot event. It covers reconnect gaps of up to
// ~2 minutes of blocks; longer gaps are healed by the client refetching its
// REST baselines on reconnect.
const blockSnapshotDepth = 10

// wsCheckOrigin returns a websocket.Upgrader CheckOrigin func that enforces the
// same allowed-origins policy as the REST CORS layer. Requests without an
// Origin header (non-browser clients such as native apps or server-to-server
// tooling) are always allowed, matching browser semantics where same-origin and
// non-browser requests omit the header. When CORS is disabled or
// allow_all_origins is configured, the policy is permissive.
func wsCheckOrigin(policy corsPolicy) func(*http.Request) bool {
	return func(r *http.Request) bool {
		origin := strings.TrimSpace(r.Header.Get("Origin"))
		if origin == "" {
			return true
		}
		if !policy.enabled || policy.allowAllOrigins {
			return true
		}
		return policy.isOriginAllowed(origin)
	}
}

// HandleWebSocket upgrades an HTTP connection to WebSocket and registers the
// client with the hub. The network is selected via the ?network= query param,
// same as all other API endpoints.
//
// Before allocating any per-client goroutines or buffers, it enforces the
// configured cross-origin policy and the global / per-IP connection caps,
// rejecting abusive or over-cap upgrades up front.
func (a *API) HandleWebSocket(w http.ResponseWriter, r *http.Request) {
	if a.hub == nil {
		a.respondError(w, http.StatusServiceUnavailable, "WebSocket not available")
		return
	}

	network, err := a.getNetworkFromRequest(r)
	if err != nil {
		a.respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Reserve a connection slot before upgrading so over-cap clients are
	// rejected without allocating goroutines or buffers.
	remoteIP := a.clientIPs.IP(r)
	if !a.hub.admit(remoteIP, a.config.WebSocket.MaxClients, a.config.WebSocket.MaxConnsPerIP) {
		logger.Warn("WebSocket connection rejected: capacity reached",
			zap.String("network", network.Name),
			zap.String("remote_ip", remoteIP))
		a.respondError(w, http.StatusTooManyRequests, "WebSocket connection limit reached")
		return
	}

	upgrader := websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
		CheckOrigin:     wsCheckOrigin(a.wsOriginPolicy),
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		// Upgrade already wrote an error response (e.g. 403 on a disallowed
		// Origin). Release the reserved slot since no client was created.
		a.hub.release(remoteIP)
		logger.Error("WebSocket upgrade failed",
			zap.String("network", network.Name),
			zap.Error(err))
		return
	}

	client := &Client{
		hub:            a.hub,
		conn:           conn,
		send:           make(chan []byte, wsSendBufferSize),
		networkChainID: network.ChainID,
		networkName:    network.Name,
		remoteIP:       remoteIP,
		registered:     make(chan struct{}),
	}

	select {
	case a.hub.register <- client:
		go client.writePump()
		go client.readPump()
		// Send the recent-blocks snapshot once registration completes (the
		// hub closes client.registered) so any block broadcast while the
		// snapshot is being built is also delivered — the client
		// deduplicates by block number, so overlap is harmless while a gap
		// would not be.
		go a.sendBlockSnapshot(client, network)
	case <-a.hub.done:
		// Hub is shutting down: release the slot and drop the connection. The
		// client was never registered, so the hub will not release it for us.
		a.hub.release(remoteIP)
		logger.Error("WebSocket registration failed: hub is shutting down",
			zap.String("network", network.Name))
		_ = conn.Close()
	}
}

// buildBlockSnapshot assembles the most recent blocks (block_metrics plus
// their blobs) for one network, newest first.
func (a *API) buildBlockSnapshot(ctx context.Context, network config.NetworkConfig) (BlockSnapshotData, error) {
	queryCtx, cancel := context.WithTimeout(ctx, pollerQueryTimeout)
	defer cancel()

	snapshot := BlockSnapshotData{Blocks: []NewBlockData{}}

	var metrics []models.BlockMetrics
	if err := a.db.SelectContext(queryCtx, &metrics, queryBlockMetrics, network.ChainID, blockSnapshotDepth); err != nil {
		return snapshot, err
	}
	if len(metrics) == 0 {
		return snapshot, nil
	}

	blockNumbers := make([]int64, len(metrics))
	for i, metric := range metrics {
		blockNumbers[i] = metric.BlockNumber
	}

	var blobs []models.Blob
	if err := a.db.SelectContext(queryCtx, &blobs, queryBlobsByBlockNumbers, network.ChainID, pq.Array(blockNumbers)); err != nil {
		return snapshot, err
	}
	blobsByBlock := make(map[int64][]BlobResponse, len(metrics))
	for _, blob := range blobs {
		blobsByBlock[blob.BlockNumber] = append(blobsByBlock[blob.BlockNumber], toBlobResponse(blob, network.Name))
	}

	for _, metric := range metrics {
		pricing := toBlockPricingResponse(metric)
		brs := blobsByBlock[metric.BlockNumber]
		if brs == nil {
			brs = []BlobResponse{}
		}
		snapshot.Blocks = append(snapshot.Blocks, NewBlockData{
			BlockNumber: metric.BlockNumber,
			BlobCount:   metric.BlobCount,
			Timestamp:   metric.BlockTimestamp,
			Blobs:       brs,
			Pricing:     &pricing,
		})
	}
	return snapshot, nil
}

// sendBlockSnapshot builds and delivers the block_snapshot event for a newly
// registered client. Failures are logged and dropped: the client still
// receives live events, and its reconnect-refetch covers the missing history.
func (a *API) sendBlockSnapshot(client *Client, network config.NetworkConfig) {
	if a.db == nil {
		return
	}
	snapshot, err := a.buildBlockSnapshot(context.Background(), network)
	if err != nil {
		logger.Warn("Failed to build WebSocket block snapshot",
			zap.String("network", network.Name),
			zap.Error(err))
		return
	}
	// The hub's select processes ready channels in random order, so the
	// direct send could otherwise be handled before the queued registration
	// and be dropped as addressed to an unknown client. Wait for the hub to
	// acknowledge registration first.
	select {
	case <-client.registered:
	case <-a.hub.done:
		return
	}
	a.hub.SendEventToClient(client, WSEvent{Type: EventBlockSnapshot, Data: snapshot})
}
