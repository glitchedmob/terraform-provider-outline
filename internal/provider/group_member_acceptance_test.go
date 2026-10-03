// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"regexp"
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

const acceptanceGroupMemberAddress = "outline_group_member.test"

type acceptanceGroupMemberRecord struct {
	APIID      string
	Permission string
	Name       string
	Role       string
	Suspended  bool
}

func acceptanceGroupMemberUser(u *client.User) (acceptanceGroupMemberRecord, error) {
	// Group endpoints use presentUser without includeDetails. Email is absent,
	// even for an admin. users.info is the separate workspace-account oracle.
	if u == nil || u.Id == nil || *u.Id == uuid.Nil || u.Name == nil || u.Role == nil || !u.Role.Valid() || u.IsSuspended == nil {
		return acceptanceGroupMemberRecord{}, fmt.Errorf("membership fixture returned an incomplete public user")
	}
	return acceptanceGroupMemberRecord{
		Name: *u.Name, Role: string(*u.Role), Suspended: *u.IsSuspended,
	}, nil
}

// Use the released HTTP API as the oracle, not observeGroupMember or its page
// decoder. Check every page, including users and memberships unrelated to the
// target. groups.list only returns an avatar preview and cannot prove absence.
func (a *acceptanceAPI) acceptanceGroupMembers(groupID string) (map[string]acceptanceGroupMemberRecord, []string, error) {
	group := uuid.MustParse(groupID)
	result := make(map[string]acceptanceGroupMemberRecord)
	var order []string
	limit, offset, total := 100, 0, -1
	for {
		r, err := a.GroupsMembershipsWithResponse(context.Background(), client.GroupsMembershipsJSONRequestBody{
			Id: group, Limit: &limit, Offset: &offset,
		})
		if err != nil {
			return nil, nil, err
		}
		if r.StatusCode() != http.StatusOK || r.JSON200 == nil || r.JSON200.Data == nil ||
			r.JSON200.Data.GroupMemberships == nil || r.JSON200.Data.Users == nil {
			return nil, nil, fmt.Errorf("groups.memberships fixture: HTTP %d: %s", r.StatusCode(), r.Body)
		}
		page := r.JSON200
		if err := checkEnvelope("acceptance groups.memberships", page.Ok, page.Status); err != nil {
			return nil, nil, err
		}
		members, users := *page.Data.GroupMemberships, *page.Data.Users
		p := page.Pagination
		if p == nil || p.Limit == nil || *p.Limit != limit || p.Offset == nil || *p.Offset != offset ||
			p.Total == nil || *p.Total < 0 || total != -1 && total != *p.Total ||
			len(members) != len(users) || len(members) > limit || offset+len(members) > *p.Total {
			return nil, nil, fmt.Errorf("groups.memberships fixture: inconsistent arrays or pagination")
		}
		total = *p.Total
		pageUsers := make(map[string]acceptanceGroupMemberRecord)
		for i := range users {
			record, err := acceptanceGroupMemberUser(&users[i])
			if err != nil {
				return nil, nil, err
			}
			id := users[i].Id.String()
			if _, exists := pageUsers[id]; exists {
				return nil, nil, fmt.Errorf("groups.memberships fixture: repeated user %s", id)
			}
			pageUsers[id] = record
		}
		for _, member := range members {
			if member.GroupId == nil || *member.GroupId != group || member.UserId == nil || *member.UserId == uuid.Nil ||
				member.Id == nil || *member.Id != member.UserId.String()+"-"+groupID || member.Permission == nil || !member.Permission.Valid() {
				return nil, nil, fmt.Errorf("groups.memberships fixture: incomplete or inconsistent membership")
			}
			id := member.UserId.String()
			record, err := acceptanceGroupMemberUser(member.User)
			if err != nil {
				return nil, nil, err
			}
			listed, exists := pageUsers[id]
			if !exists || record != listed || *member.User.Id != *member.UserId {
				return nil, nil, fmt.Errorf("groups.memberships fixture: inline and listed users disagree")
			}
			if _, exists := result[id]; exists {
				return nil, nil, fmt.Errorf("groups.memberships fixture: repeated membership %s", id)
			}
			record.APIID, record.Permission = *member.Id, string(*member.Permission)
			result[id] = record
			order = append(order, id)
		}
		if len(result) == total {
			return result, order, nil
		}
		if len(members) != limit {
			return nil, nil, fmt.Errorf("groups.memberships fixture: incomplete page before total")
		}
		offset += len(members)
	}
}

func (a *acceptanceAPI) acceptanceGroupMemberPermission(group, user, permission string) error {
	members, _, err := a.acceptanceGroupMembers(group)
	if err != nil {
		return err
	}
	member, exists := members[user]
	if permission == "" && !exists {
		return nil
	}
	if !exists || member.Permission != permission || permission == "" {
		return fmt.Errorf("pair %s/%s: exists=%t, permission=%q, wanted %q", group, user, exists, member.Permission, permission)
	}
	return nil
}

func (a *acceptanceAPI) acceptanceGroupMemberCollateral(group, target string, expected map[string]acceptanceGroupMemberRecord) error {
	actual, _, err := a.acceptanceGroupMembers(group)
	if err != nil {
		return err
	}
	delete(actual, target)
	baseline := make(map[string]acceptanceGroupMemberRecord)
	for id, member := range expected {
		if id != target {
			baseline[id] = member
		}
	}
	if !reflect.DeepEqual(actual, baseline) {
		return fmt.Errorf("targeted membership operation changed unrelated memberships or users")
	}
	return nil
}

// Deliberately use add_user here as well as update_user. The released server
// upserts existing permissions, so provider Create must not adopt an existing pair.
func (a *acceptanceAPI) acceptanceWriteGroupMember(group, user string, permission client.GroupPermission, add bool) error {
	var response *http.Response
	var body []byte
	var err error
	if add {
		var r *client.GroupsAddUserResponse
		r, err = a.GroupsAddUserWithResponse(context.Background(), client.GroupsAddUserJSONRequestBody{
			Id: uuid.MustParse(group), UserId: uuid.MustParse(user), Permission: &permission,
		})
		if r != nil {
			response, body = r.HTTPResponse, r.Body
		}
	} else {
		var r *client.GroupsUpdateUserResponse
		r, err = a.GroupsUpdateUserWithResponse(context.Background(), client.GroupsUpdateUserJSONRequestBody{
			Id: uuid.MustParse(group), UserId: uuid.MustParse(user), Permission: permission,
		})
		if r != nil {
			response, body = r.HTTPResponse, r.Body
		}
	}
	if err != nil {
		return err
	}
	if response == nil || response.StatusCode != http.StatusOK {
		return fmt.Errorf("membership write fixture: %s", body)
	}
	var envelope struct {
		OK     *bool `json:"ok"`
		Status *int  `json:"status"`
		Data   *struct {
			Groups      *[]client.Group     `json:"groups"`
			Users       *[]client.User      `json:"users"`
			Memberships *[]client.GroupUser `json:"groupMemberships"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return err
	}
	if err := checkEnvelope("acceptance membership write", envelope.OK, envelope.Status); err != nil {
		return err
	}
	d := envelope.Data
	if d == nil || d.Groups == nil || len(*d.Groups) != 1 || (*d.Groups)[0].Id == nil || (*d.Groups)[0].Id.String() != group ||
		d.Users == nil || len(*d.Users) != 1 || d.Memberships == nil || len(*d.Memberships) != 1 {
		return fmt.Errorf("membership write fixture did not confirm one group, user, and membership")
	}
	u, m := &(*d.Users)[0], &(*d.Memberships)[0]
	listed, err := acceptanceGroupMemberUser(u)
	if err != nil {
		return err
	}
	inline, err := acceptanceGroupMemberUser(m.User)
	if err != nil {
		return err
	}
	if u.Id.String() != user || m.UserId == nil || m.UserId.String() != user || m.GroupId == nil || m.GroupId.String() != group ||
		m.Id == nil || *m.Id != user+"-"+group || m.Permission == nil || *m.Permission != permission ||
		m.User.Id.String() != user || inline != listed {
		return fmt.Errorf("membership write fixture returned a different pair or permission")
	}
	return nil
}

func (a *acceptanceAPI) acceptanceRemoveGroupMember(group, user string) error {
	r, err := a.GroupsRemoveUserWithResponse(context.Background(), client.GroupsRemoveUserJSONRequestBody{
		Id: uuid.MustParse(group), UserId: uuid.MustParse(user),
	})
	if err != nil {
		return err
	}
	if r.StatusCode() != http.StatusOK || r.JSON200 == nil || r.JSON200.Data == nil || r.JSON200.Data.Groups == nil ||
		len(*r.JSON200.Data.Groups) != 1 || (*r.JSON200.Data.Groups)[0].Id == nil || (*r.JSON200.Data.Groups)[0].Id.String() != group {
		return fmt.Errorf("groups.remove_user fixture: HTTP %d: %s", r.StatusCode(), r.Body)
	}
	if err := checkEnvelope("acceptance groups.remove_user", r.JSON200.Ok, r.JSON200.Status); err != nil {
		return err
	}
	return a.acceptanceGroupMemberPermission(group, user, "")
}

func (a *acceptanceAPI) acceptanceGroupMemberConfig(group, user, extra string) string {
	return a.providerConfig + fmt.Sprintf(`
resource "outline_group_member" "test" {
  group_id = %q
  user_id = %q
%s
}
`, group, user, extra)
}

func (a *acceptanceAPI) checkAcceptanceGroupMember(address string) resource.TestCheckFunc {
	return func(state *terraform.State) error {
		r, exists := state.RootModule().Resources[address]
		if !exists || r.Primary == nil {
			return fmt.Errorf("%s has no membership state", address)
		}
		attrs := r.Primary.Attributes
		id := attrs["group_id"] + "/" + attrs["user_id"]
		if r.Primary.ID != id || attrs["id"] != id || strings.Count(id, "/") != 1 {
			return fmt.Errorf("membership state must use the group_id/user_id slash ID")
		}
		return a.acceptanceGroupMemberPermission(attrs["group_id"], attrs["user_id"], attrs["permission"])
	}
}

func acceptanceGroupMemberImport(group, user, permission string) resource.TestCheckFunc {
	return resource.ComposeAggregateTestCheckFunc(
		resource.TestCheckResourceAttr(acceptanceGroupMemberAddress, "id", group+"/"+user),
		resource.TestCheckResourceAttr(acceptanceGroupMemberAddress, "group_id", group),
		resource.TestCheckResourceAttr(acceptanceGroupMemberAddress, "user_id", user),
		resource.TestCheckResourceAttr(acceptanceGroupMemberAddress, "permission", permission),
	)
}

// Run after Terraform has saved its plan, so refresh cannot mask an Update or
// Delete preflight failure. This exercises the resource against a live server.
// An empty plan after restoring access proves the failed operation retained state.
type acceptanceGroupMemberBeforeApply func() error

func (f acceptanceGroupMemberBeforeApply) CheckPlan(_ context.Context, _ plancheck.CheckPlanRequest, resp *plancheck.CheckPlanResponse) {
	resp.Error = f()
}

func TestAccGroupMemberLifecycle(t *testing.T) {
	api := newAcceptanceAPI(t)
	const address = acceptanceGroupMemberAddress
	parents := api.providerConfig + `
resource "outline_group" "test" {
  name = "Terraform group member lifecycle"
}
resource "outline_user" "test" {
  email = "group-member-target@example.invalid"
  name = "Membership target"
  suspended = false
  delete_permanently = true
}
`
	config := func(extra string) string {
		return parents + fmt.Sprintf(`
resource "outline_group_member" "test" {
  group_id = outline_group.test.id
  user_id = outline_user.test.id
%s
}
`, extra)
	}
	var group, user, pair string
	var baseline map[string]acceptanceGroupMemberRecord
	actor, err := api.acceptanceUserActor()
	if err != nil {
		t.Fatal(err)
	}
	check := func(permission string) resource.TestCheckFunc {
		return resource.ComposeAggregateTestCheckFunc(
			api.checkAcceptanceGroupMember(address), api.checkAcceptanceGroup("outline_group.test"), api.checkAcceptanceUser("outline_user.test"),
			resource.TestCheckResourceAttr(address, "permission", permission),
			resource.TestCheckResourceAttrPair(address, "group_id", "outline_group.test", "id"),
			resource.TestCheckResourceAttrPair(address, "user_id", "outline_user.test", "id"),
			resource.TestCheckResourceAttr("outline_user.test", "role", "member"),
			func(state *terraform.State) error {
				r := state.RootModule().Resources[address].Primary
				if pair != "" && pair != r.ID {
					return fmt.Errorf("permission update replaced the membership pair")
				}
				pair, group, user = r.ID, r.Attributes["group_id"], r.Attributes["user_id"]
				if baseline == nil {
					// The API-key owner is an unrelated member, not a group admin.
					// Workspace admins still have permission to update the target.
					if err := api.acceptanceWriteGroupMember(group, actor.Id.String(), client.GroupPermissionMember, true); err != nil {
						return err
					}
					baseline, _, err = api.acceptanceGroupMembers(group)
					if err != nil {
						return err
					}
				}
				return api.acceptanceGroupMemberCollateral(group, user, baseline)
			},
		)
	}
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acceptanceProviderFactories(),
		CheckDestroy: func(state *terraform.State) error {
			if err := api.checkAcceptanceGroupsDestroyed(state); err != nil {
				return err
			}
			return api.checkAcceptanceUsersDestroyed(state)
		},
		Steps: []resource.TestStep{
			{Config: config(""), Check: check("member")},
			{ResourceName: address, ImportState: true, ImportStateVerify: true},
			{Config: config(""), ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}}},
			{Config: config(`  permission = "admin"`), ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
				plancheck.ExpectResourceAction(address, plancheck.ResourceActionUpdate),
			}}, Check: check("admin")},
			{ResourceName: address, ImportState: true, ImportStateVerify: true},
			{PreConfig: func() {
				if err := api.acceptanceWriteGroupMember(group, user, client.GroupPermissionMember, false); err != nil {
					t.Fatal(err)
				}
			}, RefreshState: true, ExpectNonEmptyPlan: true, Check: check("member")},
			{Config: config(`  permission = "admin"`), ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
				plancheck.ExpectResourceAction(address, plancheck.ResourceActionUpdate),
			}}, Check: check("admin")},
			{Config: parents, Check: func(_ *terraform.State) error {
				if err := api.acceptanceGroupMemberPermission(group, user, ""); err != nil {
					return err
				}
				return api.acceptanceGroupMemberCollateral(group, user, baseline)
			}},
			{Config: api.providerConfig, Check: func(_ *terraform.State) error {
				if err := api.acceptanceGroupAbsent(group); err != nil {
					return err
				}
				return api.acceptanceUserAbsent(user)
			}},
		},
	})
}

func TestAccGroupMemberExistingPairAndPagination(t *testing.T) {
	api := newAcceptanceAPI(t)
	group, err := api.acceptanceCreateGroup("Terraform group member pagination")
	if err != nil {
		t.Fatal(err)
	}
	users, err := api.acceptanceInviteUsers([]client.Invite{{
		Email: "oldest-member@example.invalid", Name: "Oldest membership", Role: client.UserRoleMember,
	}})
	if err != nil || len(users) != 1 || users[0].Id == nil {
		t.Fatalf("invite target: %v", err)
	}
	user := users[0].Id.String()
	if err := api.acceptanceWriteGroupMember(group, user, client.GroupPermissionAdmin, true); err != nil {
		t.Fatal(err)
	}
	// Invitations are capped at 20 by Outline. Add 101 newer memberships through
	// the API, leaving the target after page one in the release's createdAt DESC order.
	for start := 0; start < 101; start += 20 {
		var invites []client.Invite
		for i := start; i < start+20 && i < 101; i++ {
			invites = append(invites, client.Invite{
				Email: fmt.Sprintf("membership-page-%03d@example.invalid", i), Name: fmt.Sprintf("Page member %03d", i), Role: client.UserRoleMember,
			})
		}
		invited, err := api.acceptanceInviteUsers(invites)
		if err != nil {
			t.Fatal(err)
		}
		for i, u := range invited {
			if u.Id == nil {
				t.Fatal("pagination invitation has no user UUID")
			}
			permission := client.GroupPermissionMember
			if (start+i)%7 == 0 {
				permission = client.GroupPermissionAdmin
			}
			if err := api.acceptanceWriteGroupMember(group, u.Id.String(), permission, true); err != nil {
				t.Fatal(err)
			}
		}
	}
	baseline, order, err := api.acceptanceGroupMembers(group)
	if err != nil {
		t.Fatal(err)
	}
	if len(order) != 102 || order[101] != user {
		t.Fatalf("pagination fixture must put the target beyond the first 100 memberships, got %d memberships", len(order))
	}
	// Verify add_user's upsert contract directly. Updating the oldest pair must
	// not reorder createdAt or change any unrelated membership or workspace user.
	for _, permission := range []client.GroupPermission{client.GroupPermissionMember, client.GroupPermissionAdmin} {
		if err := api.acceptanceWriteGroupMember(group, user, permission, true); err != nil {
			t.Fatal(err)
		}
		if err := api.acceptanceGroupMemberPermission(group, user, string(permission)); err != nil {
			t.Fatal(err)
		}
		if err := api.acceptanceGroupMemberCollateral(group, user, baseline); err != nil {
			t.Fatal(err)
		}
	}
	_, order, err = api.acceptanceGroupMembers(group)
	if err != nil || len(order) != 102 || order[101] != user {
		t.Fatalf("upsert must leave the target beyond page one: %v", err)
	}
	assert := func(permission string) resource.TestCheckFunc {
		return resource.ComposeAggregateTestCheckFunc(api.checkAcceptanceGroupMember(acceptanceGroupMemberAddress),
			acceptanceGroupMemberImport(group, user, permission), func(_ *terraform.State) error {
				return api.acceptanceGroupMemberCollateral(group, user, baseline)
			})
	}
	checkRetained := func() {
		if err := api.acceptanceGroupMemberPermission(group, user, "admin"); err != nil {
			t.Fatal(err)
		}
		if err := api.acceptanceGroupMemberCollateral(group, user, baseline); err != nil {
			t.Fatal(err)
		}
	}
	config := api.acceptanceGroupMemberConfig(group, user, "")
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acceptanceProviderFactories(),
		CheckDestroy:             func(_ *terraform.State) error { return api.acceptanceGroupMemberPermission(group, user, "") },
		Steps: []resource.TestStep{
			{Config: config, ExpectError: regexp.MustCompile(`(?s)Group membership already exists.*Import.*` + regexp.QuoteMeta(group+"/"+user))},
			{PreConfig: checkRetained, Config: config, ResourceName: acceptanceGroupMemberAddress,
				ImportState: true, ImportStatePersist: true, ImportStateId: group + "/" + user,
				ImportStateCheck: func(states []*terraform.InstanceState) error {
					if len(states) != 1 || states[0].ID != group+"/"+user || states[0].Attributes["id"] != group+"/"+user ||
						states[0].Attributes["group_id"] != group || states[0].Attributes["user_id"] != user || states[0].Attributes["permission"] != "admin" {
						return fmt.Errorf("slash import did not retain the existing admin pair")
					}
					checkRetained()
					return nil
				}},
			{PreConfig: checkRetained, Config: config, ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
				plancheck.ExpectResourceAction(acceptanceGroupMemberAddress, plancheck.ResourceActionUpdate),
			}}, Check: assert("member")},
			{ResourceName: acceptanceGroupMemberAddress, ImportState: true, ImportStateVerify: true},
			{Config: api.acceptanceGroupMemberConfig(group, user, `  permission = "admin"`), Check: assert("admin")},
			{PreConfig: func() {
				if err := api.acceptanceWriteGroupMember(group, user, client.GroupPermissionMember, false); err != nil {
					t.Fatal(err)
				}
			}, RefreshState: true, ExpectNonEmptyPlan: true, Check: assert("member")},
			{Config: api.acceptanceGroupMemberConfig(group, user, `  permission = "admin"`), Check: assert("admin")},
			{Config: api.providerConfig, Check: func(_ *terraform.State) error {
				if err := api.acceptanceGroupMemberPermission(group, user, ""); err != nil {
					return err
				}
				return api.acceptanceGroupMemberCollateral(group, user, baseline)
			}},
		},
	})
}

func TestAccGroupMemberMissingPairAndParents(t *testing.T) {
	api := newAcceptanceAPI(t)
	for _, missing := range []string{"pair", "group", "user"} {
		t.Run(missing, func(t *testing.T) {
			fixture := func(operation string) (string, string) {
				group, err := api.acceptanceCreateGroup("Terraform group member missing " + operation + " " + missing)
				if err != nil {
					t.Fatal(err)
				}
				users, err := api.acceptanceInviteUsers([]client.Invite{{
					Email: "missing-" + operation + "-" + missing + "@example.invalid",
					Name:  "Missing " + missing, Role: client.UserRoleMember,
				}})
				if err != nil || len(users) != 1 || users[0].Id == nil {
					t.Fatalf("invite missing fixture: %v", err)
				}
				return group, users[0].Id.String()
			}
			group, user := fixture("read")
			deleteGroup, deleteUser := fixture("delete")
			remove := func(group, user string) error {
				switch missing {
				case "group":
					return api.acceptanceDeleteGroup(group)
				case "user":
					return api.acceptanceDeleteUser(user)
				default:
					return api.acceptanceRemoveGroupMember(group, user)
				}
			}
			absent := func(group, user string) error {
				switch missing {
				case "group":
					return api.acceptanceGroupAbsent(group)
				case "user":
					if err := api.acceptanceUserAbsent(user); err != nil {
						return err
					}
				}
				return api.acceptanceGroupMemberPermission(group, user, "")
			}
			config := api.acceptanceGroupMemberConfig(group, user, "")
			steps := []resource.TestStep{
				{Config: config, Check: api.checkAcceptanceGroupMember(acceptanceGroupMemberAddress)},
				{PreConfig: func() {
					if err := remove(group, user); err != nil {
						t.Fatal(err)
					}
					if err := absent(group, user); err != nil {
						t.Fatal(err)
					}
				}, RefreshState: true, ExpectNonEmptyPlan: true, Check: func(state *terraform.State) error {
					if _, exists := state.RootModule().Resources[acceptanceGroupMemberAddress]; exists {
						return fmt.Errorf("missing %s did not remove membership state", missing)
					}
					return absent(group, user)
				}},
			}
			if missing == "pair" {
				steps = append(steps, resource.TestStep{
					Config: config, ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(acceptanceGroupMemberAddress, plancheck.ResourceActionCreate),
					}}, Check: api.checkAcceptanceGroupMember(acceptanceGroupMemberAddress),
				})
			}
			steps = append(steps,
				resource.TestStep{Config: api.providerConfig},
				resource.TestStep{Config: api.acceptanceGroupMemberConfig(deleteGroup, deleteUser, ""), Check: api.checkAcceptanceGroupMember(acceptanceGroupMemberAddress)},
				// Removing a pair or parent after the destroy plan exercises Delete,
				// not just Read's removal of already missing state during refresh.
				resource.TestStep{Config: api.providerConfig, ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(acceptanceGroupMemberAddress, plancheck.ResourceActionDestroy),
					acceptanceGroupMemberBeforeApply(func() error {
						if err := remove(deleteGroup, deleteUser); err != nil {
							return err
						}
						return absent(deleteGroup, deleteUser)
					}),
				}}, Check: func(_ *terraform.State) error { return absent(deleteGroup, deleteUser) }},
			)
			resource.Test(t, resource.TestCase{
				ProtoV6ProviderFactories: acceptanceProviderFactories(),
				CheckDestroy: func(_ *terraform.State) error {
					if err := absent(group, user); err != nil {
						return err
					}
					return absent(deleteGroup, deleteUser)
				},
				Steps: steps,
			})
		})
	}
}

func (a *acceptanceAPI) acceptanceGroupMemberSync(t *testing.T, groupID string, synced bool) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	const path = "/opt/outline/acceptance-group-member-fixture.cjs"
	if err := a.container.CopyFileToContainer(ctx, "../../integration/group-member-fixture.cjs", path, 0o644); err != nil {
		return err
	}
	action := "unsync"
	if synced {
		action = "sync"
	}
	exitCode, output, err := a.container.Exec(ctx, []string{"node", path, groupID, action}, tcexec.Multiplexed())
	if err != nil {
		return err
	}
	data, err := io.ReadAll(output)
	if err != nil {
		return err
	}
	if exitCode != 0 {
		return fmt.Errorf("Outline group member fixture exited %d: %s", exitCode, data)
	}
	var fixture struct {
		Version string `json:"version"`
		GroupID string `json:"group_id"`
		Synced  bool   `json:"synced"`
	}
	const marker = "OUTLINE_ACCEPTANCE_GROUP_MEMBER="
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, marker) {
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, marker)), &fixture); err != nil {
				return err
			}
		} else if line != "" {
			t.Logf("Outline group member fixture: %s", line)
		}
	}
	if fixture.Version != "1.10.1" || fixture.GroupID != groupID || fixture.Synced != synced {
		return fmt.Errorf("Outline group member fixture did not confirm synchronization")
	}
	group, err := a.acceptanceGroup(groupID)
	if err != nil {
		return err
	}
	if group.ExternalId.IsSpecified() && !group.ExternalId.IsNull() && group.ExternalId.GetOrEmpty() != "" ||
		(group.ExternalGroup.IsSpecified() && !group.ExternalGroup.IsNull()) != synced {
		return fmt.Errorf("groups.info did not confirm an externalGroup-only synchronization fixture")
	}
	return nil
}

func TestAccGroupMemberExternalGroups(t *testing.T) {
	api := newAcceptanceAPI(t)
	users, err := api.acceptanceInviteUsers([]client.Invite{
		{Email: "external-member@example.invalid", Name: "External target", Role: client.UserRoleMember},
		{Email: "external-other@example.invalid", Name: "External unrelated", Role: client.UserRoleMember},
	})
	if err != nil || len(users) != 2 || users[0].Id == nil || users[1].Id == nil {
		t.Fatalf("invite external fixture: %v", err)
	}
	user, other := users[0].Id.String(), users[1].Id.String()
	externalError := regexp.MustCompile(`externally linked or synchronized groups cannot be managed`)
	t.Run("external ID", func(t *testing.T) {
		group, err := api.acceptanceCreateGroup("Terraform group member externally linked")
		if err != nil {
			t.Fatal(err)
		}
		if err := api.acceptanceWriteGroupMember(group, other, client.GroupPermissionAdmin, true); err != nil {
			t.Fatal(err)
		}
		externalID := "terraform-external-membership"
		r, err := api.GroupsUpdateWithResponse(t.Context(), client.GroupsUpdateJSONRequestBody{Id: uuid.MustParse(group), ExternalId: &externalID})
		if err != nil || r == nil || r.StatusCode() != http.StatusOK || r.JSON200 == nil || r.JSON200.Data == nil || r.JSON200.Data.ExternalId.GetOrEmpty() != externalID {
			t.Fatalf("externalId fixture update failed: %v", err)
		}
		baseline, _, err := api.acceptanceGroupMembers(group)
		if err != nil {
			t.Fatal(err)
		}
		resource.Test(t, resource.TestCase{
			ProtoV6ProviderFactories: acceptanceProviderFactories(),
			Steps: []resource.TestStep{
				{Config: api.acceptanceGroupMemberConfig(group, user, ""), ExpectError: externalError},
				{Config: api.acceptanceGroupMemberConfig(group, other, ""), ResourceName: acceptanceGroupMemberAddress,
					ImportState: true, ImportStateId: group + "/" + other, ExpectError: externalError},
				{Config: api.providerConfig, Check: func(_ *terraform.State) error {
					if err := api.acceptanceGroupMemberPermission(group, user, ""); err != nil {
						return err
					}
					return api.acceptanceGroupMemberCollateral(group, user, baseline)
				}},
			},
		})
	})
	t.Run("synced after plan", func(t *testing.T) {
		group, err := api.acceptanceCreateGroup("Terraform group member synchronized")
		if err != nil {
			t.Fatal(err)
		}
		if err := api.acceptanceWriteGroupMember(group, other, client.GroupPermissionAdmin, true); err != nil {
			t.Fatal(err)
		}
		baseline, _, err := api.acceptanceGroupMembers(group)
		if err != nil {
			t.Fatal(err)
		}
		memberConfig := api.acceptanceGroupMemberConfig(group, user, "")
		assertUnchanged := func() error {
			if err := api.acceptanceGroupMemberPermission(group, user, "member"); err != nil {
				return err
			}
			return api.acceptanceGroupMemberCollateral(group, user, baseline)
		}
		restore := func() {
			if err := assertUnchanged(); err != nil {
				t.Fatal(err)
			}
			if err := api.acceptanceGroupMemberSync(t, group, false); err != nil {
				t.Fatal(err)
			}
		}
		check := resource.ComposeAggregateTestCheckFunc(api.checkAcceptanceGroupMember(acceptanceGroupMemberAddress),
			func(_ *terraform.State) error { return assertUnchanged() })
		resource.Test(t, resource.TestCase{
			ProtoV6ProviderFactories: acceptanceProviderFactories(),
			CheckDestroy:             func(_ *terraform.State) error { return api.acceptanceGroupMemberPermission(group, user, "") },
			Steps: []resource.TestStep{
				{PreConfig: func() {
					if err := api.acceptanceGroupMemberSync(t, group, true); err != nil {
						t.Fatal(err)
					}
				}, Config: memberConfig, ExpectError: externalError},
				{PreConfig: func() {
					if err := api.acceptanceGroupMemberPermission(group, user, ""); err != nil {
						t.Fatal(err)
					}
					if err := api.acceptanceGroupMemberSync(t, group, false); err != nil {
						t.Fatal(err)
					}
				}, Config: memberConfig, Check: check},
				{PreConfig: func() {
					if err := api.acceptanceGroupMemberSync(t, group, true); err != nil {
						t.Fatal(err)
					}
				}, Config: memberConfig, ExpectError: externalError},
				{PreConfig: restore, Config: memberConfig, ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}}, Check: check},
				{Config: api.acceptanceGroupMemberConfig(group, user, `  permission = "admin"`), ExpectError: regexp.MustCompile(`(?s)Unable to update group member.*externally linked or synchronized`),
					ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction(acceptanceGroupMemberAddress, plancheck.ResourceActionUpdate),
						acceptanceGroupMemberBeforeApply(func() error { return api.acceptanceGroupMemberSync(t, group, true) }),
					}}},
				{PreConfig: restore, Config: memberConfig, ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}}, Check: check},
				{Config: api.providerConfig, ExpectError: regexp.MustCompile(`(?s)Unable to delete group member.*externally linked or synchronized`), ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(acceptanceGroupMemberAddress, plancheck.ResourceActionDestroy),
					acceptanceGroupMemberBeforeApply(func() error { return api.acceptanceGroupMemberSync(t, group, true) }),
				}}},
				{PreConfig: restore, Config: memberConfig, ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}}, Check: check},
				{Config: api.providerConfig, Check: func(_ *terraform.State) error {
					if err := api.acceptanceGroupMemberPermission(group, user, ""); err != nil {
						return err
					}
					return api.acceptanceGroupMemberCollateral(group, user, baseline)
				}},
			},
		})
	})
}

func TestAccGroupMemberAdminRequirementAndForbidden(t *testing.T) {
	api := newAcceptanceAPI(t)
	group, err := api.acceptanceCreateGroup("Terraform group member admin requirement")
	if err != nil {
		t.Fatal(err)
	}
	users, err := api.acceptanceInviteUsers([]client.Invite{
		{Email: "membership-actor@example.invalid", Name: "Membership actor", Role: client.UserRoleAdmin},
		{Email: "membership-protected@example.invalid", Name: "Protected target", Role: client.UserRoleMember},
		{Email: "membership-suspended@example.invalid", Name: "Suspended actor", Role: client.UserRoleAdmin},
		{Email: "membership-unrelated@example.invalid", Name: "Unrelated member", Role: client.UserRoleMember},
		{Email: "membership-guest@example.invalid", Name: "Guest actor", Role: client.UserRoleMember},
	})
	if err != nil || len(users) != 5 || users[0].Id == nil || users[1].Id == nil || users[2].Id == nil || users[3].Id == nil || users[4].Id == nil {
		t.Fatalf("invite admin fixture: %v", err)
	}
	actor, user, suspended := users[0].Id.String(), users[1].Id.String(), users[2].Id.String()
	key := api.acceptanceInspectUser(t, actor, "key").APIKey
	suspendedKey := api.acceptanceInspectUser(t, suspended, "key").APIKey
	// Guest invitations are stored as member in this release. Demotion to guest
	// asynchronously revokes keys, so wait for revocation before creating the
	// test-only guest key. Never demote the managed membership's key owner to guest.
	guestID := users[4].Id.String()
	revokedKey := api.acceptanceInspectUser(t, guestID, "key").APIKey
	if err := api.acceptanceUserRole(guestID, client.UserRoleGuest); err != nil {
		t.Fatal(err)
	}
	revoked, err := newAPIClient(api.baseURL, revokedKey, 30, "acceptance")
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		r, err := revoked.AuthInfoWithResponse(t.Context())
		if err != nil || r == nil {
			t.Fatalf("guest key revocation probe: %v", err)
		}
		if r.StatusCode() == http.StatusUnauthorized && r.JSON401 != nil && r.JSON401.Error != nil && *r.JSON401.Error == "authentication_required" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("guest demotion did not revoke the old key: HTTP %d", r.StatusCode())
		}
		time.Sleep(100 * time.Millisecond)
	}
	guestKey := api.acceptanceInspectUser(t, guestID, "key").APIKey
	if err := api.acceptanceUserSuspended(suspended, true); err != nil {
		t.Fatal(err)
	}
	if err := api.acceptanceWriteGroupMember(group, actor, client.GroupPermissionAdmin, true); err != nil {
		t.Fatal(err)
	}
	if err := api.acceptanceWriteGroupMember(group, users[3].Id.String(), client.GroupPermissionMember, true); err != nil {
		t.Fatal(err)
	}
	baseline, _, err := api.acceptanceGroupMembers(group)
	if err != nil {
		t.Fatal(err)
	}
	withKey := func(apiKey, extra string) string {
		return strings.Replace(api.acceptanceGroupMemberConfig(group, user, extra), api.apiKey, apiKey, 1)
	}
	memberConfig := withKey(key, "")
	adminError := regexp.MustCompile(`active admin-owned API key`)
	assertUnchanged := func() error {
		if err := api.acceptanceGroupMemberPermission(group, user, "member"); err != nil {
			return err
		}
		actual, _, err := api.acceptanceGroupMembers(group)
		if err != nil {
			return err
		}
		actualActor, actorExists := actual[actor]
		// Only the key owner's workspace role is deliberately changed by this test.
		delete(actual, user)
		delete(actual, actor)
		expected := make(map[string]acceptanceGroupMemberRecord)
		for id, record := range baseline {
			if id != actor && id != user {
				expected[id] = record
			}
		}
		if !reflect.DeepEqual(actual, expected) {
			return fmt.Errorf("forbidden operation changed unrelated memberships")
		}
		if !actorExists || actualActor.Permission != "admin" {
			return fmt.Errorf("forbidden operation changed the key owner's group admin membership")
		}
		return nil
	}
	restore := func() {
		if err := assertUnchanged(); err != nil {
			t.Fatal(err)
		}
		current, err := api.acceptanceUser(actor)
		if err != nil {
			t.Fatal(err)
		}
		if *current.Role != client.UserRoleAdmin {
			if err := api.acceptanceUserRole(actor, client.UserRoleAdmin); err != nil {
				t.Fatal(err)
			}
		}
	}
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acceptanceProviderFactories(),
		CheckDestroy:             func(_ *terraform.State) error { return api.acceptanceGroupMemberPermission(group, user, "") },
		Steps: []resource.TestStep{
			{PreConfig: func() {
				if err := api.acceptanceUserRole(actor, client.UserRoleMember); err != nil {
					t.Fatal(err)
				}
			}, Config: memberConfig, ExpectError: adminError},
			{Config: withKey(suspendedKey, ""), ExpectError: regexp.MustCompile(`(?s)auth.info: HTTP (403.*user_suspended|401.*authentication_required)`)},
			{PreConfig: func() {
				if err := api.acceptanceGroupMemberPermission(group, user, ""); err != nil {
					t.Fatal(err)
				}
				if err := api.acceptanceUserRole(actor, client.UserRoleAdmin); err != nil {
					t.Fatal(err)
				}
			}, Config: memberConfig, Check: api.checkAcceptanceGroupMember(acceptanceGroupMemberAddress)},
			{PreConfig: func() {
				guest, err := newAPIClient(api.baseURL, guestKey, 30, "acceptance")
				if err != nil {
					t.Fatal(err)
				}
				r, err := guest.GroupsInfoWithResponse(t.Context(), client.GroupsInfoJSONRequestBody{Id: uuid.MustParse(group)})
				if err != nil || r == nil {
					t.Fatalf("guest groups.info: %v", err)
				}
				if r.StatusCode() != http.StatusForbidden || r.JSON403 == nil || r.JSON403.Error == nil || *r.JSON403.Error != "authorization_error" {
					t.Fatalf("existing group must be forbidden to a guest, not absent: HTTP %d: %s", r.StatusCode(), r.Body)
				}
			}, Config: withKey(guestKey, ""), ExpectError: adminError},
			{PreConfig: restore, Config: memberConfig, ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}}, Check: api.checkAcceptanceGroupMember(acceptanceGroupMemberAddress)},
			{Config: withKey(key, `  permission = "admin"`), ExpectError: regexp.MustCompile(`(?s)Unable to update group member.*active admin-owned API key`), ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
				plancheck.ExpectResourceAction(acceptanceGroupMemberAddress, plancheck.ResourceActionUpdate),
				acceptanceGroupMemberBeforeApply(func() error { return api.acceptanceUserRole(actor, client.UserRoleMember) }),
			}}},
			{PreConfig: restore, Config: memberConfig, ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}}, Check: api.checkAcceptanceGroupMember(acceptanceGroupMemberAddress)},
			{Config: strings.Replace(api.providerConfig, api.apiKey, key, 1), ExpectError: regexp.MustCompile(`(?s)Unable to delete group member.*active admin-owned API key`), ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
				plancheck.ExpectResourceAction(acceptanceGroupMemberAddress, plancheck.ResourceActionDestroy),
				acceptanceGroupMemberBeforeApply(func() error { return api.acceptanceUserRole(actor, client.UserRoleMember) }),
			}}},
			{PreConfig: restore, Config: memberConfig, ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}}, Check: api.checkAcceptanceGroupMember(acceptanceGroupMemberAddress)},
			{Config: strings.Replace(api.providerConfig, api.apiKey, key, 1), Check: func(_ *terraform.State) error {
				if err := api.acceptanceGroupMemberPermission(group, user, ""); err != nil {
					return err
				}
				return api.acceptanceGroupMemberCollateral(group, user, baseline)
			}},
		},
	})
}

func TestAccGroupMemberWorkspaceAdminSelf(t *testing.T) {
	api := newAcceptanceAPI(t)
	actor, err := api.acceptanceUserActor()
	if err != nil {
		t.Fatal(err)
	}
	group, err := api.acceptanceCreateGroup("Terraform group member workspace admin self")
	if err != nil {
		t.Fatal(err)
	}
	users, err := api.acceptanceInviteUsers([]client.Invite{{Email: "self-other@example.invalid", Name: "Self unrelated", Role: client.UserRoleMember}})
	if err != nil || len(users) != 1 || users[0].Id == nil {
		t.Fatalf("invite self fixture: %v", err)
	}
	if err := api.acceptanceWriteGroupMember(group, users[0].Id.String(), client.GroupPermissionAdmin, true); err != nil {
		t.Fatal(err)
	}
	baseline, _, err := api.acceptanceGroupMembers(group)
	if err != nil {
		t.Fatal(err)
	}
	user := actor.Id.String()
	check := func(permission string) resource.TestCheckFunc {
		return resource.ComposeAggregateTestCheckFunc(api.checkAcceptanceGroupMember(acceptanceGroupMemberAddress),
			acceptanceGroupMemberImport(group, user, permission), func(_ *terraform.State) error {
				current, err := api.acceptanceUserActor()
				if err != nil {
					return err
				}
				if *current.Id != *actor.Id || *current.Name != *actor.Name || *current.Role != client.UserRoleAdmin || *current.IsSuspended {
					return fmt.Errorf("self group permission changed the API-key owner's workspace account")
				}
				return api.acceptanceGroupMemberCollateral(group, user, baseline)
			})
	}
	// isGroupAdmin treats workspace admins as group admins regardless of their
	// membership. Unlike outline_user, group permission has no owner protection.
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acceptanceProviderFactories(),
		CheckDestroy:             func(_ *terraform.State) error { return api.acceptanceGroupMemberPermission(group, user, "") },
		Steps: []resource.TestStep{
			{Config: api.acceptanceGroupMemberConfig(group, user, ""), Check: check("member")},
			{Config: api.acceptanceGroupMemberConfig(group, user, `  permission = "admin"`), Check: check("admin")},
			{ResourceName: acceptanceGroupMemberAddress, ImportState: true, ImportStateVerify: true},
			{Config: api.acceptanceGroupMemberConfig(group, user, ""), Check: check("member")},
			{Config: api.providerConfig, Check: func(_ *terraform.State) error {
				if err := api.acceptanceGroupMemberPermission(group, user, ""); err != nil {
					return err
				}
				if _, err := api.acceptanceUserActor(); err != nil {
					return err
				}
				return api.acceptanceGroupMemberCollateral(group, user, baseline)
			}},
		},
	})
}
