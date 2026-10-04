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
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

func (a *acceptanceAPI) acceptanceCollectionGroupFixture(ctx context.Context, action, id string) error {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	const path = "/opt/outline/acceptance-collection-group-fixture.cjs"
	code, data, failure := runAcceptanceFixture(ctx, a.container, "../../integration/collection-group-fixture.cjs", path, action, id)
	if failure != nil {
		return failure
	}
	if code != 0 {
		return fmt.Errorf("Outline collection group fixture exited %d: %s", code, data)
	}
	var fixture struct {
		Version, Action  string
		CollectionID     string `json:"collection_id"`
		GroupID          string `json:"group_id"`
		Archived, Synced bool
	}
	const marker = "OUTLINE_ACCEPTANCE_COLLECTION_GROUP="
	found := false
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, marker) {
			if found {
				return fmt.Errorf("collection group fixture repeated its result marker")
			}
			found = true
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, marker)), &fixture); err != nil {
				return err
			}
		}
	}
	if !found || fixture.Version != "1.10.1" || fixture.Action != action {
		return fmt.Errorf("collection group fixture did not confirm the release and action: %s", data)
	}
	switch action {
	case "sync", "unsync":
		if fixture.GroupID != id || fixture.Synced != (action == "sync") {
			return fmt.Errorf("collection group fixture did not confirm synchronization")
		}
		group, err := a.acceptanceGroup(id)
		if err != nil {
			return err
		}
		if group.ExternalId.GetOrEmpty() != "" || (group.ExternalGroup.IsSpecified() && !group.ExternalGroup.IsNull()) != fixture.Synced {
			return fmt.Errorf("groups.info did not confirm the externalGroup-only fixture")
		}
	case "archive", "restore":
		if fixture.CollectionID != id || fixture.Archived != (action == "archive") {
			return fmt.Errorf("collection group fixture did not confirm archive metadata")
		}
		r, err := a.CollectionsInfoWithResponse(ctx, client.CollectionsInfoJSONRequestBody{Id: uuid.MustParse(id)})
		if err != nil {
			return err
		}
		if r.StatusCode() != http.StatusOK || r.JSON200 == nil || r.JSON200.Data == nil ||
			!r.JSON200.Data.ArchivedAt.IsSpecified() || !r.JSON200.Data.DeletedAt.IsNull() ||
			!r.JSON200.Data.ArchivedAt.IsNull() != fixture.Archived {
			return fmt.Errorf("collections.info did not confirm archive metadata: HTTP %d: %s", r.StatusCode(), r.Body)
		}
		if err := checkEnvelope("acceptance collection group archive", r.JSON200.Ok, r.JSON200.Status); err != nil {
			return err
		}
	}
	return nil
}

// collections.add_group/remove_group authorize collection update and group
// read, not group update or membership administration. A synchronized group
// can receive a manual collection grant without renaming it or adding members.
func testAccCollectionGroupExternal(t *testing.T, api *acceptanceAPI) {
	collection, external := acceptanceCollectionGroupParents(t, api, "externally linked")
	synchronized, err := api.acceptanceCreateGroup("Terraform collection group synchronized")
	if err != nil {
		t.Fatal(err)
	}
	actor, err := api.acceptanceUserActor()
	if err != nil {
		t.Fatal(err)
	}
	for _, group := range []string{external, synchronized} {
		if err := api.acceptanceWriteGroupMember(group, actor.Id.String(), client.GroupPermissionAdmin, true); err != nil {
			t.Fatal(err)
		}
	}
	externalID := "terraform-collection-group-external"
	r, err := api.GroupsUpdateWithResponse(t.Context(), client.GroupsUpdateJSONRequestBody{
		Id: uuid.MustParse(external), ExternalId: &externalID,
	})
	if err != nil || r == nil || r.StatusCode() != http.StatusOK || r.JSON200 == nil || r.JSON200.Data == nil ||
		r.JSON200.Data.ExternalId.GetOrEmpty() != externalID {
		t.Fatalf("externalId fixture update failed: %v, response %v", err, r)
	}
	if err := api.acceptanceCollectionGroupFixture(t.Context(), "sync", synchronized); err != nil {
		t.Fatal(err)
	}
	groups := []string{external, synchronized}
	baseline, err := api.acceptanceCollectionGroupSnapshot(collection, groups)
	if err != nil {
		t.Fatal(err)
	}
	config := func(permission string) string {
		return api.providerConfig + fmt.Sprintf(`
resource "outline_collection_group" "external" {
  collection_id = %q
  group_id = %q
  permission = %q
}
resource "outline_collection_group" "synchronized" {
  collection_id = %q
  group_id = %q
  permission = %q
}
`, collection, external, permission, collection, synchronized, permission)
	}
	address := func(name string) string { return "outline_collection_group." + name }
	check := func(permission string) resource.TestCheckFunc {
		return resource.ComposeAggregateTestCheckFunc(
			api.checkAcceptanceCollectionGroup(address("external")), api.checkAcceptanceCollectionGroup(address("synchronized")),
			resource.TestCheckResourceAttr(address("external"), "permission", permission),
			resource.TestCheckResourceAttr(address("synchronized"), "permission", permission),
			func(_ *terraform.State) error {
				return api.acceptanceCollectionGroupCollateral(collection, groups, baseline)
			},
		)
	}
	deleted := func(_ *terraform.State) error {
		for _, group := range groups {
			if err := api.acceptanceCollectionGroupPermission(collection, group, ""); err != nil {
				return err
			}
		}
		return api.acceptanceCollectionGroupCollateral(collection, groups, baseline)
	}
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acceptanceProviderFactories(), CheckDestroy: deleted,
		Steps: []resource.TestStep{
			{Config: config("read"), Check: check("read")},
			{ResourceName: address("external"), ImportState: true, ImportStateVerify: true},
			{ResourceName: address("synchronized"), ImportState: true, ImportStateVerify: true},
			{Config: config("admin"), ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
				plancheck.ExpectResourceAction(address("external"), plancheck.ResourceActionUpdate),
				plancheck.ExpectResourceAction(address("synchronized"), plancheck.ResourceActionUpdate),
			}}, Check: check("admin")},
			{Config: config("admin"), ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}}, Check: check("admin")},
			{Config: api.providerConfig, Check: deleted},
		},
	})
}

type acceptanceCollectionGroupRoundTripper func(*http.Request) (*http.Response, error)

func (f acceptanceCollectionGroupRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func acceptanceCollectionGroupAbsentRemoval(t *testing.T, api *acceptanceAPI, collection, group string) {
	t.Helper()
	r, err := api.CollectionsRemoveGroupWithResponse(t.Context(), client.CollectionsRemoveGroupJSONRequestBody{
		Id: uuid.MustParse(collection), GroupId: uuid.MustParse(group),
	})
	if err != nil {
		t.Fatal(err)
	}
	absent := r.JSON400
	if r.StatusCode() != http.StatusBadRequest || absent == nil || absent.Ok == nil || *absent.Ok ||
		absent.Status == nil || *absent.Status != http.StatusBadRequest || absent.Error == nil || *absent.Error != "invalid_request" ||
		absent.Message == nil || *absent.Message != "This Group is not a part of the collection" {
		t.Fatalf("release absent-pair remove did not return the exact 400 envelope: HTTP %d: %s", r.StatusCode(), r.Body)
	}
	for _, malformed := range []bool{false, true} {
		name := "complete live confirmation"
		if malformed {
			name = "malformed final live page is not absence"
		}
		t.Run(name, func(t *testing.T) {
			candidate, err := newAPIClient(api.baseURL, api.apiKey, 30, "acceptance")
			if err != nil {
				t.Fatal(err)
			}
			upstream := candidate.httpClient.Transport
			var offsets []int
			removes := 0
			candidate.httpClient.Transport = acceptanceCollectionGroupRoundTripper(func(req *http.Request) (*http.Response, error) {
				offset := -1
				switch req.URL.Path {
				case "/api/collections.remove_group":
					removes++
				case "/api/collections.group_memberships":
					if req.GetBody == nil {
						return nil, fmt.Errorf("remove confirmation request has no replayable JSON body")
					}
					body, err := req.GetBody()
					if err != nil {
						return nil, err
					}
					defer func() { _ = body.Close() }()
					var input client.CollectionsGroupMembershipsJSONRequestBody
					if err := json.NewDecoder(body).Decode(&input); err != nil {
						return nil, err
					}
					if input.Id.String() != collection || input.Limit == nil || *input.Limit != 100 || input.Offset == nil ||
						input.Query != nil || input.Permission != nil {
						return nil, fmt.Errorf("remove confirmation must list the unfiltered collection grants with limit 100 and offset")
					}
					offset = *input.Offset
					offsets = append(offsets, offset)
				default:
					return nil, fmt.Errorf("unexpected remove confirmation endpoint %s", req.URL.Path)
				}
				response, err := upstream.RoundTrip(req)
				if err != nil || !malformed || offset != 100 {
					return response, err
				}
				// Keep the real server's final page but remove its completion
				// metadata. The exact remove 400 cannot justify accepting it.
				data, err := io.ReadAll(response.Body)
				_ = response.Body.Close()
				if err != nil {
					return nil, err
				}
				if response.StatusCode != http.StatusOK {
					return nil, fmt.Errorf("expected live final page HTTP 200, got %d", response.StatusCode)
				}
				var page map[string]any
				if err := json.Unmarshal(data, &page); err != nil {
					return nil, err
				}
				delete(page, "pagination")
				data, err = json.Marshal(page)
				if err != nil {
					return nil, err
				}
				response.Body = io.NopCloser(strings.NewReader(string(data)))
				response.ContentLength = int64(len(data))
				response.Header.Set("Content-Length", fmt.Sprint(len(data)))
				return response, nil
			})
			err = candidate.removeCollectionGroup(t.Context(), uuid.MustParse(collection), uuid.MustParse(group))
			if malformed {
				if err == nil || !strings.Contains(err.Error(), "pagination") {
					t.Fatalf("incomplete final page must fail absent-pair confirmation, got %v", err)
				}
			} else if err != nil {
				t.Fatalf("exact live absent-pair 400 plus a complete validated list must succeed: %v", err)
			}
			if removes != 1 || !reflect.DeepEqual(offsets, []int{0, 100}) {
				t.Fatalf("absent remove must not retry and must validate both pages: removes=%d offsets=%v", removes, offsets)
			}
		})
	}
}

func testAccCollectionGroupLastManager(t *testing.T, api *acceptanceAPI) {
	collection, group := acceptanceCollectionGroupParents(t, api, "last manager")
	actor, err := api.acceptanceUserActor()
	if err != nil {
		t.Fatal(err)
	}
	const providerAddress = "registry.terraform.io/glitchedmob/outline"
	reattach := groupPartialCreateProvider(t, providerAddress)
	config := fmt.Sprintf(`terraform {
  required_providers { outline = { source = %q } }
}
`, providerAddress) + api.acceptanceCollectionGroupConfig(collection, group, "admin")
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
	// The managed group is now the only user-or-group manager on the server.
	if err := api.acceptanceRemoveCollectionCaller(collection, actor.Id.String()); err != nil {
		t.Fatal(err)
	}
	baseline, err := api.acceptanceCollectionGroupSnapshot(collection, []string{group})
	if err != nil {
		t.Fatal(err)
	}
	grantsBaseline, err := api.acceptanceCollectionGroups(collection)
	if err != nil {
		t.Fatal(err)
	}
	if len(baseline.UserGrants) != 0 || len(grantsBaseline) != 1 || grantsBaseline[group].Permission != "admin" {
		t.Fatalf("last-manager fixture must have one group manager and no user grants: snapshot=%+v grants=%+v", baseline, grantsBaseline)
	}
	stateBaseline := acceptanceReleaseState(t, ctx, tf)
	if len(stateBaseline) != 1 || stateBaseline[0].Type != "outline_collection_group" || len(stateBaseline[0].Instances) != 1 ||
		stateBaseline[0].Instances[0].Attributes["id"] != collection+"/"+group || stateBaseline[0].Instances[0].Attributes["permission"] != "admin" {
		t.Fatalf("incomplete last-manager CLI baseline: %+v", stateBaseline)
	}
	retained := func() {
		t.Helper()
		if actual := acceptanceReleaseState(t, ctx, tf); !reflect.DeepEqual(actual, stateBaseline) {
			t.Fatalf("last-manager 400 changed persisted state: before=%+v after=%+v", stateBaseline, actual)
		}
		actual, err := api.acceptanceCollectionGroups(collection)
		if err != nil || !reflect.DeepEqual(actual, grantsBaseline) {
			t.Fatalf("last-manager 400 changed the explicit grant: before=%v after=%v err=%v", grantsBaseline, actual, err)
		}
		if err := api.acceptanceCollectionGroupCollateral(collection, []string{group}, baseline); err != nil {
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
	updated, err := api.CollectionsAddGroupWithResponse(ctx, client.CollectionsAddGroupJSONRequestBody{
		Id: uuid.MustParse(collection), GroupId: uuid.MustParse(group), Permission: &permission,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertEnvelope(updated.StatusCode(), updated.Body, updated.JSON400)
	retained()
	removed, err := api.CollectionsRemoveGroupWithResponse(ctx, client.CollectionsRemoveGroupJSONRequestBody{
		Id: uuid.MustParse(collection), GroupId: uuid.MustParse(group),
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
	restored, err := api.acceptanceCollectionGroupSnapshot(collection, []string{group})
	if err != nil {
		t.Fatal(err)
	}
	if err := tf.Destroy(ctx, tfexec.Reattach(reattach)); err != nil {
		t.Fatal(err)
	}
	cleanedUp = true
	if state := acceptanceReleaseState(t, ctx, tf); len(state) != 0 {
		t.Fatalf("destroy with another manager left collection group state: %+v", state)
	}
	if err := api.acceptanceCollectionGroupPermission(collection, group, ""); err != nil {
		t.Fatal(err)
	}
	if err := api.acceptanceCollectionGroupCollateral(collection, []string{group}, restored); err != nil {
		t.Fatal(err)
	}
}

func testAccCollectionGroupArchived(t *testing.T, api *acceptanceAPI) {
	collection, group := acceptanceCollectionGroupParents(t, api, "archived")
	other, err := api.acceptanceCreateGroup("Terraform collection group archive create target")
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := api.acceptanceCollectionGroupSnapshot(collection, []string{group, other})
	if err != nil {
		t.Fatal(err)
	}
	const providerAddress = "registry.terraform.io/glitchedmob/outline"
	reattach := groupPartialCreateProvider(t, providerAddress)
	config := fmt.Sprintf(`terraform {
  required_providers { outline = { source = %q } }
}
`, providerAddress) + api.acceptanceCollectionGroupConfig(collection, group, "read")
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
			if err := api.acceptanceCollectionGroupFixture(cleanupCtx, "restore", collection); err != nil {
				t.Errorf("restore collection group archive before cleanup: %v", err)
				return
			}
		}
		if err := os.WriteFile(filepath.Join(tf.WorkingDir(), "main.tf"), []byte(config), 0o600); err != nil {
			t.Error(err)
			return
		}
		if err := tf.Destroy(cleanupCtx, tfexec.Reattach(reattach)); err != nil {
			t.Errorf("destroy restored collection group fixture: %v", err)
		}
	})
	stateBaseline := acceptanceReleaseState(t, ctx, tf)
	if len(stateBaseline) != 1 || stateBaseline[0].Type != "outline_collection_group" || len(stateBaseline[0].Instances) != 1 {
		t.Fatalf("expected one collection group resource in CLI state: %+v", stateBaseline)
	}
	attrs := stateBaseline[0].Instances[0].Attributes
	if attrs["id"] != collection+"/"+group || attrs["collection_id"] != collection || attrs["group_id"] != group || attrs["permission"] != "read" {
		t.Fatalf("incomplete collection group CLI baseline: %+v", attrs)
	}
	setArchive := func(value bool) {
		t.Helper()
		action := "restore"
		if value {
			action = "archive"
		}
		if err := api.acceptanceCollectionGroupFixture(ctx, action, collection); err != nil {
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
			t.Fatal("collection group operation changed the parent's archive metadata")
		}
		if err := api.acceptanceCollectionGroupPermission(collection, group, "read"); err != nil {
			t.Fatal(err)
		}
		if err := api.acceptanceCollectionGroupCollateral(collection, []string{group, other}, baseline); err != nil {
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
resource "outline_collection_group" "rejected" {
  collection_id = %q
  group_id = %q
  permission = "read"
}
`, collection, group))
	if err := tf.Import(ctx, "outline_collection_group.rejected", collection+"/"+group, tfexec.Reattach(reattach)); err == nil ||
		!strings.Contains(strings.ToLower(err.Error()), "archived") {
		t.Fatalf("expected archived import refusal, got %v", err)
	}
	retained()
	writeConfig(config)
	// This case has no owned state, so it reaches Create on the archived parent
	// instead of failing while refreshing the CLI's existing grant.
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acceptanceProviderFactories(),
		Steps: []resource.TestStep{{Config: api.acceptanceCollectionGroupConfig(collection, other, "read_write"),
			ExpectError: regexp.MustCompile(`(?is)archived`)}},
	})
	if err := api.acceptanceCollectionGroupPermission(collection, other, ""); err != nil {
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
		t.Fatalf("destroy left collection group state: %+v", state)
	}
	if err := api.acceptanceCollectionGroupPermission(collection, group, ""); err != nil {
		t.Fatal(err)
	}
	if err := api.acceptanceCollectionGroupCollateral(collection, []string{group, other}, baseline); err != nil {
		t.Fatal(err)
	}
}
