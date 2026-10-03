// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	resourceschema "github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

func collectionTestProtocolConfig(t *testing.T, values map[string]any, dataSource bool) *tfprotov6.DynamicValue {
	t.Helper()
	attributes := map[string]tftypes.Type{
		"id": tftypes.String, "name": tftypes.String, "description": tftypes.String, "permission": tftypes.String, "sharing": tftypes.Bool,
	}
	if !dataSource {
		attributes["allow_destroy"] = tftypes.Bool
	}
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

func TestCollectionProtocolValidation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		dataSource bool
		values     map[string]any
		wantError  bool
	}{
		{"minimal private resource", false, map[string]any{"name": "Engineering"}, false},
		{"explicit private", false, map[string]any{"name": "Engineering", "permission": nil}, false},
		{"read", false, map[string]any{"name": "Engineering", "permission": "read"}, false},
		{"read_write", false, map[string]any{"name": "Engineering", "permission": "read_write"}, false},
		{"all configurable fields", false, map[string]any{"name": "Engineering", "description": "# Landing page\nhttps://example.com", "permission": "read", "sharing": true, "allow_destroy": true}, false},
		{"admin not supported", false, map[string]any{"name": "Engineering", "permission": "admin"}, true},
		{"empty permission", false, map[string]any{"name": "Engineering", "permission": ""}, true},
		{"unknown enum", false, map[string]any{"name": "Engineering", "permission": "owner"}, true},
		{"name absent", false, map[string]any{}, true},
		{"name empty", false, map[string]any{"name": ""}, true},
		{"name whitespace", false, map[string]any{"name": " \t\n"}, true},
		{"leading whitespace", false, map[string]any{"name": " Engineering"}, true},
		{"leading ECMAScript BOM whitespace", false, map[string]any{"name": "\uFEFFEngineering"}, true},
		{"trailing ECMAScript BOM whitespace", false, map[string]any{"name": "Engineering\uFEFF"}, true},
		{"trailing whitespace", false, map[string]any{"name": "Engineering\u00a0"}, true},
		{"URL in name", false, map[string]any{"name": "Docs https://example.com"}, true},
		{"www URL in name", false, map[string]any{"name": "www.example.com"}, true},
		{"file URL in name", false, map[string]any{"name": "file:///docs.md"}, true},
		{"name maximum", false, map[string]any{"name": strings.Repeat("x", 100)}, false},
		{"name too long", false, map[string]any{"name": strings.Repeat("x", 101)}, true},
		{"name Unicode maximum", false, map[string]any{"name": strings.Repeat("😀", 100)}, false},
		{"name Unicode too long", false, map[string]any{"name": strings.Repeat("😀", 101)}, true},
		{"description empty", false, map[string]any{"name": "Engineering", "description": ""}, false},
		{"description maximum", false, map[string]any{"name": "Engineering", "description": strings.Repeat("x", 100000)}, false},
		{"description too long", false, map[string]any{"name": "Engineering", "description": strings.Repeat("x", 100001)}, true},
		{"description Unicode maximum", false, map[string]any{"name": "Engineering", "description": strings.Repeat("😀", 100000)}, false},
		{"description Unicode too long", false, map[string]any{"name": "Engineering", "description": strings.Repeat("😀", 100001)}, true},
		{"unknown fields deferred", false, map[string]any{"name": tftypes.UnknownValue, "description": tftypes.UnknownValue, "permission": tftypes.UnknownValue, "sharing": tftypes.UnknownValue, "allow_destroy": tftypes.UnknownValue}, false},
		{"resource computed ID", false, map[string]any{"id": collectionTestID, "name": "Engineering"}, true},
		{"data ID", true, map[string]any{"id": collectionTestID}, false},
		{"data exact name", true, map[string]any{"name": "Engineering"}, false},
		{"data neither", true, map[string]any{}, true},
		{"data both", true, map[string]any{"id": collectionTestID, "name": "Engineering"}, true},
		{"data malformed ID", true, map[string]any{"id": "collection"}, true},
		{"data zero ID", true, map[string]any{"id": "00000000-0000-0000-0000-000000000000"}, true},
		{"data uppercase ID", true, map[string]any{"id": strings.ToUpper(collectionTestID)}, true},
		{"data compact UUID", true, map[string]any{"id": strings.ReplaceAll(collectionTestID, "-", "")}, true},
		{"data invalid variant", true, map[string]any{"id": "a32c2ee6-fbde-4654-041b-0eabdc71b812"}, true},
		{"data invalid version", true, map[string]any{"id": "a32c2ee6-fbde-0654-841b-0eabdc71b812"}, true},
		{"data empty name", true, map[string]any{"name": ""}, true},
		{"data name maximum", true, map[string]any{"name": strings.Repeat("x", 200)}, false},
		{"data name too long", true, map[string]any{"name": strings.Repeat("x", 201)}, true},
		{"data supplementary maximum", true, map[string]any{"name": strings.Repeat("😀", 100)}, false},
		{"data supplementary too long", true, map[string]any{"name": strings.Repeat("😀", 101)}, true},
		{"data unknown ID", true, map[string]any{"id": tftypes.UnknownValue}, false},
		{"data unknown name", true, map[string]any{"name": tftypes.UnknownValue}, false},
		{"data computed description", true, map[string]any{"id": collectionTestID, "description": "not configurable"}, true},
		{"data computed permission", true, map[string]any{"id": collectionTestID, "permission": "read"}, true},
		{"data computed sharing", true, map[string]any{"id": collectionTestID, "sharing": false}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			server := providerserver.NewProtocol6(New("unit")())()
			config := collectionTestProtocolConfig(t, tc.values, tc.dataSource)
			var diagnostics []*tfprotov6.Diagnostic
			if tc.dataSource {
				response, err := server.ValidateDataResourceConfig(t.Context(), &tfprotov6.ValidateDataResourceConfigRequest{TypeName: "outline_collection", Config: config})
				if err != nil {
					t.Fatal(err)
				}
				diagnostics = response.Diagnostics
			} else {
				response, err := server.ValidateResourceConfig(t.Context(), &tfprotov6.ValidateResourceConfigRequest{TypeName: "outline_collection", Config: config})
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
				t.Fatalf("want validation error=%t: %s", tc.wantError, strings.Join(details, "\n"))
			}
		})
	}
}

func TestCollectionSchemaAndConfigure(t *testing.T) {
	t.Parallel()
	r, d := NewCollectionResource().(*collectionResource), NewCollectionDataSource().(*collectionDataSource)
	var resourceMetadata resource.MetadataResponse
	r.Metadata(t.Context(), resource.MetadataRequest{ProviderTypeName: "outline"}, &resourceMetadata)
	var dataMetadata datasource.MetadataResponse
	d.Metadata(t.Context(), datasource.MetadataRequest{ProviderTypeName: "outline"}, &dataMetadata)
	if resourceMetadata.TypeName != "outline_collection" || dataMetadata.TypeName != "outline_collection" {
		t.Fatal("incorrect collection type names")
	}
	var schema resource.SchemaResponse
	r.Schema(t.Context(), resource.SchemaRequest{}, &schema)
	if schema.Diagnostics.HasError() || len(schema.Schema.Attributes) != 6 {
		t.Fatalf("resource schema: %+v", schema)
	}
	id := schema.Schema.Attributes["id"].(resourceschema.StringAttribute)
	name := schema.Schema.Attributes["name"].(resourceschema.StringAttribute)
	description := schema.Schema.Attributes["description"].(resourceschema.StringAttribute)
	permission := schema.Schema.Attributes["permission"].(resourceschema.StringAttribute)
	if !id.Computed || id.Optional || id.Required || len(id.PlanModifiers) != 1 || !name.Required || name.Optional || name.Computed || len(name.PlanModifiers) != 0 ||
		!description.Optional || !description.Computed || description.Default == nil || !permission.Optional || permission.Computed || permission.Required || permission.Default != nil {
		t.Fatal("incorrect resource ID, name, description, or private permission flags")
	}
	for _, key := range []string{"sharing", "allow_destroy"} {
		attribute := schema.Schema.Attributes[key].(resourceschema.BoolAttribute)
		if !attribute.Optional || !attribute.Computed || attribute.Required || attribute.Default == nil || len(attribute.PlanModifiers) != 0 {
			t.Fatalf("incorrect %s flags: %+v", key, attribute)
		}
	}
	server := providerserver.NewProtocol6(New("unit")())()
	protocol, err := server.GetProviderSchema(t.Context(), &tfprotov6.GetProviderSchemaRequest{})
	if err != nil || protocolHasError(protocol.Diagnostics) {
		t.Fatalf("protocol schema: %v %v", protocol, err)
	}
	for label, schema := range map[string]*tfprotov6.Schema{"resource": protocol.ResourceSchemas["outline_collection"], "data source": protocol.DataSourceSchemas["outline_collection"]} {
		wantCount := 6
		if label == "data source" {
			wantCount = 5
		}
		if schema == nil || schema.Block == nil || len(schema.Block.Attributes) != wantCount || schema.Block.Description == "" {
			t.Fatalf("missing %s protocol schema", label)
		}
		for _, attribute := range schema.Block.Attributes {
			wantType := tftypes.String
			if attribute.Name == "sharing" || attribute.Name == "allow_destroy" {
				wantType = tftypes.Bool
			}
			if !attribute.Type.Equal(wantType) || attribute.Sensitive || attribute.Description == "" {
				t.Fatalf("wrong protocol attribute: %+v", attribute)
			}
			if label == "data source" && (!attribute.Computed || attribute.Required || attribute.Optional != (attribute.Name == "id" || attribute.Name == "name")) {
				t.Fatalf("wrong lookup flags: %+v", attribute)
			}
		}
	}
	api := collectionTestClient(t, func(http.ResponseWriter, *http.Request) { t.Error("configure contacted Outline") })
	for _, tc := range []struct {
		name string
		data any
		bad  bool
	}{
		{"nil", nil, false}, {"wrong type", "not a client", true}, {"typed nil", (*apiClient)(nil), true}, {"valid", api, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, d := &collectionResource{}, &collectionDataSource{}
			var resourceResponse resource.ConfigureResponse
			r.Configure(t.Context(), resource.ConfigureRequest{ProviderData: tc.data}, &resourceResponse)
			var dataResponse datasource.ConfigureResponse
			d.Configure(t.Context(), datasource.ConfigureRequest{ProviderData: tc.data}, &dataResponse)
			if resourceResponse.Diagnostics.HasError() != tc.bad || dataResponse.Diagnostics.HasError() != tc.bad {
				t.Fatalf("configure: %v %v", resourceResponse.Diagnostics, dataResponse.Diagnostics)
			}
			if tc.name == "valid" && (r.api != api || d.api != api) {
				t.Fatal("configure did not retain the client")
			}
		})
	}
}

func TestCollectionProtocolPlanDefaultsStableIDAndNoReplacement(t *testing.T) {
	t.Parallel()
	for _, existing := range []bool{false, true} {
		for _, configured := range []bool{false, true} {
			t.Run(fmt.Sprintf("existing=%t/configured=%t", existing, configured), func(t *testing.T) {
				t.Parallel()
				server := providerserver.NewProtocol6(New("unit")())()
				var schema resource.SchemaResponse
				(&collectionResource{}).Schema(t.Context(), resource.SchemaRequest{}, &schema)
				typ := schema.Schema.Type().TerraformType(t.Context())
				var prior *tfprotov6.DynamicValue
				if existing {
					prior = collectionTestProtocolConfig(t, map[string]any{"id": collectionTestID, "name": "Old name", "description": "Old description", "permission": "read_write", "sharing": true, "allow_destroy": true}, false)
				} else {
					dynamic, err := tfprotov6.NewDynamicValue(typ, tftypes.NewValue(typ, nil))
					if err != nil {
						t.Fatal(err)
					}
					prior = &dynamic
				}
				values := map[string]any{"name": "Renamed"}
				proposed := map[string]any{"id": tftypes.UnknownValue, "name": "Renamed", "description": tftypes.UnknownValue, "sharing": tftypes.UnknownValue, "allow_destroy": tftypes.UnknownValue}
				wantDescription, wantPermission, wantSharing, wantDestroy := "", "", false, false
				if configured {
					wantDescription, wantPermission, wantSharing, wantDestroy = "# Markdown", "read", false, true
					values["description"], values["permission"], values["sharing"], values["allow_destroy"] = wantDescription, wantPermission, wantSharing, wantDestroy
					proposed["description"], proposed["permission"], proposed["sharing"], proposed["allow_destroy"] = wantDescription, wantPermission, wantSharing, wantDestroy
				}
				response, err := server.PlanResourceChange(t.Context(), &tfprotov6.PlanResourceChangeRequest{
					TypeName: "outline_collection", PriorState: prior,
					Config: collectionTestProtocolConfig(t, values, false), ProposedNewState: collectionTestProtocolConfig(t, proposed, false),
				})
				if err != nil || protocolHasError(response.Diagnostics) || response.PlannedState == nil || len(response.RequiresReplace) != 0 {
					t.Fatalf("metadata/access changes caused an error or replacement: %v %v", response, err)
				}
				value, err := response.PlannedState.Unmarshal(typ)
				if err != nil {
					t.Fatal(err)
				}
				var fields map[string]tftypes.Value
				if err := value.As(&fields); err != nil {
					t.Fatal(err)
				}
				var name, description string
				var sharing, destroy bool
				if err := fields["name"].As(&name); err != nil || name != "Renamed" {
					t.Fatalf("planned name: %q %v", name, err)
				}
				if err := fields["description"].As(&description); err != nil || description != wantDescription {
					t.Fatalf("description reset/default: %q %v", description, err)
				}
				if err := fields["sharing"].As(&sharing); err != nil || sharing != wantSharing {
					t.Fatalf("sharing reset/default: %t %v", sharing, err)
				}
				if err := fields["allow_destroy"].As(&destroy); err != nil || destroy != wantDestroy {
					t.Fatalf("destruction guard default: %t %v", destroy, err)
				}
				if configured {
					var permission string
					if err := fields["permission"].As(&permission); err != nil || permission != wantPermission {
						t.Fatalf("planned permission: %q %v", permission, err)
					}
				} else if !fields["permission"].IsNull() {
					t.Fatal("removing permission configuration did not plan private access")
				}
				if existing {
					var id string
					if err := fields["id"].As(&id); err != nil || id != collectionTestID {
						t.Fatalf("UseStateForUnknown did not retain stable ID: %q %v", id, err)
					}
				} else if fields["id"].IsKnown() {
					t.Fatal("new collection ID must remain unknown")
				}
			})
		}
	}
}
