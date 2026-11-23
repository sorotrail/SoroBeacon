package auth

import (
	"reflect"
	"strings"
	"testing"

	"github.com/sorotrail/sorobeacon/internal/workspace"
)

func TestValidateGroupMappingsNormalises(t *testing.T) {
	in := []GroupMapping{
		{Group: "  platform-oncall ", Workspace: " acme ", Role: RoleEditor},
		{Group: "auditors", Workspace: "acme-eu", Role: RoleViewer},
		{Group: "sre", Workspace: workspace.Default, Role: RoleAdmin},
	}
	got, err := ValidateGroupMappings(in)
	if err != nil {
		t.Fatalf("ValidateGroupMappings() error = %v", err)
	}
	want := []GroupMapping{
		{Group: "platform-oncall", Workspace: "acme", Role: RoleEditor},
		{Group: "auditors", Workspace: "acme-eu", Role: RoleViewer},
		{Group: "sre", Workspace: workspace.Default, Role: RoleAdmin},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ValidateGroupMappings() = %+v, want %+v", got, want)
	}
	if in[0].Group != "  platform-oncall " {
		t.Fatalf("ValidateGroupMappings modified its input: %+v", in[0])
	}
}

func TestValidateGroupMappingsEmptyInput(t *testing.T) {
	for _, in := range [][]GroupMapping{nil, {}} {
		got, err := ValidateGroupMappings(in)
		if err != nil || got != nil {
			t.Fatalf("ValidateGroupMappings(%v) = %v, %v, want nil, nil", in, got, err)
		}
	}
}

func TestValidateGroupMappingsRejects(t *testing.T) {
	const secretGroup = "secret-group-name"
	valid := GroupMapping{Group: "ok", Workspace: "acme", Role: RoleViewer}
	tests := []struct {
		name    string
		bad     GroupMapping
		wantErr string
	}{
		{name: "empty group", bad: GroupMapping{Group: "   ", Workspace: "acme", Role: RoleViewer}, wantErr: "group name is empty"},
		{name: "empty workspace", bad: GroupMapping{Group: secretGroup, Workspace: "", Role: RoleViewer}, wantErr: "invalid workspace id"},
		{name: "malformed workspace", bad: GroupMapping{Group: secretGroup, Workspace: "Not A Slug!", Role: RoleViewer}, wantErr: "invalid workspace id"},
		{name: "unknown role", bad: GroupMapping{Group: secretGroup, Workspace: "acme", Role: RoleUnknown}, wantErr: "invalid role"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// The bad rule is second, so the error must name rule 2.
			got, err := ValidateGroupMappings([]GroupMapping{valid, tc.bad})
			if err == nil {
				t.Fatalf("ValidateGroupMappings() = %+v, want an error", got)
			}
			if got != nil {
				t.Fatalf("ValidateGroupMappings() returned %+v alongside an error", got)
			}
			msg := err.Error()
			if !strings.Contains(msg, "mapping 2") || !strings.Contains(msg, tc.wantErr) {
				t.Fatalf("error %q should name rule 2 and say %q", msg, tc.wantErr)
			}
			if strings.Contains(msg, secretGroup) {
				t.Fatalf("error %q echoes the raw group name", msg)
			}
		})
	}
}
