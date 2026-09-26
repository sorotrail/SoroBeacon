// Terraform provider for SoroBeacon: manage monitors, rules and channels as
// code. See the README for the channel-secret-handling decision.
package main

import (
	"context"
	"log"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/sorotrail/terraform-provider-sorobeacon/internal/provider"
)

func main() {
	if err := providerserver.Serve(context.Background(), provider.New, providerserver.ServeOpts{
		Address: "registry.terraform.io/sorotrail/sorobeacon",
	}); err != nil {
		log.Fatal(err)
	}
}
