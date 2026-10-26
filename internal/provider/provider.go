// Package provider is the sfcompute Terraform/OpenTofu provider. Its
// resources and data sources are listed in registry_gen.go, which
// tools/sfcgen generates from spec/openapi.json and generator_config.yml;
// they all run on the generic engine in internal/runtime.
package provider

import (
	"context"
	"os"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/hofvarpnir-studios/terraform-provider-sfcompute/internal/runtime"
)

// DefaultBaseURL is the SF Compute API endpoint.
const DefaultBaseURL = "https://api.sfcompute.com"

// Environment variables read when the corresponding argument is unset.
const (
	EnvToken     = "SFC_TOKEN"
	EnvBaseURL   = "SFC_BASE_URL"
	EnvWorkspace = "SFC_WORKSPACE"
)

// Provider is the sfcompute provider.
type Provider struct {
	version string
}

var _ provider.Provider = (*Provider)(nil)

// New returns a provider constructor.
func New(version string) func() provider.Provider {
	return func() provider.Provider { return &Provider{version: version} }
}

type providerModel struct {
	Token         types.String `tfsdk:"token"`
	BaseURL       types.String `tfsdk:"base_url"`
	Workspace     types.String `tfsdk:"workspace"`
	AllowSpending types.Bool   `tfsdk:"allow_spending"`
}

// Metadata implements provider.Provider.
func (p *Provider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "sfcompute"
	resp.Version = p.version
}

// Schema implements provider.Provider.
func (p *Provider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manage [SF Compute](https://sfcompute.com) resources. " +
			"The resources and data sources are generated from SF Compute's OpenAPI document.",
		Attributes: map[string]schema.Attribute{
			"token": schema.StringAttribute{
				Optional:  true,
				Sensitive: true,
				MarkdownDescription: "SF Compute API token (`sk_live_...`), sent as `Authorization: Bearer <token>`. " +
					"Create one with `sf tokens create` or in the dashboard, then grant it a role with `sf grants create`. " +
					"Defaults to the `" + EnvToken + "` environment variable.",
			},
			"base_url": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "API base URL. Defaults to the `" + EnvBaseURL + "` environment variable, " +
					"or `" + DefaultBaseURL + "`.",
			},
			"workspace": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Default workspace, as an ID (`wksp_...`) or resource path (`sfc:workspace:<account>:<workspace>`). " +
					"Used by resources and data sources that take a `workspace` and leave it unset. " +
					"Defaults to the `" + EnvWorkspace + "` environment variable. Changing it does not move existing resources.",
			},
			"allow_spending": schema.BoolAttribute{
				Optional: true,
				MarkdownDescription: "Allow creating resources that spend money or consume purchased compute " +
					"(`sfcompute_order`, `sfcompute_instance`). Defaults to `false`, in which case planning such a " +
					"resource fails with an error. There is deliberately no environment variable for this setting.",
			},
		},
	}
}

// Configure implements provider.Provider.
func (p *Provider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var cfg providerModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}
	for attr, v := range map[string]types.String{"token": cfg.Token, "base_url": cfg.BaseURL, "workspace": cfg.Workspace} {
		if v.IsUnknown() {
			resp.Diagnostics.AddAttributeError(path.Root(attr), "Unknown provider argument",
				"The provider cannot be configured with a value that is only known after apply. Use a literal, a variable or an environment variable.")
		}
	}
	if cfg.AllowSpending.IsUnknown() {
		resp.Diagnostics.AddAttributeError(path.Root("allow_spending"), "Unknown provider argument",
			"allow_spending must be known when planning.")
	}
	if resp.Diagnostics.HasError() {
		return
	}

	token := stringOr(cfg.Token, os.Getenv(EnvToken))
	if token == "" {
		resp.Diagnostics.AddAttributeError(path.Root("token"), "Missing SF Compute API token",
			"Set `token` in the provider block or the "+EnvToken+" environment variable. Create a token with `sf tokens create`.")
		return
	}
	baseURL := stringOr(cfg.BaseURL, os.Getenv(EnvBaseURL))
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	client, err := runtime.NewClient(baseURL, token, "terraform-provider-sfcompute/"+p.version)
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("base_url"), "Invalid base_url", err.Error())
		return
	}
	client.DefaultWorkspace = stringOr(cfg.Workspace, os.Getenv(EnvWorkspace))
	client.AllowSpending = cfg.AllowSpending.ValueBool()

	resp.ResourceData = client
	resp.DataSourceData = client
}

func stringOr(v types.String, fallback string) string {
	if !v.IsNull() && !v.IsUnknown() && v.ValueString() != "" {
		return v.ValueString()
	}
	return fallback
}

// Resources implements provider.Provider.
func (p *Provider) Resources(context.Context) []func() resource.Resource {
	return generatedResources()
}

// DataSources implements provider.Provider.
func (p *Provider) DataSources(context.Context) []func() datasource.DataSource {
	return generatedDataSources()
}
