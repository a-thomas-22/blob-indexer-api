package indexer

import (
	"context"
	"database/sql"
	"math"
	"time"

	"github.com/jmoiron/sqlx"
	"go.uber.org/zap"

	"github.com/a-thomas-22/blob-indexer-api/internal/beacon"
	"github.com/a-thomas-22/blob-indexer-api/internal/logger"
)

// Live blocks reach the workers from several producers — the polling walker,
// the WebSocket follower, the reorg re-queue, the gap scanner — and the
// workers fetch and commit them in parallel, so block N can commit before
// block N-1 (mainnet 2026-09-20: 26020403 committed 67ms before 26020402).
//
// That is harmless for the block data itself, but not for the pending-pool
// snapshot: the snapshot for N reads mempool_blobs after N's own promotion
// deletes, so anything block N-1 included is still "pending" until N-1
// commits, and N's builder gets blamed for skipping transactions it could
// never have included. A quarter of mainnet's candidate rows were wrong
// that way before this file existed.
//
// Two mechanisms keep the snapshot honest. Before a live block opens its
// transaction, the worker waits (bounded) for the in-flight blocks just
// below it to finish, so the common case commits in order. Inside the
// transaction, the snapshot is taken only when every block in the window
// below it is already committed; otherwise the block stores its builder with
// candidate_snapshot = false, like a historical block, rather than a
// snapshot of a pool the builder never faced. The maintenance loop's repair
// (repairIncludedCandidates) then catches anything that still slips past.

const (
	// defaultPredecessorWait bounds how long a live block waits for the
	// in-flight blocks below it before going ahead regardless. A predecessor
	// stuck in its retry backoff must not stall the tip; the snapshot gate
	// then skips the snapshot rather than taking a wrong one.
	defaultPredecessorWait = 5 * time.Second
	// predecessorPollInterval is how often a waiting worker re-checks the
	// in-flight set.
	predecessorPollInterval = 20 * time.Millisecond
)

// enqueueBlock records the block as in flight and hands it to the workers.
// It returns false when the indexer is shutting down and the task was not
// queued. Every producer must go through here: the in-flight set is what a
// live block waits on, and a task queued behind its back would be invisible
// to that wait.
func (i *Indexer) enqueueBlock(blockNumber uint64) bool {
	i.inFlightMu.Lock()
	if i.inFlight == nil {
		i.inFlight = make(map[uint64]int)
	}
	i.inFlight[blockNumber]++
	i.inFlightMu.Unlock()

	select {
	case <-i.ctx.Done():
		i.finishBlock(blockNumber)
		return false
	case i.blockTaskCh <- BlockTask{BlockNumber: blockNumber}:
		return true
	}
}

// finishBlock releases one in-flight reference for the block, whatever the
// outcome of processing it: a block that failed and was handed to the gap
// scanner is no longer something the tip should wait for. The same height
// can be queued twice (walker and WebSocket follower), hence the count. A
// task that bypassed enqueueBlock (tests write to the channel directly)
// releases nothing.
func (i *Indexer) finishBlock(blockNumber uint64) {
	i.inFlightMu.Lock()
	defer i.inFlightMu.Unlock()
	if i.inFlight[blockNumber] <= 1 {
		delete(i.inFlight, blockNumber)
		return
	}
	i.inFlight[blockNumber]--
}

// lowestInFlightBelow returns the lowest in-flight block in
// [lowest, blockNumber), and whether there is one.
func (i *Indexer) lowestInFlightBelow(blockNumber, lowest uint64) (uint64, bool) {
	i.inFlightMu.Lock()
	defer i.inFlightMu.Unlock()
	var found uint64
	ok := false
	for pending := range i.inFlight {
		if pending < lowest || pending >= blockNumber {
			continue
		}
		if !ok || pending < found {
			found, ok = pending, true
		}
	}
	return found, ok
}

// candidateSnapshotPredecessors is how many blocks below a live block must
// already be committed for its pending-pool snapshot to be trusted.
//
// Only a block inside the candidate liveness window can leak transactions
// into a later snapshot: a transaction the node dropped from its pool
// (because an earlier block included it) stops having last_seen refreshed,
// and once it falls out of the window selectPendingCandidates ignores it
// whether or not the block that included it has committed. So the window,
// in slots, is exactly how far back an uncommitted block still matters — plus
// one slot for refresh jitter.
func (i *Indexer) candidateSnapshotPredecessors() uint64 {
	secondsPerSlot := i.network.SecondsPerSlot
	if secondsPerSlot == 0 {
		secondsPerSlot = beacon.DefaultSecondsPerSlot
	}
	slots := math.Ceil(i.candidateLivenessWindow().Seconds() / float64(secondsPerSlot))
	return uint64(slots) + 1
}

// snapshotPredecessorRange is the block range a live block's snapshot
// depends on: the candidateSnapshotPredecessors blocks below it. ok is
// false when the range is empty. The range is not floored at the network's
// start here — start_block may be LATEST or LATEST-k, resolved against the
// node at start — so predecessorsCommitted floors it at the earliest block
// actually indexed, which nothing below will ever join.
func (i *Indexer) snapshotPredecessorRange(blockNumber int64) (from, to int64, ok bool) {
	to = blockNumber - 1
	from = blockNumber - int64(i.candidateSnapshotPredecessors())
	if from < 0 {
		from = 0
	}
	return from, to, from <= to
}

// awaitPredecessors blocks, for at most predecessorWait, while a block in the
// snapshot window below blockNumber is still in flight. It is called for a
// live block after its RPC fetch and before its transaction opens, so the
// parallel fetch is kept and only the commit is ordered. Historical blocks
// never call it. Giving up is logged: the snapshot gate then decides.
func (i *Indexer) awaitPredecessors(blockNumber uint64) {
	if i.predecessorWait <= 0 {
		return
	}
	span := i.candidateSnapshotPredecessors()
	var lowest uint64
	if blockNumber > span {
		lowest = blockNumber - span
	}
	if _, waiting := i.lowestInFlightBelow(blockNumber, lowest); !waiting {
		return
	}

	deadline := time.Now().Add(i.predecessorWait)
	ticker := time.NewTicker(predecessorPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-i.ctx.Done():
			return
		case <-ticker.C:
		}
		pending, waiting := i.lowestInFlightBelow(blockNumber, lowest)
		if !waiting {
			return
		}
		if time.Now().After(deadline) {
			logger.Warn("Live block stopped waiting for an earlier block still in flight",
				zap.String("network", i.network.Name),
				zap.Uint64("block", blockNumber),
				zap.Uint64("in_flight_block", pending),
				zap.Duration("waited", i.predecessorWait))
			return
		}
	}
}

// predecessorsCommitted reports, from inside the block's own transaction,
// whether every block in its snapshot window is already in indexed_blocks.
// Those blocks' promotion deletes are then visible to the snapshot's pool
// read, which is the only way the pool it reads is the pool the builder
// faced.
//
// The window is floored at the network's earliest indexed block: a network
// started at LATEST-k has nothing below its first block and never will, and
// a numeric start_block is simply that first block. With nothing indexed
// yet, the block is the first one and has nothing to wait for.
func (i *Indexer) predecessorsCommitted(ctx context.Context, tx *sqlx.Tx, blockNumber int64) (bool, error) {
	from, to, ok := i.snapshotPredecessorRange(blockNumber)
	if !ok {
		return true, nil
	}
	var earliest sql.NullInt64
	var indexed int64
	err := tx.QueryRowContext(ctx, `
		SELECT
			(SELECT MIN(block_number) FROM indexed_blocks WHERE chain_id = $1) AS earliest,
			(SELECT COUNT(*) FROM indexed_blocks
				WHERE chain_id = $1 AND block_number BETWEEN $2 AND $3) AS indexed
	`, i.network.ChainID, from, to).Scan(&earliest, &indexed)
	if err != nil {
		return false, err
	}
	if !earliest.Valid {
		return true, nil
	}
	if earliest.Int64 > from {
		from = earliest.Int64
	}
	if from > to {
		return true, nil
	}
	return indexed == to-from+1, nil
}
