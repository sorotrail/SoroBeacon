package alerts

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorobeacon/internal/workspace"
)

func TestIsResolved(t *testing.T) {
	ctx := context.Background()
	st := openTestSQLiteForLifecycle(t)
	wsCtx := workspace.With(ctx, "alpha")
	alertID, _ := seedLifecycleAlertWithEscalation(t, st, ctx, "alpha")

	assert.False(t, IsResolved(alertID), "a new alert is not resolved")
	assert.False(t, IsResolved(alertID+1000), "an unknown alert id is not resolved")

	// Acknowledging is not resolving.
	_, err := Acknowledge(wsCtx, st, alertID, "alice")
	require.NoError(t, err)
	assert.False(t, IsResolved(alertID), "an acknowledged alert is not resolved")

	_, err = ResolveAt(wsCtx, st, alertID, "alice", time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	assert.True(t, IsResolved(alertID), "a resolved alert reports resolved")
	assert.False(t, IsResolved(alertID+1000), "resolving one alert does not resolve another")
}

func TestIsResolvedStaysFalseWhenResolveFails(t *testing.T) {
	ctx := context.Background()
	st := openTestSQLiteForLifecycle(t)
	wsCtx := workspace.With(ctx, "alpha")
	alertID, _ := seedLifecycleAlertWithEscalation(t, st, ctx, "alpha")

	// Resolving from another workspace cannot see the alert, so it fails and
	// must not mark it resolved.
	_, err := ResolveAt(workspace.With(ctx, "beta"), st, alertID, "mallory", time.Now().UTC())
	require.Error(t, err)
	assert.False(t, IsResolved(alertID))

	// A nil store is refused before anything is recorded.
	_, err = ResolveAt(wsCtx, nil, alertID, "alice", time.Now().UTC())
	require.Error(t, err)
	assert.False(t, IsResolved(alertID))
}
