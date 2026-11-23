-- Revert retention gaps.
DROP INDEX IF EXISTS retention_gaps_network_detected_idx;
DROP TABLE IF EXISTS retention_gaps;
