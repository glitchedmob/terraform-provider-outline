// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/glitchedmob/terraform-provider-outline/internal/client"
	"github.com/google/uuid"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	tcexec "github.com/testcontainers/testcontainers-go/exec"
)

// These tests stop at pending workspace accounts. Activation changes suspension,
// not sign-in status. They do not perform an OIDC handshake or verify IdP access.
func (a *acceptanceAPI) acceptanceUser(id string) (*client.User, error) {
	userID, err := uuid.Parse(id)
	if err != nil {
		return nil, err
	}
	r, err := a.UsersInfoWithResponse(context.Background(), client.UsersInfoJSONRequestBody{Id: userID})
	if err != nil {
		return nil, err
	}
	if r.StatusCode() != http.StatusOK || r.JSON200 == nil || r.JSON200.Data == nil {
		return nil, fmt.Errorf("users.info %s: HTTP %d: %s", id, r.StatusCode(), r.Body)
	}
	if err := checkEnvelope("acceptance users.info", r.JSON200.Ok, r.JSON200.Status); err != nil {
		return nil, err
	}
	u := r.JSON200.Data
	if u.Id == nil || *u.Id != userID || u.Name == nil || u.Role == nil || !u.Role.Valid() ||
		u.IsSuspended == nil || !u.Email.IsSpecified() || u.Email.IsNull() || u.Email.GetOrEmpty() == "" {
		return nil, fmt.Errorf("users.info %s returned an incomplete or different account", id)
	}
	return u, nil
}

func (a *acceptanceAPI) acceptanceUserActor() (*client.User, error) {
	r, err := a.AuthInfoWithResponse(context.Background())
	if err != nil {
		return nil, err
	}
	if r.StatusCode() != http.StatusOK || r.JSON200 == nil || r.JSON200.Data == nil || r.JSON200.Data.User == nil {
		return nil, fmt.Errorf("auth.info: HTTP %d: %s", r.StatusCode(), r.Body)
	}
	u := r.JSON200.Data.User
	if u.Id == nil || u.Role == nil || *u.Role != client.UserRoleAdmin || u.IsSuspended == nil || *u.IsSuspended {
		return nil, fmt.Errorf("absence checks require an active admin-owned fixture key")
	}
	return u, checkEnvelope("acceptance auth.info", r.JSON200.Ok, r.JSON200.Status)
}

// Do not use production readUser as the test oracle. A 403 alone is not absence.
func (a *acceptanceAPI) acceptanceUserAbsent(id string) error {
	userID, err := uuid.Parse(id)
	if err != nil {
		return err
	}
	if _, err := a.acceptanceUserActor(); err != nil {
		return err
	}
	r, err := a.UsersInfoWithResponse(context.Background(), client.UsersInfoJSONRequestBody{Id: userID})
	if err != nil {
		return err
	}
	missing := r.StatusCode() == http.StatusNotFound && r.JSON404 != nil
	forbidden := r.StatusCode() == http.StatusForbidden && r.JSON403 != nil && r.JSON403.Error != nil && *r.JSON403.Error == "authorization_error"
	if !missing && !forbidden {
		return fmt.Errorf("user %s should be absent, users.info returned HTTP %d: %s", id, r.StatusCode(), r.Body)
	}
	limit, offset := 100, 0
	filter, sort, direction := client.All, "createdAt", client.UsersListJSONBodyDirection("ASC")
	seen := make(map[uuid.UUID]bool)
	for {
		//nolint:staticcheck // Outline 1.10.1 needs explicit filter=all to include suspended users.
		page, err := a.UsersListWithResponse(context.Background(), client.UsersListJSONRequestBody{
			Limit: &limit, Offset: &offset, Filter: &filter, Sort: &sort, Direction: &direction,
		})
		if err != nil {
			return err
		}
		if page.StatusCode() != http.StatusOK || page.JSON200 == nil || page.JSON200.Data == nil {
			return fmt.Errorf("users.list absence check: HTTP %d: %s", page.StatusCode(), page.Body)
		}
		if err := checkEnvelope("users.list absence check", page.JSON200.Ok, page.JSON200.Status); err != nil {
			return err
		}
		for _, u := range *page.JSON200.Data {
			if u.Id == nil || *u.Id == uuid.Nil || seen[*u.Id] {
				return fmt.Errorf("users.list absence check: missing or repeated user ID")
			}
			seen[*u.Id] = true
			if *u.Id == userID {
				return fmt.Errorf("user %s still exists in filter=all users.list", id)
			}
		}
		next, more, err := nextOffset(page.JSON200.Pagination, offset, len(*page.JSON200.Data))
		if err != nil {
			return err
		}
		if !more {
			return nil
		}
		offset = next
	}
}

func (a *acceptanceAPI) acceptanceInviteUsers(invites []client.Invite) ([]client.User, error) {
	suppress := true
	r, err := a.UsersInviteWithResponse(context.Background(), client.UsersInviteJSONRequestBody{Invites: invites, SuppressEmail: &suppress})
	if err != nil {
		return nil, err
	}
	if r.StatusCode() != http.StatusOK || r.JSON200 == nil || r.JSON200.Data == nil || r.JSON200.Data.Users == nil ||
		r.JSON200.Data.Sent == nil || r.JSON200.Data.Unsent == nil || len(*r.JSON200.Data.Unsent) != 0 ||
		len(*r.JSON200.Data.Users) != len(invites) || len(*r.JSON200.Data.Sent) != len(invites) {
		return nil, fmt.Errorf("users.invite fixture: HTTP %d: %s", r.StatusCode(), r.Body)
	}
	return *r.JSON200.Data.Users, checkEnvelope("acceptance users.invite", r.JSON200.Ok, r.JSON200.Status)
}

func acceptanceUserWrite(op, id string, response *http.Response, body []byte, requestErr error) error {
	if requestErr != nil {
		return requestErr
	}
	if response == nil || response.StatusCode != http.StatusOK {
		return fmt.Errorf("%s %s failed: %s", op, id, body)
	}
	var envelope struct {
		OK      *bool        `json:"ok"`
		Status  *int         `json:"status"`
		Data    *client.User `json:"data"`
		Success *bool        `json:"success"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return err
	}
	if err := checkEnvelope(op, envelope.OK, envelope.Status); err != nil {
		return err
	}
	if op == "users.delete" {
		if envelope.Success == nil || !*envelope.Success {
			return fmt.Errorf("users.delete did not confirm success")
		}
	} else if envelope.Data == nil || envelope.Data.Id == nil || envelope.Data.Id.String() != id {
		return fmt.Errorf("%s did not confirm target UUID %s", op, id)
	}
	return nil
}

func (a *acceptanceAPI) acceptanceUserName(id, name string) error {
	r, err := a.UsersUpdateWithResponse(context.Background(), client.UsersUpdateJSONRequestBody{Id: uuid.MustParse(id), Name: &name})
	if r == nil {
		return fmt.Errorf("users.update: %v", err)
	}
	return acceptanceUserWrite("users.update", id, r.HTTPResponse, r.Body, err)
}

func (a *acceptanceAPI) acceptanceUserRole(id string, role client.UserRole) error {
	r, err := a.UsersUpdateRoleWithResponse(context.Background(), client.UsersUpdateRoleJSONRequestBody{Id: uuid.MustParse(id), Role: role})
	if r == nil {
		return fmt.Errorf("users.update_role: %v", err)
	}
	return acceptanceUserWrite("users.update_role", id, r.HTTPResponse, r.Body, err)
}

func (a *acceptanceAPI) acceptanceUserSuspended(id string, suspended bool) error {
	if suspended {
		r, err := a.UsersSuspendWithResponse(context.Background(), client.UsersSuspendJSONRequestBody{Id: uuid.MustParse(id)})
		if r == nil {
			return fmt.Errorf("users.suspend: %v", err)
		}
		return acceptanceUserWrite("users.suspend", id, r.HTTPResponse, r.Body, err)
	}
	r, err := a.UsersActivateWithResponse(context.Background(), client.UsersActivateJSONRequestBody{Id: uuid.MustParse(id)})
	if r == nil {
		return fmt.Errorf("users.activate: %v", err)
	}
	return acceptanceUserWrite("users.activate", id, r.HTTPResponse, r.Body, err)
}

func (a *acceptanceAPI) acceptanceDeleteUser(id string) error {
	r, err := a.UsersDeleteWithResponse(context.Background(), client.UsersDeleteJSONRequestBody{Id: uuid.MustParse(id)})
	if r == nil {
		return fmt.Errorf("users.delete: %v", err)
	}
	return acceptanceUserWrite("users.delete", id, r.HTTPResponse, r.Body, err)
}

func (a *acceptanceAPI) checkAcceptanceUser(address string) resource.TestCheckFunc {
	return func(state *terraform.State) error {
		r, ok := state.RootModule().Resources[address]
		if !ok || r.Primary == nil || r.Primary.ID == "" {
			return fmt.Errorf("%s has no user ID", address)
		}
		u, err := a.acceptanceUser(r.Primary.ID)
		if err != nil {
			return err
		}
		for attr, value := range map[string]string{
			"id": u.Id.String(), "name": *u.Name, "email": string(u.Email.GetOrEmpty()),
			"role": string(*u.Role), "suspended": strconv.FormatBool(*u.IsSuspended),
		} {
			stateValue := r.Primary.Attributes[attr]
			if attr == "email" {
				stateValue, value = strings.ToLower(stateValue), strings.ToLower(value)
			}
			if stateValue != value {
				return fmt.Errorf("%s: API %s=%q, state=%q", address, attr, value, stateValue)
			}
		}
		return nil
	}
}

func (a *acceptanceAPI) checkAcceptanceUsersDestroyed(state *terraform.State) error {
	for _, r := range state.RootModule().Resources {
		if r.Type != "outline_user" || r.Primary == nil || r.Primary.ID == "" {
			continue
		}
		if r.Primary.Attributes["delete_permanently"] == "true" {
			if err := a.acceptanceUserAbsent(r.Primary.ID); err != nil {
				return err
			}
		} else if err := a.acceptanceUserRetained(r.Primary.ID, r.Primary.Attributes["email"]); err != nil {
			return err
		}
	}
	return nil
}

func (a *acceptanceAPI) acceptanceUserRetained(id, email string) error {
	u, err := a.acceptanceUser(id)
	if err != nil {
		return err
	}
	if !*u.IsSuspended || !strings.EqualFold(string(u.Email.GetOrEmpty()), email) {
		return fmt.Errorf("destroy must retain suspended account %s with email %s", id, email)
	}
	return nil
}

func (a *acceptanceAPI) acceptanceUserConfig(email string, suspended bool, extra string) string {
	return a.providerConfig + fmt.Sprintf(`
resource "outline_user" "test" {
  email = %q
  suspended = %t
%s
}
`, email, suspended, extra)
}

type acceptanceUserFixture struct {
	Version             string  `json:"version"`
	UserID              string  `json:"user_id"`
	Pending             bool    `json:"pending"`
	LastSignedInAt      *string `json:"last_signed_in_at"`
	InvitationEmailSent bool    `json:"invitation_email_sent"`
	PasswordAttribute   bool    `json:"password_attribute"`
	EmailEnabled        bool    `json:"email_enabled"`
	SuspendedAt         *string `json:"suspended_at"`
	SuspendedByID       *string `json:"suspended_by_id"`
	APIKey              string  `json:"api_key"`
}

func (a *acceptanceAPI) acceptanceInspectUser(t *testing.T, id, action string) acceptanceUserFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	const fixturePath = "/opt/outline/acceptance-user-fixture.cjs"
	if err := a.container.CopyFileToContainer(ctx, "../../integration/user-fixture.cjs", fixturePath, 0o644); err != nil {
		t.Fatal(err)
	}
	exitCode, output, err := a.container.Exec(ctx, []string{"node", fixturePath, id, action}, tcexec.Multiplexed())
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(output)
	if err != nil {
		t.Fatal(err)
	}
	if exitCode != 0 {
		t.Fatalf("Outline user fixture exited %d:\n%s", exitCode, data)
	}
	var fixture acceptanceUserFixture
	const marker = "OUTLINE_ACCEPTANCE_USER="
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, marker) {
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, marker)), &fixture); err != nil {
				t.Fatal(err)
			}
		} else if line != "" {
			t.Logf("Outline user fixture: %s", line)
		}
	}
	if fixture.UserID != id || fixture.Version == "" || action == "key" && fixture.APIKey == "" {
		t.Fatal("Outline user fixture did not return the requested account")
	}
	return fixture
}

func TestAccUserLifecycle(t *testing.T) {
	api := newAcceptanceAPI(t)
	const address, email = "outline_user.test", "terraform-user@www.example.invalid"
	actor, err := api.acceptanceUserActor()
	if err != nil {
		t.Fatal(err)
	}
	actorName := *actor.Name
	var id string
	check := func(name, role string, suspended bool) resource.TestCheckFunc {
		return resource.ComposeAggregateTestCheckFunc(
			api.checkAcceptanceUser(address),
			resource.TestCheckResourceAttr(address, "name", name),
			resource.TestCheckResourceAttr(address, "role", role),
			resource.TestCheckResourceAttr(address, "suspended", strconv.FormatBool(suspended)),
			resource.TestCheckResourceAttr(address, "suppress_email", "true"),
			resource.TestCheckResourceAttr(address, "delete_permanently", "false"),
			func(state *terraform.State) error {
				newID := state.RootModule().Resources[address].Primary.ID
				if id != "" && id != newID {
					return fmt.Errorf("user update replaced %s with %s", id, newID)
				}
				id = newID
				u, err := api.acceptanceUser(actor.Id.String())
				if err != nil {
					return err
				}
				if *u.Name != actorName || *u.Role != client.UserRoleAdmin || *u.IsSuspended {
					return fmt.Errorf("targeted user update modified the API-key owner")
				}
				return nil
			},
		)
	}
	config := func(role string, suspended bool, name string) string {
		extra := fmt.Sprintf("  role = %q\n", role)
		if name != "" {
			extra += fmt.Sprintf("  name = %q\n", name)
		}
		return api.acceptanceUserConfig(email, suspended, extra)
	}
	managed := config("member", false, "Managed user")
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acceptanceProviderFactories(),
		CheckDestroy:             api.checkAcceptanceUsersDestroyed,
		Steps: []resource.TestStep{
			{Config: api.acceptanceUserConfig(email, false, ""), Check: resource.ComposeAggregateTestCheckFunc(
				check("Pending user", "member", false), func(_ *terraform.State) error {
					fixture := api.acceptanceInspectUser(t, id, "inspect")
					if !fixture.Pending || fixture.LastSignedInAt != nil || fixture.InvitationEmailSent || fixture.EmailEnabled || fixture.PasswordAttribute {
						return fmt.Errorf("expected a pending account without sign-in, password, invitation email, or SMTP: %+v", fixture)
					}
					return nil
				},
			)},
			{ResourceName: address, ImportState: true, ImportStateVerify: true},
			{Config: api.acceptanceUserConfig(email, false, ""), ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}}},
			{Config: config("member", false, strings.Repeat("😀", 255)), Check: check(strings.Repeat("😀", 255), "member", false)},
			{Config: config("admin", false, "Managed user"), Check: check("Managed user", "admin", false)},
			{Config: config("viewer", true, "Managed user"), Check: check("Managed user", "viewer", true)},
			{
				PreConfig: func() {
					// Release 1.10.1 rejects direct role writes on suspended users.
					r, err := api.UsersUpdateRoleWithResponse(t.Context(), client.UsersUpdateRoleJSONRequestBody{Id: uuid.MustParse(id), Role: client.UserRoleGuest})
					if err != nil || r == nil || r.StatusCode() != http.StatusForbidden || r.JSON403 == nil || r.JSON403.Error == nil || *r.JSON403.Error != "authorization_error" {
						t.Fatalf("expected suspended-role authorization_error, got %v, %v", r, err)
					}
				},
				Config: config("guest", true, "Managed user"), Check: check("Managed user", "guest", true),
			},
			{ResourceName: address, ImportState: true, ImportStateVerify: true},
			{Config: managed, Check: check("Managed user", "member", false)},
			{
				PreConfig: func() {
					if err := api.acceptanceUserName(id, "External name"); err != nil {
						t.Fatal(err)
					}
					if err := api.acceptanceUserRole(id, client.UserRoleAdmin); err != nil {
						t.Fatal(err)
					}
					if err := api.acceptanceUserSuspended(id, true); err != nil {
						t.Fatal(err)
					}
				},
				RefreshState: true, ExpectNonEmptyPlan: true, Check: check("External name", "admin", true),
			},
			{Config: managed, ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
				plancheck.ExpectResourceAction(address, plancheck.ResourceActionUpdate),
			}}, Check: check("Managed user", "member", false)},
			{Config: config("member", false, ""), Check: check("Managed user", "member", false)},
			{
				PreConfig: func() {
					if err := api.acceptanceUserName(id, "Unmanaged external name"); err != nil {
						t.Fatal(err)
					}
				},
				Config: config("member", false, ""), ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}},
				Check: check("Unmanaged external name", "member", false),
			},
			{Config: config("viewer", true, ""), Check: check("Unmanaged external name", "viewer", true)},
			{Config: api.acceptanceUserConfig(email, false, ""), Check: check("Unmanaged external name", "member", false)},
			{Config: api.providerConfig, Check: func(_ *terraform.State) error { return api.acceptanceUserRetained(id, email) }},
		},
	})
	// Remove invitation metadata after Outline commits the account. Verify
	// detached cleanup and truthful tainted state against the real API.
	for _, failure := range []string{"", "suspend"} {
		userMalformedInviteTerraform(t, api.apiClient, "malformed-invite-"+failure+"@www.example.invalid", failure, func(id string, suspended bool) {
			u, err := api.acceptanceUser(id)
			if err != nil || *u.IsSuspended != suspended {
				t.Fatalf("malformed invitation cleanup: user=%v err=%v want suspended=%t", u, err, suspended)
			}
			owner, err := api.acceptanceUser(actor.Id.String())
			if err != nil || *owner.IsSuspended || *owner.Role != client.UserRoleAdmin {
				t.Fatalf("invitation cleanup modified the API owner: %v", err)
			}
		})
	}
}

func TestAccUserInvitationRoles(t *testing.T) {
	api := newAcceptanceAPI(t)
	// The guest role is accepted by users.invite's schema but stored as member.
	// Terraform must reconcile it through users.update_role after creation.
	users, err := api.acceptanceInviteUsers([]client.Invite{{Email: "guest-probe@example.invalid", Name: "Guest probe", Role: client.UserRoleGuest}})
	if err != nil {
		t.Fatal(err)
	}
	if users[0].Role == nil || *users[0].Role != client.UserRoleMember {
		t.Fatalf("Outline 1.10.1 guest invitation did not store the expected member role")
	}
	for _, role := range []string{"admin", "viewer", "guest"} {
		t.Run(role, func(t *testing.T) {
			const address = "outline_user.test"
			resource.Test(t, resource.TestCase{
				ProtoV6ProviderFactories: acceptanceProviderFactories(),
				CheckDestroy:             api.checkAcceptanceUsersDestroyed,
				Steps: []resource.TestStep{{
					Config: api.acceptanceUserConfig(role+"-invite@example.invalid", true, fmt.Sprintf("  role = %q\n  delete_permanently = true", role)),
					Check: resource.ComposeAggregateTestCheckFunc(api.checkAcceptanceUser(address),
						resource.TestCheckResourceAttr(address, "role", role),
						resource.TestCheckResourceAttr(address, "suspended", "true")),
				}},
			})
		})
	}
}

func TestAccUserDefaultDestroyAndImport(t *testing.T) {
	api := newAcceptanceAPI(t)
	const address, email = "outline_user.test", "retained@example.invalid"
	var id, suspendedAt string
	capture := func(state *terraform.State) error {
		newID := state.RootModule().Resources[address].Primary.ID
		if id != "" && id != newID {
			return fmt.Errorf("import or activation changed the account UUID")
		}
		id = newID
		return nil
	}
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acceptanceProviderFactories(),
		CheckDestroy:             api.checkAcceptanceUsersDestroyed,
		Steps: []resource.TestStep{
			{Config: api.acceptanceUserConfig(email, false, ""), Check: resource.ComposeAggregateTestCheckFunc(api.checkAcceptanceUser(address), capture)},
			{Config: api.providerConfig, Check: func(state *terraform.State) error {
				if _, ok := state.RootModule().Resources[address]; ok {
					return fmt.Errorf("destroy did not remove the Terraform resource")
				}
				return api.acceptanceUserRetained(id, email)
			}},
			{Config: api.acceptanceUserConfig(email, false, ""), ExpectError: regexp.MustCompile(`(?s)User already exists.*Import.*UUID`)},
			{
				PreConfig: func() {
					if err := api.acceptanceUserRetained(id, email); err != nil {
						t.Fatal(err)
					}
				},
				Config: api.acceptanceUserConfig(email, true, ""), ResourceName: address,
				ImportState: true, ImportStatePersist: true,
				ImportStateIdFunc: func(_ *terraform.State) (string, error) { return id, nil },
				ImportStateCheck: func(states []*terraform.InstanceState) error {
					if len(states) != 1 || states[0].ID != id || states[0].Attributes["suspended"] != "true" ||
						states[0].Attributes["suppress_email"] != "true" || states[0].Attributes["delete_permanently"] != "false" {
						return fmt.Errorf("UUID import did not retain the account and safe defaults")
					}
					return nil
				},
			},
			{Config: api.acceptanceUserConfig(email, true, ""), ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}}, Check: capture},
			{Config: api.acceptanceUserConfig(email, false, ""), Check: resource.ComposeAggregateTestCheckFunc(api.checkAcceptanceUser(address), capture)},
			{Config: api.acceptanceUserConfig(email, true, ""), Check: func(_ *terraform.State) error {
				fixture := api.acceptanceInspectUser(t, id, "inspect")
				if !fixture.Pending || fixture.LastSignedInAt != nil {
					return fmt.Errorf("activation must not count as a user sign-in")
				}
				if fixture.SuspendedAt == nil || fixture.SuspendedByID == nil {
					return fmt.Errorf("suspend did not record a timestamp and actor")
				}
				suspendedAt = *fixture.SuspendedAt
				return nil
			}},
			{Config: api.providerConfig, Check: func(_ *terraform.State) error {
				if err := api.acceptanceUserRetained(id, email); err != nil {
					return err
				}
				fixture := api.acceptanceInspectUser(t, id, "inspect")
				if fixture.SuspendedAt == nil || *fixture.SuspendedAt != suspendedAt {
					return fmt.Errorf("destroy of an already suspended account called suspend again")
				}
				return nil
			}},
		},
	})
}

func TestAccUserPermanentDestroyAndMissingState(t *testing.T) {
	api := newAcceptanceAPI(t)
	const address, email = "outline_user.test", "deleted@example.invalid"
	config := api.acceptanceUserConfig(email, false, "  delete_permanently = true")
	var deletedID, recreatedID string
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acceptanceProviderFactories(),
		CheckDestroy:             api.checkAcceptanceUsersDestroyed,
		Steps: []resource.TestStep{
			{Config: config, Check: func(state *terraform.State) error {
				deletedID = state.RootModule().Resources[address].Primary.ID
				return api.checkAcceptanceUser(address)(state)
			}},
			{ResourceName: address, ImportState: true, ImportStateVerify: true, ImportStateVerifyIgnore: []string{"delete_permanently"}},
			{
				PreConfig: func() {
					if err := api.acceptanceDeleteUser(deletedID); err != nil {
						t.Fatal(err)
					}
					info, err := api.UsersInfoWithResponse(t.Context(), client.UsersInfoJSONRequestBody{Id: uuid.MustParse(deletedID)})
					if err != nil || info == nil || info.StatusCode() != http.StatusForbidden || info.JSON403 == nil || info.JSON403.Error == nil || *info.JSON403.Error != "authorization_error" {
						t.Fatalf("Outline 1.10.1 missing user should return 403 authorization_error: %v, %v", info, err)
					}
					if err := api.acceptanceUserAbsent(deletedID); err != nil {
						t.Fatal(err)
					}
				},
				Config: config, ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(address, plancheck.ResourceActionCreate)}},
				Check: func(state *terraform.State) error {
					recreatedID = state.RootModule().Resources[address].Primary.ID
					if recreatedID == deletedID {
						return fmt.Errorf("deleted account was not recreated with a new UUID")
					}
					return api.checkAcceptanceUser(address)(state)
				},
			},
			{Config: api.providerConfig, Check: func(_ *terraform.State) error { return api.acceptanceUserAbsent(recreatedID) }},
		},
	})
}

func TestAccUserDataSourceLookups(t *testing.T) {
	api := newAcceptanceAPI(t)
	// The target follows a full page. Neither substring email nor matching name
	// is an exact email match, and suspended users require filter=all.
	for batch := 0; batch < 6; batch++ {
		var invites []client.Invite
		for i := 0; i < 20; i++ {
			invites = append(invites, client.Invite{Email: fmt.Sprintf("target+%03d@example.invalid", batch*20+i), Name: "target@example.invalid", Role: client.UserRoleMember})
		}
		if _, err := api.acceptanceInviteUsers(invites); err != nil {
			t.Fatal(err)
		}
	}
	const email = "target@example.invalid"
	users, err := api.acceptanceInviteUsers([]client.Invite{{Email: email, Name: "Lookup target", Role: client.UserRoleViewer}})
	if err != nil {
		t.Fatal(err)
	}
	id := users[0].Id.String()
	limit, offset := 100, 0
	filter, sort, direction := client.All, "createdAt", client.UsersListJSONBodyDirection("ASC")
	//nolint:staticcheck // Match the released server's suspended-inclusive pagination contract.
	page, err := api.UsersListWithResponse(t.Context(), client.UsersListJSONRequestBody{Limit: &limit, Offset: &offset, Filter: &filter, Sort: &sort, Direction: &direction})
	if err != nil || page == nil || page.JSON200 == nil || page.JSON200.Data == nil || len(*page.JSON200.Data) != 100 || page.JSON200.Pagination == nil || page.JSON200.Pagination.Total == nil || *page.JSON200.Pagination.Total <= 100 {
		t.Fatalf("fixture did not produce more than one users.list page: %v", err)
	}
	for _, u := range *page.JSON200.Data {
		if u.Id != nil && u.Id.String() == id {
			t.Fatal("lookup target unexpectedly appears on the first page")
		}
	}
	byEmail := func(email string) string {
		return api.providerConfig + fmt.Sprintf("\ndata \"outline_user\" \"by_email\" {\n  email = %q\n}\n", email)
	}
	config := byEmail("TARGET@EXAMPLE.INVALID") + fmt.Sprintf("\ndata \"outline_user\" \"by_id\" {\n  id = %q\n}\n", id)
	check := func(name, role string, suspended bool) resource.TestCheckFunc {
		return resource.ComposeAggregateTestCheckFunc(
			api.checkAcceptanceUser("data.outline_user.by_id"), api.checkAcceptanceUser("data.outline_user.by_email"),
			resource.TestCheckResourceAttr("data.outline_user.by_email", "id", id),
			resource.TestCheckResourceAttr("data.outline_user.by_email", "email", "TARGET@EXAMPLE.INVALID"),
			resource.TestCheckResourceAttr("data.outline_user.by_id", "name", name),
			resource.TestCheckResourceAttr("data.outline_user.by_id", "role", role),
			resource.TestCheckResourceAttr("data.outline_user.by_id", "suspended", strconv.FormatBool(suspended)),
		)
	}
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acceptanceProviderFactories(),
		Steps: []resource.TestStep{
			{Config: config, Check: check("Lookup target", "viewer", false)},
			{
				PreConfig: func() {
					if err := api.acceptanceUserName(id, "External lookup name"); err != nil {
						t.Fatal(err)
					}
					if err := api.acceptanceUserRole(id, client.UserRoleAdmin); err != nil {
						t.Fatal(err)
					}
					if err := api.acceptanceUserSuspended(id, true); err != nil {
						t.Fatal(err)
					}
				},
				Config: config, Check: check("External lookup name", "admin", true),
			},
			{Config: byEmail("target@example.inval"), ExpectError: regexp.MustCompile(`exact normalized email`)},
			{Config: api.providerConfig + fmt.Sprintf("\ndata \"outline_user\" \"missing\" {\n  id = %q\n}\n", uuid.NewString()), ExpectError: regexp.MustCompile(`(?i)not found`)},
			{Config: byEmail(email) + fmt.Sprintf("\ndata \"outline_user\" \"invalid\" {\n  id = %q\n  email = %q\n}\n", id, email), ExpectError: regexp.MustCompile(`(?i)(only one|exactly one|invalid attribute combination)`)},
			{Config: api.providerConfig + "\ndata \"outline_user\" \"invalid\" {}\n", ExpectError: regexp.MustCompile(`(?i)(at least one|exactly one|invalid attribute combination)`)},
			{Config: config, Check: check("External lookup name", "admin", true)},
		},
	})
}

func TestAccUserAdminAndOwnerProtection(t *testing.T) {
	api := newAcceptanceAPI(t)
	actor, err := api.acceptanceUserActor()
	if err != nil {
		t.Fatal(err)
	}
	const email = "protected-create@example.invalid"
	users, err := api.acceptanceInviteUsers([]client.Invite{
		{Email: "member-key@example.invalid", Name: "Member key", Role: client.UserRoleMember},
		{Email: "suspended-key@example.invalid", Name: "Suspended admin key", Role: client.UserRoleAdmin},
	})
	if err != nil {
		t.Fatal(err)
	}
	memberKey := api.acceptanceInspectUser(t, users[0].Id.String(), "key").APIKey
	adminKey := api.acceptanceInspectUser(t, users[1].Id.String(), "key").APIKey
	adminClient, err := newAPIClient(api.baseURL, adminKey, 30, "acceptance")
	if err != nil {
		t.Fatal(err)
	}
	active, err := adminClient.AuthInfoWithResponse(t.Context())
	if err != nil || active == nil || active.StatusCode() != http.StatusOK || active.JSON200 == nil || active.JSON200.Data == nil || active.JSON200.Data.User == nil || active.JSON200.Data.User.Id == nil || *active.JSON200.Data.User.Id != *users[1].Id {
		t.Fatalf("fixture admin key must authenticate before suspension: %v", err)
	}
	if err := api.acceptanceUserSuspended(users[1].Id.String(), true); err != nil {
		t.Fatal(err)
	}
	keyConfig := func(key string) string {
		return fmt.Sprintf("provider \"outline\" {\n  base_url = %q\n  api_key = %q\n}\nresource \"outline_user\" \"test\" {\n  email = %q\n  suspended = false\n}\n", api.baseURL, key, email)
	}
	// Suspension rejects authentication, then the worker revokes the user's
	// keys. The same previously valid key returns 403 before cleanup or 401 after.
	suspendedKeyError := regexp.MustCompile(`(?s)auth.info: HTTP (403.*user_suspended|401.*authentication_required)`)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acceptanceProviderFactories(),
		Steps: []resource.TestStep{
			{Config: api.providerConfig + fmt.Sprintf("\nresource \"outline_user\" \"test\" {\n  email = %q\n}\n", email), ExpectError: regexp.MustCompile(`(?s)Missing required argument.*suspended`)},
			{Config: api.providerConfig + "\nresource \"outline_user\" \"test\" {\n  suspended = false\n}\n", ExpectError: regexp.MustCompile(`(?s)Missing required argument.*email`)},
			{Config: api.acceptanceUserConfig(strings.ToUpper(string(actor.Email.GetOrEmpty())), false, ""), ExpectError: regexp.MustCompile(`(?s)refusing to manage the API-key owner.*account`)},
			{Config: keyConfig(memberKey), ExpectError: regexp.MustCompile(`active admin-owned API key`)},
			{Config: keyConfig(adminKey), ExpectError: suspendedKeyError},
			{Config: fmt.Sprintf("provider \"outline\" {\n  base_url = %q\n  api_key = %q\n}\ndata \"outline_user\" \"invalid\" {\n  id = %q\n}\n", api.baseURL, memberKey, actor.Id.String()), ExpectError: regexp.MustCompile(`active admin-owned API key`)},
			{Config: fmt.Sprintf("provider \"outline\" {\n  base_url = %q\n  api_key = %q\n}\ndata \"outline_user\" \"invalid\" {\n  id = %q\n}\n", api.baseURL, adminKey, actor.Id.String()), ExpectError: suspendedKeyError},
			// Reading the owner is safe. Writes are not.
			{Config: api.providerConfig + fmt.Sprintf("\ndata \"outline_user\" \"owner\" {\n  id = %q\n}\n", actor.Id.String()), Check: api.checkAcceptanceUser("data.outline_user.owner")},
		},
	})
	u, err := api.acceptanceUser(actor.Id.String())
	if err != nil {
		t.Fatal(err)
	}
	if *u.Role != client.UserRoleAdmin || *u.IsSuspended || *u.Name != *actor.Name {
		t.Fatal("self-management attempt changed the bootstrap admin")
	}

	// Import is read-only. Switching to the imported account's own key must
	// refuse updates and both destroy policies, without breaking safe cleanup.
	const ownerEmail, ownerName, address = "imported-owner@example.invalid", "Imported owner", "outline_user.test"
	owners, err := api.acceptanceInviteUsers([]client.Invite{{Email: ownerEmail, Name: ownerName, Role: client.UserRoleAdmin}})
	if err != nil {
		t.Fatal(err)
	}
	ownerID := owners[0].Id.String()
	ownerKey := api.acceptanceInspectUser(t, ownerID, "key").APIKey
	ownerProvider := fmt.Sprintf("provider \"outline\" {\n  base_url = %q\n  api_key = %q\n}\n", api.baseURL, ownerKey)
	ownerConfig := func(providerConfig, name string, suspended, permanent bool) string {
		return providerConfig + fmt.Sprintf(`
resource "outline_user" "test" {
  email = %q
  name = %q
  role = "admin"
  suspended = %t
  delete_permanently = %t
}
`, ownerEmail, name, suspended, permanent)
	}
	checkOwnerUnchanged := func() {
		u, err := api.acceptanceUser(ownerID)
		if err != nil {
			t.Fatal(err)
		}
		if *u.Name != ownerName || *u.Role != client.UserRoleAdmin || *u.IsSuspended {
			t.Fatal("imported API-key owner's account was changed")
		}
	}
	selfError := regexp.MustCompile(`(?s)refusing to manage the API-key owner.*account`)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acceptanceProviderFactories(),
		CheckDestroy:             api.checkAcceptanceUsersDestroyed,
		Steps: []resource.TestStep{
			{
				Config: ownerConfig(api.providerConfig, ownerName, false, false), ResourceName: address,
				ImportState: true, ImportStatePersist: true, ImportStateId: ownerID,
			},
			{Config: ownerConfig(ownerProvider, "Refused self rename", true, false), ExpectError: selfError},
			{PreConfig: checkOwnerUnchanged, Config: ownerProvider, ExpectError: selfError},
			{
				PreConfig: checkOwnerUnchanged, Config: ownerConfig(api.providerConfig, ownerName, false, true),
				Check: api.checkAcceptanceUser(address),
			},
			{Config: ownerProvider, ExpectError: selfError},
			{
				PreConfig: checkOwnerUnchanged, Config: ownerConfig(api.providerConfig, ownerName, false, false),
				Check: api.checkAcceptanceUser(address),
			},
			{Config: api.providerConfig, Check: func(_ *terraform.State) error { return api.acceptanceUserRetained(ownerID, ownerEmail) }},
		},
	})
}
