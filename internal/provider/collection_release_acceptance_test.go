// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/glitchedmob/terraform-provider-outline/internal/client"
	"github.com/google/uuid"
	"github.com/hashicorp/terraform-exec/tfexec"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/oapi-codegen/nullable"
	tcexec "github.com/testcontainers/testcontainers-go/exec"
)

// Outline 1.10.1's collections.list excludes archives only when statusFilter
// is omitted. Admin includeListOnly bypasses the caller's collection IDs.
// CollectionsUpdateSchema accepts CollectionPermission.Admin, and update
// assigns it as the default. Exercise those released routes, not mock responses.
func TestAccCollectionArchiveAndAdminDefault(t *testing.T) {
	api := newAcceptanceAPI(t)
	api.acceptanceCollectionAnchor(t)
	const duplicateName = "Terraform collection release duplicate"
	const adminName = "Terraform collection release admin default"

	create := func(name string) string {
		t.Helper()
		id, err := api.acceptanceCreateCollection(name)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			c := acceptanceReleaseCollection(t, api, id)
			if !c.ArchivedAt.IsNull() {
				ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
				defer cancel()
				if err := acceptanceReleaseRestore(ctx, api, id); err != nil {
					t.Errorf("restore archived fixture before cleanup: %v", err)
					return
				}
			}
			if !c.Permission.IsNull() {
				if err := api.acceptanceUpdateCollection(client.CollectionsUpdateJSONRequestBody{
					Id: uuid.MustParse(id), Permission: nullable.NewNullNullable[client.Permission](),
				}); err != nil {
					t.Errorf("restore fixture default permission: %v", err)
					return
				}
			}
			if err := api.acceptanceDeleteCollection(id); err != nil {
				t.Errorf("delete restored release fixture: %v", err)
			}
		})
		return id
	}
	activeID, archivedID, adminID := create(duplicateName), create(duplicateName), create(adminName)

	// Keep the private archive outside the caller's memberships. Listing it
	// requires admin includeListOnly as well as the explicit empty status filter.
	actor, err := api.acceptanceUserActor()
	if err != nil {
		t.Fatal(err)
	}
	users, err := api.acceptanceInviteUsers([]client.Invite{{
		Email: "collection-release-manager@example.invalid", Name: "Collection release manager", Role: client.UserRoleMember,
	}})
	if err != nil || len(users) != 1 || users[0].Id == nil {
		t.Fatalf("invite archive manager: %v", err)
	}
	permission := client.PermissionAdmin
	grant, err := api.CollectionsAddUserWithResponse(t.Context(), client.CollectionsAddUserJSONRequestBody{
		Id: uuid.MustParse(archivedID), UserId: *users[0].Id, Permission: &permission,
	})
	if err != nil || grant == nil || grant.StatusCode() != http.StatusOK || grant.JSON200 == nil {
		t.Fatalf("add archive manager: %v, response %v", err, grant)
	}
	if err := api.acceptanceCollectionMembership(archivedID, users[0].Id.String(), "admin"); err != nil {
		t.Fatal(err)
	}
	if err := api.acceptanceRemoveCollectionCaller(archivedID, actor.Id.String()); err != nil {
		t.Fatal(err)
	}
	api.acceptanceCollectionFixture(t, "archive", archivedID)
	archived := acceptanceReleaseCollection(t, api, archivedID)
	active := acceptanceReleaseCollection(t, api, activeID)
	if archived.ArchivedAt.IsNull() || !active.ArchivedAt.IsNull() || *archived.Name != duplicateName ||
		*active.Name != duplicateName || !archived.Permission.IsNull() {
		t.Fatal("fixture must have an active and a private archived collection with the same exact name")
	}

	if err := api.acceptanceUpdateCollection(client.CollectionsUpdateJSONRequestBody{
		Id: uuid.MustParse(adminID), Permission: nullable.NewNullableWithValue(client.PermissionAdmin),
	}); err != nil {
		t.Fatal(err)
	}
	admin := acceptanceReleaseCollection(t, api, adminID)
	if admin.Permission.IsNull() || admin.Permission.GetOrEmpty() != client.PermissionAdmin || !admin.ArchivedAt.IsNull() {
		t.Fatal("released collections.update did not persist the explicit admin default")
	}

	emptyStatuses := []client.CollectionStatus{}
	for _, test := range []struct {
		name     string
		include  bool
		statuses *[]client.CollectionStatus
		found    bool
	}{
		{"admin with []", true, &emptyStatuses, true},
		{"admin with omitted statusFilter", true, nil, false},
		{"membership list with []", false, &emptyStatuses, false},
	} {
		listed := acceptanceReleaseCollectionList(t, api, test.include, test.statuses)
		c, found := listed[archivedID]
		if found != test.found || found && (c.ArchivedAt.IsNull() || *c.Name != duplicateName || !c.Permission.IsNull()) {
			t.Fatalf("%s: private archive present=%t, want %t", test.name, found, test.found)
		}
		if test.include {
			if c, found := listed[activeID]; !found || !c.ArchivedAt.IsNull() || *c.Name != duplicateName {
				t.Fatalf("%s: active duplicate missing or archived", test.name)
			}
			if c, found := listed[adminID]; !found || c.Permission.IsNull() || c.Permission.GetOrEmpty() != client.PermissionAdmin {
				t.Fatalf("%s: admin default missing from released list", test.name)
			}
		}
	}

	t.Run("lookups", func(t *testing.T) {
		byName := api.providerConfig + fmt.Sprintf(`data "outline_collection" "archived_name" { name = %q }`, duplicateName)
		byID := api.providerConfig + fmt.Sprintf(`
data "outline_collection" "archived_id" { id = %q }
data "outline_collection" "active_id" { id = %q }
data "outline_collection" "admin_id" { id = %q }
data "outline_collection" "admin_name" { name = %q }
`, archivedID, activeID, adminID, adminName)
		resource.Test(t, resource.TestCase{
			ProtoV6ProviderFactories: acceptanceProviderFactories(),
			Steps: []resource.TestStep{
				{Config: byName, ExpectError: regexp.MustCompile(`ambiguous collection name`)},
				{Config: byID, Check: resource.ComposeAggregateTestCheckFunc(
					acceptanceReleaseLookupCheck("data.outline_collection.archived_id", archived),
					acceptanceReleaseLookupCheck("data.outline_collection.active_id", active),
					acceptanceReleaseLookupCheck("data.outline_collection.admin_id", admin),
					acceptanceReleaseLookupCheck("data.outline_collection.admin_name", admin),
				)},
				{PreConfig: func() {
					name := "Terraform collection release active renamed"
					if err := api.acceptanceUpdateCollection(client.CollectionsUpdateJSONRequestBody{
						Id: uuid.MustParse(activeID), Name: &name, Permission: nullable.NewNullNullable[client.Permission](),
					}); err != nil {
						t.Fatal(err)
					}
				}, Config: byName + fmt.Sprintf(`
data "outline_collection" "archived_id" { id = %q }
`, archivedID), Check: resource.ComposeAggregateTestCheckFunc(
					acceptanceReleaseLookupCheck("data.outline_collection.archived_name", archived),
					acceptanceReleaseLookupCheck("data.outline_collection.archived_id", archived),
				)},
			},
		})
	})

	t.Run("import and refresh retain state", func(t *testing.T) {
		// Use the CLI so state pull checks the persisted state immediately after
		// each rejection, while the remote object is still unsupported.
		const providerAddress = "registry.terraform.io/glitchedmob/outline"
		reattach := groupPartialCreateProvider(t, providerAddress)
		config := fmt.Sprintf(`terraform {
  required_providers { outline = { source = %q } }
}
`, providerAddress) + api.acceptanceCollectionConfig("Terraform collection release managed", `
  allow_destroy = true
  description = "Retain **this state** after failed reads."
  sharing = true
`)
		tf := groupPartialCreateTerraform(t, config)
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
		defer cancel()
		if err := tf.Init(ctx, tfexec.Reattach(reattach)); err != nil {
			t.Fatal(err)
		}
		var managedID string
		cleanedUp := false
		t.Cleanup(func() {
			if cleanedUp || managedID == "" {
				return
			}
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Minute)
			defer cleanupCancel()
			c := acceptanceReleaseCollection(t, api, managedID)
			if !c.ArchivedAt.IsNull() {
				if err := acceptanceReleaseRestore(cleanupCtx, api, managedID); err != nil {
					t.Errorf("restore managed archive before cleanup: %v", err)
					return
				}
			}
			if !c.Permission.IsNull() {
				if err := api.acceptanceUpdateCollection(client.CollectionsUpdateJSONRequestBody{
					Id: uuid.MustParse(managedID), Permission: nullable.NewNullNullable[client.Permission](),
				}); err != nil {
					t.Errorf("restore managed default before cleanup: %v", err)
					return
				}
			}
			if err := os.WriteFile(filepath.Join(tf.WorkingDir(), "main.tf"), []byte(config), 0o600); err != nil {
				t.Error(err)
				return
			}
			if err := tf.Destroy(cleanupCtx, tfexec.Reattach(reattach)); err != nil {
				t.Errorf("destroy restored collection: %v", err)
			}
		})
		if err := tf.Apply(ctx, tfexec.Reattach(reattach)); err != nil {
			t.Fatal(err)
		}
		baseline := acceptanceReleaseState(t, ctx, tf)
		if len(baseline) != 1 || baseline[0].Mode != "managed" || baseline[0].Type != "outline_collection" ||
			baseline[0].Name != "test" || len(baseline[0].Instances) != 1 {
			t.Fatalf("expected one managed collection in CLI state: %+v", baseline)
		}
		attrs := baseline[0].Instances[0].Attributes
		managedID, _ = attrs["id"].(string)
		if _, err := uuid.Parse(managedID); err != nil || attrs["permission"] != nil || attrs["allow_destroy"] != true ||
			attrs["sharing"] != true || attrs["description"] != "Retain **this state** after failed reads." {
			t.Fatalf("incomplete baseline state: %+v", attrs)
		}
		retained := func() {
			t.Helper()
			if state := acceptanceReleaseState(t, ctx, tf); !reflect.DeepEqual(state, baseline) {
				t.Fatalf("rejection changed persisted collection state before restoration: before=%+v after=%+v", baseline, state)
			}
		}
		rejected := func(err error, want string) {
			t.Helper()
			// Terraform wraps diagnostic text to the terminal width.
			text := ""
			if err != nil {
				text = strings.Join(strings.Fields(err.Error()), " ")
			}
			if !strings.Contains(text, "Unable to read collection") || !strings.Contains(text, want) {
				t.Fatalf("expected unsupported collection read %q, got %v", want, err)
			}
			retained()
		}
		const archiveError = "archived collections cannot be managed by this resource"
		const adminError = "admin default permission is not supported by this resource"
		importConfig := config + `
resource "outline_collection" "rejected" { name = "Must never enter state" }
`
		if err := os.WriteFile(filepath.Join(tf.WorkingDir(), "main.tf"), []byte(importConfig), 0o600); err != nil {
			t.Fatal(err)
		}
		rejected(tf.Import(ctx, "outline_collection.rejected", archivedID, tfexec.Reattach(reattach)), archiveError)
		rejected(tf.Import(ctx, "outline_collection.rejected", adminID, tfexec.Reattach(reattach)), adminError)
		if err := os.WriteFile(filepath.Join(tf.WorkingDir(), "main.tf"), []byte(config), 0o600); err != nil {
			t.Fatal(err)
		}

		api.acceptanceCollectionFixture(t, "archive", managedID)
		if acceptanceReleaseCollection(t, api, managedID).ArchivedAt.IsNull() {
			t.Fatal("managed collection was not actually archived")
		}
		rejected(tf.Refresh(ctx, tfexec.Reattach(reattach)), archiveError)
		api.acceptanceCollectionFixture(t, "restore", managedID)
		if err := tf.Refresh(ctx, tfexec.Reattach(reattach)); err != nil {
			t.Fatal(err)
		}
		retained()
		if changed, err := tf.Plan(ctx, tfexec.Reattach(reattach)); err != nil || changed {
			t.Fatalf("restored archive must have an empty plan: changed=%t err=%v", changed, err)
		}

		if err := api.acceptanceUpdateCollection(client.CollectionsUpdateJSONRequestBody{
			Id: uuid.MustParse(managedID), Permission: nullable.NewNullableWithValue(client.PermissionAdmin),
		}); err != nil {
			t.Fatal(err)
		}
		if c := acceptanceReleaseCollection(t, api, managedID); c.Permission.IsNull() || c.Permission.GetOrEmpty() != client.PermissionAdmin {
			t.Fatal("managed collection did not actually acquire the admin default")
		}
		rejected(tf.Refresh(ctx, tfexec.Reattach(reattach)), adminError)
		if err := api.acceptanceUpdateCollection(client.CollectionsUpdateJSONRequestBody{
			Id: uuid.MustParse(managedID), Permission: nullable.NewNullNullable[client.Permission](),
		}); err != nil {
			t.Fatal(err)
		}
		if err := tf.Refresh(ctx, tfexec.Reattach(reattach)); err != nil {
			t.Fatal(err)
		}
		retained()
		if changed, err := tf.Plan(ctx, tfexec.Reattach(reattach)); err != nil || changed {
			t.Fatalf("restored default must have an empty plan: changed=%t err=%v", changed, err)
		}
		if err := tf.Destroy(ctx, tfexec.Reattach(reattach)); err != nil {
			t.Fatal(err)
		}
		cleanedUp = true
		if state := acceptanceReleaseState(t, ctx, tf); len(state) != 0 {
			t.Fatalf("destroy left collection state: %+v", state)
		}
		if err := api.acceptanceCollectionAbsent(managedID); err != nil {
			t.Fatal(err)
		}
	})

	api.acceptanceCollectionFixture(t, "restore", archivedID)
	if err := api.acceptanceUpdateCollection(client.CollectionsUpdateJSONRequestBody{
		Id: uuid.MustParse(adminID), Permission: nullable.NewNullNullable[client.Permission](),
	}); err != nil {
		t.Fatal(err)
	}
}

// Unlike acceptanceCollection, this oracle accepts archive metadata and admin
// defaults. It never calls the provider's lookup or managedCollection helpers.
func acceptanceReleaseCollection(t *testing.T, api *acceptanceAPI, id string) *client.Collection {
	t.Helper()
	// Cleanup runs after t.Context is canceled, but must restore before delete.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r, err := api.CollectionsInfoWithResponse(ctx, client.CollectionsInfoJSONRequestBody{Id: uuid.MustParse(id)})
	if err != nil || r == nil {
		t.Fatalf("release collections.info: %v", err)
	}
	if r.StatusCode() != http.StatusOK || r.JSON200 == nil || r.JSON200.Data == nil {
		t.Fatalf("release collections.info: HTTP %d: %s", r.StatusCode(), r.Body)
	}
	if err := checkEnvelope("release collections.info", r.JSON200.Ok, r.JSON200.Status); err != nil {
		t.Fatal(err)
	}
	c := r.JSON200.Data
	if c.Id == nil || c.Id.String() != id || c.Name == nil || c.Sharing == nil || !c.Description.IsSpecified() ||
		!c.Permission.IsSpecified() || !c.ArchivedAt.IsSpecified() || !c.DeletedAt.IsNull() {
		t.Fatalf("release collections.info returned incomplete or deleted metadata for %s", id)
	}
	return c
}

func acceptanceReleaseCollectionList(t *testing.T, api *acceptanceAPI, include bool, statuses *[]client.CollectionStatus) map[string]client.Collection {
	t.Helper()
	limit, offset := 100, 0
	//nolint:staticcheck // Compare [] and omission on the released statusFilter route.
	r, err := api.CollectionsListWithResponse(t.Context(), client.CollectionsListJSONRequestBody{
		Limit: &limit, Offset: &offset, IncludeListOnly: &include, StatusFilter: statuses,
	})
	if err != nil || r == nil {
		t.Fatalf("release collections.list: %v", err)
	}
	if r.StatusCode() != http.StatusOK || r.JSON200 == nil || r.JSON200.Data == nil {
		t.Fatalf("release collections.list: HTTP %d: %s", r.StatusCode(), r.Body)
	}
	if err := checkEnvelope("release collections.list", r.JSON200.Ok, r.JSON200.Status); err != nil {
		t.Fatal(err)
	}
	p := r.JSON200.Pagination
	if p == nil || p.Limit == nil || *p.Limit != limit || p.Offset == nil || *p.Offset != 0 ||
		p.Total == nil || *p.Total != len(*r.JSON200.Data) || *p.Total > limit {
		t.Fatal("release fixture must fit on one complete list page")
	}
	result := make(map[string]client.Collection)
	for _, c := range *r.JSON200.Data {
		if c.Id == nil || *c.Id == uuid.Nil || c.Name == nil || !c.ArchivedAt.IsSpecified() ||
			!c.Permission.IsSpecified() || !c.DeletedAt.IsNull() {
			t.Fatal("release collections.list returned incomplete or deleted metadata")
		}
		if _, exists := result[c.Id.String()]; exists {
			t.Fatal("release collections.list repeated a UUID")
		}
		result[c.Id.String()] = c
	}
	return result
}

func acceptanceReleaseLookupCheck(address string, c *client.Collection) resource.TestCheckFunc {
	permission := resource.TestCheckNoResourceAttr(address, "permission")
	if !c.Permission.IsNull() {
		permission = resource.TestCheckResourceAttr(address, "permission", string(c.Permission.GetOrEmpty()))
	}
	return resource.ComposeAggregateTestCheckFunc(
		resource.TestCheckResourceAttr(address, "id", c.Id.String()),
		resource.TestCheckResourceAttr(address, "name", *c.Name),
		resource.TestCheckResourceAttr(address, "description", c.Description.GetOrEmpty()),
		resource.TestCheckResourceAttr(address, "sharing", fmt.Sprint(*c.Sharing)), permission,
	)
}

type acceptanceReleaseResourceState struct {
	Mode, Type, Name string
	Instances        []struct {
		Status, Deposed string
		Attributes      map[string]any
	}
}

// The archive action already copied this guarded script. Cleanup uses its own
// context because testing cancels t.Context before running cleanup functions.
func acceptanceReleaseRestore(ctx context.Context, api *acceptanceAPI, id string) error {
	code, output, err := api.container.Exec(ctx, []string{
		"node", "/opt/outline/acceptance-collection-fixture.cjs", "restore", id,
	}, tcexec.Multiplexed())
	if err != nil {
		return err
	}
	data, err := io.ReadAll(output)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("restore fixture exited %d: %s", code, data)
	}
	return nil
}

func acceptanceReleaseState(t *testing.T, ctx context.Context, tf *tfexec.Terraform) []acceptanceReleaseResourceState {
	t.Helper()
	raw, err := tf.StatePull(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var state struct {
		Resources []acceptanceReleaseResourceState
	}
	if err := json.Unmarshal([]byte(raw), &state); err != nil {
		t.Fatal(err)
	}
	return state.Resources
}
