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

var _ datasource.DataSourceWithConfigure = &userDataSource{}

type userDataSource struct{ api *apiClient }

func NewUserDataSource() datasource.DataSource { return &userDataSource{} }
func (d *userDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_user"
}
func (d *userDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Looks up an Outline user by UUID or exact normalized email. Email lookup checks all pages, including pending and suspended accounts, and rejects ambiguity. Requires an active admin-owned API key. Tested with Outline 1.10.1.",
		Attributes: map[string]schema.Attribute{
			"id":        schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "Canonical lowercase, nonzero user UUID. Configure exactly one of id or email.", Validators: append(groupIDValidators(), stringvalidator.ExactlyOneOf(path.MatchRoot("email")))},
			"email":     schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "Exact email after lowercase normalization. Configure exactly one of id or email. No substring or display-name matching.", Validators: userEmailValidators()},
			"name":      schema.StringAttribute{Computed: true, MarkdownDescription: "Current display name."},
			"role":      schema.StringAttribute{Computed: true, MarkdownDescription: "Current workspace role."},
			"suspended": schema.BoolAttribute{Computed: true, MarkdownDescription: "Whether the account is suspended."},
		},
	}
}
func (d *userDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
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
func (d *userDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var config userLookupModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}
	_, err := d.api.requireUserAdmin(ctx)
	var user *client.User
	if err == nil {
		if !config.ID.IsNull() && !config.ID.IsUnknown() {
			var idErr error
			id, idErr := parseUserID(config.ID.ValueString())
			err = idErr
			if err == nil {
				user, err = d.api.readUser(ctx, id)
			}
		} else {
			user, err = d.api.findUser(ctx, config.Email.ValueString())
		}
	}
	if err != nil {
		resp.Diagnostics.AddError("Unable to look up user", err.Error())
		return
	}
	config.setUser(user)
	resp.Diagnostics.Append(resp.State.Set(ctx, &config)...)
}
