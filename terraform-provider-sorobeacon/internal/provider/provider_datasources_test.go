package provider

import (
	"context"
	"testing"
)

func TestSoroBeaconProviderDataSources(t *testing.T) {
	factories := (&SoroBeaconProvider{}).DataSources(context.Background())
	if len(factories) != 1 {
		t.Fatalf("len(DataSources) = %d, want 1", len(factories))
	}
	for i, f := range factories {
		if f == nil {
			t.Fatalf("factory %d is nil", i)
		}
		if f() == nil {
			t.Fatalf("factory %d returned a nil data source", i)
		}
	}
	if _, ok := factories[0]().(*MonitorDataSource); !ok {
		t.Fatalf("factory 0 did not return *MonitorDataSource")
	}
}
