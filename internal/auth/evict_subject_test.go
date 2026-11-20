package auth

import (
	"testing"
	"time"
)

// EvictSubject is what forces a subject's next request to re-resolve its
// groups instead of trusting the cache until GroupCacheTTL runs out, so it
// must remove exactly the named subject and must be a safe no-op on the
// inputs it rejects.
func TestEvictSubject(t *testing.T) {
	seed := func() *OIDCProvider {
		expires := time.Now().Add(time.Hour)
		return &OIDCProvider{
			groupCache: map[string]cachedGroupResolution{
				"alice": {role: RoleEditor, expiresAt: expires},
				"bob":   {role: RoleViewer, expiresAt: expires},
			},
		}
	}

	tests := []struct {
		name      string
		subject   string
		wantKept  []string
		wantGone  []string
		wantCount int
	}{
		{
			name:      "a cached subject is evicted and the others are kept",
			subject:   "alice",
			wantKept:  []string{"bob"},
			wantGone:  []string{"alice"},
			wantCount: 1,
		},
		{
			name:      "an empty subject is ignored",
			subject:   "",
			wantKept:  []string{"alice", "bob"},
			wantCount: 2,
		},
		{
			name:      "an unknown subject is a no-op",
			subject:   "mallory",
			wantKept:  []string{"alice", "bob"},
			wantCount: 2,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := seed()
			p.EvictSubject(tc.subject)

			for _, s := range tc.wantKept {
				if _, ok := p.groupCache[s]; !ok {
					t.Errorf("subject %q was evicted, want it kept", s)
				}
			}
			for _, s := range tc.wantGone {
				if _, ok := p.groupCache[s]; ok {
					t.Errorf("subject %q is still cached, want it evicted", s)
				}
			}
			if got := len(p.groupCache); got != tc.wantCount {
				t.Errorf("cache holds %d entries, want %d", got, tc.wantCount)
			}
		})
	}
}

// A nil provider is the unconfigured case: evicting from it must not panic.
func TestEvictSubjectOnNilProvider(t *testing.T) {
	var p *OIDCProvider
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("EvictSubject on a nil provider panicked: %v", r)
		}
	}()
	p.EvictSubject("alice")
}
