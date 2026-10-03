// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/glitchedmob/terraform-provider-outline/internal/client"
	"github.com/google/uuid"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/oapi-codegen/nullable"
)

type collectionModel struct {
	ID           types.String `tfsdk:"id"`
	Name         types.String `tfsdk:"name"`
	Description  types.String `tfsdk:"description"`
	Permission   types.String `tfsdk:"permission"`
	Sharing      types.Bool   `tfsdk:"sharing"`
	AllowDestroy types.Bool   `tfsdk:"allow_destroy"`
}

type collectionLookupModel struct {
	ID          types.String `tfsdk:"id"`
	Name        types.String `tfsdk:"name"`
	Description types.String `tfsdk:"description"`
	Permission  types.String `tfsdk:"permission"`
	Sharing     types.Bool   `tfsdk:"sharing"`
}

// Collection ORM Length counts code points, unlike the group Zod limits.
// Update trims names. Reject surrounding whitespace before a write to avoid
// inconsistent results, and match the released ORM's NotContainsUrl rule.
type collectionText struct{ name bool }

func (v collectionText) Description(context.Context) string {
	if v.name {
		return "must contain 1 to 100 Unicode code points, no URL, and no surrounding whitespace"
	}
	return "must contain at most 100000 Unicode code points"
}
func (v collectionText) MarkdownDescription(ctx context.Context) string { return v.Description(ctx) }
func (v collectionText) ValidateString(ctx context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	text := req.ConfigValue.ValueString()
	n := utf8.RuneCountInString(text)
	invalid := n > 100000
	if v.name {
		invalid = n < 1 || n > 100 || strings.TrimFunc(text, func(r rune) bool { return unicode.IsSpace(r) || r == '\uFEFF' }) != text || outlineUserNameURL.MatchString(text)
	}
	if invalid {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid collection text", v.Description(ctx))
	}
}

func parseCollectionID(id string) (uuid.UUID, error) {
	parsed, err := parseGroupID(id)
	if err != nil {
		return uuid.Nil, errors.New("collection ID must be a canonical lowercase, nonzero UUID")
	}
	return parsed, nil
}

func validateCollection(collection *client.Collection, expectedID uuid.UUID) error {
	if collection == nil || collection.Id == nil || collection.Name == nil || *collection.Name == "" || collection.Sharing == nil ||
		!collection.Description.IsSpecified() || !collection.Permission.IsSpecified() || !collection.ArchivedAt.IsSpecified() || !collection.DeletedAt.IsSpecified() {
		return errors.New("malformed collection response: missing ID, name, markdown description, permission, sharing, archivedAt, or deletedAt")
	}
	if _, err := parseCollectionID(collection.Id.String()); err != nil {
		return fmt.Errorf("malformed collection response: %w", err)
	}
	if expectedID != uuid.Nil && *collection.Id != expectedID {
		return errors.New("collection response returned a different ID")
	}
	if !collection.Permission.IsNull() && !collection.Permission.GetOrEmpty().Valid() {
		return errors.New("collection response returned an unsupported default permission")
	}
	if !collection.DeletedAt.IsNull() {
		return errors.New("collection response unexpectedly returned a deleted collection")
	}
	return nil
}

func managedCollection(collection *client.Collection) error {
	if !collection.ArchivedAt.IsNull() {
		return errors.New("archived collections cannot be managed by this resource; restore the collection outside Terraform or use the lookup data source")
	}
	if !collection.Permission.IsNull() && collection.Permission.GetOrEmpty() == client.PermissionAdmin {
		return errors.New("the server's admin default permission is not supported by this resource; use read, read_write, or null outside Terraform before importing, or use the lookup data source")
	}
	return nil
}

func (m *collectionLookupModel) setCollection(collection *client.Collection) {
	m.ID = types.StringValue(collection.Id.String())
	m.Name = types.StringValue(*collection.Name)
	m.Description = types.StringValue(collection.Description.GetOrEmpty())
	m.Permission = types.StringNull()
	if !collection.Permission.IsNull() {
		m.Permission = types.StringValue(string(collection.Permission.GetOrEmpty()))
	}
	m.Sharing = types.BoolValue(*collection.Sharing)
}

func (m *collectionModel) setCollection(collection *client.Collection) {
	lookup := collectionLookupModel{}
	lookup.setCollection(collection)
	m.ID, m.Name, m.Description, m.Permission, m.Sharing = lookup.ID, lookup.Name, lookup.Description, lookup.Permission, lookup.Sharing
	if m.AllowDestroy.IsNull() || m.AllowDestroy.IsUnknown() {
		m.AllowDestroy = types.BoolValue(false)
	}
}

func collectionPermission(value types.String) nullable.Nullable[client.Permission] {
	if value.IsNull() {
		return nullable.NewNullNullable[client.Permission]()
	}
	return nullable.NewNullableWithValue(client.Permission(value.ValueString()))
}

// Keep the raw create body until generated decoding finishes. A malformed
// setting can make the generated parser discard an otherwise usable identity.
func (a *apiClient) createCollection(ctx context.Context, body client.CollectionsCreateJSONRequestBody) (*client.CollectionsCreateResponse, uuid.UUID, error) {
	response, err := a.CollectionsCreate(ctx, body)
	if err != nil || response == nil {
		return nil, uuid.Nil, err
	}
	data, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil {
		return nil, uuid.Nil, err
	}
	id := uuid.Nil
	if response.StatusCode == http.StatusOK && strings.Contains(response.Header.Get("Content-Type"), "json") {
		// This is identity-only recovery, not a second API response model. All
		// actual settings and envelopes still use the generated parser.
		var envelope, collection map[string]json.RawMessage
		var value string
		if json.Unmarshal(data, &envelope) == nil && json.Unmarshal(envelope["data"], &collection) == nil && json.Unmarshal(collection["id"], &value) == nil {
			if parsed, parseErr := parseCollectionID(value); parseErr == nil {
				id = parsed
			}
		}
	}
	response.Body = io.NopCloser(bytes.NewReader(data))
	parsed, err := client.ParseCollectionsCreateResponse(response)
	if parsed == nil {
		parsed = &client.CollectionsCreateResponse{HTTPResponse: response, Body: data}
	}
	return parsed, id, err
}

func (a *apiClient) readCollection(ctx context.Context, id uuid.UUID) (*client.Collection, error) {
	if _, err := a.requireIAMAdmin(ctx, "outline_collection"); err != nil {
		return nil, err
	}
	response, err := a.CollectionsInfoWithResponse(ctx, client.CollectionsInfoJSONRequestBody{Id: id})
	if response == nil {
		return nil, a.checkResponse("collections.info", nil, nil, err)
	}
	if err = a.checkResponse("collections.info", response.HTTPResponse, response.Body, err); err != nil {
		// v1.10.1 uses rejectOnEmpty, unlike groups/users. Only a decoded Outline
		// 404 not_found is absence. A 403 can name another workspace's existing
		// collection. Even a complete admin workspace list cannot disprove that.
		if errors.Is(err, errNotFound) && (response.JSON404 == nil || response.JSON404.Error == nil || *response.JSON404.Error != "not_found" ||
			response.JSON404.Status == nil || *response.JSON404.Status != http.StatusNotFound || response.JSON404.Ok == nil || *response.JSON404.Ok) {
			return nil, errors.New("collections.info: unverified HTTP 404; refusing to treat an inaccessible or unexpected response as absence")
		}
		return nil, err
	}
	if response.JSON200 == nil {
		return nil, errors.New("collections.info: missing JSON response")
	}
	if err = checkEnvelope("collections.info", response.JSON200.Ok, response.JSON200.Status); err == nil {
		err = validateCollection(response.JSON200.Data, id)
	}
	if err != nil {
		return nil, err
	}
	return response.JSON200.Data, nil
}

func (a *apiClient) updateCollection(ctx context.Context, id uuid.UUID, model collectionModel) (*client.Collection, error) {
	name, sharing := model.Name.ValueString(), model.Sharing.ValueBool()
	response, err := a.CollectionsUpdateWithResponse(ctx, client.CollectionsUpdateJSONRequestBody{
		Id: id, Name: &name, Description: nullable.NewNullableWithValue(model.Description.ValueString()), Sharing: &sharing,
		// Never omit this field. Omission preserves access but can insert an
		// unintended caller-admin grant when the stored default is read_write.
		Permission: collectionPermission(model.Permission),
	})
	if response == nil {
		return nil, a.checkResponse("collections.update", nil, nil, err)
	}
	if err = a.checkResponse("collections.update", response.HTTPResponse, response.Body, err); err != nil {
		return nil, err
	}
	if response.JSON200 == nil {
		return nil, errors.New("collections.update: missing JSON response")
	}
	if err = checkEnvelope("collections.update", response.JSON200.Ok, response.JSON200.Status); err == nil {
		err = validateCollection(response.JSON200.Data, id)
	}
	if err == nil {
		err = managedCollection(response.JSON200.Data)
	}
	if err != nil {
		return nil, err
	}
	return response.JSON200.Data, nil
}

func (a *apiClient) walkCollections(ctx context.Context, visit func(*client.Collection) error) error {
	if _, err := a.requireIAMAdmin(ctx, "outline_collection lookup"); err != nil {
		return err
	}
	limit, offset, includeListOnly := 100, 0, true
	// Explicit [] includes active AND archived records in v1.10.1. Omission
	// excludes archives. Deleted records and other workspaces are never listed.
	statuses := []client.CollectionStatus{}
	total := -1
	seen := make(map[uuid.UUID]bool)
	for {
		//nolint:staticcheck // The pinned release supports statusFilter; [] includes archives.
		response, err := a.CollectionsListWithResponse(ctx, client.CollectionsListJSONRequestBody{
			Limit: &limit, Offset: &offset, IncludeListOnly: &includeListOnly, StatusFilter: &statuses,
		})
		if response == nil {
			return a.checkResponse("collections.list", nil, nil, err)
		}
		if err = a.checkResponse("collections.list", response.HTTPResponse, response.Body, err); err != nil {
			// A missing list route never proves a collection absent.
			return errors.New(err.Error())
		}
		if response.JSON200 == nil || response.JSON200.Data == nil {
			return errors.New("collections.list: missing collections data")
		}
		page := response.JSON200
		if err = checkEnvelope("collections.list", page.Ok, page.Status); err != nil {
			return err
		}
		for _, collection := range *page.Data {
			if err = validateCollection(&collection, uuid.Nil); err != nil {
				return err
			}
			if seen[*collection.Id] {
				return errors.New("collections.list: duplicate collection across pages; retry lookup when the list is stable")
			}
			seen[*collection.Id] = true
			if err = visit(&collection); err != nil {
				return err
			}
		}
		next, more, err := nextOffset(page.Pagination, offset, len(*page.Data))
		if err != nil {
			return fmt.Errorf("collections.list: %w", err)
		}
		if total != -1 && total != *page.Pagination.Total {
			return errors.New("collections.list: total changed during pagination; retry lookup when the list is stable")
		}
		total = *page.Pagination.Total
		if !more {
			return nil
		}
		offset = next
	}
}

func (a *apiClient) findCollection(ctx context.Context, name string) (*client.Collection, error) {
	var match *client.Collection
	err := a.walkCollections(ctx, func(collection *client.Collection) error {
		if *collection.Name == name {
			if match != nil {
				return errors.New("ambiguous collection name: multiple collections match exactly; use an explicit ID")
			}
			copy := *collection
			match = &copy
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if match == nil {
		return nil, errors.New("no collection found with the exact name in the admin workspace list, including archives but excluding trash")
	}
	collection, err := a.readCollection(ctx, *match.Id)
	if err != nil {
		return nil, err
	}
	if *collection.Name != name {
		return nil, errors.New("collection name changed during lookup; retry or use an explicit ID")
	}
	return collection, nil
}
