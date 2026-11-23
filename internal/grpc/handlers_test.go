package grpc

import (
	"bytes"
	"context"
	"encoding/gob"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/encoding"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/sorotrail/sorobeacon/internal/store"
	"github.com/sorotrail/sorobeacon/internal/workspace"
)

type handlerGobCodec struct{}

func (handlerGobCodec) Name() string { return "sorobeacon-handler-gob" }
func (handlerGobCodec) Marshal(v any) ([]byte, error) {
	var b bytes.Buffer
	err := gob.NewEncoder(&b).Encode(v)
	return b.Bytes(), err
}
func (handlerGobCodec) Unmarshal(b []byte, v any) error {
	return gob.NewDecoder(bytes.NewReader(b)).Decode(v)
}

func init() { encoding.RegisterCodec(handlerGobCodec{}) }

func handlerTestConn(t *testing.T, st store.Store) *grpc.ClientConn {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	s := New(st, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	go func() { _ = s.srv.Serve(lis) }()
	t.Cleanup(func() { _ = lis.Close(); s.Stop() })
	conn, err := grpc.NewClient("passthrough:///bufconn",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.ForceCodec(handlerGobCodec{})))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func handlerTestStore(t *testing.T) store.Store {
	t.Helper()
	url := "sqlite://" + filepath.Join(t.TempDir(), "grpc.db")
	require.NoError(t, store.Migrate(url))
	st, err := store.New(context.Background(), url, store.PoolSettings{}, nil)
	require.NoError(t, err)
	t.Cleanup(st.Close)
	return st
}

func TestGRPCGetMonitor(t *testing.T) {
	st := handlerTestStore(t)
	ctx := workspace.With(context.Background(), workspace.Default)
	m := &store.Monitor{Name: "fixture", ContractIDs: []string{"CA"}, Enabled: true}
	require.NoError(t, st.CreateMonitor(ctx, m))
	conn := handlerTestConn(t, st)

	t.Run("found", func(t *testing.T) {
		var got MonitorProto
		err := conn.Invoke(context.Background(), "/sorobeacon.v1.MonitorService/GetMonitor", &GetMonitorRequest{ID: m.ID}, &got)
		require.NoError(t, err)
		require.Equal(t, m.ID, got.ID)
		require.Equal(t, m.Name, got.Name)
		require.Equal(t, m.ContractIDs, got.ContractIDs)
		require.Equal(t, m.Enabled, got.Enabled)
	})

	t.Run("missing", func(t *testing.T) {
		var got MonitorProto
		err := conn.Invoke(context.Background(), "/sorobeacon.v1.MonitorService/GetMonitor", &GetMonitorRequest{ID: 99999}, &got)
		require.Equal(t, codes.NotFound, status.Code(err))
	})
}

func TestGRPCListMonitorsPaginationAndEmptyResult(t *testing.T) {
	st := handlerTestStore(t)
	ctx := workspace.With(context.Background(), workspace.Default)
	conn := handlerTestConn(t, st)
	var empty ListMonitorsResponse
	require.NoError(t, conn.Invoke(context.Background(), "/sorobeacon.v1.MonitorService/ListMonitors", &ListMonitorsRequest{Limit: 2}, &empty))
	require.Empty(t, empty.Monitors)

	for _, name := range []string{"first", "second", "third"} {
		require.NoError(t, st.CreateMonitor(ctx, &store.Monitor{Name: name, Enabled: true}))
	}
	var page1 ListMonitorsResponse
	require.NoError(t, conn.Invoke(context.Background(), "/sorobeacon.v1.MonitorService/ListMonitors", &ListMonitorsRequest{Limit: 2}, &page1))
	require.Len(t, page1.Monitors, 2)
	var page2 ListMonitorsResponse
	require.NoError(t, conn.Invoke(context.Background(), "/sorobeacon.v1.MonitorService/ListMonitors", &ListMonitorsRequest{Limit: 2, Cursor: page1.Monitors[1].ID}, &page2))
	require.Len(t, page2.Monitors, 1)
	require.Greater(t, page2.Monitors[0].ID, page1.Monitors[1].ID)
}

func TestGRPCListAlertsEmptyResult(t *testing.T) {
	conn := handlerTestConn(t, handlerTestStore(t))
	var got ListAlertsResponse
	err := conn.Invoke(context.Background(), "/sorobeacon.v1.MonitorService/ListAlerts", &ListAlertsRequest{Limit: 10}, &got)
	require.NoError(t, err)
	require.Empty(t, got.Alerts)
}
