ALTER TABLE rules ADD COLUMN severity TEXT NOT NULL DEFAULT 'warning'
  CHECK (severity IN ('info', 'warning', 'critical'));