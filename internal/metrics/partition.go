package metrics

// SetAlertPartitionRunway sets the gauge reporting how many consecutive future
// monthly alert partitions currently exist beyond the current UTC month.
func (m *Metrics) SetAlertPartitionRunway(months int) {
	if m == nil || m.alertPartitionRunwayMonths == nil {
		return
	}
	m.alertPartitionRunwayMonths.Set(float64(months))
}

// RecordAlertPartitionError increments the error counter for a partition
// lifecycle operation ("ensure", "drop", or "inspect").
func (m *Metrics) RecordAlertPartitionError(operation string) {
	if m == nil || m.alertPartitionErrorsTotal == nil {
		return
	}
	if operation == "" {
		operation = "unknown"
	}
	m.alertPartitionErrorsTotal.WithLabelValues(operation).Inc()
}

// RecordAlertPartitionsCreated increments the total count of created alert
// partitions.
func (m *Metrics) RecordAlertPartitionsCreated(n int) {
	if m == nil || m.alertPartitionsCreatedTotal == nil || n <= 0 {
		return
	}
	m.alertPartitionsCreatedTotal.Add(float64(n))
}

// RecordAlertPartitionsDropped increments the total count of dropped expired
// alert partitions.
func (m *Metrics) RecordAlertPartitionsDropped(n int) {
	if m == nil || m.alertPartitionsDroppedTotal == nil || n <= 0 {
		return
	}
	m.alertPartitionsDroppedTotal.Add(float64(n))
}
