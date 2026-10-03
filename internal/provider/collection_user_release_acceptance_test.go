// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"encoding/json"
	"errors"
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
	"github.com/oapi-codegen/nullable"
)

// Policy abilities are either booleans or arrays of the grants that authorize
// the action. An explicit viewer/guest write grant is not a read-only policy.
func acceptanceIAMAbility(policies *[]client.Policy, id, ability string, want bool) error {
	if policies != nil {
		for _, p := range *policies {
			if p.Id == nil || p.Id.String() != id || p.Abilities == nil {
				continue
			}
			value, ok := (*p.Abilities)[ability]
			if !ok {
				return fmt.Errorf("collection policy has no %s ability", ability)
			}
			allowed, err := value.AsAbility1()
			if err != nil {
				ids, decodeErr := value.AsAbility0()
				if decodeErr != nil {
					return fmt.Errorf("invalid %s policy: %v", ability, decodeErr)
				}
				allowed = len(ids) > 0
			}
			if bool(allowed) != want {
				return fmt.Errorf("%s ability: got %t, want %t", ability, allowed, want)
			}
			return nil
		}
	}
	return fmt.Errorf("collection %s has no policy", id)
}

func acceptanceIAMEffective(api *apiClient, collection string, read, write, manage bool) error {
	r, err := api.CollectionsInfoWithResponse(context.Background(), client.CollectionsInfoJSONRequestBody{Id: uuid.MustParse(collection)})
	if err != nil {
		return err
	}
	if !read {
		if r.StatusCode() != http.StatusForbidden || r.JSON403 == nil || r.JSON403.Error == nil || *r.JSON403.Error != "authorization_error" {
			return fmt.Errorf("expected private collection access denial, HTTP %d: %s", r.StatusCode(), r.Body)
		}
		return nil
	}
	if r.StatusCode() != http.StatusOK || r.JSON200 == nil || r.JSON200.Data == nil {
		return fmt.Errorf("effective collection access: HTTP %d: %s", r.StatusCode(), r.Body)
	}
	for ability, want := range map[string]bool{"read": true, "updateDocument": write, "createDocument": write, "update": manage} {
		if err := acceptanceIAMAbility(r.JSON200.Policies, collection, ability, want); err != nil {
			return err
		}
	}
	return nil
}

func testAccCollectionUserGroupAccess(t *testing.T, api *acceptanceAPI) {
	collection, user := acceptanceCollectionUserParents(t, api, "effective access")
	key := api.acceptanceInspectUser(t, user, "key").APIKey
	target, err := newAPIClient(api.baseURL, key, 30, "acceptance")
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := api.acceptanceCollectionUserSnapshot(collection, []string{user})
	if err != nil {
		t.Fatal(err)
	}
	if baseline.Permission != "null" || len(baseline.GroupGrants) != 1 {
		t.Fatal("effective-access fixture must have a private default and one group read_write grant")
	}
	for _, grant := range baseline.GroupGrants {
		if grant.Permission != "read_write" {
			t.Fatal("effective-access fixture must have a read_write group grant")
		}
	}
	check := func(direct bool) resource.TestCheckFunc {
		return func(state *terraform.State) error {
			permission := ""
			if direct {
				permission = "read"
				if err := api.checkAcceptanceCollectionUser(acceptanceCollectionUserAddress)(state); err != nil {
					return err
				}
				if err := resource.TestCheckResourceAttr(acceptanceCollectionUserAddress, "permission", "read")(state); err != nil {
					return err
				}
			}
			if err := api.acceptanceCollectionUserPermission(collection, user, permission); err != nil {
				return err
			}
			if err := acceptanceIAMEffective(target, collection, true, true, false); err != nil {
				return err
			}
			return api.acceptanceCollectionUserCollateral(collection, []string{user}, baseline)
		}
	}
	config := api.acceptanceCollectionUserConfig(collection, user, "read")
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acceptanceProviderFactories(), CheckDestroy: check(false),
		Steps: []resource.TestStep{
			{Config: config, Check: check(true)},
			{ResourceName: acceptanceCollectionUserAddress, ImportState: true, ImportStateVerify: true},
			{Config: config, ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}}, Check: check(true)},
			{Config: api.providerConfig, Check: check(false)},
		},
	})
}

// The download/export policies honor ViewersCanExport for both viewers and
// guests. The export route separately requires at least the workspace member
// role, even when a viewer or guest has an explicit collection-admin grant.
func acceptanceIAMRoleLimits(api *apiClient, collection, role, permission string, public, viewersCanExport bool) error {
	read := permission != "" || public && role != "guest"
	if read {
		r, err := api.CollectionsInfoWithResponse(context.Background(), client.CollectionsInfoJSONRequestBody{Id: uuid.MustParse(collection)})
		if err != nil {
			return err
		}
		if r.StatusCode() != http.StatusOK || r.JSON200 == nil {
			return fmt.Errorf("role-limit collection policies: HTTP %d: %s", r.StatusCode(), r.Body)
		}
		download := role == "member" || viewersCanExport
		share := role != "guest" && (permission == "read_write" || permission == "admin" || public && role == "member")
		for ability, want := range map[string]bool{"share": share, "download": download, "export": permission == "admin" && download} {
			if err := acceptanceIAMAbility(r.JSON200.Policies, collection, ability, want); err != nil {
				return err
			}
		}
	}
	if role == "viewer" || role == "guest" {
		// Do not call export as a member. A successful request would enqueue
		// work and create files outside the grant-management test's scope.
		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, api.baseURL+"/collections.export",
			strings.NewReader(fmt.Sprintf(`{"id":%q,"format":"markdown"}`, collection)))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		r, err := api.httpClient.Do(req)
		if err != nil {
			return err
		}
		defer func() { _ = r.Body.Close() }()
		var denied client.Unauthorized
		if err := json.NewDecoder(r.Body).Decode(&denied); err != nil {
			return err
		}
		if r.StatusCode != http.StatusForbidden || denied.Error == nil || *denied.Error != "authorization_error" ||
			denied.Ok == nil || *denied.Ok || denied.Status == nil || *denied.Status != http.StatusForbidden {
			return fmt.Errorf("%s collection export must be rejected by the workspace role gate: HTTP %d", role, r.StatusCode)
		}
	}
	return nil
}

func testAccCollectionUserRoles(t *testing.T, api *acceptanceAPI) {
	collection, err := api.acceptanceCreateCollection("Terraform collection user target roles")
	if err != nil {
		t.Fatal(err)
	}
	if err := api.acceptanceUpdateCollection(client.CollectionsUpdateJSONRequestBody{Id: uuid.MustParse(collection), Sharing: acceptanceIAMBoolPointer(true)}); err != nil {
		t.Fatal(err)
	}
	fixture, err := api.acceptanceCollectionUserFixtureMetadata(t.Context(), "export-off", collection)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		action := "export-reset"
		if fixture.PreviousExport != nil {
			action = "export-off"
			if *fixture.PreviousExport {
				action = "export-on"
			}
		}
		if _, err := api.acceptanceCollectionUserFixtureMetadata(context.Background(), action, collection); err != nil {
			t.Errorf("restore viewer export preference: %v", err)
		}
	})
	viewersCanExport := false
	roles := []string{"member", "viewer", "guest", "suspended"}
	targets := make(map[string]string)
	clients := make(map[string]*apiClient)
	for _, role := range roles {
		id, err := api.acceptanceCreateGrantUser("Terraform collection user target " + role)
		if err != nil {
			t.Fatal(err)
		}
		targets[role] = id
		if role == "viewer" || role == "guest" {
			// Guest demotion revokes old keys asynchronously. Wait for that event
			// before creating the key used to inspect effective access.
			oldKey := api.acceptanceInspectUser(t, id, "key").APIKey
			if err := api.acceptanceUserRole(id, client.UserRole(role)); err != nil {
				t.Fatal(err)
			}
			if role == "guest" {
				old, err := newAPIClient(api.baseURL, oldKey, 30, "acceptance")
				if err != nil {
					t.Fatal(err)
				}
				deadline := time.Now().Add(30 * time.Second)
				for {
					r, err := old.AuthInfoWithResponse(t.Context())
					if err != nil {
						t.Fatal(err)
					}
					if r.StatusCode() == http.StatusUnauthorized {
						break
					}
					if time.Now().After(deadline) {
						t.Fatalf("guest demotion did not revoke old API key: HTTP %d", r.StatusCode())
					}
					time.Sleep(100 * time.Millisecond)
				}
			}
		}
		key := api.acceptanceInspectUser(t, id, "key").APIKey
		clients[role], err = newAPIClient(api.baseURL, key, 30, "acceptance")
		if err != nil {
			t.Fatal(err)
		}
		if role == "suspended" {
			if err := api.acceptanceUserSuspended(id, true); err != nil {
				t.Fatal(err)
			}
		}
	}
	baseline, err := api.acceptanceCollectionUserSnapshot(collection, []string{targets["member"], targets["viewer"], targets["guest"], targets["suspended"]})
	if err != nil {
		t.Fatal(err)
	}
	config := func(permission string) string {
		var b strings.Builder
		b.WriteString(api.providerConfig)
		for _, role := range roles {
			fmt.Fprintf(&b, `resource "outline_collection_user" %q {
 collection_id = %q
 user_id = %q
 permission = %q
}
`, role, collection, targets[role], permission)
		}
		return b.String()
	}
	check := func(permission string, public bool) resource.TestCheckFunc {
		return func(state *terraform.State) error {
			for _, role := range roles {
				if permission != "" {
					if err := api.checkAcceptanceCollectionUser("outline_collection_user." + role)(state); err != nil {
						return err
					}
				}
				if err := api.acceptanceCollectionUserPermission(collection, targets[role], permission); err != nil {
					return err
				}
				if role == "suspended" {
					r, err := clients[role].AuthInfoWithResponse(context.Background())
					if err != nil {
						return err
					}
					if r.StatusCode() != http.StatusForbidden && r.StatusCode() != http.StatusUnauthorized {
						return fmt.Errorf("suspended target authenticated: HTTP %d", r.StatusCode())
					}
					continue
				}
				read := permission != "" || public && role != "guest"
				write := permission == "read_write" || permission == "admin" || public && role == "member"
				if err := acceptanceIAMEffective(clients[role], collection, read, write, permission == "admin"); err != nil {
					return fmt.Errorf("%s target with %s direct grant: %w", role, permission, err)
				}
				if err := acceptanceIAMRoleLimits(clients[role], collection, role, permission, public, viewersCanExport); err != nil {
					return fmt.Errorf("%s role limits with %s direct grant: %w", role, permission, err)
				}
			}
			return api.acceptanceCollectionUserCollateral(collection, []string{targets["member"], targets["viewer"], targets["guest"], targets["suspended"]}, baseline)
		}
	}
	steps := []resource.TestStep{{Config: config("read"), Check: check("read", false)}}
	for _, role := range roles {
		steps = append(steps, resource.TestStep{ResourceName: "outline_collection_user." + role, ImportState: true, ImportStateVerify: true})
	}
	steps = append(steps,
		resource.TestStep{Config: config("read_write"), Check: check("read_write", false)},
		resource.TestStep{Config: config("admin"), Check: check("admin", false)},
		resource.TestStep{PreConfig: func() {
			if _, err := api.acceptanceCollectionUserFixtureMetadata(t.Context(), "export-on", collection); err != nil {
				t.Fatal(err)
			}
			viewersCanExport = true
		}, Config: config("admin"), ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}}, Check: check("admin", false)},
		resource.TestStep{Config: config("read"), Check: check("read", false)},
		resource.TestStep{PreConfig: func() {
			if err := api.acceptanceUpdateCollection(client.CollectionsUpdateJSONRequestBody{Id: uuid.MustParse(collection), Permission: nullable.NewNullableWithValue(client.PermissionReadWrite)}); err != nil {
				t.Fatal(err)
			}
			baseline.Permission = "read_write"
		}, Config: config("read"), ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}}, Check: check("read", true)},
		resource.TestStep{Config: api.providerConfig, Check: check("", true)},
	)
	resource.Test(t, resource.TestCase{ProtoV6ProviderFactories: acceptanceProviderFactories(), CheckDestroy: check("", true), Steps: steps})
}

func testAccCollectionUserOwner(t *testing.T, api *acceptanceAPI) {
	actor, err := api.acceptanceUserActor()
	if err != nil {
		t.Fatal(err)
	}
	collection, err := api.acceptanceCreateCollection("Terraform collection user creator grant")
	if err != nil {
		t.Fatal(err)
	}
	owner := actor.Id.String()
	if err := api.acceptanceCollectionUserPermission(collection, owner, "admin"); err != nil {
		t.Fatal(err)
	}
	admin, err := api.acceptanceCreateGrantUser("Terraform collection user alternate admin")
	if err != nil {
		t.Fatal(err)
	}
	if err := api.acceptanceUserRole(admin, client.UserRoleAdmin); err != nil {
		t.Fatal(err)
	}
	key := api.acceptanceInspectUser(t, admin, "key").APIKey
	if err := api.acceptanceWriteCollectionUser(collection, admin, "admin"); err != nil {
		t.Fatal(err)
	}
	// Creator grants are pre-existing, even when the key belongs to another
	// administrator. Never let Create turn an automatic admin grant into read.
	otherConfig := strings.Replace(api.acceptanceCollectionUserConfig(collection, owner, "admin"), api.apiKey, key, 1)
	resource.Test(t, resource.TestCase{ProtoV6ProviderFactories: acceptanceProviderFactories(), Steps: []resource.TestStep{
		{Config: strings.Replace(otherConfig, `permission = "admin"`, `permission = "read"`, 1), ExpectError: regexp.MustCompile(`(?is)already exists.*import.*` + regexp.QuoteMeta(collection+"/"+owner))},
	}})
	if err := api.acceptanceCollectionUserPermission(collection, owner, "admin"); err != nil {
		t.Fatal(err)
	}
	testAccCollectionUserRejectedCLI(t, api, collection, owner, key, api.apiKey, "owner")
}

func testAccCollectionUserForbidden(t *testing.T, api *acceptanceAPI) {
	collection, user := acceptanceCollectionUserParents(t, api, "forbidden")
	if err := api.acceptanceWriteCollectionUser(collection, user, "read"); err != nil {
		t.Fatal(err)
	}
	fixture := api.acceptanceCollectionFixture(t, "workspace", "")
	foreign, err := newAPIClient(api.baseURL, fixture.APIKey, 30, "acceptance")
	if err != nil {
		t.Fatal(err)
	}
	_, err = foreign.observeCollectionUser(t.Context(), uuid.MustParse(collection), uuid.MustParse(user))
	if err == nil || errors.Is(err, errNotFound) || !strings.Contains(err.Error(), "HTTP 403") {
		t.Fatalf("foreign observation became absence: %v", err)
	}
	_, err = foreign.readCollectionUserPages(t.Context(), uuid.MustParse(collection), uuid.MustParse(user))
	if err == nil || errors.Is(err, errNotFound) || !strings.Contains(err.Error(), "collections.memberships: HTTP 403") {
		t.Fatalf("foreign membership list became absence: %v", err)
	}
	testAccCollectionUserRejectedCLI(t, api, collection, user, api.apiKey, fixture.APIKey, "forbidden")
}

// Import with a safe key, then exercise denied Read/import/Create and saved-plan
// Update/Delete with refresh disabled. Compare persisted state and remote grants
// after every refusal; failed Read must not drop a key owner's grant from state.
func testAccCollectionUserRejectedCLI(t *testing.T, api *acceptanceAPI, collection, user, safeKey, deniedKey, reason string) {
	t.Helper()
	permission := "read"
	if reason == "owner" {
		permission = "admin"
	}
	baseline, err := api.acceptanceCollectionUserSnapshot(collection, nil)
	if err != nil {
		t.Fatal(err)
	}
	const providerAddress = "registry.terraform.io/glitchedmob/outline"
	reattach := groupPartialCreateProvider(t, providerAddress)
	config := fmt.Sprintf(`terraform {
  required_providers { outline = { source = %q } }
}
`, providerAddress) + strings.Replace(api.acceptanceCollectionUserConfig(collection, user, permission), api.apiKey, safeKey, 1)
	denied := strings.Replace(config, safeKey, deniedKey, 1)
	tf := groupPartialCreateTerraform(t, config)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	writeConfig := func(value string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(tf.WorkingDir(), "main.tf"), []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := tf.Init(ctx, tfexec.Reattach(reattach)); err != nil {
		t.Fatal(err)
	}
	if err := tf.Import(ctx, acceptanceCollectionUserAddress, collection+"/"+user, tfexec.Reattach(reattach)); err != nil {
		t.Fatal(err)
	}
	imported := true
	t.Cleanup(func() {
		if !imported {
			return
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Minute)
		defer cleanupCancel()
		if err := os.WriteFile(filepath.Join(tf.WorkingDir(), "main.tf"), []byte(config), 0o600); err != nil {
			t.Error(err)
			return
		}
		if err := tf.Destroy(cleanupCtx, tfexec.Reattach(reattach)); err != nil {
			t.Error(err)
		}
	})
	stateBaseline := acceptanceReleaseState(t, ctx, tf)
	if len(stateBaseline) != 1 || len(stateBaseline[0].Instances) != 1 || stateBaseline[0].Instances[0].Attributes["id"] != collection+"/"+user || stateBaseline[0].Instances[0].Attributes["permission"] != permission {
		t.Fatalf("unexpected imported direct-grant state: %+v", stateBaseline)
	}
	retained := func() {
		t.Helper()
		if actual := acceptanceReleaseState(t, ctx, tf); !reflect.DeepEqual(actual, stateBaseline) {
			t.Fatalf("%s refusal changed state: before=%+v after=%+v", reason, stateBaseline, actual)
		}
		if err := api.acceptanceCollectionUserCollateral(collection, nil, baseline); err != nil {
			t.Fatal(err)
		}
	}
	rejected := func(err error, operation string) {
		t.Helper()
		message := ""
		if err != nil {
			message = strings.Join(strings.Fields(err.Error()), " ")
		}
		if !strings.Contains(message, "Unable to "+operation+" collection user") {
			t.Fatalf("expected %s %s refusal, got %v", reason, operation, err)
		}
		if reason == "owner" && !strings.Contains(message, "owner") || reason == "forbidden" && (!strings.Contains(message, "HTTP 403") || !strings.Contains(message, "authorization_error")) {
			t.Fatalf("%s refusal lost diagnostic: %v", reason, err)
		}
		retained()
	}
	writeConfig(denied)
	rejected(tf.Refresh(ctx, tfexec.Reattach(reattach)), "read")
	// Refresh=false lets the rejected import/Create reach the new resource,
	// rather than stopping on refresh of the existing protected resource.
	extra := fmt.Sprintf(`
resource "outline_collection_user" "rejected" {
 collection_id = %q
 user_id = %q
 permission = "read_write"
}
`, collection, user)
	writeConfig(denied + extra)
	rejected(tf.Import(ctx, "outline_collection_user.rejected", collection+"/"+user, tfexec.Reattach(reattach)), "import")
	if reason == "forbidden" {
		target, err := api.acceptanceCreateGrantUser("Terraform collection user forbidden create")
		if err != nil {
			t.Fatal(err)
		}
		extra = strings.Replace(extra, user, target, 1)
	}
	writeConfig(denied + extra)
	rejected(tf.Apply(ctx, tfexec.Reattach(reattach), tfexec.Refresh(false)), "create")
	for _, operation := range []string{"update", "delete"} {
		planned := denied
		if operation == "update" {
			replacement := "admin"
			if permission == "admin" {
				replacement = "read"
			}
			planned = strings.Replace(planned, `permission = "`+permission+`"`, `permission = "`+replacement+`"`, 1)
		}
		writeConfig(planned)
		path := filepath.Join(tf.WorkingDir(), reason+"-"+operation+".tfplan")
		options := []tfexec.PlanOption{tfexec.Reattach(reattach), tfexec.Refresh(false), tfexec.Out(path)}
		if operation == "delete" {
			options = append(options, tfexec.Destroy(true))
		}
		if changed, err := tf.Plan(ctx, options...); err != nil || !changed {
			t.Fatalf("plan rejected %s: changed=%t err=%v", operation, changed, err)
		}
		plan, err := tf.ShowPlanFile(ctx, path, tfexec.Reattach(reattach))
		if err != nil {
			t.Fatal(err)
		}
		if len(plan.ResourceChanges) != 1 || plan.ResourceChanges[0].Address != acceptanceCollectionUserAddress || plan.ResourceChanges[0].Change == nil {
			t.Fatalf("unexpected denied plan: %+v", plan.ResourceChanges)
		}
		actions := plan.ResourceChanges[0].Change.Actions
		if operation == "update" && !actions.Update() || operation == "delete" && !actions.Delete() {
			t.Fatalf("wrong denied %s action: %v", operation, actions)
		}
		rejected(tf.Apply(ctx, tfexec.Reattach(reattach), tfexec.DirOrPlan(path)), operation)
	}
	writeConfig(config)
	if err := tf.Refresh(ctx, tfexec.Reattach(reattach)); err != nil {
		t.Fatal(err)
	}
	retained()
	if changed, err := tf.Plan(ctx, tfexec.Reattach(reattach)); err != nil || changed {
		t.Fatalf("restored key should have empty plan: changed=%t err=%v", changed, err)
	}
	if err := tf.Destroy(ctx, tfexec.Reattach(reattach)); err != nil {
		t.Fatal(err)
	}
	imported = false
	if state := acceptanceReleaseState(t, ctx, tf); len(state) != 0 {
		t.Fatalf("destroy retained grant state: %+v", state)
	}
	if err := api.acceptanceCollectionUserPermission(collection, user, ""); err != nil {
		t.Fatal(err)
	}
	// Deleting a creator grant with another admin key is allowed after import.
	delete(baseline.UserGrants, user)
	if err := api.acceptanceCollectionUserCollateral(collection, nil, baseline); err != nil {
		t.Fatal(err)
	}
}

func acceptanceCollectionUserAbsentRemoval(t *testing.T, api *acceptanceAPI, collection, user string) {
	t.Helper()
	r, err := api.CollectionsRemoveUserWithResponse(t.Context(), client.CollectionsRemoveUserJSONRequestBody{
		Id: uuid.MustParse(collection), UserId: uuid.MustParse(user),
	})
	if err != nil {
		t.Fatal(err)
	}
	absent := r.JSON400
	if r.StatusCode() != http.StatusBadRequest || absent == nil || absent.Ok == nil || *absent.Ok ||
		absent.Status == nil || *absent.Status != http.StatusBadRequest || absent.Error == nil || *absent.Error != "invalid_request" ||
		absent.Message == nil || *absent.Message != "User is not a collection member" {
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
				case "/api/collections.remove_user":
					removes++
				case "/api/collections.memberships":
					if req.GetBody == nil {
						return nil, fmt.Errorf("remove confirmation request has no replayable JSON body")
					}
					body, err := req.GetBody()
					if err != nil {
						return nil, err
					}
					defer func() { _ = body.Close() }()
					var input client.CollectionsMembershipsJSONRequestBody
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
			err = candidate.removeCollectionUser(t.Context(), uuid.MustParse(collection), uuid.MustParse(user))
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
