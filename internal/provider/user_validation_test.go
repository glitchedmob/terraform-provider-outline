// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	resourceschema "github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

func userTestProtocolConfig(t *testing.T, values map[string]any, dataSource bool) *tfprotov6.DynamicValue {
	t.Helper()
	attributes := map[string]tftypes.Type{"id": tftypes.String, "email": tftypes.String, "name": tftypes.String, "role": tftypes.String, "suspended": tftypes.Bool}
	if !dataSource {
		attributes["suppress_email"], attributes["delete_permanently"] = tftypes.Bool, tftypes.Bool
		attributes["allow_temporary_activation_for_role_change"] = tftypes.Bool
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

func TestUserProtocolValidation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		dataSource bool
		values     map[string]any
		invalid    bool
	}{
		{"minimal resource", false, map[string]any{"email": userTestEmail, "suspended": false}, false},
		{"explicit suspension", false, map[string]any{"email": userTestEmail, "suspended": true}, false},
		{"email casing", false, map[string]any{"email": "OIDC@EXAMPLE.COM", "suspended": false}, false},
		{"www local email", false, map[string]any{"email": "www.person@example.com", "suspended": false}, false},
		{"www domain email", false, map[string]any{"email": "oidc@www.example.com", "suspended": false}, false},
		{"all resource fields", false, map[string]any{"email": userTestEmail, "suspended": false, "name": "OIDC User", "role": "guest", "suppress_email": false, "delete_permanently": true, "allow_temporary_activation_for_role_change": true}, false},
		{"missing email", false, map[string]any{"suspended": false}, true},
		{"missing suspension", false, map[string]any{"email": userTestEmail}, true},
		{"empty email", false, map[string]any{"email": "", "suspended": false}, true},
		{"email without at", false, map[string]any{"email": "example.com", "suspended": false}, true},
		{"email without domain", false, map[string]any{"email": "user@", "suspended": false}, true},
		{"email substring", false, map[string]any{"email": "example", "suspended": false}, true},
		{"email whitespace", false, map[string]any{"email": " oidc@example.com ", "suspended": false}, true},
		{"email interior whitespace", false, map[string]any{"email": "oid c@example.com", "suspended": false}, true},
		{"email multiple at", false, map[string]any{"email": "oidc@@example.com", "suspended": false}, true},
		{"email maximum", false, map[string]any{"email": strings.Repeat("x", 242) + "@example.com", "suspended": false}, false},
		{"email overflow", false, map[string]any{"email": strings.Repeat("x", 243) + "@example.com", "suspended": false}, true},
		{"blank name", false, map[string]any{"email": userTestEmail, "suspended": false, "name": " \t"}, true},
		{"empty name", false, map[string]any{"email": userTestEmail, "suspended": false, "name": ""}, true},
		{"name maximum codepoints", false, map[string]any{"email": userTestEmail, "suspended": false, "name": strings.Repeat("😀", 255)}, false},
		{"name overflow codepoints", false, map[string]any{"email": userTestEmail, "suspended": false, "name": strings.Repeat("😀", 256)}, true},
		{"name Unicode whitespace", false, map[string]any{"email": userTestEmail, "suspended": false, "name": "\u2003\u00a0"}, true},
		{"name HTTP URL", false, map[string]any{"email": userTestEmail, "suspended": false, "name": "OIDC http://example.com user"}, true},
		{"name HTTPS URL", false, map[string]any{"email": userTestEmail, "suspended": false, "name": "https://example.com"}, true},
		{"name file URL", false, map[string]any{"email": userTestEmail, "suspended": false, "name": "file:/tmp/document"}, true},
		{"name www URL", false, map[string]any{"email": userTestEmail, "suspended": false, "name": "OIDC www.example.com user"}, true},
		{"name supplied www email", false, map[string]any{"email": "www.person@example.com", "suspended": false, "name": "www.person@example.com"}, true},
		{"name URL regex without slashes", false, map[string]any{"email": userTestEmail, "suspended": false, "name": "http:ab"}, true},
		{"name URL regex underscore suffix", false, map[string]any{"email": userTestEmail, "suspended": false, "name": "www.a_"}, true},
		{"name URL regex Unicode whitespace", false, map[string]any{"email": userTestEmail, "suspended": false, "name": "www.\u00a0Alice"}, false},
		{"name URL regex BOM whitespace", false, map[string]any{"email": userTestEmail, "suspended": false, "name": "http:\ufeffAlice"}, false},
		{"name URL regex case sensitive", false, map[string]any{"email": userTestEmail, "suspended": false, "name": "HTTPS://EXAMPLE.COM"}, false},
		{"name URL regex needs two suffix characters", false, map[string]any{"email": userTestEmail, "suspended": false, "name": "https:a"}, false},
		{"name URL regex needs ASCII word suffix", false, map[string]any{"email": userTestEmail, "suspended": false, "name": "www.☃☃"}, false},
		{"admin role", false, map[string]any{"email": userTestEmail, "suspended": false, "role": "admin"}, false},
		{"viewer role", false, map[string]any{"email": userTestEmail, "suspended": false, "role": "viewer"}, false},
		{"unsupported role", false, map[string]any{"email": userTestEmail, "suspended": false, "role": "superadmin"}, true},
		{"uppercase role", false, map[string]any{"email": userTestEmail, "suspended": false, "role": "Member"}, true},
		{"computed ID configured", false, map[string]any{"id": userTestID, "email": userTestEmail, "suspended": false}, true},
		{"unknown resource fields", false, map[string]any{"email": tftypes.UnknownValue, "suspended": tftypes.UnknownValue, "role": tftypes.UnknownValue, "name": tftypes.UnknownValue, "allow_temporary_activation_for_role_change": tftypes.UnknownValue}, false},
		{"data ID", true, map[string]any{"id": userTestID}, false},
		{"data email", true, map[string]any{"email": userTestEmail}, false},
		{"data neither", true, map[string]any{}, true},
		{"data both", true, map[string]any{"id": userTestID, "email": userTestEmail}, true},
		{"data empty ID", true, map[string]any{"id": ""}, true},
		{"data malformed ID", true, map[string]any{"id": "user"}, true},
		{"data zero UUID", true, map[string]any{"id": "00000000-0000-0000-0000-000000000000"}, true},
		{"data uppercase ID", true, map[string]any{"id": strings.ToUpper(userTestID)}, true},
		{"data compact ID", true, map[string]any{"id": strings.ReplaceAll(userTestID, "-", "")}, true},
		{"data invalid variant", true, map[string]any{"id": "d32c2ee6-fbde-4654-041b-0eabdc71b812"}, true},
		{"data invalid version", true, map[string]any{"id": "d32c2ee6-fbde-0654-841b-0eabdc71b812"}, true},
		{"data invalid email", true, map[string]any{"email": "example.com"}, true},
		{"data email whitespace", true, map[string]any{"email": " oidc@example.com"}, true},
		{"data unknown ID", true, map[string]any{"id": tftypes.UnknownValue}, false},
		{"data unknown email", true, map[string]any{"email": tftypes.UnknownValue}, false},
		{"data computed name", true, map[string]any{"id": userTestID, "name": "not configurable"}, true},
		{"data computed role", true, map[string]any{"id": userTestID, "role": "member"}, true},
		{"data computed suspended", true, map[string]any{"id": userTestID, "suspended": false}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			server := providerserver.NewProtocol6(New("unit")())()
			config := userTestProtocolConfig(t, tc.values, tc.dataSource)
			var diagnostics []*tfprotov6.Diagnostic
			if tc.dataSource {
				response, err := server.ValidateDataResourceConfig(t.Context(), &tfprotov6.ValidateDataResourceConfigRequest{TypeName: "outline_user", Config: config})
				if err != nil {
					t.Fatal(err)
				}
				diagnostics = response.Diagnostics
			} else {
				response, err := server.ValidateResourceConfig(t.Context(), &tfprotov6.ValidateResourceConfigRequest{TypeName: "outline_user", Config: config})
				if err != nil {
					t.Fatal(err)
				}
				diagnostics = response.Diagnostics
			}
			if protocolHasError(diagnostics) != tc.invalid {
				t.Fatalf("validation error=%t, want=%t: %v", protocolHasError(diagnostics), tc.invalid, diagnostics)
			}
		})
	}
}

func TestUserSchemaAndConfigure(t *testing.T) {
	t.Parallel()
	r, d := NewUserResource().(*userResource), NewUserDataSource().(*userDataSource)
	var resourceMetadata resource.MetadataResponse
	r.Metadata(t.Context(), resource.MetadataRequest{ProviderTypeName: "outline"}, &resourceMetadata)
	var dataMetadata datasource.MetadataResponse
	d.Metadata(t.Context(), datasource.MetadataRequest{ProviderTypeName: "outline"}, &dataMetadata)
	if resourceMetadata.TypeName != "outline_user" || dataMetadata.TypeName != "outline_user" {
		t.Fatal("wrong user type names")
	}
	var schema resource.SchemaResponse
	r.Schema(t.Context(), resource.SchemaRequest{}, &schema)
	if schema.Diagnostics.HasError() || schema.Schema.ValidateImplementation(t.Context()).HasError() || len(schema.Schema.Attributes) != 8 {
		t.Fatalf("resource schema: %v", schema)
	}
	id := schema.Schema.Attributes["id"].(resourceschema.StringAttribute)
	email := schema.Schema.Attributes["email"].(resourceschema.StringAttribute)
	name := schema.Schema.Attributes["name"].(resourceschema.StringAttribute)
	role := schema.Schema.Attributes["role"].(resourceschema.StringAttribute)
	suspended := schema.Schema.Attributes["suspended"].(resourceschema.BoolAttribute)
	if !id.Computed || id.Optional || id.Required || len(id.PlanModifiers) != 1 || !email.Required || email.Optional || email.Computed || len(email.PlanModifiers) != 1 || !name.Optional || !name.Computed || name.Required || name.Default != nil || !role.Optional || !role.Computed || role.Default == nil || !suspended.Required || suspended.Optional || suspended.Computed || suspended.Default != nil {
		t.Fatalf("incorrect resource attribute flags: %v", schema.Schema.Attributes)
	}
	for _, field := range []string{"suppress_email", "delete_permanently", "allow_temporary_activation_for_role_change"} {
		attribute := schema.Schema.Attributes[field].(resourceschema.BoolAttribute)
		if !attribute.Optional || !attribute.Computed || attribute.Required || attribute.Default == nil {
			t.Fatalf("missing %s default", field)
		}
	}
	server := providerserver.NewProtocol6(New("unit")())()
	protocol, err := server.GetProviderSchema(t.Context(), &tfprotov6.GetProviderSchemaRequest{})
	if err != nil || protocolHasError(protocol.Diagnostics) {
		t.Fatalf("protocol schema: %v %v", protocol, err)
	}
	for label, schema := range map[string]*tfprotov6.Schema{"resource": protocol.ResourceSchemas["outline_user"], "data source": protocol.DataSourceSchemas["outline_user"]} {
		wantCount := 8
		if label == "data source" {
			wantCount = 5
		}
		if schema == nil || schema.Block == nil || len(schema.Block.Attributes) != wantCount || schema.Block.Description == "" {
			t.Fatalf("missing %s schema", label)
		}
		for _, attribute := range schema.Block.Attributes {
			wantType := tftypes.String
			if attribute.Name == "suspended" || attribute.Name == "suppress_email" || attribute.Name == "delete_permanently" || attribute.Name == "allow_temporary_activation_for_role_change" {
				wantType = tftypes.Bool
			}
			if !attribute.Type.Equal(wantType) || attribute.Sensitive || attribute.Description == "" {
				t.Fatalf("wrong protocol attribute: %+v", attribute)
			}
			if label == "data source" && (!attribute.Computed || attribute.Required || attribute.Optional != (attribute.Name == "id" || attribute.Name == "email")) {
				t.Fatalf("lookup flags: %+v", attribute)
			}
		}
	}
	api := userTestAdminClient(t, func(http.ResponseWriter, *http.Request) { t.Error("configure must not contact Outline") })
	for _, tc := range []struct {
		name    string
		data    any
		invalid bool
	}{{"nil", nil, false}, {"wrong type", "not a client", true}, {"typed nil", (*apiClient)(nil), true}, {"valid", api, false}} {
		t.Run(tc.name, func(t *testing.T) {
			r, d := &userResource{}, &userDataSource{}
			var rr resource.ConfigureResponse
			r.Configure(t.Context(), resource.ConfigureRequest{ProviderData: tc.data}, &rr)
			var dr datasource.ConfigureResponse
			d.Configure(t.Context(), datasource.ConfigureRequest{ProviderData: tc.data}, &dr)
			if rr.Diagnostics.HasError() != tc.invalid || dr.Diagnostics.HasError() != tc.invalid {
				t.Fatalf("configure: %v %v", rr.Diagnostics, dr.Diagnostics)
			}
			if tc.name == "valid" && (r.api != api || d.api != api) {
				t.Fatal("configured client not retained")
			}
		})
	}
}

func TestUserImportAndUUIDValidation(t *testing.T) {
	t.Parallel()
	for _, id := range []string{userTestID, "", "user", userTestEmail, "00000000-0000-0000-0000-000000000000", strings.ToUpper(userTestID), strings.ReplaceAll(userTestID, "-", ""), "{" + userTestID + "}", "urn:uuid:" + userTestID, " " + userTestID, "d32c2ee6-fbde-4654-041b-0eabdc71b812", "d32c2ee6-fbde-0654-841b-0eabdc71b812"} {
		t.Run(id, func(t *testing.T) {
			t.Parallel()
			valid := id == userTestID
			var calls atomic.Int32
			r := &userResource{api: userTestAdminClient(t, func(http.ResponseWriter, *http.Request) {
				calls.Add(1)
				t.Error("UUID validation or import contacted Outline")
			})}
			state := tfsdk.State(userTestPlan(t, r, userModel{}))
			response := resource.ImportStateResponse{State: state}
			r.ImportState(t.Context(), resource.ImportStateRequest{ID: id}, &response)
			if response.Diagnostics.HasError() == valid {
				t.Fatalf("import validation: %v", response.Diagnostics)
			}
			if valid {
				got := userTestStateModel(t, response.State)
				if got.ID.ValueString() != id || !got.SuppressEmail.ValueBool() || got.DeletePermanently.IsNull() || got.DeletePermanently.ValueBool() || got.AllowTemporaryActivationForRoleChange != types.BoolValue(false) || !got.Email.IsNull() || !got.Suspended.IsNull() {
					t.Fatalf("import state: %+v", got)
				}
			} else {
				if !response.State.Raw.Equal(state.Raw) {
					t.Fatal("invalid import changed state")
				}
				model := userTestModel()
				model.ID = types.StringValue(id)
				state = tfsdk.State(userTestPlan(t, r, model))
				read := resource.ReadResponse{State: state}
				r.Read(t.Context(), resource.ReadRequest{State: state}, &read)
				groupTestDiagnostics(t, read.Diagnostics, "canonical lowercase")
				updated := userTestUpdate(t, r, model, true)
				groupTestDiagnostics(t, updated.Diagnostics, "canonical lowercase")
				deleted := resource.DeleteResponse{State: state}
				r.Delete(t.Context(), resource.DeleteRequest{State: state}, &deleted)
				groupTestDiagnostics(t, deleted.Diagnostics, "canonical lowercase")
			}
			if calls.Load() != 0 {
				t.Fatal("invalid ID made an API call")
			}
		})
	}
}

func TestUserProtocolPlanDefaultsIdentityAndReplacement(t *testing.T) {
	t.Parallel()
	for _, existing := range []bool{false, true} {
		for _, changeEmail := range []bool{false, true} {
			t.Run(fmt.Sprintf("existing=%t changeEmail=%t", existing, changeEmail), func(t *testing.T) {
				t.Parallel()
				server := providerserver.NewProtocol6(New("unit")())()
				var schema resource.SchemaResponse
				NewUserResource().Schema(t.Context(), resource.SchemaRequest{}, &schema)
				typ := schema.Schema.Type().TerraformType(t.Context())
				null, err := tfprotov6.NewDynamicValue(typ, tftypes.NewValue(typ, nil))
				if err != nil {
					t.Fatal(err)
				}
				prior := &null
				if existing {
					prior = userTestProtocolConfig(t, map[string]any{"id": userTestID, "email": userTestEmail, "name": "OIDC User", "role": "member", "suspended": false, "suppress_email": true, "delete_permanently": false, "allow_temporary_activation_for_role_change": false}, false)
				}
				email := userTestEmail
				if changeEmail {
					email = "changed@example.com"
				}
				response, err := server.PlanResourceChange(t.Context(), &tfprotov6.PlanResourceChangeRequest{TypeName: "outline_user", PriorState: prior,
					Config:           userTestProtocolConfig(t, map[string]any{"email": email, "suspended": true}, false),
					ProposedNewState: userTestProtocolConfig(t, map[string]any{"id": tftypes.UnknownValue, "email": email, "name": tftypes.UnknownValue, "role": tftypes.UnknownValue, "suspended": true, "suppress_email": tftypes.UnknownValue, "delete_permanently": tftypes.UnknownValue, "allow_temporary_activation_for_role_change": tftypes.UnknownValue}, false),
				})
				if err != nil || protocolHasError(response.Diagnostics) || response.PlannedState == nil {
					t.Fatalf("plan: %v %v", response, err)
				}
				wantReplacement := existing && changeEmail
				if (len(response.RequiresReplace) != 0) != wantReplacement {
					t.Fatalf("email replacement=%t, paths=%v", wantReplacement, response.RequiresReplace)
				}
				if wantReplacement && (len(response.RequiresReplace) != 1 || !response.RequiresReplace[0].Equal(tftypes.NewAttributePath().WithAttributeName("email"))) {
					t.Fatalf("wrong replacement path: %v", response.RequiresReplace)
				}
				value, err := response.PlannedState.Unmarshal(typ)
				if err != nil {
					t.Fatal(err)
				}
				var fields map[string]tftypes.Value
				if err := value.As(&fields); err != nil {
					t.Fatal(err)
				}
				var role string
				if err := fields["role"].As(&role); err != nil || role != "member" {
					t.Fatalf("role default: %q %v", role, err)
				}
				for field, want := range map[string]bool{"suppress_email": true, "delete_permanently": false, "suspended": true, "allow_temporary_activation_for_role_change": false} {
					var got bool
					if err := fields[field].As(&got); err != nil || got != want {
						t.Fatalf("%s=%t want=%t: %v", field, got, want, err)
					}
				}
				if existing {
					var id string
					if err := fields["id"].As(&id); err != nil || id != userTestID {
						t.Fatalf("ID not retained: %q %v", id, err)
					}
				} else if fields["id"].IsKnown() {
					t.Fatal("new identity should be unknown")
				}
			})
		}
	}
}
