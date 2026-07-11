-- Drop the replacement event log. The eviction deletes keep working; the
-- observations are simply no longer recorded.
--
-- DDL only, idempotent, no explicit transaction control — see README.md.

DROP TABLE IF EXISTS blob_replacements;
