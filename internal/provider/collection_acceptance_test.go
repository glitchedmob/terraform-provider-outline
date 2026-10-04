// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
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
	"github.com/oapi-codegen/nullable"
)

const acceptanceCollectionAddress = "outline_collection.test"

func (a *acceptanceAPI) acceptanceCollection(id string) (*client.Collection, error) {
	r, err := a.CollectionsInfoWithResponse(context.Background(), client.CollectionsInfoJSONRequestBody{Id: uuid.MustParse(id)})
	if err != nil {
		return nil, err
	}
	if r.StatusCode() != http.StatusOK || r.JSON200 == nil || r.JSON200.Data == nil {
		return nil, fmt.Errorf("collections.info %s: HTTP %d: %s", id, r.StatusCode(), r.Body)
	}
	if err := checkEnvelope("acceptance collections.info", r.JSON200.Ok, r.JSON200.Status); err != nil {
		return nil, err
	}
	c := r.JSON200.Data
	if c.Id == nil || c.Id.String() != id || c.Name == nil || c.Sharing == nil ||
		!c.Description.IsSpecified() || !c.Permission.IsSpecified() || !c.ArchivedAt.IsNull() || !c.DeletedAt.IsNull() {
		return nil, fmt.Errorf("collections.info %s returned incomplete or inactive metadata", id)
	}
	return c, nil
}

func (a *acceptanceAPI) acceptanceCreateCollection(name string) (string, error) {
	sharing := false
	r, err := a.CollectionsCreateWithResponse(context.Background(), client.CollectionsCreateJSONRequestBody{
		Name: name, Permission: nullable.NewNullNullable[client.Permission](), Sharing: &sharing,
		Description: nullable.NewNullableWithValue(""),
	})
	if err != nil {
		return "", err
	}
	if r.StatusCode() != http.StatusOK || r.JSON200 == nil || r.JSON200.Data == nil || r.JSON200.Data.Id == nil {
		return "", fmt.Errorf("collections.create: HTTP %d: %s", r.StatusCode(), r.Body)
	}
	if err := checkEnvelope("acceptance collections.create", r.JSON200.Ok, r.JSON200.Status); err != nil {
		return "", err
	}
	return r.JSON200.Data.Id.String(), nil
}

func (a *acceptanceAPI) acceptanceUpdateCollection(body client.CollectionsUpdateJSONRequestBody) error {
	r, err := a.CollectionsUpdateWithResponse(context.Background(), body)
	if err != nil {
		return err
	}
	if r.StatusCode() != http.StatusOK || r.JSON200 == nil || r.JSON200.Data == nil || r.JSON200.Data.Id == nil || *r.JSON200.Data.Id != body.Id {
		return fmt.Errorf("collections.update: HTTP %d: %s", r.StatusCode(), r.Body)
	}
	return checkEnvelope("acceptance collections.update", r.JSON200.Ok, r.JSON200.Status)
}

func (a *acceptanceAPI) acceptanceDeleteCollection(id string) error {
	r, err := a.CollectionsDeleteWithResponse(context.Background(), client.CollectionsDeleteJSONRequestBody{Id: uuid.MustParse(id)})
	if err != nil {
		return err
	}
	if r.StatusCode() != http.StatusOK || r.JSON200 == nil || r.JSON200.Success == nil || !*r.JSON200.Success {
		return fmt.Errorf("collections.delete %s: HTTP %d: %s", id, r.StatusCode(), r.Body)
	}
	return checkEnvelope("acceptance collections.delete", r.JSON200.Ok, r.JSON200.Status)
}

func (a *acceptanceAPI) acceptanceCollectionAbsent(id string) error {
	r, err := a.CollectionsInfoWithResponse(context.Background(), client.CollectionsInfoJSONRequestBody{Id: uuid.MustParse(id)})
	if err != nil {
		return err
	}
	// Unlike groups.info, this release reports deleted collections as 404.
	// A 403 may refer to an existing collection in another workspace.
	if r.StatusCode() != http.StatusNotFound || r.JSON404 == nil || r.JSON404.Error == nil ||
		*r.JSON404.Error != "not_found" || r.JSON404.Ok == nil || *r.JSON404.Ok || r.JSON404.Status == nil || *r.JSON404.Status != http.StatusNotFound {
		return fmt.Errorf("collection %s is not proven absent: HTTP %d: %s", id, r.StatusCode(), r.Body)
	}
	return nil
}

func (a *acceptanceAPI) checkAcceptanceCollection(address string) resource.TestCheckFunc {
	return func(state *terraform.State) error {
		r, exists := state.RootModule().Resources[address]
		if !exists || r.Primary == nil || r.Primary.ID == "" {
			return fmt.Errorf("%s has no collection state", address)
		}
		c, err := a.acceptanceCollection(r.Primary.ID)
		if err != nil {
			return err
		}
		for attr, want := range map[string]string{
			"id": c.Id.String(), "name": *c.Name, "description": c.Description.GetOrEmpty(), "sharing": strconv.FormatBool(*c.Sharing),
		} {
			if got := r.Primary.Attributes[attr]; got != want {
				return fmt.Errorf("%s: API %s=%q, state=%q", address, attr, want, got)
			}
		}
		permission, present := r.Primary.Attributes["permission"]
		if c.Permission.IsNull() {
			if present {
				return fmt.Errorf("%s: private collection permission must be null, got %q", address, permission)
			}
		} else if permission != string(c.Permission.GetOrEmpty()) {
			return fmt.Errorf("%s: API permission=%q, state=%q", address, c.Permission.GetOrEmpty(), permission)
		}
		return nil
	}
}

func (a *acceptanceAPI) checkAcceptanceCollectionsDestroyed(state *terraform.State) error {
	for _, r := range state.RootModule().Resources {
		if r.Type == "outline_collection" && r.Primary != nil && r.Primary.ID != "" {
			if err := a.acceptanceCollectionAbsent(r.Primary.ID); err != nil {
				return err
			}
		}
	}
	return nil
}

func (a *acceptanceAPI) acceptanceCollectionConfig(name, extra string) string {
	return a.providerConfig + fmt.Sprintf(`
resource "outline_collection" "test" {
  name = %q
%s
}
`, name, extra)
}

func (a *acceptanceAPI) acceptanceCollectionAnchor(t *testing.T) {
	t.Helper()
	if _, err := a.acceptanceCreateCollection("Terraform collection cleanup anchor"); err != nil {
		t.Fatal(err)
	}
}

func TestAccCollectionLifecycle(t *testing.T) {
	api := newAcceptanceAPI(t)
	api.acceptanceCollectionAnchor(t)
	const address = acceptanceCollectionAddress
	const markdown = "# Collection notes\n\nManaged with **Terraform** and `code`."
	initial := api.acceptanceCollectionConfig("Terraform collection lifecycle", `
  allow_destroy = true
  sharing = true
  description = "# Collection notes\n\nManaged with **Terraform** and `+"`code`"+`."
`)
	read := api.acceptanceCollectionConfig("Terraform collection renamed", `
  allow_destroy = true
  permission = "read"
  sharing = true
  description = "# Updated notes\n\nA **Markdown** description."
`)
	readWrite := strings.Replace(read, `permission = "read"`, `permission = "read_write"`, 1)
	reset := api.acceptanceCollectionConfig("Terraform collection renamed", "  allow_destroy = true\n  permission = null")
	omitted := api.acceptanceCollectionConfig("Terraform collection renamed", `  allow_destroy = true`)
	var id string
	check := func(name, description, permission, sharing string) resource.TestCheckFunc {
		permissionCheck := resource.TestCheckNoResourceAttr(address, "permission")
		if permission != "" {
			permissionCheck = resource.TestCheckResourceAttr(address, "permission", permission)
		}
		return resource.ComposeAggregateTestCheckFunc(api.checkAcceptanceCollection(address),
			resource.TestCheckResourceAttr(address, "name", name), resource.TestCheckResourceAttr(address, "description", description),
			resource.TestCheckResourceAttr(address, "sharing", sharing), resource.TestCheckResourceAttr(address, "allow_destroy", "true"), permissionCheck,
			func(state *terraform.State) error {
				current := state.RootModule().Resources[address].Primary.ID
				if id != "" && id != current {
					return fmt.Errorf("collection update replaced %s with %s", id, current)
				}
				id = current
				return nil
			})
	}
	importStep := resource.TestStep{
		ResourceName: address, ImportState: true, ImportStateVerify: true, ImportStateVerifyIgnore: []string{"allow_destroy"},
		ImportStateCheck: func(states []*terraform.InstanceState) error {
			if len(states) != 1 || states[0].ID != id || states[0].Attributes["allow_destroy"] != "false" {
				return fmt.Errorf("collection UUID import must reset allow_destroy to false")
			}
			return nil
		},
	}
	updatePlan := resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(address, plancheck.ResourceActionUpdate)}}
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acceptanceProviderFactories(), CheckDestroy: api.checkAcceptanceCollectionsDestroyed,
		Steps: []resource.TestStep{
			{Config: initial, Check: check("Terraform collection lifecycle", markdown, "", "true")},
			importStep,
			{Config: initial, ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}}},
			{Config: read, ConfigPlanChecks: updatePlan, Check: check("Terraform collection renamed", "# Updated notes\n\nA **Markdown** description.", "read", "true")},
			{Config: readWrite, ConfigPlanChecks: updatePlan, Check: check("Terraform collection renamed", "# Updated notes\n\nA **Markdown** description.", "read_write", "true")},
			importStep,
			{PreConfig: func() {
				name, sharing := "Terraform collection external drift", false
				if err := api.acceptanceUpdateCollection(client.CollectionsUpdateJSONRequestBody{
					Id: uuid.MustParse(id), Name: &name, Sharing: &sharing, Description: nullable.NewNullableWithValue("External **Markdown**"),
					Permission: nullable.NewNullableWithValue(client.PermissionRead),
				}); err != nil {
					t.Fatal(err)
				}
			}, RefreshState: true, ExpectNonEmptyPlan: true, Check: check("Terraform collection external drift", "External **Markdown**", "read", "false")},
			{Config: readWrite, ConfigPlanChecks: updatePlan, Check: check("Terraform collection renamed", "# Updated notes\n\nA **Markdown** description.", "read_write", "true")},
			{Config: reset, ConfigPlanChecks: updatePlan, Check: check("Terraform collection renamed", "", "", "false")},
			{Config: omitted, ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}}, Check: check("Terraform collection renamed", "", "", "false")},
			{RefreshState: true, Check: check("Terraform collection renamed", "", "", "false")},
			{Config: api.providerConfig, Check: func(_ *terraform.State) error { return api.acceptanceCollectionAbsent(id) }},
		},
	})
}

func TestAccCollectionMissingStateRecreation(t *testing.T) {
	api := newAcceptanceAPI(t)
	api.acceptanceCollectionAnchor(t)
	const address = acceptanceCollectionAddress
	config := api.acceptanceCollectionConfig("Terraform collection recreate", `  allow_destroy = true`)
	var deletedID, recreatedID string
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acceptanceProviderFactories(), CheckDestroy: api.checkAcceptanceCollectionsDestroyed,
		Steps: []resource.TestStep{
			{Config: config, Check: resource.ComposeAggregateTestCheckFunc(api.checkAcceptanceCollection(address), func(state *terraform.State) error {
				deletedID = state.RootModule().Resources[address].Primary.ID
				return nil
			})},
			{PreConfig: func() {
				if err := api.acceptanceDeleteCollection(deletedID); err != nil {
					t.Fatal(err)
				}
			}, Config: config, ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(address, plancheck.ResourceActionCreate)}},
				Check: resource.ComposeAggregateTestCheckFunc(api.checkAcceptanceCollection(address), func(state *terraform.State) error {
					recreatedID = state.RootModule().Resources[address].Primary.ID
					if recreatedID == deletedID {
						return fmt.Errorf("remotely deleted collection was not recreated")
					}
					return api.acceptanceCollectionAbsent(deletedID)
				})},
			{Config: api.providerConfig, Check: func(_ *terraform.State) error { return api.acceptanceCollectionAbsent(recreatedID) }},
		},
	})
}

// Read every real list page independently of the provider's lookup helpers.
func (a *acceptanceAPI) acceptanceCollectionPages(includeListOnly bool) ([][]client.Collection, error) {
	limit, offset, total := 100, 0, -1
	statuses := []client.CollectionStatus{}
	seen := make(map[uuid.UUID]bool)
	var pages [][]client.Collection
	for {
		//nolint:staticcheck // Outline 1.10.1 uses statusFilter; [] includes archives.
		r, err := a.CollectionsListWithResponse(context.Background(), client.CollectionsListJSONRequestBody{
			Limit: &limit, Offset: &offset, IncludeListOnly: &includeListOnly, StatusFilter: &statuses,
		})
		if err != nil {
			return nil, err
		}
		if r.StatusCode() != http.StatusOK || r.JSON200 == nil || r.JSON200.Data == nil {
			return nil, fmt.Errorf("collections.list fixture: HTTP %d: %s", r.StatusCode(), r.Body)
		}
		if err := checkEnvelope("acceptance collections.list", r.JSON200.Ok, r.JSON200.Status); err != nil {
			return nil, err
		}
		p, collections := r.JSON200.Pagination, *r.JSON200.Data
		if p == nil || p.Limit == nil || *p.Limit != limit || p.Offset == nil || *p.Offset != offset || p.Total == nil ||
			*p.Total < 0 || total != -1 && total != *p.Total || len(collections) > limit || offset+len(collections) > *p.Total {
			return nil, fmt.Errorf("collections.list fixture returned inconsistent pagination")
		}
		total = *p.Total
		for _, c := range collections {
			if c.Id == nil || *c.Id == uuid.Nil || seen[*c.Id] || c.Name == nil {
				return nil, fmt.Errorf("collections.list fixture returned missing or repeated identities")
			}
			seen[*c.Id] = true
		}
		pages = append(pages, collections)
		offset += len(collections)
		if offset == total {
			return pages, nil
		}
		if len(collections) != limit {
			return nil, fmt.Errorf("collections.list fixture ended before total")
		}
	}
}

func TestAccCollectionDataSourceLookups(t *testing.T) {
	api := newAcceptanceAPI(t)
	const name = "Terraform collection exact target"
	id, err := api.acceptanceCreateCollection(name)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 101; i++ {
		if _, err := api.acceptanceCreateCollection(fmt.Sprintf("%s extra %03d", name, i)); err != nil {
			t.Fatal(err)
		}
	}
	// Use the actual second page's name and ID, without assuming server ordering.
	pages, err := api.acceptanceCollectionPages(true)
	if err != nil || len(pages) != 2 || len(pages[0]) != 100 || len(pages[1]) != 2 {
		t.Fatalf("expected a full first page and two collections on the second: %v", err)
	}
	lateID, lateName := pages[1][1].Id.String(), *pages[1][1].Name
	byName := func(lookup string) string {
		return api.providerConfig + fmt.Sprintf(`
data "outline_collection" "by_name" { name = %q }
`, lookup)
	}
	config := byName(lateName) + fmt.Sprintf(`
data "outline_collection" "by_id" { id = %q }
`, lateID)
	var duplicateID string
	missingID := uuid.NewString()
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acceptanceProviderFactories(),
		Steps: []resource.TestStep{
			{Config: config, Check: resource.ComposeAggregateTestCheckFunc(api.checkAcceptanceCollection("data.outline_collection.by_name"),
				api.checkAcceptanceCollection("data.outline_collection.by_id"), resource.TestCheckResourceAttr("data.outline_collection.by_name", "id", lateID),
				resource.TestCheckResourceAttr("data.outline_collection.by_id", "name", lateName))},
			{PreConfig: func() {
				// Duplicate collection names are valid in Outline. No ORM bypass.
				duplicate, err := api.acceptanceCreateCollection(lateName)
				if err != nil || duplicate == id || duplicate == lateID {
					t.Fatalf("create a valid duplicate collection: %v", err)
				}
				duplicateID = duplicate
				pages, err := api.acceptanceCollectionPages(true)
				if err != nil {
					t.Fatal(err)
				}
				matches := make([]int, len(pages))
				for i, page := range pages {
					for _, c := range page {
						if *c.Name == lateName {
							matches[i]++
						}
					}
				}
				if len(matches) != 2 || matches[0] != 1 || matches[1] != 1 {
					t.Fatalf("duplicate fixture must have one exact match on each page, got %v", matches)
				}
			}, Config: byName(lateName), ExpectError: regexp.MustCompile(`ambiguous collection name`)},
			{Config: byName(strings.ToLower(lateName)), ExpectError: regexp.MustCompile(`no collection found with the exact name`)},
			{Config: byName("Terraform collection exact"), ExpectError: regexp.MustCompile(`no collection found with the exact name`)},
			{PreConfig: func() {
				if err := api.acceptanceCollectionAbsent(missingID); err != nil {
					t.Fatal(err)
				}
			}, Config: api.providerConfig + fmt.Sprintf(`data "outline_collection" "missing" { id = %q }`, missingID), ExpectError: regexp.MustCompile(`collections.info: outline object not found`)},
			{PreConfig: func() {
				if err := api.acceptanceDeleteCollection(duplicateID); err != nil {
					t.Fatal(err)
				}
			}, Config: config, Check: api.checkAcceptanceCollection("data.outline_collection.by_name")},
		},
	})
}

func (a *acceptanceAPI) acceptanceCollectionMembership(id, user, permission string) error {
	limit, offset := 100, 0
	r, err := a.CollectionsMembershipsWithResponse(context.Background(), client.CollectionsMembershipsJSONRequestBody{
		Id: uuid.MustParse(id), Limit: &limit, Offset: &offset,
	})
	if err != nil {
		return err
	}
	if r.StatusCode() != http.StatusOK || r.JSON200 == nil || r.JSON200.Data == nil || r.JSON200.Data.Memberships == nil ||
		r.JSON200.Pagination == nil || r.JSON200.Pagination.Total == nil || *r.JSON200.Pagination.Total != len(*r.JSON200.Data.Memberships) {
		return fmt.Errorf("collections.memberships fixture must fit on one complete page: HTTP %d: %s", r.StatusCode(), r.Body)
	}
	if err := checkEnvelope("acceptance collections.memberships", r.JSON200.Ok, r.JSON200.Status); err != nil {
		return err
	}
	for _, m := range *r.JSON200.Data.Memberships {
		if m.UserId == nil || m.CollectionId.IsNull() || m.CollectionId.GetOrEmpty() != uuid.MustParse(id) || m.Permission == nil {
			return fmt.Errorf("collections.memberships returned an incomplete membership")
		}
		if m.UserId.String() == user {
			if permission == "" || string(*m.Permission) != permission {
				return fmt.Errorf("caller membership is %q, expected %q", *m.Permission, permission)
			}
			return nil
		}
	}
	if permission != "" {
		return fmt.Errorf("caller membership %q is missing", permission)
	}
	return nil
}

func (a *acceptanceAPI) acceptanceRemoveCollectionCaller(id, user string) error {
	r, err := a.CollectionsRemoveUserWithResponse(context.Background(), client.CollectionsRemoveUserJSONRequestBody{Id: uuid.MustParse(id), UserId: uuid.MustParse(user)})
	if err != nil {
		return err
	}
	if r.StatusCode() != http.StatusOK || r.JSON200 == nil || r.JSON200.Success == nil || !*r.JSON200.Success {
		return fmt.Errorf("collections.remove_user: HTTP %d: %s", r.StatusCode(), r.Body)
	}
	if err := checkEnvelope("acceptance collections.remove_user", r.JSON200.Ok, r.JSON200.Status); err != nil {
		return err
	}
	return a.acceptanceCollectionMembership(id, user, "")
}

func acceptanceCollectionAbility(policies *[]client.Policy, id, ability string, want bool) error {
	if policies != nil {
		for _, p := range *policies {
			if p.Id == nil || p.Id.String() != id || p.Abilities == nil {
				continue
			}
			value, exists := (*p.Abilities)[ability]
			if !exists {
				return fmt.Errorf("collection policy has no %s ability", ability)
			}
			got, err := value.AsAbility1()
			if err != nil || got != want {
				return fmt.Errorf("collection %s policy %s: got %v, want %t, decode error %v", id, ability, got, want, err)
			}
			return nil
		}
	}
	return fmt.Errorf("collection %s has no policy", id)
}

func TestAccCollectionAdminVisibilityAndUpdatePermission(t *testing.T) {
	api := newAcceptanceAPI(t)
	api.acceptanceCollectionAnchor(t)
	actor, err := api.acceptanceUserActor()
	if err != nil {
		t.Fatal(err)
	}
	user := actor.Id.String()
	users, err := api.acceptanceInviteUsers([]client.Invite{{Email: "collection-manager@example.invalid", Name: "Collection manager", Role: client.UserRoleMember}})
	if err != nil || len(users) != 1 || users[0].Id == nil {
		t.Fatalf("invite other collection manager: %v", err)
	}
	const address = acceptanceCollectionAddress
	private := api.acceptanceCollectionConfig("Terraform collection private visibility", `  allow_destroy = true`)
	var id string
	withoutCaller := func(_ *terraform.State) error { return api.acceptanceCollectionMembership(id, user, "") }
	checkPrivate := func(_ *terraform.State) error {
		if err := withoutCaller(nil); err != nil {
			return err
		}
		for _, include := range []bool{false, true} {
			pages, err := api.acceptanceCollectionPages(include)
			if err != nil {
				return err
			}
			found := false
			for _, page := range pages {
				for _, c := range page {
					found = found || c.Id.String() == id
				}
			}
			if found != include {
				return fmt.Errorf("private collection listed=%t with includeListOnly=%t", found, include)
			}
		}
		r, err := api.CollectionsInfoWithResponse(t.Context(), client.CollectionsInfoJSONRequestBody{Id: uuid.MustParse(id)})
		if err != nil {
			return err
		}
		if r.StatusCode() != http.StatusOK || r.JSON200 == nil {
			return fmt.Errorf("admin metadata read should remain allowed: HTTP %d: %s", r.StatusCode(), r.Body)
		}
		if err := acceptanceCollectionAbility(r.JSON200.Policies, id, "read", true); err != nil {
			return err
		}
		return acceptanceCollectionAbility(r.JSON200.Policies, id, "readDocument", false)
	}
	readWrite := strings.Replace(private, `  allow_destroy = true`, "  allow_destroy = true\n  permission = \"read_write\"", 1)
	renamed := strings.Replace(readWrite, "Terraform collection private visibility", "Terraform collection explicit permission update", 1)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acceptanceProviderFactories(), CheckDestroy: api.checkAcceptanceCollectionsDestroyed,
		Steps: []resource.TestStep{
			{Config: private, Check: func(state *terraform.State) error {
				id = state.RootModule().Resources[address].Primary.ID
				return api.acceptanceCollectionMembership(id, user, "admin")
			}},
			{PreConfig: func() {
				permission := client.PermissionAdmin
				r, err := api.CollectionsAddUserWithResponse(t.Context(), client.CollectionsAddUserJSONRequestBody{
					Id: uuid.MustParse(id), UserId: *users[0].Id, Permission: &permission,
				})
				if err != nil || r == nil || r.StatusCode() != http.StatusOK || r.JSON200 == nil || r.JSON200.Data == nil {
					t.Fatalf("add other collection manager: %v, response %v", err, r)
				}
				if err := api.acceptanceCollectionMembership(id, users[0].Id.String(), "admin"); err != nil {
					t.Fatal(err)
				}
				if err := api.acceptanceRemoveCollectionCaller(id, user); err != nil {
					t.Fatal(err)
				}
			}, Config: private + `
data "outline_collection" "private_name" { name = outline_collection.test.name }
data "outline_collection" "private_id" { id = outline_collection.test.id }
`, Check: resource.ComposeAggregateTestCheckFunc(api.checkAcceptanceCollection(address), api.checkAcceptanceCollection("data.outline_collection.private_name"),
				api.checkAcceptanceCollection("data.outline_collection.private_id"), checkPrivate)},
			{Config: readWrite, Check: resource.ComposeAggregateTestCheckFunc(api.checkAcceptanceCollection(address), withoutCaller)},
			{Config: renamed, ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(address, plancheck.ResourceActionUpdate)}},
				Check: resource.ComposeAggregateTestCheckFunc(api.checkAcceptanceCollection(address), withoutCaller)},
			{PreConfig: func() {
				// Control request deliberately omits permission. Outline preserves the
				// read_write default but adds a caller-admin grant on this update.
				name := "Terraform collection explicit permission update"
				if err := api.acceptanceUpdateCollection(client.CollectionsUpdateJSONRequestBody{Id: uuid.MustParse(id), Name: &name}); err != nil {
					t.Fatal(err)
				}
			}, Config: renamed, ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}},
				Check: resource.ComposeAggregateTestCheckFunc(api.checkAcceptanceCollection(address), resource.TestCheckResourceAttr(address, "permission", "read_write"),
					func(_ *terraform.State) error { return api.acceptanceCollectionMembership(id, user, "admin") })},
			{Config: api.providerConfig, Check: func(_ *terraform.State) error { return api.acceptanceCollectionAbsent(id) }},
		},
	})
}

// These are ORM fixture observations, not handwritten Outline API responses.
type acceptanceCollectionFixture struct {
	Version      string `json:"version"`
	APIKey       string `json:"api_key"`
	CollectionID string `json:"collection_id"`
	Deleted      bool   `json:"deleted"`
	Documents    []struct {
		ID           string  `json:"id"`
		Kind         string  `json:"kind"`
		CollectionID *string `json:"collection_id"`
		Published    bool    `json:"published"`
		Archived     bool    `json:"archived"`
		Deleted      bool    `json:"deleted"`
		DeletedByID  *string `json:"deleted_by_id"`
	} `json:"documents"`
	UserMemberships  []json.RawMessage `json:"user_memberships"`
	GroupMemberships []json.RawMessage `json:"group_memberships"`
}

func (a *acceptanceAPI) acceptanceCollectionFixture(t *testing.T, action, id string) acceptanceCollectionFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	const path = "/opt/outline/acceptance-collection-fixture.cjs"
	code, data, failure := runAcceptanceFixture(ctx, a.container, "../../integration/collection-fixture.cjs", path, action, id)
	if failure != nil && failure.step != "read" {
		t.Fatal(failure)
	}
	if failure != nil || code != 0 {
		t.Fatalf("collection fixture exited %d: %v\n%s", code, failure, data)
	}
	const marker = "OUTLINE_ACCEPTANCE_COLLECTION="
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, marker) {
			var result acceptanceCollectionFixture
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, marker)), &result); err != nil {
				t.Fatal(err)
			}
			if result.Version != "1.10.1" {
				t.Fatalf("collection fixture ran on unsupported version %q", result.Version)
			}
			return result
		}
	}
	t.Fatalf("collection fixture did not return observations:\n%s", data)
	return acceptanceCollectionFixture{}
}

func TestAccCollectionDeletionSemantics(t *testing.T) {
	api := newAcceptanceAPI(t)
	api.acceptanceCollectionAnchor(t)
	actor, err := api.acceptanceUserActor()
	if err != nil {
		t.Fatal(err)
	}
	group, err := api.acceptanceCreateGroup("Terraform collection retained group grant")
	if err != nil {
		t.Fatal(err)
	}
	var id string
	var before acceptanceCollectionFixture
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acceptanceProviderFactories(), CheckDestroy: api.checkAcceptanceCollectionsDestroyed,
		Steps: []resource.TestStep{
			{Config: api.acceptanceCollectionConfig("Terraform collection deletion semantics", `  allow_destroy = true`), Check: func(state *terraform.State) error {
				id = state.RootModule().Resources[acceptanceCollectionAddress].Primary.ID
				permission := client.PermissionRead
				r, err := api.CollectionsAddGroupWithResponse(t.Context(), client.CollectionsAddGroupJSONRequestBody{
					Id: uuid.MustParse(id), GroupId: uuid.MustParse(group), Permission: &permission,
				})
				if err != nil || r == nil || r.StatusCode() != http.StatusOK || r.JSON200 == nil || r.JSON200.Data == nil {
					return fmt.Errorf("add retained group grant: %v, response %v", err, r)
				}
				before = api.acceptanceCollectionFixture(t, "seed", id)
				if before.CollectionID != id || before.Deleted || len(before.Documents) != 3 || len(before.UserMemberships) != 1 || len(before.GroupMemberships) != 1 {
					return fmt.Errorf("document and membership baseline is incomplete")
				}
				for _, doc := range before.Documents {
					if doc.Deleted || doc.CollectionID == nil || *doc.CollectionID != id || doc.Published != (doc.Kind != "draft") || doc.Archived != (doc.Kind == "archived") {
						return fmt.Errorf("invalid %s document baseline", doc.Kind)
					}
				}
				return nil
			}},
			{Config: api.providerConfig, Check: func(_ *terraform.State) error {
				if err := api.acceptanceCollectionAbsent(id); err != nil {
					return err
				}
				after := api.acceptanceCollectionFixture(t, "deleted", id)
				if after.CollectionID != id || !after.Deleted || !reflect.DeepEqual(before.UserMemberships, after.UserMemberships) || !reflect.DeepEqual(before.GroupMemberships, after.GroupMemberships) {
					return fmt.Errorf("collection delete must soft-delete the collection and retain unchanged user/group grant rows")
				}
				for i, doc := range after.Documents {
					if doc.ID != before.Documents[i].ID || doc.Kind != before.Documents[i].Kind || doc.Published != before.Documents[i].Published || doc.Archived != before.Documents[i].Archived {
						return fmt.Errorf("collection delete replaced or changed publication/archive status of a document")
					}
					switch doc.Kind {
					case "published":
						if !doc.Deleted || doc.CollectionID == nil || *doc.CollectionID != id || doc.DeletedByID == nil || *doc.DeletedByID != actor.Id.String() {
							return fmt.Errorf("published document must remain linked and be soft-deleted by the caller")
						}
					case "draft":
						if doc.Deleted || doc.CollectionID != nil {
							return fmt.Errorf("draft must survive and detach asynchronously")
						}
					case "archived":
						if doc.Deleted || doc.CollectionID == nil || *doc.CollectionID != id {
							return fmt.Errorf("archived document must remain linked and not be deleted")
						}
					default:
						return fmt.Errorf("unexpected deletion fixture document %q", doc.Kind)
					}
				}
				return nil
			}},
		},
	})
}

func TestAccCollectionDestroyGuards(t *testing.T) {
	api := newAcceptanceAPI(t)
	const address = acceptanceCollectionAddress
	config := api.acceptanceCollectionConfig("Terraform collection guarded last", "")
	allowed := api.acceptanceCollectionConfig("Terraform collection guarded last", `  allow_destroy = true`)
	var id string
	sameID := func(state *terraform.State) error {
		if state.RootModule().Resources[address].Primary.ID != id {
			return fmt.Errorf("failed delete lost the collection identity")
		}
		return api.checkAcceptanceCollection(address)(state)
	}
	emptyPlan := resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}}
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acceptanceProviderFactories(), CheckDestroy: api.checkAcceptanceCollectionsDestroyed,
		Steps: []resource.TestStep{
			{Config: config, Check: resource.ComposeAggregateTestCheckFunc(api.checkAcceptanceCollection(address),
				resource.TestCheckResourceAttr(address, "allow_destroy", "false"), resource.TestCheckResourceAttr(address, "sharing", "false"),
				resource.TestCheckResourceAttr(address, "description", ""), resource.TestCheckNoResourceAttr(address, "permission"), func(state *terraform.State) error {
					id = state.RootModule().Resources[address].Primary.ID
					return nil
				})},
			{Config: api.providerConfig, ExpectError: regexp.MustCompile(`(?s)Collection destruction blocked.*allow_destroy = true`)},
			{Config: config, ConfigPlanChecks: emptyPlan, Check: sameID},
			{Config: allowed, Check: resource.ComposeAggregateTestCheckFunc(sameID, resource.TestCheckResourceAttr(address, "allow_destroy", "true"))},
			{Config: api.providerConfig, ExpectError: regexp.MustCompile(`(?s)Unable to delete collection.*Cannot delete\s+last collection`)},
			{Config: allowed, ConfigPlanChecks: emptyPlan, Check: sameID},
			{PreConfig: func() { api.acceptanceCollectionAnchor(t) }, Config: api.providerConfig, Check: func(_ *terraform.State) error { return api.acceptanceCollectionAbsent(id) }},
		},
	})
}

func TestAccCollectionCrossWorkspaceForbidden(t *testing.T) {
	api := newAcceptanceAPI(t)
	api.acceptanceCollectionAnchor(t)
	fixture := api.acceptanceCollectionFixture(t, "workspace", "")
	if fixture.APIKey == "" {
		t.Fatal("foreign workspace fixture did not return an API key")
	}
	foreignClient, err := newAPIClient(api.baseURL, fixture.APIKey, 30, "acceptance")
	if err != nil {
		t.Fatal(err)
	}
	foreign := &acceptanceAPI{apiClient: foreignClient}
	foreignID, err := foreign.acceptanceCreateCollection("Terraform collection foreign workspace target")
	if err != nil {
		t.Fatal(err)
	}
	response, err := api.CollectionsInfoWithResponse(t.Context(), client.CollectionsInfoJSONRequestBody{Id: uuid.MustParse(foreignID)})
	if err != nil || response == nil || response.StatusCode() != http.StatusForbidden || response.JSON403 == nil || response.JSON403.Error == nil || *response.JSON403.Error != "authorization_error" {
		t.Fatalf("existing foreign collection must return 403 authorization_error: %v, response %v", err, response)
	}
	if _, err := foreign.acceptanceCollection(foreignID); err != nil {
		t.Fatal(err)
	}
	if _, err := api.readCollection(t.Context(), uuid.MustParse(foreignID)); err == nil || errors.Is(err, errNotFound) {
		t.Fatalf("a real cross-workspace 403 must not become missing: %v", err)
	}
	pages, err := api.acceptanceCollectionPages(true)
	if err != nil {
		t.Fatal(err)
	}
	for _, page := range pages {
		for _, c := range page {
			if c.Id.String() == foreignID {
				t.Fatal("foreign collection leaked into the complete admin workspace list")
			}
		}
	}
	const address = acceptanceCollectionAddress
	config := api.acceptanceCollectionConfig("Terraform collection retained forbidden state", `  allow_destroy = true`)
	wrongWorkspace := strings.Replace(config, api.apiKey, fixture.APIKey, 1)
	var id string
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acceptanceProviderFactories(), CheckDestroy: api.checkAcceptanceCollectionsDestroyed,
		Steps: []resource.TestStep{
			{Config: api.providerConfig + fmt.Sprintf(`data "outline_collection" "foreign" { id = %q }`, foreignID), ExpectError: regexp.MustCompile(`(?s)Unable to look up collection.*HTTP 403.*authorization_error`)},
			{Config: config, Check: func(state *terraform.State) error {
				id = state.RootModule().Resources[address].Primary.ID
				return api.checkAcceptanceCollection(address)(state)
			}},
			{Config: wrongWorkspace, ExpectError: regexp.MustCompile(`(?s)Unable to read collection.*HTTP 403.*authorization_error`)},
			{Config: config, ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}}, Check: func(state *terraform.State) error {
				if state.RootModule().Resources[address].Primary.ID != id {
					return fmt.Errorf("forbidden refresh discarded collection state")
				}
				return api.checkAcceptanceCollection(address)(state)
			}},
			{Config: api.providerConfig, Check: func(_ *terraform.State) error { return api.acceptanceCollectionAbsent(id) }},
		},
	})
}
