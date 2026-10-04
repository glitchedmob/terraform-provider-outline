// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/glitchedmob/terraform-provider-outline/internal/client"
	"github.com/google/uuid"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	resourceschema "github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"github.com/oapi-codegen/nullable"
	"golang.org/x/time/rate"
)

const groupTestID = "a32c2ee6-fbde-4654-841b-0eabdc71b812"
const groupTestOtherID = "b32c2ee6-fbde-4654-841b-0eabdc71b813"
const groupTestKey = "group-unit-secret"

func groupTestPointer[T any](value T) *T { return &value }

func groupTestGroup() *client.Group {
	return &client.Group{
		Id: groupTestPointer(uuid.MustParse(groupTestID)), Name: groupTestPointer("Engineering"),
		Description: nullable.NewNullNullable[string](), DisableMentions: groupTestPointer(false),
	}
}

func groupTestModel() groupModel {
	return groupModel{
		ID: types.StringValue(groupTestID), Name: types.StringValue("Engineering"),
		Description: types.StringValue(""), DisableMentions: types.BoolValue(false),
	}
}

func groupTestClient(t *testing.T, handler http.HandlerFunc) *apiClient {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost || (!strings.HasPrefix(req.URL.Path, "/api/groups.") && req.URL.Path != "/api/auth.info") || req.URL.RawQuery != "" {
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
	// Most tests concern response handling, not pacing. Keep the real transport.
	api.httpClient.Transport.(*bearerTransport).limiter = rate.NewLimiter(rate.Inf, 1)
	return api
}

func groupTestPlan(t *testing.T, r resource.Resource, model groupModel) tfsdk.Plan {
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

func groupTestConfig(t *testing.T, d datasource.DataSource, model groupModel) tfsdk.Config {
	t.Helper()
	var schema datasource.SchemaResponse
	d.Schema(t.Context(), datasource.SchemaRequest{}, &schema)
	if schema.Diagnostics.HasError() {
		t.Fatal(schema.Diagnostics)
	}
	state := tfsdk.State{Schema: schema.Schema}
	if diagnostics := state.Set(t.Context(), &model); diagnostics.HasError() {
		t.Fatal(diagnostics)
	}
	return tfsdk.Config{Schema: schema.Schema, Raw: state.Raw}
}

func groupTestStateModel(t *testing.T, state tfsdk.State) groupModel {
	t.Helper()
	var model groupModel
	if diagnostics := state.Get(t.Context(), &model); diagnostics.HasError() {
		t.Fatal(diagnostics)
	}
	return model
}

func groupTestEncode(t *testing.T, w io.Writer, value any) {
	t.Helper()
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Errorf("encode fixture: %v", err)
	}
}

func groupTestDecode(t *testing.T, w http.ResponseWriter, req *http.Request, body any) bool {
	t.Helper()
	if err := json.NewDecoder(req.Body).Decode(body); err != nil {
		t.Errorf("decode %s: %v", req.URL.Path, err)
		w.WriteHeader(http.StatusBadRequest)
		return false
	}
	return true
}

func groupTestListOffset(t *testing.T, w http.ResponseWriter, req *http.Request) (int, bool) {
	t.Helper()
	var body map[string]any
	if !groupTestDecode(t, w, req, &body) {
		return 0, false
	}
	offset, ok := body["offset"].(float64)
	// Check the wire shape, including the deprecated name filter, without
	// referring to deprecated fields in the generated request type.
	if !ok || offset < 0 || offset != float64(int(offset)) || !reflect.DeepEqual(body, map[string]any{"limit": float64(100), "offset": offset}) {
		t.Errorf("list must contain only limit and offset: %v", body)
		w.WriteHeader(http.StatusBadRequest)
		return 0, false
	}
	return int(offset), true
}

func groupTestWrite(t *testing.T, w io.Writer, body string) {
	t.Helper()
	if _, err := io.WriteString(w, body); err != nil {
		t.Errorf("write fixture: %v", err)
	}
}

func groupTestEnvelope(group *client.Group) any {
	return struct {
		Ok     bool          `json:"ok"`
		Status int           `json:"status"`
		Data   *client.Group `json:"data"`
	}{true, http.StatusOK, group}
}

func groupTestList(groups []client.Group, offset, limit, total int) any {
	return struct {
		Ok         bool                      `json:"ok"`
		Status     int                       `json:"status"`
		Data       map[string][]client.Group `json:"data"`
		Pagination client.PaginationResponse `json:"pagination"`
	}{true, http.StatusOK, map[string][]client.Group{"groups": groups}, client.PaginationResponse{
		Offset: &offset, Limit: &limit, Total: &total,
		NextPath: groupTestPointer("https://do-not-follow.invalid/api/groups.list?offset=999"),
	}}
}

func groupTestDiagnostics(t *testing.T, diagnostics diag.Diagnostics, contains string) {
	t.Helper()
	if !diagnostics.HasError() || !strings.Contains(diagnostics.Errors()[0].Detail(), contains) {
		t.Fatalf("expected error containing %q, got %v", contains, diagnostics)
	}
	for _, diagnostic := range diagnostics {
		if strings.Contains(diagnostic.Detail(), groupTestKey) {
			t.Fatal("diagnostic leaked the API key")
		}
	}
}

func TestGroupLifecycle(t *testing.T) {
	t.Parallel()
	current := groupTestGroup()
	var calls []string
	r := &groupResource{api: groupTestClient(t, func(w http.ResponseWriter, req *http.Request) {
		calls = append(calls, req.URL.Path)
		switch req.URL.Path {
		case "/api/groups.create":
			var body client.GroupsCreateJSONRequestBody
			if !groupTestDecode(t, w, req, &body) {
				return
			}
			if body.Name != "Engineering" || body.DisableMentions == nil || !*body.DisableMentions || body.ExternalId != nil {
				t.Errorf("create body: %+v", body)
			}
			current.DisableMentions = body.DisableMentions
		case "/api/groups.update":
			var body client.GroupsUpdateJSONRequestBody
			if !groupTestDecode(t, w, req, &body) {
				return
			}
			if body.Id.String() != groupTestID || body.Name == nil || body.Description == nil || body.DisableMentions == nil || body.ExternalId != nil {
				t.Errorf("update omitted mutable fields or attempted externalId: %+v", body)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			current.Name, current.DisableMentions = body.Name, body.DisableMentions
			current.Description = nullable.NewNullableWithValue(*body.Description)
		case "/api/groups.info":
			var body client.GroupsInfoJSONRequestBody
			if !groupTestDecode(t, w, req, &body) {
				return
			}
			if body.Id.String() != groupTestID {
				t.Errorf("info ID: %s", body.Id)
			}
		case "/api/groups.delete":
			var body client.GroupsDeleteJSONRequestBody
			if !groupTestDecode(t, w, req, &body) {
				return
			}
			if body.Id.String() != groupTestID {
				t.Errorf("delete ID: %s", body.Id)
			}
			groupTestWrite(t, w, `{"ok":true,"status":200,"success":true}`)
			return
		default:
			t.Errorf("unexpected endpoint: %s", req.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		groupTestEncode(t, w, groupTestEnvelope(current))
	})}
	model := groupTestModel()
	model.ID, model.Description, model.DisableMentions = types.StringUnknown(), types.StringValue("Initial description"), types.BoolValue(true)
	plan := groupTestPlan(t, r, model)
	created := resource.CreateResponse{State: tfsdk.State{Schema: plan.Schema}}
	r.Create(t.Context(), resource.CreateRequest{Plan: plan}, &created)
	if created.Diagnostics.HasError() {
		t.Fatal(created.Diagnostics)
	}
	model.ID = types.StringValue(groupTestID)
	if got := groupTestStateModel(t, created.State); !reflect.DeepEqual(got, model) {
		t.Fatalf("create state: %+v, want %+v", got, model)
	}
	if !reflect.DeepEqual(calls, []string{"/api/groups.create", "/api/groups.update"}) {
		t.Fatalf("description must be set by a second write: %v", calls)
	}
	// Omitted optional fields reset to their defaults without replacing identity.
	model.Name, model.Description, model.DisableMentions = types.StringValue("Renamed"), types.StringValue(""), types.BoolValue(false)
	updated := resource.UpdateResponse{State: created.State}
	r.Update(t.Context(), resource.UpdateRequest{Plan: groupTestPlan(t, r, model), State: created.State}, &updated)
	if updated.Diagnostics.HasError() || !reflect.DeepEqual(groupTestStateModel(t, updated.State), model) {
		t.Fatalf("update: %v", updated.Diagnostics)
	}
	imported := resource.ImportStateResponse{State: tfsdk.State(groupTestPlan(t, r, groupModel{}))}
	r.ImportState(t.Context(), resource.ImportStateRequest{ID: groupTestID}, &imported)
	if imported.Diagnostics.HasError() || groupTestStateModel(t, imported.State).ID.ValueString() != groupTestID {
		t.Fatalf("import: %v", imported.Diagnostics)
	}
	read := resource.ReadResponse{State: imported.State}
	r.Read(t.Context(), resource.ReadRequest{State: imported.State}, &read)
	if read.Diagnostics.HasError() || !read.State.Raw.Equal(updated.State.Raw) {
		t.Fatalf("import/read mismatch: %v", read.Diagnostics)
	}
	current.Name, current.Description, current.DisableMentions = groupTestPointer("Drift"), nullable.NewNullNullable[string](), groupTestPointer(true)
	r.Read(t.Context(), resource.ReadRequest{State: read.State}, &read)
	got := groupTestStateModel(t, read.State)
	if read.Diagnostics.HasError() || got.Name.ValueString() != "Drift" || got.Description.ValueString() != "" || got.Description.IsNull() || !got.DisableMentions.ValueBool() {
		t.Fatalf("drift/null description: %v %+v", read.Diagnostics, got)
	}
	deleted := resource.DeleteResponse{State: read.State}
	r.Delete(t.Context(), resource.DeleteRequest{State: read.State}, &deleted)
	if deleted.Diagnostics.HasError() {
		t.Fatal(deleted.Diagnostics)
	}
	wantCalls := []string{"/api/groups.create", "/api/groups.update", "/api/groups.info", "/api/groups.update", "/api/groups.info", "/api/groups.info", "/api/groups.info", "/api/groups.delete"}
	if !reflect.DeepEqual(calls, wantCalls) {
		t.Fatalf("unexpected requests/replayed writes: %v", calls)
	}
}

func TestGroupCreateEmptyDescriptionDoesNotUpdate(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	r := &groupResource{api: groupTestClient(t, func(w http.ResponseWriter, req *http.Request) {
		calls.Add(1)
		if req.URL.Path != "/api/groups.create" {
			t.Errorf("empty description caused an unnecessary update: %s", req.URL.Path)
		}
		var body map[string]any
		if !groupTestDecode(t, w, req, &body) {
			return
		}
		if !reflect.DeepEqual(body, map[string]any{"name": "Engineering", "disableMentions": false}) {
			t.Errorf("create wire shape: %v", body)
		}
		groupTestEncode(t, w, groupTestEnvelope(groupTestGroup()))
	})}
	model := groupTestModel()
	model.ID = types.StringUnknown()
	plan := groupTestPlan(t, r, model)
	response := resource.CreateResponse{State: tfsdk.State{Schema: plan.Schema}}
	r.Create(t.Context(), resource.CreateRequest{Plan: plan}, &response)
	if response.Diagnostics.HasError() || calls.Load() != 1 {
		t.Fatalf("create: %v, calls=%d", response.Diagnostics, calls.Load())
	}
	if got := groupTestStateModel(t, response.State); !reflect.DeepEqual(got, groupTestModel()) {
		t.Fatalf("state: %+v", got)
	}
}

func TestGroupCreateDescriptionFailureRetainsID(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"forbidden", 403, `{"error":"authorization_error","message":"denied"}`},
		{"not found", 404, `{}`},
		{"rate limited", 429, `{"error":"rate_limit_exceeded"}`},
		{"server", 500, `{}`},
		{"malformed JSON", 200, `{`},
		{"missing data", 200, `{"ok":true,"status":200}`},
		{"replacement ID", 200, `{"ok":true,"data":{"id":"` + groupTestOtherID + `","name":"Engineering","disableMentions":false}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var calls []string
			r := &groupResource{api: groupTestClient(t, func(w http.ResponseWriter, req *http.Request) {
				calls = append(calls, req.URL.Path)
				if req.URL.Path == "/api/groups.create" {
					groupTestEncode(t, w, groupTestEnvelope(groupTestGroup()))
					return
				}
				if req.URL.Path != "/api/groups.update" {
					t.Errorf("unexpected endpoint: %s", req.URL.Path)
				}
				w.Header().Set("Retry-After", "2")
				w.WriteHeader(tc.status)
				groupTestWrite(t, w, tc.body)
			})}
			model := groupTestModel()
			model.ID, model.Description = types.StringUnknown(), types.StringValue("Desired description")
			plan := groupTestPlan(t, r, model)
			response := resource.CreateResponse{State: tfsdk.State{Schema: plan.Schema}}
			r.Create(t.Context(), resource.CreateRequest{Plan: plan}, &response)
			groupTestDiagnostics(t, response.Diagnostics, "retained in state")
			got := groupTestStateModel(t, response.State)
			if got.ID.ValueString() != groupTestID || got.Name.ValueString() != "Engineering" || got.Description.ValueString() != "" {
				t.Fatalf("lost created identity or saved a failed update: %+v", got)
			}
			if !reflect.DeepEqual(calls, []string{"/api/groups.create", "/api/groups.update"}) {
				t.Fatalf("write replayed: %v", calls)
			}
		})
	}
}

func TestGroupMalformedResponses(t *testing.T) {
	t.Parallel()
	valid := `{"id":"` + groupTestID + `","name":"Engineering","disableMentions":false}`
	for _, operation := range []string{"create", "read", "update", "data source"} {
		for _, tc := range []struct {
			name, body, contentType string
			retainID                bool
		}{
			{"empty", ``, "", false},
			{"invalid JSON", `{"secret":"` + groupTestKey + `",`, "", false},
			{"wrong JSON type", `[]`, "", false},
			{"null envelope", `null`, "", false},
			{"empty envelope", `{}`, "", false},
			{"missing ok", `{"data":` + valid + `}`, "", true},
			{"false ok", `{"ok":false,"data":` + valid + `}`, "", true},
			{"null ok", `{"ok":null,"data":` + valid + `}`, "", true},
			{"bad envelope status", `{"ok":true,"status":201,"data":` + valid + `}`, "", true},
			{"missing data", `{"ok":true}`, "", false},
			{"null data", `{"ok":true,"data":null}`, "", false},
			{"missing ID", `{"ok":true,"data":{"name":"Engineering","disableMentions":false}}`, "", false},
			{"null ID", `{"ok":true,"data":{"id":null,"name":"Engineering","disableMentions":false}}`, "", false},
			{"zero ID", `{"ok":true,"data":{"id":"00000000-0000-0000-0000-000000000000","name":"Engineering","disableMentions":false}}`, "", false},
			{"invalid UUID", `{"ok":true,"data":{"id":"` + groupTestKey + `","name":"Engineering","disableMentions":false}}`, "", false},
			{"missing name", `{"ok":true,"data":{"id":"` + groupTestID + `","disableMentions":false}}`, "", true},
			{"null name", `{"ok":true,"data":{"id":"` + groupTestID + `","name":null,"disableMentions":false}}`, "", true},
			{"missing mentions", `{"ok":true,"data":{"id":"` + groupTestID + `","name":"Engineering"}}`, "", true},
			{"null mentions", `{"ok":true,"data":{"id":"` + groupTestID + `","name":"Engineering","disableMentions":null}}`, "", true},
			{"wrong description type", `{"ok":true,"data":{"id":"` + groupTestID + `","name":"Engineering","disableMentions":false,"description":{}}}`, "", false},
			{"not JSON content type", `{"ok":true,"data":` + valid + `}`, "text/plain", false},
		} {
			t.Run(operation+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				var writes atomic.Int32
				api := groupTestClient(t, func(w http.ResponseWriter, req *http.Request) {
					if operation == "update" && req.URL.Path == "/api/groups.info" {
						groupTestEncode(t, w, groupTestEnvelope(groupTestGroup()))
						return
					}
					if req.URL.Path == "/api/groups.create" || req.URL.Path == "/api/groups.update" {
						writes.Add(1)
					}
					if tc.contentType != "" {
						w.Header().Set("Content-Type", tc.contentType)
					}
					groupTestWrite(t, w, tc.body)
				})
				r := &groupResource{api: api}
				plan := groupTestPlan(t, r, groupTestModel())
				state := tfsdk.State(plan)
				var diagnostics diag.Diagnostics
				switch operation {
				case "create":
					response := resource.CreateResponse{State: tfsdk.State{Schema: plan.Schema}}
					r.Create(t.Context(), resource.CreateRequest{Plan: plan}, &response)
					diagnostics = response.Diagnostics
					if tc.retainID && groupTestStateModel(t, response.State).ID.ValueString() != groupTestID {
						t.Error("malformed post-create response lost the known ID")
					}
				case "read":
					response := resource.ReadResponse{State: state}
					r.Read(t.Context(), resource.ReadRequest{State: state}, &response)
					diagnostics = response.Diagnostics
					if !response.State.Raw.Equal(state.Raw) {
						t.Error("malformed response changed resource state")
					}
				case "update":
					response := resource.UpdateResponse{State: state}
					r.Update(t.Context(), resource.UpdateRequest{Plan: plan, State: state}, &response)
					diagnostics = response.Diagnostics
					if !response.State.Raw.Equal(state.Raw) {
						t.Error("malformed update changed state")
					}
				case "data source":
					d := &groupDataSource{api: api}
					config := groupTestConfig(t, d, groupModel{ID: types.StringValue(groupTestID)})
					response := datasource.ReadResponse{State: tfsdk.State{Schema: config.Schema}}
					d.Read(t.Context(), datasource.ReadRequest{Config: config}, &response)
					diagnostics = response.Diagnostics
				}
				groupTestDiagnostics(t, diagnostics, "")
				if (operation == "create" || operation == "update") && writes.Load() != 1 {
					t.Fatalf("write replayed: %d", writes.Load())
				}
			})
		}
	}
}

func TestGroupHTTPStatusHandling(t *testing.T) {
	t.Parallel()
	for _, operation := range []string{"read", "update", "find"} {
		for _, status := range []int{201, 204, 301, 400, 401, 403, 404, 409, 429, 500, 503} {
			t.Run(fmt.Sprintf("%s/%d", operation, status), func(t *testing.T) {
				t.Parallel()
				var calls atomic.Int32
				api := groupTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
					calls.Add(1)
					w.Header().Set("Retry-After", "7")
					w.Header().Set("Location", "/api/groups.info")
					w.WriteHeader(status)
					if status != http.StatusNoContent {
						groupTestEncode(t, w, client.Error{Error: groupTestPointer("api_error"), Message: groupTestPointer("Bearer " + groupTestKey + " denied")})
					}
				})
				var group *client.Group
				var err error
				switch operation {
				case "read":
					group, err = api.readGroup(t.Context(), uuid.MustParse(groupTestID))
				case "update":
					group, err = api.updateGroup(t.Context(), uuid.MustParse(groupTestID), groupTestModel())
				case "find":
					group, err = api.findGroup(t.Context(), "Engineering")
				}
				wantCalls := int32(1)
				if operation == "read" && status == http.StatusNotFound {
					wantCalls = 2 // The failed auth.info check cannot establish absence.
				}
				if err == nil || group != nil || calls.Load() != wantCalls {
					t.Fatalf("status accepted or write replayed: group=%v err=%v calls=%d", group, err, calls.Load())
				}
				if errors.Is(err, errNotFound) {
					t.Fatalf("raw HTTP error authorized absence: %v", err)
				}
				if strings.Contains(err.Error(), groupTestKey) {
					t.Fatal("API error leaked bearer token")
				}
				if !strings.Contains(err.Error(), fmt.Sprintf("HTTP %d", status)) {
					t.Fatalf("missing HTTP status: %v", err)
				}
				if status == 429 && (!strings.Contains(err.Error(), `Retry-After="7"`) || !strings.Contains(err.Error(), "no automatic retry")) {
					t.Fatalf("rate limit diagnostic: %v", err)
				}
				if status == 403 && !strings.Contains(err.Error(), "[REDACTED] denied") {
					t.Fatalf("missing sanitized API detail: %v", err)
				}
			})
		}
	}
}

func TestGroupResourceAbsenceAndForbidden(t *testing.T) {
	t.Parallel()
	for _, status := range []int{401, 403, 404, 429, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			t.Parallel()
			var calls atomic.Int32
			r := &groupResource{api: groupTestClient(t, func(w http.ResponseWriter, req *http.Request) {
				calls.Add(1)
				if req.URL.Path != "/api/groups.info" && (status != 404 || req.URL.Path != "/api/auth.info") {
					t.Errorf("failed preflight must not delete: %s", req.URL.Path)
				}
				w.WriteHeader(status)
				groupTestEncode(t, w, client.Error{Message: groupTestPointer("denied " + groupTestKey)})
			})}
			state := tfsdk.State(groupTestPlan(t, r, groupTestModel()))
			read := resource.ReadResponse{State: state}
			r.Read(t.Context(), resource.ReadRequest{State: state}, &read)
			if !read.Diagnostics.HasError() || !read.State.Raw.Equal(state.Raw) {
				t.Fatalf("read removed inaccessible/unverified state: %v", read.Diagnostics)
			}
			deleted := resource.DeleteResponse{State: state}
			r.Delete(t.Context(), resource.DeleteRequest{State: state}, &deleted)
			wantCalls := int32(2)
			if status == 404 {
				wantCalls = 4 // Each read also verifies admin identity, unsuccessfully.
			}
			if !deleted.Diagnostics.HasError() || !deleted.State.Raw.Equal(state.Raw) || calls.Load() != wantCalls {
				t.Fatalf("delete preflight: %v calls=%d", deleted.Diagnostics, calls.Load())
			}
			d := &groupDataSource{api: r.api}
			config := groupTestConfig(t, d, groupModel{ID: types.StringValue(groupTestID)})
			lookup := datasource.ReadResponse{State: tfsdk.State{Schema: config.Schema}}
			d.Read(t.Context(), datasource.ReadRequest{Config: config}, &lookup)
			groupTestDiagnostics(t, lookup.Diagnostics, "")
			if status != 404 {
				groupTestDiagnostics(t, read.Diagnostics, fmt.Sprintf("HTTP %d", status))
				groupTestDiagnostics(t, deleted.Diagnostics, fmt.Sprintf("HTTP %d", status))
			}
		})
	}
}

type groupTestAbsenceReply struct {
	status      int
	body        any
	contentType string
}

type groupTestAbsenceCase struct {
	name        string
	replies     []groupTestAbsenceReply // auth.info, then each groups.list page
	absent      bool
	verifyError string
}

func groupTestAbsenceCases() []groupTestAbsenceCase {
	admin := groupTestAbsenceReply{body: `{"ok":true,"status":200,"data":{"user":{"role":"admin"}}}`}
	empty := groupTestAbsenceReply{body: groupTestList([]client.Group{}, 0, 100, 0)}
	other := *groupTestGroup()
	other.Id = groupTestPointer(uuid.MustParse(groupTestOtherID))
	first := make([]client.Group, 100)
	for i := range first {
		first[i] = *groupTestGroup()
		first[i].Id = groupTestPointer(uuid.MustParse(fmt.Sprintf("10000000-0000-4000-8000-%012d", i+1)))
	}
	firstPage := groupTestAbsenceReply{body: groupTestList(first, 0, 100, 101)}
	lastPage := groupTestAbsenceReply{body: groupTestList([]client.Group{other}, 100, 100, 101)}
	foundFirst := append([]client.Group(nil), first...)
	foundFirst[0] = *groupTestGroup()
	cases := []groupTestAbsenceCase{
		{name: "admin empty workspace", replies: []groupTestAbsenceReply{admin, empty}, absent: true},
		{name: "admin without optional status", replies: []groupTestAbsenceReply{{body: `{"ok":true,"data":{"user":{"role":"admin"}}}`}, empty}, absent: true},
		{name: "same name but different ID", replies: []groupTestAbsenceReply{admin, {body: groupTestList([]client.Group{other}, 0, 100, 1)}}, absent: true},
		{name: "absent after last page", replies: []groupTestAbsenceReply{admin, firstPage, lastPage}, absent: true},
		{name: "ID on first page", replies: []groupTestAbsenceReply{admin, {body: groupTestList(foundFirst, 0, 100, 101)}, lastPage}},
		{name: "ID on later page", replies: []groupTestAbsenceReply{admin, firstPage, {body: groupTestList([]client.Group{*groupTestGroup()}, 100, 100, 101)}}},
	}
	for _, tc := range []struct {
		name, body, verifyError string
		status                  int
		contentType             string
	}{
		{"member", `{"ok":true,"data":{"user":{"role":"member"}}}`, "only an admin", 200, ""},
		{"viewer", `{"ok":true,"data":{"user":{"role":"viewer"}}}`, "only an admin", 200, ""},
		{"guest", `{"ok":true,"data":{"user":{"role":"guest"}}}`, "only an admin", 200, ""},
		{"unknown role", `{"ok":true,"data":{"user":{"role":"superadmin"}}}`, "only an admin", 200, ""},
		{"missing role", `{"ok":true,"data":{"user":{}}}`, "only an admin", 200, ""},
		{"null role", `{"ok":true,"data":{"user":{"role":null}}}`, "only an admin", 200, ""},
		{"missing user", `{"ok":true,"data":{}}`, "only an admin", 200, ""},
		{"null user", `{"ok":true,"data":{"user":null}}`, "only an admin", 200, ""},
		{"missing data", `{"ok":true}`, "only an admin", 200, ""},
		{"null data", `{"ok":true,"data":null}`, "only an admin", 200, ""},
		{"missing ok", `{"data":{"user":{"role":"admin"}}}`, "response envelope", 200, ""},
		{"null ok", `{"ok":null,"data":{"user":{"role":"admin"}}}`, "response envelope", 200, ""},
		{"false ok", `{"ok":false,"data":{"user":{"role":"admin"}}}`, "response envelope", 200, ""},
		{"bad envelope status", `{"ok":true,"status":403,"data":{"user":{"role":"admin"}}}`, "response envelope", 200, ""},
		{"malformed JSON", `{"secret":"` + groupTestKey + `",`, "could not be decoded", 200, ""},
		{"malformed role", `{"ok":true,"data":{"user":{"role":42}}}`, "could not be decoded", 200, ""},
		{"wrong JSON type", `[]`, "could not be decoded", 200, ""},
		{"wrong content type", `{"ok":true,"data":{"user":{"role":"admin"}}}`, "missing JSON", 200, "text/plain"},
		{"unauthorized", `{"error":"authentication_error","message":"` + groupTestKey + `"}`, "HTTP 401", 401, ""},
		{"forbidden", `{"error":"authorization_error"}`, "HTTP 403", 403, ""},
		{"not found", `{}`, "HTTP 404", 404, ""},
		{"rate limited", `{}`, "HTTP 429", 429, ""},
		{"server error", `{}`, "HTTP 500", 500, ""},
	} {
		cases = append(cases, groupTestAbsenceCase{
			name: "auth " + tc.name, verifyError: tc.verifyError,
			replies: []groupTestAbsenceReply{{status: tc.status, body: tc.body, contentType: tc.contentType}},
		})
	}
	listWithPagination := func(groups []client.Group, pagination any) any {
		return map[string]any{"ok": true, "status": 200, "data": map[string]any{"groups": groups}, "pagination": pagination}
	}
	for _, tc := range []struct {
		name, verifyError string
		reply             groupTestAbsenceReply
	}{
		{"forbidden", "HTTP 403", groupTestAbsenceReply{status: 403, body: `{"error":"authorization_error","message":"` + groupTestKey + `"}`}},
		{"not found", "HTTP 404", groupTestAbsenceReply{status: 404, body: `{}`}},
		{"rate limited", "HTTP 429", groupTestAbsenceReply{status: 429, body: `{}`}},
		{"server error", "HTTP 500", groupTestAbsenceReply{status: 500, body: `{}`}},
		{"malformed JSON", "could not be decoded", groupTestAbsenceReply{body: `{"secret":"` + groupTestKey + `",`}},
		{"wrong content type", "missing JSON", groupTestAbsenceReply{body: empty.body, contentType: "text/plain"}},
		{"false ok", "response envelope", groupTestAbsenceReply{body: `{"ok":false,"data":{"groups":[]},"pagination":{"offset":0,"limit":100,"total":0}}`}},
		{"bad envelope status", "response envelope", groupTestAbsenceReply{body: `{"ok":true,"status":403,"data":{"groups":[]},"pagination":{"offset":0,"limit":100,"total":0}}`}},
		{"missing groups", "missing groups", groupTestAbsenceReply{body: `{"ok":true,"data":{},"pagination":{"offset":0,"limit":100,"total":0}}`}},
		{"null groups", "missing groups", groupTestAbsenceReply{body: `{"ok":true,"data":{"groups":null},"pagination":{"offset":0,"limit":100,"total":0}}`}},
		{"null pagination", "pagination", groupTestAbsenceReply{body: listWithPagination([]client.Group{}, nil)}},
		{"missing offset", "pagination", groupTestAbsenceReply{body: listWithPagination([]client.Group{}, map[string]int{"limit": 100, "total": 0})}},
		{"missing limit", "pagination", groupTestAbsenceReply{body: listWithPagination([]client.Group{}, map[string]int{"offset": 0, "total": 0})}},
		{"missing total", "pagination", groupTestAbsenceReply{body: listWithPagination([]client.Group{}, map[string]int{"offset": 0, "limit": 100})}},
		{"wrong offset", "pagination", groupTestAbsenceReply{body: groupTestList([]client.Group{}, 100, 100, 0)}},
		{"zero limit", "pagination", groupTestAbsenceReply{body: groupTestList([]client.Group{}, 0, 0, 0)}},
		{"negative limit", "pagination", groupTestAbsenceReply{body: groupTestList([]client.Group{}, 0, -1, 0)}},
		{"excessive limit", "pagination", groupTestAbsenceReply{body: groupTestList([]client.Group{}, 0, 101, 0)}},
		{"negative total", "pagination", groupTestAbsenceReply{body: groupTestList([]client.Group{}, 0, 100, -1)}},
		{"count exceeds limit", "pagination", groupTestAbsenceReply{body: groupTestList(first[:2], 0, 1, 2)}},
		{"count exceeds total", "pagination", groupTestAbsenceReply{body: groupTestList([]client.Group{other}, 0, 100, 0)}},
		{"empty incomplete page", "pagination", groupTestAbsenceReply{body: groupTestList([]client.Group{}, 0, 100, 101)}},
		{"partial incomplete page", "pagination", groupTestAbsenceReply{body: groupTestList([]client.Group{other}, 0, 100, 101)}},
		{"duplicate ID", "duplicate group", groupTestAbsenceReply{body: groupTestList([]client.Group{other, other}, 0, 100, 2)}},
		{"malformed group", "malformed group", groupTestAbsenceReply{body: groupTestList([]client.Group{{}}, 0, 100, 1)}},
	} {
		cases = append(cases, groupTestAbsenceCase{name: "list " + tc.name, replies: []groupTestAbsenceReply{admin, tc.reply}, verifyError: tc.verifyError})
	}
	invalid := other
	invalid.DisableMentions = nil
	for _, tc := range []struct {
		name, verifyError string
		reply             groupTestAbsenceReply
	}{
		{"forbidden", "HTTP 403", groupTestAbsenceReply{status: 403, body: `{"error":"authorization_error"}`}},
		{"not found", "HTTP 404", groupTestAbsenceReply{status: 404, body: `{}`}},
		{"malformed JSON", "could not be decoded", groupTestAbsenceReply{body: `{`}},
		{"wrong offset", "pagination", groupTestAbsenceReply{body: groupTestList([]client.Group{other}, 0, 100, 101)}},
		{"missing pagination", "pagination", groupTestAbsenceReply{body: `{"ok":true,"data":{"groups":[]}}`}},
		{"empty incomplete page", "pagination", groupTestAbsenceReply{body: groupTestList([]client.Group{}, 100, 100, 101)}},
		{"total below offset", "pagination", groupTestAbsenceReply{body: groupTestList([]client.Group{}, 100, 100, 99)}},
		{"total changed before final page", "pagination", groupTestAbsenceReply{body: groupTestList([]client.Group{}, 100, 100, 100)}},
		{"duplicate ID", "duplicate group", groupTestAbsenceReply{body: groupTestList(first[:1], 100, 100, 101)}},
		{"malformed group", "malformed group", groupTestAbsenceReply{body: groupTestList([]client.Group{invalid}, 100, 100, 101)}},
	} {
		cases = append(cases, groupTestAbsenceCase{name: "later list " + tc.name, replies: []groupTestAbsenceReply{admin, firstPage, tc.reply}, verifyError: tc.verifyError})
	}
	return cases
}

func groupTestAbsenceClient(t *testing.T, tc groupTestAbsenceCase, withInfo bool) *apiClient {
	t.Helper()
	var calls atomic.Int32
	api := groupTestClient(t, func(w http.ResponseWriter, req *http.Request) {
		index := int(calls.Add(1)) - 1
		if withInfo {
			if index == 0 {
				if req.URL.Path != "/api/groups.info" {
					t.Errorf("expected info preflight, got %s", req.URL.Path)
				}
				var body map[string]any
				if !groupTestDecode(t, w, req, &body) {
					return
				}
				if !reflect.DeepEqual(body, map[string]any{"id": groupTestID}) {
					t.Errorf("info body: %v", body)
				}
				w.WriteHeader(http.StatusForbidden)
				groupTestWrite(t, w, `{"ok":false,"status":403,"error":"authorization_error","message":"denied `+groupTestKey+`"}`)
				return
			}
			index--
		}
		if index >= len(tc.replies) {
			t.Errorf("fallback made an extra request or attempted a write: %s", req.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if index == 0 {
			if req.URL.Path != "/api/auth.info" {
				t.Errorf("expected auth verification before listing, got %s", req.URL.Path)
			}
			if body, err := io.ReadAll(req.Body); err != nil || len(body) != 0 {
				t.Errorf("auth body must be empty: %q, %v", body, err)
			}
		} else {
			if req.URL.Path != "/api/groups.list" {
				t.Errorf("fallback followed nextPath or attempted a write: %s", req.URL.Path)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			offset, ok := groupTestListOffset(t, w, req)
			if !ok {
				return
			}
			if want := (index - 1) * 100; offset != want {
				t.Errorf("list offset=%d, want %d", offset, want)
			}
		}
		reply := tc.replies[index]
		if reply.contentType != "" {
			w.Header().Set("Content-Type", reply.contentType)
		}
		status := reply.status
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
		if body, ok := reply.body.(string); ok {
			groupTestWrite(t, w, body)
		} else {
			groupTestEncode(t, w, reply.body)
		}
	})
	t.Cleanup(func() {
		want := len(tc.replies)
		if withInfo {
			want++
		}
		if got := int(calls.Load()); got != want {
			t.Errorf("fallback stopped too early or replayed a request: calls=%d, want=%d", got, want)
		}
	})
	return api
}

func TestGroupAuthorizationErrorAbsence(t *testing.T) {
	t.Parallel()
	for _, tc := range groupTestAbsenceCases() {
		for _, operation := range []string{"readGroup", "confirmGroupAbsent", "resource read", "resource delete", "resource update", "data source"} {
			t.Run(tc.name+"/"+operation, func(t *testing.T) {
				t.Parallel()
				api := groupTestAbsenceClient(t, tc, operation != "confirmGroupAbsent")
				id := uuid.MustParse(groupTestID)
				if operation == "confirmGroupAbsent" {
					absent, err := api.confirmGroupAbsent(t.Context(), id)
					if absent != tc.absent || (err != nil) != (tc.verifyError != "") {
						t.Fatalf("absence=%t, error=%v", absent, err)
					}
					if err != nil && (!strings.Contains(err.Error(), tc.verifyError) || strings.Contains(err.Error(), groupTestKey)) {
						t.Fatalf("wrong or unredacted verification error: %v", err)
					}
					return
				}
				if operation == "readGroup" {
					group, err := api.readGroup(t.Context(), id)
					if group != nil || err == nil || errors.Is(err, errNotFound) != tc.absent {
						t.Fatalf("group=%+v, error=%v, want absent=%t", group, err, tc.absent)
					}
					if strings.Contains(err.Error(), groupTestKey) {
						t.Fatalf("fallback error leaked credentials: %v", err)
					}
					if !tc.absent && (!strings.Contains(err.Error(), "HTTP 403 Forbidden") || !strings.Contains(err.Error(), "authorization_error")) {
						t.Fatalf("fallback lost the original forbidden error: %v", err)
					}
					if tc.verifyError != "" && (!strings.Contains(err.Error(), "cannot establish group absence") || !strings.Contains(err.Error(), tc.verifyError)) {
						t.Fatalf("fallback lost the verification error: %v", err)
					}
					return
				}
				r := &groupResource{api: api}
				state := tfsdk.State(groupTestPlan(t, r, groupTestModel()))
				var diagnostics diag.Diagnostics
				var gotState tfsdk.State
				switch operation {
				case "resource read":
					response := resource.ReadResponse{State: state}
					r.Read(t.Context(), resource.ReadRequest{State: state}, &response)
					diagnostics, gotState = response.Diagnostics, response.State
					if tc.absent {
						if diagnostics.HasError() || !gotState.Raw.IsNull() {
							t.Fatalf("confirmed absence did not remove resource state: %v", diagnostics)
						}
						return
					}
				case "resource delete":
					response := resource.DeleteResponse{State: state}
					r.Delete(t.Context(), resource.DeleteRequest{State: state}, &response)
					diagnostics, gotState = response.Diagnostics, response.State
				case "resource update":
					plan := groupTestModel()
					plan.Name = types.StringValue("Must not be written")
					response := resource.UpdateResponse{State: state}
					r.Update(t.Context(), resource.UpdateRequest{Plan: groupTestPlan(t, r, plan), State: state}, &response)
					diagnostics, gotState = response.Diagnostics, response.State
				case "data source":
					d := &groupDataSource{api: api}
					config := groupTestConfig(t, d, groupModel{ID: types.StringValue(groupTestID)})
					state = tfsdk.State{Schema: config.Schema}
					if diagnostics := state.Set(t.Context(), groupTestModel()); diagnostics.HasError() {
						t.Fatal(diagnostics)
					}
					response := datasource.ReadResponse{State: state}
					d.Read(t.Context(), datasource.ReadRequest{Config: config}, &response)
					diagnostics, gotState = response.Diagnostics, response.State
				}
				if !gotState.Raw.Equal(state.Raw) {
					t.Fatal("failed or already-absent operation changed state")
				}
				if tc.absent && operation == "resource delete" {
					if diagnostics.HasError() {
						t.Fatalf("confirmed absence blocked delete: %v", diagnostics)
					}
					return
				}
				want := "HTTP 403 Forbidden"
				if tc.absent {
					want = errNotFound.Error()
				}
				groupTestDiagnostics(t, diagnostics, want)
			})
		}
	}
}

func TestGroupAuthorizationErrorFallbackRequiresTyped403(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, body, contentType string
		status                  int
	}{
		{"different forbidden error", `{"error":"permission_error"}`, "", 403},
		{"message only", `{"message":"authorization_error"}`, "", 403},
		{"missing error", `{}`, "", 403},
		{"null error", `{"error":null}`, "", 403},
		{"malformed JSON", `{"error":"authorization_error",`, "", 403},
		{"wrong error type", `{"error":42}`, "", 403},
		{"wrong content type", `{"error":"authorization_error"}`, "text/plain", 403},
		{"unauthorized", `{"error":"authorization_error"}`, "", 401},
		{"server error", `{"error":"authorization_error"}`, "", 500},
		{"unsuccessful HTTP 200 envelope", `{"ok":false,"status":403,"error":"authorization_error"}`, "", 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var calls atomic.Int32
			api := groupTestClient(t, func(w http.ResponseWriter, req *http.Request) {
				calls.Add(1)
				if req.URL.Path != "/api/groups.info" {
					t.Errorf("untyped or non-403 error triggered fallback: %s", req.URL.Path)
				}
				if tc.contentType != "" {
					w.Header().Set("Content-Type", tc.contentType)
				}
				w.WriteHeader(tc.status)
				groupTestWrite(t, w, tc.body)
			})
			group, err := api.readGroup(t.Context(), uuid.MustParse(groupTestID))
			if group != nil || err == nil || errors.Is(err, errNotFound) || calls.Load() != 1 {
				t.Fatalf("error became absence or triggered fallback: group=%+v, err=%v, calls=%d", group, err, calls.Load())
			}
		})
	}
}

func TestGroupWalkGroupsServerPageLimit(t *testing.T) {
	t.Parallel()
	groups := []client.Group{*groupTestGroup(), *groupTestGroup(), *groupTestGroup()}
	groups[1].Id = groupTestPointer(uuid.MustParse(groupTestOtherID))
	groups[2].Id = groupTestPointer(uuid.MustParse("10000000-0000-4000-8000-000000000001"))
	var offsets []int
	api := groupTestClient(t, func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/api/groups.list" {
			t.Errorf("walk followed nextPath or called wrong endpoint: %s", req.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		offset, ok := groupTestListOffset(t, w, req)
		if !ok {
			return
		}
		offsets = append(offsets, offset)
		if offset >= len(groups) {
			t.Errorf("extra page: offset=%d", offset)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		groupTestEncode(t, w, groupTestList(groups[offset:offset+1], offset, 1, len(groups)))
	})
	var visited []uuid.UUID
	err := api.walkGroups(t.Context(), func(group *client.Group) error {
		visited = append(visited, *group.Id)
		return nil
	})
	wantIDs := []uuid.UUID{*groups[0].Id, *groups[1].Id, *groups[2].Id}
	if err != nil || !reflect.DeepEqual(visited, wantIDs) || !reflect.DeepEqual(offsets, []int{0, 1, 2}) {
		t.Fatalf("walk did not use the server's effective page limit: error=%v, IDs=%v, offsets=%v", err, visited, offsets)
	}
}

func TestGroupWalkGroupsVisitorFailure(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	api := groupTestClient(t, func(w http.ResponseWriter, req *http.Request) {
		calls.Add(1)
		if req.URL.Path != "/api/groups.list" {
			t.Errorf("unexpected endpoint: %s", req.URL.Path)
		}
		if _, ok := groupTestListOffset(t, w, req); !ok {
			return
		}
		groupTestEncode(t, w, groupTestList([]client.Group{*groupTestGroup()}, 0, 1, 2))
	})
	visitorErr := errors.New("stop visiting")
	var visits int
	err := api.walkGroups(t.Context(), func(group *client.Group) error {
		visits++
		if group.Id.String() != groupTestID {
			t.Errorf("visited the wrong group: %+v", group)
		}
		return visitorErr
	})
	if !errors.Is(err, visitorErr) || visits != 1 || calls.Load() != 1 {
		t.Fatalf("visitor failure was lost or pagination continued: %v, visits=%d, calls=%d", err, visits, calls.Load())
	}
}

func TestGroupDeleteResponseValidation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		status int
		body   string
		ok     bool
	}{
		{"success", 200, `{"ok":true,"status":200,"success":true}`, true},
		{"missing optional status", 200, `{"ok":true,"success":true}`, true},
		{"unverified delete route 404", 404, `{}`, false},
		{"forbidden", 403, `{"message":"denied"}`, false},
		{"rate limited", 429, `{}`, false},
		{"server", 500, `{}`, false},
		{"unexpected no content", 204, ``, false},
		{"missing JSON", 200, ``, false},
		{"malformed JSON", 200, `{`, false},
		{"null envelope", 200, `null`, false},
		{"empty envelope", 200, `{}`, false},
		{"missing ok", 200, `{"success":true}`, false},
		{"false ok", 200, `{"ok":false,"success":true}`, false},
		{"bad status", 200, `{"ok":true,"status":403,"success":true}`, false},
		{"missing success", 200, `{"ok":true}`, false},
		{"false success", 200, `{"ok":true,"success":false}`, false},
		{"null success", 200, `{"ok":true,"success":null}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var calls []string
			r := &groupResource{api: groupTestClient(t, func(w http.ResponseWriter, req *http.Request) {
				calls = append(calls, req.URL.Path)
				if req.URL.Path == "/api/groups.info" {
					groupTestEncode(t, w, groupTestEnvelope(groupTestGroup()))
					return
				}
				if req.URL.Path != "/api/groups.delete" {
					t.Errorf("unexpected endpoint: %s", req.URL.Path)
				}
				w.WriteHeader(tc.status)
				groupTestWrite(t, w, tc.body)
			})}
			state := tfsdk.State(groupTestPlan(t, r, groupTestModel()))
			response := resource.DeleteResponse{State: state}
			r.Delete(t.Context(), resource.DeleteRequest{State: state}, &response)
			if response.Diagnostics.HasError() == tc.ok || !response.State.Raw.Equal(state.Raw) {
				t.Fatalf("delete: %v", response.Diagnostics)
			}
			want := []string{"/api/groups.info", "/api/groups.delete"}
			if tc.status == http.StatusNotFound {
				want = append(want, "/api/groups.info")
				groupTestDiagnostics(t, response.Diagnostics, "HTTP 404")
			}
			if !reflect.DeepEqual(calls, want) {
				t.Fatalf("delete replayed: %v", calls)
			}
		})
	}
}

func TestGroupRejectsReplacementIdentity(t *testing.T) {
	t.Parallel()
	for _, operation := range []string{"read", "update preflight", "update response", "delete", "data source"} {
		t.Run(operation, func(t *testing.T) {
			t.Parallel()
			var calls atomic.Int32
			api := groupTestClient(t, func(w http.ResponseWriter, req *http.Request) {
				calls.Add(1)
				group := groupTestGroup()
				if operation != "update response" || req.URL.Path == "/api/groups.update" {
					group.Id = groupTestPointer(uuid.MustParse(groupTestOtherID))
				}
				groupTestEncode(t, w, groupTestEnvelope(group))
			})
			r := &groupResource{api: api}
			plan := groupTestPlan(t, r, groupTestModel())
			state := tfsdk.State(plan)
			var diagnostics diag.Diagnostics
			var gotState tfsdk.State
			switch operation {
			case "read":
				response := resource.ReadResponse{State: state}
				r.Read(t.Context(), resource.ReadRequest{State: state}, &response)
				diagnostics, gotState = response.Diagnostics, response.State
			case "update preflight", "update response":
				response := resource.UpdateResponse{State: state}
				r.Update(t.Context(), resource.UpdateRequest{Plan: plan, State: state}, &response)
				diagnostics, gotState = response.Diagnostics, response.State
			case "delete":
				response := resource.DeleteResponse{State: state}
				r.Delete(t.Context(), resource.DeleteRequest{State: state}, &response)
				diagnostics, gotState = response.Diagnostics, response.State
			case "data source":
				d := &groupDataSource{api: api}
				config := groupTestConfig(t, d, groupModel{ID: types.StringValue(groupTestID)})
				response := datasource.ReadResponse{State: tfsdk.State{Schema: config.Schema}}
				d.Read(t.Context(), datasource.ReadRequest{Config: config}, &response)
				diagnostics = response.Diagnostics
			}
			groupTestDiagnostics(t, diagnostics, "different ID")
			if operation != "data source" && !gotState.Raw.Equal(state.Raw) {
				t.Fatal("replacement identity overwrote state")
			}
			wantCalls := int32(1)
			if operation == "update response" {
				wantCalls = 2
			}
			if calls.Load() != wantCalls {
				t.Fatalf("unexpected requests: %d", calls.Load())
			}
		})
	}
}

func TestGroupExternalSyncSafety(t *testing.T) {
	t.Parallel()
	for _, link := range []string{"external ID", "external group", "both"} {
		for _, operation := range []string{"create", "read", "update", "delete", "data source"} {
			t.Run(link+"/"+operation, func(t *testing.T) {
				t.Parallel()
				group := groupTestGroup()
				if link != "external group" {
					group.ExternalId = nullable.NewNullableWithValue("idp-group")
				}
				if link != "external ID" {
					group.ExternalGroup = nullable.NewNullableWithValue(map[string]interface{}{})
				}
				var calls []string
				api := groupTestClient(t, func(w http.ResponseWriter, req *http.Request) {
					calls = append(calls, req.URL.Path)
					groupTestEncode(t, w, groupTestEnvelope(group))
				})
				r := &groupResource{api: api}
				model := groupTestModel()
				model.Description = types.StringValue("Must not be written")
				plan := groupTestPlan(t, r, model)
				state := tfsdk.State(plan)
				var diagnostics diag.Diagnostics
				switch operation {
				case "create":
					response := resource.CreateResponse{State: tfsdk.State{Schema: plan.Schema}}
					r.Create(t.Context(), resource.CreateRequest{Plan: plan}, &response)
					diagnostics = response.Diagnostics
					if groupTestStateModel(t, response.State).ID.ValueString() != groupTestID {
						t.Fatal("create lost identity before external-sync rejection")
					}
				case "read":
					response := resource.ReadResponse{State: state}
					r.Read(t.Context(), resource.ReadRequest{State: state}, &response)
					diagnostics = response.Diagnostics
					if !response.State.Raw.Equal(state.Raw) {
						t.Fatal("external read changed managed state")
					}
				case "update":
					response := resource.UpdateResponse{State: state}
					r.Update(t.Context(), resource.UpdateRequest{Plan: plan, State: state}, &response)
					diagnostics = response.Diagnostics
					if !response.State.Raw.Equal(state.Raw) {
						t.Fatal("external update changed state")
					}
				case "delete":
					response := resource.DeleteResponse{State: state}
					r.Delete(t.Context(), resource.DeleteRequest{State: state}, &response)
					diagnostics = response.Diagnostics
				case "data source":
					d := &groupDataSource{api: api}
					config := groupTestConfig(t, d, groupModel{ID: types.StringValue(groupTestID)})
					response := datasource.ReadResponse{State: tfsdk.State{Schema: config.Schema}}
					d.Read(t.Context(), datasource.ReadRequest{Config: config}, &response)
					if response.Diagnostics.HasError() || groupTestStateModel(t, response.State).ID.ValueString() != groupTestID {
						t.Fatalf("externally synchronized groups should remain readable by data source: %v", response.Diagnostics)
					}
				}
				if operation != "data source" {
					groupTestDiagnostics(t, diagnostics, "externally linked or synchronized")
				}
				want := "/api/groups.info"
				if operation == "create" {
					want = "/api/groups.create"
				}
				if !reflect.DeepEqual(calls, []string{want}) {
					t.Fatalf("wrote after external-sync rejection: %v", calls)
				}
			})
		}
	}
	for _, group := range []*client.Group{groupTestGroup(), {
		ExternalId: nullable.NewNullableWithValue(""), ExternalGroup: nullable.NewNullNullable[map[string]interface{}](),
	}} {
		if err := managedGroup(group); err != nil {
			t.Fatalf("empty/null external metadata must be allowed: %v", err)
		}
	}
}

func TestGroupDataSourceLookup(t *testing.T) {
	t.Parallel()
	for _, lookup := range []string{"ID", "name"} {
		t.Run(lookup, func(t *testing.T) {
			t.Parallel()
			var calls []string
			d := &groupDataSource{api: groupTestClient(t, func(w http.ResponseWriter, req *http.Request) {
				calls = append(calls, req.URL.Path)
				group := groupTestGroup()
				if req.URL.Path == "/api/groups.list" {
					offset, ok := groupTestListOffset(t, w, req)
					if !ok {
						return
					}
					if offset != 0 {
						t.Errorf("first list offset: %d", offset)
					}
					groupTestEncode(t, w, groupTestList([]client.Group{*group}, 0, 100, 1))
					return
				}
				if req.URL.Path != "/api/groups.info" {
					t.Errorf("unexpected endpoint: %s", req.URL.Path)
				}
				var body client.GroupsInfoJSONRequestBody
				if !groupTestDecode(t, w, req, &body) {
					return
				}
				if body.Id.String() != groupTestID {
					t.Errorf("info ID: %s", body.Id)
				}
				// Info, not the stale list preview, supplies the resulting attributes.
				group.Description, group.DisableMentions = nullable.NewNullableWithValue("Fresh description"), groupTestPointer(true)
				groupTestEncode(t, w, groupTestEnvelope(group))
			})}
			model := groupModel{ID: types.StringValue(groupTestID)}
			if lookup == "name" {
				model = groupModel{Name: types.StringValue("Engineering")}
			}
			config := groupTestConfig(t, d, model)
			response := datasource.ReadResponse{State: tfsdk.State{Schema: config.Schema}}
			d.Read(t.Context(), datasource.ReadRequest{Config: config}, &response)
			if response.Diagnostics.HasError() {
				t.Fatal(response.Diagnostics)
			}
			got := groupTestStateModel(t, response.State)
			if got.ID.ValueString() != groupTestID || got.Name.ValueString() != "Engineering" || got.Description.ValueString() != "Fresh description" || !got.DisableMentions.ValueBool() {
				t.Fatalf("lookup state: %+v", got)
			}
			want := []string{"/api/groups.info"}
			if lookup == "name" {
				want = []string{"/api/groups.list", "/api/groups.info"}
			}
			if !reflect.DeepEqual(calls, want) {
				t.Fatalf("unexpected requests: %v", calls)
			}
		})
	}
}

func TestGroupNameLookupScansEveryPage(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"first page match", "last page match", "duplicate name later", "duplicate ID later", "invalid group later", "case mismatch", "partial match", "no match"} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			first := make([]client.Group, 100)
			for i := range first {
				first[i] = *groupTestGroup()
				first[i].Id = groupTestPointer(uuid.MustParse(fmt.Sprintf("10000000-0000-4000-8000-%012d", i+1)))
				first[i].Name = groupTestPointer(fmt.Sprintf("Unrelated %d", i))
			}
			last := []client.Group{*groupTestGroup()}
			last[0].Id = groupTestPointer(uuid.MustParse(groupTestOtherID))
			last[0].Name = groupTestPointer("Unrelated tail")
			switch scenario {
			case "first page match", "duplicate name later", "duplicate ID later", "invalid group later":
				first[0] = *groupTestGroup()
			case "case mismatch":
				first[0].Name = groupTestPointer("engineering")
			case "partial match":
				first[0].Name = groupTestPointer("Engineering extended")
			}
			switch scenario {
			case "last page match":
				last[0] = *groupTestGroup()
			case "duplicate name later":
				last[0].Name = groupTestPointer("Engineering")
			case "duplicate ID later":
				last[0].Id = first[1].Id // Duplicate unrelated objects must also fail.
			case "invalid group later":
				last[0].DisableMentions = nil
			}
			var offsets []int
			var infos atomic.Int32
			api := groupTestClient(t, func(w http.ResponseWriter, req *http.Request) {
				if req.URL.Path == "/api/groups.info" {
					infos.Add(1)
					groupTestEncode(t, w, groupTestEnvelope(groupTestGroup()))
					return
				}
				if req.URL.Path != "/api/groups.list" {
					t.Errorf("followed nextPath or called wrong endpoint: %s", req.URL.Path)
					w.WriteHeader(http.StatusNotFound)
					return
				}
				offset, ok := groupTestListOffset(t, w, req)
				if !ok {
					return
				}
				offsets = append(offsets, offset)
				switch offset {
				case 0:
					groupTestEncode(t, w, groupTestList(first, 0, 100, 101))
				case 100:
					groupTestEncode(t, w, groupTestList(last, 100, 100, 101))
				default:
					t.Errorf("non-progressing or extra page: %d", offset)
					w.WriteHeader(http.StatusBadRequest)
				}
			})
			group, err := api.findGroup(t.Context(), "Engineering")
			wantError := scenario != "first page match" && scenario != "last page match"
			if (err != nil) != wantError || !reflect.DeepEqual(offsets, []int{0, 100}) {
				t.Fatalf("lookup: %v offsets=%v", err, offsets)
			}
			if !wantError && (group == nil || group.Id.String() != groupTestID || infos.Load() != 1) {
				t.Fatalf("missing info refresh: %+v info calls=%d", group, infos.Load())
			}
			if wantError && (group != nil || infos.Load() != 0) {
				t.Fatalf("failed lookup continued to info: %+v info calls=%d", group, infos.Load())
			}
			if scenario == "duplicate name later" && !strings.Contains(err.Error(), "ambiguous") {
				t.Fatalf("wrong ambiguity diagnostic: %v", err)
			}
		})
	}
}

func TestGroupListMalformedResponses(t *testing.T) {
	t.Parallel()
	valid := `{"id":"` + groupTestID + `","name":"Engineering","disableMentions":false}`
	pagination := `,"pagination":{"offset":0,"limit":100,"total":1}`
	for name, body := range map[string]string{
		"invalid JSON":          `{"secret":"` + groupTestKey + `",`,
		"null envelope":         `null`,
		"empty envelope":        `{}`,
		"missing ok":            `{"data":{"groups":[` + valid + `]}` + pagination + `}`,
		"false ok":              `{"ok":false,"data":{"groups":[` + valid + `]}` + pagination + `}`,
		"unsuccessful status":   `{"ok":true,"status":500,"data":{"groups":[` + valid + `]}` + pagination + `}`,
		"missing data":          `{"ok":true` + pagination + `}`,
		"null data":             `{"ok":true,"data":null` + pagination + `}`,
		"missing groups":        `{"ok":true,"data":{}` + pagination + `}`,
		"null groups":           `{"ok":true,"data":{"groups":null}` + pagination + `}`,
		"wrong groups type":     `{"ok":true,"data":{"groups":{}}` + pagination + `}`,
		"invalid trailing item": `{"ok":true,"data":{"groups":[` + valid + `,{}]}` + pagination + `}`,
		"ambiguous same page":   `{"ok":true,"data":{"groups":[` + valid + `,{"id":"` + groupTestOtherID + `","name":"Engineering","disableMentions":false}]},"pagination":{"offset":0,"limit":100,"total":2}}`,
		"duplicate same page":   `{"ok":true,"data":{"groups":[` + valid + `,` + valid + `]},"pagination":{"offset":0,"limit":100,"total":2}}`,
		"missing pagination":    `{"ok":true,"data":{"groups":[` + valid + `]}}`,
		"null pagination":       `{"ok":true,"data":{"groups":[` + valid + `]},"pagination":null}`,
		"empty pagination":      `{"ok":true,"data":{"groups":[` + valid + `]},"pagination":{}}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var calls atomic.Int32
			d := &groupDataSource{api: groupTestClient(t, func(w http.ResponseWriter, req *http.Request) {
				calls.Add(1)
				if req.URL.Path != "/api/groups.list" {
					t.Errorf("malformed list continued lookup: %s", req.URL.Path)
				}
				groupTestWrite(t, w, body)
			})}
			config := groupTestConfig(t, d, groupModel{Name: types.StringValue("Engineering")})
			response := datasource.ReadResponse{State: tfsdk.State{Schema: config.Schema}}
			d.Read(t.Context(), datasource.ReadRequest{Config: config}, &response)
			groupTestDiagnostics(t, response.Diagnostics, "")
			if calls.Load() != 1 {
				t.Fatalf("malformed list replayed: %d", calls.Load())
			}
		})
	}
}

func TestGroupPaginationValidation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                 string
		offset, limit, total int
		count                int
		wantError, more      bool
		next                 int
	}{
		{"empty list", 0, 100, 0, 0, false, false, 0},
		{"last full page", 0, 100, 100, 100, false, false, 0},
		{"last partial page", 100, 100, 101, 1, false, false, 0},
		{"continue", 0, 100, 101, 100, false, true, 100},
		{"zero limit", 0, 0, 0, 0, true, false, 0},
		{"negative limit", 0, -1, 1, 1, true, false, 0},
		{"excessive limit", 0, 101, 1, 1, true, false, 0},
		{"negative total", 0, 100, -1, 0, true, false, 0},
		{"too many groups", 0, 1, 2, 2, true, false, 0},
		{"empty intermediate page", 0, 100, 101, 0, true, false, 0},
		{"partial intermediate page", 0, 100, 101, 1, true, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var calls atomic.Int32
			groups := make([]client.Group, tc.count)
			for i := range groups {
				groups[i] = *groupTestGroup()
				groups[i].Id = groupTestPointer(uuid.MustParse(fmt.Sprintf("10000000-0000-4000-8000-%012d", i+1)))
				groups[i].Name = groupTestPointer("Unrelated")
			}
			api := groupTestClient(t, func(w http.ResponseWriter, req *http.Request) {
				calls.Add(1)
				if req.URL.Path != "/api/groups.list" {
					t.Errorf("unexpected endpoint: %s", req.URL.Path)
				}
				var body client.GroupsListJSONRequestBody
				if !groupTestDecode(t, w, req, &body) {
					return
				}
				if body.Offset != nil && *body.Offset > 0 {
					groupTestEncode(t, w, groupTestList([]client.Group{*groupTestGroup()}, 100, 100, 101))
					return
				}
				groupTestEncode(t, w, groupTestList(groups, 0, tc.limit, tc.total-tc.offset))
			})
			_, err := api.findGroup(t.Context(), "Not present")
			if err == nil || strings.Contains(err.Error(), "pagination") != tc.wantError {
				t.Fatalf("pagination error=%t: %v", tc.wantError, err)
			}
			wantCalls := int32(1)
			if tc.more {
				wantCalls = 2
			}
			if calls.Load() != wantCalls {
				t.Fatalf("pagination calls=%d want=%d", calls.Load(), wantCalls)
			}
			p := &client.PaginationResponse{Offset: &tc.offset, Limit: &tc.limit, Total: &tc.total}
			next, more, err := nextOffset(p, tc.offset, tc.count)
			if (err != nil) != tc.wantError || next != tc.next || more != tc.more {
				t.Fatalf("nextOffset: next=%d more=%t err=%v", next, more, err)
			}
		})
	}
	for name, p := range map[string]*client.PaginationResponse{
		"nil":            nil,
		"missing offset": {Limit: groupTestPointer(100), Total: groupTestPointer(1)},
		"missing limit":  {Offset: groupTestPointer(0), Total: groupTestPointer(1)},
		"missing total":  {Offset: groupTestPointer(0), Limit: groupTestPointer(100)},
		"wrong offset":   {Offset: groupTestPointer(100), Limit: groupTestPointer(100), Total: groupTestPointer(101)},
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := nextOffset(p, 0, 1); err == nil {
				t.Fatal("accepted malformed pagination")
			}
		})
	}
	maxInt := int(^uint(0) >> 1)
	p := &client.PaginationResponse{Offset: groupTestPointer(maxInt - 1), Limit: groupTestPointer(1), Total: &maxInt}
	if next, more, err := nextOffset(p, maxInt-1, 1); err != nil || more || next != 0 {
		t.Fatalf("last page overflow: %d %t %v", next, more, err)
	}
}

func TestGroupUUIDValidationBeforeRequests(t *testing.T) {
	t.Parallel()
	for _, id := range []string{"", "not-a-uuid", "00000000-0000-0000-0000-000000000000", strings.ToUpper(groupTestID), strings.ReplaceAll(groupTestID, "-", ""), "{" + groupTestID + "}", "urn:uuid:" + groupTestID, " " + groupTestID, groupTestID + " "} {
		t.Run(id, func(t *testing.T) {
			t.Parallel()
			if _, err := parseGroupID(id); err == nil {
				t.Fatalf("accepted noncanonical/nonzero ID %q", id)
			}
			r := &groupResource{api: groupTestClient(t, func(http.ResponseWriter, *http.Request) {
				t.Error("invalid ID reached Outline")
			})}
			model := groupTestModel()
			model.ID = types.StringValue(id)
			plan := groupTestPlan(t, r, model)
			state := tfsdk.State(plan)
			read := resource.ReadResponse{State: state}
			r.Read(t.Context(), resource.ReadRequest{State: state}, &read)
			groupTestDiagnostics(t, read.Diagnostics, "UUID")
			updated := resource.UpdateResponse{State: state}
			r.Update(t.Context(), resource.UpdateRequest{Plan: plan, State: state}, &updated)
			groupTestDiagnostics(t, updated.Diagnostics, "UUID")
			deleted := resource.DeleteResponse{State: state}
			r.Delete(t.Context(), resource.DeleteRequest{State: state}, &deleted)
			groupTestDiagnostics(t, deleted.Diagnostics, "UUID")
			imported := resource.ImportStateResponse{State: state}
			r.ImportState(t.Context(), resource.ImportStateRequest{ID: id}, &imported)
			groupTestDiagnostics(t, imported.Diagnostics, "UUID")
			d := &groupDataSource{api: r.api}
			config := groupTestConfig(t, d, groupModel{ID: types.StringValue(id)})
			lookup := datasource.ReadResponse{State: tfsdk.State{Schema: config.Schema}}
			d.Read(t.Context(), datasource.ReadRequest{Config: config}, &lookup)
			groupTestDiagnostics(t, lookup.Diagnostics, "UUID")
			for _, got := range []tfsdk.State{read.State, updated.State, deleted.State, imported.State} {
				if !got.Raw.Equal(state.Raw) {
					t.Fatal("invalid identity changed state")
				}
			}
		})
	}
	if got, err := parseGroupID(groupTestID); err != nil || got.String() != groupTestID {
		t.Fatalf("canonical ID rejected: %v", err)
	}
}

func TestGroupContextCancellation(t *testing.T) {
	t.Parallel()
	api := groupTestClient(t, func(http.ResponseWriter, *http.Request) { t.Error("canceled request reached server") })
	r := &groupResource{api: api}
	plan := groupTestPlan(t, r, groupTestModel())
	state := tfsdk.State(plan)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	created := resource.CreateResponse{State: tfsdk.State{Schema: plan.Schema}}
	r.Create(ctx, resource.CreateRequest{Plan: plan}, &created)
	groupTestDiagnostics(t, created.Diagnostics, "canceled")
	read := resource.ReadResponse{State: state}
	r.Read(ctx, resource.ReadRequest{State: state}, &read)
	groupTestDiagnostics(t, read.Diagnostics, "canceled")
	updated := resource.UpdateResponse{State: state}
	r.Update(ctx, resource.UpdateRequest{Plan: plan, State: state}, &updated)
	groupTestDiagnostics(t, updated.Diagnostics, "canceled")
	deleted := resource.DeleteResponse{State: state}
	r.Delete(ctx, resource.DeleteRequest{State: state}, &deleted)
	groupTestDiagnostics(t, deleted.Diagnostics, "canceled")
	for _, got := range []tfsdk.State{read.State, updated.State, deleted.State} {
		if !got.Raw.Equal(state.Raw) {
			t.Fatal("context cancellation lost resource state")
		}
	}
	for _, model := range []groupModel{{ID: types.StringValue(groupTestID)}, {Name: types.StringValue("Engineering")}} {
		d := &groupDataSource{api: api}
		config := groupTestConfig(t, d, model)
		lookup := datasource.ReadResponse{State: tfsdk.State{Schema: config.Schema}}
		d.Read(ctx, datasource.ReadRequest{Config: config}, &lookup)
		groupTestDiagnostics(t, lookup.Diagnostics, "canceled")
	}
	ctx, cancel = context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()
	if _, err := api.readGroup(ctx, uuid.MustParse(groupTestID)); err == nil || !strings.Contains(err.Error(), "deadline exceeded") {
		t.Fatalf("expired context: %v", err)
	}
}

func TestGroupInFlightCancellation(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	release := make(chan struct{})
	api := groupTestClient(t, func(_ http.ResponseWriter, req *http.Request) {
		close(started)
		select {
		case <-req.Context().Done():
		case <-release:
		}
	})
	defer close(release)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := api.readGroup(ctx, uuid.MustParse(groupTestID))
		result <- err
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("request did not reach local server")
	}
	cancel()
	select {
	case err := <-result:
		if err == nil || !strings.Contains(err.Error(), "request canceled") || strings.Contains(err.Error(), groupTestKey) {
			t.Fatalf("in-flight cancellation: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("request ignored cancellation")
	}
}

func TestGroupDefaultRateLimiterPacesRequests(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		groupTestEncode(t, w, groupTestEnvelope(groupTestGroup()))
	}))
	defer server.Close()
	api, err := newAPIClient(server.URL+"/api", groupTestKey, 5, "unit")
	if err != nil {
		t.Fatal(err)
	}
	limiter := api.httpClient.Transport.(*bearerTransport).limiter
	if limiter == nil || limiter.Limit() != rate.Limit(5) || limiter.Burst() != 1 {
		t.Fatal("group API calls must share a five-request-per-second limiter without bursts")
	}
	if _, err := api.readGroup(t.Context(), uuid.MustParse(groupTestID)); err != nil {
		t.Fatal(err)
	}
	// Drain any token accrued while the first response was parsed. Avoid an
	// elapsed-time assertion, which can fail on a busy CI host.
	limiter.SetLimit(rate.Every(time.Hour))
	_ = limiter.Allow()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if _, err := api.readGroup(ctx, uuid.MustParse(groupTestID)); err == nil || calls.Load() != 1 {
		t.Fatalf("empty limiter allowed an unpaced HTTP call: %v calls=%d", err, calls.Load())
	}
}

func TestGroupRateLimiterHonorsCancellation(t *testing.T) {
	t.Parallel()
	var requests atomic.Int32
	api := groupTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		groupTestEncode(t, w, groupTestEnvelope(groupTestGroup()))
	})
	api.httpClient.Transport.(*bearerTransport).limiter = rate.NewLimiter(rate.Every(time.Second), 1)
	if _, err := api.readGroup(t.Context(), uuid.MustParse(groupTestID)); err != nil {
		t.Fatal(err)
	}
	r := &groupResource{api: api}
	plan := groupTestPlan(t, r, groupTestModel())
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan resource.CreateResponse, 1)
	go func() {
		response := resource.CreateResponse{State: tfsdk.State{Schema: plan.Schema}}
		r.Create(ctx, resource.CreateRequest{Plan: plan}, &response)
		result <- response
	}()
	// The empty bucket prevents the non-idempotent create from reaching HTTP.
	select {
	case response := <-result:
		t.Fatalf("empty limiter did not block: %v", response.Diagnostics)
	case <-time.After(20 * time.Millisecond):
	}
	cancel()
	select {
	case response := <-result:
		groupTestDiagnostics(t, response.Diagnostics, "canceled")
	case <-time.After(time.Second):
		t.Fatal("limiter ignored cancellation")
	}
	if requests.Load() != 1 {
		t.Fatalf("canceled create reached HTTP: %d requests", requests.Load())
	}
	ctx, cancel = context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if _, err := api.readGroup(ctx, uuid.MustParse(groupTestID)); err == nil || requests.Load() != 1 {
		t.Fatalf("limiter ignored deadline: %v requests=%d", err, requests.Load())
	}
}

func TestGroupWritesNeverReplay429(t *testing.T) {
	t.Parallel()
	for _, operation := range []string{"create", "update", "delete"} {
		t.Run(operation, func(t *testing.T) {
			t.Parallel()
			var calls []string
			r := &groupResource{api: groupTestClient(t, func(w http.ResponseWriter, req *http.Request) {
				calls = append(calls, req.URL.Path)
				if req.URL.Path == "/api/groups.info" {
					groupTestEncode(t, w, groupTestEnvelope(groupTestGroup()))
					return
				}
				w.Header().Set("Retry-After", "1")
				w.WriteHeader(http.StatusTooManyRequests)
				groupTestEncode(t, w, client.Error{Error: groupTestPointer("rate_limit_exceeded"), Message: groupTestPointer("Bearer " + groupTestKey)})
			})}
			plan := groupTestPlan(t, r, groupTestModel())
			state := tfsdk.State(plan)
			var diagnostics diag.Diagnostics
			switch operation {
			case "create":
				response := resource.CreateResponse{State: tfsdk.State{Schema: plan.Schema}}
				r.Create(t.Context(), resource.CreateRequest{Plan: plan}, &response)
				diagnostics = response.Diagnostics
			case "update":
				response := resource.UpdateResponse{State: state}
				r.Update(t.Context(), resource.UpdateRequest{Plan: plan, State: state}, &response)
				diagnostics = response.Diagnostics
				if !response.State.Raw.Equal(state.Raw) {
					t.Fatal("rate limited update changed state")
				}
			case "delete":
				response := resource.DeleteResponse{State: state}
				r.Delete(t.Context(), resource.DeleteRequest{State: state}, &response)
				diagnostics = response.Diagnostics
				if !response.State.Raw.Equal(state.Raw) {
					t.Fatal("rate limited delete changed state")
				}
			}
			groupTestDiagnostics(t, diagnostics, `HTTP 429 rate limited; Retry-After="1"; no automatic retry`)
			want := []string{"/api/groups." + operation}
			if operation != "create" {
				want = append([]string{"/api/groups.info"}, want...)
			}
			if !reflect.DeepEqual(calls, want) {
				t.Fatalf("non-idempotent write replayed: %v", calls)
			}
		})
	}
}

func TestGroupTransportFailureDoesNotReplayWrites(t *testing.T) {
	t.Parallel()
	for _, operation := range []string{"create", "update", "delete"} {
		t.Run(operation, func(t *testing.T) {
			t.Parallel()
			var requests atomic.Int32
			r := &groupResource{api: groupTestClient(t, func(w http.ResponseWriter, req *http.Request) {
				if req.URL.Path == "/api/groups.info" {
					groupTestEncode(t, w, groupTestEnvelope(groupTestGroup()))
					return
				}
				requests.Add(1)
				if req.URL.Path != "/api/groups."+operation {
					t.Errorf("unexpected endpoint: %s", req.URL.Path)
				}
				// Simulate a committed server-side write whose response connection dies.
				if _, err := io.Copy(io.Discard, req.Body); err != nil {
					t.Errorf("consume request: %v", err)
				}
				connection, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Errorf("hijack: %v", err)
					return
				}
				if err := connection.Close(); err != nil {
					t.Errorf("close response connection: %v", err)
				}
			})}
			plan := groupTestPlan(t, r, groupTestModel())
			state := tfsdk.State(plan)
			var diagnostics diag.Diagnostics
			switch operation {
			case "create":
				response := resource.CreateResponse{State: tfsdk.State{Schema: plan.Schema}}
				r.Create(t.Context(), resource.CreateRequest{Plan: plan}, &response)
				diagnostics = response.Diagnostics
			case "update":
				response := resource.UpdateResponse{State: state}
				r.Update(t.Context(), resource.UpdateRequest{Plan: plan, State: state}, &response)
				diagnostics = response.Diagnostics
				if !response.State.Raw.Equal(state.Raw) {
					t.Fatal("transport failure changed update state")
				}
			case "delete":
				response := resource.DeleteResponse{State: state}
				r.Delete(t.Context(), resource.DeleteRequest{State: state}, &response)
				diagnostics = response.Diagnostics
				if !response.State.Raw.Equal(state.Raw) {
					t.Fatal("transport failure changed delete state")
				}
			}
			groupTestDiagnostics(t, diagnostics, "request failed")
			if requests.Load() != 1 {
				t.Fatalf("ambiguous non-idempotent %s replayed: %d", operation, requests.Load())
			}
		})
	}
}

func TestGroupAPIErrorRedactionAndBounds(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, body, contentType string
	}{
		{"JSON API error", `{"error":"authorization_error","message":"Bearer ` + groupTestKey + `"}`, "application/json"},
		{"raw HTML error", `<html>Bearer ` + groupTestKey + `</html>`, "text/html"},
		{"malformed error JSON", `{"error":"` + groupTestKey + `",`, "application/json"},
		{"long API detail", `{"error":"api_error","message":"` + strings.Repeat("x", 600) + groupTestKey + `"}`, "application/json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			api := groupTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				w.WriteHeader(http.StatusForbidden)
				groupTestWrite(t, w, tc.body)
			})
			_, err := api.readGroup(t.Context(), uuid.MustParse(groupTestID))
			if err == nil || strings.Contains(err.Error(), groupTestKey) || strings.Contains(err.Error(), api.baseURL) || len(err.Error()) > 600 {
				t.Fatalf("unbounded or unsanitized error: %v", err)
			}
		})
	}
	api := &apiClient{apiKey: groupTestKey}
	response := &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": []string{groupTestKey + strings.Repeat("x", 200)}}}
	err := api.checkResponse("groups.create", response, []byte(`{"message":"`+groupTestKey+`"}`), nil)
	if err == nil || strings.Contains(err.Error(), groupTestKey) || !strings.Contains(err.Error(), "[REDACTED]") || len(err.Error()) > 200 {
		t.Fatalf("Retry-After redaction/truncation: %v", err)
	}
	if err := api.checkResponse("groups.info", nil, nil, nil); err == nil || !strings.Contains(err.Error(), "missing HTTP response") {
		t.Fatalf("missing response accepted: %v", err)
	}
	if err := api.checkResponse("groups.info", nil, nil, fmt.Errorf("URL and token %s", groupTestKey)); err == nil || strings.Contains(err.Error(), groupTestKey) {
		t.Fatalf("transport error leaked secret: %v", err)
	}
}

func groupTestProtocolConfig(t *testing.T, values map[string]any) *tfprotov6.DynamicValue {
	t.Helper()
	attributes := map[string]tftypes.Type{"id": tftypes.String, "name": tftypes.String, "description": tftypes.String, "disable_mentions": tftypes.Bool}
	fields := make(map[string]tftypes.Value, len(attributes))
	for name, typ := range attributes {
		fields[name] = tftypes.NewValue(typ, values[name])
	}
	value := tftypes.NewValue(tftypes.Object{AttributeTypes: attributes}, fields)
	dynamic, err := tfprotov6.NewDynamicValue(value.Type(), value)
	if err != nil {
		t.Fatal(err)
	}
	return &dynamic
}

func TestGroupProtocolValidation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		dataSource bool
		values     map[string]any
		wantError  bool
	}{
		{"minimal resource", false, map[string]any{"name": "Engineering"}, false},
		{"all resource fields", false, map[string]any{"name": "Engineering", "description": "Managed", "disable_mentions": true}, false},
		{"name absent", false, map[string]any{}, true},
		{"name empty", false, map[string]any{"name": ""}, true},
		{"name whitespace", false, map[string]any{"name": " \t\n"}, true},
		{"name maximum", false, map[string]any{"name": strings.Repeat("x", 255)}, false},
		{"name too long", false, map[string]any{"name": strings.Repeat("x", 256)}, true},
		{"Unicode name maximum", false, map[string]any{"name": strings.Repeat("界", 255)}, false},
		{"Unicode name too long", false, map[string]any{"name": strings.Repeat("界", 256)}, true},
		{"description maximum", false, map[string]any{"name": "Engineering", "description": strings.Repeat("x", 2000)}, false},
		{"description too long", false, map[string]any{"name": "Engineering", "description": strings.Repeat("x", 2001)}, true},
		{"Unicode description maximum", false, map[string]any{"name": "Engineering", "description": strings.Repeat("界", 2000)}, false},
		{"Unicode description too long", false, map[string]any{"name": "Engineering", "description": strings.Repeat("界", 2001)}, true},
		{"unknown resource fields", false, map[string]any{"name": tftypes.UnknownValue, "description": tftypes.UnknownValue, "disable_mentions": tftypes.UnknownValue}, false},
		{"configured computed ID", false, map[string]any{"id": groupTestID, "name": "Engineering"}, true},
		{"data ID", true, map[string]any{"id": groupTestID}, false},
		{"data name", true, map[string]any{"name": "Engineering"}, false},
		{"data neither", true, map[string]any{}, true},
		{"data both", true, map[string]any{"id": groupTestID, "name": "Engineering"}, true},
		{"data empty ID", true, map[string]any{"id": ""}, true},
		{"data malformed ID", true, map[string]any{"id": "group"}, true},
		{"data nil UUID", true, map[string]any{"id": "00000000-0000-0000-0000-000000000000"}, true},
		{"data uppercase ID", true, map[string]any{"id": strings.ToUpper(groupTestID)}, true},
		{"data compact UUID", true, map[string]any{"id": strings.ReplaceAll(groupTestID, "-", "")}, true},
		{"data invalid variant", true, map[string]any{"id": "a32c2ee6-fbde-4654-041b-0eabdc71b812"}, true},
		{"data invalid version", true, map[string]any{"id": "a32c2ee6-fbde-0654-841b-0eabdc71b812"}, true},
		{"data blank name", true, map[string]any{"name": " \t"}, true},
		{"data empty name", true, map[string]any{"name": ""}, true},
		{"data name too long", true, map[string]any{"name": strings.Repeat("x", 256)}, true},
		{"data unknown ID", true, map[string]any{"id": tftypes.UnknownValue}, false},
		{"data unknown name", true, map[string]any{"name": tftypes.UnknownValue}, false},
		{"data computed description", true, map[string]any{"id": groupTestID, "description": "not configurable"}, true},
		{"data computed mentions", true, map[string]any{"id": groupTestID, "disable_mentions": false}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			server := providerserver.NewProtocol6(New("unit")())()
			config := groupTestProtocolConfig(t, tc.values)
			var diagnostics []*tfprotov6.Diagnostic
			if tc.dataSource {
				response, err := server.ValidateDataResourceConfig(t.Context(), &tfprotov6.ValidateDataResourceConfigRequest{TypeName: "outline_group", Config: config})
				if err != nil {
					t.Fatal(err)
				}
				diagnostics = response.Diagnostics
			} else {
				response, err := server.ValidateResourceConfig(t.Context(), &tfprotov6.ValidateResourceConfigRequest{TypeName: "outline_group", Config: config})
				if err != nil {
					t.Fatal(err)
				}
				diagnostics = response.Diagnostics
			}
			if protocolHasError(diagnostics) != tc.wantError {
				var details []string
				for _, diagnostic := range diagnostics {
					details = append(details, diagnostic.Summary+": "+diagnostic.Detail)
				}
				t.Fatalf("want validation error=%t, got %s", tc.wantError, strings.Join(details, "\n"))
			}
		})
	}
}

func TestGroupSchemaAndConfigure(t *testing.T) {
	t.Parallel()
	r := NewGroupResource().(*groupResource)
	d := NewGroupDataSource().(*groupDataSource)
	var resourceMetadata resource.MetadataResponse
	r.Metadata(t.Context(), resource.MetadataRequest{ProviderTypeName: "outline"}, &resourceMetadata)
	var dataMetadata datasource.MetadataResponse
	d.Metadata(t.Context(), datasource.MetadataRequest{ProviderTypeName: "outline"}, &dataMetadata)
	if resourceMetadata.TypeName != "outline_group" || dataMetadata.TypeName != "outline_group" {
		t.Fatal("wrong group type names")
	}
	var schema resource.SchemaResponse
	r.Schema(t.Context(), resource.SchemaRequest{}, &schema)
	id := schema.Schema.Attributes["id"].(resourceschema.StringAttribute)
	name := schema.Schema.Attributes["name"].(resourceschema.StringAttribute)
	description := schema.Schema.Attributes["description"].(resourceschema.StringAttribute)
	mentions := schema.Schema.Attributes["disable_mentions"].(resourceschema.BoolAttribute)
	if schema.Diagnostics.HasError() || len(schema.Schema.Attributes) != 4 || !id.Computed || id.Optional || id.Required || len(id.PlanModifiers) != 1 || !name.Required || name.Computed || name.Optional || !description.Optional || !description.Computed || description.Default == nil || !mentions.Optional || !mentions.Computed || mentions.Default == nil {
		t.Fatalf("unexpected resource schema: %+v", schema)
	}
	server := providerserver.NewProtocol6(New("unit")())()
	protocol, err := server.GetProviderSchema(t.Context(), &tfprotov6.GetProviderSchemaRequest{})
	if err != nil || protocolHasError(protocol.Diagnostics) {
		t.Fatalf("protocol schema: %v %v", protocol, err)
	}
	for label, schema := range map[string]*tfprotov6.Schema{"resource": protocol.ResourceSchemas["outline_group"], "data source": protocol.DataSourceSchemas["outline_group"]} {
		if schema == nil || schema.Block == nil || len(schema.Block.Attributes) != 4 || schema.Block.Description == "" {
			t.Fatalf("missing %s schema", label)
		}
		for _, attribute := range schema.Block.Attributes {
			wantType := tftypes.String
			if attribute.Name == "disable_mentions" {
				wantType = tftypes.Bool
			}
			if !attribute.Type.Equal(wantType) || attribute.Sensitive || attribute.Description == "" {
				t.Fatalf("wrong protocol attribute: %+v", attribute)
			}
			if label == "data source" && (!attribute.Computed || attribute.Required || attribute.Optional != (attribute.Name == "id" || attribute.Name == "name")) {
				t.Fatalf("data source flags: %+v", attribute)
			}
		}
	}
	api := groupTestClient(t, func(http.ResponseWriter, *http.Request) { t.Error("configure must not contact Outline") })
	for _, tc := range []struct {
		name string
		data any
		bad  bool
	}{
		{"nil", nil, false}, {"wrong type", "not a client", true}, {"typed nil", (*apiClient)(nil), true}, {"valid client", api, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, d := &groupResource{}, &groupDataSource{}
			var resourceResponse resource.ConfigureResponse
			r.Configure(t.Context(), resource.ConfigureRequest{ProviderData: tc.data}, &resourceResponse)
			var dataResponse datasource.ConfigureResponse
			d.Configure(t.Context(), datasource.ConfigureRequest{ProviderData: tc.data}, &dataResponse)
			if resourceResponse.Diagnostics.HasError() != tc.bad || dataResponse.Diagnostics.HasError() != tc.bad {
				t.Fatalf("configure: %v %v", resourceResponse.Diagnostics, dataResponse.Diagnostics)
			}
			if tc.name == "valid client" && (r.api != api || d.api != api) {
				t.Fatal("configured client was not retained")
			}
		})
	}
}

func TestGroupProtocolPlanDefaultsAndStableID(t *testing.T) {
	t.Parallel()
	for _, existing := range []bool{false, true} {
		t.Run(fmt.Sprint(existing), func(t *testing.T) {
			t.Parallel()
			server := providerserver.NewProtocol6(New("unit")())()
			var prior *tfprotov6.DynamicValue
			if existing {
				prior = groupTestProtocolConfig(t, map[string]any{"id": groupTestID, "name": "Old name", "description": "Old description", "disable_mentions": true})
			} else {
				var schema resource.SchemaResponse
				(&groupResource{}).Schema(t.Context(), resource.SchemaRequest{}, &schema)
				typ := schema.Schema.Type().TerraformType(t.Context())
				dynamic, err := tfprotov6.NewDynamicValue(typ, tftypes.NewValue(typ, nil))
				if err != nil {
					t.Fatal(err)
				}
				prior = &dynamic
			}
			response, err := server.PlanResourceChange(t.Context(), &tfprotov6.PlanResourceChangeRequest{
				TypeName: "outline_group", PriorState: prior,
				Config:           groupTestProtocolConfig(t, map[string]any{"name": "Engineering"}),
				ProposedNewState: groupTestProtocolConfig(t, map[string]any{"id": tftypes.UnknownValue, "name": "Engineering", "description": tftypes.UnknownValue, "disable_mentions": tftypes.UnknownValue}),
			})
			if err != nil || protocolHasError(response.Diagnostics) || response.PlannedState == nil || len(response.RequiresReplace) != 0 {
				t.Fatalf("plan caused an error or replacement: %v %v", response, err)
			}
			var schema resource.SchemaResponse
			(&groupResource{}).Schema(t.Context(), resource.SchemaRequest{}, &schema)
			value, err := response.PlannedState.Unmarshal(schema.Schema.Type().TerraformType(t.Context()))
			if err != nil {
				t.Fatal(err)
			}
			var attributes map[string]tftypes.Value
			if err := value.As(&attributes); err != nil {
				t.Fatal(err)
			}
			var description string
			var mentions bool
			if err := attributes["description"].As(&description); err != nil || description != "" {
				t.Fatalf("description default: %v %q", err, description)
			}
			if err := attributes["disable_mentions"].As(&mentions); err != nil || mentions {
				t.Fatalf("disable_mentions default: %v %t", err, mentions)
			}
			if existing {
				var id string
				if err := attributes["id"].As(&id); err != nil || id != groupTestID {
					t.Fatalf("UseStateForUnknown did not retain ID: %v %q", err, id)
				}
			} else if attributes["id"].IsKnown() {
				t.Fatal("new group ID should remain unknown until creation")
			}
		})
	}
}
