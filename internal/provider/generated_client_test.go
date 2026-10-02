// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/glitchedmob/terraform-provider-outline/internal/client"
)

func TestAPIClientGeneratedMethods(t *testing.T) {
	t.Parallel()
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		if request.Method != http.MethodPost || request.Header.Get("Authorization") != "Bearer test-key" || request.Header.Get("User-Agent") != "terraform-provider-outline/test" {
			t.Error("generated calls must use POST and the provider's authenticated transport")
		}
		if request.Header.Get("X-Client-Version") != "" || request.Header.Get("X-Api-Version") != "" {
			t.Error("do not opt into application pagination or versioned collection presentation")
		}
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/proxy/api/users.invite":
			if request.Header.Get("Content-Type") != "application/json" {
				t.Error("generated JSON calls must set Content-Type")
			}
			var body client.UsersInviteJSONRequestBody
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if len(body.Invites) != 1 || body.Invites[0].Email != "oidc@example.com" || body.SuppressEmail == nil || !*body.SuppressEmail {
				t.Error("generated invite fields were not sent")
			}
			_, _ = writer.Write([]byte(`{"ok":true,"status":200,"data":{"sent":[],"unsent":[],"users":[]}}`))
		case "/proxy/api/auth.info":
			_, _ = writer.Write([]byte(`{"ok":true,"status":200,"data":{"user":{"name":"Administrator","role":"admin"},"team":{"name":"Example"}}}`))
		default:
			t.Errorf("unexpected request path %q", request.URL.Path)
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	api, err := newAPIClient(server.URL+"/proxy/api///", "test-key", 30, "test")
	if err != nil {
		t.Fatal(err)
	}
	if api.ClientWithResponses == nil || requests.Load() != 0 || api.httpClient.Timeout != 30*time.Second {
		t.Fatal("construct generated methods locally with the configured timeout")
	}
	suppressEmail := true
	invited, err := api.UsersInviteWithResponse(t.Context(), client.UsersInviteJSONRequestBody{
		Invites: []client.Invite{{Name: "OIDC User", Email: "oidc@example.com", Role: client.UserRoleMember}}, SuppressEmail: &suppressEmail,
	})
	if err != nil || invited.JSON200 == nil || invited.JSON200.Data == nil || invited.StatusCode() != http.StatusOK {
		t.Fatalf("unexpected invite response: %v", err)
	}
	auth, err := api.AuthInfoWithResponse(t.Context())
	if err != nil || auth.JSON200 == nil || auth.JSON200.Data == nil || auth.JSON200.Data.User == nil || auth.JSON200.Data.User.Role == nil || *auth.JSON200.Data.User.Role != client.UserRoleAdmin {
		t.Fatalf("unexpected explicit auth.info response: %v", err)
	}
	if requests.Load() != 2 {
		t.Fatal("only the two explicit calls should reach the API")
	}
}

func TestAPIClientGeneratedRedirectAndOriginProtection(t *testing.T) {
	t.Parallel()
	sink := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("credentials must not reach another origin")
	}))
	defer sink.Close()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/auth.info" {
			t.Error("generated client must not follow redirects")
		}
		http.Redirect(writer, request, sink.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	api, err := newAPIClient(server.URL+"/api", "test-key", 30, "test")
	if err != nil {
		t.Fatal(err)
	}
	response, err := api.AuthInfoWithResponse(t.Context())
	if err != nil || response.StatusCode() != http.StatusTemporaryRedirect || response.JSON200 != nil {
		t.Fatalf("expected an unparsed redirect response, got %v", err)
	}
	response, err = api.AuthInfoWithResponse(t.Context(), func(_ context.Context, request *http.Request) error {
		request.URL.Host = strings.TrimPrefix(sink.URL, "http://")
		return nil
	})
	if err == nil || response != nil || !strings.Contains(err.Error(), "different origin") {
		t.Fatalf("generated request editors must not bypass origin protection: %v", err)
	}
}

func TestAPIClientGeneratedTimeoutAndContext(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name       string
		contextual bool
	}{
		{name: "HTTP timeout"},
		{name: "context cancellation", contextual: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			started := make(chan struct{})
			release := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				close(started)
				<-release
			}))
			defer server.Close()
			defer close(release)
			api, err := newAPIClient(server.URL+"/api", "test-key", 1, "test")
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if test.contextual {
				go func() {
					select {
					case <-started:
						cancel()
					case <-ctx.Done():
					}
				}()
			}
			response, err := api.AuthInfoWithResponse(ctx)
			if err == nil || response != nil {
				t.Fatal("generated call must stop before the server responds")
			}
			if test.contextual && !errors.Is(err, context.Canceled) {
				t.Fatalf("expected context cancellation: %v", err)
			}
			if !test.contextual && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("expected configured HTTP timeout: %v", err)
			}
		})
	}
}
