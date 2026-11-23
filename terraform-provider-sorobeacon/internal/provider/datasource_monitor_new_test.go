package provider

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
)

func TestNewMonitorDataSource(t *testing.T) {
	ds := NewMonitorDataSource()
	if ds == nil {
		t.Fatal("NewMonitorDataSource() returned nil")
	}
	m, ok := ds.(*MonitorDataSource)
	if !ok {
		t.Fatalf("NewMonitorDataSource() returned %T, want *MonitorDataSource", ds)
	}
	if m.client != nil {
		t.Fatal("new data source must start without a client")
	}
	if _, ok := ds.(datasource.DataSourceWithConfigure); !ok {
		t.Fatal("data source must implement DataSourceWithConfigure so the provider can inject its client")
	}
	if NewMonitorDataSource() == ds {
		t.Fatal("each call must return a fresh instance")
	}

	resp := &datasource.SchemaResponse{}
	ds.Schema(context.Background(), datasource.SchemaRequest{}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("schema diagnostics: %v", resp.Diagnostics)
	}
	id, ok := resp.Schema.Attributes["id"].(schema.Int64Attribute)
	if !ok || !id.Required {
		t.Fatalf("id attribute must be a required Int64, got %#v", resp.Schema.Attributes["id"])
	}
	for _, name := range []string{"name", "contract_ids", "enabled", "channel_ids"} {
		attr, ok := resp.Schema.Attributes[name]
		if !ok {
			t.Fatalf("schema missing attribute %q", name)
		}
		if !attr.IsComputed() {
			t.Fatalf("attribute %q must be computed", name)
		}
	}
}

func TestNewMonitorDataSourceConfigure(t *testing.T) {
	client := &apiClient{endpoint: "http://example.invalid", token: "tok"}
	tests := []struct {
		name         string
		providerData any
		wantClient   *apiClient
	}{
		{name: "provider client is injected", providerData: client, wantClient: client},
		{name: "nil provider data leaves client unset", providerData: nil, wantClient: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ds := NewMonitorDataSource().(datasource.DataSourceWithConfigure)
			resp := &datasource.ConfigureResponse{}
			ds.Configure(context.Background(), datasource.ConfigureRequest{ProviderData: tt.providerData}, resp)
			if resp.Diagnostics.HasError() {
				t.Fatalf("configure diagnostics: %v", resp.Diagnostics)
			}
			if got := ds.(*MonitorDataSource).client; got != tt.wantClient {
				t.Fatalf("client = %p, want %p", got, tt.wantClient)
			}
		})
	}
}
