package buildinfo

import (
	"testing"
)

func TestNormalizeDate(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{
			name:  "utc rfc3339 unchanged",
			input: "2026-10-21T14:30:00Z",
			want:  "2026-10-21T14:30:00Z",
		},
		{
			name:  "committer offset normalized to utc",
			input: "2026-10-21T16:30:00+02:00",
			want:  "2026-10-21T14:30:00Z",
		},
		{
			name:  "source_date_epoch unix seconds normalized to utc",
			input: "1792593000",
			want:  "2026-10-21T14:30:00Z",
		},
		{
			name:    "empty rejected",
			input:   "   ",
			wantErr: true,
		},
		{
			name:    "invalid string rejected",
			input:   "not-a-timestamp",
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeDate(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got %q", tc.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for %q: %v", tc.input, err)
			}
			if got != tc.want {
				t.Fatalf("NormalizeDate(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestCurrentNormalizesDateWhenValid(t *testing.T) {
	origVersion, origCommit, origDate := Version, Commit, Date
	t.Cleanup(func() {
		Version, Commit, Date = origVersion, origCommit, origDate
	})

	Version = "v1.4.0"
	Commit = "a1b2c3d4e5f6"
	Date = "2026-10-21T16:30:00+02:00"

	got := Current()
	if got.Version != "v1.4.0" || got.Commit != "a1b2c3d4e5f6" || got.Date != "2026-10-21T14:30:00Z" {
		t.Fatalf("Current() = %+v, want normalized UTC build_date", got)
	}

	Date = "unknown"
	gotUnknown := Current()
	if gotUnknown.Date != "unknown" {
		t.Fatalf("Current().Date = %q, want unknown fallback", gotUnknown.Date)
	}
}
