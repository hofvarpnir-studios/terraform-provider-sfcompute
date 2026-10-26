package provider

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
)

// TestProviderSchemaIsValid asks the framework for the full provider schema,
// which validates every generated resource and data source schema
// (attribute names, nested types, defaults, validators).
func TestProviderSchemaIsValid(t *testing.T) {
	server, err := providerserver.NewProtocol6WithError(New("test")())()
	if err != nil {
		t.Fatal(err)
	}
	resp, err := server.GetProviderSchema(context.Background(), &tfprotov6.GetProviderSchemaRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range resp.Diagnostics {
		t.Errorf("%s: %s: %s", d.Severity, d.Summary, d.Detail)
	}
	if got, want := len(resp.ResourceSchemas), len(ResourceDefinitions); got != want {
		t.Errorf("got %d resource schemas, want %d", got, want)
	}
	if got, want := len(resp.DataSourceSchemas), len(DataSourceDefinitions); got != want {
		t.Errorf("got %d data source schemas, want %d", got, want)
	}
	for _, name := range []string{"sfcompute_workspace", "sfcompute_pool", "sfcompute_firewall", "sfcompute_instance_template", "sfcompute_order", "sfcompute_instance"} {
		if resp.ResourceSchemas[name] == nil {
			t.Errorf("missing resource %s", name)
		}
	}
	for _, name := range []string{"sfcompute_account", "sfcompute_limits", "sfcompute_skus", "sfcompute_pools", "sfcompute_orders", "sfcompute_instances"} {
		if resp.DataSourceSchemas[name] == nil {
			t.Errorf("missing data source %s", name)
		}
	}
}
