package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorobeacon/internal/auth"
	"github.com/sorotrail/sorobeacon/internal/workspace"
)

// testAPITokens is the storage half of scoped tokens, run once per backend.
//
// What matters here is not that a row can be written — it is that the row holds a
// digest and nothing else, that the *authentication* read is the one read not
// filtered by tenant (because a request has no tenant until this row answers),
// and that every other read and write is. Those three properties are the whole
// security argument of the feature, and both backends have to agree on all of
// them.
func testAPITokens(t *testing.T, newStore conformanceFactory) {
	st := newStore(t)
	bg := context.Background()
	acme := workspace.With(bg, "acme")
	beta := workspace.With(bg, "beta")

	// token builds a row the way auth.Manager does: hash in, secret nowhere.
	token := func(name, secret string, expires time.Time) *auth.Token {
		return &auth.Token{
			Name:      name,
			Prefix:    auth.TokenPrefix + secret[:8],
			Hash:      auth.HashToken(secret),
			Scopes:    []auth.Scope{auth.ScopeMonitorsRead, auth.ScopeAlertsWrite},
			ExpiresAt: expires,
		}
	}

	const acmeSecret = "sb_acmesecretvalue000000000000000000000000000000"
	const betaSecret = "sb_betasecretvalue0000000000000000000000000000000"

	saved := token("ci", acmeSecret, time.Time{})
	require.NoError(t, st.CreateAPIToken(acme, saved))
	assert.NotZero(t, saved.ID, "the insert must fill the caller's row id")
	assert.False(t, saved.CreatedAt.IsZero(), "created_at comes back from the insert")
	assert.Equal(t, workspace.ID("acme"), saved.Workspace,
		"the row's tenant is decided by the context, not by the caller")

	// A row with an expiry, so the NULL and the value are both exercised.
	withExpiry := token("nightly", betaSecret, time.Now().UTC().Add(time.Hour).Truncate(time.Second))
	require.NoError(t, st.CreateAPIToken(beta, withExpiry))

	// --- authentication read ---

	got, found, err := st.TokenByHash(bg, auth.HashToken(acmeSecret))
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "ci", got.Name)
	assert.Equal(t, workspace.ID("acme"), got.Workspace,
		"the tenant a request gets comes from this row and from nowhere else")
	assert.Equal(t, saved.Scopes, got.Scopes, "the scope list survives the comma-separated column")
	assert.Equal(t, auth.HashToken(acmeSecret), got.Hash)
	assert.NotContains(t, got.Name+got.Prefix+got.Hash, acmeSecret[len(auth.TokenPrefix):],
		"no column or field of a token row holds the secret")
	// The lookup is by digest, so presenting the raw secret must find nothing.
	_, found, err = st.TokenByHash(bg, acmeSecret)
	require.NoError(t, err)
	assert.False(t, found, "the plaintext is not a key")

	// Unknown digest is "not found", not an error: an invalid credential and a
	// database failure have to stay distinguishable by the caller.
	_, found, err = st.TokenByHash(bg, auth.HashToken("sb_nobody-has-this"))
	require.NoError(t, err)
	assert.False(t, found)

	// Null expiry reads back as the zero time, which is what auth.Token.Live and
	// the dashboard's "never expires" both key off.
	assert.True(t, got.ExpiresAt.IsZero(), "a NULL expires_at must be the zero time, not year 1")
	assert.True(t, got.LastUsedAt.IsZero())
	assert.True(t, got.RevokedAt.IsZero())
	live, found, err := st.TokenByHash(bg, auth.HashToken(betaSecret))
	require.NoError(t, err)
	require.True(t, found)
	assert.WithinDuration(t, withExpiry.ExpiresAt, live.ExpiresAt, time.Second,
		"an expiry survives the round trip")

	// TokenByHash ignores the context's tenant on purpose: it is what *discovers*
	// the tenant. Assert the deliberate behaviour so a later "harden this" edit
	// has to change this line and think about it.
	acmeRow, found, err := st.TokenByHash(beta, auth.HashToken(acmeSecret))
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, workspace.ID("acme"), acmeRow.Workspace,
		"a beta-scoped context still resolves acme's credential, whose own tenant then wins")

	// --- the tenant-scoped reads and writes ---

	listAcme, err := st.ListAPITokens(acme)
	require.NoError(t, err)
	require.Len(t, listAcme, 1, "a tenant lists its own tokens only")
	assert.Equal(t, "ci", listAcme[0].Name)

	listBeta, err := st.ListAPITokens(beta)
	require.NoError(t, err)
	require.Len(t, listBeta, 1)
	assert.Equal(t, "nightly", listBeta[0].Name)

	// Newest first, so a dashboard page shows what was just minted.
	third := token("third", "sb_thirdsecretvalue00000000000000000000000000000", time.Time{})
	require.NoError(t, st.CreateAPIToken(acme, third))
	listAcme, err = st.ListAPITokens(acme)
	require.NoError(t, err)
	require.Len(t, listAcme, 2)
	assert.Equal(t, "third", listAcme[0].Name, "the listing is ordered by descending id")

	// Touch records use, and only within the tenant.
	at := time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Second)
	require.NoError(t, st.TouchAPIToken(acme, saved.ID, at))
	used, found, err := st.TokenByHash(bg, auth.HashToken(acmeSecret))
	require.NoError(t, err)
	require.True(t, found)
	assert.WithinDuration(t, at, used.LastUsedAt, time.Second)

	require.NoError(t, st.TouchAPIToken(beta, saved.ID, time.Now()))
	untouched, found, err := st.TokenByHash(bg, auth.HashToken(acmeSecret))
	require.NoError(t, err)
	require.True(t, found)
	assert.WithinDuration(t, at, untouched.LastUsedAt, time.Second,
		"a cross-tenant touch must not move another workspace's row")

	// Revoke: idempotent, keeps the first timestamp, and invisible across tenants.
	require.ErrorIs(t, st.RevokeAPIToken(beta, saved.ID), ErrNotFound,
		"another workspace's token does not exist here — the same answer a missing id gives")
	require.NoError(t, st.RevokeAPIToken(acme, saved.ID))
	first, found, err := st.TokenByHash(bg, auth.HashToken(acmeSecret))
	require.NoError(t, err)
	require.True(t, found)
	require.False(t, first.RevokedAt.IsZero(), "revoking must leave a timestamp, not a flag")

	require.NoError(t, st.RevokeAPIToken(acme, saved.ID), "revoking twice is not a failure")
	second, found, err := st.TokenByHash(bg, auth.HashToken(acmeSecret))
	require.NoError(t, err)
	require.True(t, found)
	assert.True(t, second.RevokedAt.Equal(first.RevokedAt),
		"the second revocation must not move the audit timestamp: %v then %v", first.RevokedAt, second.RevokedAt)
	assert.ErrorIs(t, st.RevokeAPIToken(acme, 1<<40), ErrNotFound)

	// Revocation keeps the row: the listing still shows it, which is the whole
	// point of auditing a retired credential.
	listAcme, err = st.ListAPITokens(acme)
	require.NoError(t, err)
	assert.Len(t, listAcme, 2, "a revoked token stays listed")

	// A context with no workspace is the default tenant, as everywhere else.
	_, err = st.ListAPITokens(bg)
	require.NoError(t, err)
}
