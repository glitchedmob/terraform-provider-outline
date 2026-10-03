// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/glitchedmob/terraform-provider-outline/internal/client"
	"github.com/google/uuid"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

type userModel struct {
	ID                types.String `tfsdk:"id"`
	Email             types.String `tfsdk:"email"`
	Name              types.String `tfsdk:"name"`
	Role              types.String `tfsdk:"role"`
	Suspended         types.Bool   `tfsdk:"suspended"`
	SuppressEmail     types.Bool   `tfsdk:"suppress_email"`
	DeletePermanently types.Bool   `tfsdk:"delete_permanently"`
}

type userLookupModel struct {
	ID        types.String `tfsdk:"id"`
	Email     types.String `tfsdk:"email"`
	Name      types.String `tfsdk:"name"`
	Role      types.String `tfsdk:"role"`
	Suspended types.Bool   `tfsdk:"suspended"`
}

func normalizeUserEmail(email string) string { return strings.ToLower(strings.TrimSpace(email)) }

func userEmailValidators() []validator.String {
	return []validator.String{outlineStringLength{min: 3, max: 254}, stringvalidator.RegexMatches(
		regexp.MustCompile(`^[^\s@]+@[^\s@]+\.[^\s@]+$`), "must be an email address without whitespace")}
}

func parseUserID(id string) (uuid.UUID, error) {
	parsed, err := parseGroupID(id)
	if err != nil {
		return uuid.Nil, errors.New("user ID must be a canonical lowercase, nonzero UUID")
	}
	return parsed, nil
}

func validateUser(user *client.User, id uuid.UUID, email string) error {
	if user == nil || user.Id == nil || *user.Id == uuid.Nil || user.Name == nil || user.Role == nil || !user.Role.Valid() || user.IsSuspended == nil ||
		!user.Email.IsSpecified() || user.Email.IsNull() || normalizeUserEmail(string(user.Email.GetOrEmpty())) == "" {
		return errors.New("malformed user response: missing ID, name, email, valid role, or isSuspended")
	}
	if id != uuid.Nil && *user.Id != id {
		return errors.New("user response returned a different ID")
	}
	if email != "" && normalizeUserEmail(string(user.Email.GetOrEmpty())) != normalizeUserEmail(email) {
		return errors.New("user response returned a different email; refusing to modify a different account")
	}
	return nil
}

func (m *userModel) setUser(user *client.User) {
	m.ID = types.StringValue(user.Id.String())
	// Preserve equivalent configured casing so normalization does not cause a
	// Terraform inconsistent-result error. Import and actual drift use API email.
	if m.Email.IsNull() || m.Email.IsUnknown() || normalizeUserEmail(m.Email.ValueString()) != normalizeUserEmail(string(user.Email.GetOrEmpty())) {
		m.Email = types.StringValue(string(user.Email.GetOrEmpty()))
	}
	m.Name = types.StringValue(*user.Name)
	m.Role = types.StringValue(string(*user.Role))
	m.Suspended = types.BoolValue(*user.IsSuspended)
}

func (m *userLookupModel) setUser(user *client.User) {
	u := userModel{Email: m.Email}
	u.setUser(user)
	m.ID, m.Email, m.Name, m.Role, m.Suspended = u.ID, u.Email, u.Name, u.Role, u.Suspended
}

func (a *apiClient) requireUserAdmin(ctx context.Context) (*client.User, error) {
	response, err := a.AuthInfoWithResponse(ctx)
	if response == nil {
		return nil, a.checkResponse("auth.info", nil, nil, err)
	}
	if err = a.checkResponse("auth.info", response.HTTPResponse, response.Body, err); err != nil {
		// Missing authentication endpoints do not prove a target user is absent.
		return nil, errors.New(err.Error())
	}
	if response.JSON200 == nil || response.JSON200.Data == nil {
		return nil, errors.New("auth.info: missing JSON user response")
	}
	if err = checkEnvelope("auth.info", response.JSON200.Ok, response.JSON200.Status); err != nil {
		return nil, err
	}
	user := response.JSON200.Data.User
	if err = validateUser(user, uuid.Nil, ""); err != nil {
		return nil, fmt.Errorf("auth.info: %w", err)
	}
	if *user.Role != client.UserRoleAdmin || *user.IsSuspended {
		return nil, errors.New("outline_user requires an active admin-owned API key")
	}
	return user, nil
}

func protectUserOwner(actor *client.User, id uuid.UUID, email string) error {
	if (id != uuid.Nil && id == *actor.Id) || (email != "" && normalizeUserEmail(email) == normalizeUserEmail(string(actor.Email.GetOrEmpty()))) {
		return errors.New("refusing to manage the API-key owner's account; use a different admin key and import only other users")
	}
	return nil
}

func (a *apiClient) readUser(ctx context.Context, id uuid.UUID) (*client.User, error) {
	response, err := a.UsersInfoWithResponse(ctx, client.UsersInfoJSONRequestBody{Id: id})
	if response == nil {
		return nil, a.checkResponse("users.info", nil, nil, err)
	}
	requestErr := err
	if err = a.checkResponse("users.info", response.HTTPResponse, response.Body, err); err != nil {
		// Verified independently for users in v1.10.1: a nil User is authorized
		// before presentation, returning 403 authorization_error, not 404.
		// A proxy/route 404 is not a missing-user contract either. Require a
		// complete admin-visible filter=all list for either absence candidate.
		if errors.Is(err, errNotFound) {
			err = errors.New(err.Error())
		}
		forbidden := response.StatusCode() == http.StatusForbidden && response.JSON403 != nil && response.JSON403.Error != nil && *response.JSON403.Error == "authorization_error"
		if requestErr == nil && (response.StatusCode() == http.StatusNotFound || forbidden) {
			found := false
			verifyErr := a.walkUsers(ctx, func(user *client.User) error {
				if *user.Id == id {
					found = true
				}
				return nil
			})
			if verifyErr != nil {
				return nil, fmt.Errorf("%w; cannot establish user absence: %v", err, verifyErr)
			}
			if !found {
				return nil, fmt.Errorf("users.info: %w in the full admin workspace user list", errNotFound)
			}
		}
		return nil, err
	}
	if response.JSON200 == nil {
		return nil, errors.New("users.info: missing JSON response")
	}
	if err = checkEnvelope("users.info", response.JSON200.Ok, response.JSON200.Status); err != nil {
		return nil, err
	}
	if err = validateUser(response.JSON200.Data, id, ""); err != nil {
		return nil, err
	}
	return response.JSON200.Data, nil
}

func (a *apiClient) walkUsers(ctx context.Context, visit func(*client.User) error) error {
	if _, err := a.requireUserAdmin(ctx); err != nil {
		return err
	}
	limit, offset, total := 100, 0, -1
	filter, sort, direction := client.All, "createdAt", client.UsersListJSONBodyDirection("ASC")
	seen := make(map[uuid.UUID]bool)
	for {
		// The release supports filter=all, including suspended users for admins.
		//nolint:staticcheck // This release-verified status filter includes all users.
		response, err := a.UsersListWithResponse(ctx, client.UsersListJSONRequestBody{Limit: &limit, Offset: &offset, Filter: &filter, Sort: &sort, Direction: &direction})
		if response == nil {
			return a.checkResponse("users.list", nil, nil, err)
		}
		if err = a.checkResponse("users.list", response.HTTPResponse, response.Body, err); err != nil {
			// A missing list endpoint is not a completed empty enumeration.
			return errors.New(err.Error())
		}
		if response.JSON200 == nil || response.JSON200.Data == nil {
			return errors.New("users.list: missing JSON users response")
		}
		page := response.JSON200
		if err = checkEnvelope("users.list", page.Ok, page.Status); err != nil {
			return err
		}
		for _, user := range *page.Data {
			if err = validateUser(&user, uuid.Nil, ""); err != nil {
				return err
			}
			if seen[*user.Id] {
				return errors.New("users.list: duplicate user across pages; retry when the list is stable")
			}
			seen[*user.Id] = true
			if err = visit(&user); err != nil {
				return err
			}
		}
		next, more, err := nextOffset(page.Pagination, offset, len(*page.Data))
		if err != nil {
			return fmt.Errorf("users.list: %w", err)
		}
		if total != -1 && total != *page.Pagination.Total {
			return errors.New("users.list: total changed during pagination; retry when the list is stable")
		}
		total = *page.Pagination.Total
		if !more {
			return nil
		}
		offset = next
	}
}

// findUser returns errNotFound only after checking the entire admin-visible list.
func (a *apiClient) findUser(ctx context.Context, email string) (*client.User, error) {
	var match *client.User
	err := a.walkUsers(ctx, func(user *client.User) error {
		if normalizeUserEmail(string(user.Email.GetOrEmpty())) == normalizeUserEmail(email) {
			if match != nil {
				return errors.New("ambiguous user email; use an explicit UUID")
			}
			copy := *user
			match = &copy
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if match == nil {
		return nil, fmt.Errorf("no user found with the exact normalized email: %w", errNotFound)
	}
	user, err := a.readUser(ctx, *match.Id)
	if err == nil {
		err = validateUser(user, *match.Id, email)
	}
	return user, err
}

func (a *apiClient) userMutationResult(op string, response *http.Response, body []byte, requestErr error, ok *bool, status *int, user *client.User, id uuid.UUID, email string) (*client.User, error) {
	if err := a.checkResponse(op, response, body, requestErr); err != nil {
		return nil, err
	}
	if err := checkEnvelope(op, ok, status); err != nil {
		return nil, err
	}
	if err := validateUser(user, id, email); err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	return user, nil
}

func (a *apiClient) setUserSuspended(ctx context.Context, user *client.User, suspended bool) (*client.User, error) {
	if *user.IsSuspended == suspended {
		return user, nil
	}
	op := "users.activate"
	var updated *client.User
	var err error
	if suspended {
		op = "users.suspend"
		r, e := a.UsersSuspendWithResponse(ctx, client.UsersSuspendJSONRequestBody{Id: *user.Id})
		if r == nil {
			return nil, a.checkResponse(op, nil, nil, e)
		}
		if r.JSON200 == nil {
			return nil, missingUserJSON(a, op, r.HTTPResponse, r.Body, e)
		}
		updated, err = a.userMutationResult(op, r.HTTPResponse, r.Body, e, r.JSON200.Ok, r.JSON200.Status, r.JSON200.Data, *user.Id, string(user.Email.GetOrEmpty()))
	} else {
		r, e := a.UsersActivateWithResponse(ctx, client.UsersActivateJSONRequestBody{Id: *user.Id})
		if r == nil {
			return nil, a.checkResponse(op, nil, nil, e)
		}
		if r.JSON200 == nil {
			return nil, missingUserJSON(a, op, r.HTTPResponse, r.Body, e)
		}
		updated, err = a.userMutationResult(op, r.HTTPResponse, r.Body, e, r.JSON200.Ok, r.JSON200.Status, r.JSON200.Data, *user.Id, string(user.Email.GetOrEmpty()))
	}
	if err == nil && *updated.IsSuspended != suspended {
		err = fmt.Errorf("%s: response did not confirm desired suspension", op)
	}
	return updated, err
}

func missingUserJSON(a *apiClient, op string, response *http.Response, body []byte, err error) error {
	if err = a.checkResponse(op, response, body, err); err != nil {
		return err
	}
	return fmt.Errorf("%s: missing JSON response", op)
}

// Caller must authenticate and reject the owner before using any write helper.
// Returns the last validated user on failure so state can record partial writes.
func (a *apiClient) updateUser(ctx context.Context, current *client.User, plan userModel, manageName bool) (user *client.User, err error) {
	user = current
	if plan.Suspended.IsNull() || plan.Suspended.IsUnknown() {
		return user, errors.New("suspended must explicitly specify the desired final state")
	}
	role := client.UserRole(plan.Role.ValueString())
	if !role.Valid() {
		return user, errors.New("unsupported user role; use admin, member, viewer, or guest")
	}
	// Install compensation before activation. A failed response does not prove
	// that a write failed to commit. Desired suspension also applies to users
	// that were initially active, including newly invited guests.
	defer func() {
		if err != nil && plan.Suspended.ValueBool() {
			user, err = a.recoverUserSuspension(ctx, user, err)
		}
	}()
	if *user.Role != role && *user.IsSuspended {
		updated, activateErr := a.setUserSuspended(ctx, user, false)
		if activateErr != nil {
			return user, activateErr
		}
		user = updated
	}
	if *user.Role != role {
		r, e := a.UsersUpdateRoleWithResponse(ctx, client.UsersUpdateRoleJSONRequestBody{Id: *user.Id, Role: role})
		if r == nil {
			return user, a.checkResponse("users.update_role", nil, nil, e)
		}
		if r.JSON200 == nil {
			return user, missingUserJSON(a, "users.update_role", r.HTTPResponse, r.Body, e)
		}
		updated, e := a.userMutationResult("users.update_role", r.HTTPResponse, r.Body, e, r.JSON200.Ok, r.JSON200.Status, r.JSON200.Data, *user.Id, string(user.Email.GetOrEmpty()))
		if e != nil {
			return user, e
		}
		user = updated
		if *user.Role != role {
			return user, errors.New("users.update_role: response did not confirm desired role; check server edition and API restrictions")
		}
	}
	if manageName && *user.Name != plan.Name.ValueString() {
		name := plan.Name.ValueString()
		r, e := a.UsersUpdateWithResponse(ctx, client.UsersUpdateJSONRequestBody{Id: *user.Id, Name: &name})
		if r == nil {
			return user, a.checkResponse("users.update", nil, nil, e)
		}
		if r.JSON200 == nil {
			return user, missingUserJSON(a, "users.update", r.HTTPResponse, r.Body, e)
		}
		updated, e := a.userMutationResult("users.update", r.HTTPResponse, r.Body, e, r.JSON200.Ok, r.JSON200.Status, r.JSON200.Data, *user.Id, string(user.Email.GetOrEmpty()))
		if e != nil {
			return user, e
		}
		user = updated
		if *user.Name != name {
			return user, errors.New("users.update: response did not confirm desired name")
		}
	}
	updated, e := a.setUserSuspended(ctx, user, plan.Suspended.ValueBool())
	if e != nil {
		return user, e
	}
	user = updated
	return user, nil
}

// Compensation uses only the already-authorized identity, not an unvalidated
// mutation response. Force the suspension request because cached status can be
// stale after an ambiguous activation failure. This is not a replay of the
// original role/name/activation write. A bounded detached context permits
// cleanup even when Terraform canceled the original request.
func (a *apiClient) recoverUserSuspension(ctx context.Context, trusted *client.User, operationErr error) (*client.User, error) {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	forced := *trusted
	active := false
	forced.IsSuspended = &active
	restored, restoreErr := a.setUserSuspended(cleanupCtx, &forced, true)
	if restoreErr != nil {
		return trusted, fmt.Errorf("%w; restoring suspension also failed: %v; inspect the account before retrying", operationErr, restoreErr)
	}
	return restored, operationErr
}

func (a *apiClient) deleteUser(ctx context.Context, user *client.User) error {
	r, err := a.UsersDeleteWithResponse(ctx, client.UsersDeleteJSONRequestBody{Id: *user.Id})
	if r == nil {
		return a.checkResponse("users.delete", nil, nil, err)
	}
	if err = a.checkResponse("users.delete", r.HTTPResponse, r.Body, err); err != nil {
		return err
	}
	if r.JSON200 == nil {
		return errors.New("users.delete: missing JSON response")
	}
	if err = checkEnvelope("users.delete", r.JSON200.Ok, r.JSON200.Status); err != nil {
		return err
	}
	if r.JSON200.Success == nil || !*r.JSON200.Success {
		return errors.New("users.delete: missing or false success flag")
	}
	return nil
}
