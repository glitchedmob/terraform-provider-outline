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

var _ resource.ResourceWithConfigure = &collectionGroupResource{}
var _ resource.ResourceWithImportState = &collectionGroupResource{}
var _ resource.ResourceWithModifyPlan = &collectionGroupResource{}

type collectionGroupResource struct{ api *apiClient }

func NewCollectionGroupResource() resource.Resource { return &collectionGroupResource{} }

func (r *collectionGroupResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_collection_group"
}

func (r *collectionGroupResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages one explicit collection/group access grant, not effective or inherited access. Requires an active admin-owned API key. Existing pairs require import. Externally linked and synchronized groups are supported because this resource does not change their names or user memberships. Archived collections are refused. Tested with Outline 1.10.1.",
		Attributes: map[string]schema.Attribute{
			"id":            schema.StringAttribute{Computed: true, MarkdownDescription: "Stable composite `collection_id/group_id`, separated by exactly one slash. This is the import ID, not the grant row's API UUID."},
			"collection_id": schema.StringAttribute{Required: true, MarkdownDescription: "Canonical lowercase, nonzero collection UUID. Changing it replaces this grant.", Validators: groupIDValidators(), PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()}},
			"group_id":      schema.StringAttribute{Required: true, MarkdownDescription: "Canonical lowercase, nonzero group UUID. The group must already exist. Changing it replaces this grant.", Validators: groupIDValidators(), PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()}},
			"permission":    schema.StringAttribute{Required: true, MarkdownDescription: "Explicit collection grant permission, `read`, `read_write`, or `admin`. Required, with no default. Does not change collection default access, workspace roles, or membership within the group.", Validators: []validator.String{stringvalidator.OneOf("read", "read_write", "admin")}},
		},
	}
}

func (r *collectionGroupResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() {
		return
	}
	var collectionID, groupID types.String
	resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("collection_id"), &collectionID)...)
	resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("group_id"), &groupID)...)
	if resp.Diagnostics.HasError() || collectionID.IsUnknown() || groupID.IsUnknown() || collectionID.IsNull() || groupID.IsNull() {
		return
	}
	collection, err := parseCollectionID(collectionID.ValueString())
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("collection_id"), "Invalid collection UUID", err.Error())
		return
	}
	group, err := parseGroupID(groupID.ValueString())
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("group_id"), "Invalid group UUID", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("id"), collectionGroupID(collection, group))...)
}

func (r *collectionGroupResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

func (r *collectionGroupResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan collectionGroupModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	collection, group, err := plan.pair()
	permission := client.Permission(plan.Permission.ValueString())
	if err == nil && !permission.Valid() {
		err = errors.New("collection grant permission must be read, read_write, or admin")
	}
	var current *client.GroupMembership
	if err == nil {
		current, err = r.api.observeCollectionGroup(ctx, collection, group)
	}
	if err != nil {
		resp.Diagnostics.AddError("Unable to create collection group", err.Error())
		return
	}
	if current != nil {
		resp.Diagnostics.AddError("Collection group grant already exists", "Import the existing pair using "+collectionGroupID(collection, group)+" before managing its permission. No grant was changed.")
		return
	}
	// Keep the trusted pair before attempting the write. A response or transport
	// failure can follow a committed upsert and must not discard recovery state.
	plan.ID = types.StringValue(collectionGroupID(collection, group))
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	_, err = r.api.writeCollectionGroup(ctx, collection, group, permission)
	if err != nil {
		resp.Diagnostics.AddError("Unable to create collection group", err.Error()+". The pair ID has been retained in state. Inspect the grant before retrying. Terraform taints failed creates; fix the error and use terraform untaint before reconciling in place, or import the pair if state was lost.")
	}
}

func (r *collectionGroupResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state collectionGroupModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	collection, group, err := state.pair()
	var member *client.GroupMembership
	if err == nil {
		member, err = r.api.refreshCollectionGroup(ctx, collection, group)
	}
	if errors.Is(err, errNotFound) || err == nil && member == nil {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Unable to read collection group", err.Error())
		return
	}
	state.ID = types.StringValue(collectionGroupID(collection, group))
	state.Permission = types.StringValue(string(*member.Permission))
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *collectionGroupResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state collectionGroupModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.State = req.State
	collection, group, err := plan.pair()
	if err == nil && (plan.CollectionID != state.CollectionID || plan.GroupID != state.GroupID) {
		err = errors.New("collection_id and group_id require replacement")
	}
	if err == nil {
		_, _, err = state.pair()
	}
	permission := client.Permission(plan.Permission.ValueString())
	if err == nil && !permission.Valid() {
		err = errors.New("collection grant permission must be read, read_write, or admin")
	}
	var current *client.GroupMembership
	if err == nil {
		current, err = r.api.observeCollectionGroup(ctx, collection, group)
	}
	if err == nil && current == nil {
		err = errors.New("grant disappeared before update; refresh state before applying again")
	}
	if err == nil && *current.Permission != permission {
		_, err = r.api.writeCollectionGroup(ctx, collection, group, permission)
	}
	if err != nil {
		resp.Diagnostics.AddError("Unable to update collection group", err.Error())
		return
	}
	plan.ID = types.StringValue(collectionGroupID(collection, group))
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *collectionGroupResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state collectionGroupModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	collection, group, err := state.pair()
	var current *client.GroupMembership
	if err == nil {
		current, err = r.api.observeCollectionGroup(ctx, collection, group)
	}
	if errors.Is(err, errNotFound) || err == nil && current == nil {
		return
	}
	if err == nil {
		err = r.api.removeCollectionGroup(ctx, collection, group)
	}
	if err != nil {
		resp.Diagnostics.AddError("Unable to delete collection group", err.Error())
	}
}

func (r *collectionGroupResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	collection, group, err := parseCollectionGroupID(req.ID)
	var member *client.GroupMembership
	if err == nil {
		member, err = r.api.observeCollectionGroup(ctx, collection, group)
	}
	if err == nil && member == nil {
		err = errors.New("no explicit grant found for collection_id/group_id; inherited or default access is not an importable grant")
	}
	if err != nil {
		resp.Diagnostics.AddError("Unable to import collection group", err.Error())
		return
	}
	model := collectionGroupModel{ID: types.StringValue(req.ID), CollectionID: types.StringValue(collection.String()), GroupID: types.StringValue(group.String()), Permission: types.StringValue(string(*member.Permission))}
	resp.Diagnostics.Append(resp.State.Set(ctx, &model)...)
}
