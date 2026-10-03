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
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var _ resource.ResourceWithConfigure = &groupMemberResource{}
var _ resource.ResourceWithImportState = &groupMemberResource{}
var _ resource.ResourceWithModifyPlan = &groupMemberResource{}

type groupMemberResource struct{ api *apiClient }

func NewGroupMemberResource() resource.Resource { return &groupMemberResource{} }

func (r *groupMemberResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_group_member"
}

func (r *groupMemberResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages one user's membership in a manually maintained Outline group. Requires an active admin-owned API key. Existing pairs require import. Externally linked or synchronized groups are refused. Tested with Outline 1.10.1.",
		Attributes: map[string]schema.Attribute{
			"id":         schema.StringAttribute{Computed: true, MarkdownDescription: "Stable composite `group_id/user_id`, separated by exactly one slash. This is the import ID, not Outline's returned `userId-groupId` string."},
			"group_id":   schema.StringAttribute{Required: true, MarkdownDescription: "Canonical lowercase, nonzero group UUID. Changing it replaces this membership.", Validators: groupIDValidators(), PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()}},
			"user_id":    schema.StringAttribute{Required: true, MarkdownDescription: "Canonical lowercase, nonzero user UUID. The user must already exist in the workspace. Pending invitations need no password, SMTP, or first sign-in. Changing it replaces this membership.", Validators: groupIDValidators(), PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()}},
			"permission": schema.StringAttribute{Optional: true, Computed: true, Default: stringdefault.StaticString("member"), MarkdownDescription: "Group permission, `member` or `admin`. Defaults to `member`, including when reconciling an imported pair. This does not change the user's workspace role.", Validators: []validator.String{stringvalidator.OneOf("member", "admin")}},
		},
	}
}

// Compute the logical identity from the planned pair. Reusing the old ID during
// pair replacement would promise the wrong composite value to Terraform.
func (r *groupMemberResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() {
		return
	}
	var groupID, userID types.String
	resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("group_id"), &groupID)...)
	resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("user_id"), &userID)...)
	if resp.Diagnostics.HasError() || groupID.IsUnknown() || userID.IsUnknown() || groupID.IsNull() || userID.IsNull() {
		return
	}
	group, err := parseGroupID(groupID.ValueString())
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("group_id"), "Invalid group UUID", err.Error())
		return
	}
	user, err := parseUserID(userID.ValueString())
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("user_id"), "Invalid user UUID", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("id"), groupMemberID(group, user))...)
}

func (r *groupMemberResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

func (r *groupMemberResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan groupMemberModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	group, user, err := plan.pair()
	var current *client.GroupUser
	if err == nil {
		current, err = r.api.observeGroupMember(ctx, group, user)
	}
	if err != nil {
		resp.Diagnostics.AddError("Unable to create group member", err.Error())
		return
	}
	if current != nil {
		resp.Diagnostics.AddError("Group membership already exists", "Import the existing pair using "+groupMemberID(group, user)+" before managing its permission. No membership was changed.")
		return
	}
	permission := client.GroupPermission(plan.Permission.ValueString())
	if !permission.Valid() {
		resp.Diagnostics.AddError("Unable to create group member", "Group permission must be member or admin.")
		return
	}
	// The trusted pair is known before the write. Retain it even if decoding or
	// response validation fails after the server commits the mutation.
	plan.ID = types.StringValue(groupMemberID(group, user))
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	_, err = r.api.writeGroupMember(ctx, group, user, permission, true)
	if err != nil {
		resp.Diagnostics.AddError("Unable to create group member", err.Error()+". The pair ID has been retained in state. Inspect the membership before retrying. Terraform taints failed creates; fix the error and use terraform untaint before reconciling in place, or import the pair if state was lost.")
	}
}

func (r *groupMemberResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state groupMemberModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	group, user, err := state.pair()
	var member *client.GroupUser
	if err == nil {
		member, err = r.api.observeGroupMember(ctx, group, user)
	}
	if errors.Is(err, errNotFound) || (err == nil && member == nil) {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Unable to read group member", err.Error())
		return
	}
	state.ID = types.StringValue(groupMemberID(group, user))
	state.Permission = types.StringValue(string(*member.Permission))
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *groupMemberResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state groupMemberModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.State = req.State
	group, user, err := plan.pair()
	if err == nil && (plan.GroupID != state.GroupID || plan.UserID != state.UserID) {
		err = errors.New("group_id and user_id require replacement")
	}
	var current *client.GroupUser
	if err == nil {
		current, err = r.api.observeGroupMember(ctx, group, user)
	}
	if err == nil && current == nil {
		err = errors.New("membership disappeared before update; refresh state before applying again")
	}
	permission := client.GroupPermission(plan.Permission.ValueString())
	if err == nil && !permission.Valid() {
		err = errors.New("group permission must be member or admin")
	}
	if err == nil && *current.Permission != permission {
		_, err = r.api.writeGroupMember(ctx, group, user, permission, false)
	}
	if err != nil {
		resp.Diagnostics.AddError("Unable to update group member", err.Error())
		return
	}
	plan.ID = types.StringValue(groupMemberID(group, user))
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *groupMemberResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state groupMemberModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	group, user, err := state.pair()
	var current *client.GroupUser
	if err == nil {
		current, err = r.api.observeGroupMember(ctx, group, user)
	}
	if errors.Is(err, errNotFound) || (err == nil && current == nil) {
		return
	}
	if err == nil {
		err = r.api.removeGroupMember(ctx, group, user)
	}
	if err != nil {
		resp.Diagnostics.AddError("Unable to delete group member", err.Error())
	}
}

func (r *groupMemberResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	group, user, err := parseGroupMemberID(req.ID)
	if err != nil {
		resp.Diagnostics.AddError("Invalid group member import ID", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("group_id"), group.String())...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("user_id"), user.String())...)
}
