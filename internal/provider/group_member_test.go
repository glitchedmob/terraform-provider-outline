// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/glitchedmob/terraform-provider-outline/internal/client"
	"github.com/google/uuid"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/oapi-codegen/nullable"
	"golang.org/x/time/rate"
)

func memberTestModel() groupMemberModel {
	return groupMemberModel{ID: types.StringValue(groupTestID + "/" + userTestID), GroupID: types.StringValue(groupTestID), UserID: types.StringValue(userTestID), Permission: types.StringValue("member")}
}

func memberTestMember(user string, permission client.GroupPermission) client.GroupUser {
	u := userTestUser()
	u.Id = groupTestPointer(uuid.MustParse(user))
	// Public group users do not include email in the released server.
	u.Email = nil
	return client.GroupUser{Id: groupTestPointer(user + "-" + groupTestID), GroupId: groupTestPointer(uuid.MustParse(groupTestID)), UserId: u.Id, Permission: &permission, User: u}
}

func memberTestEnvelope(members []client.GroupUser, offset, total int, mutation bool) map[string]any {
	users := make([]client.User, len(members))
	for i := range members {
		users[i] = *members[i].User
	}
	data := map[string]any{"groupMemberships": members, "users": users}
	result := map[string]any{"ok": true, "status": 200, "data": data}
	if mutation {
		data["groups"] = []client.Group{*groupTestGroup()}
	} else {
		result["pagination"] = client.PaginationResponse{Limit: groupTestPointer(100), Offset: &offset, Total: &total}
	}
	return result
}

func memberTestClient(t *testing.T, handler http.HandlerFunc) *apiClient {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost || req.URL.RawQuery != "" || req.Header.Get("Authorization") != "Bearer "+groupTestKey ||
			(req.URL.Path != "/api/auth.info" && req.Header.Get("Content-Type") != "application/json") {
			t.Errorf("unexpected request %s %s or missing bearer/JSON headers", req.Method, req.URL)
		}
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

func memberTestParents(t *testing.T, w http.ResponseWriter, req *http.Request) bool {
	t.Helper()
	switch req.URL.Path {
	case "/api/auth.info":
		groupTestEncode(t, w, userTestAuth(userTestOwner()))
	case "/api/groups.info":
		memberTestBody(t, w, req, map[string]any{"id": groupTestID})
		groupTestEncode(t, w, map[string]any{"ok": true, "status": 200, "data": groupTestGroup()})
	case "/api/users.info":
		memberTestBody(t, w, req, map[string]any{"id": userTestID})
		groupTestEncode(t, w, userTestEnvelope(userTestUser()))
	default:
		return false
	}
	return true
}

func memberTestBody(t *testing.T, w http.ResponseWriter, req *http.Request, want map[string]any) {
	t.Helper()
	var body map[string]any
	if groupTestDecode(t, w, req, &body) && !reflect.DeepEqual(body, want) {
		t.Errorf("%s body=%v, want=%v", req.URL.Path, body, want)
	}
}

func memberTestPlan(t *testing.T, r *groupMemberResource, model groupMemberModel) tfsdk.Plan {
	t.Helper()
	var s resource.SchemaResponse
	r.Schema(t.Context(), resource.SchemaRequest{}, &s)
	plan := tfsdk.Plan{Schema: s.Schema}
	if diags := plan.Set(t.Context(), &model); diags.HasError() {
		t.Fatal(diags)
	}
	return plan
}

func memberTestState(t *testing.T, state tfsdk.State) groupMemberModel {
	t.Helper()
	var model groupMemberModel
	if diags := state.Get(t.Context(), &model); diags.HasError() {
		t.Fatal(diags)
	}
	return model
}

func memberTestCreate(t *testing.T, r *groupMemberResource, model groupMemberModel) resource.CreateResponse {
	t.Helper()
	model.ID = types.StringUnknown()
	plan := memberTestPlan(t, r, model)
	resp := resource.CreateResponse{State: tfsdk.State{Schema: plan.Schema}}
	r.Create(t.Context(), resource.CreateRequest{Plan: plan}, &resp)
	return resp
}

func TestGroupMemberLifecycleRequestShapesAndCollateral(t *testing.T) {
	members := []client.GroupUser{memberTestMember(userTestOtherID, client.GroupPermissionAdmin)}
	other := members[0]
	var writes []string
	r := &groupMemberResource{api: memberTestClient(t, func(w http.ResponseWriter, req *http.Request) {
		if memberTestParents(t, w, req) {
			return
		}
		switch req.URL.Path {
		case "/api/groups.memberships":
			memberTestBody(t, w, req, map[string]any{"id": groupTestID, "limit": float64(100), "offset": float64(0)})
			groupTestEncode(t, w, memberTestEnvelope(members, 0, len(members), false))
		case "/api/groups.add_user", "/api/groups.update_user":
			permission := client.GroupPermissionMember
			if req.URL.Path == "/api/groups.update_user" {
				permission = client.GroupPermissionAdmin
			}
			memberTestBody(t, w, req, map[string]any{"id": groupTestID, "userId": userTestID, "permission": string(permission)})
			writes = append(writes, req.URL.Path)
			m := memberTestMember(userTestID, permission)
			members = []client.GroupUser{other, m}
			groupTestEncode(t, w, memberTestEnvelope([]client.GroupUser{m}, 0, 0, true))
		case "/api/groups.remove_user":
			memberTestBody(t, w, req, map[string]any{"id": groupTestID, "userId": userTestID})
			writes = append(writes, req.URL.Path)
			members = []client.GroupUser{other}
			groupTestEncode(t, w, map[string]any{"ok": true, "status": 200, "data": map[string]any{"groups": []client.Group{*groupTestGroup()}}})
		default:
			t.Errorf("unexpected endpoint %s", req.URL.Path)
		}
	})}
	created := memberTestCreate(t, r, memberTestModel())
	if created.Diagnostics.HasError() || memberTestState(t, created.State) != memberTestModel() {
		t.Fatalf("create: %v", created.Diagnostics)
	}
	model := memberTestModel()
	model.Permission = types.StringValue("admin")
	updated := resource.UpdateResponse{State: created.State}
	r.Update(t.Context(), resource.UpdateRequest{State: created.State, Plan: memberTestPlan(t, r, model)}, &updated)
	if updated.Diagnostics.HasError() || memberTestState(t, updated.State) != model {
		t.Fatalf("update: %v", updated.Diagnostics)
	}
	// A permission drift is read, not rewritten during refresh.
	read := resource.ReadResponse{State: created.State}
	r.Read(t.Context(), resource.ReadRequest{State: created.State}, &read)
	if read.Diagnostics.HasError() || memberTestState(t, read.State) != model {
		t.Fatalf("read permission drift: %v", read.Diagnostics)
	}
	deleted := resource.DeleteResponse{State: updated.State}
	r.Delete(t.Context(), resource.DeleteRequest{State: updated.State}, &deleted)
	if deleted.Diagnostics.HasError() {
		t.Fatal(deleted.Diagnostics)
	}
	if len(members) != 1 || !reflect.DeepEqual(members[0], other) || !reflect.DeepEqual(writes, []string{"/api/groups.add_user", "/api/groups.update_user", "/api/groups.remove_user"}) {
		t.Fatalf("collateral or wrong mutations: %v", writes)
	}
	read = resource.ReadResponse{State: updated.State}
	r.Read(t.Context(), resource.ReadRequest{State: updated.State}, &read)
	if read.Diagnostics.HasError() || !read.State.Raw.IsNull() {
		t.Fatalf("missing membership: %v", read.Diagnostics)
	}
}

func TestGroupMemberCreateExistingPairRequiresImport(t *testing.T) {
	for _, permission := range []client.GroupPermission{client.GroupPermissionMember, client.GroupPermissionAdmin} {
		t.Run(string(permission), func(t *testing.T) {
			r := &groupMemberResource{api: memberTestClient(t, func(w http.ResponseWriter, req *http.Request) {
				if memberTestParents(t, w, req) {
					return
				}
				if req.URL.Path != "/api/groups.memberships" {
					t.Errorf("existing pair was mutated: %s", req.URL.Path)
					return
				}
				groupTestEncode(t, w, memberTestEnvelope([]client.GroupUser{memberTestMember(userTestID, permission)}, 0, 1, false))
			})}
			resp := memberTestCreate(t, r, memberTestModel())
			groupTestDiagnostics(t, resp.Diagnostics, "Import the existing pair using "+groupTestID+"/"+userTestID)
			if resp.State.Raw.IsKnown() && !resp.State.Raw.IsNull() {
				t.Fatal("create adopted existing pair")
			}
		})
	}
}

func TestGroupMemberPaginationCompleteValidatedSet(t *testing.T) {
	for _, target := range []int{-1, 0, 100} {
		t.Run(fmt.Sprint(target), func(t *testing.T) {
			var offsets []int
			api := memberTestClient(t, func(w http.ResponseWriter, req *http.Request) {
				if req.URL.Path != "/api/groups.memberships" {
					t.Errorf("unexpected endpoint %s", req.URL.Path)
					return
				}
				var body client.GroupsMembershipsJSONRequestBody
				groupTestDecode(t, w, req, &body)
				if body.Id.String() != groupTestID || body.Limit == nil || *body.Limit != 100 || body.Offset == nil || body.Query != nil || body.Permission != nil {
					t.Errorf("filtered/incomplete request: %+v", body)
					return
				}
				offset := *body.Offset
				offsets = append(offsets, offset)
				count := 100
				if offset == 100 {
					count = 1
				}
				members := make([]client.GroupUser, count)
				for i := range members {
					id := uuid.New().String()
					if offset+i == target {
						id = userTestID
					}
					members[i] = memberTestMember(id, client.GroupPermissionAdmin)
				}
				groupTestEncode(t, w, memberTestEnvelope(members, offset, 101, false))
			})
			m, err := api.readGroupMemberPages(t.Context(), uuid.MustParse(groupTestID), uuid.MustParse(userTestID))
			if err != nil || (m == nil) != (target == -1) || !reflect.DeepEqual(offsets, []int{0, 100}) {
				t.Fatalf("full pagination: member=%v err=%v offsets=%v", m, err, offsets)
			}
		})
	}
}

func TestGroupMemberMalformedPagesNeverProveAbsence(t *testing.T) {
	for _, failure := range []string{"missing json", "bad envelope", "missing data", "missing members", "missing users", "missing pagination", "short page", "wrong offset", "changed total", "duplicate", "wrong group", "wrong API id", "missing user id", "nil UUID", "invalid UUID", "wrong nested user", "missing nested user", "missing permission", "invalid permission", "missing name", "invalid user role", "missing suspension", "inconsistent users", "duplicate users", "403", "404", "429", "invalid json"} {
		t.Run(failure, func(t *testing.T) {
			first := make([]client.GroupUser, 100)
			for i := range first {
				first[i] = memberTestMember(uuid.New().String(), client.GroupPermissionMember)
			}
			first[0] = memberTestMember(userTestID, client.GroupPermissionAdmin)
			pageCalls := 0
			r := &groupMemberResource{api: memberTestClient(t, func(w http.ResponseWriter, req *http.Request) {
				if memberTestParents(t, w, req) {
					return
				}
				if req.URL.Path != "/api/groups.memberships" {
					t.Errorf("unexpected write %s", req.URL.Path)
					return
				}
				pageCalls++
				if pageCalls%2 == 1 {
					groupTestEncode(t, w, memberTestEnvelope(first, 0, 101, false))
					return
				}
				m := memberTestMember(uuid.New().String(), client.GroupPermissionMember)
				switch failure {
				case "wrong group":
					m.GroupId = groupTestPointer(uuid.MustParse(groupTestOtherID))
				case "wrong API id":
					m.Id = groupTestPointer(groupTestID + "/" + userTestID)
				case "missing user id":
					m.UserId = nil
				case "nil UUID":
					m.UserId = groupTestPointer(uuid.Nil)
				case "invalid UUID":
					m.UserId = groupTestPointer(uuid.MustParse("d32c2ee6-fbde-0654-841b-0eabdc71b812"))
				case "wrong nested user":
					m.User.Id = groupTestPointer(uuid.MustParse(userTestOtherID))
				case "missing permission":
					m.Permission = nil
				case "invalid permission":
					m.Permission = groupTestPointer(client.GroupPermission("owner"))
				case "missing name":
					m.User.Name = nil
				case "invalid user role":
					m.User.Role = groupTestPointer(client.UserRole("owner"))
				case "missing suspension":
					m.User.IsSuspended = nil
				case "duplicate":
					m = first[0]
				}
				p := memberTestEnvelope([]client.GroupUser{m}, 100, 101, false)
				d := p["data"].(map[string]any)
				switch failure {
				case "missing json":
					w.Header().Set("Content-Type", "text/plain")
				case "invalid json":
					_, _ = w.Write([]byte("{bad"))
					return
				case "bad envelope":
					p["ok"] = false
				case "missing data":
					delete(p, "data")
				case "missing members":
					delete(d, "groupMemberships")
				case "missing users":
					delete(d, "users")
				case "missing pagination":
					delete(p, "pagination")
				case "short page":
					p["pagination"] = client.PaginationResponse{Limit: groupTestPointer(100), Offset: groupTestPointer(100), Total: groupTestPointer(103)}
				case "wrong offset":
					p["pagination"] = client.PaginationResponse{Limit: groupTestPointer(100), Offset: groupTestPointer(0), Total: groupTestPointer(101)}
				case "changed total":
					p = memberTestEnvelope([]client.GroupUser{m, memberTestMember(uuid.New().String(), client.GroupPermissionMember)}, 100, 102, false)
				case "inconsistent users":
					d["users"] = []client.User{*userTestUser()}
				case "duplicate users":
					d["users"] = []client.User{*m.User, *m.User}
				case "missing nested user":
					m.User = nil
					d["groupMemberships"] = []client.GroupUser{m}
				case "403", "404", "429":
					code := map[string]int{"403": 403, "404": 404, "429": 429}[failure]
					w.WriteHeader(code)
					p = map[string]any{"ok": false, "error": "authorization_error", "message": groupTestKey}
				}
				groupTestEncode(t, w, p)
			})}
			state := tfsdk.State(memberTestPlan(t, r, memberTestModel()))
			resp := resource.ReadResponse{State: state}
			r.Read(t.Context(), resource.ReadRequest{State: state}, &resp)
			if !resp.Diagnostics.HasError() || !resp.State.Raw.Equal(state.Raw) || pageCalls != 2 {
				t.Fatalf("invalid complete page set changed state: %v calls=%d", resp.Diagnostics, pageCalls)
			}
			if strings.Contains(fmt.Sprint(resp.Diagnostics), groupTestKey) {
				t.Fatal("secret leaked")
			}
		})
	}
}

func TestGroupMemberMutationValidationRetainsIdentity(t *testing.T) {
	for _, op := range []string{"create", "update", "delete"} {
		for _, failure := range []string{"missing json", "bad envelope", "missing data", "wrong group", "external group", "missing groups", "wrong pair", "wrong permission", "missing members", "missing users", "403", "404", "invalid json"} {
			t.Run(op+"/"+failure, func(t *testing.T) {
				writes := 0
				r := &groupMemberResource{api: memberTestClient(t, func(w http.ResponseWriter, req *http.Request) {
					if memberTestParents(t, w, req) {
						return
					}
					if req.URL.Path == "/api/groups.memberships" {
						members := []client.GroupUser{}
						if op != "create" {
							members = append(members, memberTestMember(userTestID, client.GroupPermissionMember))
						}
						groupTestEncode(t, w, memberTestEnvelope(members, 0, len(members), false))
						return
					}
					writes++
					p := memberTestEnvelope([]client.GroupUser{memberTestMember(userTestID, client.GroupPermissionAdmin)}, 0, 0, true)
					d := p["data"].(map[string]any)
					if op == "delete" {
						delete(d, "users")
						delete(d, "groupMemberships")
					}
					switch failure {
					case "missing json":
						w.Header().Set("Content-Type", "text/plain")
					case "invalid json":
						_, _ = w.Write([]byte("{bad"))
						return
					case "bad envelope":
						p["ok"] = false
					case "missing data":
						delete(p, "data")
					case "missing groups":
						delete(d, "groups")
					case "wrong group":
						g := groupTestGroup()
						g.Id = groupTestPointer(uuid.MustParse(groupTestOtherID))
						d["groups"] = []client.Group{*g}
					case "external group":
						g := groupTestGroup()
						g.ExternalId = nullable.NewNullableWithValue("idp-group")
						d["groups"] = []client.Group{*g}
					case "wrong pair":
						d["groupMemberships"] = []client.GroupUser{memberTestMember(userTestOtherID, client.GroupPermissionAdmin)}
					case "wrong permission":
						d["groupMemberships"] = []client.GroupUser{memberTestMember(userTestID, client.GroupPermissionMember)}
					case "missing members":
						delete(d, "groupMemberships")
					case "missing users":
						delete(d, "users")
					case "403", "404":
						w.WriteHeader(map[string]int{"403": 403, "404": 404}[failure])
						p = map[string]any{"error": "authorization_error", "message": groupTestKey}
					}
					groupTestEncode(t, w, p)
				})}
				state := tfsdk.State(memberTestPlan(t, r, memberTestModel()))
				planModel := memberTestModel()
				planModel.Permission = types.StringValue("admin")
				switch op {
				case "create":
					resp := memberTestCreate(t, r, planModel)
					if !resp.Diagnostics.HasError() || memberTestState(t, resp.State).ID.ValueString() != memberTestModel().ID.ValueString() {
						t.Fatalf("create lost trusted pair: %v", resp.Diagnostics)
					}
				case "update":
					resp := resource.UpdateResponse{State: state}
					r.Update(t.Context(), resource.UpdateRequest{State: state, Plan: memberTestPlan(t, r, planModel)}, &resp)
					if !resp.Diagnostics.HasError() || !resp.State.Raw.Equal(state.Raw) {
						t.Fatalf("update changed pair on failure: %v", resp.Diagnostics)
					}
				case "delete":
					resp := resource.DeleteResponse{State: state}
					r.Delete(t.Context(), resource.DeleteRequest{State: state}, &resp)
					if !resp.Diagnostics.HasError() || !resp.State.Raw.Equal(state.Raw) {
						t.Fatalf("failed/unconfirmed delete removed pair: %v", resp.Diagnostics)
					}
				}
				if writes != 1 {
					t.Fatalf("write retried or not attempted: %d", writes)
				}
			})
		}
	}
}

func TestGroupMemberExternalGroupsRefusedBeforeMutations(t *testing.T) {
	for _, kind := range []string{"external ID", "external group"} {
		for _, op := range []string{"create", "read", "update", "delete"} {
			t.Run(kind+"/"+op, func(t *testing.T) {
				r := &groupMemberResource{api: memberTestClient(t, func(w http.ResponseWriter, req *http.Request) {
					if req.URL.Path == "/api/groups.info" {
						g := groupTestGroup()
						if kind == "external ID" {
							g.ExternalId = nullable.NewNullableWithValue("idp-group")
						} else {
							g.ExternalGroup = nullable.NewNullableWithValue(map[string]interface{}{})
						}
						groupTestEncode(t, w, map[string]any{"ok": true, "data": g})
						return
					}
					if req.URL.Path != "/api/auth.info" || !memberTestParents(t, w, req) {
						t.Errorf("external group accessed or mutated: %s", req.URL.Path)
					}
				})}
				state := tfsdk.State(memberTestPlan(t, r, memberTestModel()))
				switch op {
				case "create":
					resp := memberTestCreate(t, r, memberTestModel())
					groupTestDiagnostics(t, resp.Diagnostics, "externally linked or synchronized")
				case "read":
					resp := resource.ReadResponse{State: state}
					r.Read(t.Context(), resource.ReadRequest{State: state}, &resp)
					groupTestDiagnostics(t, resp.Diagnostics, "externally linked or synchronized")
					if !resp.State.Raw.Equal(state.Raw) {
						t.Fatal("external group refresh removed membership")
					}
				case "update":
					resp := resource.UpdateResponse{State: state}
					r.Update(t.Context(), resource.UpdateRequest{State: state, Plan: memberTestPlan(t, r, memberTestModel())}, &resp)
					groupTestDiagnostics(t, resp.Diagnostics, "externally linked or synchronized")
				case "delete":
					resp := resource.DeleteResponse{State: state}
					r.Delete(t.Context(), resource.DeleteRequest{State: state}, &resp)
					groupTestDiagnostics(t, resp.Diagnostics, "externally linked or synchronized")
				}
			})
		}
	}
}

func TestGroupMemberParentAbsenceAndForbidden(t *testing.T) {
	for _, parent := range []string{"group", "user"} {
		for _, present := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/present=%t", parent, present), func(t *testing.T) {
				listCalls := 0
				r := &groupMemberResource{api: memberTestClient(t, func(w http.ResponseWriter, req *http.Request) {
					if req.URL.Path == "/api/"+map[string]string{"group": "groups.info", "user": "users.info"}[parent] {
						w.WriteHeader(403)
						groupTestEncode(t, w, map[string]any{"error": "authorization_error"})
						return
					}
					if req.URL.Path == "/api/groups.list" {
						listCalls++
						memberTestBody(t, w, req, map[string]any{"limit": float64(100), "offset": float64(0)})
						groups := []client.Group{}
						if present {
							groups = append(groups, *groupTestGroup())
						}
						groupTestEncode(t, w, map[string]any{"ok": true, "data": map[string]any{"groups": groups}, "pagination": client.PaginationResponse{Limit: groupTestPointer(100), Offset: groupTestPointer(0), Total: groupTestPointer(len(groups))}})
						return
					}
					if req.URL.Path == "/api/users.list" {
						listCalls++
						memberTestBody(t, w, req, map[string]any{"limit": float64(100), "offset": float64(0), "filter": "all", "sort": "createdAt", "direction": "ASC"})
						users := []client.User{}
						if present {
							users = append(users, *userTestUser())
						}
						groupTestEncode(t, w, userTestList(users, 0, 100, len(users)))
						return
					}
					if !memberTestParents(t, w, req) {
						t.Errorf("unexpected membership request %s", req.URL.Path)
					}
				})}
				state := tfsdk.State(memberTestPlan(t, r, memberTestModel()))
				read := resource.ReadResponse{State: state}
				r.Read(t.Context(), resource.ReadRequest{State: state}, &read)
				if read.Diagnostics.HasError() != present || read.State.Raw.IsNull() == present || listCalls != 1 {
					t.Fatalf("parent presence=%t: %v lists=%d", present, read.Diagnostics, listCalls)
				}
				deleted := resource.DeleteResponse{State: state}
				r.Delete(t.Context(), resource.DeleteRequest{State: state}, &deleted)
				if deleted.Diagnostics.HasError() != present {
					t.Fatalf("missing parent delete: %v", deleted.Diagnostics)
				}
			})
		}
	}
}

func TestGroupMemberAdminRequired(t *testing.T) {
	for _, kind := range []string{"member", "suspended", "forbidden", "missing", "malformed"} {
		t.Run(kind, func(t *testing.T) {
			r := &groupMemberResource{api: memberTestClient(t, func(w http.ResponseWriter, req *http.Request) {
				if req.URL.Path != "/api/auth.info" {
					t.Errorf("non-admin accessed target: %s", req.URL.Path)
					return
				}
				u := userTestOwner()
				switch kind {
				case "member":
					u.Role = groupTestPointer(client.UserRoleMember)
				case "suspended":
					u.IsSuspended = groupTestPointer(true)
				case "forbidden", "missing":
					w.WriteHeader(map[string]int{"forbidden": 403, "missing": 404}[kind])
					groupTestEncode(t, w, map[string]any{"error": "authorization_error"})
					return
				case "malformed":
					u.Id = nil
				}
				groupTestEncode(t, w, userTestAuth(u))
			})}
			state := tfsdk.State(memberTestPlan(t, r, memberTestModel()))
			created := memberTestCreate(t, r, memberTestModel())
			read := resource.ReadResponse{State: state}
			r.Read(t.Context(), resource.ReadRequest{State: state}, &read)
			updated := resource.UpdateResponse{State: state}
			r.Update(t.Context(), resource.UpdateRequest{State: state, Plan: memberTestPlan(t, r, memberTestModel())}, &updated)
			deleted := resource.DeleteResponse{State: state}
			r.Delete(t.Context(), resource.DeleteRequest{State: state}, &deleted)
			if !created.Diagnostics.HasError() || !read.Diagnostics.HasError() || !updated.Diagnostics.HasError() || !deleted.Diagnostics.HasError() || !read.State.Raw.Equal(state.Raw) {
				t.Fatal("admin check failed to preserve state/block operation")
			}
		})
	}
}
