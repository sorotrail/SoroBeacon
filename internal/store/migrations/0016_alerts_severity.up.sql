ALTER TABLE alerts ADD COLUMN severity TEXT NOT NULL DEFAULT 'warning'
  CHECK (severity IN ('info', 'warning', 'critical'));