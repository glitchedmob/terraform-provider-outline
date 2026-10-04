// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/glitchedmob/terraform-provider-outline/internal/client"
	"github.com/google/uuid"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var _ resource.ResourceWithConfigure = &groupResource{}
var _ resource.ResourceWithImportState = &groupResource{}

type groupResource struct{ api *apiClient }

func NewGroupResource() resource.Resource { return &groupResource{} }

func (r *groupResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_group"
}

func (r *groupResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a manually maintained Outline group. Externally linked or synchronized groups are not supported. Requires an admin-owned API key. Tested with Outline 1.10.1.",
		Attributes: map[string]schema.Attribute{
			"id":               schema.StringAttribute{Computed: true, MarkdownDescription: "Stable Outline group UUID. Import using this ID.", PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"name":             schema.StringAttribute{Required: true, MarkdownDescription: "Group name, 1 to 255 UTF-16 code units, with at least one non-whitespace character. Outline 1.10.1 rejects duplicate manual group names.", Validators: groupNameValidators()},
			"description":      schema.StringAttribute{Optional: true, Computed: true, Default: stringdefault.StaticString(""), MarkdownDescription: "Group description, at most 2000 UTF-16 code units. Defaults to an empty string. Outline 1.10.1 requires an update after creation to set a nonempty description.", Validators: []validator.String{outlineStringLength{min: 0, max: 2000}}},
			"disable_mentions": schema.BoolAttribute{Optional: true, Computed: true, Default: booldefault.StaticBool(false), MarkdownDescription: "Disable mentions of this group. Defaults to `false`."},
		},
	}
}

func (r *groupResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

func (r *groupResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan groupModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	mentions := plan.DisableMentions.ValueBool()
	response, err := r.api.GroupsCreateWithResponse(ctx, client.GroupsCreateJSONRequestBody{Name: plan.Name.ValueString(), DisableMentions: &mentions})
	if response == nil {
		resp.Diagnostics.AddError("Unable to create group", r.api.checkResponse("groups.create", nil, nil, err).Error())
		return
	}
	if err = r.api.checkResponse("groups.create", response.HTTPResponse, response.Body, err); err != nil {
		resp.Diagnostics.AddError("Unable to create group", err.Error())
		return
	}
	if response.JSON200 == nil {
		resp.Diagnostics.AddError("Unable to create group", "groups.create: missing JSON response")
		return
	}
	group := response.JSON200.Data
	// Save the returned identity before any validation or second write. A failed
	// description update must not leave an orphan and trigger another create.
	if group != nil && group.Id != nil && *group.Id != uuid.Nil {
		initial := plan
		initial.ID = types.StringValue(group.Id.String())
		resp.Diagnostics.Append(resp.State.Set(ctx, &initial)...)
	}
	if err = checkEnvelope("groups.create", response.JSON200.Ok, response.JSON200.Status); err == nil {
		err = validateGroup(group, uuid.Nil)
	}
	if err == nil {
		err = managedGroup(group)
	}
	if err != nil {
		resp.Diagnostics.AddError("Unable to create group", err.Error())
		return
	}
	initial := plan
	initial.setGroup(group)
	resp.Diagnostics.Append(resp.State.Set(ctx, &initial)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if plan.Description.ValueString() != "" {
		group, err = r.api.updateGroup(ctx, *group.Id, plan)
		if err != nil {
			resp.Diagnostics.AddError("Group created but description update failed", err.Error()+". The group ID has been retained in state. Terraform taints failed creates. Fix the API error, then use terraform untaint on this resource before applying again to update the existing group instead of replacing it.")
			return
		}
	}
	plan.setGroup(group)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *groupResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state groupModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	id, err := parseGroupID(state.ID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Unable to read group", err.Error())
		return
	}
	group, err := r.api.readGroup(ctx, id)
	if errors.Is(err, errNotFound) {
		resp.State.RemoveResource(ctx)
		return
	}
	if err == nil {
		err = managedGroup(group)
	}
	if err != nil {
		resp.Diagnostics.AddError("Unable to read group", err.Error())
		return
	}
	state.setGroup(group)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *groupResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan groupModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	id, err := parseGroupID(plan.ID.ValueString())
	if err == nil {
		// Reject a newly externalized group before sending any mutable fields.
		var current *client.Group
		current, err = r.api.readGroup(ctx, id)
		if err == nil {
			err = managedGroup(current)
		}
	}
	var group *client.Group
	if err == nil {
		group, err = r.api.updateGroup(ctx, id, plan)
	}
	if err != nil {
		resp.Diagnostics.AddError("Unable to update group", err.Error())
		return
	}
	plan.setGroup(group)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *groupResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state groupModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	id, err := parseGroupID(state.ID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Unable to delete group", err.Error())
		return
	}
	current, err := r.api.readGroup(ctx, id)
	if errors.Is(err, errNotFound) {
		return
	}
	if err == nil {
		err = managedGroup(current)
	}
	if err != nil {
		resp.Diagnostics.AddError("Unable to delete group", err.Error())
		return
	}
	response, err := r.api.GroupsDeleteWithResponse(ctx, client.GroupsDeleteJSONRequestBody{Id: id})
	if response == nil {
		resp.Diagnostics.AddError("Unable to delete group", r.api.checkResponse("groups.delete", nil, nil, err).Error())
		return
	}
	requestErr := err
	err = r.api.checkResponse("groups.delete", response.HTTPResponse, response.Body, requestErr)
	if requestErr == nil && response.StatusCode() == http.StatusNotFound {
		// A delete route/proxy 404 does not prove that the group disappeared.
		// Only the trusted read/full admin-list strategy can establish absence.
		_, verifyErr := r.api.readGroup(ctx, id)
		if errors.Is(verifyErr, errNotFound) {
			return
		}
		if verifyErr != nil {
			err = fmt.Errorf("%w; cannot establish group absence: %v", err, verifyErr)
		}
	}
	if err == nil {
		if response.JSON200 == nil {
			err = errors.New("groups.delete: missing JSON response")
		} else {
			err = checkEnvelope("groups.delete", response.JSON200.Ok, response.JSON200.Status)
			if err == nil && (response.JSON200.Success == nil || !*response.JSON200.Success) {
				err = errors.New("groups.delete: missing or false success flag")
			}
		}
	}
	if err != nil {
		resp.Diagnostics.AddError("Unable to delete group", err.Error())
	}
}

func (r *groupResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	if _, err := parseGroupID(req.ID); err != nil {
		resp.Diagnostics.AddError("Invalid group import ID", err.Error())
		return
	}
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}
