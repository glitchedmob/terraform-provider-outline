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

const acceptanceCollectionGroupAddress = "outline_collection_group.test"

var acceptanceCollectionGroupRoles = []string{"read", "read_write", "admin"}

type acceptanceCollectionGroupRecord struct {
	APIID, Permission string
}

func acceptanceCollectionGroupRecordFromAPI(collection string, m client.GroupMembership) (acceptanceCollectionGroupRecord, error) {
	if m.Id == nil || m.GroupId == nil || *m.GroupId == uuid.Nil ||
		!m.CollectionId.IsSpecified() || m.CollectionId.IsNull() || m.CollectionId.GetOrEmpty().String() != collection ||
		!m.DocumentId.IsSpecified() || !m.DocumentId.IsNull() || !m.SourceId.IsSpecified() || !m.SourceId.IsNull() ||
		m.Permission == nil || !m.Permission.Valid() {
		return acceptanceCollectionGroupRecord{}, fmt.Errorf("collection group fixture returned an incomplete or non-explicit grant")
	}
	id, err := uuid.Parse(*m.Id)
	if err != nil || id == uuid.Nil || id.String() != *m.Id {
		return acceptanceCollectionGroupRecord{}, fmt.Errorf("collection group API grant must have its own canonical UUID")
	}
	return acceptanceCollectionGroupRecord{APIID: *m.Id, Permission: string(*m.Permission)}, nil
}

// The released listing is the oracle. Do not use the provider's pair lookup,
// effective collection policies, groups.list previews, or its membership decoder.
func (a *acceptanceAPI) acceptanceCollectionGroups(collection string) (map[string]acceptanceCollectionGroupRecord, error) {
	result := make(map[string]acceptanceCollectionGroupRecord)
	limit, offset, total := 100, 0, -1
	for {
		r, err := a.CollectionsGroupMembershipsWithResponse(context.Background(), client.CollectionsGroupMembershipsJSONRequestBody{
			Id: uuid.MustParse(collection), Limit: &limit, Offset: &offset,
		})
		if err != nil {
			return nil, err
		}
		if r.StatusCode() != http.StatusOK || r.JSON200 == nil || r.JSON200.Data == nil ||
			r.JSON200.Data.GroupMemberships == nil || r.JSON200.Data.Groups == nil {
			return nil, fmt.Errorf("collections.group_memberships fixture: HTTP %d: %s", r.StatusCode(), r.Body)
		}
		page := r.JSON200
		if err := checkEnvelope("acceptance collections.group_memberships", page.Ok, page.Status); err != nil {
			return nil, err
		}
		members, groups, p := *page.Data.GroupMemberships, *page.Data.Groups, page.Pagination
		if p == nil || p.Limit == nil || *p.Limit != limit || p.Offset == nil || *p.Offset != offset ||
			p.Total == nil || *p.Total < 0 || total != -1 && total != *p.Total || len(members) != len(groups) ||
			len(members) > limit || offset+len(members) > *p.Total {
			return nil, fmt.Errorf("collections.group_memberships fixture: inconsistent arrays or pagination")
		}
		total = *p.Total
		for i, member := range members {
			record, err := acceptanceCollectionGroupRecordFromAPI(collection, member)
			if err != nil {
				return nil, err
			}
			if groups[i].Id == nil || *groups[i].Id != *member.GroupId || groups[i].Name == nil {
				return nil, fmt.Errorf("collection grant and listed group disagree")
			}
			id := member.GroupId.String()
			if _, exists := result[id]; exists {
				return nil, fmt.Errorf("collections.group_memberships fixture repeated group %s", id)
			}
			result[id] = record
		}
		if len(result) == total {
			return result, nil
		}
		if len(members) != limit {
			return nil, fmt.Errorf("collections.group_memberships fixture: incomplete page before total")
		}
		offset += len(members)
	}
}

func (a *acceptanceAPI) acceptanceCollectionGroupPermission(collection, group, permission string) error {
	members, err := a.acceptanceCollectionGroups(collection)
	if err != nil {
		return err
	}
	member, exists := members[group]
	if permission == "" && !exists {
		return nil
	}
	if !exists || permission == "" || member.Permission != permission {
		return fmt.Errorf("collection/group %s/%s: exists=%t, permission=%q, wanted %q", collection, group, exists, member.Permission, permission)
	}
	return nil
}

// Outline 1.10.1 has no separate update_group route. add_group is an upsert.
// Bulk seeding validates each write, then lists once instead of after every row.
func (a *acceptanceAPI) acceptanceUpsertCollectionGroup(collection, group, permission string) (acceptanceCollectionGroupRecord, error) {
	role := client.Permission(permission)
	r, err := a.CollectionsAddGroupWithResponse(context.Background(), client.CollectionsAddGroupJSONRequestBody{
		Id: uuid.MustParse(collection), GroupId: uuid.MustParse(group), Permission: &role,
	})
	if err != nil {
		return acceptanceCollectionGroupRecord{}, err
	}
	if r.StatusCode() != http.StatusOK || r.JSON200 == nil || r.JSON200.Data == nil ||
		r.JSON200.Data.GroupMemberships == nil || len(*r.JSON200.Data.GroupMemberships) != 1 {
		return acceptanceCollectionGroupRecord{}, fmt.Errorf("collections.add_group fixture: HTTP %d: %s", r.StatusCode(), r.Body)
	}
	if err := checkEnvelope("acceptance collections.add_group", r.JSON200.Ok, r.JSON200.Status); err != nil {
		return acceptanceCollectionGroupRecord{}, err
	}
	member := (*r.JSON200.Data.GroupMemberships)[0]
	written, err := acceptanceCollectionGroupRecordFromAPI(collection, member)
	if err != nil {
		return acceptanceCollectionGroupRecord{}, err
	}
	if member.GroupId.String() != group || written.Permission != permission {
		return acceptanceCollectionGroupRecord{}, fmt.Errorf("collections.add_group fixture returned another pair or permission")
	}
	return written, nil
}

func (a *acceptanceAPI) acceptanceWriteCollectionGroup(collection, group, permission string) error {
	written, err := a.acceptanceUpsertCollectionGroup(collection, group, permission)
	if err != nil {
		return err
	}
	observed, err := a.acceptanceCollectionGroups(collection)
	if err != nil {
		return err
	}
	if observed[group] != written {
		return fmt.Errorf("collections.add_group response and subsequent listing disagree")
	}
	return nil
}

func (a *acceptanceAPI) acceptanceRemoveCollectionGroup(collection, group string) error {
	r, err := a.CollectionsRemoveGroupWithResponse(context.Background(), client.CollectionsRemoveGroupJSONRequestBody{
		Id: uuid.MustParse(collection), GroupId: uuid.MustParse(group),
	})
	if err != nil {
		return err
	}
	if r.StatusCode() != http.StatusOK || r.JSON200 == nil || r.JSON200.Success == nil || !*r.JSON200.Success {
		return fmt.Errorf("collections.remove_group fixture: HTTP %d: %s", r.StatusCode(), r.Body)
	}
	if err := checkEnvelope("acceptance collections.remove_group", r.JSON200.Ok, r.JSON200.Status); err != nil {
		return err
	}
	return a.acceptanceCollectionGroupPermission(collection, group, "")
}

func (a *acceptanceAPI) acceptanceCollectionGroupConfig(collection, group, permission string) string {
	return a.providerConfig + fmt.Sprintf(`
resource "outline_collection_group" "test" {
  collection_id = %q
  group_id = %q
  permission = %q
}
`, collection, group, permission)
}

func (a *acceptanceAPI) checkAcceptanceCollectionGroup(address string) resource.TestCheckFunc {
	return func(state *terraform.State) error {
		r, exists := state.RootModule().Resources[address]
		if !exists || r.Primary == nil {
			return fmt.Errorf("%s has no collection group state", address)
		}
		attrs := r.Primary.Attributes
		for _, field := range []string{"collection_id", "group_id"} {
			id, err := uuid.Parse(attrs[field])
			if err != nil || id == uuid.Nil || id.String() != attrs[field] {
				return fmt.Errorf("%s must be a canonical nonzero UUID", field)
			}
		}
		id := attrs["collection_id"] + "/" + attrs["group_id"]
		if r.Primary.ID != id || attrs["id"] != id || strings.Count(id, "/") != 1 {
			return fmt.Errorf("collection group state must use collection_id/group_id, not the API membership UUID")
		}
		return a.acceptanceCollectionGroupPermission(attrs["collection_id"], attrs["group_id"], attrs["permission"])
	}
}

type acceptanceCollectionGroupSnapshot struct {
	Name, Description, Permission string
	Sharing                       bool
	UserGrants                    map[string]client.Membership
	GroupGrants                   map[string]acceptanceCollectionGroupRecord
	Groups                        map[string]*client.Group
	GroupMembers                  map[string]map[string]acceptanceGroupMemberRecord
}

// The bulk test compares every grant and the collection defaults without
// issuing groups.info/groups.memberships requests for all 102 groups.
func (a *acceptanceAPI) acceptanceCollectionGrantSnapshot(collection string, managed []string) (*acceptanceCollectionGroupSnapshot, error) {
	r, err := a.CollectionsInfoWithResponse(context.Background(), client.CollectionsInfoJSONRequestBody{Id: uuid.MustParse(collection)})
	if err != nil {
		return nil, err
	}
	if r.StatusCode() != http.StatusOK || r.JSON200 == nil || r.JSON200.Data == nil {
		return nil, fmt.Errorf("collection snapshot: HTTP %d: %s", r.StatusCode(), r.Body)
	}
	if err := checkEnvelope("acceptance collection snapshot", r.JSON200.Ok, r.JSON200.Status); err != nil {
		return nil, err
	}
	c := r.JSON200.Data
	if c.Id == nil || c.Id.String() != collection || c.Name == nil || c.Sharing == nil ||
		!c.Description.IsSpecified() || !c.Permission.IsSpecified() || !c.DeletedAt.IsNull() {
		return nil, fmt.Errorf("collection snapshot returned incomplete metadata")
	}
	grants, err := a.acceptanceCollectionGroups(collection)
	if err != nil {
		return nil, err
	}
	snapshot := &acceptanceCollectionGroupSnapshot{
		Name: *c.Name, Description: c.Description.GetOrEmpty(), Permission: "null", Sharing: *c.Sharing,
		UserGrants: make(map[string]client.Membership), GroupGrants: grants,
		Groups: make(map[string]*client.Group), GroupMembers: make(map[string]map[string]acceptanceGroupMemberRecord),
	}
	if !c.Permission.IsNull() {
		snapshot.Permission = string(c.Permission.GetOrEmpty())
	}
	limit, offset := 100, 0
	users, err := a.CollectionsMembershipsWithResponse(context.Background(), client.CollectionsMembershipsJSONRequestBody{
		Id: uuid.MustParse(collection), Limit: &limit, Offset: &offset,
	})
	if err != nil {
		return nil, err
	}
	if users.StatusCode() != http.StatusOK || users.JSON200 == nil || users.JSON200.Data == nil ||
		users.JSON200.Data.Memberships == nil || users.JSON200.Pagination == nil || users.JSON200.Pagination.Total == nil ||
		*users.JSON200.Pagination.Total != len(*users.JSON200.Data.Memberships) || *users.JSON200.Pagination.Total > limit {
		return nil, fmt.Errorf("collection user-grant fixture must fit on one complete page: HTTP %d: %s", users.StatusCode(), users.Body)
	}
	if err := checkEnvelope("acceptance collection user snapshot", users.JSON200.Ok, users.JSON200.Status); err != nil {
		return nil, err
	}
	for _, member := range *users.JSON200.Data.Memberships {
		if member.Id == nil || member.UserId == nil || member.Permission == nil ||
			member.CollectionId.IsNull() || member.CollectionId.GetOrEmpty().String() != collection {
			return nil, fmt.Errorf("collection user snapshot returned an incomplete grant")
		}
		snapshot.UserGrants[member.UserId.String()] = member
	}
	for _, group := range managed {
		delete(snapshot.GroupGrants, group)
	}
	return snapshot, nil
}

// Small fixtures also compare group names, external links, and every member.
// Workspace-user timestamps are excluded because requests update activity.
func (a *acceptanceAPI) acceptanceCollectionGroupSnapshot(collection string, managed []string) (*acceptanceCollectionGroupSnapshot, error) {
	snapshot, err := a.acceptanceCollectionGrantSnapshot(collection, managed)
	if err != nil {
		return nil, err
	}
	groupIDs := make(map[string]bool)
	for group := range snapshot.GroupGrants {
		groupIDs[group] = true
	}
	for _, group := range managed {
		groupIDs[group] = true
	}
	for group := range groupIDs {
		snapshot.Groups[group], err = a.acceptanceGroup(group)
		if err != nil {
			return nil, err
		}
		snapshot.GroupMembers[group], _, err = a.acceptanceGroupMembers(group)
		if err != nil {
			return nil, err
		}
	}
	return snapshot, nil
}

func (a *acceptanceAPI) acceptanceCollectionGroupCollateral(collection string, managed []string, baseline *acceptanceCollectionGroupSnapshot) error {
	actual, err := a.acceptanceCollectionGroupSnapshot(collection, managed)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(actual, baseline) {
		before, _ := json.Marshal(baseline)
		after, _ := json.Marshal(actual)
		return fmt.Errorf("collection group operation changed unrelated grants, defaults, group metadata, or memberships: before=%s after=%s", before, after)
	}
	return nil
}

// Apply-time mutations must happen after Terraform saves its plan. Otherwise
// refresh would remove the missing resource before Delete gets to handle it.
type acceptanceCollectionGroupBeforeApply func() error

func (f acceptanceCollectionGroupBeforeApply) CheckPlan(_ context.Context, _ plancheck.CheckPlanRequest, resp *plancheck.CheckPlanResponse) {
	resp.Error = f()
}

func acceptanceCollectionGroupParents(t *testing.T, api *acceptanceAPI, name string) (string, string) {
	t.Helper()
	collection, err := api.acceptanceCreateCollection("Terraform collection group " + name)
	if err != nil {
		t.Fatal(err)
	}
	group, err := api.acceptanceCreateGroup("Terraform collection group " + name)
	if err != nil {
		t.Fatal(err)
	}
	return collection, group
}

// All cases share one disposable release stack. Avoid a container startup for
// each permission and missing-parent case, or hundreds of seed HTTP requests.
func TestAccCollectionGroup(t *testing.T) {
	api := newAcceptanceAPI(t)
	api.acceptanceCollectionAnchor(t)
	t.Run("lifecycle and drift", func(t *testing.T) { testAccCollectionGroupLifecycle(t, api) })
	t.Run("existing explicit pair", func(t *testing.T) { testAccCollectionGroupExistingPair(t, api) })
	t.Run("multi-page explicit grants", func(t *testing.T) { testAccCollectionGroupPagination(t, api) })
	t.Run("inherited default is not a grant", func(t *testing.T) { testAccCollectionGroupInheritedDefault(t, api) })
	t.Run("missing grant and parents", func(t *testing.T) { testAccCollectionGroupMissing(t, api) })
	t.Run("external groups allow manual grants", func(t *testing.T) { testAccCollectionGroupExternal(t, api) })
	t.Run("archived restrictions retain state", func(t *testing.T) { testAccCollectionGroupArchived(t, api) })
	t.Run("last manager errors retain state", func(t *testing.T) { testAccCollectionGroupLastManager(t, api) })
	// The foreign-workspace fixture must run after the one-team archive/sync fixtures.
	t.Run("forbidden retains state", func(t *testing.T) { testAccCollectionGroupForbidden(t, api) })
}

func testAccCollectionGroupLifecycle(t *testing.T, api *acceptanceAPI) {
	collection, other := acceptanceCollectionGroupParents(t, api, "lifecycle")
	// Grant management must also leave the release's admin default intact,
	// even though outline_collection cannot manage that default itself.
	if err := api.acceptanceUpdateCollection(client.CollectionsUpdateJSONRequestBody{
		Id: uuid.MustParse(collection), Permission: nullable.NewNullableWithValue(client.PermissionAdmin),
	}); err != nil {
		t.Fatal(err)
	}
	if err := api.acceptanceWriteCollectionGroup(collection, other, "admin"); err != nil {
		t.Fatal(err)
	}
	actor, err := api.acceptanceUserActor()
	if err != nil {
		t.Fatal(err)
	}
	groups := make([]string, len(acceptanceCollectionGroupRoles))
	for i, role := range acceptanceCollectionGroupRoles {
		groups[i], err = api.acceptanceCreateGroup("Terraform collection group lifecycle " + role)
		if err != nil {
			t.Fatal(err)
		}
		if err := api.acceptanceWriteGroupMember(groups[i], actor.Id.String(), client.GroupPermissionMember, true); err != nil {
			t.Fatal(err)
		}
	}
	baseline, err := api.acceptanceCollectionGroupSnapshot(collection, groups)
	if err != nil {
		t.Fatal(err)
	}
	// The same group also has a grant on another collection. Pair operations
	// must not search or delete by group ID alone.
	cross, err := api.acceptanceCreateCollection("Terraform collection group other collection")
	if err != nil {
		t.Fatal(err)
	}
	if err := api.acceptanceWriteCollectionGroup(cross, groups[0], "read_write"); err != nil {
		t.Fatal(err)
	}
	crossBaseline, err := api.acceptanceCollectionGroups(cross)
	if err != nil {
		t.Fatal(err)
	}
	collateral := func(_ *terraform.State) error {
		if err := api.acceptanceCollectionGroupCollateral(collection, groups, baseline); err != nil {
			return err
		}
		crossGrants, err := api.acceptanceCollectionGroups(cross)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(crossGrants, crossBaseline) {
			return fmt.Errorf("pair operation changed another collection's grants")
		}
		return nil
	}
	address := func(role string) string { return "outline_collection_group." + role }
	config := func(rotate bool) string {
		var resources strings.Builder
		resources.WriteString(api.providerConfig)
		for i, role := range acceptanceCollectionGroupRoles {
			permission := role
			if rotate {
				permission = acceptanceCollectionGroupRoles[(i+1)%len(groups)]
			}
			fmt.Fprintf(&resources, `
resource "outline_collection_group" %q {
  collection_id = %q
  group_id = %q
  permission = %q
}
`, role, collection, groups[i], permission)
		}
		return resources.String()
	}
	apiIDs := make(map[string]string)
	check := func(rotate bool) resource.TestCheckFunc {
		var checks []resource.TestCheckFunc
		for i, role := range acceptanceCollectionGroupRoles {
			permission := role
			if rotate {
				permission = acceptanceCollectionGroupRoles[(i+1)%len(groups)]
			}
			checks = append(checks, api.checkAcceptanceCollectionGroup(address(role)),
				resource.TestCheckResourceAttr(address(role), "id", collection+"/"+groups[i]),
				resource.TestCheckResourceAttr(address(role), "permission", permission))
		}
		checks = append(checks, collateral, func(_ *terraform.State) error {
			grants, err := api.acceptanceCollectionGroups(collection)
			if err != nil {
				return err
			}
			for _, group := range groups {
				if previous := apiIDs[group]; previous != "" && previous != grants[group].APIID {
					return fmt.Errorf("permission update recreated API membership for group %s", group)
				}
				apiIDs[group] = grants[group].APIID
			}
			return nil
		})
		return resource.ComposeAggregateTestCheckFunc(checks...)
	}
	steps := []resource.TestStep{{Config: config(false), Check: check(false)}}
	for _, role := range acceptanceCollectionGroupRoles {
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
			if err := api.acceptanceWriteCollectionGroup(collection, groups[0], "admin"); err != nil {
				t.Fatal(err)
			}
		}, RefreshState: true, ExpectNonEmptyPlan: true, Check: resource.ComposeAggregateTestCheckFunc(
			api.checkAcceptanceCollectionGroup(address("read")), resource.TestCheckResourceAttr(address("read"), "permission", "admin"), collateral)},
		resource.TestStep{Config: config(true), ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
			plancheck.ExpectResourceAction(address("read"), plancheck.ResourceActionUpdate),
		}}, Check: check(true)},
		resource.TestStep{Config: api.providerConfig, Check: func(state *terraform.State) error {
			for _, group := range groups {
				if err := api.acceptanceCollectionGroupPermission(collection, group, ""); err != nil {
					return err
				}
			}
			return collateral(state)
		}},
	)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acceptanceProviderFactories(),
		CheckDestroy: func(state *terraform.State) error {
			for _, group := range groups {
				if err := api.acceptanceCollectionGroupPermission(collection, group, ""); err != nil {
					return err
				}
			}
			return collateral(state)
		},
		Steps: steps,
	})
}

func testAccCollectionGroupExistingPair(t *testing.T, api *acceptanceAPI) {
	collection, group := acceptanceCollectionGroupParents(t, api, "existing")
	if err := api.acceptanceWriteCollectionGroup(collection, group, "admin"); err != nil {
		t.Fatal(err)
	}
	before, err := api.acceptanceCollectionGroups(collection)
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := api.acceptanceCollectionGroupSnapshot(collection, []string{group})
	if err != nil {
		t.Fatal(err)
	}
	retained := func() {
		t.Helper()
		after, err := api.acceptanceCollectionGroups(collection)
		if err != nil || !reflect.DeepEqual(before, after) {
			t.Fatalf("failed Create adopted or changed an existing explicit pair: before=%v after=%v err=%v", before, after, err)
		}
		if err := api.acceptanceCollectionGroupCollateral(collection, []string{group}, baseline); err != nil {
			t.Fatal(err)
		}
	}
	var steps []resource.TestStep
	for _, role := range acceptanceCollectionGroupRoles {
		steps = append(steps, resource.TestStep{
			PreConfig: retained, Config: api.acceptanceCollectionGroupConfig(collection, group, role),
			ExpectError: regexp.MustCompile(`(?is)already exists.*import.*` + regexp.QuoteMeta(collection+"/"+group)),
		})
	}
	config := api.acceptanceCollectionGroupConfig(collection, group, "admin")
	steps = append(steps,
		resource.TestStep{PreConfig: retained, Config: config, ResourceName: acceptanceCollectionGroupAddress,
			ImportState: true, ImportStatePersist: true, ImportStateId: collection + "/" + group,
			ImportStateCheck: func(states []*terraform.InstanceState) error {
				if len(states) != 1 || states[0].ID != collection+"/"+group || states[0].Attributes["permission"] != "admin" ||
					states[0].Attributes["collection_id"] != collection || states[0].Attributes["group_id"] != group {
					return fmt.Errorf("import did not read the existing explicit grant: %+v", states)
				}
				return nil
			}},
		resource.TestStep{Config: config, ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}},
			Check: api.checkAcceptanceCollectionGroup(acceptanceCollectionGroupAddress)},
		resource.TestStep{Config: api.providerConfig, Check: func(_ *terraform.State) error {
			if err := api.acceptanceCollectionGroupPermission(collection, group, ""); err != nil {
				return err
			}
			return api.acceptanceCollectionGroupCollateral(collection, []string{group}, baseline)
		}},
	)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acceptanceProviderFactories(),
		CheckDestroy:             func(_ *terraform.State) error { return api.acceptanceCollectionGroupPermission(collection, group, "") },
		Steps:                    steps,
	})
}

func testAccCollectionGroupPagination(t *testing.T, api *acceptanceAPI) {
	collection, target := acceptanceCollectionGroupParents(t, api, "pagination target")
	if err := api.acceptanceUpdateCollection(client.CollectionsUpdateJSONRequestBody{
		Id: uuid.MustParse(collection), Permission: nullable.NewNullableWithValue(client.PermissionReadWrite),
	}); err != nil {
		t.Fatal(err)
	}
	// The release orders grants by createdAt DESC. Insert the managed pair
	// first, then 101 others, so matching it requires the final page.
	seeded := make(map[string]acceptanceCollectionGroupRecord)
	var err error
	seeded[target], err = api.acceptanceUpsertCollectionGroup(collection, target, "read")
	if err != nil {
		t.Fatal(err)
	}
	for i := range 101 {
		group, err := api.acceptanceCreateGroup(fmt.Sprintf("Terraform collection group pagination %03d", i))
		if err != nil {
			t.Fatal(err)
		}
		seeded[group], err = api.acceptanceUpsertCollectionGroup(collection, group, acceptanceCollectionGroupRoles[i%len(acceptanceCollectionGroupRoles)])
		if err != nil {
			t.Fatal(err)
		}
	}
	observed, err := api.acceptanceCollectionGroups(collection)
	if err != nil || !reflect.DeepEqual(observed, seeded) {
		t.Fatalf("seeded 102 explicit grants do not match the complete HTTP listing: err=%v seeded=%v observed=%v", err, seeded, observed)
	}
	limit := 100
	for _, offset := range []int{0, 100} {
		r, err := api.CollectionsGroupMembershipsWithResponse(t.Context(), client.CollectionsGroupMembershipsJSONRequestBody{
			Id: uuid.MustParse(collection), Limit: &limit, Offset: &offset,
		})
		if err != nil {
			t.Fatal(err)
		}
		if r.StatusCode() != http.StatusOK || r.JSON200 == nil || r.JSON200.Data == nil ||
			r.JSON200.Data.GroupMemberships == nil || r.JSON200.Pagination == nil || r.JSON200.Pagination.Total == nil ||
			*r.JSON200.Pagination.Total != 102 || r.JSON200.Pagination.Offset == nil || *r.JSON200.Pagination.Offset != offset {
			t.Fatalf("pagination seed: HTTP %d: %s", r.StatusCode(), r.Body)
		}
		if err := checkCollectionGrantEnvelope("acceptance pagination seed", r.JSON200.Ok, r.JSON200.Status); err != nil {
			t.Fatal(err)
		}
		members := *r.JSON200.Data.GroupMemberships
		wantCount := 100
		if offset == 100 {
			wantCount = 2
		}
		if len(members) != wantCount {
			t.Fatalf("offset %d returned %d grants, expected %d", offset, len(members), wantCount)
		}
		for i, member := range members {
			if _, err := acceptanceCollectionGroupRecordFromAPI(collection, member); err != nil {
				t.Fatal(err)
			}
			if (member.GroupId.String() == target) != (offset == 100 && i == 1) {
				t.Fatalf("target must be the last of 102 createdAt DESC grants, got offset %d index %d group %s", offset, i, member.GroupId)
			}
		}
	}
	baseline, err := api.acceptanceCollectionGrantSnapshot(collection, []string{target})
	if err != nil {
		t.Fatal(err)
	}
	if len(baseline.GroupGrants) != 101 || baseline.Permission != "read_write" {
		t.Fatalf("pagination collateral baseline must contain 101 grants and read_write default: %+v", baseline)
	}
	collateral := func() error {
		actual, err := api.acceptanceCollectionGrantSnapshot(collection, []string{target})
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
			api.checkAcceptanceCollectionGroup(acceptanceCollectionGroupAddress),
			resource.TestCheckResourceAttr(acceptanceCollectionGroupAddress, "id", collection+"/"+target),
			resource.TestCheckResourceAttr(acceptanceCollectionGroupAddress, "permission", permission),
			func(_ *terraform.State) error {
				grants, err := api.acceptanceCollectionGroups(collection)
				if err != nil {
					return err
				}
				if len(grants) != 102 || grants[target].APIID != seeded[target].APIID || grants[target].Permission != permission {
					return fmt.Errorf("paginated read/update changed pair identity or failed to read its permission: %+v", grants[target])
				}
				return collateral()
			},
		)
	}
	retained := func() {
		t.Helper()
		grants, err := api.acceptanceCollectionGroups(collection)
		if err != nil || !reflect.DeepEqual(grants, seeded) {
			t.Fatalf("refused Create changed the existing paginated grant: err=%v before=%v after=%v", err, seeded, grants)
		}
		if err := collateral(); err != nil {
			t.Fatal(err)
		}
	}
	deleted := func(_ *terraform.State) error {
		if err := api.acceptanceCollectionGroupPermission(collection, target, ""); err != nil {
			return err
		}
		return collateral()
	}
	readConfig := api.acceptanceCollectionGroupConfig(collection, target, "read")
	writeConfig := api.acceptanceCollectionGroupConfig(collection, target, "read_write")
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acceptanceProviderFactories(), CheckDestroy: deleted,
		Steps: []resource.TestStep{
			{Config: api.acceptanceCollectionGroupConfig(collection, target, "admin"),
				ExpectError: regexp.MustCompile(`(?is)already exists.*import.*` + regexp.QuoteMeta(collection+"/"+target))},
			{PreConfig: retained, Config: readConfig, ResourceName: acceptanceCollectionGroupAddress,
				ImportState: true, ImportStatePersist: true, ImportStateId: collection + "/" + target,
				ImportStateCheck: func(states []*terraform.InstanceState) error {
					if len(states) != 1 || states[0].ID != collection+"/"+target || states[0].Attributes["permission"] != "read" ||
						states[0].Attributes["collection_id"] != collection || states[0].Attributes["group_id"] != target {
						return fmt.Errorf("import did not read the explicit grant on the final page: %+v", states)
					}
					return collateral()
				}},
			{Config: readConfig, ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}}, Check: check("read")},
			{ResourceName: acceptanceCollectionGroupAddress, ImportState: true, ImportStateVerify: true},
			{Config: writeConfig, ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
				plancheck.ExpectResourceAction(acceptanceCollectionGroupAddress, plancheck.ResourceActionUpdate),
			}}, Check: check("read_write")},
			{PreConfig: func() {
				if err := api.acceptanceWriteCollectionGroup(collection, target, "admin"); err != nil {
					t.Fatal(err)
				}
			}, RefreshState: true, ExpectNonEmptyPlan: true, Check: check("admin")},
			{Config: writeConfig, ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
				plancheck.ExpectResourceAction(acceptanceCollectionGroupAddress, plancheck.ResourceActionUpdate),
			}}, Check: check("read_write")},
			{Config: api.providerConfig, ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
				plancheck.ExpectResourceAction(acceptanceCollectionGroupAddress, plancheck.ResourceActionDestroy),
			}}, Check: deleted},
		},
	})
	// 101 untouched grants remain, so absent-pair confirmation still has two
	// real pages to validate after Terraform deletes the managed pair.
	acceptanceCollectionGroupAbsentRemoval(t, api, collection, target)
	if err := collateral(); err != nil {
		t.Fatal(err)
	}
}

func testAccCollectionGroupInheritedDefault(t *testing.T, api *acceptanceAPI) {
	collection, group := acceptanceCollectionGroupParents(t, api, "default")
	if err := api.acceptanceUpdateCollection(client.CollectionsUpdateJSONRequestBody{
		Id: uuid.MustParse(collection), Permission: nullable.NewNullableWithValue(client.PermissionReadWrite),
	}); err != nil {
		t.Fatal(err)
	}
	users, err := api.acceptanceInviteUsers([]client.Invite{{
		Email: "collection-group-default@example.invalid", Name: "Collection group default member", Role: client.UserRoleMember,
	}})
	if err != nil || len(users) != 1 || users[0].Id == nil {
		t.Fatalf("invite default fixture: %v", err)
	}
	user := users[0].Id.String()
	if err := api.acceptanceWriteGroupMember(group, user, client.GroupPermissionMember, true); err != nil {
		t.Fatal(err)
	}
	key := api.acceptanceInspectUser(t, user, "key").APIKey
	member, err := newAPIClient(api.baseURL, key, 30, "acceptance")
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := api.acceptanceCollectionGroupSnapshot(collection, []string{group})
	if err != nil {
		t.Fatal(err)
	}
	effective := func() error {
		r, err := member.CollectionsInfoWithResponse(context.Background(), client.CollectionsInfoJSONRequestBody{Id: uuid.MustParse(collection)})
		if err != nil {
			return err
		}
		if r.StatusCode() != http.StatusOK || r.JSON200 == nil || r.JSON200.Data == nil ||
			r.JSON200.Data.Permission.IsNull() || r.JSON200.Data.Permission.GetOrEmpty() != client.PermissionReadWrite {
			return fmt.Errorf("member lost the collection's inherited read_write default")
		}
		if err := checkEnvelope("acceptance inherited collection access", r.JSON200.Ok, r.JSON200.Status); err != nil {
			return err
		}
		return acceptanceCollectionAbility(r.JSON200.Policies, collection, "updateDocument", true)
	}
	if err := effective(); err != nil {
		t.Fatal(err)
	}
	if err := api.acceptanceCollectionGroupPermission(collection, group, ""); err != nil {
		t.Fatal(err)
	}
	check := func(permission string) resource.TestCheckFunc {
		return func(_ *terraform.State) error {
			if err := api.acceptanceCollectionGroupPermission(collection, group, permission); err != nil {
				return err
			}
			if err := effective(); err != nil {
				return err
			}
			return api.acceptanceCollectionGroupCollateral(collection, []string{group}, baseline)
		}
	}
	config := api.acceptanceCollectionGroupConfig(collection, group, "read")
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acceptanceProviderFactories(), CheckDestroy: check(""),
		Steps: []resource.TestStep{
			{Config: config, Check: resource.ComposeAggregateTestCheckFunc(
				api.checkAcceptanceCollectionGroup(acceptanceCollectionGroupAddress),
				resource.TestCheckResourceAttr(acceptanceCollectionGroupAddress, "permission", "read"), check("read"))},
			{ResourceName: acceptanceCollectionGroupAddress, ImportState: true, ImportStateVerify: true},
			{Config: config, ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}}, Check: check("read")},
			{Config: api.providerConfig, Check: check("")},
		},
	})
}

func testAccCollectionGroupMissing(t *testing.T, api *acceptanceAPI) {
	for _, missing := range []string{"grant", "collection", "group"} {
		t.Run(missing, func(t *testing.T) {
			collection, group := acceptanceCollectionGroupParents(t, api, "missing read "+missing)
			deleteCollection, deleteGroup := acceptanceCollectionGroupParents(t, api, "missing delete "+missing)
			remove := func(collection, group string) error {
				switch missing {
				case "collection":
					return api.acceptanceDeleteCollection(collection)
				case "group":
					return api.acceptanceDeleteGroup(group)
				default:
					return api.acceptanceRemoveCollectionGroup(collection, group)
				}
			}
			absent := func(collection, group string) error {
				if missing == "collection" {
					return api.acceptanceCollectionAbsent(collection)
				}
				if missing == "group" {
					if err := api.acceptanceGroupAbsent(group); err != nil {
						return err
					}
				}
				return api.acceptanceCollectionGroupPermission(collection, group, "")
			}
			config := api.acceptanceCollectionGroupConfig(collection, group, "read_write")
			steps := []resource.TestStep{
				{Config: config, Check: api.checkAcceptanceCollectionGroup(acceptanceCollectionGroupAddress)},
				{PreConfig: func() {
					if err := remove(collection, group); err != nil {
						t.Fatal(err)
					}
				}, RefreshState: true, ExpectNonEmptyPlan: true, Check: func(state *terraform.State) error {
					if _, exists := state.RootModule().Resources[acceptanceCollectionGroupAddress]; exists {
						return fmt.Errorf("missing %s did not remove collection group state", missing)
					}
					return absent(collection, group)
				}},
			}
			if missing == "grant" {
				steps = append(steps, resource.TestStep{Config: config, ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(acceptanceCollectionGroupAddress, plancheck.ResourceActionCreate),
				}}, Check: api.checkAcceptanceCollectionGroup(acceptanceCollectionGroupAddress)})
			}
			steps = append(steps,
				resource.TestStep{Config: api.providerConfig},
				resource.TestStep{Config: api.acceptanceCollectionGroupConfig(deleteCollection, deleteGroup, "admin"),
					Check: api.checkAcceptanceCollectionGroup(acceptanceCollectionGroupAddress)},
				resource.TestStep{Config: api.providerConfig, ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(acceptanceCollectionGroupAddress, plancheck.ResourceActionDestroy),
					acceptanceCollectionGroupBeforeApply(func() error {
						if err := remove(deleteCollection, deleteGroup); err != nil {
							return err
						}
						return absent(deleteCollection, deleteGroup)
					}),
				}}, Check: func(_ *terraform.State) error { return absent(deleteCollection, deleteGroup) }},
			)
			resource.Test(t, resource.TestCase{
				ProtoV6ProviderFactories: acceptanceProviderFactories(),
				CheckDestroy: func(_ *terraform.State) error {
					if err := absent(collection, group); err != nil {
						return err
					}
					return absent(deleteCollection, deleteGroup)
				}, Steps: steps,
			})
		})
	}
}
