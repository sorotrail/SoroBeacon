package faultinject

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// TestChaosSuiteRegressionSeeds runs the promoted regression seed corpus in
// standard `go test -race ./...` runs, verifying that composed faults across
// all 7 domains preserve every universal invariant and replay deterministically.
func TestChaosSuiteRegressionSeeds(t *testing.T) {
	regressionSeeds := []int64{
		1,
		42,
		611,
		1337,
		20260410,
		987654321,
	}

	for _, seed := range regressionSeeds {
		seed := seed
		t.Run(ReplayCommand(seed), func(t *testing.T) {
			// Log the seed before running so a panic or timeout never hides it.
			t.Logf("chaos_run_starting seed=%d replay_cmd=%q", seed, ReplayCommand(seed))

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			cfg := DefaultChaosConfig(seed)
			res1, err := RunChaosSuite(ctx, cfg)
			if err != nil {
				t.Fatalf("chaos suite failed for seed %d:\n%v", seed, err)
			}

			if !strings.Contains(res1.LogOutput, "chaos_suite_starting") {
				t.Errorf("expected pre-run seed log entry in LogOutput for seed %d", seed)
			}

			// Verify all 7 fault domains were composed during the run.
			domainsSeen := make(map[FaultDomain]int)
			for _, ev := range res1.FaultSequence {
				domainsSeen[ev.Domain]++
			}
			requiredDomains := []FaultDomain{
				DomainChannelDelivery,
				DomainDatabase,
				DomainLeaderLease,
				DomainRPC,
				DomainEscalation,
				DomainDigestFlush,
				DomainProcessRestart,
			}
			for _, d := range requiredDomains {
				if domainsSeen[d] == 0 {
					t.Errorf("seed %d did not exercise required fault domain %q", seed, d)
				}
			}

			// Verify deterministic replay: running the same seed a second time
			// must produce the identical fault sequence and alert outcomes.
			res2, err := RunChaosSuite(ctx, cfg)
			if err != nil {
				t.Fatalf("replay of seed %d failed:\n%v", seed, err)
			}
			if len(res1.FaultSequence) != len(res2.FaultSequence) {
				t.Fatalf("non-deterministic fault sequence length for seed %d: %d vs %d",
					seed, len(res1.FaultSequence), len(res2.FaultSequence))
			}
			for i := range res1.FaultSequence {
				if res1.FaultSequence[i] != res2.FaultSequence[i] {
					t.Fatalf("fault sequence diverged at index %d for seed %d:\n  first:  %+v\n  second: %+v",
						i, seed, res1.FaultSequence[i], res2.FaultSequence[i])
				}
			}
			if len(res1.Deliveries) != len(res2.Deliveries) || len(res1.DeadLetters) != len(res2.DeadLetters) {
				t.Fatalf("non-deterministic outcome counts for seed %d", seed)
			}
		})
	}
}

// TestChaosSuiteReplay is the entry point targeted by ReplayCommand(seed) and
// scheduled chaos runs:
//   - Set SOROBEACON_CHAOS_SEED=<int64> to replay a specific failing seed.
//   - Set SOROBEACON_CHAOS_DURATION=<duration> (e.g. "30s", "5m") for scheduled
//     soak runs that sweep seeds continuously and log each seed before starting.
func TestChaosSuiteReplay(t *testing.T) {
	ctx := context.Background()

	seed, hasSeed, err := SeedFromEnv()
	if err != nil {
		t.Fatalf("parse SOROBEACON_CHAOS_SEED: %v", err)
	}
	if !hasSeed {
		seed = 611
	}

	t.Logf("chaos_replay_starting seed=%d replay_cmd=%q", seed, ReplayCommand(seed))
	res, err := RunChaosSuite(ctx, DefaultChaosConfig(seed))
	if err != nil {
		t.Fatalf("replay failed:\n%v", err)
	}
	if reportJSON, mErr := res.MarshalReport(); mErr != nil || len(reportJSON) == 0 {
		t.Fatalf("MarshalReport failed: %v", mErr)
	}

	if rawDur := strings.TrimSpace(os.Getenv("SOROBEACON_CHAOS_DURATION")); rawDur != "" {
		dur, err := time.ParseDuration(rawDur)
		if err != nil {
			t.Fatalf("invalid SOROBEACON_CHAOS_DURATION %q: %v", rawDur, err)
		}
		deadline := time.Now().Add(dur)
		nextSeed := time.Now().UnixNano()
		runs := 0
		for time.Now().Before(deadline) {
			runs++
			runSeed := nextSeed + int64(runs)
			// Log seed BEFORE starting the run so a timeout/panic preserves it.
			t.Logf("scheduled_chaos_seed_starting run=%d seed=%d replay_cmd=%q",
				runs, runSeed, ReplayCommand(runSeed))
			if _, err := RunChaosSuite(ctx, DefaultChaosConfig(runSeed)); err != nil {
				t.Fatalf("scheduled chaos failed on run %d (seed=%d):\n%v", runs, runSeed, err)
			}
		}
	}
}

// TestChaosInvariantDetectors mutates a valid ChaosResult to prove that each
// of the universal invariants fails loudly with seed, replay command, fault
// sequence, and broken invariant details when violated.
func TestChaosInvariantDetectors(t *testing.T) {
	ctx := context.Background()
	base, err := RunChaosSuite(ctx, DefaultChaosConfig(42))
	if err != nil {
		t.Fatalf("baseline seed 42 failed: %v", err)
	}

	cloneResult := func() *ChaosResult {
		c := *base
		c.Alerts = append([]ChaosAlert(nil), base.Alerts...)
		c.Deliveries = append([]ChaosDeliveryReceipt(nil), base.Deliveries...)
		c.DeadLetters = append([]ChaosDeadLetter(nil), base.DeadLetters...)
		c.FaultSequence = append([]FaultEvent(nil), base.FaultSequence...)
		c.secretCanaries = append([]string(nil), base.secretCanaries...)
		return &c
	}

	assertInvariantFailure := func(t *testing.T, r *ChaosResult, want InvariantID, wantSubstr string) {
		t.Helper()
		err := r.CheckInvariants()
		if err == nil {
			t.Fatalf("expected CheckInvariants to fail with %s, but it passed", want)
		}
		var failure *ChaosFailureError
		if !errors.As(err, &failure) {
			t.Fatalf("expected *ChaosFailureError, got %T: %v", err, err)
		}
		found := false
		for _, v := range failure.Violations {
			if v.Invariant == want && strings.Contains(v.Description, wantSubstr) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("expected violation %s containing %q in:\n%s", want, wantSubstr, failure.Error())
		}
		formatted := failure.Error()
		if !strings.Contains(formatted, ReplayCommand(r.Seed)) {
			t.Errorf("expected failure output to include replay command %q, got:\n%s", ReplayCommand(r.Seed), formatted)
		}
		if !strings.Contains(formatted, "fault_sequence") {
			t.Errorf("expected failure output to include fault_sequence, got:\n%s", formatted)
		}
	}

	t.Run("detects undelivered and un-dead-lettered alert", func(t *testing.T) {
		r := cloneResult()
		r.Alerts = append(r.Alerts, ChaosAlert{
			ID:          99999,
			WorkspaceID: "ws-alpha",
			EventID:     "evt-lost-in-flight",
			ChannelID:   100,
		})
		assertInvariantFailure(t, r, InvariantTerminalOutcome, "neither delivered nor dead-lettered")
	})

	t.Run("detects stranded pending digest alerts", func(t *testing.T) {
		r := cloneResult()
		r.PendingDigests = 2
		assertInvariantFailure(t, r, InvariantTerminalOutcome, "stranded in pending_digests")
	})

	t.Run("detects duplicate delivery without deduplication key", func(t *testing.T) {
		r := cloneResult()
		target := r.Alerts[0]
		r.Deliveries = append(r.Deliveries,
			ChaosDeliveryReceipt{
				AlertID:     target.ID,
				ChannelID:   target.ChannelID,
				WorkspaceID: target.WorkspaceID,
				DedupKey:    "",
				StepIndex:   0,
				DeliveredAt: time.Now().UTC(),
			},
			ChaosDeliveryReceipt{
				AlertID:     target.ID,
				ChannelID:   target.ChannelID,
				WorkspaceID: target.WorkspaceID,
				DedupKey:    "",
				StepIndex:   0,
				DeliveredAt: time.Now().UTC().Add(time.Millisecond),
			},
		)
		assertInvariantFailure(t, r, InvariantNoDuplicateWithout, "without a deduplication key")
	})

	t.Run("detects duplicate delivery reusing already-committed dedup key", func(t *testing.T) {
		r := cloneResult()
		dup := r.Deliveries[0]
		dup.DeliveredAt = dup.DeliveredAt.Add(5 * time.Second)
		r.Deliveries = append(r.Deliveries, dup)
		assertInvariantFailure(t, r, InvariantNoDuplicateWithout, "already-committed dedup key")
	})

	t.Run("detects acknowledged alert escalating afterwards", func(t *testing.T) {
		r := cloneResult()
		ackTime := time.Date(2026, time.January, 15, 12, 5, 0, 0, time.UTC)
		r.Alerts[0].AcknowledgedAt = &ackTime
		r.Deliveries = append(r.Deliveries, ChaosDeliveryReceipt{
			AlertID:     r.Alerts[0].ID,
			ChannelID:   r.Alerts[0].EscalationChID,
			WorkspaceID: r.Alerts[0].WorkspaceID,
			DedupKey:    "dedup:late-escalation",
			StepIndex:   2,
			IsEscalated: true,
			DeliveredAt: ackTime.Add(100 * time.Millisecond),
		})
		assertInvariantFailure(t, r, InvariantAckStopsEscalation, "escalated at")
	})

	t.Run("detects cross-workspace delivery leak", func(t *testing.T) {
		r := cloneResult()
		r.Deliveries[0].WorkspaceID = "ws-foreign-tenant"
		assertInvariantFailure(t, r, InvariantWorkspaceIsolation, "cross-workspace delivery leak")
	})

	t.Run("detects channel secret leaked into logs or dead-letter error", func(t *testing.T) {
		r := cloneResult()
		r.LogOutput += "\nleaked secret: " + r.secretCanaries[0]
		assertInvariantFailure(t, r, InvariantSecretRedaction, "leaked sensitive channel config secret")
	})
}
