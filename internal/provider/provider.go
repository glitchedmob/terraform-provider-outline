// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"os"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

const defaultBaseURL = "https://app.getoutline.com/api"
const defaultTimeoutSeconds int64 = 30

var _ provider.Provider = &OutlineProvider{}

// OutlineProvider defines the provider implementation.
type OutlineProvider struct {
	version string
}

// OutlineProviderModel describes the provider configuration.
type OutlineProviderModel struct {
	BaseURL        types.String `tfsdk:"base_url"`
	APIKey         types.String `tfsdk:"api_key"`
	TimeoutSeconds types.Int64  `tfsdk:"timeout_seconds"`
}

func (p *OutlineProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "outline"
	resp.Version = p.version
}

func (p *OutlineProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "The Outline provider configures access to an Outline API.",
		Attributes: map[string]schema.Attribute{
			"base_url": schema.StringAttribute{
				MarkdownDescription: "Outline API base URL, including the `/api` path. Defaults to `https://app.getoutline.com/api`. May also be set with the `OUTLINE_BASE_URL` environment variable. Must use HTTP or HTTPS and must not include credentials, a query string, or a fragment.",
				Optional:            true,
			},
			"api_key": schema.StringAttribute{
				MarkdownDescription: "Outline API key sent as a bearer token. Required through this attribute or the `OUTLINE_API_KEY` environment variable. Leading and trailing whitespace is trimmed; the token must not contain whitespace or control characters.",
				Optional:            true,
				Sensitive:           true,
			},
			"timeout_seconds": schema.Int64Attribute{
				MarkdownDescription: "HTTP request timeout in seconds. Defaults to `30`. Must be a positive integer no greater than `9223372036`.",
				Optional:            true,
				Validators: []validator.Int64{
					int64validator.Between(1, maxTimeoutSeconds),
				},
			},
		},
	}
}

func (p *OutlineProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var config OutlineProviderModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if config.BaseURL.IsUnknown() {
		resp.Diagnostics.AddAttributeError(path.Root("base_url"), "Unknown Outline Base URL",
			"The base_url must be known while the provider is being configured.")
	}
	if config.APIKey.IsUnknown() {
		resp.Diagnostics.AddAttributeError(path.Root("api_key"), "Unknown Outline API Key",
			"The api_key must be known while the provider is being configured.")
	}
	if config.TimeoutSeconds.IsUnknown() {
		resp.Diagnostics.AddAttributeError(path.Root("timeout_seconds"), "Unknown Outline Timeout",
			"The timeout_seconds must be known while the provider is being configured.")
	}
	if resp.Diagnostics.HasError() {
		return
	}

	baseURL, apiKey, timeoutSeconds := resolveProviderConfig(config, os.Getenv)
	// Construction validates local configuration only. No API requests happen here.
	client, err := newAPIClient(baseURL, apiKey, timeoutSeconds, p.version)
	if err != nil {
		resp.Diagnostics.AddError("Unable to Configure Outline API Client", err.Error())
		return
	}
	resp.DataSourceData = client
	resp.ResourceData = client
}

func (p *OutlineProvider) Resources(context.Context) []func() resource.Resource {
	return []func() resource.Resource{NewGroupResource, NewUserResource, NewGroupMemberResource, NewCollectionResource, NewCollectionGroupResource}
}

func (p *OutlineProvider) DataSources(context.Context) []func() datasource.DataSource {
	return []func() datasource.DataSource{NewGroupDataSource, NewUserDataSource, NewCollectionDataSource}
}

// New returns a provider factory for protocol server registration and tests.
func New(version string) func() provider.Provider {
	return func() provider.Provider {
		return &OutlineProvider{version: version}
	}
}

func resolveProviderConfig(config OutlineProviderModel, getenv func(string) string) (string, string, int64) {
	baseURL := strings.TrimSpace(getenv("OUTLINE_BASE_URL"))
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	if !config.BaseURL.IsNull() {
		baseURL = strings.TrimSpace(config.BaseURL.ValueString())
	}

	apiKey := getenv("OUTLINE_API_KEY")
	if !config.APIKey.IsNull() {
		apiKey = config.APIKey.ValueString()
	}

	timeoutSeconds := defaultTimeoutSeconds
	if !config.TimeoutSeconds.IsNull() {
		timeoutSeconds = config.TimeoutSeconds.ValueInt64()
	}
	return baseURL, strings.TrimSpace(apiKey), timeoutSeconds
}
