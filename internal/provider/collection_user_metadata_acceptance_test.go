// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"encoding/json"
	"fmt"
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
)

type acceptanceCollectionUserFixture struct {
	Version, Action  string
	CollectionID     string `json:"collection_id"`
	Archived         bool
	PreviousExport   *bool `json:"previous_export"`
	ViewersCanExport bool  `json:"viewers_can_export"`
}

// This helper can change only guarded metadata in this disposable release stack.
func (a *acceptanceAPI) acceptanceCollectionUserFixtureMetadata(ctx context.Context, action, id string) (*acceptanceCollectionUserFixture, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	const path = "/opt/outline/acceptance-collection-user-fixture.cjs"
	code, data, failure := runAcceptanceFixture(ctx, a.container, "../../integration/collection-user-fixture.cjs", path, action, id)
	if failure != nil {
		return nil, failure
	}
	if code != 0 {
		return nil, fmt.Errorf("Outline collection user metadata fixture exited %d: %s", code, data)
	}
	var fixture acceptanceCollectionUserFixture
	const marker = "OUTLINE_ACCEPTANCE_COLLECTION_USER="
	found := false
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, marker) {
			if found {
				return nil, fmt.Errorf("collection user metadata fixture repeated its result marker")
			}
			found = true
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, marker)), &fixture); err != nil {
				return nil, err
			}
		}
	}
	if !found || fixture.Version != "1.10.1" || fixture.Action != action || fixture.CollectionID != id {
		return nil, fmt.Errorf("collection user metadata fixture did not confirm release, action, and collection: %s", data)
	}
	if action == "archive" || action == "restore" {
		r, err := a.CollectionsInfoWithResponse(ctx, client.CollectionsInfoJSONRequestBody{Id: uuid.MustParse(id)})
		if err != nil {
			return nil, err
		}
		if r.StatusCode() != http.StatusOK || r.JSON200 == nil || r.JSON200.Data == nil ||
			!r.JSON200.Data.ArchivedAt.IsSpecified() || !r.JSON200.Data.DeletedAt.IsNull() ||
			fixture.Archived != (action == "archive") || !r.JSON200.Data.ArchivedAt.IsNull() != fixture.Archived {
			return nil, fmt.Errorf("collections.info did not confirm archive metadata: HTTP %d: %s", r.StatusCode(), r.Body)
		}
		if err := checkEnvelope("acceptance collection user archive", r.JSON200.Ok, r.JSON200.Status); err != nil {
			return nil, err
		}
	} else {
		if action != "export-reset" && fixture.ViewersCanExport != (action == "export-on") {
			return nil, fmt.Errorf("collection user fixture did not confirm the viewer export preference")
		}
		if action != "export-reset" {
			r, err := a.AuthInfoWithResponse(ctx)
			if err != nil {
				return nil, err
			}
			if r.StatusCode() != http.StatusOK || r.JSON200 == nil || r.JSON200.Data == nil || r.JSON200.Data.Team == nil ||
				r.JSON200.Data.Team.Preferences.GetOrEmpty()["viewersCanExport"] != fixture.ViewersCanExport {
				return nil, fmt.Errorf("auth.info did not confirm the viewer export preference: HTTP %d", r.StatusCode())
			}
		}
	}
	return &fixture, nil
}

func testAccCollectionUserLastManager(t *testing.T, api *acceptanceAPI) {
	collection, user := acceptanceCollectionUserParents(t, api, "last manager")
	actor, err := api.acceptanceUserActor()
	if err != nil {
		t.Fatal(err)
	}
	const providerAddress = "registry.terraform.io/glitchedmob/outline"
	reattach := groupPartialCreateProvider(t, providerAddress)
	config := fmt.Sprintf(`terraform {
  required_providers { outline = { source = %q } }
}
`, providerAddress) + api.acceptanceCollectionUserConfig(collection, user, "admin")
	tf := groupPartialCreateTerraform(t, config)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	if err := tf.Init(ctx, tfexec.Reattach(reattach)); err != nil {
		t.Fatal(err)
	}
	if err := tf.Apply(ctx, tfexec.Reattach(reattach)); err != nil {
		t.Fatal(err)
	}
	restoreManager := func(ctx context.Context) error {
		permission := client.PermissionAdmin
		r, err := api.CollectionsAddUserWithResponse(ctx, client.CollectionsAddUserJSONRequestBody{
			Id: uuid.MustParse(collection), UserId: *actor.Id, Permission: &permission,
		})
		if err != nil {
			return err
		}
		if r.StatusCode() != http.StatusOK || r.JSON200 == nil || r.JSON200.Data == nil ||
			r.JSON200.Data.Memberships == nil || len(*r.JSON200.Data.Memberships) != 1 {
			return fmt.Errorf("restore user manager: HTTP %d: %s", r.StatusCode(), r.Body)
		}
		if err := checkCollectionGrantEnvelope("restore user manager", r.JSON200.Ok, r.JSON200.Status); err != nil {
			return err
		}
		return api.acceptanceCollectionMembership(collection, actor.Id.String(), "admin")
	}
	cleanedUp := false
	t.Cleanup(func() {
		if cleanedUp {
			return
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Minute)
		defer cleanupCancel()
		if err := restoreManager(cleanupCtx); err != nil {
			t.Errorf("restore another manager before cleanup: %v", err)
			return
		}
		if err := os.WriteFile(filepath.Join(tf.WorkingDir(), "main.tf"), []byte(config), 0o600); err != nil {
			t.Error(err)
			return
		}
		if err := tf.Destroy(cleanupCtx, tfexec.Reattach(reattach)); err != nil {
			t.Errorf("destroy last-manager grant after restoring user manager: %v", err)
		}
	})
	// Keep the workspace admin key but remove its explicit collection grant.
	// The managed user is now the only user-or-group manager on the server.
	if err := api.acceptanceRemoveCollectionCaller(collection, actor.Id.String()); err != nil {
		t.Fatal(err)
	}
	baseline, err := api.acceptanceCollectionUserSnapshot(collection, []string{user})
	if err != nil {
		t.Fatal(err)
	}
	grantsBaseline, err := api.acceptanceCollectionUsers(collection)
	if err != nil {
		t.Fatal(err)
	}
	if len(baseline.UserGrants) != 0 || len(grantsBaseline) != 1 || grantsBaseline[user].Permission != "admin" {
		t.Fatalf("last-manager fixture must have exactly one direct user admin: snapshot=%+v grants=%+v", baseline, grantsBaseline)
	}
	for _, grant := range baseline.GroupGrants {
		if grant.Permission == "admin" {
			t.Fatal("last-user-admin fixture must not have a group admin")
		}
	}
	stateBaseline := acceptanceReleaseState(t, ctx, tf)
	if len(stateBaseline) != 1 || stateBaseline[0].Type != "outline_collection_user" || len(stateBaseline[0].Instances) != 1 ||
		stateBaseline[0].Instances[0].Attributes["id"] != collection+"/"+user || stateBaseline[0].Instances[0].Attributes["permission"] != "admin" {
		t.Fatalf("incomplete last-manager CLI baseline: %+v", stateBaseline)
	}
	retained := func() {
		t.Helper()
		if actual := acceptanceReleaseState(t, ctx, tf); !reflect.DeepEqual(actual, stateBaseline) {
			t.Fatalf("last-manager 400 changed persisted state: before=%+v after=%+v", stateBaseline, actual)
		}
		actual, err := api.acceptanceCollectionUsers(collection)
		if err != nil || !reflect.DeepEqual(actual, grantsBaseline) {
			t.Fatalf("last-manager 400 changed the explicit grant: before=%v after=%v err=%v", grantsBaseline, actual, err)
		}
		if err := api.acceptanceCollectionUserCollateral(collection, []string{user}, baseline); err != nil {
			t.Fatal(err)
		}
	}
	const message = "At least one user or group must have manage permissions"
	assertEnvelope := func(status int, body []byte, rejected *client.Validation) {
		t.Helper()
		if status != http.StatusBadRequest || rejected == nil || rejected.Ok == nil || *rejected.Ok ||
			rejected.Status == nil || *rejected.Status != http.StatusBadRequest || rejected.Error == nil || *rejected.Error != "validation_error" ||
			rejected.Message == nil || *rejected.Message != message {
			t.Fatalf("last-manager rejection must be its own server 400, not absent-pair invalid_request: HTTP %d: %s", status, body)
		}
	}
	permission := client.PermissionRead
	updated, err := api.CollectionsAddUserWithResponse(ctx, client.CollectionsAddUserJSONRequestBody{
		Id: uuid.MustParse(collection), UserId: uuid.MustParse(user), Permission: &permission,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertEnvelope(updated.StatusCode(), updated.Body, updated.JSON400)
	retained()
	removed, err := api.CollectionsRemoveUserWithResponse(ctx, client.CollectionsRemoveUserJSONRequestBody{
		Id: uuid.MustParse(collection), UserId: uuid.MustParse(user),
	})
	if err != nil {
		t.Fatal(err)
	}
	assertEnvelope(removed.StatusCode(), removed.Body, removed.JSON400)
	retained()
	for _, operation := range []string{"update", "delete"} {
		nextConfig := config
		if operation == "update" {
			nextConfig = strings.Replace(config, `permission = "admin"`, `permission = "read"`, 1)
		}
		if err := os.WriteFile(filepath.Join(tf.WorkingDir(), "main.tf"), []byte(nextConfig), 0o600); err != nil {
			t.Fatal(err)
		}
		plan := filepath.Join(tf.WorkingDir(), "last-manager-"+operation+".tfplan")
		options := []tfexec.PlanOption{tfexec.Reattach(reattach), tfexec.Out(plan)}
		if operation == "delete" {
			options = append(options, tfexec.Destroy(true))
		}
		if changed, err := tf.Plan(ctx, options...); err != nil || !changed {
			t.Fatalf("plan last-manager %s: changed=%t err=%v", operation, changed, err)
		}
		// The saved plan reaches Update/Delete without a refresh. Both must
		// retain the previously persisted pair and admin permission on HTTP 400.
		err := tf.Apply(ctx, tfexec.Reattach(reattach), tfexec.DirOrPlan(plan))
		if err == nil {
			t.Fatalf("last-manager %s unexpectedly succeeded", operation)
		}
		diagnostic := strings.Join(strings.Fields(err.Error()), " ")
		if !strings.Contains(diagnostic, "HTTP 400") || !strings.Contains(diagnostic, message) {
			t.Fatalf("expected last-manager %s server 400, got %v", operation, err)
		}
		retained()
	}
	if err := restoreManager(ctx); err != nil {
		t.Fatal(err)
	}
	restored, err := api.acceptanceCollectionUserSnapshot(collection, []string{user})
	if err != nil {
		t.Fatal(err)
	}
	if err := tf.Destroy(ctx, tfexec.Reattach(reattach)); err != nil {
		t.Fatal(err)
	}
	cleanedUp = true
	if state := acceptanceReleaseState(t, ctx, tf); len(state) != 0 {
		t.Fatalf("destroy with another manager left collection user state: %+v", state)
	}
	if err := api.acceptanceCollectionUserPermission(collection, user, ""); err != nil {
		t.Fatal(err)
	}
	if err := api.acceptanceCollectionUserCollateral(collection, []string{user}, restored); err != nil {
		t.Fatal(err)
	}
}

func testAccCollectionUserArchived(t *testing.T, api *acceptanceAPI) {
	collection, user := acceptanceCollectionUserParents(t, api, "archived")
	other, err := api.acceptanceCreateGrantUser("Terraform collection user archive create target")
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := api.acceptanceCollectionUserSnapshot(collection, []string{user, other})
	if err != nil {
		t.Fatal(err)
	}
	const providerAddress = "registry.terraform.io/glitchedmob/outline"
	reattach := groupPartialCreateProvider(t, providerAddress)
	config := fmt.Sprintf(`terraform {
  required_providers { outline = { source = %q } }
}
`, providerAddress) + api.acceptanceCollectionUserConfig(collection, user, "read")
	tf := groupPartialCreateTerraform(t, config)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	if err := tf.Init(ctx, tfexec.Reattach(reattach)); err != nil {
		t.Fatal(err)
	}
	if err := tf.Apply(ctx, tfexec.Reattach(reattach)); err != nil {
		t.Fatal(err)
	}
	cleanedUp := false
	archived := false
	writeConfig := func(value string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(tf.WorkingDir(), "main.tf"), []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		if cleanedUp {
			return
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Minute)
		defer cleanupCancel()
		if archived {
			if _, err := api.acceptanceCollectionUserFixtureMetadata(cleanupCtx, "restore", collection); err != nil {
				t.Errorf("restore collection user archive before cleanup: %v", err)
				return
			}
		}
		if err := os.WriteFile(filepath.Join(tf.WorkingDir(), "main.tf"), []byte(config), 0o600); err != nil {
			t.Error(err)
			return
		}
		if err := tf.Destroy(cleanupCtx, tfexec.Reattach(reattach)); err != nil {
			t.Errorf("destroy restored collection user fixture: %v", err)
		}
	})
	stateBaseline := acceptanceReleaseState(t, ctx, tf)
	if len(stateBaseline) != 1 || stateBaseline[0].Type != "outline_collection_user" || len(stateBaseline[0].Instances) != 1 {
		t.Fatalf("expected one collection user resource in CLI state: %+v", stateBaseline)
	}
	attrs := stateBaseline[0].Instances[0].Attributes
	if attrs["id"] != collection+"/"+user || attrs["collection_id"] != collection || attrs["user_id"] != user || attrs["permission"] != "read" {
		t.Fatalf("incomplete collection user CLI baseline: %+v", attrs)
	}
	grantsBaseline, err := api.acceptanceCollectionUsers(collection)
	if err != nil {
		t.Fatal(err)
	}
	setArchive := func(value bool) {
		t.Helper()
		action := "restore"
		if value {
			action = "archive"
		}
		if _, err := api.acceptanceCollectionUserFixtureMetadata(ctx, action, collection); err != nil {
			t.Fatal(err)
		}
		archived = value
	}
	retained := func() {
		t.Helper()
		if actual := acceptanceReleaseState(t, ctx, tf); !reflect.DeepEqual(actual, stateBaseline) {
			t.Fatalf("archive rejection changed persisted grant state: before=%+v after=%+v", stateBaseline, actual)
		}
		if c := acceptanceReleaseCollection(t, api, collection); !c.ArchivedAt.IsNull() != archived {
			t.Fatal("collection user operation changed the parent's archive metadata")
		}
		actual, err := api.acceptanceCollectionUsers(collection)
		if err != nil || !reflect.DeepEqual(actual, grantsBaseline) {
			t.Fatalf("archive refusal changed direct grant identities or permissions: before=%v after=%v err=%v", grantsBaseline, actual, err)
		}
		if err := api.acceptanceCollectionUserCollateral(collection, []string{user, other}, baseline); err != nil {
			t.Fatal(err)
		}
	}
	setArchive(true)
	// Read and import must reject the unsupported parent without erasing the
	// existing grant or saving a guessed state for the attempted import.
	if err := tf.Refresh(ctx, tfexec.Reattach(reattach)); err == nil ||
		!strings.Contains(strings.ToLower(err.Error()), "archived") {
		t.Fatalf("expected archived read refusal, got %v", err)
	}
	retained()
	writeConfig(config + fmt.Sprintf(`
resource "outline_collection_user" "rejected" {
  collection_id = %q
  user_id = %q
  permission = "read"
}
`, collection, user))
	if err := tf.Import(ctx, "outline_collection_user.rejected", collection+"/"+user, tfexec.Reattach(reattach)); err == nil ||
		!strings.Contains(strings.ToLower(err.Error()), "archived") {
		t.Fatalf("expected archived import refusal, got %v", err)
	}
	retained()
	writeConfig(config)
	// This case has no owned state, so it reaches Create on the archived parent
	// instead of failing while refreshing the CLI's existing grant.
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acceptanceProviderFactories(),
		Steps: []resource.TestStep{{Config: api.acceptanceCollectionUserConfig(collection, other, "read_write"),
			ExpectError: regexp.MustCompile(`(?is)archived`)}},
	})
	if err := api.acceptanceCollectionUserPermission(collection, other, ""); err != nil {
		t.Fatal(err)
	}
	retained()
	setArchive(false)

	for _, operation := range []string{"update", "delete"} {
		writeConfig(config)
		if operation == "update" {
			writeConfig(strings.Replace(config, `permission = "read"`, `permission = "admin"`, 1))
		}
		plan := filepath.Join(tf.WorkingDir(), "archive-"+operation+".tfplan")
		options := []tfexec.PlanOption{tfexec.Reattach(reattach), tfexec.Out(plan)}
		if operation == "delete" {
			options = append(options, tfexec.Destroy(true))
		}
		if changed, err := tf.Plan(ctx, options...); err != nil || !changed {
			t.Fatalf("plan %s before archive: changed=%t err=%v", operation, changed, err)
		}
		setArchive(true)
		// Applying the saved plan bypasses refresh, so the resource's mutation
		// preflight must reject the archive and keep its existing state intact.
		if err := tf.Apply(ctx, tfexec.Reattach(reattach), tfexec.DirOrPlan(plan)); err == nil ||
			!strings.Contains(strings.ToLower(err.Error()), "archived") {
			t.Fatalf("expected archived %s refusal, got %v", operation, err)
		}
		retained()
		setArchive(false)
		writeConfig(config)
		if err := tf.Refresh(ctx, tfexec.Reattach(reattach)); err != nil {
			t.Fatal(err)
		}
		retained()
		if changed, err := tf.Plan(ctx, tfexec.Reattach(reattach)); err != nil || changed {
			t.Fatalf("restored archive must have an empty plan: changed=%t err=%v", changed, err)
		}
	}
	if err := tf.Destroy(ctx, tfexec.Reattach(reattach)); err != nil {
		t.Fatal(err)
	}
	cleanedUp = true
	if state := acceptanceReleaseState(t, ctx, tf); len(state) != 0 {
		t.Fatalf("destroy left collection user state: %+v", state)
	}
	if err := api.acceptanceCollectionUserPermission(collection, user, ""); err != nil {
		t.Fatal(err)
	}
	if err := api.acceptanceCollectionUserCollateral(collection, []string{user, other}, baseline); err != nil {
		t.Fatal(err)
	}
}
