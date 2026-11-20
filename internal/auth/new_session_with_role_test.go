package auth

import (
	"testing"
	"time"

	"github.com/sorotrail/sorobeacon/internal/workspace"
)

// NewSessionWithRole stores whatever role it is handed; it does not validate
// or upgrade it. These edge cases pin that down, along with the expiry
// boundary every minted session is subject to.
func TestNewSessionWithRoleEdgeCases(t *testing.T) {
	tests := []struct {
		name     string
		role     Role
		wantRole Role
	}{
		{
			// The zero value must not be silently promoted to a usable role:
			// a session minted without a real role holds no permission.
			name:     "the zero role stays RoleUnknown",
			role:     RoleUnknown,
			wantRole: RoleUnknown,
		},
		{
			name:     "the lowest real role is kept",
			role:     RoleViewer,
			wantRole: RoleViewer,
		},
		{
			name:     "the highest real role is kept",
			role:     RoleAdmin,
			wantRole: RoleAdmin,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := New([]string{"static-token"}, 0)
			id := a.NewSessionWithRole(tc.role)
			if id == "" {
				t.Fatal("NewSessionWithRole returned no session id")
			}
			role, ok := a.SessionRole(id)
			if !ok {
				t.Fatal("the session must be live")
			}
			if role != tc.wantRole {
				t.Fatalf("session role = %v, want %v", role, tc.wantRole)
			}
			ws, ok := a.SessionWorkspace(id)
			if !ok || ws != workspace.Default {
				t.Fatalf("session workspace = (%q, %v), want (%q, true)", ws, ok, workspace.Default)
			}
		})
	}
}

// Two sessions minted with the same role are still two sessions: their ids
// must differ, or signing one out would sign out the other.
func TestNewSessionWithRoleMintsDistinctIDs(t *testing.T) {
	a := New([]string{"static-token"}, 0)
	first := a.NewSessionWithRole(RoleEditor)
	second := a.NewSessionWithRole(RoleEditor)
	if first == "" || second == "" {
		t.Fatal("NewSessionWithRole returned no session id")
	}
	if first == second {
		t.Fatalf("two sessions share the id %q", first)
	}
}

// A session is live strictly before its TTL elapses and dead at the exact
// boundary, so the expiry check is pinned at both sides of the edge.
func TestNewSessionWithRoleExpiresAtTheTTLBoundary(t *testing.T) {
	const ttl = time.Minute
	now := time.Unix(1_700_000_000, 0)

	tests := []struct {
		name     string
		elapsed  time.Duration
		wantLive bool
	}{
		{name: "one nanosecond before the TTL is live", elapsed: ttl - time.Nanosecond, wantLive: true},
		{name: "exactly at the TTL is expired", elapsed: ttl, wantLive: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := New([]string{"static-token"}, ttl)
			a.now = func() time.Time { return now }
			id := a.NewSessionWithRole(RoleViewer)

			a.now = func() time.Time { return now.Add(tc.elapsed) }
			_, ok := a.SessionRole(id)
			if ok != tc.wantLive {
				t.Fatalf("session live = %v after %v, want %v", ok, tc.elapsed, tc.wantLive)
			}
		})
	}
}
