package api

import (
	"context"
	"encoding/json"
	"time"

	"github.com/lib/pq"
	"go.uber.org/zap"

	"github.com/a-thomas-22/blob-indexer-api/internal/config"
	"github.com/a-thomas-22/blob-indexer-api/internal/db/models"
	"github.com/a-thomas-22/blob-indexer-api/internal/logger"
)

const (
	defaultPollInterval  = 3 * time.Second
	defaultUsersThrottle = 30 * time.Second
	pollerQueryTimeout   = 5 * time.Second

	// blockNotifyChannel is the LISTEN/NOTIFY channel the block_metrics
	// trigger (migration 000006) posts to when a block's row commits.
	// Notifications deliver on COMMIT in commit order, so every committed
	// block produces exactly one notification and out-of-order worker commits
	// cannot be skipped — the property the previous last_indexed_block
	// watermark lacked.
	blockNotifyChannel = "blob_indexer_new_block"

	// trailingScanWindow is how many blocks behind the broadcast head the
	// fallback scan re-checks. It exists for periods when notifications are
	// missed (listener down or reconnecting): a block that committed late and
	// out of order lands behind the head and would otherwise never be seen by
	// a forward-only cursor. Notifications older than this window are treated
	// as deep-history rewrites (reindex/backfill) and not broadcast.
	trailingScanWindow = 32

	// maxScanBlocksPerTick bounds one catch-up scan. Anything beyond it is
	// picked up on subsequent ticks: the head only ever advances to blocks
	// that were actually broadcast, so a bounded scan cannot strand blocks the
	// way the previous LIMIT-then-jump-to-metadata cursor could.
	maxScanBlocksPerTick = 64

	// listenerPingEveryTicks is how many poll ticks pass between listener
	// health pings. Pinging prompts pq.Listener to notice a dead connection
	// and start its internal reconnect loop.
	listenerPingEveryTicks = 20

	// listenerMinReconnect/listenerMaxReconnect bound pq.Listener's internal
	// reconnect backoff.
	listenerMinReconnect = time.Second
	listenerMaxReconnect = 30 * time.Second
)

// cacheInvalidator lets the poller drop origin response caches the moment it
// detects new data — BEFORE broadcasting the WebSocket events that trigger the
// dashboard's refetch herd. Without this, the herd arriving right after a
// new_block broadcast would be served pre-block cache entries for up to a full
// TTL, leaving pricing/stats one block behind for most of each block interval.
type cacheInvalidator interface {
	invalidateBlockCaches(chainID int)
	invalidateMempoolCaches(chainID int)
}

// blockListener is the subset of *pq.Listener the poller uses, abstracted so
// tests can inject a fake notification stream.
type blockListener interface {
	Listen(channel string) error
	NotificationChannel() <-chan *pq.Notification
	Ping() error
	Close() error
}

// listenerFactory creates a blockListener. onEvent receives pq listener
// lifecycle events (connected, reconnected, disconnected).
type listenerFactory func(onEvent func(pq.ListenerEventType)) blockListener

// pqListenerFactory returns a listenerFactory backed by a real pq.Listener
// connected with the given DSN.
func pqListenerFactory(dsn string) listenerFactory {
	return func(onEvent func(pq.ListenerEventType)) blockListener {
		return pq.NewListener(dsn, listenerMinReconnect, listenerMaxReconnect,
			func(ev pq.ListenerEventType, err error) {
				if err != nil {
					logger.Warn("WebSocket block listener event",
						zap.Int("event", int(ev)),
						zap.Error(err))
				}
				onEvent(ev)
			})
	}
}

// blockNotification is the payload posted by the block_metrics trigger.
type blockNotification struct {
	ChainID     int    `json:"chain_id"`
	BlockNumber uint64 `json:"block_number"`
}

// chainBroadcastState tracks what has been broadcast for one network. It is
// only touched from the Run goroutine.
type chainBroadcastState struct {
	// baselined is set once the startup baseline (current newest block) has
	// been established; nothing is broadcast before that, and a baseline
	// query error simply retries next tick instead of corrupting the state.
	baselined bool
	// head is the highest block number handled so far.
	head uint64
	// seen records handled blocks within the trailing scan window so the
	// catch-up scan does not re-broadcast them.
	seen map[uint64]struct{}
}

// Poller drives WebSocket broadcasts. New blocks are primarily detected via
// LISTEN/NOTIFY on block_metrics commits (see blockNotifyChannel); a periodic
// catch-up scan over block_metrics covers missed notifications, and the same
// ticker drives mempool diffing and stats/users rebroadcasts. Every block gets
// a new_block event — including blocks with zero blob transactions, which the
// previous blobs-derived detection silently dropped while REST responses
// (block_metrics-backed) still contained them.
type Poller struct {
	db              DBProvider
	hub             *Hub
	networks        map[int]config.NetworkConfig
	pollInterval    time.Duration
	usersThrottle   time.Duration
	invalidator     cacheInvalidator // optional; set before Run starts
	listenerFactory listenerFactory  // optional; set before Run starts. nil = scan-only.

	chains          map[int]*chainBroadcastState
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
		chains:          make(map[int]*chainBroadcastState),
		lastUsersUpdate: make(map[int]time.Time),
		lastPendingTxs:  make(map[int]map[string]struct{}),
	}
}

func (p *Poller) chainState(chainID int) *chainBroadcastState {
	st, ok := p.chains[chainID]
	if !ok {
		st = &chainBroadcastState{seen: make(map[uint64]struct{})}
		p.chains[chainID] = st
	}
	return st
}

// Run starts the notification listener and the polling loop. It blocks until
// ctx is canceled.
func (p *Poller) Run(ctx context.Context) {
	if p.db == nil {
		logger.Info("WebSocket poller not started: no database connection")
		return
	}

	var notifCh <-chan *pq.Notification
	listenerEvents := make(chan pq.ListenerEventType, 8)
	var listener blockListener
	if p.listenerFactory != nil {
		listener = p.listenerFactory(func(ev pq.ListenerEventType) {
			select {
			case listenerEvents <- ev:
			default:
			}
		})
		if err := listener.Listen(blockNotifyChannel); err != nil {
			logger.Warn("WebSocket poller: LISTEN failed, falling back to scan-only detection",
				zap.String("channel", blockNotifyChannel),
				zap.Error(err))
			_ = listener.Close()
			listener = nil
		} else {
			notifCh = listener.NotificationChannel()
			defer func() { _ = listener.Close() }()
		}
	}

	ticker := time.NewTicker(p.pollInterval)
	defer ticker.Stop()

	logger.Info("WebSocket poller started",
		zap.Duration("poll_interval", p.pollInterval),
		zap.Duration("users_throttle", p.usersThrottle),
		zap.Bool("notify_listener", listener != nil),
		zap.Int("networks", len(p.networks)))

	tick := 0
	for {
		select {
		case <-ctx.Done():
			logger.Info("WebSocket poller stopped")
			return

		case n := <-notifCh:
			// pq delivers nil when the connection is lost; the reconnect is
			// handled internally and the catch-up scan covers the gap.
			if n != nil {
				p.handleNotification(ctx, n.Extra)
			}

		case ev := <-listenerEvents:
			// After a reconnect, scan immediately: notifications sent while
			// the connection was down are gone.
			if ev == pq.ListenerEventReconnected {
				p.scanAll(ctx)
			}

		case <-ticker.C:
			tick++
			p.scanAll(ctx)
			p.pollMempoolAll(ctx)
			if listener != nil && tick%listenerPingEveryTicks == 0 {
				go func() {
					if err := listener.Ping(); err != nil {
						logger.Warn("WebSocket block listener ping failed", zap.Error(err))
					}
				}()
			}
		}
	}
}

// handleNotification reacts to one block_metrics commit notification.
func (p *Poller) handleNotification(ctx context.Context, payload string) {
	var n blockNotification
	if err := json.Unmarshal([]byte(payload), &n); err != nil {
		logger.Warn("WebSocket poller: malformed block notification",
			zap.String("payload", payload),
			zap.Error(err))
		return
	}

	network, ok := p.networks[n.ChainID]
	if !ok {
		return
	}
	st := p.chainState(n.ChainID)
	if !st.baselined {
		// Startup: the scan establishes the baseline first.
		return
	}
	if n.BlockNumber+trailingScanWindow <= st.head {
		// Deep-history rewrite (reindex/backfill) — not a live block.
		return
	}

	if p.broadcastBlock(ctx, network, st, n.BlockNumber) {
		p.broadcastNetworkUpdates(ctx, network)
	}
}

// scanAll runs the catch-up scan for every network.
func (p *Poller) scanAll(ctx context.Context) {
	for _, network := range p.networks {
		p.scanNetwork(ctx, network)
	}
}

// scanNetwork detects committed blocks whose notification was missed. On the
// first successful run it only establishes the baseline. The scan starts a
// trailing window behind the head so late out-of-order commits are still
// caught in scan-only operation.
func (p *Poller) scanNetwork(ctx context.Context, network config.NetworkConfig) {
	st := p.chainState(network.ChainID)

	if !st.baselined {
		queryCtx, cancel := context.WithTimeout(ctx, pollerQueryTimeout)
		defer cancel()
		// Seed the seen-set with the newest trailing window of existing
		// blocks: the head alone is not enough, because the catch-up scan
		// deliberately re-checks that window and would otherwise replay it
		// as fresh broadcasts on the next tick.
		var numbers []uint64
		if err := p.db.SelectContext(queryCtx, &numbers, queryRecentBlockMetricsNumbers, network.ChainID, trailingScanWindow); err != nil {
			// Retry next tick. Unlike the previous watermark, an error here
			// never corrupts broadcast state.
			logger.Error("Poller: failed to establish broadcast baseline",
				zap.String("network", network.Name),
				zap.Error(err))
			return
		}
		for _, blockNumber := range numbers {
			st.seen[blockNumber] = struct{}{}
			if blockNumber > st.head {
				st.head = blockNumber
			}
		}
		st.baselined = true
		return
	}

	lower := uint64(0)
	if st.head > trailingScanWindow {
		lower = st.head - trailingScanWindow
	}

	queryCtx, cancel := context.WithTimeout(ctx, pollerQueryTimeout)
	defer cancel()
	var numbers []uint64
	if err := p.db.SelectContext(queryCtx, &numbers, queryBlockMetricsNumbersSince, network.ChainID, lower, maxScanBlocksPerTick); err != nil {
		logger.Error("Poller: failed to scan for new blocks",
			zap.String("network", network.Name),
			zap.Error(err))
		return
	}

	broadcastAny := false
	for _, blockNumber := range numbers {
		if _, done := st.seen[blockNumber]; done {
			continue
		}
		if p.broadcastBlock(ctx, network, st, blockNumber) {
			broadcastAny = true
		}
	}
	if broadcastAny {
		p.broadcastNetworkUpdates(ctx, network)
	}

	// Prune seen entries that fell out of the trailing window.
	for blockNumber := range st.seen {
		if blockNumber+trailingScanWindow < st.head {
			delete(st.seen, blockNumber)
		}
	}
}

// broadcastBlock emits one new_block event for blockNumber. It returns true
// if an event was broadcast. Origin caches are invalidated and broadcast
// state advances even with zero connected clients, so REST readers converge
// and a later scan does not replay history at the next client.
func (p *Poller) broadcastBlock(ctx context.Context, network config.NetworkConfig, st *chainBroadcastState, blockNumber uint64) bool {
	st.seen[blockNumber] = struct{}{}
	if blockNumber > st.head {
		st.head = blockNumber
	}

	// Drop block-derived response caches before broadcasting so the refetch
	// herd the broadcast triggers is served the new block.
	if p.invalidator != nil {
		p.invalidator.invalidateBlockCaches(network.ChainID)
	}

	if p.hub == nil || p.hub.ClientCount() == 0 {
		return false
	}

	queryCtx, cancel := context.WithTimeout(ctx, pollerQueryTimeout)
	defer cancel()

	var metrics []models.BlockMetrics
	if err := p.db.SelectContext(queryCtx, &metrics, queryBlockMetricsByNumber, network.ChainID, pq.Array([]int64{int64(blockNumber)})); err != nil {
		// Transient failure: unmark the block so the trailing scan retries it.
		logger.Error("Poller: failed to query block metrics for broadcast",
			zap.String("network", network.Name),
			zap.Uint64("block", blockNumber),
			zap.Error(err))
		delete(st.seen, blockNumber)
		return false
	}
	if len(metrics) == 0 {
		// The row vanished between commit and read — a reorg deleted it. The
		// replacement block re-notifies, so stay silent here.
		return false
	}
	metric := metrics[0]

	var blobs []models.Blob
	if err := p.db.SelectContext(queryCtx, &blobs, queryBlobsByBlockNumber, network.ChainID, blockNumber); err != nil {
		logger.Error("Poller: failed to query blobs for broadcast",
			zap.String("network", network.Name),
			zap.Uint64("block", blockNumber),
			zap.Error(err))
		delete(st.seen, blockNumber)
		return false
	}

	pricing := toBlockPricingResponse(metric)
	brs := make([]BlobResponse, 0, len(blobs))
	for _, blob := range blobs {
		brs = append(brs, toBlobResponse(blob, network))
	}

	p.hub.BroadcastEvent(network.Name, WSEvent{
		Type: EventNewBlock,
		Data: NewBlockData{
			BlockNumber: metric.BlockNumber,
			BlobCount:   metric.BlobCount,
			Timestamp:   metric.BlockTimestamp,
			Blobs:       brs,
			Pricing:     &pricing,
		},
	})

	logger.Debug("Poller: broadcast new block",
		zap.String("network", network.Name),
		zap.Uint64("block", blockNumber),
		zap.Int("blobs", len(brs)))

	return true
}

// broadcastNetworkUpdates emits the per-block companion events: a stats update
// on every block, and a users update at most once per throttle interval.
func (p *Poller) broadcastNetworkUpdates(ctx context.Context, network config.NetworkConfig) {
	p.broadcastStatsUpdate(ctx, network)
	if time.Since(p.lastUsersUpdate[network.ChainID]) >= p.usersThrottle {
		p.broadcastUsersUpdate(ctx, network)
		p.lastUsersUpdate[network.ChainID] = time.Now()
	}
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

	p.hub.BroadcastEvent(network.Name, WSEvent{
		Type: EventStatsUpdate,
		Data: toStatsResponse(stats, network.ChainID, network.Name),
	})
}

// broadcastUsersUpdate queries top blob users and broadcasts users_update
// events: the per-address rows first (the historical payload), then the
// entity-grouped rows tagged with the envelope group field, mirroring GET
// /users?group=entity so live tables in either mode can apply matching
// pushes and drop the other variant.
func (p *Poller) broadcastUsersUpdate(ctx context.Context, network config.NetworkConfig) {
	queryCtx, cancel := context.WithTimeout(ctx, pollerQueryTimeout)
	defer cancel()

	var users []models.BlobUserStats
	if err := p.db.SelectContext(queryCtx, &users, queryTopBlobUsersAllByCount, network.ChainID, 10, 0, string(userWindowAll)); err != nil {
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
		Type:  EventUsersUpdate,
		Range: string(userWindowAll),
		Data:  response,
	})

	// A grouped-query failure only costs the grouped variant this tick; the
	// per-address broadcast above already went out.
	var groups []models.BlobUserGroupStats
	if err := p.db.SelectContext(queryCtx, &groups, queryTopBlobUserGroupsAll, network.ChainID, 10, 0, string(userWindowAll), string(userSortCount)); err != nil {
		logger.Error("Poller: failed to query top user groups",
			zap.String("network", network.Name),
			zap.Error(err))
		return
	}

	groupedResponse := make([]UserResponse, 0, len(groups))
	for _, userGroup := range groups {
		groupedResponse = append(groupedResponse, toGroupedUserResponse(userGroup, network.ChainID, network.Name))
	}

	p.hub.BroadcastEvent(network.Name, WSEvent{
		Type:  EventUsersUpdate,
		Range: string(userWindowAll),
		Group: string(userGroupEntity),
		Data:  groupedResponse,
	})
}

// pollMempoolAll diffs the mempool for every network. Mempool events only
// matter to connected clients, so the diffing is skipped entirely when there
// are none (the first poll after a client connects re-baselines silently).
func (p *Poller) pollMempoolAll(ctx context.Context) {
	if p.hub != nil && p.hub.ClientCount() == 0 {
		return
	}
	for _, network := range p.networks {
		p.pollMempool(ctx, network)
	}
}

// pollMempool diffs the pending tx-hash set and broadcasts add/remove events.
// The steady-state probe is hash-only (an index-only scan of the partial
// pending index); full rows are fetched just for hashes that are new since the
// previous tick, which in steady state is zero or a handful per block. Diffing
// the complete pending set — instead of a newest-N window — also avoids
// spurious add/remove churn when rows slide across a window boundary.
func (p *Poller) pollMempool(ctx context.Context, network config.NetworkConfig) {
	queryCtx, cancel := context.WithTimeout(ctx, pollerQueryTimeout)
	defer cancel()

	chainID := network.ChainID

	var hashes []string
	if err := p.db.SelectContext(queryCtx, &hashes, queryPendingBlobTxHashes, chainID); err != nil {
		logger.Error("Poller: failed to query mempool",
			zap.String("network", network.Name),
			zap.Error(err))
		return
	}

	currentTxs := make(map[string]struct{}, len(hashes))
	for _, hash := range hashes {
		currentTxs[hash] = struct{}{}
	}

	prevTxs := p.lastPendingTxs[chainID]
	if prevTxs == nil {
		// First poll — set baseline without broadcasting.
		p.lastPendingTxs[chainID] = currentTxs
		return
	}

	// Detect additions and removals.
	added := make([]string, 0)
	for hash := range currentTxs {
		if _, existed := prevTxs[hash]; !existed {
			added = append(added, hash)
		}
	}
	removed := make([]string, 0)
	for hash := range prevTxs {
		if _, exists := currentTxs[hash]; !exists {
			removed = append(removed, hash)
		}
	}

	// Drop mempool-derived response caches before broadcasting the diff, so
	// clients reacting to the events read post-change data.
	if (len(added) > 0 || len(removed) > 0) && p.invalidator != nil {
		p.invalidator.invalidateMempoolCaches(chainID)
	}

	if len(added) > 0 {
		var blobs []models.Blob
		if err := p.db.SelectContext(queryCtx, &blobs, queryPendingBlobsByTxHashes, chainID, pq.Array(added)); err != nil {
			logger.Error("Poller: failed to query new pending blobs",
				zap.String("network", network.Name),
				zap.Error(err))
			// Keep the previous baseline so the additions are retried next tick.
			return
		}
		// One add event per transaction: multi-blob txs store one row per blob,
		// so keep only the first (lowest blob_index) row per hash — the diff is
		// hash-keyed and removals are likewise emitted once per hash.
		broadcast := make(map[string]struct{}, len(added))
		for _, blob := range blobs {
			if _, done := broadcast[blob.TxHash]; done {
				continue
			}
			broadcast[blob.TxHash] = struct{}{}
			p.hub.BroadcastEvent(network.Name, WSEvent{
				Type: EventMempoolUpdate,
				Data: MempoolUpdateData{
					Action: MempoolActionAdd,
					Blob:   toBlobResponse(blob, network),
				},
			})
		}
	}

	for _, hash := range removed {
		p.hub.BroadcastEvent(network.Name, WSEvent{
			Type: EventMempoolUpdate,
			Data: MempoolUpdateData{
				Action: MempoolActionRemove,
				Blob:   BlobResponse{TxHash: hash, NetworkName: network.Name, ChainID: chainID},
			},
		})
	}

	p.lastPendingTxs[chainID] = currentTxs
}
