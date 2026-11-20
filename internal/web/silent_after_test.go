package web

import (
	"testing"
	"time"
)

func TestWithSilentAfter(t *testing.T) {
	const def = 24 * time.Hour
	tests := []struct {
		name string
		in   time.Duration
		want time.Duration
	}{
		{"positive value is applied", 2 * time.Hour, 2 * time.Hour},
		{"zero is ignored", 0, def},
		{"negative is ignored", -time.Minute, def},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t)
			if got := s.SilentAfter(); got != def {
				t.Fatalf("default SilentAfter = %v, want %v", got, def)
			}
			if ret := s.WithSilentAfter(tc.in); ret != s {
				t.Errorf("WithSilentAfter must return the same *Server for chaining")
			}
			if got := s.SilentAfter(); got != tc.want {
				t.Errorf("SilentAfter = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestWithSilentAfterKeepsPreviousOnInvalid(t *testing.T) {
	s := newTestServer(t).WithSilentAfter(time.Hour).WithSilentAfter(0).WithSilentAfter(-time.Second)
	if got := s.SilentAfter(); got != time.Hour {
		t.Errorf("SilentAfter = %v, want %v", got, time.Hour)
	}
}
