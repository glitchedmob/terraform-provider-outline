// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/glitchedmob/terraform-provider-outline/internal/client"
	"github.com/google/uuid"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/oapi-codegen/nullable"
)

const acceptanceCollectionUserAddress = "outline_collection_user.test"

var acceptanceCollectionUserRoles = []string{"read", "read_write", "admin"}

type acceptanceCollectionUserRecord struct{ APIID, Permission string }

func acceptanceCollectionUserRecordFromAPI(collection string, m client.Membership) (acceptanceCollectionUserRecord, error) {
	if m.Id == nil || m.UserId == nil || *m.UserId == uuid.Nil ||
		!m.CollectionId.IsSpecified() || m.CollectionId.IsNull() || m.CollectionId.GetOrEmpty().String() != collection ||
		!m.DocumentId.IsSpecified() || !m.DocumentId.IsNull() || !m.SourceId.IsSpecified() || !m.SourceId.IsNull() ||
		m.Permission == nil || !m.Permission.Valid() {
		return acceptanceCollectionUserRecord{}, fmt.Errorf("collection user fixture returned an incomplete or non-direct grant")
	}
	id, err := uuid.Parse(*m.Id)
	if err != nil || id == uuid.Nil || id.String() != *m.Id {
		return acceptanceCollectionUserRecord{}, fmt.Errorf("collection user API grant must have its own canonical UUID")
	}
	return acceptanceCollectionUserRecord{APIID: *m.Id, Permission: string(*m.Permission)}, nil
}

// Use the release's complete unfiltered membership listing, not provider lookup
// helpers, collection policies, or groups.list membership previews.
func (a *acceptanceAPI) acceptanceCollectionUsers(collection string) (map[string]acceptanceCollectionUserRecord, error) {
	result := make(map[string]acceptanceCollectionUserRecord)
	seen := make(map[string]bool)
	limit, offset, total := 100, 0, -1
	for {
		r, err := a.CollectionsMembershipsWithResponse(context.Background(), client.CollectionsMembershipsJSONRequestBody{Id: uuid.MustParse(collection), Limit: &limit, Offset: &offset})
		if err != nil {
			return nil, err
		}
		if r.StatusCode() != http.StatusOK || r.JSON200 == nil || r.JSON200.Data == nil || r.JSON200.Data.Memberships == nil || r.JSON200.Data.Users == nil {
			return nil, fmt.Errorf("collections.memberships fixture: HTTP %d: %s", r.StatusCode(), r.Body)
		}
		page := r.JSON200
		if err := checkCollectionGrantEnvelope("acceptance collections.memberships", page.Ok, page.Status); err != nil {
			return nil, err
		}
		members, users, p := *page.Data.Memberships, *page.Data.Users, page.Pagination
		if p == nil || p.Limit == nil || *p.Limit != limit || p.Offset == nil || *p.Offset != offset || p.Total == nil || *p.Total < 0 || total != -1 && total != *p.Total || len(members) != len(users) || len(members) > limit || offset+len(members) > *p.Total {
			return nil, fmt.Errorf("collections.memberships fixture: inconsistent arrays or pagination")
		}
		total = *p.Total
		for i, member := range members {
			record, err := acceptanceCollectionUserRecordFromAPI(collection, member)
			if err != nil {
				return nil, err
			}
			u := users[i]
			if u.Id == nil || *u.Id != *member.UserId || u.Name == nil || u.Role == nil || !u.Role.Valid() || u.IsSuspended == nil || u.Email.IsSpecified() {
				return nil, fmt.Errorf("direct grant and public user disagree or presenter unexpectedly includes email")
			}
			id := member.UserId.String()
			if _, exists := result[id]; exists || seen[record.APIID] {
				return nil, fmt.Errorf("collections.memberships fixture repeated user or grant %s", id)
			}
			seen[record.APIID], result[id] = true, record
		}
		if len(result) == total {
			return result, nil
		}
		if len(members) != limit {
			return nil, fmt.Errorf("collections.memberships fixture: incomplete page before total")
		}
		offset += len(members)
	}
}

func (a *acceptanceAPI) acceptanceCollectionUserPermission(collection, user, permission string) error {
	members, err := a.acceptanceCollectionUsers(collection)
	if err != nil {
		return err
	}
	member, exists := members[user]
	if permission == "" && !exists {
		return nil
	}
	if !exists || permission == "" || member.Permission != permission {
		return fmt.Errorf("collection/user %s/%s: exists=%t, permission=%q, wanted %q", collection, user, exists, member.Permission, permission)
	}
	return nil
}

func (a *acceptanceAPI) acceptanceUpsertCollectionUser(collection, user, permission string) (acceptanceCollectionUserRecord, error) {
	role := client.Permission(permission)
	r, err := a.CollectionsAddUserWithResponse(context.Background(), client.CollectionsAddUserJSONRequestBody{Id: uuid.MustParse(collection), UserId: uuid.MustParse(user), Permission: &role})
	if err != nil {
		return acceptanceCollectionUserRecord{}, err
	}
	if r.StatusCode() != http.StatusOK || r.JSON200 == nil || r.JSON200.Data == nil || r.JSON200.Data.Memberships == nil || len(*r.JSON200.Data.Memberships) != 1 || r.JSON200.Data.Users == nil || len(*r.JSON200.Data.Users) != 1 {
		return acceptanceCollectionUserRecord{}, fmt.Errorf("collections.add_user fixture: HTTP %d: %s", r.StatusCode(), r.Body)
	}
	if err := checkCollectionGrantEnvelope("acceptance collections.add_user", r.JSON200.Ok, r.JSON200.Status); err != nil {
		return acceptanceCollectionUserRecord{}, err
	}
	member, u := (*r.JSON200.Data.Memberships)[0], (*r.JSON200.Data.Users)[0]
	written, err := acceptanceCollectionUserRecordFromAPI(collection, member)
	if err != nil {
		return acceptanceCollectionUserRecord{}, err
	}
	if member.UserId.String() != user || written.Permission != permission || u.Id == nil || u.Id.String() != user || u.Email.IsSpecified() {
		return acceptanceCollectionUserRecord{}, fmt.Errorf("collections.add_user fixture returned another pair, permission, or non-public user")
	}
	return written, nil
}

func (a *acceptanceAPI) acceptanceWriteCollectionUser(collection, user, permission string) error {
	written, err := a.acceptanceUpsertCollectionUser(collection, user, permission)
	if err != nil {
		return err
	}
	observed, err := a.acceptanceCollectionUsers(collection)
	if err != nil {
		return err
	}
	if observed[user] != written {
		return fmt.Errorf("collections.add_user response and subsequent listing disagree")
	}
	return nil
}

func (a *acceptanceAPI) acceptanceRemoveCollectionUser(collection, user string) error {
	r, err := a.CollectionsRemoveUserWithResponse(context.Background(), client.CollectionsRemoveUserJSONRequestBody{Id: uuid.MustParse(collection), UserId: uuid.MustParse(user)})
	if err != nil {
		return err
	}
	if r.StatusCode() != http.StatusOK || r.JSON200 == nil || r.JSON200.Success == nil || !*r.JSON200.Success {
		return fmt.Errorf("collections.remove_user fixture: HTTP %d: %s", r.StatusCode(), r.Body)
	}
	if err := checkCollectionGrantEnvelope("acceptance collections.remove_user", r.JSON200.Ok, r.JSON200.Status); err != nil {
		return err
	}
	return a.acceptanceCollectionUserPermission(collection, user, "")
}

func (a *acceptanceAPI) acceptanceCreateGrantUser(name string) (string, error) {
	users, err := a.acceptanceInviteUsers([]client.Invite{{Email: "collection-user-" + uuid.NewString() + "@example.invalid", Name: name, Role: client.UserRoleMember}})
	if err != nil {
		return "", err
	}
	if len(users) != 1 || users[0].Id == nil {
		return "", fmt.Errorf("collection user invitation did not return one user")
	}
	return users[0].Id.String(), nil
}

func (a *acceptanceAPI) acceptanceCollectionUserConfig(collection, user, permission string) string {
	return a.providerConfig + fmt.Sprintf(`
resource "outline_collection_user" "test" {
 collection_id = %q
 user_id = %q
 permission = %q
}
`, collection, user, permission)
}

func (a *acceptanceAPI) checkAcceptanceCollectionUser(address string) resource.TestCheckFunc {
	return func(state *terraform.State) error {
		r, exists := state.RootModule().Resources[address]
		if !exists || r.Primary == nil {
			return fmt.Errorf("%s has no collection user state", address)
		}
		attrs := r.Primary.Attributes
		for _, field := range []string{"collection_id", "user_id"} {
			id, err := uuid.Parse(attrs[field])
			if err != nil || id == uuid.Nil || id.String() != attrs[field] {
				return fmt.Errorf("%s must be a canonical nonzero UUID", field)
			}
		}
		id := attrs["collection_id"] + "/" + attrs["user_id"]
		if r.Primary.ID != id || attrs["id"] != id || strings.Count(id, "/") != 1 {
			return fmt.Errorf("collection user state must use collection_id/user_id, not the API membership UUID")
		}
		return a.acceptanceCollectionUserPermission(attrs["collection_id"], attrs["user_id"], attrs["permission"])
	}
}

type acceptanceCollectionUserMetadata struct {
	Name, Role string
	Suspended  bool
}
type acceptanceCollectionUserSnapshot struct {
	Name, Description, Permission string
	Sharing                       bool
	UserGrants                    map[string]acceptanceCollectionUserRecord
	Users                         map[string]acceptanceCollectionUserMetadata
	GroupGrants                   map[string]acceptanceCollectionGroupRecord
	Groups                        map[string]*client.Group
	GroupMembers                  map[string]map[string]acceptanceGroupMemberRecord
}

// Record all direct grants even for the 100+ user fixture. Compare every group
// grant, group member, and collection default without timestamp noise.
func (a *acceptanceAPI) acceptanceCollectionUserSnapshot(collection string, managed []string) (*acceptanceCollectionUserSnapshot, error) {
	r, err := a.CollectionsInfoWithResponse(context.Background(), client.CollectionsInfoJSONRequestBody{Id: uuid.MustParse(collection)})
	if err != nil {
		return nil, err
	}
	if r.StatusCode() != http.StatusOK || r.JSON200 == nil || r.JSON200.Data == nil {
		return nil, fmt.Errorf("collection user snapshot: HTTP %d: %s", r.StatusCode(), r.Body)
	}
	if err := checkEnvelope("acceptance collection user snapshot", r.JSON200.Ok, r.JSON200.Status); err != nil {
		return nil, err
	}
	c := r.JSON200.Data
	if c.Id == nil || c.Id.String() != collection || c.Name == nil || c.Sharing == nil ||
		!c.Description.IsSpecified() || !c.Permission.IsSpecified() || !c.ArchivedAt.IsSpecified() || !c.DeletedAt.IsNull() {
		return nil, fmt.Errorf("collection user snapshot returned incomplete metadata")
	}
	users, err := a.acceptanceCollectionUsers(collection)
	if err != nil {
		return nil, err
	}
	groups, err := a.acceptanceCollectionGroups(collection)
	if err != nil {
		return nil, err
	}
	snapshot := &acceptanceCollectionUserSnapshot{Name: *c.Name, Description: c.Description.GetOrEmpty(), Permission: "null", Sharing: *c.Sharing, UserGrants: users, GroupGrants: groups, Users: map[string]acceptanceCollectionUserMetadata{}, Groups: map[string]*client.Group{}, GroupMembers: map[string]map[string]acceptanceGroupMemberRecord{}}
	if !c.Permission.IsNull() {
		snapshot.Permission = string(c.Permission.GetOrEmpty())
	}
	for _, id := range managed {
		delete(snapshot.UserGrants, id)
		u, err := a.acceptanceUser(id)
		if err != nil {
			return nil, err
		}
		snapshot.Users[id] = acceptanceCollectionUserMetadata{Name: *u.Name, Role: string(*u.Role), Suspended: *u.IsSuspended}
	}
	for id := range groups {
		snapshot.Groups[id], err = a.acceptanceGroup(id)
		if err != nil {
			return nil, err
		}
		snapshot.GroupMembers[id], _, err = a.acceptanceGroupMembers(id)
		if err != nil {
			return nil, err
		}
	}
	return snapshot, nil
}

func (a *acceptanceAPI) acceptanceCollectionUserCollateral(collection string, managed []string, baseline *acceptanceCollectionUserSnapshot) error {
	actual, err := a.acceptanceCollectionUserSnapshot(collection, managed)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(actual, baseline) {
		before, _ := json.Marshal(baseline)
		after, _ := json.Marshal(actual)
		return fmt.Errorf("collection user operation changed unrelated grants, defaults, user metadata, groups, or memberships: before=%s after=%s", before, after)
	}
	return nil
}

func acceptanceCollectionUserParents(t *testing.T, api *acceptanceAPI, name string) (string, string) {
	t.Helper()
	collection, err := api.acceptanceCreateCollection("Terraform collection user " + name)
	if err != nil {
		t.Fatal(err)
	}
	user, err := api.acceptanceCreateGrantUser("Terraform collection user " + name)
	if err != nil {
		t.Fatal(err)
	}
	group, err := api.acceptanceCreateGroup("Terraform collection user collateral " + name)
	if err != nil {
		t.Fatal(err)
	}
	if err := api.acceptanceWriteGroupMember(group, user, client.GroupPermissionMember, true); err != nil {
		t.Fatal(err)
	}
	if err := api.acceptanceWriteCollectionGroup(collection, group, "read_write"); err != nil {
		t.Fatal(err)
	}
	return collection, user
}

// One stack covers all direct-grant cases and the integrated IAM graph.
func TestAccCollectionUser(t *testing.T) {
	api := newAcceptanceAPI(t)
	api.acceptanceCollectionAnchor(t)
	t.Run("lifecycle and drift", func(t *testing.T) { testAccCollectionUserLifecycle(t, api) })
	t.Run("pair replacement", func(t *testing.T) { testAccCollectionUserReplacement(t, api) })
	t.Run("existing explicit pair", func(t *testing.T) { testAccCollectionUserExistingPair(t, api) })
	t.Run("multi-page direct grants", func(t *testing.T) { testAccCollectionUserPagination(t, api) })
	t.Run("missing grant and parents", func(t *testing.T) { testAccCollectionUserMissing(t, api) })
	t.Run("direct and group effective access", func(t *testing.T) { testAccCollectionUserGroupAccess(t, api) })
	t.Run("target roles and inherited access", func(t *testing.T) { testAccCollectionUserRoles(t, api) })
	t.Run("last stored collection admin", func(t *testing.T) { testAccCollectionUserLastManager(t, api) })
	t.Run("archived retains state", func(t *testing.T) { testAccCollectionUserArchived(t, api) })
	t.Run("owner and creator protection", func(t *testing.T) { testAccCollectionUserOwner(t, api) })
	t.Run("integrated IAM", func(t *testing.T) { testAccIAM(t, api) })
	// Foreign workspace creation is last because the key fixture requires one team.
	t.Run("forbidden retains state", func(t *testing.T) { testAccCollectionUserForbidden(t, api) })
}

func testAccCollectionUserLifecycle(t *testing.T, api *acceptanceAPI) {
	collection, other := acceptanceCollectionUserParents(t, api, "lifecycle")
	// Grant management must also leave the release's admin default intact,
	// even though outline_collection cannot manage that default itself.
	if err := api.acceptanceUpdateCollection(client.CollectionsUpdateJSONRequestBody{
		Id: uuid.MustParse(collection), Permission: nullable.NewNullableWithValue(client.PermissionAdmin),
	}); err != nil {
		t.Fatal(err)
	}
	if err := api.acceptanceWriteCollectionUser(collection, other, "admin"); err != nil {
		t.Fatal(err)
	}
	var err error
	users := make([]string, len(acceptanceCollectionUserRoles))
	for i, role := range acceptanceCollectionUserRoles {
		users[i], err = api.acceptanceCreateGrantUser("Terraform collection user lifecycle " + role)
		if err != nil {
			t.Fatal(err)
		}

	}
	baseline, err := api.acceptanceCollectionUserSnapshot(collection, users)
	if err != nil {
		t.Fatal(err)
	}
	// The same user also has a grant on another collection. Pair operations
	// must not search or delete by user ID alone.
	cross, err := api.acceptanceCreateCollection("Terraform collection user other collection")
	if err != nil {
		t.Fatal(err)
	}
	if err := api.acceptanceWriteCollectionUser(cross, users[0], "read_write"); err != nil {
		t.Fatal(err)
	}
	crossBaseline, err := api.acceptanceCollectionUsers(cross)
	if err != nil {
		t.Fatal(err)
	}
	collateral := func(_ *terraform.State) error {
		if err := api.acceptanceCollectionUserCollateral(collection, users, baseline); err != nil {
			return err
		}
		crossGrants, err := api.acceptanceCollectionUsers(cross)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(crossGrants, crossBaseline) {
			return fmt.Errorf("pair operation changed another collection's grants")
		}
		return nil
	}
	address := func(role string) string { return "outline_collection_user." + role }
	config := func(rotate bool) string {
		var resources strings.Builder
		resources.WriteString(api.providerConfig)
		for i, role := range acceptanceCollectionUserRoles {
			permission := role
			if rotate {
				permission = acceptanceCollectionUserRoles[(i+1)%len(users)]
			}
			fmt.Fprintf(&resources, `
resource "outline_collection_user" %q {
  collection_id = %q
  user_id = %q
  permission = %q
}
`, role, collection, users[i], permission)
		}
		return resources.String()
	}
	apiIDs := make(map[string]string)
	check := func(rotate bool) resource.TestCheckFunc {
		var checks []resource.TestCheckFunc
		for i, role := range acceptanceCollectionUserRoles {
			permission := role
			if rotate {
				permission = acceptanceCollectionUserRoles[(i+1)%len(users)]
			}
			checks = append(checks, api.checkAcceptanceCollectionUser(address(role)),
				resource.TestCheckResourceAttr(address(role), "id", collection+"/"+users[i]),
				resource.TestCheckResourceAttr(address(role), "permission", permission))
		}
		checks = append(checks, collateral, func(_ *terraform.State) error {
			grants, err := api.acceptanceCollectionUsers(collection)
			if err != nil {
				return err
			}
			for _, user := range users {
				if previous := apiIDs[user]; previous != "" && previous != grants[user].APIID {
					return fmt.Errorf("permission update recreated API membership for user %s", user)
				}
				apiIDs[user] = grants[user].APIID
			}
			return nil
		})
		return resource.ComposeAggregateTestCheckFunc(checks...)
	}
	steps := []resource.TestStep{{Config: config(false), Check: check(false)}}
	for _, role := range acceptanceCollectionUserRoles {
		steps = append(steps, resource.TestStep{ResourceName: address(role), ImportState: true, ImportStateVerify: true})
	}
	steps = append(steps,
		resource.TestStep{Config: config(false), ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}}, Check: check(false)},
		resource.TestStep{Config: config(true), ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
			plancheck.ExpectResourceAction(address("read"), plancheck.ResourceActionUpdate),
			plancheck.ExpectResourceAction(address("read_write"), plancheck.ResourceActionUpdate),
			plancheck.ExpectResourceAction(address("admin"), plancheck.ResourceActionUpdate),
		}}, Check: check(true)},
		resource.TestStep{PreConfig: func() {
			if err := api.acceptanceWriteCollectionUser(collection, users[0], "admin"); err != nil {
				t.Fatal(err)
			}
		}, RefreshState: true, ExpectNonEmptyPlan: true, Check: resource.ComposeAggregateTestCheckFunc(
			api.checkAcceptanceCollectionUser(address("read")), resource.TestCheckResourceAttr(address("read"), "permission", "admin"), collateral)},
		resource.TestStep{Config: config(true), ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
			plancheck.ExpectResourceAction(address("read"), plancheck.ResourceActionUpdate),
		}}, Check: check(true)},
		resource.TestStep{Config: api.providerConfig, Check: func(state *terraform.State) error {
			for _, user := range users {
				if err := api.acceptanceCollectionUserPermission(collection, user, ""); err != nil {
					return err
				}
			}
			return collateral(state)
		}},
	)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acceptanceProviderFactories(),
		CheckDestroy: func(state *terraform.State) error {
			for _, user := range users {
				if err := api.acceptanceCollectionUserPermission(collection, user, ""); err != nil {
					return err
				}
			}
			return collateral(state)
		},
		Steps: steps,
	})
}

func testAccCollectionUserExistingPair(t *testing.T, api *acceptanceAPI) {
	collection, user := acceptanceCollectionUserParents(t, api, "existing")
	if err := api.acceptanceWriteCollectionUser(collection, user, "admin"); err != nil {
		t.Fatal(err)
	}
	before, err := api.acceptanceCollectionUsers(collection)
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := api.acceptanceCollectionUserSnapshot(collection, []string{user})
	if err != nil {
		t.Fatal(err)
	}
	retained := func() {
		t.Helper()
		after, err := api.acceptanceCollectionUsers(collection)
		if err != nil || !reflect.DeepEqual(before, after) {
			t.Fatalf("failed Create adopted or changed an existing explicit pair: before=%v after=%v err=%v", before, after, err)
		}
		if err := api.acceptanceCollectionUserCollateral(collection, []string{user}, baseline); err != nil {
			t.Fatal(err)
		}
	}
	var steps []resource.TestStep
	for _, role := range acceptanceCollectionUserRoles {
		steps = append(steps, resource.TestStep{
			PreConfig: retained, Config: api.acceptanceCollectionUserConfig(collection, user, role),
			ExpectError: regexp.MustCompile(`(?is)already exists.*import.*` + regexp.QuoteMeta(collection+"/"+user)),
		})
	}
	config := api.acceptanceCollectionUserConfig(collection, user, "admin")
	steps = append(steps,
		resource.TestStep{PreConfig: retained, Config: config, ResourceName: acceptanceCollectionUserAddress,
			ImportState: true, ImportStatePersist: true, ImportStateId: collection + "/" + user,
			ImportStateCheck: func(states []*terraform.InstanceState) error {
				if len(states) != 1 || states[0].ID != collection+"/"+user || states[0].Attributes["permission"] != "admin" ||
					states[0].Attributes["collection_id"] != collection || states[0].Attributes["user_id"] != user {
					return fmt.Errorf("import did not read the existing explicit grant: %+v", states)
				}
				return nil
			}},
		resource.TestStep{Config: config, ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}},
			Check: api.checkAcceptanceCollectionUser(acceptanceCollectionUserAddress)},
		resource.TestStep{Config: api.providerConfig, Check: func(_ *terraform.State) error {
			if err := api.acceptanceCollectionUserPermission(collection, user, ""); err != nil {
				return err
			}
			return api.acceptanceCollectionUserCollateral(collection, []string{user}, baseline)
		}},
	)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acceptanceProviderFactories(),
		CheckDestroy:             func(_ *terraform.State) error { return api.acceptanceCollectionUserPermission(collection, user, "") },
		Steps:                    steps,
	})
}

func testAccCollectionUserPagination(t *testing.T, api *acceptanceAPI) {
	collection, target := acceptanceCollectionUserParents(t, api, "pagination target")
	if err := api.acceptanceUpdateCollection(client.CollectionsUpdateJSONRequestBody{
		Id: uuid.MustParse(collection), Permission: nullable.NewNullableWithValue(client.PermissionReadWrite),
	}); err != nil {
		t.Fatal(err)
	}
	// The release orders grants by createdAt DESC. Insert the managed pair
	// first, then 101 others, so matching it requires the final page.
	seeded, err := api.acceptanceCollectionUsers(collection)
	if err != nil {
		t.Fatal(err)
	}
	seeded[target], err = api.acceptanceUpsertCollectionUser(collection, target, "read")
	if err != nil {
		t.Fatal(err)
	}
	for i := range 101 {
		user, err := api.acceptanceCreateGrantUser(fmt.Sprintf("Terraform collection user pagination %03d", i))
		if err != nil {
			t.Fatal(err)
		}
		seeded[user], err = api.acceptanceUpsertCollectionUser(collection, user, acceptanceCollectionUserRoles[i%len(acceptanceCollectionUserRoles)])
		if err != nil {
			t.Fatal(err)
		}
	}
	observed, err := api.acceptanceCollectionUsers(collection)
	if err != nil || !reflect.DeepEqual(observed, seeded) {
		t.Fatalf("seeded 103 explicit grants do not match the complete HTTP listing: err=%v seeded=%v observed=%v", err, seeded, observed)
	}
	limit := 100
	for _, offset := range []int{0, 100} {
		r, err := api.CollectionsMembershipsWithResponse(t.Context(), client.CollectionsMembershipsJSONRequestBody{
			Id: uuid.MustParse(collection), Limit: &limit, Offset: &offset,
		})
		if err != nil {
			t.Fatal(err)
		}
		if r.StatusCode() != http.StatusOK || r.JSON200 == nil || r.JSON200.Data == nil ||
			r.JSON200.Data.Memberships == nil || r.JSON200.Pagination == nil || r.JSON200.Pagination.Total == nil ||
			*r.JSON200.Pagination.Total != 103 || r.JSON200.Pagination.Offset == nil || *r.JSON200.Pagination.Offset != offset {
			t.Fatalf("pagination seed: HTTP %d: %s", r.StatusCode(), r.Body)
		}
		if err := checkCollectionGrantEnvelope("acceptance pagination seed", r.JSON200.Ok, r.JSON200.Status); err != nil {
			t.Fatal(err)
		}
		members := *r.JSON200.Data.Memberships
		wantCount := 100
		if offset == 100 {
			wantCount = 3
		}
		if len(members) != wantCount {
			t.Fatalf("offset %d returned %d grants, expected %d", offset, len(members), wantCount)
		}
		for i, member := range members {
			if _, err := acceptanceCollectionUserRecordFromAPI(collection, member); err != nil {
				t.Fatal(err)
			}
			if (member.UserId.String() == target) != (offset == 100 && i == 1) {
				t.Fatalf("target must be the second-last of 103 createdAt DESC grants, got offset %d index %d user %s", offset, i, member.UserId)
			}
		}
	}
	baseline, err := api.acceptanceCollectionUserSnapshot(collection, []string{target})
	if err != nil {
		t.Fatal(err)
	}
	if len(baseline.UserGrants) != 102 || baseline.Permission != "read_write" {
		t.Fatalf("pagination collateral baseline must contain 101 unrelated grants, creator grant, and read_write default: %+v", baseline)
	}
	collateral := func() error {
		actual, err := api.acceptanceCollectionUserSnapshot(collection, []string{target})
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(actual, baseline) {
			return fmt.Errorf("paginated pair operation changed one of the 101 unrelated grants, user grants, or collection defaults: before=%+v after=%+v", baseline, actual)
		}
		return nil
	}
	check := func(permission string) resource.TestCheckFunc {
		return resource.ComposeAggregateTestCheckFunc(
			api.checkAcceptanceCollectionUser(acceptanceCollectionUserAddress),
			resource.TestCheckResourceAttr(acceptanceCollectionUserAddress, "id", collection+"/"+target),
			resource.TestCheckResourceAttr(acceptanceCollectionUserAddress, "permission", permission),
			func(_ *terraform.State) error {
				grants, err := api.acceptanceCollectionUsers(collection)
				if err != nil {
					return err
				}
				if len(grants) != 103 || grants[target].APIID != seeded[target].APIID || grants[target].Permission != permission {
					return fmt.Errorf("paginated read/update changed pair identity or failed to read its permission: %+v", grants[target])
				}
				return collateral()
			},
		)
	}
	retained := func() {
		t.Helper()
		grants, err := api.acceptanceCollectionUsers(collection)
		if err != nil || !reflect.DeepEqual(grants, seeded) {
			t.Fatalf("refused Create changed the existing paginated grant: err=%v before=%v after=%v", err, seeded, grants)
		}
		if err := collateral(); err != nil {
			t.Fatal(err)
		}
	}
	deleted := func(_ *terraform.State) error {
		if err := api.acceptanceCollectionUserPermission(collection, target, ""); err != nil {
			return err
		}
		return collateral()
	}
	readConfig := api.acceptanceCollectionUserConfig(collection, target, "read")
	writeConfig := api.acceptanceCollectionUserConfig(collection, target, "read_write")
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acceptanceProviderFactories(), CheckDestroy: deleted,
		Steps: []resource.TestStep{
			{Config: api.acceptanceCollectionUserConfig(collection, target, "admin"),
				ExpectError: regexp.MustCompile(`(?is)already exists.*import.*` + regexp.QuoteMeta(collection+"/"+target))},
			{PreConfig: retained, Config: readConfig, ResourceName: acceptanceCollectionUserAddress,
				ImportState: true, ImportStatePersist: true, ImportStateId: collection + "/" + target,
				ImportStateCheck: func(states []*terraform.InstanceState) error {
					if len(states) != 1 || states[0].ID != collection+"/"+target || states[0].Attributes["permission"] != "read" ||
						states[0].Attributes["collection_id"] != collection || states[0].Attributes["user_id"] != target {
						return fmt.Errorf("import did not read the explicit grant on the final page: %+v", states)
					}
					return collateral()
				}},
			{Config: readConfig, ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}}, Check: check("read")},
			{ResourceName: acceptanceCollectionUserAddress, ImportState: true, ImportStateVerify: true},
			{Config: writeConfig, ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
				plancheck.ExpectResourceAction(acceptanceCollectionUserAddress, plancheck.ResourceActionUpdate),
			}}, Check: check("read_write")},
			{PreConfig: func() {
				if err := api.acceptanceWriteCollectionUser(collection, target, "admin"); err != nil {
					t.Fatal(err)
				}
			}, RefreshState: true, ExpectNonEmptyPlan: true, Check: check("admin")},
			{Config: writeConfig, ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
				plancheck.ExpectResourceAction(acceptanceCollectionUserAddress, plancheck.ResourceActionUpdate),
			}}, Check: check("read_write")},
			{Config: api.providerConfig, ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
				plancheck.ExpectResourceAction(acceptanceCollectionUserAddress, plancheck.ResourceActionDestroy),
			}}, Check: deleted},
		},
	})
	// 102 untouched grants remain, so absent-pair confirmation still has two
	// real pages to validate after Terraform deletes the managed pair.
	acceptanceCollectionUserAbsentRemoval(t, api, collection, target)
	if err := collateral(); err != nil {
		t.Fatal(err)
	}
}

func testAccCollectionUserMissing(t *testing.T, api *acceptanceAPI) {
	for _, missing := range []string{"grant", "collection", "user"} {
		t.Run(missing, func(t *testing.T) {
			collection, user := acceptanceCollectionUserParents(t, api, "missing read "+missing)
			deleteCollection, deleteUser := acceptanceCollectionUserParents(t, api, "missing delete "+missing)
			remove := func(collection, user string) error {
				switch missing {
				case "collection":
					return api.acceptanceDeleteCollection(collection)
				case "user":
					return api.acceptanceDeleteUser(user)
				default:
					return api.acceptanceRemoveCollectionUser(collection, user)
				}
			}
			absent := func(collection, user string) error {
				if missing == "collection" {
					return api.acceptanceCollectionAbsent(collection)
				}
				if missing == "user" {
					if err := api.acceptanceUserAbsent(user); err != nil {
						return err
					}
				}
				return api.acceptanceCollectionUserPermission(collection, user, "")
			}
			config := api.acceptanceCollectionUserConfig(collection, user, "read_write")
			steps := []resource.TestStep{
				{Config: config, Check: api.checkAcceptanceCollectionUser(acceptanceCollectionUserAddress)},
				{PreConfig: func() {
					if err := remove(collection, user); err != nil {
						t.Fatal(err)
					}
				}, RefreshState: true, ExpectNonEmptyPlan: true, Check: func(state *terraform.State) error {
					if _, exists := state.RootModule().Resources[acceptanceCollectionUserAddress]; exists {
						return fmt.Errorf("missing %s did not remove collection user state", missing)
					}
					return absent(collection, user)
				}},
			}
			if missing == "grant" {
				steps = append(steps, resource.TestStep{Config: config, ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(acceptanceCollectionUserAddress, plancheck.ResourceActionCreate),
				}}, Check: api.checkAcceptanceCollectionUser(acceptanceCollectionUserAddress)})
			}
			steps = append(steps,
				resource.TestStep{Config: api.providerConfig},
				resource.TestStep{Config: api.acceptanceCollectionUserConfig(deleteCollection, deleteUser, "admin"),
					Check: api.checkAcceptanceCollectionUser(acceptanceCollectionUserAddress)},
				resource.TestStep{Config: api.providerConfig, ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(acceptanceCollectionUserAddress, plancheck.ResourceActionDestroy),
					acceptanceCollectionGroupBeforeApply(func() error {
						if err := remove(deleteCollection, deleteUser); err != nil {
							return err
						}
						return absent(deleteCollection, deleteUser)
					}),
				}}, Check: func(_ *terraform.State) error { return absent(deleteCollection, deleteUser) }},
			)
			resource.Test(t, resource.TestCase{
				ProtoV6ProviderFactories: acceptanceProviderFactories(),
				CheckDestroy: func(_ *terraform.State) error {
					if err := absent(collection, user); err != nil {
						return err
					}
					return absent(deleteCollection, deleteUser)
				}, Steps: steps,
			})
		})
	}
}

func testAccCollectionUserReplacement(t *testing.T, api *acceptanceAPI) {
	collection, user := acceptanceCollectionUserParents(t, api, "replacement first")
	otherCollection, otherUser := acceptanceCollectionUserParents(t, api, "replacement second")
	managed := []string{user, otherUser}
	baselines := make(map[string]*acceptanceCollectionUserSnapshot)
	for _, id := range []string{collection, otherCollection} {
		baseline, err := api.acceptanceCollectionUserSnapshot(id, managed)
		if err != nil {
			t.Fatal(err)
		}
		baselines[id] = baseline
	}
	check := func(activeCollection, activeUser string) resource.TestCheckFunc {
		return func(state *terraform.State) error {
			if activeCollection != "" {
				if err := api.checkAcceptanceCollectionUser(acceptanceCollectionUserAddress)(state); err != nil {
					return err
				}
			}
			for _, id := range []string{collection, otherCollection} {
				for _, target := range managed {
					permission := ""
					if id == activeCollection && target == activeUser {
						permission = "read"
					}
					if err := api.acceptanceCollectionUserPermission(id, target, permission); err != nil {
						return err
					}
				}
				if err := api.acceptanceCollectionUserCollateral(id, managed, baselines[id]); err != nil {
					return err
				}
			}
			return nil
		}
	}
	replacement := resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(acceptanceCollectionUserAddress, plancheck.ResourceActionDestroyBeforeCreate)}}
	resource.Test(t, resource.TestCase{ProtoV6ProviderFactories: acceptanceProviderFactories(), CheckDestroy: check("", ""), Steps: []resource.TestStep{
		{Config: api.acceptanceCollectionUserConfig(collection, user, "read"), Check: check(collection, user)},
		{Config: api.acceptanceCollectionUserConfig(collection, otherUser, "read"), ConfigPlanChecks: replacement, Check: check(collection, otherUser)},
		{Config: api.acceptanceCollectionUserConfig(otherCollection, otherUser, "read"), ConfigPlanChecks: replacement, Check: check(otherCollection, otherUser)},
		{ResourceName: acceptanceCollectionUserAddress, ImportState: true, ImportStateVerify: true},
		{Config: api.providerConfig, Check: check("", "")},
	}})
}
