package api

import (
	"net/http"
	"strings"

	"github.com/gorilla/websocket"
	"go.uber.org/zap"

	"github.com/a-thomas-22/blob-indexer-api/internal/logger"
)

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
	}

	select {
	case a.hub.register <- client:
		go client.writePump()
		go client.readPump()
	case <-a.hub.done:
		// Hub is shutting down: release the slot and drop the connection. The
		// client was never registered, so the hub will not release it for us.
		a.hub.release(remoteIP)
		logger.Error("WebSocket registration failed: hub is shutting down",
			zap.String("network", network.Name))
		_ = conn.Close()
	}
}
