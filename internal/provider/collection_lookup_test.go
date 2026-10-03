// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/glitchedmob/terraform-provider-outline/internal/client"
	"github.com/google/uuid"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/oapi-codegen/nullable"
)

func collectionTestList(collections []client.Collection, offset, limit, total int) map[string]any {
	return map[string]any{"ok": true, "status": 200, "data": collections, "pagination": client.PaginationResponse{
		Offset: &offset, Limit: &limit, Total: &total,
		NextPath: groupTestPointer("https://do-not-follow.invalid/api/collections.list?offset=999"),
	}}
}

func collectionTestListOffset(t *testing.T, w http.ResponseWriter, req *http.Request) int {
	t.Helper()
	var body map[string]any
	if !groupTestDecode(t, w, req, &body) {
		return -1
	}
	offset, ok := body["offset"].(float64)
	if !ok || offset < 0 || offset != float64(int(offset)) || !reflect.DeepEqual(body, map[string]any{
		"limit": float64(100), "offset": offset, "includeListOnly": true, "statusFilter": []any{},
	}) {
		t.Errorf("lookup must include private and archived collections with no filters: %v", body)
		return -1
	}
	return int(offset)
}

func collectionTestLookup(t *testing.T, api *apiClient, model collectionLookupModel) datasource.ReadResponse {
	t.Helper()
	d := &collectionDataSource{api: api}
	config := collectionTestConfig(t, d, model)
	response := datasource.ReadResponse{State: tfsdk.State{Schema: config.Schema}}
	d.Read(t.Context(), datasource.ReadRequest{Config: config}, &response)
	return response
}

func collectionTestLookupState(t *testing.T, state tfsdk.State) collectionLookupModel {
	t.Helper()
	var model collectionLookupModel
	if diagnostics := state.Get(t.Context(), &model); diagnostics.HasError() {
		t.Fatal(diagnostics)
	}
	return model
}

func TestCollectionNameLookupScansAllPagesBeforeInfo(t *testing.T) {
	t.Parallel()
	for _, matchPage := range []int{0, 1, 2} {
		t.Run(fmt.Sprintf("match page %d", matchPage), func(t *testing.T) {
			t.Parallel()
			match := collectionTestCollection()
			match.ArchivedAt = nullable.NewNullableWithValue(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
			match.Permission = nullable.NewNullableWithValue(client.PermissionAdmin)
			match.Description = nullable.NewNullableWithValue("# Archived landing page")
			var offsets []int
			var calls []string
			api := collectionTestClient(t, func(w http.ResponseWriter, req *http.Request) {
				calls = append(calls, req.URL.Path)
				switch req.URL.Path {
				case "/api/auth.info":
					groupTestEncode(t, w, userTestAuth(userTestOwner()))
				case "/api/collections.list":
					offset := collectionTestListOffset(t, w, req)
					offsets = append(offsets, offset)
					if offset < 0 || offset > 2 {
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					collection := collectionTestCollection()
					collection.Id = groupTestPointer(uuid.MustParse([]string{userTestID, userTestOtherID, userTestOwnerID}[offset]))
					collection.Name = groupTestPointer("Unrelated")
					if offset == matchPage {
						collection = match
					}
					// Honor the server's page size rather than our requested 100.
					groupTestEncode(t, w, collectionTestList([]client.Collection{*collection}, offset, 1, 3))
				case "/api/collections.info":
					if !reflect.DeepEqual(offsets, []int{0, 1, 2}) {
						t.Errorf("lookup read a match before validating every page: %v", offsets)
					}
					collectionTestAssertIDBody(t, w, req)
					groupTestEncode(t, w, collectionTestEnvelope(match))
				default:
					t.Errorf("unexpected lookup endpoint: %s", req.URL.Path)
					w.WriteHeader(http.StatusBadRequest)
				}
			})
			response := collectionTestLookup(t, api, collectionLookupModel{Name: types.StringValue("Engineering")})
			want := collectionLookupModel{ID: types.StringValue(collectionTestID), Name: types.StringValue("Engineering"), Description: types.StringValue("# Archived landing page"), Permission: types.StringValue("admin"), Sharing: types.BoolValue(false)}
			if response.Diagnostics.HasError() || collectionTestLookupState(t, response.State) != want {
				t.Fatalf("paginated lookup: %v %+v", response.Diagnostics, response.State)
			}
			if !reflect.DeepEqual(calls, []string{"/api/auth.info", "/api/collections.list", "/api/collections.list", "/api/collections.list", "/api/auth.info", "/api/collections.info"}) {
				t.Fatalf("unexpected lookup calls: %v", calls)
			}
		})
	}
}

func TestCollectionLookupByIDDoesNotListOrRequireOldName(t *testing.T) {
	t.Parallel()
	var calls []string
	collection := collectionTestCollection()
	collection.Name = groupTestPointer("Renamed remotely")
	collection.Description = nullable.NewNullNullable[string]()
	api := collectionTestAdminClient(t, func(w http.ResponseWriter, req *http.Request) {
		calls = append(calls, req.URL.Path)
		collectionTestAssertIDBody(t, w, req)
		groupTestEncode(t, w, collectionTestEnvelope(collection))
	})
	response := collectionTestLookup(t, api, collectionLookupModel{ID: types.StringValue(collectionTestID)})
	want := collectionLookupModel{ID: types.StringValue(collectionTestID), Name: types.StringValue("Renamed remotely"), Description: types.StringValue(""), Permission: types.StringNull(), Sharing: types.BoolValue(false)}
	if response.Diagnostics.HasError() || collectionTestLookupState(t, response.State) != want || !reflect.DeepEqual(calls, []string{"/api/collections.info"}) {
		t.Fatalf("ID lookup: %v %+v calls=%v", response.Diagnostics, response.State, calls)
	}
}

func TestCollectionNameLookupAmbiguousExactAndRenamed(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, first, second, infoName, contains string
		infoCalls                               int
	}{
		{"ambiguous across pages", "Engineering", "Engineering", "", "ambiguous", 0},
		{"case-sensitive", "engineering", "ENGINEERING", "", "exact name", 0},
		{"no substring match", "Engineering Ops", " Engineering", "", "exact name", 0},
		{"renamed after enumeration", "Engineering", "Unrelated", "Renamed", "name changed", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var offsets []int
			infoCalls := 0
			api := collectionTestAdminClient(t, func(w http.ResponseWriter, req *http.Request) {
				if req.URL.Path == "/api/collections.info" {
					infoCalls++
					collectionTestAssertIDBody(t, w, req)
					collection := collectionTestCollection()
					collection.Name = groupTestPointer(tc.infoName)
					groupTestEncode(t, w, collectionTestEnvelope(collection))
					return
				}
				if req.URL.Path != "/api/collections.list" {
					t.Errorf("unexpected lookup endpoint: %s", req.URL.Path)
				}
				offset := collectionTestListOffset(t, w, req)
				offsets = append(offsets, offset)
				collection := collectionTestCollection()
				collection.Name = groupTestPointer(tc.first)
				if offset == 1 {
					collection.Id = groupTestPointer(uuid.MustParse(collectionTestOtherID))
					collection.Name = groupTestPointer(tc.second)
				} else if offset != 0 {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				groupTestEncode(t, w, collectionTestList([]client.Collection{*collection}, offset, 1, 2))
			})
			response := collectionTestLookup(t, api, collectionLookupModel{Name: types.StringValue("Engineering")})
			groupTestDiagnostics(t, response.Diagnostics, tc.contains)
			collectionTestAssertNoState(t, response.State)
			if !reflect.DeepEqual(offsets, []int{0, 1}) || infoCalls != tc.infoCalls {
				t.Fatalf("lookup did not validate both pages: offsets=%v info=%d", offsets, infoCalls)
			}
		})
	}
	api := collectionTestAdminClient(t, func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/api/collections.list" {
			t.Errorf("empty list caused info request: %s", req.URL.Path)
		}
		collectionTestListOffset(t, w, req)
		groupTestEncode(t, w, collectionTestList([]client.Collection{}, 0, 100, 0))
	})
	response := collectionTestLookup(t, api, collectionLookupModel{Name: types.StringValue("Engineering")})
	groupTestDiagnostics(t, response.Diagnostics, "no collection found")
	collectionTestAssertNoState(t, response.State)
}

func TestCollectionNameLookupRejectsMalformedLaterPages(t *testing.T) {
	t.Parallel()
	type testCase struct {
		name, body, contains string
		status               int
	}
	other := collectionTestCollection()
	other.Id = groupTestPointer(uuid.MustParse(collectionTestOtherID))
	other.Name = groupTestPointer("Unrelated")
	cases := []testCase{
		{"invalid JSON", `{`, "decoded", 200},
		{"missing data", `{"ok":true}`, "missing collections data", 200},
		{"null data", `{"ok":true,"data":null}`, "missing collections data", 200},
		{"list forbidden", `{"ok":false,"status":403,"error":"authorization_error"}`, "HTTP 403", 403},
		{"missing list route", `{"ok":false,"status":404,"error":"not_found"}`, "not found", 404},
		{"duplicate across pages", userTestJSON(t, collectionTestList([]client.Collection{*collectionTestCollection()}, 1, 1, 3)), "duplicate collection", 200},
	}
	for _, tc := range []struct {
		name, contains string
		change         func(map[string]any)
	}{
		{"total changed", "total changed", func(page map[string]any) {
			page["pagination"] = client.PaginationResponse{Offset: groupTestPointer(1), Limit: groupTestPointer(1), Total: groupTestPointer(2)}
		}},
		{"wrong offset", "malformed pagination", func(page map[string]any) {
			page["pagination"] = client.PaginationResponse{Offset: groupTestPointer(0), Limit: groupTestPointer(1), Total: groupTestPointer(3)}
		}},
		{"negative offset", "malformed pagination", func(page map[string]any) {
			page["pagination"] = client.PaginationResponse{Offset: groupTestPointer(-1), Limit: groupTestPointer(1), Total: groupTestPointer(3)}
		}},
		{"zero limit", "malformed pagination", func(page map[string]any) {
			page["pagination"] = client.PaginationResponse{Offset: groupTestPointer(1), Limit: groupTestPointer(0), Total: groupTestPointer(3)}
		}},
		{"excessive limit", "malformed pagination", func(page map[string]any) {
			page["pagination"] = client.PaginationResponse{Offset: groupTestPointer(1), Limit: groupTestPointer(101), Total: groupTestPointer(3)}
		}},
		{"negative total", "malformed pagination", func(page map[string]any) {
			page["pagination"] = client.PaginationResponse{Offset: groupTestPointer(1), Limit: groupTestPointer(1), Total: groupTestPointer(-1)}
		}},
		{"total below offset", "malformed pagination", func(page map[string]any) {
			page["pagination"] = client.PaginationResponse{Offset: groupTestPointer(1), Limit: groupTestPointer(1), Total: groupTestPointer(0)}
		}},
		{"short page", "stopped before", func(page map[string]any) {
			page["pagination"] = client.PaginationResponse{Offset: groupTestPointer(1), Limit: groupTestPointer(2), Total: groupTestPointer(3)}
		}},
		{"empty intermediate page", "stopped before", func(page map[string]any) { page["data"] = []client.Collection{} }},
		{"missing pagination", "malformed pagination", func(page map[string]any) { delete(page, "pagination") }},
		{"missing offset", "malformed pagination", func(page map[string]any) {
			page["pagination"] = client.PaginationResponse{Limit: groupTestPointer(1), Total: groupTestPointer(3)}
		}},
		{"missing limit", "malformed pagination", func(page map[string]any) {
			page["pagination"] = client.PaginationResponse{Offset: groupTestPointer(1), Total: groupTestPointer(3)}
		}},
		{"missing total", "malformed pagination", func(page map[string]any) {
			page["pagination"] = client.PaginationResponse{Offset: groupTestPointer(1), Limit: groupTestPointer(1)}
		}},
		{"missing ok", "envelope", func(page map[string]any) { delete(page, "ok") }},
		{"false ok", "envelope", func(page map[string]any) { page["ok"] = false }},
		{"wrong envelope status", "envelope", func(page map[string]any) { page["status"] = 201 }},
		{"duplicate within page", "duplicate collection", func(page map[string]any) { page["data"] = []client.Collection{*other, *other} }},
		{"too many records", "malformed pagination", func(page map[string]any) {
			third := *other
			third.Id = groupTestPointer(uuid.MustParse(userTestID))
			page["data"] = []client.Collection{*other, third}
		}},
	} {
		page := collectionTestList([]client.Collection{*other}, 1, 1, 3)
		tc.change(page)
		cases = append(cases, testCase{tc.name, userTestJSON(t, page), tc.contains, 200})
	}
	for _, field := range []string{"id", "name", "description", "permission", "sharing", "archivedAt", "deletedAt"} {
		fields := collectionTestFields()
		fields["id"], fields["name"] = collectionTestOtherID, "Unrelated"
		delete(fields, field)
		page := collectionTestList([]client.Collection{*other}, 1, 1, 3)
		page["data"] = []any{fields}
		cases = append(cases, testCase{"missing collection " + field, userTestJSON(t, page), "missing", 200})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var offsets []int
			api := collectionTestAdminClient(t, func(w http.ResponseWriter, req *http.Request) {
				if req.URL.Path != "/api/collections.list" {
					t.Errorf("lookup trusted first-page match before complete validation: %s", req.URL.Path)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				offset := collectionTestListOffset(t, w, req)
				offsets = append(offsets, offset)
				if offset == 0 {
					groupTestEncode(t, w, collectionTestList([]client.Collection{*collectionTestCollection()}, 0, 1, 3))
					return
				}
				w.WriteHeader(tc.status)
				groupTestWrite(t, w, tc.body)
			})
			response := collectionTestLookup(t, api, collectionLookupModel{Name: types.StringValue("Engineering")})
			groupTestDiagnostics(t, response.Diagnostics, tc.contains)
			collectionTestAssertNoState(t, response.State)
			if !reflect.DeepEqual(offsets, []int{0, 1}) {
				t.Fatalf("lookup skipped or replayed malformed page: %v", offsets)
			}
		})
	}
}

func TestCollectionNameLookupRevalidatesInfo(t *testing.T) {
	t.Parallel()
	mismatched := collectionTestFields()
	mismatched["id"] = collectionTestOtherID
	for _, tc := range []struct {
		name, body, contains string
		status               int
	}{
		{"forbidden after listing", `{"ok":false,"status":403,"error":"authorization_error"}`, "HTTP 403", 403},
		{"deleted after listing", `{"ok":false,"status":404,"error":"not_found"}`, "not found", 404},
		{"malformed info", `{"ok":true,"data":{}}`, "malformed collection", 200},
		{"mismatched info ID", userTestJSON(t, map[string]any{"ok": true, "data": mismatched}), "different ID", 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var calls []string
			api := collectionTestAdminClient(t, func(w http.ResponseWriter, req *http.Request) {
				calls = append(calls, req.URL.Path)
				switch req.URL.Path {
				case "/api/collections.list":
					collectionTestListOffset(t, w, req)
					groupTestEncode(t, w, collectionTestList([]client.Collection{*collectionTestCollection()}, 0, 100, 1))
				case "/api/collections.info":
					collectionTestAssertIDBody(t, w, req)
					w.WriteHeader(tc.status)
					groupTestWrite(t, w, tc.body)
				default:
					t.Errorf("unexpected lookup request: %s", req.URL.Path)
					w.WriteHeader(http.StatusBadRequest)
				}
			})
			response := collectionTestLookup(t, api, collectionLookupModel{Name: types.StringValue("Engineering")})
			groupTestDiagnostics(t, response.Diagnostics, tc.contains)
			collectionTestAssertNoState(t, response.State)
			if !reflect.DeepEqual(calls, []string{"/api/collections.list", "/api/collections.info"}) {
				t.Fatalf("failed info revalidation caused fallback or a write: %v", calls)
			}
		})
	}
}

func TestCollectionNameLookupRejectsMalformedUnmatchedCollection(t *testing.T) {
	t.Parallel()
	// A bad unrelated record on the same page must not be skipped either.
	fields := collectionTestFields()
	fields["id"], fields["name"], fields["permission"] = collectionTestOtherID, "Unrelated", "owner"
	api := collectionTestAdminClient(t, func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/api/collections.list" {
			t.Errorf("malformed list caused info read: %s", req.URL.Path)
		}
		collectionTestListOffset(t, w, req)
		page := collectionTestList([]client.Collection{}, 0, 100, 2)
		page["data"] = []any{collectionTestCollection(), fields}
		groupTestEncode(t, w, page)
	})
	response := collectionTestLookup(t, api, collectionLookupModel{Name: types.StringValue("Engineering")})
	groupTestDiagnostics(t, response.Diagnostics, "unsupported default permission")
	collectionTestAssertNoState(t, response.State)
	if strings.Contains(response.Diagnostics.Errors()[0].Detail(), "no collection found") {
		t.Fatal("malformed list was reported as absence")
	}
}
