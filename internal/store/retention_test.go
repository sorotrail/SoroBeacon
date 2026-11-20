package store_test

import (
	"testing"

	"github.com/sorotrail/sorobeacon/internal/store"
	"github.com/stretchr/testify/require"
)

func TestFormatRetentionSummaryHappyPath(t *testing.T) {
	policy := store.WorkspaceRetentionPolicy{
		WorkspaceID:               "ws123",
		PolicySource:              "workspace",
		EffectiveRetention:        "24h",
		EffectiveRetentionSeconds: 86400,
	}

	summary := store.FormatRetentionSummary(policy)
	require.Contains(t, summary, "workspace=ws123")
	require.Contains(t, summary, "policy_source=workspace")
	require.Contains(t, summary, "retention=24h")
}

func TestFormatRetentionSummaryErrorPath(t *testing.T) {
	policy := store.WorkspaceRetentionPolicy{
		WorkspaceID:               "ws456",
		PolicySource:              "unset",
		EffectiveRetentionSeconds: 0,
	}

	summary := store.FormatRetentionSummary(policy)
	require.Contains(t, summary, "workspace=ws456")
	require.Contains(t, summary, "policy_source=unset")
	require.Contains(t, summary, "retention=unset")
}

func TestFormatRetentionSummaryInstanceSource(t *testing.T) {
	policy := store.WorkspaceRetentionPolicy{
		WorkspaceID:               "ws789",
		PolicySource:              "instance",
		EffectiveRetention:        "7d",
		EffectiveRetentionSeconds: 604800,
	}

	summary := store.FormatRetentionSummary(policy)
	require.Contains(t, summary, "workspace=ws789")
	require.Contains(t, summary, "policy_source=instance")
	require.Contains(t, summary, "retention=7d")
}
