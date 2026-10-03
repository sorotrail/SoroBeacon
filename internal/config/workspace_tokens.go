// Workspace credentials: the WORKSPACE_TOKENS setting, and the resolution
// that turns the two token settings into the single list the authenticator is
// built from.
//
// It lives in its own file so config.go keeps holding only plain
// configuration; the methods below are derived views over fields Load has
// already validated.
package config

import (
	"fmt"
	"strings"

	"github.com/sorotrail/sorobeacon/internal/auth"
	"github.com/sorotrail/sorobeacon/internal/workspace"
)

// WorkspaceToken is one entry of WORKSPACE_TOKENS: a token plus the
// workspace it selects. The auth package's Binding is the same pair seen from
// the credential side, so AuthBindings exists to hand this list to it without
// making the store or the handlers reach through configuration.
type WorkspaceToken struct {
	Workspace workspace.ID
	Token     string
}

// AuthBindings returns every configured credential as its workspace, in the
// order an operator wrote them: API_TOKEN entries first (each selecting the
// default workspace), then WORKSPACE_TOKENS. It re-checks that no token names
// two workspaces, so a Config assembled by hand rather than by Load cannot
// slip past the validation Load runs.
func (c Config) AuthBindings() ([]auth.Binding, error) {
	if err := checkTokenWorkspaces(c.APITokens, c.WorkspaceTokens); err != nil {
		return nil, err
	}
	bindings := make([]auth.Binding, 0, len(c.APITokens)+len(c.WorkspaceTokens))
	for _, t := range c.APITokens {
		bindings = append(bindings, auth.Binding{Workspace: workspace.Default, Token: t})
	}
	for _, wt := range c.WorkspaceTokens {
		bindings = append(bindings, auth.Binding{Workspace: wt.Workspace, Token: wt.Token})
	}
	return bindings, nil
}

// Workspaces lists every workspace the configured credentials name, default
// first. It is what startup seeds the workspaces table with, so the table
// describes the tenants an instance actually serves rather than only the
// implicit one.
func (c Config) Workspaces() []workspace.ID {
	seen := map[workspace.ID]bool{}
	ids := make([]workspace.ID, 0, 1+len(c.WorkspaceTokens))
	if len(c.APITokens) > 0 || len(c.WorkspaceTokens) == 0 {
		// The default workspace is in play when an unscoped token exists, and
		// always when no credential exists at all: an open instance serves
		// the single workspace everything already lives in.
		seen[workspace.Default] = true
		ids = append(ids, workspace.Default)
	}
	for _, wt := range c.WorkspaceTokens {
		if !seen[wt.Workspace] {
			seen[wt.Workspace] = true
			ids = append(ids, wt.Workspace)
		}
	}
	// A single sign-on mapping names a tenant too: with OIDC_WORKSPACE set to a
	// team that has no static token, that workspace is still one this instance
	// serves, and startup should record it rather than let the first signed-in
	// user be the first to discover it is absent from the table.
	if c.OIDC.Enabled() && !seen[c.OIDC.Workspace] {
		seen[c.OIDC.Workspace] = true
		ids = append(ids, c.OIDC.Workspace)
	}
	return ids
}

// checkTokenWorkspaces rejects a token that names more than one workspace.
//
// The check has to exist because the two settings are separate: the same value
// in API_TOKEN and in WORKSPACE_TOKENS would authenticate against both, and
// resolution would then depend on which entry the comparison happened to hit.
// An error here is a credential that has to be rotated, so the message names
// the workspaces in conflict and never the token.
func checkTokenWorkspaces(unscoped []string, bound []WorkspaceToken) error {
	owner := map[string]workspace.ID{}
	for _, t := range unscoped {
		owner[t] = workspace.Default
	}
	for _, wt := range bound {
		if prev, ok := owner[wt.Token]; ok && prev != wt.Workspace {
			return fmt.Errorf("invalid WORKSPACE_TOKENS: one token is configured for both workspace %q and workspace %q; a token must name exactly one workspace", prev, wt.Workspace)
		}
		owner[wt.Token] = wt.Workspace
	}
	return nil
}

// parseWorkspaceTokens splits WORKSPACE_TOKENS on commas and each entry on its
// first "=", giving workspace=token. The separator is chosen so it cannot
// collide with the id grammar (workspace.Valid rejects "=" outright), which
// makes a token free to contain any other character — and lets a misconfigured
// id surface as an error instead of silently reading as part of the token.
//
// Unset or empty means single-tenant: every API_TOKEN entry then selects the
// default workspace. A value that is set but yields no usable entry is an
// error, exactly as with API_TOKEN, because the operator clearly meant to scope
// something. Errors never echo token material.
func parseWorkspaceTokens(raw string) ([]WorkspaceToken, error) {
	var out []WorkspaceToken
	for i, entry := range strings.Split(raw, ",") {
		if entry = strings.TrimSpace(entry); entry == "" {
			continue
		}
		// Errors name the entry by position, never by value. Either half can
		// hold token material — an operator who wrote token=workspace gets an
		// "invalid workspace id" for what is actually their secret — and the
		// index is all it takes to find the entry in the setting.
		where := fmt.Sprintf("invalid WORKSPACE_TOKENS entry %d", i+1)
		id, token, ok := strings.Cut(entry, "=")
		if !ok {
			return nil, fmt.Errorf("%s: no \"=\" separator (want comma-separated workspace=token pairs)", where)
		}
		rawID := strings.TrimSpace(id)
		if rawID == "" {
			return nil, fmt.Errorf("%s: the workspace id is empty (want workspace=token)", where)
		}
		// Parse maps "" onto the default workspace, which is why the empty id
		// is rejected above rather than by it: an entry with no left-hand side
		// did not ask to belong to the default tenant, it forgot to say.
		ws, err := workspace.Parse(rawID)
		if err != nil {
			return nil, fmt.Errorf("%s: its workspace id is not one (workspace ids are lowercase letters, digits, hyphen and underscore, starting with a letter or digit)", where)
		}
		if token = strings.TrimSpace(token); token == "" {
			return nil, fmt.Errorf("%s: the token for workspace %q is empty", where, ws)
		}
		out = append(out, WorkspaceToken{Workspace: ws, Token: token})
	}
	if raw != "" && len(out) == 0 {
		return nil, fmt.Errorf("invalid WORKSPACE_TOKENS: set but contains no entries (use comma-separated workspace=token pairs, or unset it to run single-tenant)")
	}
	return out, nil
}
