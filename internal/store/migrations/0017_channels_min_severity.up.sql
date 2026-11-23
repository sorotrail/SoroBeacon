ALTER TABLE channels ADD COLUMN min_severity TEXT
  CHECK (min_severity IN ('info', 'warning', 'critical'));