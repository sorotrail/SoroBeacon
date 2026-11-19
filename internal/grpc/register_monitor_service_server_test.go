package grpc

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

type monitorServiceStub struct{}

func (monitorServiceStub) GetMonitor(context.Context, *GetMonitorRequest) (*MonitorProto, error) {
	return nil, nil
}

func (monitorServiceStub) ListMonitors(context.Context, *ListMonitorsRequest) (*ListMonitorsResponse, error) {
	return nil, nil
}

func (monitorServiceStub) ListAlerts(context.Context, *ListAlertsRequest) (*ListAlertsResponse, error) {
	return nil, nil
}

func (monitorServiceStub) StreamAlerts(*StreamAlertsRequest, AlertStream) error { return nil }

func TestRegisterMonitorServiceServer(t *testing.T) {
	t.Run("registers the monitor service", func(t *testing.T) {
		srv := grpc.NewServer()
		t.Cleanup(srv.Stop)

		RegisterMonitorServiceServer(srv, monitorServiceStub{})

		info, ok := srv.GetServiceInfo()["sorobeacon.v1.MonitorService"]
		require.True(t, ok, "MonitorService should be registered")
		methods := make([]string, 0, len(info.Methods))
		for _, method := range info.Methods {
			methods = append(methods, method.Name)
		}
		assert.ElementsMatch(t, []string{"GetMonitor", "ListMonitors", "ListAlerts", "StreamAlerts"}, methods)
		assert.Equal(t, "proto/sorobeacon/v1/sorobeacon.proto", info.Metadata)
	})

	t.Run("panics on a nil server", func(t *testing.T) {
		assert.Panics(t, func() {
			RegisterMonitorServiceServer(nil, monitorServiceStub{})
		})
	})
}
