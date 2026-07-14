package api

import (
	"context"
	"time"

	"github.com/ethereum/go-ethereum/params"

	"go.uber.org/zap"

	"github.com/a-thomas-22/blob-indexer-api/internal/blobparams"
	"github.com/a-thomas-22/blob-indexer-api/internal/logger"
)

// blobScheduleCacheTTL bounds how stale the API's view of a network's learned
// blob schedule can be. The schedule changes only at fork boundaries, so a few
// minutes of staleness is harmless and keeps the hot pricing path off the DB.
const blobScheduleCacheTTL = 5 * time.Minute

type blobScheduleCacheEntry struct {
	cfg       *params.ChainConfig
	expiresAt time.Time
}

// blobScheduleQuery reads the learned schedule the indexer persisted from
// eth_config. Ordered ascending so BuildChainConfig assigns fork slots in time
// order.
const blobScheduleQuery = `
	SELECT activation_time, target, max, update_fraction
	FROM network_blob_schedule
	WHERE chain_id = $1
	ORDER BY activation_time ASC
`

type blobScheduleQueryRow struct {
	ActivationTime uint64 `db:"activation_time"`
	Target         int    `db:"target"`
	Max            int    `db:"max"`
	UpdateFraction uint64 `db:"update_fraction"`
}

// chainConfigForNetwork returns the fork-aware chain config for a network,
// reflecting the blob schedule learned from the node (BPO forks, arbitrary
// networks) layered on the compiled baseline. Results are cached per chain for
// blobScheduleCacheTTL. On any DB error it falls back to the compiled config so
// pricing/fork-stage responses degrade gracefully rather than failing.
func (a *API) chainConfigForNetwork(ctx context.Context, chainID int) *params.ChainConfig {
	a.cacheMu.RLock()
	cached, ok := a.blobScheduleCache[chainID]
	a.cacheMu.RUnlock()
	if ok && time.Now().Before(cached.expiresAt) {
		return cached.cfg
	}

	var rows []blobScheduleQueryRow
	if err := a.db.SelectContext(ctx, &rows, blobScheduleQuery, chainID); err != nil {
		logger.Debug("Failed to load learned blob schedule; using compiled config",
			zap.Int("chain_id", chainID), zap.Error(err))
		return blobparams.ChainConfigForID(chainID)
	}

	learned := make([]blobparams.ScheduleEntry, len(rows))
	for i, r := range rows {
		learned[i] = blobparams.ScheduleEntry{
			ActivationTime: r.ActivationTime,
			Target:         r.Target,
			Max:            r.Max,
			UpdateFraction: r.UpdateFraction,
		}
	}
	cfg := blobparams.BuildChainConfig(chainID, learned)

	a.cacheMu.Lock()
	if a.blobScheduleCache != nil {
		a.blobScheduleCache[chainID] = blobScheduleCacheEntry{cfg: cfg, expiresAt: time.Now().Add(blobScheduleCacheTTL)}
	}
	a.cacheMu.Unlock()
	return cfg
}
