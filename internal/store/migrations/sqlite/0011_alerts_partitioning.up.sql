-- Parity with the Postgres 0011_alerts_partitioning. SQLite has no
-- declarative partitioning: a single-file database has no per-partition
-- storage to drop, and the existing batched delete already fits a single-node
-- deployment. The alerts table therefore stays plain here, and retention
-- continues to delete row by row. The version number is kept aligned so the
-- two migration sets cannot drift.
SELECT 1;
