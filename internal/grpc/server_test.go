package grpc

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/sorotrail/sorobeacon/internal/auth"
	"github.com/sorotrail/sorobeacon/internal/store"
)

func TestCheckAuth_NoAuthConfigured(t *testing.T) {
	s := &Server{auth: nil}
	err := s.checkAuth(context.Background())
	assert.NoError(t, err, "nil authenticator should allow all requests")
}

func TestCheckAuth_DisabledAuth(t *testing.T) {
	// An authenticator with no tokens is effectively disabled.
	authn := auth.New(nil, 0)
	s := &Server{auth: authn}
	err := s.checkAuth(context.Background())
	assert.NoError(t, err, "disabled authenticator should allow all requests")
}

func TestCheckAuth_ValidToken(t *testing.T) {
	authn := auth.New([]string{"test-token-123"}, 0)
	s := &Server{auth: authn}

	md := metadata.New(map[string]string{"authorization": "Bearer test-token-123"})
	ctx := metadata.NewIncomingContext(context.Background(), md)

	err := s.checkAuth(ctx)
	assert.NoError(t, err)
}

func TestCheckAuth_InvalidToken(t *testing.T) {
	authn := auth.New([]string{"test-token-123"}, 0)
	s := &Server{auth: authn}

	md := metadata.New(map[string]string{"authorization": "Bearer wrong-token"})
	ctx := metadata.NewIncomingContext(context.Background(), md)

	err := s.checkAuth(ctx)
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.Unauthenticated, st.Code())
}

func TestCheckAuth_MissingMetadata(t *testing.T) {
	authn := auth.New([]string{"test-token-123"}, 0)
	s := &Server{auth: authn}

	err := s.checkAuth(context.Background())
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.Unauthenticated, st.Code())
}

func TestCheckAuth_MissingAuthorizationKey(t *testing.T) {
	authn := auth.New([]string{"test-token-123"}, 0)
	s := &Server{auth: authn}

	md := metadata.New(map[string]string{"other-key": "value"})
	ctx := metadata.NewIncomingContext(context.Background(), md)

	err := s.checkAuth(ctx)
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.Unauthenticated, st.Code())
}

func TestMonitorToProto(t *testing.T) {
	m := &store.Monitor{
		ID:          1,
		Name:        "test",
		ContractIDs: []string{"C1", "C2"},
		Enabled:     true,
		ChannelIDs:  []int64{10, 20},
	}
	p := monitorToProto(m)
	assert.Equal(t, int64(1), p.ID)
	assert.Equal(t, "test", p.Name)
	assert.Equal(t, []string{"C1", "C2"}, p.ContractIDs)
	assert.True(t, p.Enabled)
	assert.Equal(t, []int64{10, 20}, p.ChannelIDs)
}

func TestAlertToProto(t *testing.T) {
	a := &store.Alert{
		ID:        42,
		MonitorID: 1,
		RuleID:    2,
		EventID:   "evt-123",
	}
	p := alertToProto(a)
	assert.Equal(t, int64(42), p.ID)
	assert.Equal(t, int64(1), p.MonitorID)
	assert.Equal(t, "evt-123", p.EventID)
}
