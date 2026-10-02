// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"testing"

	"github.com/glitchedmob/terraform-provider-outline/internal/client"
	"github.com/google/uuid"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

func (a *acceptanceAPI) acceptanceGroup(id string) (*client.Group, error) {
	groupID, err := uuid.Parse(id)
	if err != nil {
		return nil, err
	}
	response, err := a.GroupsInfoWithResponse(context.Background(), client.GroupsInfoJSONRequestBody{Id: groupID})
	if err != nil {
		return nil, err
	}
	if response.StatusCode() != http.StatusOK || response.JSON200 == nil || response.JSON200.Data == nil {
		return nil, fmt.Errorf("groups.info %s: HTTP %d: %s", id, response.StatusCode(), response.Body)
	}
	return response.JSON200.Data, nil
}

func (a *acceptanceAPI) acceptanceGroupAbsent(id string) error {
	groupID, err := uuid.Parse(id)
	if err != nil {
		return err
	}
	response, err := a.GroupsInfoWithResponse(context.Background(), client.GroupsInfoJSONRequestBody{Id: groupID})
	if err != nil {
		return err
	}
	// Outline 1.10.1 authorizes before reporting absence, so deleted IDs return
	// authorization_error. Neither 403 nor 404 alone proves this fixture is absent.
	missing := response.StatusCode() == http.StatusNotFound && response.JSON404 != nil
	forbidden := response.StatusCode() == http.StatusForbidden && response.JSON403 != nil &&
		response.JSON403.Error != nil && *response.JSON403.Error == "authorization_error"
	if !missing && !forbidden {
		return fmt.Errorf("group %s should be absent, groups.info returned HTTP %d: %s", id, response.StatusCode(), response.Body)
	}
	limit, offset := 100, 0
	seen := make(map[uuid.UUID]bool)
	for {
		// The bootstrap key is an unrestricted admin key. Do not filter this list.
		page, err := a.GroupsListWithResponse(context.Background(), client.GroupsListJSONRequestBody{
			Limit: &limit, Offset: &offset,
		})
		if err != nil {
			return err
		}
		if page.StatusCode() != http.StatusOK || page.JSON200 == nil || page.JSON200.Data == nil || page.JSON200.Data.Groups == nil {
			return fmt.Errorf("groups.list absence check: HTTP %d: %s", page.StatusCode(), page.Body)
		}
		if err := checkEnvelope("groups.list absence check", page.JSON200.Ok, page.JSON200.Status); err != nil {
			return err
		}
		groups := *page.JSON200.Data.Groups
		for _, group := range groups {
			if group.Id == nil || *group.Id == uuid.Nil || seen[*group.Id] {
				return fmt.Errorf("groups.list absence check: missing or repeated group ID")
			}
			seen[*group.Id] = true
			if *group.Id == groupID {
				return fmt.Errorf("group %s still exists in groups.list", id)
			}
		}
		next, more, err := nextOffset(page.JSON200.Pagination, offset, len(groups))
		if err != nil {
			return fmt.Errorf("groups.list absence check: %w", err)
		}
		if !more {
			return nil
		}
		offset = next
	}
}

func (a *acceptanceAPI) acceptanceCreateGroup(name string) (string, error) {
	response, err := a.GroupsCreateWithResponse(context.Background(), client.GroupsCreateJSONRequestBody{Name: name})
	if err != nil {
		return "", err
	}
	if response.StatusCode() != http.StatusOK || response.JSON200 == nil || response.JSON200.Data == nil || response.JSON200.Data.Id == nil {
		return "", fmt.Errorf("groups.create: HTTP %d: %s", response.StatusCode(), response.Body)
	}
	return response.JSON200.Data.Id.String(), nil
}

func (a *acceptanceAPI) acceptanceUpdateGroup(id, name, description string, disableMentions bool) error {
	groupID, err := uuid.Parse(id)
	if err != nil {
		return err
	}
	response, err := a.GroupsUpdateWithResponse(context.Background(), client.GroupsUpdateJSONRequestBody{
		Id: groupID, Name: &name, Description: &description, DisableMentions: &disableMentions,
	})
	if err != nil {
		return err
	}
	if response.StatusCode() != http.StatusOK || response.JSON200 == nil || response.JSON200.Data == nil {
		return fmt.Errorf("groups.update %s: HTTP %d: %s", id, response.StatusCode(), response.Body)
	}
	return nil
}

func (a *acceptanceAPI) acceptanceDeleteGroup(id string) error {
	groupID, err := uuid.Parse(id)
	if err != nil {
		return err
	}
	response, err := a.GroupsDeleteWithResponse(context.Background(), client.GroupsDeleteJSONRequestBody{Id: groupID})
	if err != nil {
		return err
	}
	if response.StatusCode() != http.StatusOK || response.JSON200 == nil || response.JSON200.Success == nil || !*response.JSON200.Success {
		return fmt.Errorf("groups.delete %s: HTTP %d: %s", id, response.StatusCode(), response.Body)
	}
	return nil
}

func (a *acceptanceAPI) checkAcceptanceGroup(address string) resource.TestCheckFunc {
	return func(state *terraform.State) error {
		r, ok := state.RootModule().Resources[address]
		if !ok || r.Primary == nil || r.Primary.ID == "" {
			return fmt.Errorf("%s has no group ID", address)
		}
		group, err := a.acceptanceGroup(r.Primary.ID)
		if err != nil {
			return err
		}
		if group.Id == nil || group.Id.String() != r.Primary.ID || group.Name == nil || group.DisableMentions == nil {
			return fmt.Errorf("%s has an incomplete API group response", address)
		}
		for attribute, value := range map[string]string{
			"id": group.Id.String(), "name": *group.Name,
			"description": group.Description.GetOrEmpty(), "disable_mentions": strconv.FormatBool(*group.DisableMentions),
		} {
			if r.Primary.Attributes[attribute] != value {
				return fmt.Errorf("%s: API %s=%q, state=%q", address, attribute, value, r.Primary.Attributes[attribute])
			}
		}
		return nil
	}
}

func (a *acceptanceAPI) checkAcceptanceGroupsDestroyed(state *terraform.State) error {
	for _, r := range state.RootModule().Resources {
		if r.Type == "outline_group" && r.Primary != nil && r.Primary.ID != "" {
			if err := a.acceptanceGroupAbsent(r.Primary.ID); err != nil {
				return err
			}
		}
	}
	return nil
}

func (a *acceptanceAPI) acceptanceGroupConfig(name, extra string) string {
	return a.providerConfig + fmt.Sprintf(`
resource "outline_group" "test" {
  name = %q
%s
}
`, name, extra)
}

func TestAccGroupLifecycle(t *testing.T) {
	api := newAcceptanceAPI(t)
	const address = "outline_group.test"
	initial := api.acceptanceGroupConfig("Terraform group", `
  description = "Initial description"
  disable_mentions = true
`)
	updated := api.acceptanceGroupConfig("Terraform renamed group", `
  description = "Managed by Terraform"
  disable_mentions = true
`)
	reset := api.acceptanceGroupConfig("Terraform renamed group", "")
	var id string
	check := func(name, description, disableMentions string) resource.TestCheckFunc {
		return resource.ComposeAggregateTestCheckFunc(
			api.checkAcceptanceGroup(address),
			resource.TestCheckResourceAttr(address, "name", name),
			resource.TestCheckResourceAttr(address, "description", description),
			resource.TestCheckResourceAttr(address, "disable_mentions", disableMentions),
			func(state *terraform.State) error {
				newID := state.RootModule().Resources[address].Primary.ID
				if id != "" && id != newID {
					return fmt.Errorf("group update replaced %s with %s", id, newID)
				}
				id = newID
				return nil
			},
		)
	}
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acceptanceProviderFactories(),
		CheckDestroy:             api.checkAcceptanceGroupsDestroyed,
		Steps: []resource.TestStep{
			{Config: initial, Check: check("Terraform group", "Initial description", "true")},
			{ResourceName: address, ImportState: true, ImportStateVerify: true},
			{Config: initial, ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}}},
			{
				Config: updated,
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(address, plancheck.ResourceActionUpdate),
				}},
				Check: check("Terraform renamed group", "Managed by Terraform", "true"),
			},
			{ResourceName: address, ImportState: true, ImportStateVerify: true},
			{
				PreConfig: func() {
					if err := api.acceptanceUpdateGroup(id, "External name", "External description", false); err != nil {
						t.Fatal(err)
					}
				},
				RefreshState: true, ExpectNonEmptyPlan: true,
				Check: check("External name", "External description", "false"),
			},
			{
				Config: updated,
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(address, plancheck.ResourceActionUpdate),
				}},
				Check: check("Terraform renamed group", "Managed by Terraform", "true"),
			},
			{
				Config: reset,
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(address, plancheck.ResourceActionUpdate),
				}},
				Check: check("Terraform renamed group", "", "false"),
			},
			{RefreshState: true, Check: check("Terraform renamed group", "", "false")},
			{Config: api.providerConfig, Check: func(_ *terraform.State) error { return api.acceptanceGroupAbsent(id) }},
		},
	})
}

func TestAccGroupMissingStateRecreation(t *testing.T) {
	api := newAcceptanceAPI(t)
	const address = "outline_group.test"
	config := api.acceptanceGroupConfig("Terraform recreated group", "")
	var deletedID, recreatedID string
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acceptanceProviderFactories(),
		CheckDestroy:             api.checkAcceptanceGroupsDestroyed,
		Steps: []resource.TestStep{
			{Config: config, Check: resource.ComposeAggregateTestCheckFunc(api.checkAcceptanceGroup(address), func(state *terraform.State) error {
				deletedID = state.RootModule().Resources[address].Primary.ID
				return nil
			})},
			{
				PreConfig: func() {
					if err := api.acceptanceDeleteGroup(deletedID); err != nil {
						t.Fatal(err)
					}
				},
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(address, plancheck.ResourceActionCreate),
				}},
				Check: resource.ComposeAggregateTestCheckFunc(api.checkAcceptanceGroup(address), func(state *terraform.State) error {
					recreatedID = state.RootModule().Resources[address].Primary.ID
					if recreatedID == deletedID {
						return fmt.Errorf("deleted group %s was not recreated", deletedID)
					}
					return api.acceptanceGroupAbsent(deletedID)
				}),
			},
			{ResourceName: address, ImportState: true, ImportStateVerify: true},
			{Config: api.providerConfig, Check: func(_ *terraform.State) error { return api.acceptanceGroupAbsent(recreatedID) }},
		},
	})
}

func TestAccGroupDataSourceLookups(t *testing.T) {
	api := newAcceptanceAPI(t)
	const name = "Terraform exact target"
	id, err := api.acceptanceCreateGroup(name)
	if err != nil {
		t.Fatal(err)
	}
	// The oldest exact match follows a full page of substring-only matches.
	// Normal groups are created through the API. Only the duplicate below needs
	// an ORM fixture because Outline 1.10.1 rejects duplicate names.
	for i := 0; i < 101; i++ {
		if _, err := api.acceptanceCreateGroup(fmt.Sprintf("%s extra %03d", name, i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := api.checkAcceptanceGroupFirstPage(name, id, 0); err != nil {
		t.Fatal(err)
	}
	byName := func(lookup string) string {
		return api.providerConfig + fmt.Sprintf(`
data "outline_group" "by_name" {
  name = %q
}
`, lookup)
	}
	config := byName(name) + fmt.Sprintf(`
data "outline_group" "by_id" {
  id = %q
}
`, id)
	check := func(description, disableMentions string) resource.TestCheckFunc {
		return resource.ComposeAggregateTestCheckFunc(
			api.checkAcceptanceGroup("data.outline_group.by_id"), api.checkAcceptanceGroup("data.outline_group.by_name"),
			resource.TestCheckResourceAttr("data.outline_group.by_id", "name", name),
			resource.TestCheckResourceAttr("data.outline_group.by_name", "id", id),
			resource.TestCheckResourceAttr("data.outline_group.by_name", "description", description),
			resource.TestCheckResourceAttr("data.outline_group.by_name", "disable_mentions", disableMentions),
		)
	}
	var duplicateID string
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acceptanceProviderFactories(),
		Steps: []resource.TestStep{
			{Config: config, Check: check("", "false")},
			{
				PreConfig: func() {
					if err := api.acceptanceUpdateGroup(id, name, "External data source description", true); err != nil {
						t.Fatal(err)
					}
				},
				Config: config, Check: check("External data source description", "true"),
			},
			{
				PreConfig: func() {
					// Restore the old timestamp ordering by creating 101 newer matches.
					// This keeps the two exact matches on different list pages.
					for i := 0; i < 101; i++ {
						if _, err := api.acceptanceCreateGroup(fmt.Sprintf("%s newer %03d", name, i)); err != nil {
							t.Fatal(err)
						}
					}
					var err error
					duplicateID, err = api.acceptanceCreateDuplicateGroup(t, id)
					if err != nil {
						t.Fatal(err)
					}
					if err := api.checkAcceptanceGroupFirstPage(name, duplicateID, 1); err != nil {
						t.Fatal(err)
					}
				},
				Config: byName(name), ExpectError: regexp.MustCompile(`(?i)(ambiguous|multiple.*groups|more than one)`),
			},
			{
				PreConfig: func() {
					// Soft deletion validates the name too. Give the injected duplicate
					// a unique name so cleanup can still use the normal HTTP API.
					if err := api.acceptanceUpdateGroup(duplicateID, name+" fixture duplicate", "", false); err != nil {
						t.Fatal(err)
					}
					if err := api.acceptanceDeleteGroup(duplicateID); err != nil {
						t.Fatal(err)
					}
				},
				Config: byName("terraform exact target"), ExpectError: regexp.MustCompile(`(?i)(no .*group|group .*not found|not found)`),
			},
			{Config: byName("Terraform exact"), ExpectError: regexp.MustCompile(`(?i)(no .*group|group .*not found|not found)`)},
			{Config: api.providerConfig + fmt.Sprintf(`
data "outline_group" "missing" {
  id = %q
}
`, uuid.NewString()), ExpectError: regexp.MustCompile(`(?i)(not found|does not exist)`)},
			{Config: api.providerConfig + fmt.Sprintf(`
data "outline_group" "invalid" {
  id = %q
  name = %q
}
`, id, name), ExpectError: regexp.MustCompile(`(?i)(exactly one|only one|invalid attribute combination)`)},
			{Config: api.providerConfig + `
data "outline_group" "invalid" {}
`, ExpectError: regexp.MustCompile(`(?i)(exactly one|at least one|invalid attribute combination)`)},
			{Config: config, Check: check("External data source description", "true")},
		},
	})
}

// Check the server actually paginates this fixture, rather than accepting a
// provider implementation that only reads the first page by accident.
func (a *acceptanceAPI) checkAcceptanceGroupFirstPage(query, expectedID string, expectedMatches int) error {
	limit, offset := 100, 0
	response, err := a.GroupsListWithResponse(context.Background(), client.GroupsListJSONRequestBody{
		Query: &query, Limit: &limit, Offset: &offset,
	})
	if err != nil {
		return err
	}
	if response.StatusCode() != http.StatusOK || response.JSON200 == nil || response.JSON200.Data == nil || response.JSON200.Data.Groups == nil {
		return fmt.Errorf("groups.list fixture: HTTP %d: %s", response.StatusCode(), response.Body)
	}
	groups := *response.JSON200.Data.Groups
	if len(groups) != limit {
		return fmt.Errorf("pagination fixture returned %d groups, expected %d", len(groups), limit)
	}
	matches := 0
	for _, group := range groups {
		if group.Name != nil && *group.Name == query {
			matches++
			if group.Id == nil || group.Id.String() != expectedID {
				return fmt.Errorf("unexpected exact match on the first page")
			}
		}
	}
	if matches != expectedMatches {
		return fmt.Errorf("first page has %d exact matches, expected %d", matches, expectedMatches)
	}
	return nil
}
