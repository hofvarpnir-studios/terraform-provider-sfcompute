// Command terraform-provider-sfcompute is a Terraform / OpenTofu provider for
// SF Compute (https://sfcompute.com).
package main

import (
	"context"
	"flag"
	"log"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"

	"github.com/hofvarpnir-studios/terraform-provider-sfcompute/internal/provider"
)

// version is set by goreleaser.
var version = "dev"

func main() {
	var debug bool
	flag.BoolVar(&debug, "debug", false, "run the provider with support for debuggers like delve")
	flag.Parse()

	err := providerserver.Serve(context.Background(), provider.New(version), providerserver.ServeOpts{
		Address: "registry.terraform.io/hofvarpnir-studios/sfcompute",
		Debug:   debug,
	})
	if err != nil {
		log.Fatal(err)
	}
}
