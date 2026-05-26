-- Widen blobs.blob_index from SMALLINT (max 32767) to INTEGER. The pending
-- pool allocates new blob_index values from MAX(blob_index)+1 on cold paths,
-- and even with the steady-state in-place-update fix the column would still
-- be uncomfortably tight if a mempool ever lingered for very long. INTEGER
-- gives ~65535x more headroom at the cost of 2 bytes per row.
ALTER TABLE blobs ALTER COLUMN blob_index TYPE INTEGER;
