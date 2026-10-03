// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/glitchedmob/terraform-provider-outline/internal/client"
	"github.com/google/uuid"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/oapi-codegen/nullable"
	openapi_types "github.com/oapi-codegen/runtime/types"
	"golang.org/x/time/rate"
)

const userTestID = "d32c2ee6-fbde-4654-841b-0eabdc71b812"
const userTestOtherID = "e32c2ee6-fbde-4654-841b-0eabdc71b813"
const userTestOwnerID = "f32c2ee6-fbde-4654-841b-0eabdc71b814"
const userTestEmail = "oidc@example.com"

func userTestUser() *client.User {
	return &client.User{
		Id: groupTestPointer(uuid.MustParse(userTestID)), Name: groupTestPointer("OIDC User"),
		Email: nullable.NewNullableWithValue(openapi_types.Email(userTestEmail)),
		Role:  groupTestPointer(client.UserRoleMember), IsSuspended: groupTestPointer(false),
	}
}

func userTestOwner() *client.User {
	user := userTestUser()
	user.Id = groupTestPointer(uuid.MustParse(userTestOwnerID))
	user.Email = nullable.NewNullableWithValue(openapi_types.Email("admin@example.com"))
	user.Role = groupTestPointer(client.UserRoleAdmin)
	return user
}

func userTestModel() userModel {
	return userModel{
		ID: types.StringValue(userTestID), Email: types.StringValue(userTestEmail), Name: types.StringValue("OIDC User"),
		Role: types.StringValue("member"), Suspended: types.BoolValue(false),
		SuppressEmail: types.BoolValue(true), DeletePermanently: types.BoolValue(false),
		AllowTemporaryActivationForRoleChange: types.BoolValue(false),
	}
}

func userTestClient(t *testing.T, handler http.HandlerFunc) *apiClient {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost || (!strings.HasPrefix(req.URL.Path, "/api/users.") && req.URL.Path != "/api/auth.info") || req.URL.RawQuery != "" {
			t.Errorf("unexpected request: %s %s", req.Method, req.URL)
		}
		if req.Header.Get("Authorization") != "Bearer "+groupTestKey || (req.URL.Path != "/api/auth.info" && req.Header.Get("Content-Type") != "application/json") {
			t.Error("missing bearer authentication or JSON content type")
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

func userTestAuth(user *client.User) any {
	return map[string]any{"ok": true, "status": 200, "data": map[string]any{"user": user}}
}

func userTestAdminClient(t *testing.T, handler http.HandlerFunc) *apiClient {
	t.Helper()
	return userTestClient(t, func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/api/auth.info" {
			groupTestEncode(t, w, userTestAuth(userTestOwner()))
			return
		}
		handler(w, req)
	})
}

func userTestEnvelope(user *client.User) any {
	return map[string]any{"ok": true, "status": 200, "data": user}
}

func userTestInviteEnvelope(user *client.User) map[string]any {
	return map[string]any{"ok": true, "status": 200, "data": map[string]any{
		"users": []client.User{*user}, "sent": []client.Invite{{Email: userTestEmail, Name: "OIDC User", Role: client.UserRoleMember}}, "unsent": []client.Invite{},
	}}
}

func userTestList(users []client.User, offset, limit, total int) map[string]any {
	return map[string]any{"ok": true, "status": 200, "data": users, "pagination": client.PaginationResponse{
		Offset: &offset, Limit: &limit, Total: &total,
		NextPath: groupTestPointer("https://do-not-follow.invalid/api/users.list?offset=999"),
	}}
}

func userTestListOffset(t *testing.T, w http.ResponseWriter, req *http.Request) int {
	t.Helper()
	var body map[string]any
	if !groupTestDecode(t, w, req, &body) {
		return -1
	}
	offset, ok := body["offset"].(float64)
	if !ok || offset < 0 || offset != float64(int(offset)) || !reflect.DeepEqual(body, map[string]any{
		"limit": float64(100), "offset": offset, "filter": "all", "sort": "createdAt", "direction": "ASC",
	}) {
		t.Errorf("list must include all users without a query or email filter: %v", body)
		return -1
	}
	return int(offset)
}

func userTestPlan(t *testing.T, r resource.Resource, model userModel) tfsdk.Plan {
	t.Helper()
	var schema resource.SchemaResponse
	r.Schema(t.Context(), resource.SchemaRequest{}, &schema)
	plan := tfsdk.Plan{Schema: schema.Schema}
	if diagnostics := plan.Set(t.Context(), &model); diagnostics.HasError() {
		t.Fatal(diagnostics)
	}
	return plan
}

func userTestConfig(t *testing.T, d datasource.DataSource, model userLookupModel) tfsdk.Config {
	t.Helper()
	var schema datasource.SchemaResponse
	d.Schema(t.Context(), datasource.SchemaRequest{}, &schema)
	state := tfsdk.State{Schema: schema.Schema}
	if diagnostics := state.Set(t.Context(), &model); diagnostics.HasError() {
		t.Fatal(diagnostics)
	}
	return tfsdk.Config{Schema: schema.Schema, Raw: state.Raw}
}

func userTestStateModel(t *testing.T, state tfsdk.State) userModel {
	t.Helper()
	var model userModel
	if diagnostics := state.Get(t.Context(), &model); diagnostics.HasError() {
		t.Fatal(diagnostics)
	}
	return model
}

func userTestCreate(t *testing.T, r *userResource, model userModel) resource.CreateResponse {
	t.Helper()
	model.ID = types.StringUnknown()
	plan := userTestPlan(t, r, model)
	response := resource.CreateResponse{State: tfsdk.State{Schema: plan.Schema}}
	r.Create(t.Context(), resource.CreateRequest{Plan: plan}, &response)
	return response
}

func userTestUpdate(t *testing.T, r *userResource, model userModel, manageName bool) resource.UpdateResponse {
	t.Helper()
	plan := userTestPlan(t, r, model)
	config := model
	if !manageName {
		config.Name = types.StringNull()
	}
	configPlan := userTestPlan(t, r, config)
	response := resource.UpdateResponse{State: tfsdk.State(plan)}
	r.Update(t.Context(), resource.UpdateRequest{Plan: plan, Config: tfsdk.Config(configPlan), State: tfsdk.State(plan)}, &response)
	return response
}

func TestUserCreateLifecycle(t *testing.T) {
	t.Parallel()
	for _, guest := range []bool{false, true} {
		t.Run(fmt.Sprintf("guest=%t", guest), func(t *testing.T) {
			t.Parallel()
			current := userTestUser()
			var calls []string
			model := userTestModel()
			model.Email = types.StringValue("OIDC@EXAMPLE.COM")
			model.Name = types.StringUnknown()
			if guest {
				model.Role, model.Suspended = types.StringValue("guest"), types.BoolValue(true)
			}
			r := &userResource{api: userTestAdminClient(t, func(w http.ResponseWriter, req *http.Request) {
				calls = append(calls, req.URL.Path)
				switch req.URL.Path {
				case "/api/users.list":
					if userTestListOffset(t, w, req) != 0 {
						t.Error("unexpected offset")
					}
					groupTestEncode(t, w, userTestList([]client.User{}, 0, 100, 0))
					return
				case "/api/users.invite":
					var body client.UsersInviteJSONRequestBody
					if !groupTestDecode(t, w, req, &body) {
						return
					}
					if len(body.Invites) != 1 || body.Invites[0].Email != userTestEmail || body.Invites[0].Name != "Pending user" || string(body.Invites[0].Role) != model.Role.ValueString() || body.SuppressEmail == nil || !*body.SuppressEmail {
						t.Errorf("invitation body: %+v", body)
					}
					current.Name = groupTestPointer("Pending user")
					groupTestEncode(t, w, userTestInviteEnvelope(current))
					return
				case "/api/users.info":
					var body client.UsersInfoJSONRequestBody
					if !groupTestDecode(t, w, req, &body) || body.Id.String() != userTestID {
						t.Error("read omitted invitation identity")
					}
				case "/api/users.update_role":
					var body client.UsersUpdateRoleJSONRequestBody
					if !groupTestDecode(t, w, req, &body) || body.Id.String() != userTestID || body.Role != client.UserRoleGuest {
						t.Error("guest role reconciliation body")
					}
					current.Role = groupTestPointer(body.Role)
				case "/api/users.suspend":
					current.IsSuspended = groupTestPointer(true)
				default:
					t.Errorf("unexpected write: %s", req.URL.Path)
				}
				groupTestEncode(t, w, userTestEnvelope(current))
			})}
			response := userTestCreate(t, r, model)
			if response.Diagnostics.HasError() {
				t.Fatal(response.Diagnostics)
			}
			got := userTestStateModel(t, response.State)
			if got.ID.ValueString() != userTestID || got.Email != model.Email || got.Name.ValueString() != "Pending user" || got.Role != model.Role || got.Suspended != model.Suspended || !got.SuppressEmail.ValueBool() || got.DeletePermanently.ValueBool() {
				t.Fatalf("created state: %+v", got)
			}
			want := []string{"/api/users.list", "/api/users.invite", "/api/users.info"}
			if guest {
				want = append(want, "/api/users.update_role", "/api/users.suspend")
			}
			if !reflect.DeepEqual(calls, want) {
				t.Fatalf("unexpected order or replay: %v", calls)
			}
		})
	}
}

func TestUserCreateConfiguredNameAndEmailDelivery(t *testing.T) {
	t.Parallel()
	var invites atomic.Int32
	r := &userResource{api: userTestAdminClient(t, func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/api/users.list":
			userTestListOffset(t, w, req)
			groupTestEncode(t, w, userTestList([]client.User{}, 0, 100, 0))
		case "/api/users.invite":
			invites.Add(1)
			var body client.UsersInviteJSONRequestBody
			if !groupTestDecode(t, w, req, &body) {
				return
			}
			if len(body.Invites) != 1 || body.Invites[0].Name != "OIDC User" || body.Invites[0].Email != userTestEmail || body.SuppressEmail == nil || *body.SuppressEmail {
				t.Errorf("configured name or email delivery omitted: %+v", body)
			}
			groupTestEncode(t, w, userTestInviteEnvelope(userTestUser()))
		case "/api/users.info":
			groupTestEncode(t, w, userTestEnvelope(userTestUser()))
		default:
			t.Errorf("unnecessary reconciliation: %s", req.URL.Path)
		}
	})}
	model := userTestModel()
	model.SuppressEmail = types.BoolValue(false)
	response := userTestCreate(t, r, model)
	if response.Diagnostics.HasError() || invites.Load() != 1 || userTestStateModel(t, response.State).SuppressEmail.ValueBool() {
		t.Fatalf("configured invitation: %v", response.Diagnostics)
	}
}

func TestUserLookupScansAllPages(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		lastEmail string
		wantError string
	}{
		{"suspended exact match", "other@example.com", ""},
		{"duplicate normalized email", " OIDC@EXAMPLE.COM ", "ambiguous"},
		{"substring is not a match", "other@example.com", "not found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			first, match, last := userTestUser(), userTestUser(), userTestUser()
			first.Id = groupTestPointer(uuid.MustParse(userTestOtherID))
			first.Email = nullable.NewNullableWithValue(openapi_types.Email("prefix-" + userTestEmail))
			match.IsSuspended = groupTestPointer(true)
			last.Id = groupTestPointer(uuid.MustParse("10000000-0000-4000-8000-000000000001"))
			last.Email = nullable.NewNullableWithValue(openapi_types.Email(tc.lastEmail))
			query := " OIDC@EXAMPLE.COM "
			if tc.wantError == "not found" {
				query = "example.com"
			}
			var offsets []int
			var info atomic.Int32
			api := userTestAdminClient(t, func(w http.ResponseWriter, req *http.Request) {
				switch req.URL.Path {
				case "/api/users.list":
					offset := userTestListOffset(t, w, req)
					offsets = append(offsets, offset)
					pages := []*client.User{first, match, last}
					if offset < 0 || offset >= len(pages) {
						t.Errorf("unexpected offset %d", offset)
						w.WriteHeader(500)
						return
					}
					groupTestEncode(t, w, userTestList([]client.User{*pages[offset]}, offset, 1, 3))
				case "/api/users.info":
					info.Add(1)
					var body client.UsersInfoJSONRequestBody
					if !groupTestDecode(t, w, req, &body) || body.Id != *match.Id {
						t.Error("lookup read the wrong account")
					}
					groupTestEncode(t, w, userTestEnvelope(match))
				default:
					t.Errorf("unexpected endpoint: %s", req.URL.Path)
				}
			})
			user, err := api.findUser(t.Context(), query)
			if !reflect.DeepEqual(offsets, []int{0, 1, 2}) {
				t.Fatalf("lookup did not check the whole list: %v", offsets)
			}
			if tc.wantError == "" {
				if err != nil || user == nil || !*user.IsSuspended || info.Load() != 1 {
					t.Fatalf("suspended user missing: %v %v", user, err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantError) || user != nil || info.Load() != 0 || errors.Is(err, errNotFound) != (tc.wantError == "not found") {
				t.Fatalf("unsafe lookup result: %v %v reads=%d", user, err, info.Load())
			}
		})
	}
}

func TestUserLookupRejectsConcurrentIdentityChange(t *testing.T) {
	t.Parallel()
	for _, field := range []string{"id", "email"} {
		t.Run(field, func(t *testing.T) {
			t.Parallel()
			api := userTestAdminClient(t, func(w http.ResponseWriter, req *http.Request) {
				user := userTestUser()
				if req.URL.Path == "/api/users.list" {
					userTestListOffset(t, w, req)
					groupTestEncode(t, w, userTestList([]client.User{*user}, 0, 100, 1))
					return
				}
				if req.URL.Path != "/api/users.info" {
					t.Errorf("unexpected endpoint: %s", req.URL.Path)
				}
				if field == "id" {
					user.Id = groupTestPointer(uuid.MustParse(userTestOtherID))
				} else {
					user.Email = nullable.NewNullableWithValue(openapi_types.Email("changed@example.com"))
				}
				groupTestEncode(t, w, userTestEnvelope(user))
			})
			_, err := api.findUser(t.Context(), userTestEmail)
			want := "different email"
			if field == "id" {
				want = "different ID"
			}
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("concurrent identity change accepted: %v", err)
			}
		})
	}
}

func TestUserCreateRefusesExistingAccount(t *testing.T) {
	t.Parallel()
	for _, suspended := range []bool{false, true} {
		t.Run(fmt.Sprint(suspended), func(t *testing.T) {
			t.Parallel()
			var calls []string
			user := userTestUser()
			user.IsSuspended = &suspended
			r := &userResource{api: userTestAdminClient(t, func(w http.ResponseWriter, req *http.Request) {
				calls = append(calls, req.URL.Path)
				switch req.URL.Path {
				case "/api/users.list":
					userTestListOffset(t, w, req)
					groupTestEncode(t, w, userTestList([]client.User{*user}, 0, 100, 1))
				case "/api/users.info":
					groupTestEncode(t, w, userTestEnvelope(user))
				default:
					t.Errorf("Create wrote to an existing account: %s", req.URL.Path)
				}
			})}
			response := userTestCreate(t, r, userTestModel())
			groupTestDiagnostics(t, response.Diagnostics, "never adopts")
			if response.State.Raw.IsKnown() && !response.State.Raw.IsNull() || !reflect.DeepEqual(calls, []string{"/api/users.list"}) {
				t.Fatalf("existing account adopted: state=%v calls=%v", response.State.Raw, calls)
			}
		})
	}
}

func TestUserCreateInviteValidationAndPartialState(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		change   func(map[string]any, *client.User)
		followup string
		retain   bool
	}{
		{"unsent race", func(body map[string]any, user *client.User) {
			body["data"].(map[string]any)["unsent"] = []client.Invite{{Email: userTestEmail}}
		}, "", false},
		{"missing users", func(body map[string]any, user *client.User) { delete(body["data"].(map[string]any), "users") }, "", false},
		{"multiple users", func(body map[string]any, user *client.User) {
			body["data"].(map[string]any)["users"] = []client.User{*user, *user}
		}, "", false},
		{"wrong email", func(body map[string]any, user *client.User) {
			user.Email = nullable.NewNullableWithValue(openapi_types.Email("other@example.com"))
		}, "", false},
		{"owner ID", func(body map[string]any, user *client.User) { user.Id = userTestOwner().Id }, "", false},
		{"missing ID", func(body map[string]any, user *client.User) { user.Id = nil }, "", false},
		{"zero ID", func(body map[string]any, user *client.User) { user.Id = groupTestPointer(uuid.Nil) }, "", false},
		{"missing name", func(body map[string]any, user *client.User) { user.Name = nil }, "", true},
		{"invalid role", func(body map[string]any, user *client.User) {
			user.Role = groupTestPointer(client.UserRole("superadmin"))
		}, "", true},
		{"missing suspended", func(body map[string]any, user *client.User) { user.IsSuspended = nil }, "", true},
		{"unsuccessful envelope", func(body map[string]any, user *client.User) { body["ok"] = false }, "", true},
		{"missing sent", func(body map[string]any, user *client.User) { delete(body["data"].(map[string]any), "sent") }, "", true},
		{"wrong sent email", func(body map[string]any, user *client.User) {
			body["data"].(map[string]any)["sent"] = []client.Invite{{Email: "other@example.com"}}
		}, "", true},
		{"missing unsent", func(body map[string]any, user *client.User) { delete(body["data"].(map[string]any), "unsent") }, "", true},
		{"read forbidden", nil, "read forbidden", true},
		{"read malformed", nil, "read malformed", true},
		{"role reconciliation failed", nil, "role", true},
		{"suspension failed", nil, "suspend", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var calls []string
			r := &userResource{api: userTestAdminClient(t, func(w http.ResponseWriter, req *http.Request) {
				calls = append(calls, req.URL.Path)
				switch req.URL.Path {
				case "/api/users.list":
					userTestListOffset(t, w, req)
					groupTestEncode(t, w, userTestList([]client.User{}, 0, 100, 0))
				case "/api/users.invite":
					user := userTestUser()
					body := userTestInviteEnvelope(user)
					if tc.change != nil {
						tc.change(body, user)
						// Rebuild only an unchanged users array so user field edits reach the wire.
						if users, ok := body["data"].(map[string]any)["users"].([]client.User); ok && len(users) == 1 {
							body["data"].(map[string]any)["users"] = []client.User{*user}
						}
					}
					groupTestEncode(t, w, body)
				case "/api/users.info":
					if tc.followup == "read forbidden" {
						w.WriteHeader(403)
						groupTestWrite(t, w, `{"error":"permission_denied"}`)
					} else if tc.followup == "read malformed" {
						groupTestWrite(t, w, `{"ok":true,"data":{}}`)
					} else if tc.followup != "" {
						groupTestEncode(t, w, userTestEnvelope(userTestUser()))
					} else {
						t.Error("invalid invite triggered reconciliation")
					}
				case "/api/users.update_role", "/api/users.suspend":
					w.WriteHeader(500)
					groupTestWrite(t, w, `{"error":"api_error"}`)
				default:
					t.Errorf("unexpected endpoint: %s", req.URL.Path)
				}
			})}
			model := userTestModel()
			switch tc.followup {
			case "role":
				model.Role = types.StringValue("guest")
			case "suspend":
				model.Suspended = types.BoolValue(true)
			}
			response := userTestCreate(t, r, model)
			groupTestDiagnostics(t, response.Diagnostics, "")
			if tc.retain {
				got := userTestStateModel(t, response.State)
				if got.ID.ValueString() != userTestID || got.Email != model.Email {
					t.Fatalf("partial Create lost identity: %+v", got)
				}
				groupTestDiagnostics(t, response.Diagnostics, "retained in state")
			} else if response.State.Raw.IsKnown() && !response.State.Raw.IsNull() {
				t.Fatalf("unsafe invitation adopted: %v", response.State.Raw)
			}
			want := []string{"/api/users.list", "/api/users.invite"}
			if tc.followup != "" {
				want = append(want, "/api/users.info")
				switch tc.followup {
				case "role":
					want = append(want, "/api/users.update_role")
				case "suspend":
					want = append(want, "/api/users.suspend", "/api/users.suspend")
				}
			}
			if !reflect.DeepEqual(calls, want) {
				t.Fatalf("invalid invitation caused writes or replay: %v", calls)
			}
		})
	}
}

func TestUserRoleOrderingAndFailureRestoration(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, fail     string
		finalSuspended bool
		restoreFails   bool
		want           []string
		wantRole       string
		wantSuspended  bool
	}{
		{"role and name then resuspend", "", true, false, []string{"activate", "update_role", "update", "suspend"}, "guest", true},
		{"role then stay active", "", false, false, []string{"activate", "update_role", "update"}, "guest", false},
		{"activation fails", "activate", true, false, []string{"activate", "suspend"}, "member", true},
		{"role fails restores suspension", "update_role", true, false, []string{"activate", "update_role", "suspend"}, "member", true},
		{"name fails restores suspension", "update", true, false, []string{"activate", "update_role", "update", "suspend"}, "guest", true},
		{"role failure and restoration failure", "update_role", true, true, []string{"activate", "update_role", "suspend"}, "member", false},
		{"final suspension fails restoration succeeds", "suspend", true, false, []string{"activate", "update_role", "update", "suspend", "suspend"}, "guest", true},
		{"final suspension and restoration fail", "suspend", true, true, []string{"activate", "update_role", "update", "suspend", "suspend"}, "guest", false},
		{"role fails with active desired", "update_role", false, false, []string{"activate", "update_role"}, "member", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			current := userTestUser()
			current.IsSuspended = groupTestPointer(true)
			var calls []string
			var suspends int
			r := &userResource{api: userTestAdminClient(t, func(w http.ResponseWriter, req *http.Request) {
				if req.URL.Path == "/api/users.info" {
					groupTestEncode(t, w, userTestEnvelope(current))
					return
				}
				op := strings.TrimPrefix(req.URL.Path, "/api/users.")
				calls = append(calls, op)
				var body map[string]any
				if !groupTestDecode(t, w, req, &body) {
					return
				}
				want := map[string]any{"id": userTestID}
				if op == "update_role" {
					want["role"] = "guest"
				}
				if op == "update" {
					want["name"] = "Updated"
				}
				if !reflect.DeepEqual(body, want) {
					t.Errorf("write omitted explicit identity or included other fields: %s %v", op, body)
				}
				if op == "suspend" {
					suspends++
				}
				if op == tc.fail && (op != "suspend" || suspends == 1) || op == "suspend" && tc.restoreFails {
					w.WriteHeader(500)
					groupTestWrite(t, w, `{"error":"api_error"}`)
					return
				}
				switch op {
				case "activate":
					current.IsSuspended = groupTestPointer(false)
				case "update_role":
					current.Role = groupTestPointer(client.UserRoleGuest)
				case "update":
					current.Name = groupTestPointer("Updated")
				case "suspend":
					current.IsSuspended = groupTestPointer(true)
				default:
					t.Errorf("unexpected mutation %s", op)
				}
				groupTestEncode(t, w, userTestEnvelope(current))
			})}
			model := userTestModel()
			model.Role, model.Name, model.Suspended = types.StringValue("guest"), types.StringValue("Updated"), types.BoolValue(tc.finalSuspended)
			model.AllowTemporaryActivationForRoleChange = types.BoolValue(tc.finalSuspended)
			response := userTestUpdate(t, r, model, true)
			if response.Diagnostics.HasError() != (tc.fail != "") {
				t.Fatalf("update diagnostics: %v", response.Diagnostics)
			}
			if tc.restoreFails {
				groupTestDiagnostics(t, response.Diagnostics, "restoring suspension also failed")
			}
			got := userTestStateModel(t, response.State)
			if got.ID.ValueString() != userTestID || got.Role.ValueString() != tc.wantRole || got.Suspended.ValueBool() != tc.wantSuspended || !reflect.DeepEqual(calls, tc.want) {
				t.Fatalf("partial state or ordering: %+v calls=%v, want=%v", got, calls, tc.want)
			}
		})
	}
}

func TestUserUpdateOmittedNameAndInvitationFlagsDoNotWrite(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	r := &userResource{api: userTestAdminClient(t, func(w http.ResponseWriter, req *http.Request) {
		calls.Add(1)
		if req.URL.Path != "/api/users.info" {
			t.Errorf("omitted name or changed invite flags caused a write: %s", req.URL.Path)
		}
		groupTestEncode(t, w, userTestEnvelope(userTestUser()))
	})}
	model := userTestModel()
	model.Name, model.SuppressEmail, model.DeletePermanently = types.StringValue("Previously managed"), types.BoolValue(false), types.BoolValue(true)
	response := userTestUpdate(t, r, model, false)
	if response.Diagnostics.HasError() || calls.Load() != 1 || userTestStateModel(t, response.State).Name.ValueString() != "OIDC User" {
		t.Fatalf("omitted name update: %v calls=%d", response.Diagnostics, calls.Load())
	}
}

func TestUserDestroyPolicy(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                 string
		suspended, permanent bool
		want                 string
	}{
		{"default suspends", false, false, "suspend"}, {"already suspended noop", true, false, ""},
		{"opt in deletes active", false, true, "delete"}, {"opt in deletes suspended", true, true, "delete"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var writes []string
			user := userTestUser()
			user.IsSuspended = groupTestPointer(tc.suspended)
			r := &userResource{api: userTestAdminClient(t, func(w http.ResponseWriter, req *http.Request) {
				if req.URL.Path == "/api/users.info" {
					groupTestEncode(t, w, userTestEnvelope(user))
					return
				}
				op := strings.TrimPrefix(req.URL.Path, "/api/users.")
				writes = append(writes, op)
				var body map[string]any
				if !groupTestDecode(t, w, req, &body) || !reflect.DeepEqual(body, map[string]any{"id": userTestID}) {
					t.Error("destroy omitted user ID")
				}
				if op == "delete" {
					groupTestWrite(t, w, `{"ok":true,"status":200,"success":true}`)
				} else {
					user.IsSuspended = groupTestPointer(true)
					groupTestEncode(t, w, userTestEnvelope(user))
				}
			})}
			model := userTestModel()
			model.DeletePermanently = types.BoolValue(tc.permanent)
			state := tfsdk.State(userTestPlan(t, r, model))
			response := resource.DeleteResponse{State: state}
			r.Delete(t.Context(), resource.DeleteRequest{State: state}, &response)
			if response.Diagnostics.HasError() || tc.want == "" && len(writes) != 0 || tc.want != "" && !reflect.DeepEqual(writes, []string{tc.want}) {
				t.Fatalf("destroy policy: %v writes=%v", response.Diagnostics, writes)
			}
		})
	}
}

func TestUserAdminAndOwnerProtection(t *testing.T) {
	t.Parallel()
	for _, operation := range []string{"create", "update", "delete"} {
		for _, tc := range []struct {
			name                string
			actor               *client.User
			ownerID, ownerEmail bool
		}{
			{"member key", func() *client.User { u := userTestOwner(); u.Role = groupTestPointer(client.UserRoleMember); return u }(), false, false},
			{"suspended admin", func() *client.User { u := userTestOwner(); u.IsSuspended = groupTestPointer(true); return u }(), false, false},
			{"malformed actor", &client.User{Role: groupTestPointer(client.UserRoleAdmin)}, false, false},
			{"owner email normalized", userTestOwner(), false, true},
			{"owner ID", userTestOwner(), true, false},
		} {
			if operation == "create" && tc.ownerID {
				continue
			}
			t.Run(operation+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				var requests atomic.Int32
				r := &userResource{api: userTestClient(t, func(w http.ResponseWriter, req *http.Request) {
					requests.Add(1)
					if req.URL.Path != "/api/auth.info" {
						t.Errorf("protected account request: %s", req.URL.Path)
					}
					groupTestEncode(t, w, userTestAuth(tc.actor))
				})}
				model := userTestModel()
				if tc.ownerEmail {
					model.Email = types.StringValue(" ADMIN@EXAMPLE.COM ")
				}
				if tc.ownerID {
					model.ID = types.StringValue(userTestOwnerID)
				}
				model.DeletePermanently = types.BoolValue(true)
				state := tfsdk.State(userTestPlan(t, r, model))
				var diagnostics diag.Diagnostics
				switch operation {
				case "create":
					diagnostics = userTestCreate(t, r, model).Diagnostics
				case "update":
					diagnostics = userTestUpdate(t, r, model, true).Diagnostics
				case "delete":
					response := resource.DeleteResponse{State: state}
					r.Delete(t.Context(), resource.DeleteRequest{State: state}, &response)
					diagnostics = response.Diagnostics
				}
				groupTestDiagnostics(t, diagnostics, "")
				if requests.Load() != 1 {
					t.Fatalf("protection made %d requests", requests.Load())
				}
			})
		}
	}
}

type userTestListReply struct {
	body   any
	status int
}

type userTestListFailure struct {
	name  string
	pages []userTestListReply
	want  string
}

func userTestListFailures() []userTestListFailure {
	user := *userTestUser()
	other := user
	other.Id = groupTestPointer(uuid.MustParse(userTestOtherID))
	other.Email = nullable.NewNullableWithValue(openapi_types.Email("other@example.com"))
	first := userTestListReply{body: userTestList([]client.User{user}, 0, 1, 2)}
	cases := []userTestListFailure{
		{"missing data", []userTestListReply{{body: `{"ok":true,"pagination":{"offset":0,"limit":100,"total":0}}`}}, "missing JSON users"},
		{"null data", []userTestListReply{{body: `{"ok":true,"data":null}`}}, "missing JSON users"},
		{"malformed JSON", []userTestListReply{{body: `{`}}, "could not be decoded"},
		{"false ok", []userTestListReply{{body: `{"ok":false,"data":[]}`}}, "response envelope"},
		{"bad envelope status", []userTestListReply{{body: `{"ok":true,"status":403,"data":[]}`}}, "response envelope"},
		{"malformed user", []userTestListReply{{body: userTestList([]client.User{{}}, 0, 100, 1)}}, "malformed user"},
		{"duplicate on same page", []userTestListReply{{body: userTestList([]client.User{user, user}, 0, 100, 2)}}, "duplicate user"},
		{"duplicate across pages", []userTestListReply{first, {body: userTestList([]client.User{user}, 1, 1, 2)}}, "duplicate user"},
		{"total changed on final page", []userTestListReply{first, {body: userTestList([]client.User{other}, 1, 1, 3)}}, "total changed"},
		{"total changed to completed", []userTestListReply{first, {body: userTestList([]client.User{}, 1, 1, 1)}}, "total changed"},
		{"late malformed user", []userTestListReply{first, {body: userTestList([]client.User{{}}, 1, 1, 2)}}, "malformed user"},
		{"late denied page", []userTestListReply{first, {status: 403, body: `{"error":"authorization_error"}`}}, "HTTP 403"},
		{"late missing page", []userTestListReply{first, {status: 404, body: `{}`}}, ""},
	}
	for _, status := range []int{400, 401, 403, 404, 429, 500} {
		cases = append(cases, userTestListFailure{fmt.Sprintf("list HTTP %d", status), []userTestListReply{{status: status, body: `{"error":"api_error"}`}}, ""})
	}
	for _, pagination := range []any{
		nil, map[string]int{"limit": 100, "total": 0}, map[string]int{"offset": 0, "total": 0}, map[string]int{"offset": 0, "limit": 100},
		map[string]int{"offset": 1, "limit": 100, "total": 0}, map[string]int{"offset": 0, "limit": 0, "total": 0},
		map[string]int{"offset": 0, "limit": 101, "total": 0}, map[string]int{"offset": 0, "limit": 100, "total": -1},
	} {
		cases = append(cases, userTestListFailure{fmt.Sprintf("pagination=%v", pagination), []userTestListReply{{body: map[string]any{"ok": true, "data": []client.User{}, "pagination": pagination}}}, "pagination"})
	}
	for _, tc := range []struct {
		name         string
		users        []client.User
		limit, total int
	}{
		{"empty incomplete page", []client.User{}, 100, 1}, {"short incomplete page", []client.User{user}, 100, 2},
		{"count exceeds total", []client.User{user}, 100, 0}, {"count exceeds limit", []client.User{user, other}, 1, 2},
	} {
		cases = append(cases, userTestListFailure{tc.name, []userTestListReply{{body: userTestList(tc.users, 0, tc.limit, tc.total)}}, "pagination"})
	}
	return cases
}

func userTestWriteReply(t *testing.T, w http.ResponseWriter, reply userTestListReply) {
	t.Helper()
	if reply.status != 0 {
		w.WriteHeader(reply.status)
	}
	if body, ok := reply.body.(string); ok {
		groupTestWrite(t, w, body)
	} else {
		groupTestEncode(t, w, reply.body)
	}
}

func TestUserListErrorsBlockLookupAndCreate(t *testing.T) {
	t.Parallel()
	for _, tc := range userTestListFailures() {
		for _, operation := range []string{"find", "create"} {
			t.Run(operation+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				var calls int
				api := userTestAdminClient(t, func(w http.ResponseWriter, req *http.Request) {
					if req.URL.Path != "/api/users.list" || calls >= len(tc.pages) {
						t.Errorf("incomplete list caused a read, invite, or extra request: %s", req.URL.Path)
						w.WriteHeader(500)
						return
					}
					if offset := userTestListOffset(t, w, req); offset != calls {
						t.Errorf("offset=%d want=%d", offset, calls)
					}
					userTestWriteReply(t, w, tc.pages[calls])
					calls++
				})
				if operation == "find" {
					user, err := api.findUser(t.Context(), userTestEmail)
					if err == nil || user != nil || !strings.Contains(err.Error(), tc.want) || errors.Is(err, errNotFound) {
						t.Fatalf("partial list accepted or reported as verified absence: %v %v", user, err)
					}
				} else {
					response := userTestCreate(t, &userResource{api: api}, userTestModel())
					groupTestDiagnostics(t, response.Diagnostics, tc.want)
					if response.State.Raw.IsKnown() && !response.State.Raw.IsNull() {
						t.Fatal("failed preflight adopted a user")
					}
				}
				if calls != len(tc.pages) {
					t.Fatalf("list stopped at %d pages, want %d", calls, len(tc.pages))
				}
			})
		}
	}
}

func TestUserAuthorizationErrorAbsence(t *testing.T) {
	t.Parallel()
	user, other := *userTestUser(), *userTestUser()
	user.IsSuspended = groupTestPointer(true)
	other.Id = groupTestPointer(uuid.MustParse(userTestOtherID))
	other.IsSuspended = groupTestPointer(true)
	cases := []struct {
		name   string
		pages  []userTestListReply
		absent bool
		want   string
	}{
		{"empty admin list", []userTestListReply{{body: userTestList([]client.User{}, 0, 100, 0)}}, true, ""},
		{"absent after all pages", []userTestListReply{{body: userTestList([]client.User{other}, 0, 1, 2)}, {body: userTestList([]client.User{*userTestOwner()}, 1, 1, 2)}}, true, ""},
		{"present first page", []userTestListReply{{body: userTestList([]client.User{user}, 0, 1, 2)}, {body: userTestList([]client.User{other}, 1, 1, 2)}}, false, "HTTP 403"},
		{"present suspended later page", []userTestListReply{{body: userTestList([]client.User{other}, 0, 1, 2)}, {body: userTestList([]client.User{user}, 1, 1, 2)}}, false, "HTTP 403"},
	}
	for _, tc := range userTestListFailures() {
		cases = append(cases, struct {
			name   string
			pages  []userTestListReply
			absent bool
			want   string
		}{tc.name, tc.pages, false, "cannot establish user absence"})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var calls []string
			var pages int
			api := userTestClient(t, func(w http.ResponseWriter, req *http.Request) {
				calls = append(calls, req.URL.Path)
				switch req.URL.Path {
				case "/api/users.info":
					w.WriteHeader(403)
					groupTestWrite(t, w, `{"error":"authorization_error","message":"denied `+groupTestKey+`"}`)
				case "/api/auth.info":
					groupTestEncode(t, w, userTestAuth(userTestOwner()))
				case "/api/users.list":
					if pages >= len(tc.pages) {
						t.Error("extra fallback page")
						w.WriteHeader(500)
						return
					}
					if userTestListOffset(t, w, req) != pages {
						t.Error("wrong fallback offset")
					}
					userTestWriteReply(t, w, tc.pages[pages])
					pages++
				default:
					t.Errorf("absence verification attempted a write: %s", req.URL.Path)
				}
			})
			got, err := api.readUser(t.Context(), uuid.MustParse(userTestID))
			if got != nil || err == nil || errors.Is(err, errNotFound) != tc.absent || strings.Contains(err.Error(), groupTestKey) {
				t.Fatalf("unsafe absence result: %v %v", got, err)
			}
			if tc.want != "" && !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("wrong absence error: %v", err)
			}
			if pages != len(tc.pages) || len(calls) != len(tc.pages)+2 || calls[0] != "/api/users.info" || calls[1] != "/api/auth.info" {
				t.Fatalf("absence did not check full admin list: %v", calls)
			}
		})
	}
}

func TestUserAuthorizationFallbackRequiresTyped403AndActiveAdmin(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name              string
		status            int
		body, contentType string
	}{
		{"different error", 403, `{"error":"permission_denied"}`, ""}, {"missing error", 403, `{}`, ""},
		{"malformed JSON", 403, `{`, ""}, {"wrong error type", 403, `{"error":42}`, ""},
		{"wrong content type", 403, `{"error":"authorization_error"}`, "text/plain"},
		{"unauthorized", 401, `{"error":"authorization_error"}`, ""}, {"server error", 500, `{"error":"authorization_error"}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var calls atomic.Int32
			api := userTestClient(t, func(w http.ResponseWriter, req *http.Request) {
				calls.Add(1)
				if req.URL.Path != "/api/users.info" {
					t.Errorf("invalid 403 attempted fallback: %s", req.URL.Path)
				}
				if tc.contentType != "" {
					w.Header().Set("Content-Type", tc.contentType)
				}
				w.WriteHeader(tc.status)
				groupTestWrite(t, w, tc.body)
			})
			_, err := api.readUser(t.Context(), uuid.MustParse(userTestID))
			if err == nil || errors.Is(err, errNotFound) || calls.Load() != 1 {
				t.Fatalf("invalid 403 treated as absence: %v calls=%d", err, calls.Load())
			}
		})
	}
	for _, actor := range []*client.User{func() *client.User { u := userTestOwner(); u.Role = groupTestPointer(client.UserRoleMember); return u }(), func() *client.User { u := userTestOwner(); u.IsSuspended = groupTestPointer(true); return u }(), {Role: groupTestPointer(client.UserRoleAdmin)}} {
		t.Run(fmt.Sprintf("actor=%v", actor), func(t *testing.T) {
			t.Parallel()
			var calls atomic.Int32
			api := userTestClient(t, func(w http.ResponseWriter, req *http.Request) {
				calls.Add(1)
				switch req.URL.Path {
				case "/api/users.info":
					w.WriteHeader(403)
					groupTestWrite(t, w, `{"error":"authorization_error"}`)
				case "/api/auth.info":
					groupTestEncode(t, w, userTestAuth(actor))
				default:
					t.Errorf("non-admin listed users: %s", req.URL.Path)
				}
			})
			_, err := api.readUser(t.Context(), uuid.MustParse(userTestID))
			if err == nil || errors.Is(err, errNotFound) || !strings.Contains(err.Error(), "cannot establish user absence") || calls.Load() != 2 {
				t.Fatalf("unverified absence: %v calls=%d", err, calls.Load())
			}
		})
	}
}

func TestUserResourceReadAndDeleteAbsence(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name             string
		status           int
		body             string
		fallback, absent bool
	}{
		{"verified 404 absence", 404, `{"error":"not_found"}`, true, true},
		{"404 existing account", 404, `{"error":"not_found"}`, true, false}, {"verified 403 absence", 403, `{"error":"authorization_error"}`, true, true},
		{"403 existing account", 403, `{"error":"authorization_error"}`, true, false}, {"ordinary forbidden", 403, `{"error":"permission_denied"}`, false, false},
		{"unauthorized", 401, `{}`, false, false}, {"rate limited", 429, `{}`, false, false}, {"server error", 500, `{}`, false, false}, {"malformed success", 200, `{"ok":true,"data":{}}`, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var writes atomic.Int32
			r := &userResource{api: userTestAdminClient(t, func(w http.ResponseWriter, req *http.Request) {
				switch req.URL.Path {
				case "/api/users.info":
					w.WriteHeader(tc.status)
					groupTestWrite(t, w, tc.body)
				case "/api/users.list":
					if !tc.fallback {
						t.Error("unexpected fallback")
					}
					userTestListOffset(t, w, req)
					users := []client.User{}
					if !tc.absent {
						users = append(users, *userTestUser())
					}
					groupTestEncode(t, w, userTestList(users, 0, 100, len(users)))
				default:
					writes.Add(1)
					t.Errorf("failed preflight wrote: %s", req.URL.Path)
				}
			})}
			state := tfsdk.State(userTestPlan(t, r, userTestModel()))
			read := resource.ReadResponse{State: state}
			r.Read(t.Context(), resource.ReadRequest{State: state}, &read)
			if read.Diagnostics.HasError() == tc.absent || read.State.Raw.IsNull() != tc.absent || !tc.absent && !read.State.Raw.Equal(state.Raw) {
				t.Fatalf("inaccessible read removed state: %v state=%v", read.Diagnostics, read.State.Raw)
			}
			deleted := resource.DeleteResponse{State: state}
			r.Delete(t.Context(), resource.DeleteRequest{State: state}, &deleted)
			if deleted.Diagnostics.HasError() == tc.absent || writes.Load() != 0 {
				t.Fatalf("absence destroy: %v writes=%d", deleted.Diagnostics, writes.Load())
			}
			d := &userDataSource{api: r.api}
			config := userTestConfig(t, d, userLookupModel{ID: types.StringValue(userTestID)})
			lookup := datasource.ReadResponse{State: tfsdk.State{Schema: config.Schema}}
			d.Read(t.Context(), datasource.ReadRequest{Config: config}, &lookup)
			groupTestDiagnostics(t, lookup.Diagnostics, "")
		})
	}
}

func TestUserResourceRefreshAndDataSource(t *testing.T) {
	t.Parallel()
	for _, byEmail := range []bool{false, true} {
		t.Run(fmt.Sprint(byEmail), func(t *testing.T) {
			t.Parallel()
			user := userTestUser()
			user.Name = groupTestPointer("IdP name")
			user.Role = groupTestPointer(client.UserRoleViewer)
			user.IsSuspended = groupTestPointer(true)
			api := userTestAdminClient(t, func(w http.ResponseWriter, req *http.Request) {
				switch req.URL.Path {
				case "/api/users.list":
					userTestListOffset(t, w, req)
					groupTestEncode(t, w, userTestList([]client.User{*user}, 0, 100, 1))
				case "/api/users.info":
					groupTestEncode(t, w, userTestEnvelope(user))
				default:
					t.Errorf("lookup or read wrote: %s", req.URL.Path)
				}
			})
			r := &userResource{api: api}
			model := userTestModel()
			model.Email = types.StringValue("OIDC@EXAMPLE.COM")
			state := tfsdk.State(userTestPlan(t, r, model))
			read := resource.ReadResponse{State: state}
			r.Read(t.Context(), resource.ReadRequest{State: state}, &read)
			if read.Diagnostics.HasError() {
				t.Fatal(read.Diagnostics)
			}
			got := userTestStateModel(t, read.State)
			if got.Email != model.Email || got.Name.ValueString() != *user.Name || got.Role.ValueString() != string(*user.Role) || !got.Suspended.ValueBool() || got.SuppressEmail != model.SuppressEmail || got.DeletePermanently != model.DeletePermanently {
				t.Fatalf("refresh lost fields: %+v", got)
			}
			d := &userDataSource{api: api}
			lookupModel := userLookupModel{ID: types.StringValue(userTestID)}
			if byEmail {
				lookupModel.ID = types.StringNull()
				lookupModel.Email = types.StringValue("OIDC@EXAMPLE.COM")
			}
			config := userTestConfig(t, d, lookupModel)
			response := datasource.ReadResponse{State: tfsdk.State{Schema: config.Schema}}
			d.Read(t.Context(), datasource.ReadRequest{Config: config}, &response)
			if response.Diagnostics.HasError() {
				t.Fatal(response.Diagnostics)
			}
			var lookup userLookupModel
			if diags := response.State.Get(t.Context(), &lookup); diags.HasError() {
				t.Fatal(diags)
			}
			if lookup.ID.ValueString() != userTestID || normalizeUserEmail(lookup.Email.ValueString()) != userTestEmail || lookup.Name != got.Name || lookup.Role != got.Role || lookup.Suspended != got.Suspended {
				t.Fatalf("lookup fields: %+v", lookup)
			}
			if byEmail && lookup.Email != lookupModel.Email {
				t.Fatal("lookup changed equivalent configured casing")
			}
		})
	}
}

func TestUserDataSourceEmailLookupFailures(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		emails []string
		want   string
	}{
		{"absent", []string{}, "exact normalized email"},
		{"substring only", []string{"prefix-" + userTestEmail}, "exact normalized email"},
		{"ambiguous", []string{userTestEmail, "OIDC@EXAMPLE.COM"}, "ambiguous"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var lists atomic.Int32
			d := &userDataSource{api: userTestAdminClient(t, func(w http.ResponseWriter, req *http.Request) {
				if req.URL.Path != "/api/users.list" {
					t.Errorf("unsuccessful email lookup read or wrote a user: %s", req.URL.Path)
				}
				lists.Add(1)
				userTestListOffset(t, w, req)
				users := []client.User{}
				for i, email := range tc.emails {
					user := *userTestUser()
					user.Id = groupTestPointer(uuid.MustParse(fmt.Sprintf("10000000-0000-4000-8000-%012d", i+1)))
					user.Email = nullable.NewNullableWithValue(openapi_types.Email(email))
					user.IsSuspended = groupTestPointer(true)
					users = append(users, user)
				}
				groupTestEncode(t, w, userTestList(users, 0, 100, len(users)))
			})}
			config := userTestConfig(t, d, userLookupModel{Email: types.StringValue(userTestEmail)})
			response := datasource.ReadResponse{State: tfsdk.State{Schema: config.Schema}}
			d.Read(t.Context(), datasource.ReadRequest{Config: config}, &response)
			groupTestDiagnostics(t, response.Diagnostics, tc.want)
			if lists.Load() != 1 || response.State.Raw.IsKnown() && !response.State.Raw.IsNull() {
				t.Fatalf("failed data lookup adopted state: %v", response.State.Raw)
			}
		})
	}
}

func TestUserManagedIdentityDriftBlocksWrites(t *testing.T) {
	t.Parallel()
	for _, operation := range []string{"update", "delete"} {
		for _, field := range []string{"id", "email"} {
			t.Run(operation+"/"+field, func(t *testing.T) {
				t.Parallel()
				r := &userResource{api: userTestAdminClient(t, func(w http.ResponseWriter, req *http.Request) {
					if req.URL.Path != "/api/users.info" {
						t.Errorf("identity drift caused write: %s", req.URL.Path)
					}
					user := userTestUser()
					if field == "id" {
						user.Id = groupTestPointer(uuid.MustParse(userTestOtherID))
					} else {
						user.Email = nullable.NewNullableWithValue(openapi_types.Email("changed@example.com"))
					}
					groupTestEncode(t, w, userTestEnvelope(user))
				})}
				model := userTestModel()
				model.DeletePermanently = types.BoolValue(true)
				state := tfsdk.State(userTestPlan(t, r, model))
				if operation == "update" {
					response := userTestUpdate(t, r, model, true)
					groupTestDiagnostics(t, response.Diagnostics, "different")
					if !response.State.Raw.Equal(state.Raw) {
						t.Fatal("identity drift adopted new state")
					}
				} else {
					response := resource.DeleteResponse{State: state}
					r.Delete(t.Context(), resource.DeleteRequest{State: state}, &response)
					groupTestDiagnostics(t, response.Diagnostics, "different")
				}
			})
		}
	}
}

func userTestJSON(t *testing.T, value any) string {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func TestUserMalformedReadAndMutations(t *testing.T) {
	t.Parallel()
	valid := `{"id":"` + userTestID + `","email":"` + userTestEmail + `","name":"OIDC User","role":"member","isSuspended":false}`
	cases := []struct{ name, body, contentType string }{
		{"empty", "", ""}, {"invalid JSON", `{"secret":"` + groupTestKey + `",`, ""}, {"wrong type", `[]`, ""},
		{"null envelope", `null`, ""}, {"missing ok", `{"data":` + valid + `}`, ""}, {"false ok", `{"ok":false,"data":` + valid + `}`, ""},
		{"bad status", `{"ok":true,"status":201,"data":` + valid + `}`, ""}, {"missing data", `{"ok":true}`, ""}, {"null data", `{"ok":true,"data":null}`, ""},
		{"wrong content type", `{"ok":true,"data":` + valid + `}`, "text/plain"},
	}
	for _, field := range []string{"id", "email", "name", "role", "isSuspended"} {
		for _, missing := range []bool{false, true} {
			var user map[string]any
			if err := json.Unmarshal([]byte(valid), &user); err != nil {
				t.Fatal(err)
			}
			user[field] = nil
			if missing {
				delete(user, field)
			}
			cases = append(cases, struct{ name, body, contentType string }{fmt.Sprintf("%s missing=%t", field, missing), userTestJSON(t, map[string]any{"ok": true, "data": user}), ""})
		}
	}
	for _, tc := range []struct {
		field string
		value any
	}{
		{"id", "00000000-0000-0000-0000-000000000000"}, {"id", userTestOtherID}, {"id", "bad UUID"},
		{"email", ""}, {"email", "other@example.com"}, {"role", "superadmin"}, {"isSuspended", "false"},
	} {
		var user map[string]any
		if err := json.Unmarshal([]byte(valid), &user); err != nil {
			t.Fatal(err)
		}
		user[tc.field] = tc.value
		cases = append(cases, struct{ name, body, contentType string }{fmt.Sprintf("%s=%v", tc.field, tc.value), userTestJSON(t, map[string]any{"ok": true, "data": user}), ""})
	}
	for _, operation := range []string{"read", "activate", "suspend", "update_role", "update"} {
		for _, tc := range cases {
			if operation == "read" && tc.name == "email=other@example.com" {
				continue
			} // UUID reads report email drift.
			t.Run(operation+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				var calls atomic.Int32
				api := userTestClient(t, func(w http.ResponseWriter, req *http.Request) {
					calls.Add(1)
					want := operation
					if want == "read" {
						want = "info"
					}
					if req.URL.Path != "/api/users."+want {
						t.Errorf("unexpected endpoint: %s", req.URL.Path)
					}
					if tc.contentType != "" {
						w.Header().Set("Content-Type", tc.contentType)
					}
					groupTestWrite(t, w, tc.body)
				})
				current, plan := userTestUser(), userTestModel()
				var err error
				switch operation {
				case "read":
					_, err = api.readUser(t.Context(), *current.Id)
				case "activate":
					current.IsSuspended = groupTestPointer(true)
					_, err = api.setUserSuspended(t.Context(), current, false)
				case "suspend":
					_, err = api.setUserSuspended(t.Context(), current, true)
				case "update_role":
					plan.Role = types.StringValue("guest")
					_, err = api.updateUser(t.Context(), current, plan, false)
				case "update":
					plan.Name = types.StringValue("Updated")
					_, err = api.updateUser(t.Context(), current, plan, true)
				}
				if err == nil || calls.Load() != 1 || strings.Contains(err.Error(), groupTestKey) {
					t.Fatalf("malformed response accepted or replayed: %v calls=%d", err, calls.Load())
				}
			})
		}
	}
}

func TestUserMutationMustConfirmDesiredValue(t *testing.T) {
	t.Parallel()
	for _, operation := range []string{"activate", "suspend", "update_role", "update"} {
		t.Run(operation, func(t *testing.T) {
			t.Parallel()
			current := userTestUser()
			if operation == "activate" {
				current.IsSuspended = groupTestPointer(true)
			}
			api := userTestClient(t, func(w http.ResponseWriter, req *http.Request) {
				if req.URL.Path != "/api/users."+operation {
					t.Errorf("unexpected endpoint: %s", req.URL.Path)
				}
				groupTestEncode(t, w, userTestEnvelope(current))
			})
			plan := userTestModel()
			var err error
			if operation == "activate" || operation == "suspend" {
				_, err = api.setUserSuspended(t.Context(), current, operation == "suspend")
			} else {
				if operation == "update_role" {
					plan.Role = types.StringValue("guest")
				} else {
					plan.Name = types.StringValue("Updated")
				}
				_, err = api.updateUser(t.Context(), current, plan, true)
			}
			if err == nil || !strings.Contains(err.Error(), "did not confirm desired") {
				t.Fatalf("unconfirmed mutation accepted: %v", err)
			}
		})
	}
}

func TestUserHTTPErrorHandling(t *testing.T) {
	t.Parallel()
	for _, operation := range []string{"info", "invite", "update", "update_role", "suspend", "activate", "delete"} {
		for _, status := range []int{201, 204, 301, 400, 401, 403, 404, 409, 429, 500, 503} {
			t.Run(fmt.Sprintf("%s/%d", operation, status), func(t *testing.T) {
				t.Parallel()
				var writes atomic.Int32
				api := userTestAdminClient(t, func(w http.ResponseWriter, req *http.Request) {
					if operation == "invite" && req.URL.Path == "/api/users.list" {
						userTestListOffset(t, w, req)
						groupTestEncode(t, w, userTestList([]client.User{}, 0, 100, 0))
						return
					}
					if operation == "info" && status == 404 && req.URL.Path == "/api/users.list" {
						userTestListOffset(t, w, req)
						groupTestEncode(t, w, userTestList([]client.User{*userTestUser()}, 0, 100, 1))
						return
					}
					if req.URL.Path != "/api/users."+operation {
						t.Errorf("unexpected endpoint: %s", req.URL.Path)
					}
					writes.Add(1)
					w.Header().Set("Retry-After", "7")
					w.Header().Set("Location", "/api/users."+operation)
					w.WriteHeader(status)
					if status != 204 {
						groupTestWrite(t, w, `{"error":"api_error","message":"Bearer `+groupTestKey+` denied"}`)
					}
				})
				current, plan := userTestUser(), userTestModel()
				var err error
				switch operation {
				case "info":
					_, err = api.readUser(t.Context(), *current.Id)
				case "invite":
					response := userTestCreate(t, &userResource{api: api}, plan)
					groupTestDiagnostics(t, response.Diagnostics, "")
					if response.State.Raw.IsKnown() && !response.State.Raw.IsNull() {
						t.Fatal("failed invite adopted state")
					}
					err = errors.New(response.Diagnostics.Errors()[0].Detail())
				case "activate":
					current.IsSuspended = groupTestPointer(true)
					_, err = api.setUserSuspended(t.Context(), current, false)
				case "suspend":
					_, err = api.setUserSuspended(t.Context(), current, true)
				case "update_role":
					plan.Role = types.StringValue("guest")
					_, err = api.updateUser(t.Context(), current, plan, false)
				case "update":
					plan.Name = types.StringValue("Updated")
					_, err = api.updateUser(t.Context(), current, plan, true)
				case "delete":
					err = api.deleteUser(t.Context(), current)
				}
				if err == nil || writes.Load() != 1 || strings.Contains(err.Error(), groupTestKey) {
					t.Fatalf("HTTP error accepted, replayed, or leaked token: %v calls=%d", err, writes.Load())
				}
				if status == 429 && (!strings.Contains(err.Error(), `Retry-After="7"`) || !strings.Contains(err.Error(), "no automatic retry")) {
					t.Fatalf("missing retry guidance: %v", err)
				}
			})
		}
	}
}

func TestUserMalformedInviteDoesNotAdoptIdentity(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, body, contentType string }{
		{"empty", "", ""}, {"invalid JSON", `{"secret":"` + groupTestKey + `",`, ""},
		{"wrong JSON type", `[]`, ""}, {"null envelope", `null`, ""},
		{"missing data", `{"ok":true}`, ""}, {"null data", `{"ok":true,"data":null}`, ""},
		{"wrong content type", `{"ok":true,"data":{}}`, "text/plain"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var invites atomic.Int32
			r := &userResource{api: userTestAdminClient(t, func(w http.ResponseWriter, req *http.Request) {
				switch req.URL.Path {
				case "/api/users.list":
					userTestListOffset(t, w, req)
					groupTestEncode(t, w, userTestList([]client.User{}, 0, 100, 0))
				case "/api/users.invite":
					invites.Add(1)
					if tc.contentType != "" {
						w.Header().Set("Content-Type", tc.contentType)
					}
					groupTestWrite(t, w, tc.body)
				default:
					t.Errorf("malformed invite caused reconciliation: %s", req.URL.Path)
				}
			})}
			response := userTestCreate(t, r, userTestModel())
			groupTestDiagnostics(t, response.Diagnostics, "")
			if response.State.Raw.IsKnown() && !response.State.Raw.IsNull() || invites.Load() != 1 {
				t.Fatalf("malformed invite adopted state or replayed: %v calls=%d", response.State.Raw, invites.Load())
			}
		})
	}
}

func TestUserAuthResponseValidation(t *testing.T) {
	t.Parallel()
	valid := userTestJSON(t, userTestOwner())
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"empty", "", 200}, {"malformed JSON", `{`, 200}, {"null envelope", `null`, 200}, {"missing data", `{"ok":true}`, 200},
		{"missing user", `{"ok":true,"data":{}}`, 200}, {"null user", `{"ok":true,"data":{"user":null}}`, 200},
		{"missing ok", `{"data":{"user":` + valid + `}}`, 200}, {"false ok", `{"ok":false,"data":{"user":` + valid + `}}`, 200},
		{"wrong envelope status", `{"ok":true,"status":403,"data":{"user":` + valid + `}}`, 200},
		{"role only is not a valid actor", `{"ok":true,"data":{"user":{"role":"admin"}}}`, 200},
		{"unauthorized", `{"error":"authentication_error"}`, 401}, {"forbidden", `{"error":"authorization_error"}`, 403},
		{"rate limited", `{}`, 429}, {"server error", `{}`, 500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var requests atomic.Int32
			api := userTestClient(t, func(w http.ResponseWriter, req *http.Request) {
				requests.Add(1)
				if req.URL.Path != "/api/auth.info" {
					t.Errorf("malformed auth used for request: %s", req.URL.Path)
				}
				w.WriteHeader(tc.status)
				groupTestWrite(t, w, tc.body)
			})
			actor, err := api.requireUserAdmin(t.Context())
			if actor != nil || err == nil || requests.Load() != 1 {
				t.Fatalf("invalid actor accepted: %v %v", actor, err)
			}
		})
	}
	for _, role := range []client.UserRole{client.UserRoleMember, client.UserRoleViewer, client.UserRoleGuest, client.UserRole("superadmin")} {
		t.Run(string(role), func(t *testing.T) {
			t.Parallel()
			actor := userTestOwner()
			actor.Role = &role
			api := userTestClient(t, func(w http.ResponseWriter, req *http.Request) { groupTestEncode(t, w, userTestAuth(actor)) })
			if user, err := api.requireUserAdmin(t.Context()); user != nil || err == nil {
				t.Fatalf("non-admin accepted: %v %v", user, err)
			}
		})
	}
}

func TestUserUpdateRequiresExplicitSuspensionAndValidRole(t *testing.T) {
	t.Parallel()
	api := userTestClient(t, func(http.ResponseWriter, *http.Request) { t.Error("invalid desired state caused a write") })
	for _, suspended := range []types.Bool{types.BoolNull(), types.BoolUnknown()} {
		plan := userTestModel()
		plan.Suspended = suspended
		current := userTestUser()
		user, err := api.updateUser(t.Context(), current, plan, false)
		if user != current || err == nil || !strings.Contains(err.Error(), "explicitly") {
			t.Fatalf("unspecified suspension accepted: %v %v", user, err)
		}
	}
	plan := userTestModel()
	plan.Role = types.StringValue("superadmin")
	current := userTestUser()
	if user, err := api.updateUser(t.Context(), current, plan, false); user != current || err == nil || !strings.Contains(err.Error(), "unsupported user role") {
		t.Fatalf("invalid role accepted: %v %v", user, err)
	}
}

func TestUserTransportFailureDoesNotReplayMutations(t *testing.T) {
	t.Parallel()
	for _, operation := range []string{"invite", "activate", "suspend", "update_role", "update", "delete"} {
		t.Run(operation, func(t *testing.T) {
			t.Parallel()
			var writes atomic.Int32
			api := userTestAdminClient(t, func(w http.ResponseWriter, req *http.Request) {
				if operation == "invite" && req.URL.Path == "/api/users.list" {
					userTestListOffset(t, w, req)
					groupTestEncode(t, w, userTestList([]client.User{}, 0, 100, 0))
					return
				}
				if req.URL.Path != "/api/users."+operation {
					t.Errorf("unexpected endpoint: %s", req.URL.Path)
				}
				writes.Add(1)
				connection, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Errorf("hijack: %v", err)
					return
				}
				if err := connection.Close(); err != nil {
					t.Errorf("close: %v", err)
				}
			})
			current, plan := userTestUser(), userTestModel()
			var err error
			switch operation {
			case "invite":
				response := userTestCreate(t, &userResource{api: api}, plan)
				groupTestDiagnostics(t, response.Diagnostics, "request failed")
				err = errors.New(response.Diagnostics.Errors()[0].Detail())
			case "activate":
				current.IsSuspended = groupTestPointer(true)
				_, err = api.setUserSuspended(t.Context(), current, false)
			case "suspend":
				_, err = api.setUserSuspended(t.Context(), current, true)
			case "update_role":
				plan.Role = types.StringValue("guest")
				_, err = api.updateUser(t.Context(), current, plan, false)
			case "update":
				plan.Name = types.StringValue("Updated")
				_, err = api.updateUser(t.Context(), current, plan, true)
			case "delete":
				err = api.deleteUser(t.Context(), current)
			}
			if err == nil || !strings.Contains(err.Error(), "request failed") || writes.Load() != 1 {
				t.Fatalf("ambiguous mutation replayed: %v writes=%d", err, writes.Load())
			}
		})
	}
}

func TestUserWalkVisitorFailure(t *testing.T) {
	t.Parallel()
	var pages atomic.Int32
	api := userTestAdminClient(t, func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/api/users.list" {
			t.Errorf("unexpected endpoint: %s", req.URL.Path)
		}
		pages.Add(1)
		userTestListOffset(t, w, req)
		groupTestEncode(t, w, userTestList([]client.User{*userTestUser()}, 0, 1, 2))
	})
	want := errors.New("visitor failed")
	if err := api.walkUsers(t.Context(), func(*client.User) error { return want }); !errors.Is(err, want) || pages.Load() != 1 {
		t.Fatalf("visitor failure swallowed: %v pages=%d", err, pages.Load())
	}
}

func TestUserCreateOmittedNameAcceptsURLLikeEmail(t *testing.T) {
	t.Parallel()
	for _, email := range []string{"www.person@example.com", "oidc@www.example.com"} {
		for _, name := range []types.String{types.StringNull(), types.StringUnknown()} {
			t.Run(email+"/"+name.String(), func(t *testing.T) {
				t.Parallel()
				current := userTestUser()
				current.Email = nullable.NewNullableWithValue(openapi_types.Email(email))
				current.Name = groupTestPointer("Pending user")
				var calls []string
				r := &userResource{api: userTestAdminClient(t, func(w http.ResponseWriter, req *http.Request) {
					calls = append(calls, req.URL.Path)
					switch req.URL.Path {
					case "/api/users.list":
						userTestListOffset(t, w, req)
						groupTestEncode(t, w, userTestList([]client.User{}, 0, 100, 0))
					case "/api/users.invite":
						var body client.UsersInviteJSONRequestBody
						if !groupTestDecode(t, w, req, &body) {
							return
						}
						if len(body.Invites) != 1 || body.Invites[0].Email != email || body.Invites[0].Name != "Pending user" {
							t.Errorf("omitted name reused a URL-like email: %+v", body)
						}
						envelope := userTestInviteEnvelope(current)
						envelope["data"].(map[string]any)["sent"] = []client.Invite{{Email: email, Name: "Pending user", Role: client.UserRoleMember}}
						groupTestEncode(t, w, envelope)
					case "/api/users.info":
						groupTestEncode(t, w, userTestEnvelope(current))
					default:
						t.Errorf("omitted name caused a write: %s", req.URL.Path)
					}
				})}
				model := userTestModel()
				model.Email, model.Name = types.StringValue(email), name
				response := userTestCreate(t, r, model)
				if response.Diagnostics.HasError() {
					t.Fatal(response.Diagnostics)
				}
				got := userTestStateModel(t, response.State)
				if got.ID.ValueString() != userTestID || got.Email != model.Email || got.Name.ValueString() != "Pending user" ||
					!reflect.DeepEqual(calls, []string{"/api/users.list", "/api/users.invite", "/api/users.info"}) {
					t.Fatalf("URL-like email invitation: %+v calls=%v", got, calls)
				}
			})
		}
	}
}

func TestUserCommittedActivationWithInvalidResponseForcesTrustedSuspension(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, want string
		change     func(*client.User)
		lost       bool
	}{
		{"malformed JSON", "could not be decoded", nil, false},
		{"different ID", "different ID", func(user *client.User) { user.Id = groupTestPointer(uuid.MustParse(userTestOtherID)) }, false},
		{"owner ID", "different ID", func(user *client.User) { user.Id = groupTestPointer(uuid.MustParse(userTestOwnerID)) }, false},
		{"different email", "different email", func(user *client.User) {
			user.Email = nullable.NewNullableWithValue(openapi_types.Email("other@example.com"))
		}, false},
		{"stale suspended response", "did not confirm desired suspension", func(user *client.User) { user.IsSuspended = groupTestPointer(true) }, false},
		{"lost response", "request failed", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			current := userTestUser()
			current.IsSuspended = groupTestPointer(true)
			var calls []string
			r := &userResource{api: userTestAdminClient(t, func(w http.ResponseWriter, req *http.Request) {
				calls = append(calls, req.URL.Path)
				var body map[string]any
				if !groupTestDecode(t, w, req, &body) || !reflect.DeepEqual(body, map[string]any{"id": userTestID}) {
					t.Errorf("recovery used an untrusted identity: %s %v", req.URL.Path, body)
					return
				}
				switch req.URL.Path {
				case "/api/users.info":
					groupTestEncode(t, w, userTestEnvelope(current))
				case "/api/users.activate":
					// Commit activation even though its response cannot be trusted.
					current.IsSuspended = groupTestPointer(false)
					if tc.lost {
						connection, _, err := w.(http.Hijacker).Hijack()
						if err != nil {
							t.Errorf("hijack: %v", err)
							return
						}
						if err := connection.Close(); err != nil {
							t.Errorf("close: %v", err)
						}
						return
					}
					if tc.change == nil {
						groupTestWrite(t, w, `{"ok":true,"data":`)
						return
					}
					response := *current
					tc.change(&response)
					groupTestEncode(t, w, userTestEnvelope(&response))
				case "/api/users.suspend":
					current.IsSuspended = groupTestPointer(true)
					groupTestEncode(t, w, userTestEnvelope(current))
				default:
					t.Errorf("ambiguous activation replayed or continued reconciliation: %s", req.URL.Path)
				}
			})}
			model := userTestModel()
			model.Role, model.Name, model.Suspended = types.StringValue("guest"), types.StringValue("Updated"), types.BoolValue(true)
			model.AllowTemporaryActivationForRoleChange = types.BoolValue(true)
			response := userTestUpdate(t, r, model, true)
			groupTestDiagnostics(t, response.Diagnostics, tc.want)
			got := userTestStateModel(t, response.State)
			if got.ID.ValueString() != userTestID || got.Email.ValueString() != userTestEmail || got.Role.ValueString() != "member" ||
				got.Name.ValueString() != "OIDC User" || !got.Suspended.ValueBool() || !*current.IsSuspended ||
				!reflect.DeepEqual(calls, []string{"/api/users.info", "/api/users.activate", "/api/users.suspend"}) {
				t.Fatalf("committed activation left the account active or adopted an untrusted response: %+v calls=%v", got, calls)
			}
		})
	}
}

func TestUserInitiallyActiveFailureStillSuspends(t *testing.T) {
	t.Parallel()
	for _, operation := range []string{"update_role", "update"} {
		t.Run(operation, func(t *testing.T) {
			t.Parallel()
			current := userTestUser()
			var calls []string
			r := &userResource{api: userTestAdminClient(t, func(w http.ResponseWriter, req *http.Request) {
				calls = append(calls, req.URL.Path)
				switch req.URL.Path {
				case "/api/users.info":
					groupTestEncode(t, w, userTestEnvelope(current))
				case "/api/users." + operation:
					w.WriteHeader(http.StatusForbidden)
					groupTestWrite(t, w, `{"error":"permission_denied"}`)
				case "/api/users.suspend":
					var body client.UsersSuspendJSONRequestBody
					if !groupTestDecode(t, w, req, &body) || body.Id.String() != userTestID {
						t.Error("cleanup omitted the trusted ID")
						return
					}
					current.IsSuspended = groupTestPointer(true)
					groupTestEncode(t, w, userTestEnvelope(current))
				default:
					t.Errorf("initially active recovery replayed or activated: %s", req.URL.Path)
				}
			})}
			model := userTestModel()
			model.Suspended = types.BoolValue(true)
			if operation == "update_role" {
				model.Role = types.StringValue("guest")
			} else {
				model.Name = types.StringValue("Updated")
			}
			response := userTestUpdate(t, r, model, true)
			groupTestDiagnostics(t, response.Diagnostics, "HTTP 403")
			got := userTestStateModel(t, response.State)
			if got.ID.ValueString() != userTestID || got.Role.ValueString() != "member" || got.Name.ValueString() != "OIDC User" ||
				!got.Suspended.ValueBool() || !*current.IsSuspended ||
				!reflect.DeepEqual(calls, []string{"/api/users.info", "/api/users." + operation, "/api/users.suspend"}) {
				t.Fatalf("initially active account was not suspended after failure: %+v calls=%v", got, calls)
			}
		})
	}
}

type userTestRoundTripper func(*http.Request) (*http.Response, error)

func (f userTestRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestUserCanceledRoleRequestUsesDetachedSuspensionCleanup(t *testing.T) {
	t.Parallel()
	type contextKey struct{}
	ctx, cancel := context.WithTimeout(context.WithValue(t.Context(), contextKey{}, "cleanup value"), 5*time.Second)
	defer cancel()
	started, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	var mu sync.Mutex
	var calls []string
	current := userTestUser()
	current.IsSuspended = groupTestPointer(true)
	api := userTestAdminClient(t, func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		calls = append(calls, req.URL.Path)
		if req.URL.Path == "/api/users.update_role" {
			mu.Unlock()
			close(started)
			select {
			case <-req.Context().Done():
			case <-release:
			}
			return
		}
		defer mu.Unlock()
		var body map[string]any
		if !groupTestDecode(t, w, req, &body) || !reflect.DeepEqual(body, map[string]any{"id": userTestID}) {
			t.Errorf("cleanup omitted authorized identity: %s %v", req.URL.Path, body)
			return
		}
		switch req.URL.Path {
		case "/api/users.info":
		case "/api/users.activate":
			current.IsSuspended = groupTestPointer(false)
		case "/api/users.suspend":
			current.IsSuspended = groupTestPointer(true)
		default:
			t.Errorf("unexpected request during canceled update: %s", req.URL.Path)
		}
		groupTestEncode(t, w, userTestEnvelope(current))
	})
	api.httpClient.Timeout = 30 * time.Second
	transport := api.httpClient.Transport.(*bearerTransport)
	base := transport.base
	var detached atomic.Bool
	transport.base = userTestRoundTripper(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/api/users.suspend" {
			deadline, ok := req.Context().Deadline()
			remaining := time.Until(deadline)
			if req.Context().Err() != nil || req.Context().Value(contextKey{}) != "cleanup value" || !ok || remaining < 9*time.Second || remaining > 10*time.Second {
				t.Errorf("cleanup context must retain values but replace cancellation/deadline with a 10s bound: err=%v remaining=%v", req.Context().Err(), remaining)
			}
			detached.Store(true)
		}
		return base.RoundTrip(req)
	})
	r := &userResource{api: api}
	model := userTestModel()
	model.Role, model.Suspended = types.StringValue("guest"), types.BoolValue(true)
	model.AllowTemporaryActivationForRoleChange = types.BoolValue(true)
	plan := userTestPlan(t, r, model)
	result := make(chan resource.UpdateResponse, 1)
	go func() {
		response := resource.UpdateResponse{State: tfsdk.State(plan)}
		r.Update(ctx, resource.UpdateRequest{Plan: plan, Config: tfsdk.Config(plan), State: tfsdk.State(plan)}, &response)
		result <- response
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("role request did not reach the server")
	}
	cancel()
	select {
	case response := <-result:
		groupTestDiagnostics(t, response.Diagnostics, "canceled")
		got := userTestStateModel(t, response.State)
		mu.Lock()
		defer mu.Unlock()
		if !detached.Load() || got.ID.ValueString() != userTestID || got.Role.ValueString() != "member" || !got.Suspended.ValueBool() ||
			!*current.IsSuspended || !reflect.DeepEqual(calls, []string{"/api/users.info", "/api/users.activate", "/api/users.update_role", "/api/users.suspend"}) {
			t.Fatalf("canceled role request left the account active: %+v calls=%v", got, calls)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("canceled role request or detached cleanup did not finish")
	}
}

func TestUserCreateReconciliationFailureSuspendsAndRetainsIdentity(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, failure, want string
		restoreFails        bool
	}{
		{"read denied", "read denied", "HTTP 403", false},
		{"malformed read", "malformed read", "malformed user", false},
		{"read different ID", "read different ID", "different ID", false},
		{"read different email", "read different email", "different email", false},
		{"guest role denied", "role", "HTTP 403", false},
		{"read and cleanup denied", "read denied", "restoring suspension also failed", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			current := userTestUser()
			var calls []string
			r := &userResource{api: userTestAdminClient(t, func(w http.ResponseWriter, req *http.Request) {
				calls = append(calls, req.URL.Path)
				switch req.URL.Path {
				case "/api/users.list":
					userTestListOffset(t, w, req)
					groupTestEncode(t, w, userTestList([]client.User{}, 0, 100, 0))
				case "/api/users.invite":
					groupTestEncode(t, w, userTestInviteEnvelope(current))
				case "/api/users.info":
					switch tc.failure {
					case "read denied":
						w.WriteHeader(http.StatusForbidden)
						groupTestWrite(t, w, `{"error":"permission_denied"}`)
					case "malformed read":
						groupTestWrite(t, w, `{"ok":true,"data":{}}`)
					default:
						response := *current
						switch tc.failure {
						case "read different ID":
							response.Id = groupTestPointer(uuid.MustParse(userTestOtherID))
						case "read different email":
							response.Email = nullable.NewNullableWithValue(openapi_types.Email("other@example.com"))
						}
						groupTestEncode(t, w, userTestEnvelope(&response))
					}
				case "/api/users.update_role":
					var body client.UsersUpdateRoleJSONRequestBody
					if !groupTestDecode(t, w, req, &body) || body.Id.String() != userTestID || body.Role != client.UserRoleGuest {
						t.Error("guest reconciliation omitted identity or role")
						return
					}
					w.WriteHeader(http.StatusForbidden)
					groupTestWrite(t, w, `{"error":"permission_denied"}`)
				case "/api/users.suspend":
					var body client.UsersSuspendJSONRequestBody
					if !groupTestDecode(t, w, req, &body) || body.Id.String() != userTestID {
						t.Error("partial Create cleanup used an untrusted identity")
						return
					}
					if tc.restoreFails {
						w.WriteHeader(http.StatusForbidden)
						groupTestWrite(t, w, `{"error":"permission_denied"}`)
						return
					}
					current.IsSuspended = groupTestPointer(true)
					groupTestEncode(t, w, userTestEnvelope(current))
				default:
					t.Errorf("partial Create replayed a write: %s", req.URL.Path)
				}
			})}
			model := userTestModel()
			model.Role, model.Suspended = types.StringValue("guest"), types.BoolValue(true)
			response := userTestCreate(t, r, model)
			groupTestDiagnostics(t, response.Diagnostics, tc.want)
			groupTestDiagnostics(t, response.Diagnostics, "retained in state")
			got := userTestStateModel(t, response.State)
			want := []string{"/api/users.list", "/api/users.invite", "/api/users.info"}
			if tc.failure == "role" {
				want = append(want, "/api/users.update_role")
			}
			want = append(want, "/api/users.suspend")
			if got.ID.ValueString() != userTestID || got.Email != model.Email || got.Role.ValueString() != "member" || got.Name.ValueString() != "OIDC User" ||
				got.Suspended.ValueBool() == tc.restoreFails || *current.IsSuspended == tc.restoreFails || !reflect.DeepEqual(calls, want) {
				t.Fatalf("failed Create lost identity or failed to suspend: %+v calls=%v want=%v", got, calls, want)
			}
		})
	}
}

func TestUserResourceAuthInfo404RetainsStateWithoutWrites(t *testing.T) {
	t.Parallel()
	for _, operation := range []string{"read", "delete", "create"} {
		t.Run(operation, func(t *testing.T) {
			t.Parallel()
			var calls []string
			r := &userResource{api: userTestClient(t, func(w http.ResponseWriter, req *http.Request) {
				calls = append(calls, req.URL.Path)
				if req.URL.Path != "/api/auth.info" {
					t.Errorf("missing auth.info endpoint caused target reads or writes: %s", req.URL.Path)
				}
				w.WriteHeader(http.StatusNotFound)
				groupTestWrite(t, w, `{"error":"not_found"}`)
			})}
			model := userTestModel()
			model.Suspended, model.DeletePermanently = types.BoolValue(true), types.BoolValue(true)
			plan := userTestPlan(t, r, model)
			state := tfsdk.State(plan)
			switch operation {
			case "read":
				response := resource.ReadResponse{State: state}
				r.Read(t.Context(), resource.ReadRequest{State: state}, &response)
				groupTestDiagnostics(t, response.Diagnostics, "auth.info")
				if !response.State.Raw.Equal(state.Raw) {
					t.Fatal("missing auth.info endpoint removed user state")
				}
			case "delete":
				response := resource.DeleteResponse{State: state}
				r.Delete(t.Context(), resource.DeleteRequest{State: state}, &response)
				groupTestDiagnostics(t, response.Diagnostics, "auth.info")
				if !response.State.Raw.Equal(state.Raw) {
					t.Fatal("missing auth.info endpoint removed user state on Delete")
				}
			case "create":
				response := userTestCreate(t, r, model)
				groupTestDiagnostics(t, response.Diagnostics, "auth.info")
				if response.State.Raw.IsKnown() && !response.State.Raw.IsNull() {
					t.Fatal("missing auth.info endpoint adopted a user on Create")
				}
			}
			if !reflect.DeepEqual(calls, []string{"/api/auth.info"}) {
				t.Fatalf("auth.info 404 was mistaken for an absent user: %v", calls)
			}
		})
	}
}

func TestUserDeleteResponseValidation(t *testing.T) {
	t.Parallel()
	for _, body := range []string{``, `{`, `null`, `{}`, `{"ok":false,"success":true}`, `{"ok":true,"status":201,"success":true}`, `{"ok":true}`, `{"ok":true,"success":null}`, `{"ok":true,"success":false}`} {
		t.Run(body, func(t *testing.T) {
			t.Parallel()
			var calls atomic.Int32
			api := userTestClient(t, func(w http.ResponseWriter, req *http.Request) { calls.Add(1); groupTestWrite(t, w, body) })
			if err := api.deleteUser(t.Context(), userTestUser()); err == nil || calls.Load() != 1 {
				t.Fatalf("invalid delete accepted or replayed: %v", err)
			}
		})
	}
}
