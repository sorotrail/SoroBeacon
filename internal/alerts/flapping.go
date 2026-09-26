package alerts

// FlappingDetector detects and damps flapping rules.
type FlappingDetector struct {
	threshold int
}

func NewFlappingDetector(thresh int) *FlappingDetector {
	return &FlappingDetector{threshold: thresh}
}

func (f *FlappingDetector) IsFlapping(ruleID string) bool {
	// Detect if rule is flapping
	return false
}
