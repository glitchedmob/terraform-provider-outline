// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/glitchedmob/terraform-provider-outline/internal/client"
	"github.com/google/uuid"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

type collectionUserModel struct {
	ID           types.String `tfsdk:"id"`
	CollectionID types.String `tfsdk:"collection_id"`
	UserID       types.String `tfsdk:"user_id"`
	Permission   types.String `tfsdk:"permission"`
}

// Terraform identity is the pair, not the UserMembership row's UUID.
func collectionUserID(collection, user uuid.UUID) string {
	return collection.String() + "/" + user.String()
}

func parseCollectionUserID(id string) (collection, user uuid.UUID, err error) {
	parts := strings.Split(id, "/")
	if len(parts) == 2 {
		collection, err = parseCollectionID(parts[0])
		if err == nil {
			user, err = parseUserID(parts[1])
			if err == nil {
				return collection, user, nil
			}
		}
	}
	return uuid.Nil, uuid.Nil, errors.New("expected collection_id/user_id with exactly one slash and two canonical lowercase, nonzero UUIDs")
}

func (m collectionUserModel) pair() (collection, user uuid.UUID, err error) {
	collection, err = parseCollectionID(m.CollectionID.ValueString())
	if err == nil {
		user, err = parseUserID(m.UserID.ValueString())
	}
	if err == nil && !m.ID.IsNull() && !m.ID.IsUnknown() && m.ID.ValueString() != collectionUserID(collection, user) {
		err = errors.New("grant ID does not match collection_id/user_id")
	}
	return
}

// Both grant endpoints use presentUser without includeDetails. Do not require
// email here or weaken validateUser's stricter parent/account-read contract.
// Suspended, guest, viewer, and pending targets are valid in v1.10.1: the
// handler authorizes same-team user read, not activation or a role change.
func validateCollectionGrantUser(user *client.User, id uuid.UUID) error {
	if user == nil || user.Id == nil || user.Name == nil || user.Role == nil || !user.Role.Valid() || user.IsSuspended == nil {
		return errors.New("malformed collection grant user: missing ID, name, valid role, or isSuspended")
	}
	if _, err := parseUserID(user.Id.String()); err != nil {
		return err
	}
	if id != uuid.Nil && *user.Id != id {
		return errors.New("collection grant user returned a different ID")
	}
	return nil
}

func validateCollectionUser(member *client.Membership, collection, user uuid.UUID) error {
	if member == nil || member.Id == nil || member.UserId == nil ||
		!member.CollectionId.IsSpecified() || member.CollectionId.IsNull() || member.CollectionId.GetOrEmpty() != collection ||
		!member.DocumentId.IsSpecified() || !member.DocumentId.IsNull() || !member.SourceId.IsSpecified() || !member.SourceId.IsNull() ||
		member.Permission == nil || !member.Permission.Valid() {
		return errors.New("malformed explicit collection user grant: missing or inconsistent identity, permission, or null documentId/sourceId")
	}
	if _, err := parseUserID(*member.Id); err != nil {
		return errors.New("collection user grant returned an invalid grant UUID")
	}
	if _, err := parseUserID(member.UserId.String()); err != nil {
		return err
	}
	if user != uuid.Nil && *member.UserId != user {
		return errors.New("collection user grant returned a different user ID")
	}
	return nil
}

// membershipRefreshQuery keeps the freshly read name unchanged. Outline's
// QueryHelper escapes SQL wildcard characters; the client must not escape them.
// Invalid UTF-8, NUL, and blank names cannot provide a usable text query.
func membershipRefreshQuery(name *string) *string {
	if name == nil || !utf8.ValidString(*name) || strings.ContainsRune(*name, '\x00') || strings.TrimSpace(*name) == "" {
		return nil
	}
	return name
}

// Only the full explicit collection-user list can prove pair absence. Group
// grants, default permissions, and effective policies are different contracts.
// Validate every page even after finding the target or a creator-admin grant.
func (a *apiClient) readCollectionUserPages(ctx context.Context, collection, user uuid.UUID) (*client.Membership, error) {
	return a.readCollectionUserQueryPages(ctx, collection, user, nil)
}

func (a *apiClient) readCollectionUserQueryPages(ctx context.Context, collection, user uuid.UUID, query *string) (*client.Membership, error) {
	limit, offset, total := 100, 0, -1
	seenUsers, seenGrants := make(map[uuid.UUID]bool), make(map[string]bool)
	var match *client.Membership
	for {
		r, err := a.CollectionsMembershipsWithResponse(ctx, client.CollectionsMembershipsJSONRequestBody{Id: collection, Limit: &limit, Offset: &offset, Query: query})
		if r == nil {
			return nil, a.checkResponse("collections.memberships", nil, nil, err)
		}
		if err = a.checkResponse("collections.memberships", r.HTTPResponse, r.Body, err); err != nil {
			// A grant-endpoint 400/403/404 is never verified parent absence.
			return nil, err
		}
		if r.JSON200 == nil || r.JSON200.Data == nil || r.JSON200.Data.Memberships == nil || r.JSON200.Data.Users == nil {
			return nil, errors.New("collections.memberships: missing JSON memberships or users data")
		}
		page := r.JSON200
		if err = checkCollectionGrantEnvelope("collections.memberships", page.Ok, page.Status); err != nil {
			return nil, err
		}
		members, users := *page.Data.Memberships, *page.Data.Users
		if len(users) != len(members) {
			return nil, errors.New("collections.memberships: users and grants disagree")
		}
		pageUsers := make(map[uuid.UUID]bool, len(users))
		for i := range users {
			if err = validateCollectionGrantUser(&users[i], uuid.Nil); err != nil {
				return nil, err
			}
			if pageUsers[*users[i].Id] {
				return nil, errors.New("collections.memberships: duplicate user")
			}
			pageUsers[*users[i].Id] = true
		}
		for i := range members {
			member := &members[i]
			if err = validateCollectionUser(member, collection, uuid.Nil); err != nil {
				return nil, err
			}
			if seenUsers[*member.UserId] || seenGrants[*member.Id] || !pageUsers[*member.UserId] {
				return nil, errors.New("collections.memberships: duplicate grant or inconsistent user identity; retry when the list is stable")
			}
			seenUsers[*member.UserId], seenGrants[*member.Id] = true, true
			if *member.UserId == user {
				match = member
			}
		}
		next, more, err := nextOffset(page.Pagination, offset, len(members))
		if err != nil {
			return nil, fmt.Errorf("collections.memberships: %w", err)
		}
		if total != -1 && total != *page.Pagination.Total {
			return nil, errors.New("collections.memberships: total changed during pagination; retry when the list is stable")
		}
		total = *page.Pagination.Total
		if !more {
			return match, nil
		}
		offset = next
	}
}

func (a *apiClient) observeCollectionUser(ctx context.Context, collection, user uuid.UUID) (*client.Membership, error) {
	return a.observeCollectionUserWithRefresh(ctx, collection, user, false)
}

// Refresh accepts an exact UUID match after validating the entire name-filtered
// result. Unlike mutation/import observations, it need not audit unrelated rows.
func (a *apiClient) refreshCollectionUser(ctx context.Context, collection, user uuid.UUID) (*client.Membership, error) {
	return a.observeCollectionUserWithRefresh(ctx, collection, user, true)
}

func (a *apiClient) observeCollectionUserWithRefresh(ctx context.Context, collection, user uuid.UUID, refresh bool) (*client.Membership, error) {
	actor, err := a.requireIAMAdmin(ctx, "outline_collection_user")
	if err != nil {
		return nil, err
	}
	// Refuse read/import as well as mutations. Importing an owner's creator
	// grant would otherwise authorize a later demotion or delete via state.
	if *actor.Id == user {
		return nil, errors.New("refusing to manage the API-key owner's own direct collection grant; use a different admin key to manage this user's grants")
	}
	parent, err := a.readCollection(ctx, collection)
	if err == nil {
		err = activeCollectionGrantTarget(parent)
	}
	if err != nil {
		return nil, err
	}
	target, err := a.readUser(ctx, user)
	if err != nil {
		return nil, err
	}
	if refresh {
		if query := membershipRefreshQuery(target.Name); query != nil {
			member, err := a.readCollectionUserQueryPages(ctx, collection, user, query)
			if err != nil || member != nil {
				return member, err
			}
			// A valid filtered miss cannot prove absence after a concurrent rename.
		}
	}
	return a.readCollectionUserPages(ctx, collection, user)
}

// v1.10.1 add_user is an upsert, not an atomic create-only operation. Always
// supply permission: omission falls back to the target's role-based default.
func (a *apiClient) writeCollectionUser(ctx context.Context, collection, user uuid.UUID, permission client.Permission) (*client.Membership, error) {
	if !permission.Valid() {
		return nil, errors.New("collection grant permission must be read, read_write, or admin")
	}
	r, err := a.CollectionsAddUserWithResponse(ctx, client.CollectionsAddUserJSONRequestBody{Id: collection, UserId: user, Permission: &permission})
	if r == nil {
		return nil, a.checkResponse("collections.add_user", nil, nil, err)
	}
	if err = a.checkResponse("collections.add_user", r.HTTPResponse, r.Body, err); err != nil {
		return nil, err
	}
	if r.JSON200 == nil || r.JSON200.Data == nil || r.JSON200.Data.Memberships == nil || len(*r.JSON200.Data.Memberships) != 1 ||
		r.JSON200.Data.Users == nil || len(*r.JSON200.Data.Users) != 1 {
		return nil, errors.New("collections.add_user: expected exactly one user and memberships grant")
	}
	if err = checkCollectionGrantEnvelope("collections.add_user", r.JSON200.Ok, r.JSON200.Status); err != nil {
		return nil, err
	}
	if err = validateCollectionGrantUser(&(*r.JSON200.Data.Users)[0], user); err != nil {
		return nil, err
	}
	member := &(*r.JSON200.Data.Memberships)[0]
	if err = validateCollectionUser(member, collection, user); err != nil {
		return nil, err
	}
	if *member.Permission != permission {
		return nil, errors.New("collections.add_user: response did not confirm desired permission")
	}
	return member, nil
}

func (a *apiClient) removeCollectionUser(ctx context.Context, collection, user uuid.UUID) error {
	r, requestErr := a.CollectionsRemoveUserWithResponse(ctx, client.CollectionsRemoveUserJSONRequestBody{Id: collection, UserId: user})
	if r == nil {
		return a.checkResponse("collections.remove_user", nil, nil, requestErr)
	}
	err := a.checkResponse("collections.remove_user", r.HTTPResponse, r.Body, requestErr)
	if err != nil {
		// Only the exact released absent-pair response is an absence candidate.
		// Last-manager 400s, other 400s, and every 403/404 retain diagnostics.
		absent := r.JSON400
		if requestErr != nil || r.StatusCode() != http.StatusBadRequest || absent == nil || absent.Ok == nil || *absent.Ok ||
			absent.Status == nil || *absent.Status != http.StatusBadRequest || absent.Error == nil || *absent.Error != "invalid_request" ||
			absent.Message == nil || *absent.Message != "User is not a collection member" {
			return err
		}
	} else {
		if r.JSON200 == nil || r.JSON200.Success == nil || !*r.JSON200.Success {
			return errors.New("collections.remove_user: missing successful JSON response")
		}
		if err = checkCollectionGrantEnvelope("collections.remove_user", r.JSON200.Ok, r.JSON200.Status); err != nil {
			return err
		}
	}
	member, err := a.readCollectionUserPages(ctx, collection, user)
	if err == nil && member != nil {
		err = errors.New("collections.remove_user: grant still exists after removal or reported absence")
	}
	return err
}
