// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"errors"

	"github.com/glitchedmob/terraform-provider-outline/internal/client"
	"github.com/google/uuid"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
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

var _ resource.ResourceWithConfigure = &userResource{}
var _ resource.ResourceWithImportState = &userResource{}

type userResource struct{ api *apiClient }

func NewUserResource() resource.Resource { return &userResource{} }

func (r *userResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_user"
}

func (r *userResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages an Outline workspace account by UUID. Creates a pending invitation without email by default, suitable for OIDC. Requires an active admin-owned API key and refuses to manage its owner. Destroy suspends by default; it does not erase the account. Tested with Outline 1.10.1.",
		Attributes: map[string]schema.Attribute{
			"id":                 schema.StringAttribute{Computed: true, MarkdownDescription: "Stable user UUID. Import this ID to manage an existing account.", PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"email":              schema.StringAttribute{Required: true, MarkdownDescription: "Account email. Invitations use lowercase email. Equivalent configured casing is preserved in state. Email changes require replacement, not administrative renaming. With default destruction, the old account remains suspended; returning to its email requires import.", Validators: userEmailValidators(), PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()}},
			"name":               schema.StringAttribute{Optional: true, Computed: true, MarkdownDescription: "Optional managed display name, 1 to 255 Unicode code points, without URLs. On creation, omission uses Pending user. After creation, omission stops name management. IdP sign-in can change the name; a configured name is restored on apply.", Validators: []validator.String{outlineUserName{}}},
			"role":               schema.StringAttribute{Optional: true, Computed: true, Default: stringdefault.StaticString("member"), MarkdownDescription: "Workspace role: admin, member, viewer, or guest. Defaults to member. Server edition, licensing, key scopes, and policies can reject role changes. Guest invitations are reconciled with a follow-up role update.", Validators: []validator.String{stringvalidator.OneOf("admin", "member", "viewer", "guest")}},
			"suspended":          schema.BoolAttribute{Required: true, MarkdownDescription: "Explicit desired final suspension state. Set false to activate, true to suspend. Changing a suspended account's role temporarily activates it, changes the role, then restores this desired state. Activation is not a sign-in or proof of an active IdP account."},
			"suppress_email":     schema.BoolAttribute{Optional: true, Computed: true, Default: booldefault.StaticBool(true), MarkdownDescription: "Suppress the invitation email. Defaults to true for OIDC provisioning without SMTP. Used only when creating the account; changing it does not send or resend an invitation."},
			"delete_permanently": schema.BoolAttribute{Optional: true, Computed: true, Default: booldefault.StaticBool(false), MarkdownDescription: "Opt in to users.delete on destruction. Defaults to false, which only suspends and retains the account and memberships. Deletion is irreversible through this provider; Outline may retain anonymized soft-deleted database rows. Does not delete documents or guarantee erasure of retained content. Never deletes the API-key owner."},
		},
	}
}

func (r *userResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

const userRecovery = " The user ID has been retained in state. Terraform taints failed creates. Fix the API error, then run terraform untaint on this resource before applying again. Default replacement only suspends the account and a new Create refuses its existing email, so do not retry replacement blindly. If state was lost, import the existing UUID."

func (r *userResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan userModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	actor, err := r.api.requireUserAdmin(ctx)
	if err == nil {
		err = protectUserOwner(actor, uuid.Nil, plan.Email.ValueString())
	}
	if err != nil {
		resp.Diagnostics.AddError("Unable to create user", err.Error())
		return
	}
	// Preflight the complete list, including suspended users. The invitation
	// response is still checked because another client can race this request.
	existingIDs := make(map[uuid.UUID]bool)
	existingEmail := false
	err = r.api.walkUsers(ctx, func(user *client.User) error {
		existingIDs[*user.Id] = true
		if normalizeUserEmail(string(user.Email.GetOrEmpty())) == normalizeUserEmail(plan.Email.ValueString()) {
			existingEmail = true
		}
		return nil
	})
	if err != nil {
		resp.Diagnostics.AddError("Unable to check existing user", err.Error())
		return
	}
	if existingEmail {
		resp.Diagnostics.AddError("User already exists", "Create never adopts or modifies existing accounts. Import the existing user's UUID instead.")
		return
	}
	email := normalizeUserEmail(plan.Email.ValueString())
	name := "Pending user"
	manageName := !plan.Name.IsNull() && !plan.Name.IsUnknown()
	if manageName {
		name = plan.Name.ValueString()
	}
	suppress := plan.SuppressEmail.ValueBool()
	response, err := r.api.UsersInviteWithResponse(ctx, client.UsersInviteJSONRequestBody{Invites: []client.Invite{{Email: email, Name: name, Role: client.UserRole(plan.Role.ValueString())}}, SuppressEmail: &suppress})
	if response == nil {
		resp.Diagnostics.AddError("Unable to invite user", r.api.checkResponse("users.invite", nil, nil, err).Error())
		return
	}
	if err = r.api.checkResponse("users.invite", response.HTTPResponse, response.Body, err); err != nil {
		resp.Diagnostics.AddError("Unable to invite user", err.Error()+" Check for a pending account before retrying; import it if the server committed the invitation.")
		return
	}
	if response.JSON200 == nil || response.JSON200.Data == nil {
		resp.Diagnostics.AddError("Unable to invite user", "users.invite: missing JSON data; check the workspace and import any created account before retrying.")
		return
	}
	data := response.JSON200.Data
	if data.Unsent != nil && len(*data.Unsent) > 0 {
		resp.Diagnostics.AddError("Invitation was not created", "users.invite returned unsent invitations. Existing users are never adopted or modified on Create. Import the existing user's UUID instead.")
		return
	}
	if data.Users == nil || len(*data.Users) != 1 {
		resp.Diagnostics.AddError("Unable to invite user", "users.invite: expected exactly one newly created user; inspect the workspace and import any created account before retrying.")
		return
	}
	user := &(*data.Users)[0]
	retained := false
	// Persist only an identity that matches our single invitation, never another
	// account or the key owner. Do so before envelope/full model validation and
	// follow-up reads or writes, which can fail after the invitation committed.
	if user.Id != nil && *user.Id != uuid.Nil && !existingIDs[*user.Id] && user.Email.IsSpecified() && !user.Email.IsNull() && normalizeUserEmail(string(user.Email.GetOrEmpty())) == email && *user.Id != *actor.Id {
		initial := plan
		initial.ID = types.StringValue(user.Id.String())
		// Preserve observed suspension, not the desired plan, on failed creates.
		// A missing status is unknown until a validated read or write confirms it.
		initial.Suspended = types.BoolNull()
		if user.IsSuspended != nil {
			initial.Suspended = types.BoolValue(*user.IsSuspended)
		}
		if initial.Name.IsNull() || initial.Name.IsUnknown() {
			initial.Name = types.StringValue(name)
		}
		resp.Diagnostics.Append(resp.State.Set(ctx, &initial)...)
		retained = !resp.Diagnostics.HasError()
	}
	if resp.Diagnostics.HasError() {
		return
	}
	if err = checkEnvelope("users.invite", response.JSON200.Ok, response.JSON200.Status); err == nil {
		err = validateUser(user, uuid.Nil, email)
	}
	if err == nil {
		err = protectUserOwner(actor, *user.Id, email)
	}
	if err == nil && existingIDs[*user.Id] {
		err = errors.New("users.invite returned an existing user ID; refusing to adopt or modify it")
	}
	if err == nil && (data.Sent == nil || len(*data.Sent) != 1 || normalizeUserEmail((*data.Sent)[0].Email) != email || data.Unsent == nil) {
		err = errors.New("users.invite: malformed sent/unsent invitation result")
	}
	if err != nil {
		if retained && plan.Suspended.ValueBool() {
			// Metadata failure must not leave a newly invited account active.
			// Verify the candidate independently before any compensating write.
			var stored *client.User
			stored, err = r.api.recoverInvitedUserSuspension(ctx, actor, *user.Id, email, err)
			if stored != nil {
				initial := plan
				initial.setUser(stored)
				resp.Diagnostics.Append(resp.State.Set(ctx, &initial)...)
			}
		}
		detail := err.Error()
		if retained {
			detail += userRecovery
		} else {
			detail += " No trusted user ID was retained. Inspect the workspace and import any newly created account before retrying."
		}
		resp.Diagnostics.AddError("Unable to validate invited user", detail)
		return
	}
	initial := plan
	initial.setUser(user)
	resp.Diagnostics.Append(resp.State.Set(ctx, &initial)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// Read the stored role rather than assuming the invitation respected it.
	invited := user
	user, err = r.api.readUser(ctx, *user.Id)
	if err == nil {
		err = validateUser(user, uuid.MustParse(initial.ID.ValueString()), email)
	}
	if err != nil && plan.Suspended.ValueBool() {
		user, err = r.api.recoverUserSuspension(ctx, invited, err)
	}
	if err == nil {
		user, err = r.api.updateUser(ctx, user, plan, manageName)
	}
	if user != nil && validateUser(user, uuid.MustParse(initial.ID.ValueString()), email) == nil {
		initial.setUser(user)
		resp.Diagnostics.Append(resp.State.Set(ctx, &initial)...)
	}
	if err != nil {
		resp.Diagnostics.AddError("User created but reconciliation failed", err.Error()+userRecovery)
		return
	}
	plan.setUser(user)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *userResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state userModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	id, err := parseUserID(state.ID.ValueString())
	if err == nil {
		_, err = r.api.requireUserAdmin(ctx)
	}
	var user *client.User
	if err == nil {
		user, err = r.api.readUser(ctx, id)
	}
	if errors.Is(err, errNotFound) {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Unable to read user", err.Error())
		return
	}
	state.setUser(user)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *userResource) managedUser(ctx context.Context, model userModel) (*client.User, error) {
	id, err := parseUserID(model.ID.ValueString())
	if err != nil {
		return nil, err
	}
	actor, err := r.api.requireUserAdmin(ctx)
	if err == nil {
		err = protectUserOwner(actor, id, model.Email.ValueString())
	}
	if err != nil {
		return nil, err
	}
	user, err := r.api.readUser(ctx, id)
	if err == nil {
		err = validateUser(user, id, model.Email.ValueString())
	}
	return user, err
}

func (r *userResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, config userModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}
	user, err := r.managedUser(ctx, plan)
	if err == nil {
		user, err = r.api.updateUser(ctx, user, plan, !config.Name.IsNull() && !config.Name.IsUnknown())
	}
	if user != nil && validateUser(user, uuid.MustParse(plan.ID.ValueString()), plan.Email.ValueString()) == nil {
		plan.setUser(user)
		resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	}
	if err != nil {
		resp.Diagnostics.AddError("Unable to update user", err.Error())
		return
	}
}

func (r *userResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state userModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	user, err := r.managedUser(ctx, state)
	if errors.Is(err, errNotFound) {
		return
	}
	if err == nil {
		if state.DeletePermanently.ValueBool() {
			err = r.api.deleteUser(ctx, user)
		} else {
			_, err = r.api.setUserSuspended(ctx, user, true)
		}
	}
	if err != nil {
		resp.Diagnostics.AddError("Unable to destroy user", err.Error())
	}
}

func (r *userResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	if _, err := parseUserID(req.ID); err != nil {
		resp.Diagnostics.AddError("Invalid user import ID", err.Error())
		return
	}
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("suppress_email"), true)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("delete_permanently"), false)...)
}
