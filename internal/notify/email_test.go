package notify

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEmailSubjectPrefix(t *testing.T) {
	tests := []struct {
		name   string
		config string
		want   string
	}{
		{
			name:   "no prefix configured",
			config: `{"host":"smtp.example.com","from":"beacon@example.com","to":["ops@example.com"]}`,
			want:   "SoroBeacon alert: my-monitor",
		},
		{
			name:   "prefix configured",
			config: `{"host":"smtp.example.com","from":"beacon@example.com","to":["ops@example.com"],"subject_prefix":"[PROD] "}`,
			want:   "[PROD] SoroBeacon alert: my-monitor",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n, err := NewEmail(json.RawMessage(tt.config))
			require.NoError(t, err)
			e, ok := n.(*Email)
			require.True(t, ok)

			got := e.subject(Alert{MonitorName: "my-monitor"})
			assert.Equal(t, tt.want, got)
		})
	}
}
