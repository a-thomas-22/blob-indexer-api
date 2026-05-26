-- This migration is not reversible because the original (one-row-per-tx)
-- representation collapsed multiple blobs into a single row with no record of
-- which blobs were combined. Rolling back would lose data; operators must
-- re-index from chain history if they need to revert.
SELECT 1;
