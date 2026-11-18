package provider

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
)

func TestMonitorDataSourceMetadata(t *testing.T) {
	tests := []struct {
		name           string
		providerPrefix string
		want           string
	}{
		{name: "sets type name from provider prefix", providerPrefix: "sorobeacon", want: "sorobeacon_monitor"},
		{name: "empty provider prefix", providerPrefix: "", want: "_monitor"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := &datasource.MetadataResponse{}
			(&MonitorDataSource{}).Metadata(context.Background(), datasource.MetadataRequest{ProviderTypeName: tt.providerPrefix}, resp)
			if resp.TypeName != tt.want {
				t.Fatalf("TypeName = %q, want %q", resp.TypeName, tt.want)
			}
		})
	}
}
