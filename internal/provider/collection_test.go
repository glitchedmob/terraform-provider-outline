// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/glitchedmob/terraform-provider-outline/internal/client"
	"github.com/google/uuid"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/oapi-codegen/nullable"
	"golang.org/x/time/rate"
)

const collectionTestID = groupTestID
const collectionTestOtherID = groupTestOtherID

func collectionTestCollection() *client.Collection {
	return &client.Collection{
		Id: groupTestPointer(uuid.MustParse(collectionTestID)), Name: groupTestPointer("Engineering"),
		Description: nullable.NewNullableWithValue(""), Permission: nullable.NewNullNullable[client.Permission](),
		Sharing: groupTestPointer(false), ArchivedAt: nullable.NewNullNullable[time.Time](), DeletedAt: nullable.NewNullNullable[time.Time](),
	}
}

func collectionTestModel() collectionModel {
	return collectionModel{
		ID: types.StringValue(collectionTestID), Name: types.StringValue("Engineering"), Description: types.StringValue(""),
		Permission: types.StringNull(), Sharing: types.BoolValue(false), AllowDestroy: types.BoolValue(false),
	}
}

func collectionTestClient(t *testing.T, handler http.HandlerFunc) *apiClient {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost || (!strings.HasPrefix(req.URL.Path, "/api/collections.") && req.URL.Path != "/api/auth.info") || req.URL.RawQuery != "" {
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

func collectionTestAdminClient(t *testing.T, handler http.HandlerFunc) *apiClient {
	t.Helper()
	return collectionTestClient(t, func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/api/auth.info" {
			groupTestEncode(t, w, userTestAuth(userTestOwner()))
			return
		}
		handler(w, req)
	})
}

func collectionTestPlan(t *testing.T, r resource.Resource, model collectionModel) tfsdk.Plan {
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

func collectionTestConfig(t *testing.T, d datasource.DataSource, model collectionLookupModel) tfsdk.Config {
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

func collectionTestStateModel(t *testing.T, state tfsdk.State) collectionModel {
	t.Helper()
	var model collectionModel
	if diagnostics := state.Get(t.Context(), &model); diagnostics.HasError() {
		t.Fatal(diagnostics)
	}
	return model
}

func collectionTestEnvelope(collection *client.Collection) map[string]any {
	return map[string]any{"ok": true, "status": http.StatusOK, "data": collection}
}

func collectionTestFields() map[string]any {
	return map[string]any{
		"id": collectionTestID, "name": "Engineering", "description": "", "permission": nil,
		"sharing": false, "archivedAt": nil, "deletedAt": nil,
	}
}

func collectionTestCreate(t *testing.T, r *collectionResource, model collectionModel) resource.CreateResponse {
	t.Helper()
	model.ID = types.StringUnknown()
	plan := collectionTestPlan(t, r, model)
	response := resource.CreateResponse{State: tfsdk.State{Schema: plan.Schema}}
	r.Create(t.Context(), resource.CreateRequest{Plan: plan}, &response)
	return response
}

func collectionTestAssertIDBody(t *testing.T, w http.ResponseWriter, req *http.Request) {
	t.Helper()
	var body map[string]any
	if groupTestDecode(t, w, req, &body) && !reflect.DeepEqual(body, map[string]any{"id": collectionTestID}) {
		t.Errorf("unexpected ID request: %v", body)
	}
}

func TestCollectionCreateWireDefaultsAndMarkdown(t *testing.T) {
	t.Parallel()
	for _, description := range []string{"", "# Landing page\n\n**Markdown**, [link](https://example.com), and 界."} {
		t.Run(description, func(t *testing.T) {
			t.Parallel()
			var calls []string
			r := &collectionResource{api: collectionTestClient(t, func(w http.ResponseWriter, req *http.Request) {
				calls = append(calls, req.URL.Path)
				if req.URL.Path == "/api/auth.info" {
					groupTestEncode(t, w, userTestAuth(userTestOwner()))
					return
				}
				if req.URL.Path != "/api/collections.create" {
					t.Errorf("unexpected create follow-up: %s", req.URL.Path)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				var body map[string]any
				if !groupTestDecode(t, w, req, &body) {
					return
				}
				want := map[string]any{"name": "Engineering", "description": description, "permission": nil, "sharing": false}
				if !reflect.DeepEqual(body, want) {
					t.Errorf("create wire body: got %v want %v", body, want)
				}
				collection := collectionTestCollection()
				collection.Description = nullable.NewNullableWithValue(description)
				groupTestEncode(t, w, collectionTestEnvelope(collection))
			})}
			model := collectionTestModel()
			model.Description = types.StringValue(description)
			response := collectionTestCreate(t, r, model)
			if response.Diagnostics.HasError() || collectionTestStateModel(t, response.State) != model {
				t.Fatalf("create state: %v %+v", response.Diagnostics, response.State)
			}
			if !reflect.DeepEqual(calls, []string{"/api/auth.info", "/api/collections.create"}) {
				t.Fatalf("unexpected calls: %v", calls)
			}
		})
	}
}

func TestCollectionUpdateAlwaysSendsPermissionAndResets(t *testing.T) {
	t.Parallel()
	for _, permission := range []types.String{types.StringValue("read_write"), types.StringValue("read"), types.StringNull()} {
		t.Run(permission.String(), func(t *testing.T) {
			t.Parallel()
			var calls []string
			r := &collectionResource{api: collectionTestClient(t, func(w http.ResponseWriter, req *http.Request) {
				calls = append(calls, req.URL.Path)
				switch req.URL.Path {
				case "/api/auth.info":
					groupTestEncode(t, w, userTestAuth(userTestOwner()))
				case "/api/collections.info":
					collectionTestAssertIDBody(t, w, req)
					collection := collectionTestCollection()
					collection.Permission = nullable.NewNullableWithValue(client.PermissionReadWrite)
					collection.Description = nullable.NewNullableWithValue("Old description")
					collection.Sharing = groupTestPointer(true)
					groupTestEncode(t, w, collectionTestEnvelope(collection))
				case "/api/collections.update":
					var body map[string]any
					if !groupTestDecode(t, w, req, &body) {
						return
					}
					var wirePermission any
					if !permission.IsNull() {
						wirePermission = permission.ValueString()
					}
					want := map[string]any{"id": collectionTestID, "name": "Renamed", "description": "", "permission": wirePermission, "sharing": false}
					if !reflect.DeepEqual(body, want) {
						t.Errorf("update wire body: got %v want %v", body, want)
					}
					collection := collectionTestCollection()
					collection.Name = groupTestPointer("Renamed")
					collection.Permission = collectionPermission(permission)
					groupTestEncode(t, w, collectionTestEnvelope(collection))
				default:
					t.Errorf("unexpected endpoint: %s", req.URL.Path)
					w.WriteHeader(http.StatusBadRequest)
				}
			})}
			prior := collectionTestModel()
			prior.Permission, prior.Description, prior.Sharing = types.StringValue("read_write"), types.StringValue("Old description"), types.BoolValue(true)
			prior.AllowDestroy = types.BoolValue(true)
			state := tfsdk.State(collectionTestPlan(t, r, prior))
			model := collectionTestModel()
			model.Name, model.Permission, model.AllowDestroy = types.StringValue("Renamed"), permission, types.BoolValue(true)
			plan := collectionTestPlan(t, r, model)
			response := resource.UpdateResponse{State: state}
			r.Update(t.Context(), resource.UpdateRequest{Plan: plan, State: state}, &response)
			if response.Diagnostics.HasError() || collectionTestStateModel(t, response.State) != model {
				t.Fatalf("update: %v %+v", response.Diagnostics, response.State)
			}
			if !reflect.DeepEqual(calls, []string{"/api/auth.info", "/api/collections.info", "/api/collections.update"}) {
				t.Fatalf("unexpected calls: %v", calls)
			}
		})
	}
}

func TestCollectionReadRefreshAndLocalGuard(t *testing.T) {
	t.Parallel()
	for _, guard := range []types.Bool{types.BoolValue(true), types.BoolValue(false), types.BoolNull(), types.BoolUnknown()} {
		t.Run(guard.String(), func(t *testing.T) {
			t.Parallel()
			collection := collectionTestCollection()
			collection.Name = groupTestPointer("Remote rename")
			collection.Description = nullable.NewNullNullable[string]()
			collection.Permission = nullable.NewNullableWithValue(client.PermissionRead)
			collection.Sharing = groupTestPointer(true)
			r := &collectionResource{api: collectionTestAdminClient(t, func(w http.ResponseWriter, req *http.Request) {
				if req.URL.Path != "/api/collections.info" {
					t.Errorf("read wrote or listed collections: %s", req.URL.Path)
				}
				collectionTestAssertIDBody(t, w, req)
				groupTestEncode(t, w, collectionTestEnvelope(collection))
			})}
			model := collectionTestModel()
			model.Description, model.AllowDestroy = types.StringValue("Stale"), guard
			state := tfsdk.State(collectionTestPlan(t, r, model))
			response := resource.ReadResponse{State: state}
			r.Read(t.Context(), resource.ReadRequest{State: state}, &response)
			want := collectionTestModel()
			want.Name, want.Permission, want.Sharing = types.StringValue("Remote rename"), types.StringValue("read"), types.BoolValue(true)
			want.AllowDestroy = types.BoolValue(guard.ValueBool())
			if response.Diagnostics.HasError() || collectionTestStateModel(t, response.State) != want {
				t.Fatalf("refresh: %v %+v", response.Diagnostics, response.State)
			}
		})
	}
}

func TestCollectionGeneratedNullableWireJSON(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		permission nullable.Nullable[client.Permission]
		want       string
		specified  bool
		null       bool
	}{
		{"omitted", nil, "", false, false},
		{"explicit null", nullable.NewNullNullable[client.Permission](), "null", true, true},
		{"read_write", nullable.NewNullableWithValue(client.PermissionReadWrite), `"read_write"`, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			for label, request := range map[string]any{
				"create": client.CollectionsCreateJSONRequestBody{Name: "Engineering", Permission: tc.permission},
				"update": client.CollectionsUpdateJSONRequestBody{Id: uuid.MustParse(collectionTestID), Permission: tc.permission},
			} {
				body, err := json.Marshal(request)
				if err != nil {
					t.Fatal(err)
				}
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(body, &fields); err != nil {
					t.Fatal(err)
				}
				field, exists := fields["permission"]
				if exists != tc.specified || string(field) != tc.want {
					t.Fatalf("%s nullable wire JSON: %s", label, body)
				}
				var collection client.Collection
				if err := json.Unmarshal(body, &collection); err != nil {
					t.Fatal(err)
				}
				if collection.Permission.IsSpecified() != tc.specified || collection.Permission.IsNull() != tc.null || (!tc.null && tc.specified && collection.Permission.GetOrEmpty() != client.PermissionReadWrite) {
					t.Fatalf("nullable decoding lost omitted/null/value distinction: %+v", collection.Permission)
				}
			}
		})
	}
	for _, field := range []string{"description", "permission", "archivedAt", "deletedAt"} {
		for _, explicitNull := range []bool{false, true} {
			fields := collectionTestFields()
			delete(fields, field)
			if explicitNull {
				fields[field] = nil
			}
			body, err := json.Marshal(fields)
			if err != nil {
				t.Fatal(err)
			}
			var collection client.Collection
			if err := json.Unmarshal(body, &collection); err != nil {
				t.Fatal(err)
			}
			if err := validateCollection(&collection, uuid.MustParse(collectionTestID)); (err == nil) != explicitNull {
				t.Fatalf("%s explicitNull=%t: %v", field, explicitNull, err)
			}
		}
	}
}
