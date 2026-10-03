// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/glitchedmob/terraform-provider-outline/internal/client"
	"github.com/google/uuid"
	"github.com/hashicorp/terraform-exec/tfexec"
)

// Run this last in TestAccCollectionGroup. Creating the second workspace makes
// the ORM fixtures' one-team guard refuse subsequent archive and sync actions.
func testAccCollectionGroupForbidden(t *testing.T, api *acceptanceAPI) {
	if api == nil || api.apiClient == nil || api.container == nil || api.providerConfig == "" {
		t.Fatal("forbidden grant acceptance requires the disposable Outline stack")
	}
	collection, group := acceptanceCollectionGroupParents(t, api, "forbidden")
	other, err := api.acceptanceCreateGroup("Terraform collection group forbidden unrelated")
	if err != nil {
		t.Fatal(err)
	}
	createTarget, err := api.acceptanceCreateGroup("Terraform collection group forbidden create target")
	if err != nil {
		t.Fatal(err)
	}
	actor, err := api.acceptanceUserActor()
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{group, other, createTarget} {
		if err := api.acceptanceWriteGroupMember(id, actor.Id.String(), client.GroupPermissionMember, true); err != nil {
			t.Fatal(err)
		}
	}
	if err := api.acceptanceWriteCollectionGroup(collection, group, "read"); err != nil {
		t.Fatal(err)
	}
	if err := api.acceptanceWriteCollectionGroup(collection, other, "admin"); err != nil {
		t.Fatal(err)
	}
	managed := []string{group, createTarget}
	baseline, err := api.acceptanceCollectionGroupSnapshot(collection, managed)
	if err != nil {
		t.Fatal(err)
	}
	grantsBaseline, err := api.acceptanceCollectionGroups(collection)
	if err != nil {
		t.Fatal(err)
	}
	if grantsBaseline[group].Permission != "read" || grantsBaseline[other].Permission != "admin" {
		t.Fatal("forbidden fixture must contain both the managed and unrelated explicit grants")
	}
	if _, exists := grantsBaseline[createTarget]; exists {
		t.Fatal("forbidden create target must not already have a grant")
	}
	// The same group has another grant. A rejected operation must not search
	// or mutate grants on another collection by group ID alone.
	cross, err := api.acceptanceCreateCollection("Terraform collection group forbidden other collection")
	if err != nil {
		t.Fatal(err)
	}
	if err := api.acceptanceWriteCollectionGroup(cross, group, "read_write"); err != nil {
		t.Fatal(err)
	}
	crossBaseline, err := api.acceptanceCollectionGroupSnapshot(cross, nil)
	if err != nil {
		t.Fatal(err)
	}
	collateral := func() {
		t.Helper()
		if c := acceptanceReleaseCollection(t, api, collection); !c.ArchivedAt.IsNull() {
			t.Fatal("forbidden operation archived the collection")
		}
		if err := api.acceptanceCollectionGroupCollateral(collection, managed, baseline); err != nil {
			t.Fatal(err)
		}
		if err := api.acceptanceCollectionGroupCollateral(cross, nil, crossBaseline); err != nil {
			t.Fatal(err)
		}
	}
	remoteRetained := func() {
		t.Helper()
		actual, err := api.acceptanceCollectionGroups(collection)
		if err != nil || !reflect.DeepEqual(actual, grantsBaseline) {
			t.Fatalf("forbidden operation changed explicit grants: before=%v after=%v err=%v", grantsBaseline, actual, err)
		}
		collateral()
	}

	// collection-fixture.cjs checks the release, the sole disposable team, and
	// its bootstrap admin before creating an unrestricted foreign admin key.
	fixture := api.acceptanceCollectionFixture(t, "workspace", "")
	if fixture.APIKey == "" || fixture.APIKey == api.apiKey {
		t.Fatal("foreign workspace fixture did not return a distinct API key")
	}
	foreignClient, err := newAPIClient(api.baseURL, fixture.APIKey, 30, "acceptance")
	if err != nil {
		t.Fatal(err)
	}
	foreign := &acceptanceAPI{apiClient: foreignClient}
	foreignActor, err := foreign.acceptanceUserActor()
	if err != nil {
		t.Fatal(err)
	}
	if *foreignActor.Id == *actor.Id || foreignActor.Email.GetOrEmpty() != "terraform-foreign@example.invalid" {
		t.Fatal("foreign key must authenticate the fixture's different active admin")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	collectionID, groupID := uuid.MustParse(collection), uuid.MustParse(group)
	forbidden := func(operation string, status int, response *client.Error, body []byte, requestErr error) {
		t.Helper()
		if requestErr != nil || status != http.StatusForbidden || response == nil ||
			response.Ok == nil || *response.Ok || response.Status == nil || *response.Status != http.StatusForbidden ||
			response.Error == nil || *response.Error != "authorization_error" || response.Message == nil || *response.Message == "" {
			t.Fatalf("existing local collection must return %s HTTP 403 authorization_error: err=%v status=%d body=%s", operation, requestErr, status, body)
		}
	}
	info, err := foreign.CollectionsInfoWithResponse(ctx, client.CollectionsInfoJSONRequestBody{Id: collectionID})
	if info == nil {
		t.Fatalf("foreign collections.info returned no response: %v", err)
	}
	forbidden("collections.info", info.StatusCode(), info.JSON403, info.Body, err)
	limit, offset := 100, 0
	listed, err := foreign.CollectionsGroupMembershipsWithResponse(ctx, client.CollectionsGroupMembershipsJSONRequestBody{
		Id: collectionID, Limit: &limit, Offset: &offset,
	})
	if listed == nil {
		t.Fatalf("foreign collections.group_memberships returned no response: %v", err)
	}
	forbidden("collections.group_memberships", listed.StatusCode(), listed.JSON403, listed.Body, err)
	// Check the core error classification too. Neither the parent observation
	// nor a denied grant page is proof that this known existing pair is absent.
	coreRejected := func(operation string, err error) {
		t.Helper()
		if err == nil || errors.Is(err, errNotFound) || !strings.Contains(err.Error(), operation+": HTTP 403") ||
			!strings.Contains(err.Error(), "authorization_error") {
			t.Fatalf("real %s 403 became absence or lost its authorization error: %v", operation, err)
		}
	}
	_, err = foreign.observeCollectionGroup(ctx, collectionID, groupID)
	coreRejected("collections.info", err)
	_, err = foreign.readCollectionGroupPages(ctx, collectionID, groupID)
	coreRejected("collections.group_memberships", err)
	remoteRetained()

	const providerAddress = "registry.terraform.io/glitchedmob/outline"
	reattach := groupPartialCreateProvider(t, providerAddress)
	config := fmt.Sprintf(`terraform {
  required_providers { outline = { source = %q } }
}
`, providerAddress) + api.acceptanceCollectionGroupConfig(collection, group, "read")
	wrongWorkspace := strings.Replace(config, api.apiKey, fixture.APIKey, 1)
	if wrongWorkspace == config {
		t.Fatal("foreign provider config did not replace the disposable admin key")
	}
	tf := groupPartialCreateTerraform(t, config)
	writeConfig := func(value string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(tf.WorkingDir(), "main.tf"), []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := tf.Init(ctx, tfexec.Reattach(reattach)); err != nil {
		t.Fatal(err)
	}
	imported, cleanedUp := false, false
	t.Cleanup(func() {
		if !imported || cleanedUp {
			return
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Minute)
		defer cleanupCancel()
		// Always restore the local key, including after a failed saved-plan apply.
		if err := os.WriteFile(filepath.Join(tf.WorkingDir(), "main.tf"), []byte(config), 0o600); err != nil {
			t.Error(err)
			return
		}
		if err := tf.Destroy(cleanupCtx, tfexec.Reattach(reattach)); err != nil {
			t.Errorf("destroy local collection group fixture: %v", err)
		}
	})
	if err := tf.Import(ctx, acceptanceCollectionGroupAddress, collection+"/"+group, tfexec.Reattach(reattach)); err != nil {
		t.Fatal(err)
	}
	imported = true
	stateBaseline := acceptanceReleaseState(t, ctx, tf)
	if len(stateBaseline) != 1 || stateBaseline[0].Mode != "managed" || stateBaseline[0].Type != "outline_collection_group" ||
		stateBaseline[0].Name != "test" || len(stateBaseline[0].Instances) != 1 {
		t.Fatalf("expected one imported collection group in CLI state: %+v", stateBaseline)
	}
	instance := stateBaseline[0].Instances[0]
	attrs := instance.Attributes
	if instance.Status != "" || instance.Deposed != "" || attrs["id"] != collection+"/"+group ||
		attrs["collection_id"] != collection || attrs["group_id"] != group || attrs["permission"] != "read" {
		t.Fatalf("incomplete or tainted imported grant baseline: %+v", instance)
	}
	retained := func() {
		t.Helper()
		if actual := acceptanceReleaseState(t, ctx, tf); !reflect.DeepEqual(actual, stateBaseline) {
			t.Fatalf("forbidden rejection changed persisted grant state: before=%+v after=%+v", stateBaseline, actual)
		}
		remoteRetained()
	}
	rejected := func(err error, operation string) {
		t.Helper()
		text := ""
		if err != nil {
			// Terraform wraps diagnostics at terminal width.
			text = strings.Join(strings.Fields(err.Error()), " ")
		}
		if !strings.Contains(text, "Unable to "+operation+" collection group") ||
			!strings.Contains(text, "collections.info: HTTP 403") || !strings.Contains(text, "authorization_error") {
			t.Fatalf("expected foreign-key %s refusal with collections.info HTTP 403 authorization_error, got %v", operation, err)
		}
		retained()
	}
	retained()
	if changed, err := tf.Plan(ctx, tfexec.Reattach(reattach)); err != nil || changed {
		t.Fatalf("imported local grant must have an empty plan: changed=%t err=%v", changed, err)
	}
	writeConfig(wrongWorkspace)
	rejected(tf.Refresh(ctx, tfexec.Reattach(reattach)), "read")
	rejectedResource := func(id string) string {
		return fmt.Sprintf(`
resource "outline_collection_group" "rejected" {
  collection_id = %q
  group_id = %q
  permission = "read_write"
}
`, collection, id)
	}
	writeConfig(wrongWorkspace + rejectedResource(group))
	rejected(tf.Import(ctx, "outline_collection_group.rejected", collection+"/"+group, tfexec.Reattach(reattach)), "import")
	// Skip refresh of the imported grant so this apply reaches Create for a
	// different, absent pair. It must not save even a guessed or tainted pair.
	writeConfig(wrongWorkspace + rejectedResource(createTarget))
	rejected(tf.Apply(ctx, tfexec.Reattach(reattach), tfexec.Refresh(false)), "create")

	for _, operation := range []string{"update", "delete"} {
		plannedConfig := wrongWorkspace
		if operation == "update" {
			plannedConfig = strings.Replace(plannedConfig, `permission = "read"`, `permission = "admin"`, 1)
		}
		writeConfig(plannedConfig)
		planPath := filepath.Join(tf.WorkingDir(), "forbidden-"+operation+".tfplan")
		// Saved plans embed provider configuration. Plan with the foreign key
		// and refresh=false, rather than swapping main.tf after a local-key plan.
		options := []tfexec.PlanOption{tfexec.Reattach(reattach), tfexec.Refresh(false), tfexec.Out(planPath)}
		if operation == "delete" {
			options = append(options, tfexec.Destroy(true))
		}
		if changed, err := tf.Plan(ctx, options...); err != nil || !changed {
			t.Fatalf("plan foreign-key %s without refresh: changed=%t err=%v", operation, changed, err)
		}
		plan, err := tf.ShowPlanFile(ctx, planPath, tfexec.Reattach(reattach))
		if err != nil {
			t.Fatal(err)
		}
		if len(plan.ResourceChanges) != 1 || plan.ResourceChanges[0].Address != acceptanceCollectionGroupAddress || plan.ResourceChanges[0].Change == nil {
			t.Fatalf("unexpected foreign-key %s plan: %+v", operation, plan.ResourceChanges)
		}
		actions := plan.ResourceChanges[0].Change.Actions
		if operation == "update" && !actions.Update() || operation == "delete" && !actions.Delete() {
			t.Fatalf("foreign-key %s plan must reach that resource operation, got %v", operation, actions)
		}
		rejected(tf.Apply(ctx, tfexec.Reattach(reattach), tfexec.DirOrPlan(planPath)), operation)
		writeConfig(config)
		if err := tf.Refresh(ctx, tfexec.Reattach(reattach)); err != nil {
			t.Fatal(err)
		}
		retained()
		if changed, err := tf.Plan(ctx, tfexec.Reattach(reattach)); err != nil || changed {
			t.Fatalf("restored local key must have an empty plan after failed %s: changed=%t err=%v", operation, changed, err)
		}
	}
	if err := tf.Destroy(ctx, tfexec.Reattach(reattach)); err != nil {
		t.Fatal(err)
	}
	cleanedUp = true
	if state := acceptanceReleaseState(t, ctx, tf); len(state) != 0 {
		t.Fatalf("local-key destroy left collection group state: %+v", state)
	}
	if err := api.acceptanceCollectionGroupPermission(collection, group, ""); err != nil {
		t.Fatal(err)
	}
	if err := api.acceptanceCollectionGroupPermission(collection, createTarget, ""); err != nil {
		t.Fatal(err)
	}
	collateral()
}
