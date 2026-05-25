package api

import (
	"context"
	"strconv"
	"time"

	"go.uber.org/zap"

	"github.com/a-thomas-22/blob-indexer-api/internal/config"
	"github.com/a-thomas-22/blob-indexer-api/internal/db/models"
	"github.com/a-thomas-22/blob-indexer-api/internal/logger"
)

const (
	defaultPollInterval     = 3 * time.Second
	defaultUsersThrottle    = 30 * time.Second
	pollerQueryTimeout      = 5 * time.Second
	maxNewBlobsPerPoll      = 1000
	defaultMempoolPollLimit = 200
)

// Poller periodically checks the database for new indexed blocks and mempool
// changes, then broadcasts WebSocket events through the Hub.
type Poller struct {
	db              DBProvider
	hub             *Hub
	networks        map[int]config.NetworkConfig
	pollInterval    time.Duration
	usersThrottle   time.Duration
	lastSeenBlocks  map[int]uint64              // chainID → last block number
	lastUsersUpdate map[int]time.Time           // chainID → last users broadcast
	lastPendingTxs  map[int]map[string]struct{} // chainID → set of pending tx hashes
}

// NewPoller creates a Poller.
func NewPoller(db DBProvider, hub *Hub, networks map[int]config.NetworkConfig, pollInterval, usersThrottle time.Duration) *Poller {
	if pollInterval <= 0 {
		pollInterval = defaultPollInterval
	}
	if usersThrottle <= 0 {
		usersThrottle = defaultUsersThrottle
	}
	return &Poller{
		db:              db,
		hub:             hub,
		networks:        networks,
		pollInterval:    pollInterval,
		usersThrottle:   usersThrottle,
		lastSeenBlocks:  make(map[int]uint64),
		lastUsersUpdate: make(map[int]time.Time),
		lastPendingTxs:  make(map[int]map[string]struct{}),
	}
}

// Run starts the polling loop. It blocks until ctx is canceled.
func (p *Poller) Run(ctx context.Context) {
	if p.db == nil {
		logger.Info("WebSocket poller not started: no database connection")
		return
	}

	ticker := time.NewTicker(p.pollInterval)
	defer ticker.Stop()

	logger.Info("WebSocket poller started",
		zap.Duration("poll_interval", p.pollInterval),
		zap.Duration("users_throttle", p.usersThrottle),
		zap.Int("networks", len(p.networks)))

	for {
		select {
		case <-ctx.Done():
			logger.Info("WebSocket poller stopped")
			return
		case <-ticker.C:
			if p.hub != nil && p.hub.ClientCount() == 0 {
				continue
			}
			p.poll(ctx)
		}
	}
}

// poll runs a single poll cycle across all networks.
func (p *Poller) poll(ctx context.Context) {
	for _, network := range p.networks {
		p.pollNetwork(ctx, network)
	}
}

// pollNetwork checks one network for new blocks and mempool changes.
func (p *Poller) pollNetwork(ctx context.Context, network config.NetworkConfig) {
	chainID := network.ChainID

	// Check for new blocks.
	currentBlock := p.queryLastIndexedBlock(ctx, chainID)
	lastSeen := p.lastSeenBlocks[chainID]

	if currentBlock > lastSeen && lastSeen > 0 {
		// New block(s) detected. Only advance lastSeenBlocks if
		// broadcastNewBlocks succeeds, so we retry on next tick.
		if p.broadcastNewBlocks(ctx, network, lastSeen) {
			p.lastSeenBlocks[chainID] = currentBlock
		}
		p.broadcastStatsUpdate(ctx, network)
		if time.Since(p.lastUsersUpdate[chainID]) >= p.usersThrottle {
			p.broadcastUsersUpdate(ctx, network)
			p.lastUsersUpdate[chainID] = time.Now()
		}
	} else {
		p.lastSeenBlocks[chainID] = currentBlock
	}

	// Check mempool changes.
	p.pollMempool(ctx, network)
}

// queryLastIndexedBlock reads the last indexed block from indexer_metadata.
func (p *Poller) queryLastIndexedBlock(ctx context.Context, chainID int) uint64 {
	queryCtx, cancel := context.WithTimeout(ctx, pollerQueryTimeout)
	defer cancel()

	var value string
	if err := p.db.GetContext(queryCtx, &value, queryLastIndexedBlock, chainID); err != nil {
		return 0
	}
	block, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		return 0
	}
	return block
}

// broadcastNewBlocks queries blobs since lastSeen, groups them by block,
// and broadcasts a new_block event for each block. Returns true on success.
func (p *Poller) broadcastNewBlocks(ctx context.Context, network config.NetworkConfig, lastSeen uint64) bool {
	queryCtx, cancel := context.WithTimeout(ctx, pollerQueryTimeout)
	defer cancel()

	var blobs []models.Blob
	if err := p.db.SelectContext(queryCtx, &blobs, queryNewBlobsSinceBlock, network.ChainID, lastSeen, maxNewBlobsPerPoll); err != nil {
		logger.Error("Poller: failed to query new blobs",
			zap.String("network", network.Name),
			zap.Error(err))
		return false
	}

	if len(blobs) == 0 {
		return true
	}

	// Group blobs by block number.
	blockGroups := make(map[int64][]BlobResponse)
	var blockOrder []int64
	for _, blob := range blobs {
		br := toBlobResponse(blob, network.Name)
		if _, exists := blockGroups[blob.BlockNumber]; !exists {
			blockOrder = append(blockOrder, blob.BlockNumber)
		}
		blockGroups[blob.BlockNumber] = append(blockGroups[blob.BlockNumber], br)
	}

	for _, blockNum := range blockOrder {
		brs := blockGroups[blockNum]
		var ts time.Time
		if len(brs) > 0 {
			ts = brs[0].Timestamp
		}
		p.hub.BroadcastEvent(network.Name, WSEvent{
			Type: EventNewBlock,
			Data: NewBlockData{
				BlockNumber: blockNum,
				BlobCount:   len(brs),
				Timestamp:   ts,
				Blobs:       brs,
			},
		})
	}

	logger.Debug("Poller: broadcast new blocks",
		zap.String("network", network.Name),
		zap.Int("blocks", len(blockOrder)),
		zap.Int("blobs", len(blobs)))

	return true
}

// broadcastStatsUpdate queries aggregate stats and broadcasts a stats_update event.
func (p *Poller) broadcastStatsUpdate(ctx context.Context, network config.NetworkConfig) {
	queryCtx, cancel := context.WithTimeout(ctx, pollerQueryTimeout)
	defer cancel()

	var stats models.BlobStatsAggregate
	if err := p.db.GetContext(queryCtx, &stats, queryBlobStats, network.ChainID); err != nil {
		logger.Error("Poller: failed to query stats",
			zap.String("network", network.Name),
			zap.Error(err))
		return
	}

	lastBlock := p.queryLastIndexedBlock(ctx, network.ChainID)

	p.hub.BroadcastEvent(network.Name, WSEvent{
		Type: EventStatsUpdate,
		Data: StatsResponse{
			TotalBlobs:          stats.TotalBlobs,
			TotalConfirmedBlobs: stats.TotalConfirmedBlobs,
			TotalPendingBlobs:   stats.TotalPendingBlobs,
			AverageBaseFee:      stats.AverageBaseFee,
			AverageTip:          stats.AverageTip,
			AverageTotalCost:    stats.AverageTotalCost,
			LastIndexedBlock:    lastBlock,
			LastIndexedTime:     stats.LastIndexedTime,
		},
	})
}

// broadcastUsersUpdate queries top blob users and broadcasts a users_update event.
func (p *Poller) broadcastUsersUpdate(ctx context.Context, network config.NetworkConfig) {
	queryCtx, cancel := context.WithTimeout(ctx, pollerQueryTimeout)
	defer cancel()

	var users []models.BlobUserStats
	if err := p.db.SelectContext(queryCtx, &users, queryTopBlobUsersWithOptions, network.ChainID, 10, 0, string(userWindowAll), string(userSortCount)); err != nil {
		logger.Error("Poller: failed to query top users",
			zap.String("network", network.Name),
			zap.Error(err))
		return
	}

	response := make([]UserResponse, 0, len(users))
	for _, user := range users {
		response = append(response, toUserResponse(user, network.ChainID, network.Name))
	}

	p.hub.BroadcastEvent(network.Name, WSEvent{
		Type: EventUsersUpdate,
		Data: response,
	})
}

// pollMempool queries pending blobs and broadcasts add/remove diffs.
func (p *Poller) pollMempool(ctx context.Context, network config.NetworkConfig) {
	queryCtx, cancel := context.WithTimeout(ctx, pollerQueryTimeout)
	defer cancel()

	chainID := network.ChainID

	var blobs []models.Blob
	if err := p.db.SelectContext(queryCtx, &blobs, queryMempoolBlobs, chainID, defaultMempoolPollLimit, 0); err != nil {
		logger.Error("Poller: failed to query mempool",
			zap.String("network", network.Name),
			zap.Error(err))
		return
	}

	currentTxs := make(map[string]models.Blob, len(blobs))
	for _, b := range blobs {
		currentTxs[b.TxHash] = b
	}

	prevTxs := p.lastPendingTxs[chainID]
	if prevTxs == nil {
		// First poll — set baseline without broadcasting.
		newSet := make(map[string]struct{}, len(currentTxs))
		for hash := range currentTxs {
			newSet[hash] = struct{}{}
		}
		p.lastPendingTxs[chainID] = newSet
		return
	}

	// Detect additions.
	for hash, blob := range currentTxs {
		if _, existed := prevTxs[hash]; !existed {
			p.hub.BroadcastEvent(network.Name, WSEvent{
				Type: EventMempoolUpdate,
				Data: MempoolUpdateData{
					Action: MempoolActionAdd,
					Blob:   toBlobResponse(blob, network.Name),
				},
			})
		}
	}

	// Detect removals.
	for hash := range prevTxs {
		if _, exists := currentTxs[hash]; !exists {
			p.hub.BroadcastEvent(network.Name, WSEvent{
				Type: EventMempoolUpdate,
				Data: MempoolUpdateData{
					Action: MempoolActionRemove,
					Blob:   BlobResponse{TxHash: hash, NetworkName: network.Name, NetworkID: chainID},
				},
			})
		}
	}

	// Update state.
	newSet := make(map[string]struct{}, len(currentTxs))
	for hash := range currentTxs {
		newSet[hash] = struct{}{}
	}
	p.lastPendingTxs[chainID] = newSet
}
