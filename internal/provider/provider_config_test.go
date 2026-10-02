// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/types"
)

func TestResolveProviderConfig(t *testing.T) {
	t.Parallel()
	environment := map[string]string{
		"OUTLINE_BASE_URL": " https://environment.example/api ",
		"OUTLINE_API_KEY":  " environment-key ",
	}
	for name, test := range map[string]struct {
		config      OutlineProviderModel
		environment map[string]string
		wantURL     string
		wantKey     string
		wantTimeout int64
	}{
		"defaults": {wantURL: defaultBaseURL, wantTimeout: defaultTimeoutSeconds},
		"environment": {
			environment: environment, wantURL: "https://environment.example/api", wantKey: "environment-key", wantTimeout: defaultTimeoutSeconds,
		},
		"configuration overrides environment": {
			config: OutlineProviderModel{
				BaseURL: types.StringValue(" https://configuration.example/api "), APIKey: types.StringValue(" configured-key "),
				TimeoutSeconds: types.Int64Value(60),
			},
			environment: environment, wantURL: "https://configuration.example/api", wantKey: "configured-key", wantTimeout: 60,
		},
		"mixed configuration and environment": {
			config:      OutlineProviderModel{APIKey: types.StringValue("configured-key")},
			environment: environment, wantURL: "https://environment.example/api", wantKey: "configured-key", wantTimeout: defaultTimeoutSeconds,
		},
		"explicit empty values do not use environment or defaults": {
			config:      OutlineProviderModel{BaseURL: types.StringValue(""), APIKey: types.StringValue(""), TimeoutSeconds: types.Int64Value(0)},
			environment: environment,
		},
		"blank environment URL uses default": {
			environment: map[string]string{"OUTLINE_BASE_URL": " "}, wantURL: defaultBaseURL, wantTimeout: defaultTimeoutSeconds,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			baseURL, apiKey, timeout := resolveProviderConfig(test.config, func(key string) string { return test.environment[key] })
			if baseURL != test.wantURL || apiKey != test.wantKey || timeout != test.wantTimeout {
				t.Fatalf("unexpected resolved URL, key, or timeout: %q, %q, %d", baseURL, apiKey, timeout)
			}
		})
	}
}
