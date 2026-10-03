// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"errors"

	"github.com/glitchedmob/terraform-provider-outline/internal/client"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var _ resource.ResourceWithConfigure = &collectionUserResource{}
var _ resource.ResourceWithImportState = &collectionUserResource{}
var _ resource.ResourceWithModifyPlan = &collectionUserResource{}

type collectionUserResource struct{ api *apiClient }

func NewCollectionUserResource() resource.Resource { return &collectionUserResource{} }

func (r *collectionUserResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_collection_user"
}

func (r *collectionUserResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages one explicit collection/user access grant, not effective or inherited access. Requires an active admin-owned API key and refuses the key owner's own direct grant, including reads and import. Existing pairs require import. Suspended, guest, viewer, and pending users are supported as targets without changing their roles or suspension. Archived collections are refused. Tested with Outline 1.10.1.",
		Attributes: map[string]schema.Attribute{
			"id":            schema.StringAttribute{Computed: true, MarkdownDescription: "Stable composite `collection_id/user_id`, separated by exactly one slash. This is the import ID, not the grant row's API UUID."},
			"collection_id": schema.StringAttribute{Required: true, MarkdownDescription: "Canonical lowercase, nonzero collection UUID. Changing it replaces this grant.", Validators: groupIDValidators(), PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()}},
			"user_id":       schema.StringAttribute{Required: true, MarkdownDescription: "Canonical lowercase, nonzero user UUID. The user must already exist and must not own the configured API key. Changing it replaces this grant.", Validators: groupIDValidators(), PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()}},
			"permission":    schema.StringAttribute{Required: true, MarkdownDescription: "Explicit collection grant permission, `read`, `read_write`, or `admin`. Required, with no default. Does not change collection default access, workspace roles, suspension, or group memberships. Role and server policies still affect effective access.", Validators: []validator.String{stringvalidator.OneOf("read", "read_write", "admin")}},
		},
	}
}

func (r *collectionUserResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() {
		return
	}
	var collectionID, userID types.String
	resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("collection_id"), &collectionID)...)
	resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("user_id"), &userID)...)
	if resp.Diagnostics.HasError() || collectionID.IsUnknown() || userID.IsUnknown() || collectionID.IsNull() || userID.IsNull() {
		return
	}
	collection, err := parseCollectionID(collectionID.ValueString())
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("collection_id"), "Invalid collection UUID", err.Error())
		return
	}
	user, err := parseUserID(userID.ValueString())
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("user_id"), "Invalid user UUID", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("id"), collectionUserID(collection, user))...)
}

func (r *collectionUserResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	api, ok := req.ProviderData.(*apiClient)
	if !ok || api == nil {
		resp.Diagnostics.AddError("Unexpected resource client", "Expected an Outline API client.")
		return
	}
	r.api = api
}

func (r *collectionUserResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan collectionUserModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	collection, user, err := plan.pair()
	permission := client.Permission(plan.Permission.ValueString())
	if err == nil && !permission.Valid() {
		err = errors.New("collection grant permission must be read, read_write, or admin")
	}
	var current *client.Membership
	if err == nil {
		current, err = r.api.observeCollectionUser(ctx, collection, user)
	}
	if err != nil {
		resp.Diagnostics.AddError("Unable to create collection user", err.Error())
		return
	}
	if current != nil {
		resp.Diagnostics.AddError("Collection user grant already exists", "Import the existing pair using "+collectionUserID(collection, user)+" before managing its permission. No grant was changed.")
		return
	}
	// Keep the trusted pair before attempting the write. A response or transport
	// failure can follow a committed upsert and must not discard recovery state.
	plan.ID = types.StringValue(collectionUserID(collection, user))
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	_, err = r.api.writeCollectionUser(ctx, collection, user, permission)
	if err != nil {
		resp.Diagnostics.AddError("Unable to create collection user", err.Error()+". The pair ID has been retained in state. Inspect the grant before retrying. Terraform taints failed creates; fix the error and use terraform untaint before reconciling in place, or import the pair if state was lost.")
	}
}

func (r *collectionUserResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state collectionUserModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	collection, user, err := state.pair()
	var member *client.Membership
	if err == nil {
		member, err = r.api.refreshCollectionUser(ctx, collection, user)
	}
	if errors.Is(err, errNotFound) || err == nil && member == nil {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Unable to read collection user", err.Error())
		return
	}
	state.ID = types.StringValue(collectionUserID(collection, user))
	state.Permission = types.StringValue(string(*member.Permission))
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *collectionUserResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state collectionUserModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.State = req.State
	collection, user, err := plan.pair()
	if err == nil && (plan.CollectionID != state.CollectionID || plan.UserID != state.UserID) {
		err = errors.New("collection_id and user_id require replacement")
	}
	if err == nil {
		_, _, err = state.pair()
	}
	permission := client.Permission(plan.Permission.ValueString())
	if err == nil && !permission.Valid() {
		err = errors.New("collection grant permission must be read, read_write, or admin")
	}
	var current *client.Membership
	if err == nil {
		current, err = r.api.observeCollectionUser(ctx, collection, user)
	}
	if err == nil && current == nil {
		err = errors.New("grant disappeared before update; refresh state before applying again")
	}
	if err == nil && *current.Permission != permission {
		_, err = r.api.writeCollectionUser(ctx, collection, user, permission)
	}
	if err != nil {
		resp.Diagnostics.AddError("Unable to update collection user", err.Error())
		return
	}
	plan.ID = types.StringValue(collectionUserID(collection, user))
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *collectionUserResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state collectionUserModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	collection, user, err := state.pair()
	var current *client.Membership
	if err == nil {
		current, err = r.api.observeCollectionUser(ctx, collection, user)
	}
	if errors.Is(err, errNotFound) || err == nil && current == nil {
		return
	}
	if err == nil {
		err = r.api.removeCollectionUser(ctx, collection, user)
	}
	if err != nil {
		resp.Diagnostics.AddError("Unable to delete collection user", err.Error())
	}
}

func (r *collectionUserResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	collection, user, err := parseCollectionUserID(req.ID)
	var member *client.Membership
	if err == nil {
		member, err = r.api.observeCollectionUser(ctx, collection, user)
	}
	if err == nil && member == nil {
		err = errors.New("no explicit grant found for collection_id/user_id; inherited or default access is not an importable grant")
	}
	if err != nil {
		resp.Diagnostics.AddError("Unable to import collection user", err.Error())
		return
	}
	model := collectionUserModel{ID: types.StringValue(req.ID), CollectionID: types.StringValue(collection.String()), UserID: types.StringValue(user.String()), Permission: types.StringValue(string(*member.Permission))}
	resp.Diagnostics.Append(resp.State.Set(ctx, &model)...)
}
