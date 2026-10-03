// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/glitchedmob/terraform-provider-outline/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	resourceschema "github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"github.com/oapi-codegen/nullable"
)

func cuUnitProtocolValue(t *testing.T, values map[string]any) *tfprotov6.DynamicValue {
	t.Helper()
	attrs := map[string]tftypes.Type{"id": tftypes.String, "collection_id": tftypes.String, "user_id": tftypes.String, "permission": tftypes.String}
	fields := make(map[string]tftypes.Value)
	for name, typ := range attrs {
		fields[name] = tftypes.NewValue(typ, values[name])
	}
	value := tftypes.NewValue(tftypes.Object{AttributeTypes: attrs}, fields)
	dynamic, err := tfprotov6.NewDynamicValue(value.Type(), value)
	if err != nil {
		t.Fatal(err)
	}
	return &dynamic
}

func TestCollectionUserSchemaRegistrationAndConfigure(t *testing.T) {
	r := NewCollectionUserResource().(*collectionUserResource)
	var schema resource.SchemaResponse
	r.Schema(t.Context(), resource.SchemaRequest{}, &schema)
	if schema.Diagnostics.HasError() || schema.Schema.ValidateImplementation(t.Context()).HasError() || len(schema.Schema.Attributes) != 4 {
		t.Fatalf("invalid schema: %v", schema.Diagnostics)
	}
	id := schema.Schema.Attributes["id"].(resourceschema.StringAttribute)
	permission := schema.Schema.Attributes["permission"].(resourceschema.StringAttribute)
	if !id.Computed || id.Optional || id.Required || !permission.Required || permission.Computed || permission.Optional || permission.Default != nil || len(permission.Validators) == 0 || len(permission.PlanModifiers) != 0 {
		t.Fatal("permission must be required without a default; only ID is computed")
	}
	for _, name := range []string{"collection_id", "user_id"} {
		attr := schema.Schema.Attributes[name].(resourceschema.StringAttribute)
		if !attr.Required || attr.Computed || attr.Optional || attr.Default != nil || len(attr.PlanModifiers) != 1 || len(attr.Validators) == 0 || attr.MarkdownDescription == "" {
			t.Fatalf("wrong pair schema: %s", name)
		}
	}
	var metadata resource.MetadataResponse
	r.Metadata(t.Context(), resource.MetadataRequest{ProviderTypeName: "outline"}, &metadata)
	if metadata.TypeName != "outline_collection_user" {
		t.Fatal(metadata.TypeName)
	}
	server := providerserver.NewProtocol6(New("unit")())()
	response, err := server.GetProviderSchema(t.Context(), &tfprotov6.GetProviderSchemaRequest{})
	if err != nil || protocolHasError(response.Diagnostics) || response.ResourceSchemas["outline_collection_user"] == nil || response.DataSourceSchemas["outline_collection_user"] != nil {
		t.Fatalf("grant must be registered as a resource only: %v %v", response, err)
	}
	for _, data := range []any{nil, "wrong client", (*apiClient)(nil), &apiClient{}} {
		var resp resource.ConfigureResponse
		r.Configure(t.Context(), resource.ConfigureRequest{ProviderData: data}, &resp)
		api, ok := data.(*apiClient)
		valid := data == nil || ok && api != nil
		if resp.Diagnostics.HasError() == valid || ok && api != nil && r.api != api {
			t.Fatalf("configure %T: %v", data, resp.Diagnostics)
		}
	}
}

func TestCollectionUserProtocolValidationRequiredUUIDsAndPermissions(t *testing.T) {
	for _, field := range []string{"collection_id", "user_id", "permission", "id"} {
		for _, value := range []any{nil, "", "not-a-uuid", "00000000-0000-0000-0000-000000000000", cuUnitCollectionID, cuUnitUserID,
			strings.ToUpper(cuUnitUserID), strings.ReplaceAll(cuUnitUserID, "-", ""), "urn:uuid:" + cuUnitUserID,
			" " + cuUnitUserID, "a32c2ee6-fbde-0654-841b-0eabdc71b812", "a32c2ee6-fbde-4654-041b-0eabdc71b812",
			"read", "read_write", "admin", "member", "owner", "READ", tftypes.UnknownValue} {
			t.Run(field+"/"+fmt.Sprint(value), func(t *testing.T) {
				values := map[string]any{"collection_id": cuUnitCollectionID, "user_id": cuUnitUserID, "permission": "read"}
				values[field] = value
				valid := value == tftypes.UnknownValue
				switch field {
				case "id":
					valid = value == nil
				case "permission":
					valid = valid || value == "read" || value == "read_write" || value == "admin"
				default:
					valid = valid || value == cuUnitCollectionID || value == cuUnitUserID
				}
				server := providerserver.NewProtocol6(New("unit")())()
				response, err := server.ValidateResourceConfig(t.Context(), &tfprotov6.ValidateResourceConfigRequest{TypeName: "outline_collection_user", Config: cuUnitProtocolValue(t, values)})
				if err != nil || protocolHasError(response.Diagnostics) == valid {
					t.Fatalf("valid=%t: %v %v", valid, response, err)
				}
			})
		}
	}
}

func TestCollectionUserProtocolPlanIdentityAndReplacement(t *testing.T) {
	for _, change := range []string{"create", "none", "collection_id", "user_id", "permission", "unknown collection", "unknown user", "destroy"} {
		t.Run(change, func(t *testing.T) {
			server := providerserver.NewProtocol6(New("unit")())()
			var schema resource.SchemaResponse
			NewCollectionUserResource().Schema(t.Context(), resource.SchemaRequest{}, &schema)
			typ := schema.Schema.Type().TerraformType(t.Context())
			prior := cuUnitProtocolValue(t, map[string]any{"id": cuUnitPairID, "collection_id": cuUnitCollectionID, "user_id": cuUnitUserID, "permission": "read"})
			var collection, user any = cuUnitCollectionID, cuUnitUserID
			permission := "read"
			switch change {
			case "collection_id":
				collection = collectionTestOtherID
			case "user_id":
				user = cuUnitOtherUserID
			case "permission":
				permission = "admin"
			case "unknown collection":
				collection = tftypes.UnknownValue
			case "unknown user":
				user = tftypes.UnknownValue
			}
			config := cuUnitProtocolValue(t, map[string]any{"collection_id": collection, "user_id": user, "permission": permission})
			proposed := cuUnitProtocolValue(t, map[string]any{"id": tftypes.UnknownValue, "collection_id": collection, "user_id": user, "permission": permission})
			null, err := tfprotov6.NewDynamicValue(typ, tftypes.NewValue(typ, nil))
			if err != nil {
				t.Fatal(err)
			}
			if change == "create" || strings.HasPrefix(change, "unknown") {
				prior = &null
			}
			if change == "destroy" {
				config, proposed = &null, &null
			}
			response, err := server.PlanResourceChange(t.Context(), &tfprotov6.PlanResourceChangeRequest{TypeName: "outline_collection_user", PriorState: prior, Config: config, ProposedNewState: proposed})
			if err != nil || protocolHasError(response.Diagnostics) || response.PlannedState == nil {
				t.Fatalf("plan: %v %v", response, err)
			}
			if change == "collection_id" || change == "user_id" {
				if len(response.RequiresReplace) != 1 || !response.RequiresReplace[0].Equal(tftypes.NewAttributePath().WithAttributeName(change)) {
					t.Fatalf("wrong replacement paths: %v", response.RequiresReplace)
				}
			} else if len(response.RequiresReplace) != 0 {
				t.Fatalf("unexpected replacement paths: %v", response.RequiresReplace)
			}
			value, err := response.PlannedState.Unmarshal(typ)
			if err != nil {
				t.Fatal(err)
			}
			if change == "destroy" {
				if !value.IsNull() {
					t.Fatal("destroy plan must stay null")
				}
				return
			}
			var fields map[string]tftypes.Value
			if err := value.As(&fields); err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(change, "unknown") {
				if fields["id"].IsKnown() {
					t.Fatal("unknown parent must leave the composite ID unknown")
				}
			} else {
				var id string
				if err := fields["id"].As(&id); err != nil || id != fmt.Sprint(collection)+"/"+fmt.Sprint(user) {
					t.Fatalf("wrong planned ID %q: %v", id, err)
				}
			}
			var plannedPermission string
			if err := fields["permission"].As(&plannedPermission); err != nil || plannedPermission != permission {
				t.Fatalf("permission defaulted or changed: %q %v", plannedPermission, err)
			}
		})
	}
}

func TestCollectionUserModifyPlanInvalidUUIDs(t *testing.T) {
	for _, field := range []string{"collection_id", "user_id"} {
		r := &collectionUserResource{}
		model := cuUnitModel()
		model.ID = types.StringUnknown()
		if field == "collection_id" {
			model.CollectionID = types.StringValue("invalid")
		} else {
			model.UserID = types.StringValue("invalid")
		}
		plan := cuUnitPlan(t, r, model)
		resp := resource.ModifyPlanResponse{Plan: plan}
		r.ModifyPlan(t.Context(), resource.ModifyPlanRequest{Plan: plan}, &resp)
		if !resp.Diagnostics.HasError() || len(resp.Diagnostics) != 1 {
			t.Fatal(resp.Diagnostics)
		}
		withPath, ok := resp.Diagnostics[0].(interface{ Path() path.Path })
		if !ok || !withPath.Path().Equal(path.Root(field)) || !resp.Plan.Raw.Equal(plan.Raw) {
			t.Fatalf("invalid UUID must be attributed to %s without planning an ID: %v", field, resp.Diagnostics)
		}
	}
}

func TestCollectionUserImportFormatAndTargetDiscovery(t *testing.T) {
	invalidIDs := []string{"", cuUnitGrantID, cuUnitCollectionID, cuUnitPairID + "/extra", "/" + cuUnitPairID, cuUnitPairID + "/",
		cuUnitCollectionID + ":" + cuUnitUserID, strings.ReplaceAll(cuUnitPairID, "/", "%2F"), strings.ReplaceAll(cuUnitPairID, "-", ""),
		strings.ToUpper(cuUnitPairID), "urn:uuid:" + cuUnitPairID, " " + cuUnitPairID, cuUnitPairID + " ", "{" + cuUnitPairID + "}",
		"00000000-0000-0000-0000-000000000000/" + cuUnitUserID, cuUnitCollectionID + "/00000000-0000-0000-0000-000000000000",
		"a32c2ee6-fbde-0654-841b-0eabdc71b812/" + cuUnitUserID, cuUnitCollectionID + "/a32c2ee6-fbde-4654-041b-0eabdc71b812"}
	for _, id := range invalidIDs {
		t.Run("format/"+id, func(t *testing.T) {
			r := &collectionUserResource{api: cuUnitClient(t, func(http.ResponseWriter, *http.Request) { t.Error("invalid import contacted Outline") })}
			model := cuUnitModel()
			model.ID = types.StringValue(id)
			diagnostics, state := cuUnitOperation(t, r, "import", cuUnitModel(), model)
			groupTestDiagnostics(t, diagnostics, "exactly one slash")
			if !state.Raw.IsNull() {
				t.Fatal("invalid import adopted state")
			}
		})
	}
	for _, permission := range []string{"read", "read_write", "admin", "absent"} {
		t.Run("discover/"+permission, func(t *testing.T) {
			var calls []string
			r := &collectionUserResource{api: cuUnitClient(t, func(w http.ResponseWriter, req *http.Request) {
				calls = append(calls, req.URL.Path)
				if req.URL.Path == "/api/collections.info" {
					collection := collectionTestCollection()
					collection.Permission = nullable.NewNullableWithValue(client.PermissionReadWrite)
					groupTestEncode(t, w, collectionTestEnvelope(collection))
					return
				}
				if cuUnitParents(t, w, req) {
					return
				}
				if req.URL.Path != "/api/collections.memberships" {
					t.Errorf("import used preview/effective access or wrote data: %s", req.URL.Path)
					return
				}
				cuUnitOffset(t, w, req)
				members := []client.Membership{}
				if permission != "absent" {
					members = append(members, cuUnitGrant(cuUnitUserID, client.Permission(permission)))
				}
				groupTestEncode(t, w, cuUnitEnvelope(members, 0, len(members), false))
			})}
			diagnostics, state := cuUnitOperation(t, r, "import", cuUnitModel(), cuUnitModel())
			if permission == "absent" {
				groupTestDiagnostics(t, diagnostics, "inherited or default access is not an importable grant")
				if !state.Raw.IsNull() {
					t.Fatal("default access imported as an explicit grant")
				}
			} else {
				want := cuUnitModel()
				want.Permission = types.StringValue(permission)
				if diagnostics.HasError() || cuUnitStateModel(t, state) != want {
					t.Fatalf("target discovery: %v", diagnostics)
				}
			}
			var targetCalls []string
			for _, call := range calls {
				if call != "/api/auth.info" {
					targetCalls = append(targetCalls, call)
				}
			}
			if !reflect.DeepEqual(targetCalls, []string{"/api/collections.info", "/api/users.info", "/api/collections.memberships"}) {
				t.Fatalf("import failed to discover both parents and the explicit grant: %v", calls)
			}
		})
	}
}

func TestCollectionUserInvalidPairAndUnsafePermissionNeverCallAPI(t *testing.T) {
	for _, failure := range []string{"collection_id", "user_id", "mismatched id", "changed collection", "changed user", "null permission", "unknown permission", "empty permission", "member", "owner", "READ"} {
		for _, operation := range []string{"create", "read", "update", "delete"} {
			if (strings.Contains(failure, "permission") || failure == "member" || failure == "owner" || failure == "READ" || strings.HasPrefix(failure, "changed")) && operation != "create" && operation != "update" {
				continue
			}
			if strings.HasPrefix(failure, "changed") && operation != "update" || failure == "mismatched id" && operation == "create" {
				continue
			}
			t.Run(operation+"/"+failure, func(t *testing.T) {
				current, desired := cuUnitModel(), cuUnitModel()
				switch failure {
				case "collection_id":
					current.CollectionID = types.StringValue("invalid")
					desired = current
				case "user_id":
					current.UserID = types.StringValue("invalid")
					desired = current
				case "mismatched id":
					current.ID = types.StringValue(cuUnitCollectionID + "/" + cuUnitOtherUserID)
					desired = current
				case "changed collection":
					desired.CollectionID = types.StringValue(collectionTestOtherID)
					desired.ID = types.StringValue(collectionTestOtherID + "/" + cuUnitUserID)
				case "changed user":
					desired.UserID = types.StringValue(cuUnitOtherUserID)
					desired.ID = types.StringValue(cuUnitCollectionID + "/" + cuUnitOtherUserID)
				case "null permission":
					desired.Permission = types.StringNull()
				case "unknown permission":
					desired.Permission = types.StringUnknown()
				case "empty permission":
					desired.Permission = types.StringValue("")
				default:
					desired.Permission = types.StringValue(failure)
				}
				r := &collectionUserResource{api: cuUnitClient(t, func(http.ResponseWriter, *http.Request) { t.Error("unsafe pair or permission contacted Outline") })}
				diagnostics, state := cuUnitOperation(t, r, operation, current, desired)
				cuUnitAssertFailureState(t, operation, diagnostics, state, current)
			})
		}
	}
}
