// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestAPIClientRequestHeaders(t *testing.T) {
	t.Parallel()
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		if request.URL.Path != "/api/ping" || request.Method != http.MethodPost {
			t.Error("expected POST to the configured API path")
		}
		if request.Header.Get("Authorization") != "Bearer test-key" {
			t.Error("expected API key bearer authorization")
		}
		if request.Header.Get("User-Agent") != "terraform-provider-outline/test" {
			t.Error("expected provider user agent")
		}
		if request.Header.Get("Content-Type") != "application/json" || request.Header.Get("X-Custom") != "preserved" {
			t.Error("transport must preserve other request headers")
		}
		if request.Header.Get("X-API-Key") != "" {
			t.Error("authentication must use bearer authorization, not X-API-Key")
		}
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	client, err := newAPIClient(server.URL+"/api/", "test-key", 30, "test")
	if err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 0 {
		t.Fatal("client construction must not make API requests")
	}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, client.baseURL+"/ping", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Custom", "preserved")
	request.Header.Set("Authorization", "original")
	for range 2 {
		response, err := client.httpClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		if err := response.Body.Close(); err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != http.StatusNoContent {
			t.Fatalf("unexpected HTTP status: %d", response.StatusCode)
		}
		if request.Header.Get("Authorization") != "original" || request.Header.Get("User-Agent") != "" {
			t.Fatal("transport mutated the caller's request")
		}
	}
	if requests.Load() != 2 {
		t.Fatal("expected two explicit requests and no authentication requests")
	}
}

func TestAPIClientDoesNotFollowRedirects(t *testing.T) {
	t.Parallel()
	sink := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("credentials must not reach a redirect target")
	}))
	defer sink.Close()
	for name, location := range map[string]string{"same origin": "/redirect-target", "different origin": sink.URL} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.URL.Path != "/api/ping" {
					t.Error("redirect must not be followed")
				}
				http.Redirect(writer, request, location, http.StatusTemporaryRedirect)
			}))
			defer server.Close()
			client, err := newAPIClient(server.URL+"/api", "test-key", 30, "test")
			if err != nil {
				t.Fatal(err)
			}
			request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, client.baseURL+"/ping", nil)
			if err != nil {
				t.Fatal(err)
			}
			response, err := client.httpClient.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			if err := response.Body.Close(); err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != http.StatusTemporaryRedirect {
				t.Fatalf("expected HTTP redirect response, got %d", response.StatusCode)
			}
		})
	}
}

func TestAPIClientRejectsOtherOrigins(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("a different origin must not receive a request")
	}))
	defer server.Close()
	client, err := newAPIClient("https://outline.example/api", "test-key", 30, "test")
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.httpClient.Do(request)
	if err == nil || response != nil || !strings.Contains(err.Error(), "different origin") {
		t.Fatalf("expected origin rejection, got %v", err)
	}
	if request.Header.Get("Authorization") != "" {
		t.Fatal("rejected request must not be modified")
	}
}

func TestAPIClientRejectsInvalidConfiguration(t *testing.T) {
	t.Parallel()
	for name, test := range map[string]struct {
		baseURL string
		apiKey  string
		timeout int64
	}{
		"empty URL":            {"", "test-key", 30},
		"relative URL":         {"outline.example/api", "test-key", 30},
		"unsupported protocol": {"ftp://outline.example/api", "test-key", 30},
		"missing host":         {"https:///api", "test-key", 30},
		"empty hostname":       {"https://:443/api", "test-key", 30},
		"invalid URL":          {"%zz-fake-secret", "test-key", 30},
		"query string":         {"https://outline.example/api?secret=fake-secret", "test-key", 30},
		"empty query":          {"https://outline.example/api?", "test-key", 30},
		"fragment":             {"https://outline.example/api#fake-secret", "test-key", 30},
		"empty fragment":       {"https://outline.example/api#", "test-key", 30},
		"URL credentials":      {"https://user:fake-secret@outline.example/api", "test-key", 30},
		"missing key":          {defaultBaseURL, "", 30},
		"blank key":            {defaultBaseURL, " ", 30},
		"key whitespace":       {defaultBaseURL, "fake-secret key", 30},
		"key control":          {defaultBaseURL, "fake-secret\x00key", 30},
		"zero timeout":         {defaultBaseURL, "test-key", 0},
		"negative timeout":     {defaultBaseURL, "test-key", -1},
		"overflowing timeout":  {defaultBaseURL, "test-key", maxTimeoutSeconds + 1},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			client, err := newAPIClient(test.baseURL, test.apiKey, test.timeout, "test")
			if err == nil || client != nil {
				t.Fatal("expected invalid configuration to fail without a client")
			}
			if strings.Contains(err.Error(), "fake-secret") {
				t.Fatal("client configuration error leaked secret input")
			}
		})
	}
}
