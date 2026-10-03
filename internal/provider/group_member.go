// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/glitchedmob/terraform-provider-outline/internal/client"
	"github.com/google/uuid"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

type groupMemberModel struct {
	ID         types.String `tfsdk:"id"`
	GroupID    types.String `tfsdk:"group_id"`
	UserID     types.String `tfsdk:"user_id"`
	Permission types.String `tfsdk:"permission"`
}

// Terraform uses group/user, not the presenter's user-group string.
func groupMemberID(group, user uuid.UUID) string { return group.String() + "/" + user.String() }

func parseGroupMemberID(id string) (group, user uuid.UUID, err error) {
	parts := strings.Split(id, "/")
	if len(parts) == 2 {
		group, err = parseGroupID(parts[0])
		if err == nil {
			user, err = parseUserID(parts[1])
			if err == nil {
				return group, user, nil
			}
		}
	}
	return uuid.Nil, uuid.Nil, errors.New("expected group_id/user_id with exactly one slash and two canonical lowercase, nonzero UUIDs")
}

func (m groupMemberModel) pair() (group, user uuid.UUID, err error) {
	group, err = parseGroupID(m.GroupID.ValueString())
	if err == nil {
		user, err = parseUserID(m.UserID.ValueString())
	}
	if err == nil && !m.ID.IsNull() && !m.ID.IsUnknown() && m.ID.ValueString() != groupMemberID(group, user) {
		err = errors.New("membership ID does not match group_id/user_id")
	}
	return
}

// Group presenters omit email, even for admins. Account reads still use the
// stricter validateUser through readUser; do not weaken that absence contract.
func validateGroupMemberUser(user *client.User, id uuid.UUID) error {
	if user == nil || user.Id == nil || *user.Id == uuid.Nil || user.Name == nil || user.Role == nil || !user.Role.Valid() || user.IsSuspended == nil {
		return errors.New("malformed group membership user: missing ID, name, valid role, or isSuspended")
	}
	if _, err := parseUserID(user.Id.String()); err != nil {
		return err
	}
	if id != uuid.Nil && *user.Id != id {
		return errors.New("group membership user returned a different ID")
	}
	return nil
}

func validateGroupMember(member *client.GroupUser, group, user uuid.UUID) error {
	if member == nil || member.GroupId == nil || *member.GroupId != group || member.UserId == nil ||
		member.Id == nil || member.Permission == nil || !member.Permission.Valid() {
		return errors.New("malformed group membership: missing or inconsistent identity or permission")
	}
	if _, err := parseUserID(member.UserId.String()); err != nil {
		return err
	}
	if user != uuid.Nil && *member.UserId != user {
		return errors.New("group membership returned a different user ID")
	}
	if *member.Id != member.UserId.String()+"-"+group.String() {
		return errors.New("group membership returned an inconsistent composite API ID")
	}
	return validateGroupMemberUser(member.User, *member.UserId)
}

// Never use the groups.list avatar preview or stop early after finding a pair.
// A match is usable only after the complete, validated page set is stable.
func (a *apiClient) readGroupMemberPages(ctx context.Context, group, user uuid.UUID) (*client.GroupUser, error) {
	return a.readGroupMemberQueryPages(ctx, group, user, nil)
}

func (a *apiClient) readGroupMemberQueryPages(ctx context.Context, group, user uuid.UUID, query *string) (*client.GroupUser, error) {
	limit, offset, total := 100, 0, -1
	seen := make(map[uuid.UUID]bool)
	var match *client.GroupUser
	for {
		r, err := a.GroupsMembershipsWithResponse(ctx, client.GroupsMembershipsJSONRequestBody{Id: group, Limit: &limit, Offset: &offset, Query: query})
		if r == nil {
			return nil, a.checkResponse("groups.memberships", nil, nil, err)
		}
		if err = a.checkResponse("groups.memberships", r.HTTPResponse, r.Body, err); err != nil {
			// An endpoint error, including 403 or 404, is not a completed empty list.
			return nil, errors.New(err.Error())
		}
		if r.JSON200 == nil || r.JSON200.Data == nil || r.JSON200.Data.GroupMemberships == nil || r.JSON200.Data.Users == nil {
			return nil, errors.New("groups.memberships: missing JSON membership or users data")
		}
		page := r.JSON200
		if err = checkEnvelope("groups.memberships", page.Ok, page.Status); err != nil {
			return nil, err
		}
		members, users := *page.Data.GroupMemberships, *page.Data.Users
		if len(users) != len(members) {
			return nil, errors.New("groups.memberships: users and memberships disagree")
		}
		pageUsers := make(map[uuid.UUID]bool, len(users))
		for i := range users {
			if err = validateGroupMemberUser(&users[i], uuid.Nil); err != nil {
				return nil, err
			}
			if pageUsers[*users[i].Id] {
				return nil, errors.New("groups.memberships: duplicate user")
			}
			pageUsers[*users[i].Id] = true
		}
		for i := range members {
			member := &members[i]
			if err = validateGroupMember(member, group, uuid.Nil); err != nil {
				return nil, err
			}
			if seen[*member.UserId] || !pageUsers[*member.UserId] {
				return nil, errors.New("groups.memberships: duplicate membership or inconsistent user identity; retry when the list is stable")
			}
			seen[*member.UserId] = true
			if *member.UserId == user {
				match = member
			}
		}
		next, more, err := nextOffset(page.Pagination, offset, len(members))
		if err != nil {
			return nil, fmt.Errorf("groups.memberships: %w", err)
		}
		if total != -1 && total != *page.Pagination.Total {
			return nil, errors.New("groups.memberships: total changed during pagination; retry when the list is stable")
		}
		total = *page.Pagination.Total
		if !more {
			return match, nil
		}
		offset = next
	}
}

// Parent absence is established only by the existing release-verified helpers.
// Membership endpoint errors never trigger their own 403/404 absence fallback.
func (a *apiClient) observeGroupMember(ctx context.Context, group, user uuid.UUID) (*client.GroupUser, error) {
	return a.observeGroupMemberWithRefresh(ctx, group, user, false)
}

// Refresh audits the complete name-filtered result for positive observations;
// mutation preflights and removal verification still use the unfiltered list.
func (a *apiClient) refreshGroupMember(ctx context.Context, group, user uuid.UUID) (*client.GroupUser, error) {
	return a.observeGroupMemberWithRefresh(ctx, group, user, true)
}

func (a *apiClient) observeGroupMemberWithRefresh(ctx context.Context, group, user uuid.UUID, refresh bool) (*client.GroupUser, error) {
	if _, err := a.requireIAMAdmin(ctx, "outline_group_member"); err != nil {
		return nil, err
	}
	parent, err := a.readGroup(ctx, group)
	if err == nil {
		err = managedGroup(parent)
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
			member, err := a.readGroupMemberQueryPages(ctx, group, user, query)
			if err != nil || member != nil {
				return member, err
			}
			// A valid filtered miss cannot prove absence after a concurrent rename.
		}
	}
	return a.readGroupMemberPages(ctx, group, user)
}

func validateGroupMemberMutation(groups *[]client.Group, users *[]client.User, members *[]client.GroupUser, group, user uuid.UUID, permission client.GroupPermission) (*client.GroupUser, error) {
	if err := validateGroupMemberGroups(groups, group); err != nil {
		return nil, err
	}
	if users == nil || len(*users) != 1 || members == nil || len(*members) != 1 {
		return nil, errors.New("membership mutation: expected exactly one user and membership")
	}
	if err := validateGroupMemberUser(&(*users)[0], user); err != nil {
		return nil, err
	}
	member := &(*members)[0]
	if err := validateGroupMember(member, group, user); err != nil {
		return nil, err
	}
	if *member.Permission != permission {
		return nil, errors.New("membership mutation: response did not confirm desired permission")
	}
	return member, nil
}

func validateGroupMemberGroups(groups *[]client.Group, group uuid.UUID) error {
	if groups == nil || len(*groups) != 1 {
		return errors.New("membership mutation: expected exactly one group")
	}
	if err := validateGroup(&(*groups)[0], group); err != nil {
		return err
	}
	return managedGroup(&(*groups)[0])
}

func (a *apiClient) writeGroupMember(ctx context.Context, group, user uuid.UUID, permission client.GroupPermission, add bool) (*client.GroupUser, error) {
	if !permission.Valid() {
		return nil, errors.New("group permission must be member or admin")
	}
	if add {
		r, err := a.GroupsAddUserWithResponse(ctx, client.GroupsAddUserJSONRequestBody{Id: group, UserId: user, Permission: &permission})
		if r == nil {
			return nil, a.checkResponse("groups.add_user", nil, nil, err)
		}
		if err = a.checkResponse("groups.add_user", r.HTTPResponse, r.Body, err); err != nil {
			return nil, err
		}
		if r.JSON200 == nil || r.JSON200.Data == nil {
			return nil, errors.New("groups.add_user: missing JSON data")
		}
		if err = checkEnvelope("groups.add_user", r.JSON200.Ok, r.JSON200.Status); err != nil {
			return nil, err
		}
		return validateGroupMemberMutation(r.JSON200.Data.Groups, r.JSON200.Data.Users, r.JSON200.Data.GroupMemberships, group, user, permission)
	}
	r, err := a.GroupsUpdateUserWithResponse(ctx, client.GroupsUpdateUserJSONRequestBody{Id: group, UserId: user, Permission: permission})
	if r == nil {
		return nil, a.checkResponse("groups.update_user", nil, nil, err)
	}
	if err = a.checkResponse("groups.update_user", r.HTTPResponse, r.Body, err); err != nil {
		return nil, err
	}
	if r.JSON200 == nil || r.JSON200.Data == nil {
		return nil, errors.New("groups.update_user: missing JSON data")
	}
	if err = checkEnvelope("groups.update_user", r.JSON200.Ok, r.JSON200.Status); err != nil {
		return nil, err
	}
	return validateGroupMemberMutation(r.JSON200.Data.Groups, r.JSON200.Data.Users, r.JSON200.Data.GroupMemberships, group, user, permission)
}

func (a *apiClient) removeGroupMember(ctx context.Context, group, user uuid.UUID) error {
	r, err := a.GroupsRemoveUserWithResponse(ctx, client.GroupsRemoveUserJSONRequestBody{Id: group, UserId: user})
	if r == nil {
		return a.checkResponse("groups.remove_user", nil, nil, err)
	}
	if err = a.checkResponse("groups.remove_user", r.HTTPResponse, r.Body, err); err != nil {
		return err
	}
	if r.JSON200 == nil || r.JSON200.Data == nil {
		return errors.New("groups.remove_user: missing JSON data")
	}
	if err = checkEnvelope("groups.remove_user", r.JSON200.Ok, r.JSON200.Status); err != nil {
		return err
	}
	if err = validateGroupMemberGroups(r.JSON200.Data.Groups, group); err != nil {
		return err
	}
	member, err := a.readGroupMemberPages(ctx, group, user)
	if err == nil && member != nil {
		err = errors.New("groups.remove_user: membership still exists after removal")
	}
	return err
}
