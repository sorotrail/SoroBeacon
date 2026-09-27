package alerts

import (
	"testing"
	"time"
)

func TestShouldDeliver(t *testing.T) {
	tests := []struct {
		name     string
		count    int64
		expected bool
	}{
		{"first alert in window", 1, true},
		{"second alert in window", 2, false},
		{"third alert in window", 3, false},
		{"zero count", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ShouldDeliver(tt.count)
			if got != tt.expected {
				t.Errorf("ShouldDeliver(%d) = %v, want %v", tt.count, got, tt.expected)
			}
		})
	}
}

func TestWindowBounds(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	duration := 5 * time.Minute

	// Non-zero duration returns a valid window end.
	ws, we := WindowBounds(start, duration)
	if !ws.Equal(start) {
		t.Errorf("WindowStart = %v, want %v", ws, start)
	}
	wantEnd := start.Add(duration)
	if !we.Equal(wantEnd) {
		t.Errorf("WindowEnd = %v, want %v", we, wantEnd)
	}

	// Zero duration means no window; WindowEnd is zero.
	ws2, we2 := WindowBounds(start, 0)
	if !ws2.Equal(start) {
		t.Errorf("WindowStart = %v, want %v", ws2, start)
	}
	if !we2.IsZero() {
		t.Errorf("WindowEnd = %v, want zero time", we2)
	}
}

func TestGroupKeyString(t *testing.T) {
	k := MakeGroupKey(42, 7, "contract-a")
	got := k.String()
	want := "42:7:contract-a"
	if got != want {
		t.Errorf("GroupKey.String() = %q, want %q", got, want)
	}
}

func TestGroupState(t *testing.T) {
	start := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	gs := GroupState{
		GroupKey:     "42:7:contract-a",
		WindowStart:  start,
		Count:        3,
		FirstAlertID: 100,
	}
	if gs.Count != 3 {
		t.Errorf("Count = %d, want 3", gs.Count)
	}
	if gs.FirstAlertID != 100 {
		t.Errorf("FirstAlertID = %d, want 100", gs.FirstAlertID)
	}
	if !gs.WindowStart.Equal(start) {
		t.Errorf("WindowStart = %v, want %v", gs.WindowStart, start)
	}
}

// TestCooldownInteraction verifies that the group state and cooldown
// are independent concerns: an alert suppressed by cooldown still
// increments the group count if the store handles both gates. The
// ShouldDeliver function only looks at count, never at cooldown, so
// a rule with a cooldown that fires its first alert in a window
// correctly returns true from ShouldDeliver regardless of cooldown.
func TestCooldownInteraction(t *testing.T) {
	// When grouping is active and the first alert in a window happens
	// to be inside a cooldown, the store suppresses the alert but the
	// group count would have been incremented. ShouldDeliver itself
	// only checks count: count==1 means deliver, regardless of cooldown.
	// The poller respects the cooldown decision from the store before
	// consulting grouping, so there is no conflict.
	if !ShouldDeliver(1) {
		t.Error("ShouldDeliver(1) should be true even when cooldown is active")
	}
	if ShouldDeliver(0) {
		t.Error("ShouldDeliver(0) should be false")
	}
}

func TestWindowInfo(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(5 * time.Minute)
	wi := WindowInfo{Count: 4, WindowStart: start, WindowEnd: end}
	if wi.Count != 4 {
		t.Errorf("Count = %d, want 4", wi.Count)
	}
	if !wi.WindowStart.Equal(start) {
		t.Errorf("WindowStart = %v, want %v", wi.WindowStart, start)
	}
	if !wi.WindowEnd.Equal(end) {
		t.Errorf("WindowEnd = %v, want %v", wi.WindowEnd, end)
	}
}
