// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/glitchedmob/terraform-provider-outline/internal/client"
	"github.com/google/uuid"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"github.com/oapi-codegen/nullable"
	"golang.org/x/time/rate"
)

const cuUnitCollectionID = collectionTestID
const cuUnitUserID = userTestID
const cuUnitOtherUserID = userTestOtherID
const cuUnitGrantID = groupTestOtherID
const cuUnitPairID = cuUnitCollectionID + "/" + cuUnitUserID

func cuUnitModel() collectionUserModel {
	return collectionUserModel{ID: types.StringValue(cuUnitPairID), CollectionID: types.StringValue(cuUnitCollectionID), UserID: types.StringValue(cuUnitUserID), Permission: types.StringValue("read")}
}

// Grant endpoints present public users, without includeDetails or email.
func cuUnitUser(id string) client.User {
	return client.User{Id: groupTestPointer(uuid.MustParse(id)), Name: groupTestPointer("Target user"),
		Role: groupTestPointer(client.UserRoleMember), IsSuspended: groupTestPointer(false)}
}

// users.info and users.list keep the stricter account-read contract.
func cuUnitParentUser(id string) client.User {
	user := *userTestUser()
	user.Id = groupTestPointer(uuid.MustParse(id))
	return user
}

func cuUnitGrant(user string, permission client.Permission) client.Membership {
	return client.Membership{
		Id: groupTestPointer(cuUnitGrantID), UserId: groupTestPointer(uuid.MustParse(user)),
		CollectionId: nullable.NewNullableWithValue(uuid.MustParse(cuUnitCollectionID)),
		DocumentId:   nullable.NewNullNullable[uuid.UUID](), SourceId: nullable.NewNullNullable[uuid.UUID](), Permission: &permission,
	}
}

func cuUnitGrants(count int) []client.Membership {
	members := make([]client.Membership, count)
	permissions := []client.Permission{client.PermissionRead, client.PermissionReadWrite, client.PermissionAdmin}
	for i := range members {
		members[i] = cuUnitGrant(uuid.NewString(), permissions[i%len(permissions)])
		members[i].Id = groupTestPointer(uuid.NewString())
	}
	return members
}

func cuUnitEnvelope(members []client.Membership, offset, total int, mutation bool) map[string]any {
	users := make([]client.User, len(members))
	roles := []client.UserRole{client.UserRoleAdmin, client.UserRoleMember, client.UserRoleViewer, client.UserRoleGuest}
	for i := range members {
		users[i] = cuUnitUser(members[i].UserId.String())
		users[i].Role = groupTestPointer(roles[i%len(roles)])
		users[i].IsSuspended = groupTestPointer(i%2 != 0)
	}
	data := map[string]any{"memberships": members, "users": users}
	result := map[string]any{"ok": true, "status": 200, "data": data}
	if !mutation {
		result["pagination"] = client.PaginationResponse{Limit: groupTestPointer(100), Offset: &offset, Total: &total, NextPath: groupTestPointer("https://untrusted.invalid/do-not-follow")}
	}
	return result
}

func cuUnitAssertRequest(t *testing.T, req *http.Request) {
	t.Helper()
	if req.Method != http.MethodPost || req.URL.RawQuery != "" || req.Header.Get("Authorization") != "Bearer "+groupTestKey ||
		(req.URL.Path != "/api/auth.info" && req.Header.Get("Content-Type") != "application/json") {
		t.Errorf("unexpected request or headers: %s %s", req.Method, req.URL)
	}
}

func cuUnitClient(t *testing.T, handler http.HandlerFunc) *apiClient {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		cuUnitAssertRequest(t, req)
		w.Header().Set("Content-Type", "application/json")
		handler(w, req)
	}))
	t.Cleanup(server.Close)
	api, err := newAPIClient(server.URL+"/api", groupTestKey, 5, "unit")
	if err != nil {
		t.Fatal(err)
	}
	api.httpClient.Transport.(*bearerTransport).limiter = rate.NewLimiter(rate.Inf, 1)
	return api
}

func cuUnitBody(t *testing.T, w http.ResponseWriter, req *http.Request, want map[string]any) {
	t.Helper()
	var body map[string]any
	if groupTestDecode(t, w, req, &body) && !reflect.DeepEqual(body, want) {
		t.Errorf("%s body=%v, want=%v", req.URL.Path, body, want)
	}
}

func cuUnitOffset(t *testing.T, w http.ResponseWriter, req *http.Request) int {
	t.Helper()
	var body map[string]any
	if !groupTestDecode(t, w, req, &body) {
		return -1
	}
	offset, ok := body["offset"].(float64)
	if !ok || !reflect.DeepEqual(body, map[string]any{"id": cuUnitCollectionID, "limit": float64(100), "offset": offset}) {
		t.Errorf("filtered or incomplete grant list request: %v", body)
		return -1
	}
	return int(offset)
}

func cuUnitParents(t *testing.T, w http.ResponseWriter, req *http.Request) bool {
	t.Helper()
	switch req.URL.Path {
	case "/api/auth.info":
		groupTestEncode(t, w, userTestAuth(userTestOwner()))
	case "/api/collections.info":
		cuUnitBody(t, w, req, map[string]any{"id": cuUnitCollectionID})
		groupTestEncode(t, w, collectionTestEnvelope(collectionTestCollection()))
	case "/api/users.info":
		cuUnitBody(t, w, req, map[string]any{"id": cuUnitUserID})
		groupTestEncode(t, w, userTestEnvelope(groupTestPointer(cuUnitParentUser(cuUnitUserID))))
	default:
		return false
	}
	return true
}

func cuUnitPlan(t *testing.T, r resource.Resource, model collectionUserModel) tfsdk.Plan {
	t.Helper()
	var schema resource.SchemaResponse
	r.Schema(t.Context(), resource.SchemaRequest{}, &schema)
	if schema.Diagnostics.HasError() {
		t.Fatal(schema.Diagnostics)
	}
	plan := tfsdk.Plan{Schema: schema.Schema}
	if diagnostics := plan.Set(t.Context(), &model); diagnostics.HasError() {
		t.Fatal(diagnostics)
	}
	return plan
}

func cuUnitStateModel(t *testing.T, state tfsdk.State) collectionUserModel {
	t.Helper()
	var model collectionUserModel
	if diagnostics := state.Get(t.Context(), &model); diagnostics.HasError() {
		t.Fatal(diagnostics)
	}
	return model
}

func cuUnitOperation(t *testing.T, r *collectionUserResource, operation string, current, desired collectionUserModel) (diag.Diagnostics, tfsdk.State) {
	t.Helper()
	state := tfsdk.State(cuUnitPlan(t, r, current))
	switch operation {
	case "create":
		desired.ID = types.StringUnknown()
		plan := cuUnitPlan(t, r, desired)
		resp := resource.CreateResponse{State: tfsdk.State{Schema: plan.Schema, Raw: tftypes.NewValue(plan.Raw.Type(), nil)}}
		r.Create(t.Context(), resource.CreateRequest{Plan: plan}, &resp)
		return resp.Diagnostics, resp.State
	case "read":
		resp := resource.ReadResponse{State: state}
		r.Read(t.Context(), resource.ReadRequest{State: state}, &resp)
		return resp.Diagnostics, resp.State
	case "update":
		resp := resource.UpdateResponse{State: state}
		r.Update(t.Context(), resource.UpdateRequest{State: state, Plan: cuUnitPlan(t, r, desired)}, &resp)
		return resp.Diagnostics, resp.State
	case "delete":
		resp := resource.DeleteResponse{State: state}
		r.Delete(t.Context(), resource.DeleteRequest{State: state}, &resp)
		return resp.Diagnostics, resp.State
	case "import":
		state.Raw = tftypes.NewValue(state.Raw.Type(), nil)
		resp := resource.ImportStateResponse{State: state}
		r.ImportState(t.Context(), resource.ImportStateRequest{ID: desired.ID.ValueString()}, &resp)
		return resp.Diagnostics, resp.State
	default:
		t.Fatalf("unknown operation %q", operation)
		return nil, state
	}
}

func cuUnitAssertFailureState(t *testing.T, operation string, diagnostics diag.Diagnostics, state tfsdk.State, current collectionUserModel) {
	t.Helper()
	if !diagnostics.HasError() {
		t.Fatal("operation accepted an unsafe response")
	}
	if operation == "create" || operation == "import" {
		if !state.Raw.IsNull() {
			t.Fatalf("failed preflight adopted a pair: %v", state.Raw)
		}
	} else if cuUnitStateModel(t, state) != current {
		t.Fatalf("failed %s changed state", operation)
	}
}

func TestCollectionUserLifecycleRequestShapesAndNoCollateral(t *testing.T) {
	other := cuUnitGrant(cuUnitOtherUserID, client.PermissionAdmin)
	other.Id = groupTestPointer(uuid.NewString())
	members := []client.Membership{other}
	collection := collectionTestCollection()
	// Unlike the collection resource, grants allow a collection's admin default.
	collection.Permission = nullable.NewNullableWithValue(client.PermissionAdmin)
	beforeCollection := *collection
	user := cuUnitParentUser(cuUnitUserID)
	beforeUser := user
	var writes []string
	lists := 0
	desiredPermission := client.PermissionRead
	r := &collectionUserResource{api: cuUnitClient(t, func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/api/collections.info":
			cuUnitBody(t, w, req, map[string]any{"id": cuUnitCollectionID})
			groupTestEncode(t, w, collectionTestEnvelope(collection))
		case "/api/users.info":
			cuUnitBody(t, w, req, map[string]any{"id": cuUnitUserID})
			groupTestEncode(t, w, userTestEnvelope(&user))
		case "/api/collections.memberships":
			lists++
			cuUnitOffset(t, w, req)
			groupTestEncode(t, w, cuUnitEnvelope(members, 0, len(members), false))
		case "/api/collections.add_user":
			cuUnitBody(t, w, req, map[string]any{"id": cuUnitCollectionID, "userId": cuUnitUserID, "permission": string(desiredPermission)})
			writes = append(writes, req.URL.Path)
			member := cuUnitGrant(cuUnitUserID, desiredPermission)
			members = []client.Membership{other, member}
			groupTestEncode(t, w, cuUnitEnvelope([]client.Membership{member}, 0, 0, true))
		case "/api/collections.remove_user":
			cuUnitBody(t, w, req, map[string]any{"id": cuUnitCollectionID, "userId": cuUnitUserID})
			writes = append(writes, req.URL.Path)
			members = []client.Membership{other}
			groupTestEncode(t, w, map[string]any{"ok": true, "status": 200, "success": true})
		default:
			if !cuUnitParents(t, w, req) {
				t.Errorf("unexpected operation: %s", req.URL.Path)
			}
		}
	})}
	current := cuUnitModel()
	diagnostics, state := cuUnitOperation(t, r, "create", current, current)
	if diagnostics.HasError() || cuUnitStateModel(t, state) != current {
		t.Fatalf("create: %v", diagnostics)
	}
	for _, permission := range []client.Permission{client.PermissionReadWrite, client.PermissionAdmin} {
		desired := current
		desired.Permission = types.StringValue(string(permission))
		desiredPermission = permission
		diagnostics, state = cuUnitOperation(t, r, "update", current, desired)
		if diagnostics.HasError() || cuUnitStateModel(t, state) != desired {
			t.Fatalf("update %s: %v", permission, diagnostics)
		}
		current = desired
	}
	// A no-op update and a refresh never replay the upsert.
	diagnostics, _ = cuUnitOperation(t, r, "update", current, current)
	if diagnostics.HasError() || len(writes) != 3 {
		t.Fatalf("no-op update: %v writes=%v", diagnostics, writes)
	}
	// The grant row UUID is not Terraform's identity, even if an upsert or
	// outside change replaces the row while keeping the same pair.
	members[1].Id = groupTestPointer(uuid.NewString())
	diagnostics, state = cuUnitOperation(t, r, "read", cuUnitModel(), current)
	if diagnostics.HasError() || cuUnitStateModel(t, state) != current || len(writes) != 3 {
		t.Fatalf("permission drift: %v", diagnostics)
	}
	diagnostics, _ = cuUnitOperation(t, r, "delete", current, current)
	if diagnostics.HasError() || lists != 7 {
		t.Fatalf("delete must confirm removal with the full list: %v lists=%d", diagnostics, lists)
	}
	diagnostics, state = cuUnitOperation(t, r, "read", current, current)
	if diagnostics.HasError() || !state.Raw.IsNull() {
		t.Fatalf("missing grant: %v", diagnostics)
	}
	diagnostics, _ = cuUnitOperation(t, r, "delete", current, current)
	if diagnostics.HasError() {
		t.Fatal(diagnostics)
	}
	if !reflect.DeepEqual(members, []client.Membership{other}) || !reflect.DeepEqual(*collection, beforeCollection) || !reflect.DeepEqual(user, beforeUser) ||
		!reflect.DeepEqual(writes, []string{"/api/collections.add_user", "/api/collections.add_user", "/api/collections.add_user", "/api/collections.remove_user"}) {
		t.Fatalf("collateral edits or wrong writes: %v", writes)
	}
}

func TestCollectionUserCreateAllPermissionsAndRefuseExisting(t *testing.T) {
	for _, permission := range []client.Permission{client.PermissionRead, client.PermissionReadWrite, client.PermissionAdmin} {
		for _, remotePermission := range []client.Permission{"", client.PermissionRead, client.PermissionReadWrite, client.PermissionAdmin} {
			existing := remotePermission != ""
			t.Run(fmt.Sprintf("%s/remote=%s", permission, remotePermission), func(t *testing.T) {
				writes := 0
				r := &collectionUserResource{api: cuUnitClient(t, func(w http.ResponseWriter, req *http.Request) {
					if cuUnitParents(t, w, req) {
						return
					}
					switch req.URL.Path {
					case "/api/collections.memberships":
						cuUnitOffset(t, w, req)
						members := []client.Membership{}
						if existing {
							members = append(members, cuUnitGrant(cuUnitUserID, remotePermission))
						}
						groupTestEncode(t, w, cuUnitEnvelope(members, 0, len(members), false))
					case "/api/collections.add_user":
						writes++
						cuUnitBody(t, w, req, map[string]any{"id": cuUnitCollectionID, "userId": cuUnitUserID, "permission": string(permission)})
						groupTestEncode(t, w, cuUnitEnvelope([]client.Membership{cuUnitGrant(cuUnitUserID, permission)}, 0, 0, true))
					default:
						t.Errorf("unexpected endpoint: %s", req.URL.Path)
					}
				})}
				model := cuUnitModel()
				model.Permission = types.StringValue(string(permission))
				diagnostics, state := cuUnitOperation(t, r, "create", model, model)
				if existing {
					groupTestDiagnostics(t, diagnostics, "Import the existing pair using "+cuUnitPairID)
					if writes != 0 || !state.Raw.IsNull() {
						t.Fatal("create adopted or changed an existing pair")
					}
				} else if diagnostics.HasError() || writes != 1 || cuUnitStateModel(t, state) != model {
					t.Fatalf("create %s: %v writes=%d", permission, diagnostics, writes)
				}
			})
		}
	}
}
