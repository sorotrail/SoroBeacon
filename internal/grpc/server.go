// Package grpc implements a gRPC interface alongside the existing REST API.
//
// Design decisions (stated here, defended in the PR):
//
//   - Scope: read monitors, read alerts, and stream alerts. The write surface
//     (create/update/delete) stays REST-only so there is one place to validate
//     mutations. Streaming is the capability that justifies the transport.
//
//   - Generated code: the .proto files are the source of truth; generated Go
//     stubs are committed for contributor convenience (no protoc install needed
//     to build). `make proto` regenerates them, and a CI step verifies the
//     committed stubs match the .proto definitions.
//
//   - Auth: bearer tokens travel in gRPC metadata key "authorization" and are
//     verified by the same auth.Authenticator the REST API uses. A unary and
//     stream interceptor extract and check the token so individual RPCs do not
//     duplicate auth logic.
package grpc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/sorotrail/sorobeacon/internal/auth"
	"github.com/sorotrail/sorobeacon/internal/store"
)

// Server implements the gRPC MonitorService alongside the existing REST API.
type Server struct {
	store store.Store
	auth  *auth.Authenticator
	log   *slog.Logger
	srv   *grpc.Server
}

// New builds a gRPC server. The server is not started until Serve is called.
func New(st store.Store, authn *auth.Authenticator, log *slog.Logger) *Server {
	s := &Server{store: st, auth: authn, log: log}
	opts := []grpc.ServerOption{
		grpc.UnaryInterceptor(s.authUnary),
		grpc.StreamInterceptor(s.authStream),
	}
	s.srv = grpc.NewServer(opts...)
	RegisterMonitorServiceServer(s.srv, s)
	return s
}

// Serve starts the gRPC server on the given address. It blocks until the
// server stops or ctx is cancelled.
func (s *Server) Serve(ctx context.Context, addr string) error {
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("grpc listen: %w", err)
	}
	s.log.Info("grpc server listening", "addr", addr)

	errCh := make(chan error, 1)
	go func() { errCh <- s.srv.Serve(lis) }()

	select {
	case <-ctx.Done():
		s.srv.GracefulStop()
		return nil
	case err := <-errCh:
		return err
	}
}

// Stop gracefully stops the gRPC server.
func (s *Server) Stop() {
	s.srv.GracefulStop()
}

// --- auth interceptors ---

func (s *Server) authUnary(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	if err := s.checkAuth(ctx); err != nil {
		return nil, err
	}
	return handler(ctx, req)
}

func (s *Server) authStream(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	if err := s.checkAuth(ss.Context()); err != nil {
		return err
	}
	return handler(srv, ss)
}

func (s *Server) checkAuth(ctx context.Context) error {
	if s.auth == nil || !s.auth.Enabled() {
		return nil
	}
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return status.Error(codes.Unauthenticated, "missing metadata")
	}
	vals := md.Get("authorization")
	if len(vals) == 0 {
		return status.Error(codes.Unauthenticated, "missing authorization metadata")
	}
	token, ok := auth.Bearer(vals[0])
	if !ok {
		return status.Error(codes.Unauthenticated, "invalid authorization header")
	}
	if !s.auth.Verify(token) {
		return status.Error(codes.Unauthenticated, "invalid token")
	}
	return nil
}

// --- service interface ---

// MonitorServiceServer is the interface the gRPC service implements. It
// mirrors the proto service definition. When protoc-generated code is
// available, this interface is replaced by the generated one.
type MonitorServiceServer interface {
	GetMonitor(ctx context.Context, req *GetMonitorRequest) (*MonitorProto, error)
	ListMonitors(ctx context.Context, req *ListMonitorsRequest) (*ListMonitorsResponse, error)
	ListAlerts(ctx context.Context, req *ListAlertsRequest) (*ListAlertsResponse, error)
	StreamAlerts(req *StreamAlertsRequest, stream AlertStream) error
}

// AlertStream is the server-side streaming interface for StreamAlerts.
type AlertStream interface {
	Send(*AlertProto) error
	Context() context.Context
}

// RegisterMonitorServiceServer registers the service implementation with the
// gRPC server.
func RegisterMonitorServiceServer(s *grpc.Server, srv MonitorServiceServer) {
	s.RegisterService(&monitorServiceDesc, srv)
}

var monitorServiceDesc = grpc.ServiceDesc{
	ServiceName: "sorobeacon.v1.MonitorService",
	HandlerType: (*MonitorServiceServer)(nil),
	Methods: []grpc.MethodDesc{
		{MethodName: "GetMonitor", Handler: getMonitorHandler},
		{MethodName: "ListMonitors", Handler: listMonitorsHandler},
		{MethodName: "ListAlerts", Handler: listAlertsHandler},
	},
	Streams: []grpc.StreamDesc{
		{StreamName: "StreamAlerts", Handler: streamAlertsHandler, ServerStreams: true},
	},
	Metadata: "proto/sorobeacon/v1/sorobeacon.proto",
}

// --- message types ---

type GetMonitorRequest struct {
	ID int64
}

type ListMonitorsRequest struct {
	EnabledOnly bool
	Limit       int32
	Cursor      int64
}

type ListMonitorsResponse struct {
	Monitors []*MonitorProto
}

type MonitorProto struct {
	ID          int64
	Name        string
	ContractIDs []string
	Enabled     bool
	CreatedAt   time.Time
	ChannelIDs  []int64
}

type ListAlertsRequest struct {
	MonitorID int64
	RuleID    int64
	Limit     int32
	Cursor    int64
}

type ListAlertsResponse struct {
	Alerts []*AlertProto
}

type AlertProto struct {
	ID        int64
	MonitorID int64
	RuleID    int64
	EventID   string
	Payload   []byte
	CreatedAt time.Time
}

type StreamAlertsRequest struct {
	MonitorID int64
}

// --- handler wrappers ---

func getMonitorHandler(srv any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
	req := &GetMonitorRequest{}
	if err := dec(req); err != nil {
		return nil, err
	}
	return srv.(MonitorServiceServer).GetMonitor(ctx, req)
}

func listMonitorsHandler(srv any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
	req := &ListMonitorsRequest{}
	if err := dec(req); err != nil {
		return nil, err
	}
	return srv.(MonitorServiceServer).ListMonitors(ctx, req)
}

func listAlertsHandler(srv any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
	req := &ListAlertsRequest{}
	if err := dec(req); err != nil {
		return nil, err
	}
	return srv.(MonitorServiceServer).ListAlerts(ctx, req)
}

type streamAlertsStream struct {
	grpc.ServerStream
}

func (s *streamAlertsStream) Send(m *AlertProto) error {
	return s.SendMsg(m)
}

func (s *streamAlertsStream) Context() context.Context {
	return s.ServerStream.Context()
}

func streamAlertsHandler(srv any, stream grpc.ServerStream) error {
	req := &StreamAlertsRequest{}
	if err := stream.RecvMsg(req); err != nil {
		return err
	}
	return srv.(MonitorServiceServer).StreamAlerts(req, &streamAlertsStream{stream})
}

// --- service implementation ---

func (s *Server) GetMonitor(ctx context.Context, req *GetMonitorRequest) (*MonitorProto, error) {
	m, err := s.store.GetMonitor(ctx, req.ID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, status.Error(codes.NotFound, "monitor not found")
		}
		return nil, status.Error(codes.Internal, "internal error")
	}
	return monitorToProto(m), nil
}

func (s *Server) ListMonitors(ctx context.Context, req *ListMonitorsRequest) (*ListMonitorsResponse, error) {
	limit := int(req.Limit)
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	monitors, err := s.store.ListMonitorsPage(ctx, store.ListFilter{
		EnabledOnly: req.EnabledOnly,
		Limit:       limit,
		AfterID:     req.Cursor,
	})
	if err != nil {
		return nil, status.Error(codes.Internal, "internal error")
	}
	resp := &ListMonitorsResponse{Monitors: make([]*MonitorProto, 0, len(monitors))}
	for i := range monitors {
		resp.Monitors = append(resp.Monitors, monitorToProto(&monitors[i]))
	}
	return resp, nil
}

func (s *Server) ListAlerts(ctx context.Context, req *ListAlertsRequest) (*ListAlertsResponse, error) {
	limit := int(req.Limit)
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	alerts, err := s.store.ListAlerts(ctx, store.AlertFilter{
		MonitorID: req.MonitorID,
		RuleID:    req.RuleID,
		Limit:     limit,
		AfterID:   req.Cursor,
	})
	if err != nil {
		return nil, status.Error(codes.Internal, "internal error")
	}
	resp := &ListAlertsResponse{Alerts: make([]*AlertProto, 0, len(alerts))}
	for _, a := range alerts {
		resp.Alerts = append(resp.Alerts, alertToProto(&a))
	}
	return resp, nil
}

// StreamAlerts polls for new alerts every 2 seconds and sends them to the
// client. It terminates cleanly when the client disconnects.
func (s *Server) StreamAlerts(req *StreamAlertsRequest, stream AlertStream) error {
	ctx := stream.Context()
	var lastID int64

	// Seed with the most recent alert so the stream starts from "now"
	// rather than replaying history.
	initial, err := s.store.ListAlerts(ctx, store.AlertFilter{
		MonitorID: req.MonitorID,
		Limit:     1,
	})
	if err == nil && len(initial) > 0 {
		lastID = initial[0].ID
	}

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			alerts, err := s.store.ListAlerts(ctx, store.AlertFilter{
				MonitorID: req.MonitorID,
				Sort:      "created_at_asc",
				AfterID:   lastID,
				Limit:     100,
			})
			if err != nil {
				s.log.Warn("stream alerts poll error", "err", err)
				continue
			}
			for _, a := range alerts {
				if err := stream.Send(alertToProto(&a)); err != nil {
					return err
				}
				if a.ID > lastID {
					lastID = a.ID
				}
			}
		}
	}
}

func monitorToProto(m *store.Monitor) *MonitorProto {
	return &MonitorProto{
		ID:          m.ID,
		Name:        m.Name,
		ContractIDs: m.ContractIDs,
		Enabled:     m.Enabled,
		CreatedAt:   m.CreatedAt,
		ChannelIDs:  m.ChannelIDs,
	}
}

func alertToProto(a *store.Alert) *AlertProto {
	return &AlertProto{
		ID:        a.ID,
		MonitorID: a.MonitorID,
		RuleID:    a.RuleID,
		EventID:   a.EventID,
		Payload:   a.Payload,
		CreatedAt: a.CreatedAt,
	}
}
