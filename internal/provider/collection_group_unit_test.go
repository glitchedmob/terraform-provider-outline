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

const cgUnitCollectionID = collectionTestID
const cgUnitGroupID = groupTestOtherID
const cgUnitOtherGroupID = userTestID
const cgUnitGrantID = userTestOtherID
const cgUnitPairID = cgUnitCollectionID + "/" + cgUnitGroupID

func cgUnitModel() collectionGroupModel {
	return collectionGroupModel{ID: types.StringValue(cgUnitPairID), CollectionID: types.StringValue(cgUnitCollectionID), GroupID: types.StringValue(cgUnitGroupID), Permission: types.StringValue("read")}
}

func cgUnitGroup(id string) client.Group {
	group := *groupTestGroup()
	group.Id = groupTestPointer(uuid.MustParse(id))
	return group
}

func cgUnitGrant(group string, permission client.Permission) client.GroupMembership {
	return client.GroupMembership{
		Id: groupTestPointer(cgUnitGrantID), GroupId: groupTestPointer(uuid.MustParse(group)),
		CollectionId: nullable.NewNullableWithValue(uuid.MustParse(cgUnitCollectionID)),
		DocumentId:   nullable.NewNullNullable[uuid.UUID](), SourceId: nullable.NewNullNullable[uuid.UUID](), Permission: &permission,
	}
}

func cgUnitGrants(count int) []client.GroupMembership {
	members := make([]client.GroupMembership, count)
	for i := range members {
		members[i] = cgUnitGrant(uuid.NewString(), client.PermissionRead)
		members[i].Id = groupTestPointer(uuid.NewString())
	}
	return members
}

func cgUnitEnvelope(members []client.GroupMembership, offset, total int, mutation bool) map[string]any {
	groups := make([]client.Group, len(members))
	for i := range members {
		groups[i] = cgUnitGroup(members[i].GroupId.String())
	}
	data := map[string]any{"groupMemberships": members, "groups": groups}
	result := map[string]any{"ok": true, "status": 200, "data": data}
	if !mutation {
		result["pagination"] = client.PaginationResponse{Limit: groupTestPointer(100), Offset: &offset, Total: &total, NextPath: groupTestPointer("https://untrusted.invalid/do-not-follow")}
	}
	return result
}

func cgUnitAssertRequest(t *testing.T, req *http.Request) {
	t.Helper()
	if req.Method != http.MethodPost || req.URL.RawQuery != "" || req.Header.Get("Authorization") != "Bearer "+groupTestKey ||
		(req.URL.Path != "/api/auth.info" && req.Header.Get("Content-Type") != "application/json") {
		t.Errorf("unexpected request or headers: %s %s", req.Method, req.URL)
	}
}

func cgUnitClient(t *testing.T, handler http.HandlerFunc) *apiClient {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		cgUnitAssertRequest(t, req)
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

func cgUnitBody(t *testing.T, w http.ResponseWriter, req *http.Request, want map[string]any) {
	t.Helper()
	var body map[string]any
	if groupTestDecode(t, w, req, &body) && !reflect.DeepEqual(body, want) {
		t.Errorf("%s body=%v, want=%v", req.URL.Path, body, want)
	}
}

func cgUnitOffset(t *testing.T, w http.ResponseWriter, req *http.Request) int {
	t.Helper()
	var body map[string]any
	if !groupTestDecode(t, w, req, &body) {
		return -1
	}
	offset, ok := body["offset"].(float64)
	if !ok || !reflect.DeepEqual(body, map[string]any{"id": cgUnitCollectionID, "limit": float64(100), "offset": offset}) {
		t.Errorf("filtered or incomplete grant list request: %v", body)
		return -1
	}
	return int(offset)
}

func cgUnitParents(t *testing.T, w http.ResponseWriter, req *http.Request) bool {
	t.Helper()
	switch req.URL.Path {
	case "/api/auth.info":
		groupTestEncode(t, w, userTestAuth(userTestOwner()))
	case "/api/collections.info":
		cgUnitBody(t, w, req, map[string]any{"id": cgUnitCollectionID})
		groupTestEncode(t, w, collectionTestEnvelope(collectionTestCollection()))
	case "/api/groups.info":
		cgUnitBody(t, w, req, map[string]any{"id": cgUnitGroupID})
		groupTestEncode(t, w, map[string]any{"ok": true, "status": 200, "data": cgUnitGroup(cgUnitGroupID)})
	default:
		return false
	}
	return true
}

func cgUnitPlan(t *testing.T, r resource.Resource, model collectionGroupModel) tfsdk.Plan {
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

func cgUnitStateModel(t *testing.T, state tfsdk.State) collectionGroupModel {
	t.Helper()
	var model collectionGroupModel
	if diagnostics := state.Get(t.Context(), &model); diagnostics.HasError() {
		t.Fatal(diagnostics)
	}
	return model
}

func cgUnitOperation(t *testing.T, r *collectionGroupResource, operation string, current, desired collectionGroupModel) (diag.Diagnostics, tfsdk.State) {
	t.Helper()
	state := tfsdk.State(cgUnitPlan(t, r, current))
	switch operation {
	case "create":
		desired.ID = types.StringUnknown()
		plan := cgUnitPlan(t, r, desired)
		resp := resource.CreateResponse{State: tfsdk.State{Schema: plan.Schema, Raw: tftypes.NewValue(plan.Raw.Type(), nil)}}
		r.Create(t.Context(), resource.CreateRequest{Plan: plan}, &resp)
		return resp.Diagnostics, resp.State
	case "read":
		resp := resource.ReadResponse{State: state}
		r.Read(t.Context(), resource.ReadRequest{State: state}, &resp)
		return resp.Diagnostics, resp.State
	case "update":
		resp := resource.UpdateResponse{State: state}
		r.Update(t.Context(), resource.UpdateRequest{State: state, Plan: cgUnitPlan(t, r, desired)}, &resp)
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

func cgUnitAssertFailureState(t *testing.T, operation string, diagnostics diag.Diagnostics, state tfsdk.State, current collectionGroupModel) {
	t.Helper()
	if !diagnostics.HasError() {
		t.Fatal("operation accepted an unsafe response")
	}
	if operation == "create" || operation == "import" {
		if !state.Raw.IsNull() {
			t.Fatalf("failed preflight adopted a pair: %v", state.Raw)
		}
	} else if cgUnitStateModel(t, state) != current {
		t.Fatalf("failed %s changed state", operation)
	}
}

func TestCollectionGroupLifecycleRequestShapesAndNoCollateral(t *testing.T) {
	other := cgUnitGrant(cgUnitOtherGroupID, client.PermissionAdmin)
	other.Id = groupTestPointer(uuid.NewString())
	members := []client.GroupMembership{other}
	collection := collectionTestCollection()
	// Unlike the collection resource, grants allow a collection's admin default.
	collection.Permission = nullable.NewNullableWithValue(client.PermissionAdmin)
	beforeCollection := *collection
	group := cgUnitGroup(cgUnitGroupID)
	beforeGroup := group
	var writes []string
	lists := 0
	desiredPermission := client.PermissionRead
	r := &collectionGroupResource{api: cgUnitClient(t, func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/api/collections.info":
			cgUnitBody(t, w, req, map[string]any{"id": cgUnitCollectionID})
			groupTestEncode(t, w, collectionTestEnvelope(collection))
		case "/api/groups.info":
			cgUnitBody(t, w, req, map[string]any{"id": cgUnitGroupID})
			groupTestEncode(t, w, map[string]any{"ok": true, "status": 200, "data": group})
		case "/api/collections.group_memberships":
			lists++
			cgUnitOffset(t, w, req)
			groupTestEncode(t, w, cgUnitEnvelope(members, 0, len(members), false))
		case "/api/collections.add_group":
			cgUnitBody(t, w, req, map[string]any{"id": cgUnitCollectionID, "groupId": cgUnitGroupID, "permission": string(desiredPermission)})
			writes = append(writes, req.URL.Path)
			member := cgUnitGrant(cgUnitGroupID, desiredPermission)
			members = []client.GroupMembership{other, member}
			groupTestEncode(t, w, cgUnitEnvelope([]client.GroupMembership{member}, 0, 0, true))
		case "/api/collections.remove_group":
			cgUnitBody(t, w, req, map[string]any{"id": cgUnitCollectionID, "groupId": cgUnitGroupID})
			writes = append(writes, req.URL.Path)
			members = []client.GroupMembership{other}
			groupTestEncode(t, w, map[string]any{"ok": true, "status": 200, "success": true})
		default:
			if !cgUnitParents(t, w, req) {
				t.Errorf("unexpected operation: %s", req.URL.Path)
			}
		}
	})}
	current := cgUnitModel()
	diagnostics, state := cgUnitOperation(t, r, "create", current, current)
	if diagnostics.HasError() || cgUnitStateModel(t, state) != current {
		t.Fatalf("create: %v", diagnostics)
	}
	for _, permission := range []client.Permission{client.PermissionReadWrite, client.PermissionAdmin} {
		desired := current
		desired.Permission = types.StringValue(string(permission))
		desiredPermission = permission
		diagnostics, state = cgUnitOperation(t, r, "update", current, desired)
		if diagnostics.HasError() || cgUnitStateModel(t, state) != desired {
			t.Fatalf("update %s: %v", permission, diagnostics)
		}
		current = desired
	}
	// A no-op update and a refresh never replay the upsert.
	diagnostics, _ = cgUnitOperation(t, r, "update", current, current)
	if diagnostics.HasError() || len(writes) != 3 {
		t.Fatalf("no-op update: %v writes=%v", diagnostics, writes)
	}
	// The grant row UUID is not Terraform's identity, even if an upsert or
	// outside change replaces the row while keeping the same pair.
	members[1].Id = groupTestPointer(uuid.NewString())
	diagnostics, state = cgUnitOperation(t, r, "read", cgUnitModel(), current)
	if diagnostics.HasError() || cgUnitStateModel(t, state) != current || len(writes) != 3 {
		t.Fatalf("permission drift: %v", diagnostics)
	}
	diagnostics, _ = cgUnitOperation(t, r, "delete", current, current)
	if diagnostics.HasError() || lists != 7 {
		t.Fatalf("delete must confirm removal with the full list: %v lists=%d", diagnostics, lists)
	}
	diagnostics, state = cgUnitOperation(t, r, "read", current, current)
	if diagnostics.HasError() || !state.Raw.IsNull() {
		t.Fatalf("missing grant: %v", diagnostics)
	}
	diagnostics, _ = cgUnitOperation(t, r, "delete", current, current)
	if diagnostics.HasError() {
		t.Fatal(diagnostics)
	}
	if !reflect.DeepEqual(members, []client.GroupMembership{other}) || !reflect.DeepEqual(*collection, beforeCollection) || !reflect.DeepEqual(group, beforeGroup) ||
		!reflect.DeepEqual(writes, []string{"/api/collections.add_group", "/api/collections.add_group", "/api/collections.add_group", "/api/collections.remove_group"}) {
		t.Fatalf("collateral edits or wrong writes: %v", writes)
	}
}

func TestCollectionGroupCreateAllPermissionsAndRefuseExisting(t *testing.T) {
	for _, permission := range []client.Permission{client.PermissionRead, client.PermissionReadWrite, client.PermissionAdmin} {
		for _, remotePermission := range []client.Permission{"", client.PermissionRead, client.PermissionReadWrite, client.PermissionAdmin} {
			existing := remotePermission != ""
			t.Run(fmt.Sprintf("%s/remote=%s", permission, remotePermission), func(t *testing.T) {
				writes := 0
				r := &collectionGroupResource{api: cgUnitClient(t, func(w http.ResponseWriter, req *http.Request) {
					if cgUnitParents(t, w, req) {
						return
					}
					switch req.URL.Path {
					case "/api/collections.group_memberships":
						cgUnitOffset(t, w, req)
						members := []client.GroupMembership{}
						if existing {
							members = append(members, cgUnitGrant(cgUnitGroupID, remotePermission))
						}
						groupTestEncode(t, w, cgUnitEnvelope(members, 0, len(members), false))
					case "/api/collections.add_group":
						writes++
						cgUnitBody(t, w, req, map[string]any{"id": cgUnitCollectionID, "groupId": cgUnitGroupID, "permission": string(permission)})
						groupTestEncode(t, w, cgUnitEnvelope([]client.GroupMembership{cgUnitGrant(cgUnitGroupID, permission)}, 0, 0, true))
					default:
						t.Errorf("unexpected endpoint: %s", req.URL.Path)
					}
				})}
				model := cgUnitModel()
				model.Permission = types.StringValue(string(permission))
				diagnostics, state := cgUnitOperation(t, r, "create", model, model)
				if existing {
					groupTestDiagnostics(t, diagnostics, "Import the existing pair using "+cgUnitPairID)
					if writes != 0 || !state.Raw.IsNull() {
						t.Fatal("create adopted or changed an existing pair")
					}
				} else if diagnostics.HasError() || writes != 1 || cgUnitStateModel(t, state) != model {
					t.Fatalf("create %s: %v writes=%d", permission, diagnostics, writes)
				}
			})
		}
	}
}
