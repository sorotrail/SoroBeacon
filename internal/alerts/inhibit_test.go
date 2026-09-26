package alerts

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sorotrail/sorobeacon/internal/store"
)

// stubFiring answers from a fixed set of "currently firing" rule ids,
// recording the windows it was asked about so tests can assert the
// per-pair window (not a global) drives the decision.
type stubFiring struct {
	firing  map[int64]bool
	windows map[int64]time.Duration
	err     error
}

func (s *stubFiring) firedWithin(_ context.Context, ruleID int64, window time.Duration) (bool, error) {
	if s.err != nil {
		return false, s.err
	}
	if s.windows == nil {
		s.windows = map[int64]time.Duration{}
	}
	s.windows[ruleID] = window
	return s.firing[ruleID], nil
}

func TestInhibitedByFiringSource(t *testing.T) {
	ctx := context.Background()
	stub := &stubFiring{firing: map[int64]bool{7: true}}
	source, inhibited, err := InhibitedBy(ctx, 9, []store.Inhibition{
		{SourceRuleID: 7, TargetRuleID: 9, FiringWindowSeconds: 300},
	}, stub.firedWithin)
	if err != nil {
		t.Fatalf("InhibitedBy: %v", err)
	}
	if !inhibited || source != 7 {
		t.Errorf("inhibited=%v source=%d, want true/7", inhibited, source)
	}
	if stub.windows[7] != 300*time.Second {
		t.Errorf("window = %v, want the pair's 300s, not a global", stub.windows[7])
	}
}

func TestInhibitedByQuietSource(t *testing.T) {
	ctx := context.Background()
	stub := &stubFiring{firing: map[int64]bool{7: false}}
	_, inhibited, err := InhibitedBy(ctx, 9, []store.Inhibition{
		{SourceRuleID: 7, TargetRuleID: 9, FiringWindowSeconds: 300},
	}, stub.firedWithin)
	if err != nil {
		t.Fatalf("InhibitedBy: %v", err)
	}
	if inhibited {
		t.Error("inhibited = true for a source outside its window; deliver")
	}
}

func TestInhibitedByNoPairs(t *testing.T) {
	ctx := context.Background()
	stub := &stubFiring{firing: map[int64]bool{7: true}}
	_, inhibited, err := InhibitedBy(ctx, 9, nil, stub.firedWithin)
	if err != nil {
		t.Fatalf("InhibitedBy: %v", err)
	}
	if inhibited {
		t.Error("inhibited = true with no pairs configured; deliver")
	}
	// A nil checker is also "nothing known to be firing".
	_, inhibited, err = InhibitedBy(ctx, 9, []store.Inhibition{
		{SourceRuleID: 7, TargetRuleID: 9},
	}, nil)
	if err != nil || inhibited {
		t.Errorf("nil checker: inhibited=%v err=%v, want false/nil", inhibited, err)
	}
}

func TestInhibitedByFirstFiringSourceWins(t *testing.T) {
	ctx := context.Background()
	stub := &stubFiring{firing: map[int64]bool{7: true, 8: true}}
	source, inhibited, err := InhibitedBy(ctx, 9, []store.Inhibition{
		{SourceRuleID: 7, TargetRuleID: 9},
		{SourceRuleID: 8, TargetRuleID: 9},
	}, stub.firedWithin)
	if err != nil || !inhibited {
		t.Fatalf("inhibited=%v err=%v, want true/nil", inhibited, err)
	}
	if source != 7 {
		t.Errorf("source = %d, want the first firing source (7)", source)
	}
}

func TestInhibitedByCycleTerminates(t *testing.T) {
	// A inhibits B inhibits A: single-hop evaluation consults only direct
	// sources, so this returns after two firing checks instead of
	// recursing. Without the single-hop rule this test would hang.
	ctx := context.Background()
	stub := &stubFiring{firing: map[int64]bool{1: true, 2: true}}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _, _ = InhibitedBy(ctx, 1, []store.Inhibition{
			{SourceRuleID: 2, TargetRuleID: 1},
			{SourceRuleID: 1, TargetRuleID: 2},
		}, stub.firedWithin)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("InhibitedBy did not terminate on a cyclic pair")
	}
}

func TestInhibitedByNonTransitive(t *testing.T) {
	// A inhibits B, B inhibits C, only A is firing: C still delivers.
	// Transitive suppression would hide C behind a rule two hops away,
	// which is exactly the silent-drop behaviour inhibition must avoid.
	ctx := context.Background()
	stub := &stubFiring{firing: map[int64]bool{1: true}}
	_, inhibited, err := InhibitedBy(ctx, 3, []store.Inhibition{
		{SourceRuleID: 1, TargetRuleID: 2},
		{SourceRuleID: 2, TargetRuleID: 3},
	}, stub.firedWithin)
	if err != nil {
		t.Fatalf("InhibitedBy: %v", err)
	}
	if inhibited {
		t.Error("inhibited = true via a transitive hop; deliver")
	}
}

func TestInhibitedByZeroWindowFallsBackToDefault(t *testing.T) {
	ctx := context.Background()
	stub := &stubFiring{firing: map[int64]bool{7: true}}
	_, inhibited, err := InhibitedBy(ctx, 9, []store.Inhibition{
		{SourceRuleID: 7, TargetRuleID: 9, FiringWindowSeconds: 0},
	}, stub.firedWithin)
	if err != nil || !inhibited {
		t.Fatalf("inhibited=%v err=%v, want true/nil", inhibited, err)
	}
	want := time.Duration(store.DefaultInhibitionWindowSeconds) * time.Second
	if stub.windows[7] != want {
		t.Errorf("window = %v, want documented default %v", stub.windows[7], want)
	}
}

func TestInhibitedByFiringError(t *testing.T) {
	// A firing-check failure must not silently deliver OR silently
	// suppress: it propagates so the dispatcher can log and deliver.
	ctx := context.Background()
	stub := &stubFiring{err: errors.New("store down")}
	_, _, err := InhibitedBy(ctx, 9, []store.Inhibition{
		{SourceRuleID: 7, TargetRuleID: 9},
	}, stub.firedWithin)
	if err == nil {
		t.Error("expected the firing-check error to propagate")
	}
}
