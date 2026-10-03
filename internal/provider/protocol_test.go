// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

func TestProtocol6Schema(t *testing.T) {
	t.Parallel()
	server := providerserver.NewProtocol6(New("test")())()
	response, err := server.GetProviderSchema(t.Context(), &tfprotov6.GetProviderSchemaRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Diagnostics) != 0 || response.Provider == nil || response.Provider.Block == nil {
		t.Fatalf("unexpected protocol schema response: %v", response)
	}
	if len(response.ResourceSchemas) != 6 || len(response.DataSourceSchemas) != 3 || len(response.Functions) != 0 || len(response.EphemeralResourceSchemas) != 0 {
		t.Fatal("expected six IAM resources and three lookup data sources, without functions or ephemeral resources")
	}
	for _, name := range []string{"outline_group_member", "outline_collection_group", "outline_collection_user"} {
		if response.ResourceSchemas[name] == nil || response.DataSourceSchemas[name] != nil {
			t.Fatalf("expected only a resource protocol schema for %s", name)
		}
	}
	for _, name := range []string{"outline_group", "outline_user", "outline_collection"} {
		if response.ResourceSchemas[name] == nil || response.DataSourceSchemas[name] == nil {
			t.Fatalf("missing %s protocol registration", name)
		}
	}
	if len(response.Provider.Block.Attributes) != 3 {
		t.Fatal("expected three protocol provider attributes")
	}
	for _, attribute := range response.Provider.Block.Attributes {
		if !attribute.Optional || attribute.Required || attribute.Computed || attribute.Description == "" {
			t.Fatalf("unexpected protocol attribute: %v", attribute)
		}
		wantType := tftypes.String
		if attribute.Name == "timeout_seconds" {
			wantType = tftypes.Number
		}
		if !attribute.Type.Equal(wantType) || attribute.Sensitive != (attribute.Name == "api_key") {
			t.Fatalf("unexpected type or sensitivity for %s", attribute.Name)
		}
	}
}

func TestProtocol6ValidateProviderConfig(t *testing.T) {
	t.Parallel()
	for name, test := range map[string]struct {
		timeout   any
		wantError bool
	}{
		"null uses default": {},
		"positive":          {timeout: 5},
		"maximum":           {timeout: maxTimeoutSeconds},
		"unknown":           {timeout: tftypes.UnknownValue},
		"zero":              {timeout: 0, wantError: true},
		"negative":          {timeout: -1, wantError: true},
		"overflow":          {timeout: maxTimeoutSeconds + 1, wantError: true},
		"fractional":        {timeout: 1.5, wantError: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			server := providerserver.NewProtocol6(New("test")())()
			config := testProtocolConfig(t, map[string]any{"timeout_seconds": test.timeout})
			response, err := server.ValidateProviderConfig(t.Context(), &tfprotov6.ValidateProviderConfigRequest{Config: config})
			if err != nil {
				t.Fatal(err)
			}
			if protocolHasError(response.Diagnostics) != test.wantError {
				t.Fatalf("unexpected validation diagnostics: %v", response.Diagnostics)
			}
		})
	}
}

func TestProtocol6ConfigureProvider(t *testing.T) {
	t.Setenv("OUTLINE_API_KEY", "")
	t.Setenv("OUTLINE_BASE_URL", "")
	var requests atomic.Int32
	fixture := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
	}))
	defer fixture.Close()
	for name, test := range map[string]struct {
		key       any
		wantError bool
	}{
		"known key":   {key: "test-key"},
		"unknown key": {key: tftypes.UnknownValue, wantError: true},
		"missing key": {wantError: true},
	} {
		t.Run(name, func(t *testing.T) {
			server := providerserver.NewProtocol6(New("test")())()
			config := testProtocolConfig(t, map[string]any{"base_url": fixture.URL + "/api", "api_key": test.key})
			response, err := server.ConfigureProvider(t.Context(), &tfprotov6.ConfigureProviderRequest{
				Config: config, TerraformVersion: "1.14.7",
			})
			if err != nil {
				t.Fatal(err)
			}
			if protocolHasError(response.Diagnostics) != test.wantError {
				t.Fatalf("unexpected configure diagnostics: %v", response.Diagnostics)
			}
		})
	}
	if requests.Load() != 0 {
		t.Fatal("protocol configuration must not contact Outline")
	}
}

func testProtocolConfig(t *testing.T, values map[string]any) *tfprotov6.DynamicValue {
	t.Helper()
	value := testProviderConfigValue(values)
	dynamic, err := tfprotov6.NewDynamicValue(value.Type(), value)
	if err != nil {
		t.Fatal(err)
	}
	return &dynamic
}

func protocolHasError(diagnostics []*tfprotov6.Diagnostic) bool {
	for _, diagnostic := range diagnostics {
		if diagnostic.Severity == tfprotov6.DiagnosticSeverityError {
			return true
		}
	}
	return false
}
