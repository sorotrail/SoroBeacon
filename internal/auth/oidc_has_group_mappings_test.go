package auth

import "testing"

func TestOIDCProviderHasGroupMappings(t *testing.T) {
	t.Run("nil provider", func(t *testing.T) {
		var p *OIDCProvider
		if p.HasGroupMappings() {
			t.Fatal("nil provider must report no group mappings")
		}
	})

	t.Run("no mappings configured", func(t *testing.T) {
		p := &OIDCProvider{}
		if p.HasGroupMappings() {
			t.Fatal("provider without mappings must report false")
		}
	})

	t.Run("mappings set", func(t *testing.T) {
		p := &OIDCProvider{}
		err := p.SetGroupMappings([]GroupMapping{
			{Group: "prod-viewers", Workspace: "prod", Role: RoleViewer},
		})
		if err != nil {
			t.Fatalf("SetGroupMappings: %v", err)
		}
		if !p.HasGroupMappings() {
			t.Fatal("provider with mappings must report true")
		}
	})

	t.Run("rejected mappings keep it false", func(t *testing.T) {
		p := &OIDCProvider{}
		if err := p.SetGroupMappings([]GroupMapping{{Group: "", Workspace: "prod", Role: RoleViewer}}); err == nil {
			t.Skip("empty group is accepted by validation; nothing to assert")
		}
		if p.HasGroupMappings() {
			t.Fatal("rejected mappings must not be activated")
		}
	})

	t.Run("clearing mappings", func(t *testing.T) {
		p := &OIDCProvider{}
		if err := p.SetGroupMappings([]GroupMapping{{Group: "g", Workspace: "prod", Role: RoleViewer}}); err != nil {
			t.Fatalf("SetGroupMappings: %v", err)
		}
		if err := p.SetGroupMappings(nil); err != nil {
			t.Fatalf("SetGroupMappings(nil): %v", err)
		}
		if p.HasGroupMappings() {
			t.Fatal("cleared mappings must report false")
		}
	})
}
