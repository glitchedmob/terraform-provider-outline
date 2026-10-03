// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/glitchedmob/terraform-provider-outline/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/provider"
)

func TestProviderConfigureClientIsolation(t *testing.T) {
	t.Parallel()
	type configuredProvider struct {
		name     string
		key      string
		apiPath  string
		server   *httptest.Server
		api      *apiClient
		requests atomic.Int32
	}
	fixtures := []*configuredProvider{
		{name: "primary", key: "fake-primary-key", apiPath: "/primary/api"},
		{name: "alias", key: "fake-alias-key", apiPath: "/alias/api"},
	}
	factory := New("test")
	for _, fixture := range fixtures {
		fixture.server = httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			fixture.requests.Add(1)
			if request.Method != http.MethodPost || request.Host != fixture.server.Listener.Addr().String() {
				t.Errorf("%s received an unexpected method or origin: %s %s", fixture.name, request.Method, request.Host)
			}
			if authorization := request.Header.Values("Authorization"); len(authorization) != 1 || authorization[0] != "Bearer "+fixture.key {
				t.Errorf("%s received authorization that does not match its configured key", fixture.name)
			}
			writer.Header().Set("Content-Type", "application/json")
			writer.Header().Set("X-Test-Origin", fixture.name)
			switch request.URL.Path {
			case fixture.apiPath + "/auth.info":
				_, _ = writer.Write([]byte(`{"ok":true,"status":200,"data":{"user":{"name":"Test User"}}}`))
			case fixture.apiPath + "/groups.list":
				if request.Header.Get("Content-Type") != "application/json" {
					t.Errorf("%s generated groups.list request must use JSON", fixture.name)
				}
				_, _ = writer.Write([]byte(`{"ok":true,"status":200,"data":{"groups":[],"groupMemberships":[]}}`))
			default:
				t.Errorf("%s received an unexpected API path: %s", fixture.name, request.URL.Path)
				writer.WriteHeader(http.StatusNotFound)
			}
		}))
		fixture.server.Start()
		t.Cleanup(fixture.server.Close)

		p := factory()
		var response provider.ConfigureResponse
		p.Configure(t.Context(), provider.ConfigureRequest{Config: testProviderConfig(t, p, map[string]any{
			"base_url": fixture.server.URL + fixture.apiPath, "api_key": fixture.key, "timeout_seconds": 5,
		})}, &response)
		if response.Diagnostics.HasError() {
			t.Fatalf("configure %s: %v", fixture.name, response.Diagnostics)
		}
		var ok bool
		fixture.api, ok = response.ResourceData.(*apiClient)
		if !ok || fixture.api == nil || response.DataSourceData != fixture.api {
			t.Fatalf("%s must provide its client to both resources and data sources", fixture.name)
		}
	}
	if fixtures[0].api == fixtures[1].api {
		t.Fatal("separately configured providers must not share a client")
	}
	for _, fixture := range fixtures {
		if fixture.requests.Load() != 0 {
			t.Fatalf("Configure must not contact the %s server", fixture.name)
		}
	}

	authInfo := func(fixture *configuredProvider) {
		t.Helper()
		response, err := fixture.api.AuthInfoWithResponse(t.Context())
		if err != nil || response == nil || response.StatusCode() != http.StatusOK || response.JSON200 == nil {
			t.Errorf("%s auth.info failed: %v", fixture.name, err)
			return
		}
		if response.HTTPResponse.Header.Get("X-Test-Origin") != fixture.name {
			t.Errorf("%s auth.info response came from another provider's origin", fixture.name)
		}
	}
	listGroups := func(fixture *configuredProvider) {
		t.Helper()
		response, err := fixture.api.GroupsListWithResponse(t.Context(), client.GroupsListJSONRequestBody{})
		if err != nil || response == nil || response.StatusCode() != http.StatusOK || response.JSON200 == nil {
			t.Errorf("%s groups.list failed: %v", fixture.name, err)
			return
		}
		if response.HTTPResponse.Header.Get("X-Test-Origin") != fixture.name {
			t.Errorf("%s groups.list response came from another provider's origin", fixture.name)
		}
	}
	// Use both clients after both providers have been configured.
	for _, call := range []func(*configuredProvider){authInfo, listGroups} {
		for _, fixture := range fixtures {
			call(fixture)
		}
	}

	const concurrentRounds = 4
	start := make(chan struct{})
	var requests sync.WaitGroup
	for range concurrentRounds {
		for _, fixture := range fixtures {
			requests.Go(func() {
				<-start
				authInfo(fixture)
				listGroups(fixture)
			})
		}
	}
	close(start)
	requests.Wait()
	const wantRequests = int32(2 + 2*concurrentRounds)
	for _, fixture := range fixtures {
		if count := fixture.requests.Load(); count != wantRequests {
			t.Fatalf("%s received %d requests, want %d", fixture.name, count, wantRequests)
		}
	}

	for i, fixture := range fixtures {
		other := fixtures[1-i]
		var rejectedRequest *http.Request
		response, err := fixture.api.AuthInfoWithResponse(t.Context(), func(_ context.Context, request *http.Request) error {
			request.URL.Host = other.server.Listener.Addr().String()
			request.URL.Path = other.apiPath + "/auth.info"
			rejectedRequest = request
			return nil
		})
		if err == nil || response != nil || !strings.Contains(err.Error(), "different origin") {
			t.Fatalf("%s generated client must reject the %s origin: %v", fixture.name, other.name, err)
		}
		for _, configured := range fixtures {
			if strings.Contains(err.Error(), configured.key) {
				t.Fatal("cross-origin rejection leaked a configured API key")
			}
		}
		if rejectedRequest == nil || rejectedRequest.Header.Get("Authorization") != "" {
			t.Fatal("a rejected generated request must not acquire an Authorization header")
		}
	}
	for _, fixture := range fixtures {
		if count := fixture.requests.Load(); count != wantRequests {
			t.Fatalf("%s received a forbidden cross-origin request", fixture.name)
		}
	}

	// A rejected request must not change either client's configured origin or key.
	for _, fixture := range fixtures {
		authInfo(fixture)
		if count := fixture.requests.Load(); count != wantRequests+1 {
			t.Fatalf("%s did not resume requests to its own origin", fixture.name)
		}
	}
}
