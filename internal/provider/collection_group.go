// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/glitchedmob/terraform-provider-outline/internal/client"
	"github.com/google/uuid"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

type collectionGroupModel struct {
	ID           types.String `tfsdk:"id"`
	CollectionID types.String `tfsdk:"collection_id"`
	GroupID      types.String `tfsdk:"group_id"`
	Permission   types.String `tfsdk:"permission"`
}

// The pair, not the grant row's UUID, is Terraform's stable identity.
func collectionGroupID(collection, group uuid.UUID) string {
	return collection.String() + "/" + group.String()
}

func parseCollectionGroupID(id string) (collection, group uuid.UUID, err error) {
	parts := strings.Split(id, "/")
	if len(parts) == 2 {
		collection, err = parseCollectionID(parts[0])
		if err == nil {
			group, err = parseGroupID(parts[1])
			if err == nil {
				return collection, group, nil
			}
		}
	}
	return uuid.Nil, uuid.Nil, errors.New("expected collection_id/group_id with exactly one slash and two canonical lowercase, nonzero UUIDs")
}

func (m collectionGroupModel) pair() (collection, group uuid.UUID, err error) {
	collection, err = parseCollectionID(m.CollectionID.ValueString())
	if err == nil {
		group, err = parseGroupID(m.GroupID.ValueString())
	}
	if err == nil && !m.ID.IsNull() && !m.ID.IsUnknown() && m.ID.ValueString() != collectionGroupID(collection, group) {
		err = errors.New("grant ID does not match collection_id/group_id")
	}
	return
}

// Grant management does not edit the collection default. In particular, the
// collection resource's rejection of an admin default does not apply here.
func activeCollectionGrantTarget(collection *client.Collection) error {
	if !collection.ArchivedAt.IsNull() {
		return errors.New("archived collections cannot receive managed grants; restore the collection outside Terraform before managing its grants")
	}
	return nil
}

// Release presenters always include status. Require it for this grant contract.
func checkCollectionGrantEnvelope(operation string, ok *bool, status *int) error {
	if status == nil {
		return fmt.Errorf("%s: missing response envelope status", operation)
	}
	return checkEnvelope(operation, ok, status)
}

func validateCollectionGroup(member *client.GroupMembership, collection, group uuid.UUID) error {
	if member == nil || member.Id == nil || member.GroupId == nil ||
		!member.CollectionId.IsSpecified() || member.CollectionId.IsNull() || member.CollectionId.GetOrEmpty() != collection ||
		!member.DocumentId.IsSpecified() || !member.DocumentId.IsNull() || !member.SourceId.IsSpecified() || !member.SourceId.IsNull() ||
		member.Permission == nil || !member.Permission.Valid() {
		return errors.New("malformed explicit collection group grant: missing or inconsistent identity, permission, or null documentId/sourceId")
	}
	if _, err := parseGroupID(*member.Id); err != nil {
		return errors.New("collection group grant returned an invalid grant UUID")
	}
	if _, err := parseGroupID(member.GroupId.String()); err != nil {
		return err
	}
	if group != uuid.Nil && *member.GroupId != group {
		return errors.New("collection group grant returned a different group ID")
	}
	return nil
}

// Never read the groups.list preview or effective access policies. Finding the
// pair early is not enough: all pages must validate before a match or absence.
func (a *apiClient) readCollectionGroupPages(ctx context.Context, collection, group uuid.UUID) (*client.GroupMembership, error) {
	limit, offset, total := 100, 0, -1
	seenGroups, seenGrants := make(map[uuid.UUID]bool), make(map[string]bool)
	var match *client.GroupMembership
	for {
		r, err := a.CollectionsGroupMembershipsWithResponse(ctx, client.CollectionsGroupMembershipsJSONRequestBody{Id: collection, Limit: &limit, Offset: &offset})
		if r == nil {
			return nil, a.checkResponse("collections.group_memberships", nil, nil, err)
		}
		if err = a.checkResponse("collections.group_memberships", r.HTTPResponse, r.Body, err); err != nil {
			// No grant endpoint error is a completed empty list or parent absence.
			return nil, errors.New(err.Error())
		}
		if r.JSON200 == nil || r.JSON200.Data == nil || r.JSON200.Data.GroupMemberships == nil || r.JSON200.Data.Groups == nil {
			return nil, errors.New("collections.group_memberships: missing JSON groupMemberships or groups data")
		}
		page := r.JSON200
		if err = checkCollectionGrantEnvelope("collections.group_memberships", page.Ok, page.Status); err != nil {
			return nil, err
		}
		members, groups := *page.Data.GroupMemberships, *page.Data.Groups
		if len(groups) != len(members) {
			return nil, errors.New("collections.group_memberships: groups and grants disagree")
		}
		pageGroups := make(map[uuid.UUID]bool, len(groups))
		for i := range groups {
			if err = validateGroup(&groups[i], uuid.Nil); err != nil {
				return nil, err
			}
			if _, err = parseGroupID(groups[i].Id.String()); err != nil {
				return nil, err
			}
			if pageGroups[*groups[i].Id] {
				return nil, errors.New("collections.group_memberships: duplicate group")
			}
			pageGroups[*groups[i].Id] = true
		}
		for i := range members {
			member := &members[i]
			if err = validateCollectionGroup(member, collection, uuid.Nil); err != nil {
				return nil, err
			}
			if seenGroups[*member.GroupId] || seenGrants[*member.Id] || !pageGroups[*member.GroupId] {
				return nil, errors.New("collections.group_memberships: duplicate grant or inconsistent group identity; retry when the list is stable")
			}
			seenGroups[*member.GroupId], seenGrants[*member.Id] = true, true
			if *member.GroupId == group {
				match = member
			}
		}
		next, more, err := nextOffset(page.Pagination, offset, len(members))
		if err != nil {
			return nil, fmt.Errorf("collections.group_memberships: %w", err)
		}
		if total != -1 && total != *page.Pagination.Total {
			return nil, errors.New("collections.group_memberships: total changed during pagination; retry when the list is stable")
		}
		total = *page.Pagination.Total
		if !more {
			return match, nil
		}
		offset = next
	}
}

func (a *apiClient) observeCollectionGroup(ctx context.Context, collection, group uuid.UUID) (*client.GroupMembership, error) {
	if _, err := a.requireIAMAdmin(ctx, "outline_collection_group"); err != nil {
		return nil, err
	}
	parent, err := a.readCollection(ctx, collection)
	if err == nil {
		err = activeCollectionGrantTarget(parent)
	}
	if err != nil {
		return nil, err
	}
	// Do not call managedGroup. The release authorizes group read for collection
	// grants; external synchronization restricts name/user membership edits only.
	if _, err = a.readGroup(ctx, group); err != nil {
		return nil, err
	}
	return a.readCollectionGroupPages(ctx, collection, group)
}

// v1.10.1 has no update_group endpoint: add_group is an upsert. Always supply
// permission, overriding its read_write default rather than silently escalating.
func (a *apiClient) writeCollectionGroup(ctx context.Context, collection, group uuid.UUID, permission client.Permission) (*client.GroupMembership, error) {
	if !permission.Valid() {
		return nil, errors.New("collection grant permission must be read, read_write, or admin")
	}
	r, err := a.CollectionsAddGroupWithResponse(ctx, client.CollectionsAddGroupJSONRequestBody{Id: collection, GroupId: group, Permission: &permission})
	if r == nil {
		return nil, a.checkResponse("collections.add_group", nil, nil, err)
	}
	if err = a.checkResponse("collections.add_group", r.HTTPResponse, r.Body, err); err != nil {
		// Grant mutation 404s do not establish parent absence either.
		return nil, errors.New(err.Error())
	}
	if r.JSON200 == nil || r.JSON200.Data == nil || r.JSON200.Data.GroupMemberships == nil || len(*r.JSON200.Data.GroupMemberships) != 1 {
		return nil, errors.New("collections.add_group: expected exactly one groupMemberships grant")
	}
	if err = checkCollectionGrantEnvelope("collections.add_group", r.JSON200.Ok, r.JSON200.Status); err != nil {
		return nil, err
	}
	member := &(*r.JSON200.Data.GroupMemberships)[0]
	if err = validateCollectionGroup(member, collection, group); err != nil {
		return nil, err
	}
	if *member.Permission != permission {
		return nil, errors.New("collections.add_group: response did not confirm desired permission")
	}
	return member, nil
}

func (a *apiClient) removeCollectionGroup(ctx context.Context, collection, group uuid.UUID) error {
	r, requestErr := a.CollectionsRemoveGroupWithResponse(ctx, client.CollectionsRemoveGroupJSONRequestBody{Id: collection, GroupId: group})
	if r == nil {
		return a.checkResponse("collections.remove_group", nil, nil, requestErr)
	}
	err := a.checkResponse("collections.remove_group", r.HTTPResponse, r.Body, requestErr)
	if err != nil {
		// The release's exact absent-pair response is a 400, after authorization.
		// Other 400s include last-manager protection. Never swallow them or 403s.
		absent := r.JSON400
		if requestErr != nil || r.StatusCode() != http.StatusBadRequest || absent == nil || absent.Ok == nil || *absent.Ok ||
			absent.Status == nil || *absent.Status != http.StatusBadRequest || absent.Error == nil || *absent.Error != "invalid_request" ||
			absent.Message == nil || *absent.Message != "This Group is not a part of the collection" {
			return errors.New(err.Error())
		}
	} else {
		if r.JSON200 == nil || r.JSON200.Success == nil || !*r.JSON200.Success {
			return errors.New("collections.remove_group: missing successful JSON response")
		}
		if err = checkCollectionGrantEnvelope("collections.remove_group", r.JSON200.Ok, r.JSON200.Status); err != nil {
			return err
		}
	}
	member, err := a.readCollectionGroupPages(ctx, collection, group)
	if err == nil && member != nil {
		err = errors.New("collections.remove_group: grant still exists after removal or reported absence")
	}
	return err
}
