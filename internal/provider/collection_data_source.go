// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"

	"github.com/glitchedmob/terraform-provider-outline/internal/client"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
)

var _ datasource.DataSourceWithConfigure = &collectionDataSource{}

type collectionDataSource struct{ api *apiClient }

func NewCollectionDataSource() datasource.DataSource { return &collectionDataSource{} }

func (d *collectionDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_collection"
}

func (d *collectionDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Looks up one Outline collection by canonical UUID or exact, case-sensitive name. Requires an active admin-owned API key. Name lookup validates every page, includes private and archived collections via admin includeListOnly, excludes trash, and rejects ambiguity. Metadata visibility does not grant access to private documents. Tested with Outline 1.10.1.",
		Attributes: map[string]schema.Attribute{
			"id":          schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "Canonical lowercase, nonzero collection UUID. Configure exactly one of id or name.", Validators: append(groupIDValidators(), stringvalidator.ExactlyOneOf(path.MatchRoot("name")))},
			"name":        schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "Exact, case-sensitive name, 1 to 200 UTF-16 code units. Configure exactly one of id or name. Duplicate names are valid in Outline; use UUID to disambiguate. Name lookup does not apply the resource's trimming or URL restrictions to existing collections.", Validators: []validator.String{outlineStringLength{min: 1, max: 200}}},
			"description": schema.StringAttribute{Computed: true, MarkdownDescription: "Current Markdown landing-page description. Null descriptions are returned as an empty string."},
			"permission":  schema.StringAttribute{Computed: true, MarkdownDescription: "Default workspace access, read, read_write, or the server's admin value. Null means private. Does not describe direct user/group grants."},
			"sharing":     schema.BoolAttribute{Computed: true, MarkdownDescription: "Whether public document sharing is allowed in this collection, subject to workspace policies."},
		},
	}
}

func (d *collectionDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	api, ok := req.ProviderData.(*apiClient)
	if !ok || api == nil {
		resp.Diagnostics.AddError("Unexpected data source client", "Expected an Outline API client.")
		return
	}
	d.api = api
}

func (d *collectionDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var config collectionLookupModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}
	var collection *client.Collection
	var err error
	if !config.ID.IsNull() && !config.ID.IsUnknown() {
		id, parseErr := parseCollectionID(config.ID.ValueString())
		err = parseErr
		if err == nil {
			collection, err = d.api.readCollection(ctx, id)
		}
	} else {
		collection, err = d.api.findCollection(ctx, config.Name.ValueString())
	}
	if err != nil {
		resp.Diagnostics.AddError("Unable to look up collection", err.Error())
		return
	}
	config.setCollection(collection)
	resp.Diagnostics.Append(resp.State.Set(ctx, &config)...)
}
