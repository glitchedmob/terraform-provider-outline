// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"

	"github.com/glitchedmob/terraform-provider-outline/internal/client"
	"github.com/google/uuid"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

type groupModel struct {
	ID              types.String `tfsdk:"id"`
	Name            types.String `tfsdk:"name"`
	Description     types.String `tfsdk:"description"`
	DisableMentions types.Bool   `tfsdk:"disable_mentions"`
}

func groupNameValidators() []validator.String {
	return []validator.String{
		outlineStringLength{min: 1, max: 255},
		stringvalidator.RegexMatches(regexp.MustCompile(`\S`), "must contain a non-whitespace character"),
	}
}

func groupIDValidators() []validator.String {
	return []validator.String{stringvalidator.RegexMatches(
		regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`),
		"must be a canonical, nonzero UUID",
	)}
}

func parseGroupID(id string) (uuid.UUID, error) {
	parsed, err := uuid.Parse(id)
	if err != nil || parsed == uuid.Nil || parsed.String() != id || parsed.Variant() != uuid.RFC4122 || parsed.Version() < 1 || parsed.Version() > 8 {
		return uuid.Nil, errors.New("group ID must be a canonical lowercase, nonzero UUID")
	}
	return parsed, nil
}

func validateGroup(group *client.Group, expectedID uuid.UUID) error {
	if group == nil || group.Id == nil || *group.Id == uuid.Nil || group.Name == nil || group.DisableMentions == nil {
		return errors.New("malformed group response: missing ID, name, or disableMentions")
	}
	if expectedID != uuid.Nil && *group.Id != expectedID {
		return errors.New("group response returned a different ID")
	}
	return nil
}

func managedGroup(group *client.Group) error {
	if (group.ExternalId.IsSpecified() && !group.ExternalId.IsNull() && group.ExternalId.GetOrEmpty() != "") ||
		(group.ExternalGroup.IsSpecified() && !group.ExternalGroup.IsNull()) {
		return errors.New("externally linked or synchronized groups cannot be managed by this provider; externalId cannot be cleared safely. Use the lookup data source instead")
	}
	return nil
}

func (m *groupModel) setGroup(group *client.Group) {
	m.ID = types.StringValue(group.Id.String())
	m.Name = types.StringValue(*group.Name)
	m.Description = types.StringValue(group.Description.GetOrEmpty())
	m.DisableMentions = types.BoolValue(*group.DisableMentions)
}

func (a *apiClient) readGroup(ctx context.Context, id uuid.UUID) (*client.Group, error) {
	response, err := a.GroupsInfoWithResponse(ctx, client.GroupsInfoJSONRequestBody{Id: id})
	if response == nil {
		return nil, a.checkResponse("groups.info", nil, nil, err)
	}
	requestErr := err
	if err = a.checkResponse("groups.info", response.HTTPResponse, response.Body, err); err != nil {
		// v1.10.1 authorizes a nil Group and returns authorization_error for a
		// deleted ID. Neither a 403 nor an unexpected route/proxy 404 proves
		// absence. Confirm admin identity and the complete workspace list.
		forbidden := response.StatusCode() == http.StatusForbidden && response.JSON403 != nil && response.JSON403.Error != nil && *response.JSON403.Error == "authorization_error"
		if requestErr == nil && (response.StatusCode() == http.StatusNotFound || forbidden) {
			absent, verifyErr := a.confirmGroupAbsent(ctx, id)
			if verifyErr != nil {
				return nil, fmt.Errorf("%w; cannot establish group absence: %v", err, verifyErr)
			}
			if absent {
				return nil, fmt.Errorf("groups.info: %w in the admin workspace group list", errNotFound)
			}
		}
		return nil, err
	}
	if response.JSON200 == nil {
		return nil, errors.New("groups.info: missing JSON response")
	}
	if err = checkEnvelope("groups.info", response.JSON200.Ok, response.JSON200.Status); err != nil {
		return nil, err
	}
	if err = validateGroup(response.JSON200.Data, id); err != nil {
		return nil, err
	}
	return response.JSON200.Data, nil
}

func (a *apiClient) updateGroup(ctx context.Context, id uuid.UUID, model groupModel) (*client.Group, error) {
	name, description, mentions := model.Name.ValueString(), model.Description.ValueString(), model.DisableMentions.ValueBool()
	response, err := a.GroupsUpdateWithResponse(ctx, client.GroupsUpdateJSONRequestBody{
		Id: id, Name: &name, Description: &description, DisableMentions: &mentions,
	})
	if response == nil {
		return nil, a.checkResponse("groups.update", nil, nil, err)
	}
	if err = a.checkResponse("groups.update", response.HTTPResponse, response.Body, err); err != nil {
		return nil, err
	}
	if response.JSON200 == nil {
		return nil, errors.New("groups.update: missing JSON response")
	}
	if err = checkEnvelope("groups.update", response.JSON200.Ok, response.JSON200.Status); err != nil {
		return nil, err
	}
	if err = validateGroup(response.JSON200.Data, id); err != nil {
		return nil, err
	}
	if err = managedGroup(response.JSON200.Data); err != nil {
		return nil, err
	}
	return response.JSON200.Data, nil
}

func (a *apiClient) walkGroups(ctx context.Context, visit func(*client.Group) error) error {
	limit, offset := 100, 0
	total := -1
	seen := make(map[uuid.UUID]bool)
	for {
		response, err := a.GroupsListWithResponse(ctx, client.GroupsListJSONRequestBody{Limit: &limit, Offset: &offset})
		if response == nil {
			return a.checkResponse("groups.list", nil, nil, err)
		}
		if err = a.checkResponse("groups.list", response.HTTPResponse, response.Body, err); err != nil {
			return err
		}
		if response.JSON200 == nil {
			return errors.New("groups.list: missing JSON response")
		}
		page := response.JSON200
		if err = checkEnvelope("groups.list", page.Ok, page.Status); err != nil {
			return err
		}
		if page.Data == nil || page.Data.Groups == nil {
			return errors.New("groups.list: missing groups data")
		}
		for _, group := range *page.Data.Groups {
			if err = validateGroup(&group, uuid.Nil); err != nil {
				return err
			}
			if seen[*group.Id] {
				return errors.New("groups.list: duplicate group across pages; retry lookup when the list is stable")
			}
			seen[*group.Id] = true
			if err = visit(&group); err != nil {
				return err
			}
		}
		next, more, err := nextOffset(page.Pagination, offset, len(*page.Data.Groups))
		if err != nil {
			return fmt.Errorf("groups.list: %w", err)
		}
		if total != -1 && total != *page.Pagination.Total {
			return errors.New("groups.list: total changed during pagination; retry lookup when the list is stable")
		}
		total = *page.Pagination.Total
		if !more {
			return nil
		}
		offset = next
	}
}

func (a *apiClient) findGroup(ctx context.Context, name string) (*client.Group, error) {
	var match *client.Group
	err := a.walkGroups(ctx, func(group *client.Group) error {
		if *group.Name == name {
			if match != nil {
				return errors.New("ambiguous group name: multiple groups match exactly; use an explicit ID")
			}
			copy := *group
			match = &copy
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if match == nil {
		return nil, errors.New("no group found with the exact name")
	}
	group, err := a.readGroup(ctx, *match.Id)
	if err != nil {
		return nil, err
	}
	if *group.Name != name {
		return nil, errors.New("group name changed during lookup; retry or use an explicit ID")
	}
	return group, nil
}

func (a *apiClient) confirmGroupAbsent(ctx context.Context, id uuid.UUID) (bool, error) {
	response, err := a.AuthInfoWithResponse(ctx)
	if response == nil {
		return false, a.checkResponse("auth.info", nil, nil, err)
	}
	if err = a.checkResponse("auth.info", response.HTTPResponse, response.Body, err); err != nil {
		return false, err
	}
	if response.JSON200 == nil {
		return false, errors.New("auth.info: missing JSON response")
	}
	if err = checkEnvelope("auth.info", response.JSON200.Ok, response.JSON200.Status); err != nil {
		return false, err
	}
	auth := response.JSON200.Data
	if auth == nil || auth.User == nil || auth.User.Role == nil || *auth.User.Role != client.UserRoleAdmin {
		return false, errors.New("only an admin can establish group absence")
	}
	found := false
	err = a.walkGroups(ctx, func(group *client.Group) error {
		if *group.Id == id {
			found = true
		}
		return nil
	})
	return !found && err == nil, err
}
