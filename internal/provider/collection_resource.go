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
	"github.com/oapi-codegen/nullable"
)

var _ resource.ResourceWithConfigure = &collectionResource{}
var _ resource.ResourceWithImportState = &collectionResource{}

type collectionResource struct{ api *apiClient }

func NewCollectionResource() resource.Resource { return &collectionResource{} }

func (r *collectionResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_collection"
}

func (r *collectionResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages an active Outline collection by stable UUID. Requires an active admin-owned API key. Tested with Outline 1.10.1. Creation grants the caller direct collection-admin membership. Deletion trashes published content and is blocked unless allow_destroy is true. Does not manage documents or per-user/group grants.",
		Attributes: map[string]schema.Attribute{
			"id":            schema.StringAttribute{Computed: true, MarkdownDescription: "Stable collection UUID. Import using this canonical lowercase, nonzero UUID.", PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"name":          schema.StringAttribute{Required: true, MarkdownDescription: "Collection name, 1 to 100 Unicode code points, with no URL or surrounding whitespace. Changes update in place.", Validators: []validator.String{collectionText{name: true}}},
			"description":   schema.StringAttribute{Optional: true, Computed: true, Default: stringdefault.StaticString(""), MarkdownDescription: "Markdown landing-page description, at most 100000 Unicode code points. Defaults to an empty string; removing configuration clears it. Rich-editor changes outside Terraform can rewrite the returned Markdown.", Validators: []validator.String{collectionText{}}},
			"permission":    schema.StringAttribute{Optional: true, MarkdownDescription: "Default workspace access, read or read_write. Omitted or null means private and removes default access on apply. The provider always sends an explicit value or JSON null, never an omitted update permission. Direct user/group grants remain separate and are not removed when this becomes null. The server's admin default is not supported by this resource.", Validators: []validator.String{stringvalidator.OneOf("read", "read_write")}},
			"sharing":       schema.BoolAttribute{Optional: true, Computed: true, Default: booldefault.StaticBool(false), MarkdownDescription: "Allow public document sharing in this collection. Defaults to false, explicitly overriding Outline's true creation default. Workspace sharing policies still apply."},
			"allow_destroy": schema.BoolAttribute{Optional: true, Computed: true, Default: booldefault.StaticBool(false), MarkdownDescription: "Permit destructive collections.delete on Terraform destroy. Defaults to false, which returns an error and retains state. Apply true before destroying. The server trashes published, non-archived ordinary documents, detaches drafts asynchronously, and retains collection grant rows. This is not non-destructive or an immediate full purge. Import resets this local guard to false."},
		},
	}
}

func (r *collectionResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

func (r *collectionResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan collectionModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if _, err := r.api.requireIAMAdmin(ctx, "outline_collection"); err != nil {
		resp.Diagnostics.AddError("Unable to create collection", err.Error())
		return
	}
	sharing := plan.Sharing.ValueBool()
	response, createdID, err := r.api.createCollection(ctx, client.CollectionsCreateJSONRequestBody{
		Name: plan.Name.ValueString(), Description: nullable.NewNullableWithValue(plan.Description.ValueString()),
		Permission: collectionPermission(plan.Permission), Sharing: &sharing,
	})
	// Retain a canonical UUID even if another field prevents generated decoding.
	if createdID != uuid.Nil {
		initial := plan
		initial.ID = types.StringValue(createdID.String())
		resp.Diagnostics.Append(resp.State.Set(ctx, &initial)...)
	}
	if response == nil {
		resp.Diagnostics.AddError("Unable to create collection", r.api.checkResponse("collections.create", nil, nil, err).Error())
		return
	}
	if err = r.api.checkResponse("collections.create", response.HTTPResponse, response.Body, err); err != nil {
		detail := err.Error()
		if createdID != uuid.Nil {
			detail += ". The returned collection UUID has been retained in state. Inspect the workspace, fix the error, and use terraform untaint to reconcile the existing collection instead of replacing it."
		}
		resp.Diagnostics.AddError("Unable to create collection", detail)
		return
	}
	if response.JSON200 == nil {
		resp.Diagnostics.AddError("Unable to create collection", "collections.create: missing JSON response")
		return
	}
	collection := response.JSON200.Data
	if err = checkEnvelope("collections.create", response.JSON200.Ok, response.JSON200.Status); err == nil {
		err = validateCollection(collection, uuid.Nil)
	}
	if err == nil {
		err = managedCollection(collection)
	}
	if err != nil {
		resp.Diagnostics.AddError("Unable to create collection", err.Error()+". If an ID was returned it has been retained in state. Inspect the workspace before retrying. Terraform taints failed creates; fix the error and untaint an existing collection to reconcile it in place, or import its UUID if state was lost.")
		return
	}
	plan.setCollection(collection)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *collectionResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state collectionModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	id, err := parseCollectionID(state.ID.ValueString())
	var collection *client.Collection
	if err == nil {
		collection, err = r.api.readCollection(ctx, id)
	}
	if errors.Is(err, errNotFound) {
		resp.State.RemoveResource(ctx)
		return
	}
	if err == nil {
		err = managedCollection(collection)
	}
	if err != nil {
		resp.Diagnostics.AddError("Unable to read collection", err.Error())
		return
	}
	state.setCollection(collection)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *collectionResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan collectionModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	id, err := parseCollectionID(plan.ID.ValueString())
	var collection *client.Collection
	if err == nil {
		collection, err = r.api.readCollection(ctx, id)
	}
	if err == nil {
		err = managedCollection(collection)
	}
	if err == nil {
		collection, err = r.api.updateCollection(ctx, id, plan)
	}
	if err != nil {
		resp.Diagnostics.AddError("Unable to update collection", err.Error())
		return
	}
	plan.setCollection(collection)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *collectionResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state collectionModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	id, err := parseCollectionID(state.ID.ValueString())
	var collection *client.Collection
	if err == nil {
		collection, err = r.api.readCollection(ctx, id)
	}
	if errors.Is(err, errNotFound) {
		return
	}
	if err == nil {
		err = managedCollection(collection)
	}
	if err != nil {
		resp.Diagnostics.AddError("Unable to delete collection", err.Error())
		return
	}
	if !state.AllowDestroy.ValueBool() {
		resp.Diagnostics.AddError("Collection destruction blocked", "collections.delete trashes collection content. Apply allow_destroy = true before destroying, or use terraform state rm to stop managing without deletion. The collection remains in state.")
		return
	}
	response, err := r.api.CollectionsDeleteWithResponse(ctx, client.CollectionsDeleteJSONRequestBody{Id: id})
	if response == nil {
		err = r.api.checkResponse("collections.delete", nil, nil, err)
	} else {
		err = r.api.checkResponse("collections.delete", response.HTTPResponse, response.Body, err)
		if err == nil {
			if response.JSON200 == nil {
				err = errors.New("collections.delete: missing JSON response")
			} else {
				err = checkEnvelope("collections.delete", response.JSON200.Ok, response.JSON200.Status)
				if err == nil && (response.JSON200.Success == nil || !*response.JSON200.Success) {
					err = errors.New("collections.delete: missing or false success flag")
				}
			}
		}
	}
	// A delete 403/404 is not the info endpoint's absence contract. Preserve
	// state on any failed write and let the next refresh establish absence.
	if err != nil {
		resp.Diagnostics.AddError("Unable to delete collection", err.Error())
	}
}

func (r *collectionResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	if _, err := parseCollectionID(req.ID); err != nil {
		resp.Diagnostics.AddError("Invalid collection import ID", err.Error())
		return
	}
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("allow_destroy"), false)...)
}
