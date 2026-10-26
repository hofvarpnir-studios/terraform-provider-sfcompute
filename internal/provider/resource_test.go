package provider

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/hofvarpnir-studios/terraform-provider-sfcompute/internal/mockapi"
)

// requireCLI makes sure terraform-plugin-testing has a Terraform or OpenTofu
// binary without downloading one: TF_ACC_TERRAFORM_PATH if set, else
// terraform on PATH, else tofu on PATH; otherwise the test is skipped.
func requireCLI(t *testing.T) {
	t.Helper()
	if os.Getenv("TF_ACC_TERRAFORM_PATH") != "" {
		return
	}
	if p, err := exec.LookPath("terraform"); err == nil {
		t.Setenv("TF_ACC_TERRAFORM_PATH", p)
		return
	}
	if p, err := exec.LookPath("tofu"); err == nil {
		t.Setenv("TF_ACC_TERRAFORM_PATH", p)
		t.Setenv("TF_ACC_PROVIDER_HOST", "registry.opentofu.org")
		return
	}
	t.Skip("no terraform or tofu binary found; set TF_ACC_TERRAFORM_PATH to run Terraform-level tests")
}

func newMock(t *testing.T) *mockapi.Server {
	t.Helper()
	spec, err := mockapi.LoadSpec("../../spec/openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	srv := mockapi.New(spec)
	srv.SeedFixtures()
	srv.Seed("workspace", map[string]any{"object": "workspace", "id": "wksp_default0001", "name": "default",
		"resource_path": "sfc:workspace:acme:default", "created_at": int64(1_690_000_000)})
	t.Cleanup(func() {
		srv.Close()
		for _, v := range srv.Violations() {
			t.Errorf("traffic did not match spec/openapi.json: %s", v)
		}
	})
	return srv
}

func factories() map[string]func() (tfprotov6.ProviderServer, error) {
	return map[string]func() (tfprotov6.ProviderServer, error){
		"sfcompute": providerserver.NewProtocol6WithError(New("test")()),
	}
}

func providerBlock(srv *mockapi.Server, extra string) string {
	return fmt.Sprintf(`
provider "sfcompute" {
  token     = %q
  base_url  = %q
  workspace = "sfc:workspace:acme:default"
  %s
}
`, srv.Token, srv.URL, extra)
}

// TestTerraformLifecycle runs real Terraform (or OpenTofu) plans and applies
// against the mock API: create, no-op re-plan, in-place update, replacement,
// import and destroy, plus data sources.
func TestTerraformLifecycle(t *testing.T) {
	requireCLI(t)
	srv := newMock(t)

	config := func(tag, fwPort string) string {
		return providerBlock(srv, "") + fmt.Sprintf(`
resource "sfcompute_workspace" "research" {
  name = "research"
}

resource "sfcompute_pool" "train" {
  name = "train"
  tags = { team = %q }
}

resource "sfcompute_firewall" "web" {
  name      = "web"
  workspace = sfcompute_workspace.research.resource_path
  rules = [
    { direction = "ingress", protocol = "tcp", port = %q },
  ]
}

resource "sfcompute_instance_template" "worker" {
  workspace = sfcompute_workspace.research.id
  image     = data.sfcompute_image.ubuntu.id
  tags      = { role = "worker" }
}

data "sfcompute_image" "ubuntu" {
  id = "sfc:image:sfcompute:public:ubuntu-24.04-cuda-13.2"
}

data "sfcompute_pools" "all" {
  depends_on = [sfcompute_pool.train]
}

data "sfcompute_account" "me" {}
`, tag, fwPort)
	}

	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(),
		CheckDestroy: func(*terraform.State) error {
			for _, kind := range []string{"pool", "firewall", "instance_template"} {
				if n := srv.CountRequests("DELETE", map[string]string{
					"pool": "/v2/pools/{id}", "firewall": "/preview/v2/firewalls/{id}", "instance_template": "/preview/v2/instance_templates/{id}",
				}[kind]); n == 0 {
					return fmt.Errorf("%s was never deleted", kind)
				}
			}
			return nil
		},
		Steps: []resource.TestStep{
			{
				Config: config("ml", "443"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("sfcompute_pool.train", "workspace", "sfc:workspace:acme:default"),
					resource.TestCheckResourceAttr("sfcompute_pool.train", "resource_path", "sfc:pool:acme:default:train"),
					resource.TestMatchResourceAttr("sfcompute_pool.train", "id", regexp.MustCompile(`^pool_`)),
					resource.TestCheckResourceAttr("sfcompute_firewall.web", "rules.0.source", "0.0.0.0/0"),
					resource.TestCheckResourceAttr("sfcompute_firewall.web", "workspace", "sfc:workspace:acme:research"),
					resource.TestCheckResourceAttr("sfcompute_instance_template.worker", "image", "sfc:image:sfcompute:public:ubuntu-24.04-cuda-13.2"),
					resource.TestCheckResourceAttr("data.sfcompute_image.ubuntu", "name", "ubuntu-24.04-cuda-13.2"),
					resource.TestCheckResourceAttr("data.sfcompute_image.ubuntu", "is_public", "true"),
					resource.TestCheckResourceAttr("sfcompute_instance_template.worker", "enable_public_ipv4", "false"),
					resource.TestCheckResourceAttr("data.sfcompute_pools.all", "data.#", "1"),
					resource.TestCheckResourceAttr("data.sfcompute_pools.all", "workspace", "sfc:workspace:acme:default"),
					resource.TestCheckResourceAttr("data.sfcompute_account.me", "account_id", "acme"),
				),
			},
			{
				// Tags and firewall rules change in place.
				Config: config("infra", "8443"),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction("sfcompute_pool.train", plancheck.ResourceActionUpdate),
					plancheck.ExpectResourceAction("sfcompute_firewall.web", plancheck.ResourceActionUpdate),
					plancheck.ExpectResourceAction("sfcompute_instance_template.worker", plancheck.ResourceActionNoop),
				}},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("sfcompute_pool.train", "tags.team", "infra"),
					resource.TestCheckResourceAttr("sfcompute_firewall.web", "rules.0.port", "8443"),
				),
			},
			{
				ResourceName:      "sfcompute_pool.train",
				ImportState:       true,
				ImportStateVerify: true,
				// The API reports the workspace as {id, name}; import
				// can only recover its ID, not the resource path used
				// in configuration.
				ImportStateVerifyIgnore: []string{"workspace"},
			},
			{
				// Instance templates cannot be updated: a change replaces.
				Config: config("infra", "8443") + `
resource "sfcompute_instance_template" "worker2" {
  workspace = sfcompute_workspace.research.id
  image     = data.sfcompute_image.ubuntu.id
  enable_public_ipv4 = false
}
`,
			},
		},
	})
	if n := srv.CountRequests("POST", "/v2/orders"); n != 0 {
		t.Errorf("unexpected order requests: %d", n)
	}
}

// TestTerraformSpendingGuard checks that a plan containing a spending
// resource fails without allow_spending and that no request is made.
func TestTerraformSpendingGuard(t *testing.T) {
	requireCLI(t)
	srv := newMock(t)
	order := `
resource "sfcompute_pool" "p" {
  name = "buy"
}

resource "sfcompute_order" "o" {
  pool                              = sfcompute_pool.p.id
  side                              = "buy"
  sku                               = "sku_mockh100a"
  allow_standing                    = true
  allow_partial                     = false
  limit_price_dollars_per_node_hour = "12.50"
  allocation_schedule_delta = [
    { start_at = 1700003600, end_at = 1700007200, node_count = 1 },
  ]
}
`
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories(),
		Steps: []resource.TestStep{
			{
				Config:      providerBlock(srv, "") + order,
				ExpectError: regexp.MustCompile(`requires allow_spending = true`),
			},
			{
				Config: providerBlock(srv, "allow_spending = true") + order,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("sfcompute_order.o", "state", "standing"),
					resource.TestMatchResourceAttr("sfcompute_order.o", "pool", regexp.MustCompile(`^pool_`)),
					resource.TestCheckResourceAttr("sfcompute_order.o", "workspace.name", "default"),
				),
			},
		},
	})
	if n := srv.CountRequests("POST", "/v2/orders"); n != 1 {
		t.Errorf("want exactly one order request (from the allowed step), got %d", n)
	}
	if n := srv.CountRequests("POST", "/v2/orders/{id}/cancel"); n != 1 {
		t.Errorf("destroy should cancel the order once, got %d", n)
	}
}
