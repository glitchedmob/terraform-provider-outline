// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/glitchedmob/terraform-provider-outline/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"github.com/oapi-codegen/nullable"
)

// The framework supplies the prior state to CRUD responses. Seed it here so
// failures must leave every attribute unchanged, not just the collection ID.
func collectionTestOperation(t *testing.T, r *collectionResource, operation string, model collectionModel) (diag.Diagnostics, tfsdk.State) {
	t.Helper()
	plan := collectionTestPlan(t, r, model)
	state := tfsdk.State(plan)
	switch operation {
	case "create":
		response := collectionTestCreate(t, r, model)
		return response.Diagnostics, response.State
	case "read":
		response := resource.ReadResponse{State: state}
		r.Read(t.Context(), resource.ReadRequest{State: state}, &response)
		return response.Diagnostics, response.State
	case "update", "update preflight":
		planned := model
		planned.Name, planned.Description = types.StringValue("New name"), types.StringValue("New description")
		response := resource.UpdateResponse{State: state}
		r.Update(t.Context(), resource.UpdateRequest{Plan: collectionTestPlan(t, r, planned), State: state}, &response)
		return response.Diagnostics, response.State
	case "delete":
		response := resource.DeleteResponse{State: state}
		r.Delete(t.Context(), resource.DeleteRequest{State: state}, &response)
		return response.Diagnostics, response.State
	case "lookup ID", "lookup name":
		d := &collectionDataSource{api: r.api}
		lookup := collectionLookupModel{ID: types.StringValue(model.ID.ValueString())}
		if operation == "lookup name" {
			lookup.ID, lookup.Name = types.StringNull(), model.Name
		}
		config := collectionTestConfig(t, d, lookup)
		response := datasource.ReadResponse{State: tfsdk.State{Schema: config.Schema}}
		d.Read(t.Context(), datasource.ReadRequest{Config: config}, &response)
		return response.Diagnostics, response.State
	default:
		t.Fatalf("unknown test operation %q", operation)
		return nil, tfsdk.State{}
	}
}

func collectionTestAssertNoState(t *testing.T, state tfsdk.State) {
	t.Helper()
	if state.Raw.IsKnown() && !state.Raw.IsNull() {
		t.Fatalf("failure adopted collection state: %+v", state)
	}
}

func TestCollectionMalformedResponsesRetainState(t *testing.T) {
	t.Parallel()
	type testCase struct {
		name, body, contains string
		retainCreate         bool
		contentType          string
	}
	cases := []testCase{
		{name: "invalid JSON", body: `{"secret":"` + groupTestKey + `",`, contains: "decoded"},
		{name: "not JSON content type", body: userTestJSON(t, collectionTestEnvelope(collectionTestCollection())), contains: "missing JSON", contentType: "text/plain"},
		{name: "empty envelope", body: `{}`, contains: "envelope"},
		{name: "missing data", body: `{"ok":true}`, contains: "missing"},
		{name: "null data", body: `{"ok":true,"data":null}`, contains: "missing"},
	}
	for _, field := range []string{"id", "name", "description", "permission", "sharing", "archivedAt", "deletedAt"} {
		fields := collectionTestFields()
		delete(fields, field)
		cases = append(cases, testCase{name: "missing " + field, body: userTestJSON(t, map[string]any{"ok": true, "data": fields}), contains: "missing", retainCreate: field != "id"})
	}
	for _, tc := range []struct {
		name, field string
		value       any
		contains    string
		retain      bool
	}{
		{"null ID", "id", nil, "missing", false},
		{"zero ID", "id", "00000000-0000-0000-0000-000000000000", "UUID", false},
		{"invalid ID", "id", "not-a-uuid", "decoded", false},
		{"invalid variant", "id", "a32c2ee6-fbde-4654-041b-0eabdc71b812", "UUID", false},
		{"invalid version", "id", "a32c2ee6-fbde-0654-841b-0eabdc71b812", "UUID", false},
		{"null name", "name", nil, "missing", true},
		{"empty name", "name", "", "missing", true},
		{"null sharing", "sharing", nil, "missing", true},
		{"invalid permission enum", "permission", "owner", "unsupported", true},
		{"deleted collection", "deletedAt", "2026-01-01T00:00:00Z", "deleted collection", true},
		{"wrong description type", "description", map[string]any{}, "decoded", true},
		{"wrong sharing type", "sharing", "false", "decoded", true},
		{"wrong permission type", "permission", []string{"read"}, "decoded", true},
		{"invalid archived timestamp", "archivedAt", "invalid", "decoded", true},
	} {
		fields := collectionTestFields()
		fields[tc.field] = tc.value
		cases = append(cases, testCase{name: tc.name, body: userTestJSON(t, map[string]any{"ok": true, "data": fields}), contains: tc.contains, retainCreate: tc.retain})
	}
	for _, tc := range []struct {
		name, field string
		value       any
	}{
		{"missing ok", "ok", nil}, {"false ok", "ok", false}, {"wrong status", "status", 201},
	} {
		envelope := collectionTestEnvelope(collectionTestCollection())
		envelope[tc.field] = tc.value
		cases = append(cases, testCase{name: tc.name, body: userTestJSON(t, envelope), contains: "envelope", retainCreate: true})
	}
	for _, operation := range []string{"create", "read", "update", "update preflight", "delete", "lookup ID"} {
		for _, tc := range cases {
			t.Run(operation+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				var calls []string
				r := &collectionResource{api: collectionTestAdminClient(t, func(w http.ResponseWriter, req *http.Request) {
					calls = append(calls, req.URL.Path)
					if operation == "update" && req.URL.Path == "/api/collections.info" {
						groupTestEncode(t, w, collectionTestEnvelope(collectionTestCollection()))
						return
					}
					if tc.contentType != "" {
						w.Header().Set("Content-Type", tc.contentType)
					}
					groupTestWrite(t, w, tc.body)
				})}
				model := collectionTestModel()
				model.AllowDestroy = types.BoolValue(true)
				diagnostics, state := collectionTestOperation(t, r, operation, model)
				groupTestDiagnostics(t, diagnostics, tc.contains)
				switch operation {
				case "create":
					if tc.retainCreate {
						if got := collectionTestStateModel(t, state); got != model {
							t.Fatalf("partial create must retain trustworthy ID and planned values: %+v", got)
						}
						if !strings.Contains(diagnostics.Errors()[0].Detail(), "untaint") {
							t.Error("partial create diagnostic omitted recovery instructions")
						}
					} else {
						collectionTestAssertNoState(t, state)
					}
				case "lookup ID":
					collectionTestAssertNoState(t, state)
				default:
					prior := tfsdk.State(collectionTestPlan(t, r, model))
					if !state.Raw.Equal(prior.Raw) {
						t.Fatal("failed CRUD changed prior state")
					}
				}
				want := []string{"/api/collections.info"}
				switch operation {
				case "create":
					want = []string{"/api/collections.create"}
				case "update":
					want = append(want, "/api/collections.update")
				}
				if !reflect.DeepEqual(calls, want) {
					t.Fatalf("malformed response caused extra calls or a write: %v want %v", calls, want)
				}
			})
		}
	}
}

func TestCollectionMismatchedIDNeverAdopted(t *testing.T) {
	t.Parallel()
	for _, operation := range []string{"read", "update", "update preflight", "delete", "lookup ID"} {
		t.Run(operation, func(t *testing.T) {
			t.Parallel()
			var calls []string
			r := &collectionResource{api: collectionTestAdminClient(t, func(w http.ResponseWriter, req *http.Request) {
				calls = append(calls, req.URL.Path)
				fields := collectionTestFields()
				if operation != "update" || req.URL.Path != "/api/collections.info" {
					fields["id"] = collectionTestOtherID
				}
				groupTestEncode(t, w, map[string]any{"ok": true, "data": fields})
			})}
			model := collectionTestModel()
			model.AllowDestroy = types.BoolValue(true)
			diagnostics, state := collectionTestOperation(t, r, operation, model)
			groupTestDiagnostics(t, diagnostics, "different ID")
			if operation == "lookup ID" {
				collectionTestAssertNoState(t, state)
			} else if !state.Raw.Equal(tfsdk.State(collectionTestPlan(t, r, model)).Raw) {
				t.Fatal("mismatched response changed prior state")
			}
			want := []string{"/api/collections.info"}
			if operation == "update" {
				want = append(want, "/api/collections.update")
			}
			if !reflect.DeepEqual(calls, want) {
				t.Fatalf("mismatched response caused writes: %v", calls)
			}
		})
	}
}

func TestCollectionInfoAbsenceContract(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, body string
		status     int
		absent     bool
	}{
		{"Outline not found", `{"ok":false,"status":404,"error":"not_found"}`, 404, true},
		{"authorization error", `{"ok":false,"status":403,"error":"authorization_error"}`, 403, false},
		{"permission error", `{"ok":false,"status":403,"error":"permission_error"}`, 403, false},
		{"plain forbidden", `Forbidden`, 403, false},
		{"proxy 404", `<html>Not found</html>`, 404, false},
		{"empty 404", `{}`, 404, false},
		{"missing ok", `{"status":404,"error":"not_found"}`, 404, false},
		{"missing status", `{"ok":false,"error":"not_found"}`, 404, false},
		{"missing error", `{"ok":false,"status":404}`, 404, false},
		{"wrong error", `{"ok":false,"status":404,"error":"authorization_error"}`, 404, false},
		{"wrong status", `{"ok":false,"status":403,"error":"not_found"}`, 404, false},
		{"successful 404 envelope", `{"ok":true,"status":404,"error":"not_found"}`, 404, false},
	} {
		for _, operation := range []string{"read", "update preflight", "delete", "lookup ID"} {
			t.Run(tc.name+"/"+operation, func(t *testing.T) {
				t.Parallel()
				var calls []string
				r := &collectionResource{api: collectionTestAdminClient(t, func(w http.ResponseWriter, req *http.Request) {
					calls = append(calls, req.URL.Path)
					if req.URL.Path == "/api/collections.list" {
						// Even an empty complete workspace list cannot disprove a
						// forbidden collection's existence in another workspace.
						groupTestEncode(t, w, collectionTestList([]client.Collection{}, 0, 100, 0))
						return
					}
					w.WriteHeader(tc.status)
					groupTestWrite(t, w, tc.body)
				})}
				model := collectionTestModel()
				diagnostics, state := collectionTestOperation(t, r, operation, model)
				if tc.absent && (operation == "read" || operation == "delete") {
					if diagnostics.HasError() || operation == "read" && !state.Raw.IsNull() {
						t.Fatalf("verified Outline 404 did not establish absence: %v %+v", diagnostics, state)
					}
				} else {
					groupTestDiagnostics(t, diagnostics, "")
					if operation == "lookup ID" {
						collectionTestAssertNoState(t, state)
					} else if !state.Raw.Equal(tfsdk.State(collectionTestPlan(t, r, model)).Raw) {
						t.Fatal("inaccessible or failed operation removed collection state")
					}
				}
				if !reflect.DeepEqual(calls, []string{"/api/collections.info"}) {
					t.Fatalf("absence check listed or wrote collections: %v", calls)
				}
			})
		}
	}
}

func TestCollectionRequiresActiveAdminBeforeRequests(t *testing.T) {
	t.Parallel()
	member, suspended, malformed := userTestOwner(), userTestOwner(), userTestOwner()
	member.Role = groupTestPointer(client.UserRoleMember)
	suspended.IsSuspended = groupTestPointer(true)
	malformed.Role = nil
	for _, tc := range []struct {
		name, body, contains string
		status               int
	}{
		{"member", userTestJSON(t, userTestAuth(member)), "active admin", 200},
		{"suspended admin", userTestJSON(t, userTestAuth(suspended)), "active admin", 200},
		{"malformed actor", userTestJSON(t, userTestAuth(malformed)), "auth.info", 200},
		{"missing auth route", `{"ok":false,"status":404,"error":"not_found"}`, "auth.info", 404},
		{"unauthorized", `{"error":"authentication_required"}`, "auth.info", 401},
		{"bad envelope", `{"ok":false,"data":{}}`, "auth.info", 200},
	} {
		for _, operation := range []string{"create", "read", "update preflight", "delete", "lookup ID", "lookup name"} {
			t.Run(tc.name+"/"+operation, func(t *testing.T) {
				t.Parallel()
				var calls []string
				r := &collectionResource{api: collectionTestClient(t, func(w http.ResponseWriter, req *http.Request) {
					calls = append(calls, req.URL.Path)
					w.WriteHeader(tc.status)
					groupTestWrite(t, w, tc.body)
				})}
				model := collectionTestModel()
				model.AllowDestroy = types.BoolValue(true)
				diagnostics, state := collectionTestOperation(t, r, operation, model)
				groupTestDiagnostics(t, diagnostics, tc.contains)
				if operation == "create" || strings.HasPrefix(operation, "lookup") {
					collectionTestAssertNoState(t, state)
				} else if !state.Raw.Equal(tfsdk.State(collectionTestPlan(t, r, model)).Raw) {
					t.Fatal("failed admin check changed state")
				}
				if !reflect.DeepEqual(calls, []string{"/api/auth.info"}) {
					t.Fatalf("non-admin reached collections endpoint: %v", calls)
				}
			})
		}
	}
}

func TestCollectionUnsupportedResourceAllowsLookup(t *testing.T) {
	t.Parallel()
	for _, archived := range []bool{false, true} {
		for _, operation := range []string{"create", "read", "update", "update preflight", "delete", "lookup ID"} {
			t.Run(operation+"/archived="+types.BoolValue(archived).String(), func(t *testing.T) {
				t.Parallel()
				collection := collectionTestCollection()
				contains := "admin default permission"
				if archived {
					collection.ArchivedAt = nullable.NewNullableWithValue(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
					contains = "archived collections"
				} else {
					collection.Permission = nullable.NewNullableWithValue(client.PermissionAdmin)
				}
				var calls []string
				r := &collectionResource{api: collectionTestAdminClient(t, func(w http.ResponseWriter, req *http.Request) {
					calls = append(calls, req.URL.Path)
					if operation == "update" && req.URL.Path == "/api/collections.info" {
						groupTestEncode(t, w, collectionTestEnvelope(collectionTestCollection()))
						return
					}
					groupTestEncode(t, w, collectionTestEnvelope(collection))
				})}
				model := collectionTestModel()
				model.AllowDestroy = types.BoolValue(true)
				diagnostics, state := collectionTestOperation(t, r, operation, model)
				if operation == "lookup ID" {
					var got collectionLookupModel
					if diagnostics.HasError() {
						t.Fatal(diagnostics)
					}
					if diagnostics := state.Get(t.Context(), &got); diagnostics.HasError() {
						t.Fatal(diagnostics)
					}
					want := collectionLookupModel{}
					want.setCollection(collection)
					if got != want {
						t.Fatalf("lookup lost unsupported metadata: %+v", got)
					}
				} else {
					groupTestDiagnostics(t, diagnostics, contains)
					if got := collectionTestStateModel(t, state); got != model {
						t.Fatalf("unsupported resource changed prior or partial-create state: %+v", got)
					}
				}
				wantCalls := []string{"/api/collections.info"}
				switch operation {
				case "create":
					wantCalls = []string{"/api/collections.create"}
				case "update":
					wantCalls = append(wantCalls, "/api/collections.update")
				}
				if !reflect.DeepEqual(calls, wantCalls) {
					t.Fatalf("unsupported resource caused unexpected writes: %v", calls)
				}
			})
		}
	}
}

func TestCollectionDestroyGuardAndDeleteValidation(t *testing.T) {
	t.Parallel()
	for _, guard := range []types.Bool{types.BoolValue(false), types.BoolNull(), types.BoolUnknown()} {
		t.Run("blocked/"+guard.String(), func(t *testing.T) {
			t.Parallel()
			var calls []string
			r := &collectionResource{api: collectionTestAdminClient(t, func(w http.ResponseWriter, req *http.Request) {
				calls = append(calls, req.URL.Path)
				groupTestEncode(t, w, collectionTestEnvelope(collectionTestCollection()))
			})}
			model := collectionTestModel()
			model.AllowDestroy = guard
			diagnostics, state := collectionTestOperation(t, r, "delete", model)
			groupTestDiagnostics(t, diagnostics, "allow_destroy = true")
			if !state.Raw.Equal(tfsdk.State(collectionTestPlan(t, r, model)).Raw) || !reflect.DeepEqual(calls, []string{"/api/collections.info"}) {
				t.Fatalf("guard changed state or called collections.delete: %v", calls)
			}
		})
	}
	for _, tc := range []struct {
		name, body string
		status     int
		bad        bool
	}{
		{"success", `{"ok":true,"status":200,"success":true}`, 200, false},
		{"missing success", `{"ok":true,"status":200}`, 200, true},
		{"false success", `{"ok":true,"success":false}`, 200, true},
		{"null success", `{"ok":true,"success":null}`, 200, true},
		{"missing ok", `{"success":true}`, 200, true},
		{"false ok", `{"ok":false,"success":true}`, 200, true},
		{"wrong status", `{"ok":true,"status":201,"success":true}`, 200, true},
		{"invalid JSON", `{`, 200, true},
		{"forbidden", `{"ok":false,"status":403,"error":"authorization_error"}`, 403, true},
		{"write 404 is not info absence", `{"ok":false,"status":404,"error":"not_found"}`, 404, true},
		{"server error", `{"error":"internal_server_error"}`, 500, true},
		{"rate limited", `{"error":"rate_limit_exceeded"}`, 429, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var calls []string
			r := &collectionResource{api: collectionTestAdminClient(t, func(w http.ResponseWriter, req *http.Request) {
				calls = append(calls, req.URL.Path)
				collectionTestAssertIDBody(t, w, req)
				if req.URL.Path == "/api/collections.info" {
					groupTestEncode(t, w, collectionTestEnvelope(collectionTestCollection()))
					return
				}
				w.WriteHeader(tc.status)
				groupTestWrite(t, w, tc.body)
			})}
			model := collectionTestModel()
			model.AllowDestroy = types.BoolValue(true)
			diagnostics, state := collectionTestOperation(t, r, "delete", model)
			if diagnostics.HasError() != tc.bad {
				t.Fatalf("delete bad=%t: %v", tc.bad, diagnostics)
			}
			if tc.bad && !state.Raw.Equal(tfsdk.State(collectionTestPlan(t, r, model)).Raw) {
				t.Fatal("failed delete removed state")
			}
			if !reflect.DeepEqual(calls, []string{"/api/collections.info", "/api/collections.delete"}) {
				t.Fatalf("delete replayed or skipped preflight: %v", calls)
			}
		})
	}
}

func TestCollectionImportAndInvalidIdentityBeforeRequests(t *testing.T) {
	t.Parallel()
	for _, id := range []string{
		"", "not-a-uuid", "00000000-0000-0000-0000-000000000000", strings.ToUpper(collectionTestID),
		strings.ReplaceAll(collectionTestID, "-", ""), "{" + collectionTestID + "}", "urn:uuid:" + collectionTestID,
		" " + collectionTestID, collectionTestID + " ", "a32c2ee6-fbde-4654-041b-0eabdc71b812", "a32c2ee6-fbde-0654-841b-0eabdc71b812",
	} {
		t.Run(id, func(t *testing.T) {
			t.Parallel()
			r := &collectionResource{api: collectionTestClient(t, func(http.ResponseWriter, *http.Request) { t.Error("invalid identity reached Outline") })}
			model := collectionTestModel()
			model.ID = types.StringValue(id)
			state := tfsdk.State(collectionTestPlan(t, r, model))
			for _, operation := range []string{"read", "update preflight", "delete", "lookup ID"} {
				diagnostics, got := collectionTestOperation(t, r, operation, model)
				groupTestDiagnostics(t, diagnostics, "UUID")
				if operation == "lookup ID" {
					collectionTestAssertNoState(t, got)
				} else if !got.Raw.Equal(state.Raw) {
					t.Fatal("invalid ID changed state")
				}
			}
			response := resource.ImportStateResponse{State: state}
			r.ImportState(t.Context(), resource.ImportStateRequest{ID: id}, &response)
			groupTestDiagnostics(t, response.Diagnostics, "UUID")
			if !response.State.Raw.Equal(state.Raw) {
				t.Fatal("invalid import changed state")
			}
		})
	}
	r := &collectionResource{api: collectionTestClient(t, func(http.ResponseWriter, *http.Request) { t.Error("import contacted Outline") })}
	model := collectionTestModel()
	model.ID, model.AllowDestroy = types.StringValue(collectionTestOtherID), types.BoolValue(true)
	response := resource.ImportStateResponse{State: tfsdk.State(collectionTestPlan(t, r, model))}
	r.ImportState(t.Context(), resource.ImportStateRequest{ID: collectionTestID}, &response)
	model.ID, model.AllowDestroy = types.StringValue(collectionTestID), types.BoolValue(false)
	if response.Diagnostics.HasError() || collectionTestStateModel(t, response.State) != model {
		t.Fatalf("canonical import failed to reset destruction guard: %v %+v", response.Diagnostics, response.State)
	}
	fresh := resource.ImportStateResponse{State: tfsdk.State{Schema: response.State.Schema, Raw: tftypes.NewValue(response.State.Raw.Type(), nil)}}
	r.ImportState(t.Context(), resource.ImportStateRequest{ID: collectionTestID}, &fresh)
	if fresh.Diagnostics.HasError() {
		t.Fatal(fresh.Diagnostics)
	}
	got := collectionTestStateModel(t, fresh.State)
	if got.ID.ValueString() != collectionTestID || got.AllowDestroy.IsNull() || got.AllowDestroy.IsUnknown() || got.AllowDestroy.ValueBool() {
		t.Fatalf("fresh import must set canonical ID and default guard: %+v", got)
	}
}
