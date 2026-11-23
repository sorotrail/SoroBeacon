package grpc

import (
	"bytes"
	"context"
	"encoding/gob"
	"io"
	"log/slog"
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/encoding"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/sorotrail/sorobeacon/internal/auth"
	"github.com/sorotrail/sorobeacon/internal/store"
	"github.com/sorotrail/sorobeacon/internal/workspace"
)

// gobCodec lets the hand-written service types exercise grpc-go's real
// interceptors before protobuf generation is introduced for this package.
type gobCodec struct{}

func (gobCodec) Name() string { return "sorobeacon-test-gob" }
func (gobCodec) Marshal(v any) ([]byte, error) {
	var b bytes.Buffer
	err := gob.NewEncoder(&b).Encode(v)
	return b.Bytes(), err
}
func (gobCodec) Unmarshal(data []byte, v any) error {
	return gob.NewDecoder(bytes.NewReader(data)).Decode(v)
}

func init() { encoding.RegisterCodec(gobCodec{}) }

func authTestConn(t *testing.T) *grpc.ClientConn {
	t.Helper()
	listener := bufconn.Listen(1 << 20)
	authn := auth.New([]string{"grpc-test-credential"}, 0)
	s := New(nil, authn, slog.New(slog.NewTextHandler(io.Discard, nil)))
	go func() { _ = s.srv.Serve(listener) }()
	t.Cleanup(func() {
		_ = listener.Close()
		s.srv.Stop()
	})
	conn, err := grpc.NewClient("passthrough:///bufnet", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
		return listener.Dial()
	}), grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithDefaultCallOptions(grpc.ForceCodec(gobCodec{})))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func TestAuthInterceptorsRejectUnauthenticatedRegisteredMethods(t *testing.T) {
	conn := authTestConn(t)
	for _, method := range monitorServiceDesc.Methods {
		t.Run(method.MethodName, func(t *testing.T) {
			var response ListMonitorsResponse
			var request any
			switch method.MethodName {
			case "GetMonitor":
				request = &GetMonitorRequest{}
			case "ListMonitors":
				request = &ListMonitorsRequest{}
			case "ListAlerts":
				request = &ListAlertsRequest{}
			}
			err := conn.Invoke(context.Background(), "/sorobeacon.v1.MonitorService/"+method.MethodName, request, &response)
			assert.Equal(t, codes.Unauthenticated, status.Code(err))
		})
	}
	for _, method := range monitorServiceDesc.Streams {
		t.Run(method.StreamName, func(t *testing.T) {
			stream, err := conn.NewStream(context.Background(), &monitorServiceDesc.Streams[0], "/sorobeacon.v1.MonitorService/"+method.StreamName)
			require.NoError(t, err)
			require.NoError(t, stream.SendMsg(&StreamAlertsRequest{}))
			require.NoError(t, stream.CloseSend())
			assert.Equal(t, codes.Unauthenticated, status.Code(stream.RecvMsg(&AlertProto{})))
		})
	}
}

func TestAuthInterceptorsRejectMalformedAndInvalidCredentials(t *testing.T) {
	conn := authTestConn(t)
	for name, credential := range map[string]string{"malformed": "Basic grpc-test-credential", "invalid": "Bearer wrong"} {
		t.Run(name, func(t *testing.T) {
			ctx := metadata.NewOutgoingContext(context.Background(), metadata.Pairs("authorization", credential))
			var response ListMonitorsResponse
			err := conn.Invoke(ctx, "/sorobeacon.v1.MonitorService/ListMonitors", &ListMonitorsRequest{}, &response)
			assert.Equal(t, codes.Unauthenticated, status.Code(err))
			assert.NotContains(t, err.Error(), "grpc-test-credential")
		})
	}
}

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

func TestAuthContextScopesBoundCredentialToWorkspace(t *testing.T) {
	authn := auth.NewBound([]auth.Binding{{Workspace: "team-a", Token: "team-a-token"}}, 0)
	s := &Server{auth: authn}
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer team-a-token"))

	got, err := s.authContext(ctx)
	require.NoError(t, err)
	gotWorkspace, ok := workspace.From(got)
	require.True(t, ok)
	assert.Equal(t, workspace.ID("team-a"), gotWorkspace)
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
