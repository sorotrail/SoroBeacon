package auth

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestAtLeast(t *testing.T) {
	tests := []struct {
		name     string
		role     Role
		required Role
		want     bool
	}{
		{name: "admin is at least viewer", role: RoleAdmin, required: RoleViewer, want: true},
		{name: "editor is at least editor", role: RoleEditor, required: RoleEditor, want: true},
		{name: "viewer is at least open", role: RoleViewer, required: RoleOpen, want: true},
		{name: "unknown is at least open", role: RoleUnknown, required: RoleOpen, want: true},
		{name: "viewer is not at least editor", role: RoleViewer, required: RoleEditor, want: false},
		{name: "editor is not at least admin", role: RoleEditor, required: RoleAdmin, want: false},
		{name: "unknown is not at least viewer", role: RoleUnknown, required: RoleViewer, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.role.AtLeast(tt.required))
		})
	}
}
