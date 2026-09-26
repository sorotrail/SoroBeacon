package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseRole(t *testing.T) {
	tests := []struct {
		input    string
		expected Role
		ok       bool
	}{
		{"viewer", RoleViewer, true},
		{"VIEWER", RoleViewer, true},
		{"  viewer  ", RoleViewer, true},
		{"editor", RoleEditor, true},
		{"ADMIN", RoleAdmin, true},
		{"unknown", RoleUnknown, false},
		{"", RoleUnknown, false},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			role, ok := ParseRole(tc.input)
			assert.Equal(t, tc.expected, role)
			assert.Equal(t, tc.ok, ok)
		})
	}
}

func TestRoleHierarchy(t *testing.T) {
	assert.True(t, RoleViewer.HasPermission(RoleViewer))
	assert.False(t, RoleViewer.HasPermission(RoleEditor))
	assert.False(t, RoleViewer.HasPermission(RoleAdmin))

	assert.True(t, RoleEditor.HasPermission(RoleViewer))
	assert.True(t, RoleEditor.HasPermission(RoleEditor))
	assert.False(t, RoleEditor.HasPermission(RoleAdmin))

	assert.True(t, RoleAdmin.HasPermission(RoleViewer))
	assert.True(t, RoleAdmin.HasPermission(RoleEditor))
	assert.True(t, RoleAdmin.HasPermission(RoleAdmin))

	assert.False(t, RoleUnknown.HasPermission(RoleViewer))
}

func TestRoleMiddlewareFailClosedAndUnassigned(t *testing.T) {
	a := New([]string{"token-admin:admin"}, 0)
	re := NewRoleEnforcer(a)
	re.RegisterRoute("GET", "/api/v1/safe", RoleViewer)

	handler := RoleMiddleware(re, RoleViewer)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// Unassigned route should fail closed (403) by default
	reqUnassigned := httptest.NewRequest("GET", "/api/v1/unknown-route", nil)
	reqUnassigned.Header.Set("Authorization", "Bearer token-admin")
	recUnassigned := httptest.NewRecorder()
	handler.ServeHTTP(recUnassigned, reqUnassigned)
	assert.Equal(t, http.StatusForbidden, recUnassigned.Code)

	// Assigned route with proper role should succeed
	reqAssigned := httptest.NewRequest("GET", "/api/v1/safe", nil)
	reqAssigned.Header.Set("Authorization", "Bearer token-admin")
	recAssigned := httptest.NewRecorder()
	handler.ServeHTTP(recAssigned, reqAssigned)
	assert.Equal(t, http.StatusOK, recAssigned.Code)
}
