package archive

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/sorotrail/sorobeacon/internal/store"
)

func TestBatchKeyEdgeCases(t *testing.T) {
	day := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	tests := []struct {
		name   string
		alerts []store.Alert
		want   string
	}{
		{
			name:   "single alert uses the same id twice",
			alerts: []store.Alert{{ID: 7, CreatedAt: day}},
			want:   "alerts/2026-01-02/0000000007-0000000007.ndjson",
		},
		{
			name:   "range uses first and last ids",
			alerts: []store.Alert{{ID: 1, CreatedAt: day}, {ID: 2, CreatedAt: day}, {ID: 9, CreatedAt: day}},
			want:   "alerts/2026-01-02/0000000001-0000000009.ndjson",
		},
		{
			name:   "day comes from the first alert",
			alerts: []store.Alert{{ID: 1, CreatedAt: day}, {ID: 2, CreatedAt: day.Add(48 * time.Hour)}},
			want:   "alerts/2026-01-02/0000000001-0000000002.ndjson",
		},
		{
			name: "non UTC timestamps are normalised to UTC",
			alerts: []store.Alert{{
				ID:        3,
				CreatedAt: time.Date(2026, 1, 2, 23, 30, 0, 0, time.FixedZone("UTC-5", -5*3600)),
			}},
			want: "alerts/2026-01-03/0000000003-0000000003.ndjson",
		},
		{
			name:   "ids are zero padded to ten digits",
			alerts: []store.Alert{{ID: 1234567890, CreatedAt: day}},
			want:   "alerts/2026-01-02/1234567890-1234567890.ndjson",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, BatchKey(tt.alerts))
		})
	}
}
