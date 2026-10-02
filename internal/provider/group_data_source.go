// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"

	"github.com/glitchedmob/terraform-provider-outline/internal/client"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/path"
)

var _ datasource.DataSourceWithConfigure = &groupDataSource{}

type groupDataSource struct{ api *apiClient }

func NewGroupDataSource() datasource.DataSource { return &groupDataSource{} }

func (d *groupDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_group"
}

func (d *groupDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Looks up an Outline group by explicit ID or exact, case-sensitive name. Name lookup reads every page and rejects ambiguous names. Requires an admin-owned API key. Tested with Outline 1.10.1.",
		Attributes: map[string]schema.Attribute{
			"id":               schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "Canonical lowercase, nonzero group UUID. Configure exactly one of `id` or `name`.", Validators: append(groupIDValidators(), stringvalidator.ExactlyOneOf(path.MatchRoot("name")))},
			"name":             schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "Exact, case-sensitive group name, 1 to 255 UTF-16 code units, with at least one non-whitespace character. Configure exactly one of `id` or `name`. A non-unique name is an error; use an ID instead.", Validators: groupNameValidators()},
			"description":      schema.StringAttribute{Computed: true, MarkdownDescription: "Group description. Null server descriptions are returned as an empty string."},
			"disable_mentions": schema.BoolAttribute{Computed: true, MarkdownDescription: "Whether mentions of this group are disabled."},
		},
	}
}

func (d *groupDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
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

func (d *groupDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var config groupModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}
	var group *client.Group
	var err error
	if !config.ID.IsNull() && !config.ID.IsUnknown() {
		id, parseErr := parseGroupID(config.ID.ValueString())
		err = parseErr
		if err == nil {
			group, err = d.api.readGroup(ctx, id)
		}
	} else {
		group, err = d.api.findGroup(ctx, config.Name.ValueString())
	}
	if err != nil {
		resp.Diagnostics.AddError("Unable to look up group", err.Error())
		return
	}
	config.setGroup(group)
	resp.Diagnostics.Append(resp.State.Set(ctx, &config)...)
}
