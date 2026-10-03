// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/glitchedmob/terraform-provider-outline/internal/client"
	"github.com/google/uuid"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// Called from TestAccCollectionUser to share its disposable release stack.
func testAccIAM(t *testing.T, api *acceptanceAPI) {
	const memberEmail = "iam-member@example.invalid"
	const viewerEmail = "iam-viewer@example.invalid"
	const groupName = "Terraform integrated IAM group"
	const collectionName = "Terraform integrated IAM private collection"
	// Keep grant parent IDs tied to resources. Data sources may be deferred
	// during parent drift and would turn unchanged pairs into replacements.
	config := api.providerConfig + fmt.Sprintf(`
resource "outline_user" "member" {
 email = %q
 name = "IAM member"
 role = "member"
 suspended = false
 delete_permanently = true
}
resource "outline_user" "viewer" {
 email = %q
 name = "IAM viewer"
 role = "viewer"
 suspended = false
 delete_permanently = true
}
resource "outline_group" "team" {
 name = %q
 description = "IAM group description"
}
resource "outline_collection" "private" {
 name = %q
 description = "IAM private description"
 sharing = false
 allow_destroy = true
}
data "outline_user" "by_email" {
 email = outline_user.member.email
 depends_on = [outline_user.member]
}
data "outline_user" "by_id" { id = outline_user.viewer.id }
data "outline_group" "by_name" {
 name = outline_group.team.name
 depends_on = [outline_group.team]
}
data "outline_group" "by_id" { id = outline_group.team.id }
data "outline_collection" "by_name" {
 name = outline_collection.private.name
 depends_on = [outline_collection.private]
}
data "outline_collection" "by_id" { id = outline_collection.private.id }
resource "outline_group_member" "member" {
 group_id = outline_group.team.id
 user_id = outline_user.member.id
 permission = "member"
}
resource "outline_collection_group" "team" {
 collection_id = outline_collection.private.id
 group_id = outline_group.team.id
 permission = "read_write"
 depends_on = [outline_group_member.member]
}
resource "outline_collection_user" "viewer" {
 collection_id = outline_collection.private.id
 user_id = outline_user.viewer.id
 permission = "read"
}
`, memberEmail, viewerEmail, groupName, collectionName)
	ids := make(map[string]string)
	var memberClient, viewerClient *apiClient
	var creatorBaseline map[string]acceptanceCollectionUserRecord
	var membersBaseline map[string]acceptanceGroupMemberRecord
	var memberAPIID, viewerAPIID string
	addresses := []string{"outline_user.member", "outline_user.viewer", "outline_group.team", "outline_collection.private", "outline_group_member.member", "outline_collection_group.team", "outline_collection_user.viewer"}
	check := func(drift bool) resource.TestCheckFunc {
		return func(state *terraform.State) error {
			for _, address := range addresses {
				r := state.RootModule().Resources[address]
				if r == nil || r.Primary == nil {
					return fmt.Errorf("integrated IAM graph missing %s", address)
				}
				if previous := ids[address]; previous != "" && previous != r.Primary.ID {
					return fmt.Errorf("integrated drift changed identity of %s", address)
				}
				ids[address] = r.Primary.ID
			}
			collection, group, member, viewer := ids["outline_collection.private"], ids["outline_group.team"], ids["outline_user.member"], ids["outline_user.viewer"]
			checks := []resource.TestCheckFunc{
				api.checkAcceptanceUser("outline_user.member"), api.checkAcceptanceUser("outline_user.viewer"),
				api.checkAcceptanceGroup("outline_group.team"), api.checkAcceptanceCollection("outline_collection.private"),
				api.checkAcceptanceGroupMember("outline_group_member.member"), api.checkAcceptanceCollectionGroup("outline_collection_group.team"), api.checkAcceptanceCollectionUser("outline_collection_user.viewer"),
			}
			// Refresh defers data sources whose dependencies have pending drift
			// corrections. Assert all lookups after apply, not in deferred state.
			if !drift {
				checks = append(checks,
					resource.TestCheckResourceAttr("data.outline_user.by_email", "id", member), resource.TestCheckResourceAttr("data.outline_user.by_id", "id", viewer),
					resource.TestCheckResourceAttr("data.outline_group.by_name", "id", group), resource.TestCheckResourceAttr("data.outline_group.by_id", "id", group),
					resource.TestCheckResourceAttr("data.outline_collection.by_name", "id", collection), resource.TestCheckResourceAttr("data.outline_collection.by_id", "id", collection),
				)
			}
			if err := resource.ComposeAggregateTestCheckFunc(checks...)(state); err != nil {
				return err
			}
			if c, err := api.acceptanceCollection(collection); err != nil || !c.Permission.IsNull() {
				return fmt.Errorf("IAM collection lost private default: %v", err)
			}
			users, err := api.acceptanceCollectionUsers(collection)
			if err != nil {
				return err
			}
			groups, err := api.acceptanceCollectionGroups(collection)
			if err != nil {
				return err
			}
			if len(users) != 2 || len(groups) != 1 {
				return fmt.Errorf("IAM collection must contain only creator+viewer direct grants and one group grant: users=%v groups=%v", users, groups)
			}
			if memberAPIID != "" && memberAPIID != groups[group].APIID || viewerAPIID != "" && viewerAPIID != users[viewer].APIID {
				return fmt.Errorf("IAM drift/update recreated grants")
			}
			memberAPIID, viewerAPIID = groups[group].APIID, users[viewer].APIID
			delete(users, viewer)
			if creatorBaseline == nil {
				creatorBaseline = users
			} else if !reflect.DeepEqual(users, creatorBaseline) {
				return fmt.Errorf("IAM operations changed creator grant")
			}
			members, _, err := api.acceptanceGroupMembers(group)
			if err != nil {
				return err
			}
			memberRecord := members[member]
			expectedName := "IAM member"
			if drift {
				expectedName = "External IAM member"
			}
			if memberRecord.Name != expectedName {
				return fmt.Errorf("group presenter did not observe user name drift: %+v", memberRecord)
			}
			memberRecord.Name = ""
			members[member] = memberRecord
			if membersBaseline == nil {
				membersBaseline = members
			} else if !reflect.DeepEqual(members, membersBaseline) {
				return fmt.Errorf("grant operations changed group memberships")
			}
			if len(members) != 1 || members[member].Permission != "member" {
				return fmt.Errorf("unexpected IAM group membership: %v", members)
			}
			if memberClient == nil {
				key := api.acceptanceInspectUser(t, member, "key").APIKey
				memberClient, err = newAPIClient(api.baseURL, key, 30, "acceptance")
				if err != nil {
					return err
				}
				key = api.acceptanceInspectUser(t, viewer, "key").APIKey
				viewerClient, err = newAPIClient(api.baseURL, key, 30, "acceptance")
				if err != nil {
					return err
				}
			}
			if err := acceptanceIAMEffective(memberClient, collection, true, true, drift); err != nil {
				return err
			}
			return acceptanceIAMEffective(viewerClient, collection, true, drift, drift)
		}
	}
	steps := []resource.TestStep{{Config: config, Check: check(false)}}
	for _, address := range addresses {
		ignore := []string(nil)
		if address == "outline_user.member" || address == "outline_user.viewer" {
			ignore = []string{"delete_permanently"}
		}
		if address == "outline_collection.private" {
			ignore = []string{"allow_destroy"}
		}
		steps = append(steps, resource.TestStep{ResourceName: address, ImportState: true, ImportStateVerify: true, ImportStateVerifyIgnore: ignore})
	}
	steps = append(steps,
		resource.TestStep{Config: config, ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}}, Check: check(false)},
		resource.TestStep{PreConfig: func() {
			collection, group := ids["outline_collection.private"], ids["outline_group.team"]
			if err := api.acceptanceWriteCollectionGroup(collection, group, "admin"); err != nil {
				t.Fatal(err)
			}
			if err := api.acceptanceWriteCollectionUser(collection, ids["outline_user.viewer"], "admin"); err != nil {
				t.Fatal(err)
			}
			if err := api.acceptanceUserName(ids["outline_user.member"], "External IAM member"); err != nil {
				t.Fatal(err)
			}
			if err := api.acceptanceUpdateGroup(group, groupName, "External IAM group description", false); err != nil {
				t.Fatal(err)
			}
			if err := api.acceptanceUpdateCollection(client.CollectionsUpdateJSONRequestBody{Id: uuid.MustParse(collection), Sharing: acceptanceIAMBoolPointer(true)}); err != nil {
				t.Fatal(err)
			}
		}, RefreshState: true, ExpectNonEmptyPlan: true, Check: check(true)},
		resource.TestStep{Config: config, ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
			plancheck.ExpectResourceAction("outline_collection_group.team", plancheck.ResourceActionUpdate),
			plancheck.ExpectResourceAction("outline_collection_user.viewer", plancheck.ResourceActionUpdate),
			plancheck.ExpectResourceAction("outline_user.member", plancheck.ResourceActionUpdate),
			plancheck.ExpectResourceAction("outline_group.team", plancheck.ResourceActionUpdate),
			plancheck.ExpectResourceAction("outline_collection.private", plancheck.ResourceActionUpdate),
		}}, Check: check(false)},
		resource.TestStep{Config: api.providerConfig, Check: func(_ *terraform.State) error {
			if err := api.acceptanceCollectionAbsent(ids["outline_collection.private"]); err != nil {
				return err
			}
			if err := api.acceptanceGroupAbsent(ids["outline_group.team"]); err != nil {
				return err
			}
			for _, address := range []string{"outline_user.member", "outline_user.viewer"} {
				if err := api.acceptanceUserAbsent(ids[address]); err != nil {
					return err
				}
			}
			return nil
		}},
	)
	resource.Test(t, resource.TestCase{ProtoV6ProviderFactories: acceptanceProviderFactories(), Steps: steps})
}

func acceptanceIAMBoolPointer(value bool) *bool { return &value }
