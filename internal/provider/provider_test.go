// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/provider"
	providerschema "github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

func TestProviderMetadata(t *testing.T) {
	t.Parallel()
	var response provider.MetadataResponse
	New("test")().Metadata(t.Context(), provider.MetadataRequest{}, &response)
	if response.TypeName != "outline" || response.Version != "test" {
		t.Fatalf("unexpected metadata: %v", response)
	}
}

func TestProviderSchema(t *testing.T) {
	t.Parallel()
	var response provider.SchemaResponse
	p := New("test")()
	p.Schema(t.Context(), provider.SchemaRequest{}, &response)
	if response.Diagnostics.HasError() {
		t.Fatal(response.Diagnostics)
	}
	if diags := response.Schema.ValidateImplementation(t.Context()); diags.HasError() {
		t.Fatal(diags)
	}
	if len(response.Schema.Attributes) != 3 || len(response.Schema.Blocks) != 0 {
		t.Fatal("expected three provider attributes and no blocks")
	}
	for _, name := range []string{"base_url", "api_key"} {
		attribute, ok := response.Schema.Attributes[name].(providerschema.StringAttribute)
		if !ok || !attribute.Optional || attribute.Required || attribute.Sensitive != (name == "api_key") || attribute.MarkdownDescription == "" {
			t.Fatalf("unexpected %s schema: %v", name, attribute)
		}
	}
	timeout, ok := response.Schema.Attributes["timeout_seconds"].(providerschema.Int64Attribute)
	if !ok || !timeout.Optional || timeout.Required || timeout.Sensitive || len(timeout.Validators) != 1 || timeout.MarkdownDescription == "" {
		t.Fatalf("unexpected timeout_seconds schema: %v", timeout)
	}
	if len(p.Resources(t.Context())) != 1 || len(p.DataSources(t.Context())) != 1 {
		t.Fatal("expected the group resource and data source")
	}
}

func TestProviderConfigure(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
	}))
	defer server.Close()

	for name, test := range map[string]struct {
		values      map[string]any
		envKey      string
		envURL      string
		wantURL     string
		wantKey     string
		wantTimeout time.Duration
		wantError   string
	}{
		"defaults": {
			envKey: "environment-key", wantURL: defaultBaseURL, wantKey: "environment-key", wantTimeout: 30 * time.Second,
		},
		"environment": {
			envKey: " environment-key ", envURL: server.URL + "/api/",
			wantURL: server.URL + "/api", wantKey: "environment-key", wantTimeout: 30 * time.Second,
		},
		"explicit overrides environment": {
			values: map[string]any{"base_url": " " + server.URL + "/api/ ", "api_key": " configured-key ", "timeout_seconds": 5},
			envKey: "environment-key", envURL: "https://environment.example/api",
			wantURL: server.URL + "/api", wantKey: "configured-key", wantTimeout: 5 * time.Second,
		},
		"unknown base URL does not use environment": {
			values: map[string]any{"base_url": tftypes.UnknownValue}, envURL: server.URL, envKey: "environment-key",
			wantError: "Unknown Outline Base URL",
		},
		"unknown key does not use environment": {
			values: map[string]any{"api_key": tftypes.UnknownValue}, envKey: "environment-key",
			wantError: "Unknown Outline API Key",
		},
		"unknown timeout": {
			values: map[string]any{"timeout_seconds": tftypes.UnknownValue}, envKey: "environment-key",
			wantError: "Unknown Outline Timeout",
		},
		"missing key":           {wantError: "set api_key or OUTLINE_API_KEY"},
		"blank environment key": {envKey: " \t ", wantError: "set api_key or OUTLINE_API_KEY"},
		"empty explicit key does not use environment": {
			values: map[string]any{"api_key": ""}, envKey: "environment-key", wantError: "set api_key or OUTLINE_API_KEY",
		},
		"blank explicit key": {
			values: map[string]any{"api_key": " "}, envKey: "environment-key", wantError: "set api_key or OUTLINE_API_KEY",
		},
		"empty explicit URL does not use environment": {
			values: map[string]any{"base_url": ""}, envKey: "environment-key", envURL: server.URL, wantError: "base_url must use HTTP or HTTPS",
		},
		"invalid URL": {
			values: map[string]any{"base_url": "%zz-fake-secret"}, envKey: "environment-key", wantError: "base_url must be a valid",
		},
		"URL credentials": {
			values: map[string]any{"base_url": "https://user:fake-secret@example.com/api"}, envKey: "environment-key",
			wantError: "base_url must not include user credentials",
		},
		"key header injection": {
			values: map[string]any{"api_key": "fake-secret\r\nInjected: header"}, wantError: "api_key must not contain whitespace",
		},
		"zero timeout": {
			values: map[string]any{"timeout_seconds": 0}, envKey: "environment-key", wantError: "timeout_seconds must be between",
		},
		"negative timeout": {
			values: map[string]any{"timeout_seconds": -1}, envKey: "environment-key", wantError: "timeout_seconds must be between",
		},
		"overflowing timeout": {
			values: map[string]any{"timeout_seconds": maxTimeoutSeconds + 1}, envKey: "environment-key", wantError: "timeout_seconds must be between",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("OUTLINE_BASE_URL", test.envURL)
			t.Setenv("OUTLINE_API_KEY", test.envKey)
			p := New("test")()
			var response provider.ConfigureResponse
			p.Configure(t.Context(), provider.ConfigureRequest{Config: testProviderConfig(t, p, test.values)}, &response)
			if test.wantError != "" {
				if !response.Diagnostics.HasError() || !strings.Contains(response.Diagnostics[0].Summary()+" "+response.Diagnostics[0].Detail(), test.wantError) {
					t.Fatalf("expected %q, got %v", test.wantError, response.Diagnostics)
				}
				for _, diagnostic := range response.Diagnostics {
					if strings.Contains(diagnostic.Summary()+" "+diagnostic.Detail(), "fake-secret") {
						t.Fatal("configuration diagnostics leaked a secret")
					}
				}
				if response.ResourceData != nil || response.DataSourceData != nil {
					t.Fatal("invalid configuration must not provide a client")
				}
				return
			}
			if response.Diagnostics.HasError() {
				t.Fatal(response.Diagnostics)
			}
			client, ok := response.ResourceData.(*apiClient)
			if !ok || client == nil || response.DataSourceData != client {
				t.Fatal("expected the same non-nil client for resources and data sources")
			}
			if client.baseURL != test.wantURL || client.httpClient.Timeout != test.wantTimeout {
				t.Fatalf("unexpected client URL or timeout: %q, %v", client.baseURL, client.httpClient.Timeout)
			}
			transport, ok := client.httpClient.Transport.(*bearerTransport)
			if !ok || transport.apiKey != test.wantKey || transport.userAgent != "terraform-provider-outline/test" {
				t.Fatal("unexpected bearer transport configuration")
			}
		})
	}
	if count := requests.Load(); count != 0 {
		t.Fatalf("Configure made %d API requests", count)
	}
}

func TestProviderConfigureMalformedConfig(t *testing.T) {
	t.Parallel()
	p := New("test")()
	config := testProviderConfig(t, p, nil)
	config.Raw = tftypes.NewValue(tftypes.String, "invalid")
	var response provider.ConfigureResponse
	p.Configure(t.Context(), provider.ConfigureRequest{Config: config}, &response)
	if !response.Diagnostics.HasError() || response.ResourceData != nil || response.DataSourceData != nil {
		t.Fatal("malformed configuration must fail without providing a client")
	}
}

func testProviderConfig(t *testing.T, p provider.Provider, values map[string]any) tfsdk.Config {
	t.Helper()
	var response provider.SchemaResponse
	p.Schema(t.Context(), provider.SchemaRequest{}, &response)
	if response.Diagnostics.HasError() {
		t.Fatal(response.Diagnostics)
	}
	return tfsdk.Config{Schema: response.Schema, Raw: testProviderConfigValue(values)}
}

func testProviderConfigValue(values map[string]any) tftypes.Value {
	attributeTypes := map[string]tftypes.Type{
		"base_url": tftypes.String, "api_key": tftypes.String, "timeout_seconds": tftypes.Number,
	}
	attributes := make(map[string]tftypes.Value, len(attributeTypes))
	for name, attributeType := range attributeTypes {
		attributes[name] = tftypes.NewValue(attributeType, values[name])
	}
	return tftypes.NewValue(tftypes.Object{AttributeTypes: attributeTypes}, attributes)
}
