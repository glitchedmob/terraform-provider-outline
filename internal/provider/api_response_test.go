// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/glitchedmob/terraform-provider-outline/internal/client"
	"github.com/google/uuid"
	"golang.org/x/time/rate"
)

func TestCheckResponseNeverEstablishesAbsence(t *testing.T) {
	t.Parallel()
	api := &apiClient{apiKey: groupTestKey}
	for _, tc := range []struct {
		name       string
		response   *http.Response
		body       string
		requestErr error
		want       string
		identity   error
	}{
		{"raw 404", &http.Response{StatusCode: 404}, `{"error":"not_found","message":"denied ` + groupTestKey + `"}`, nil, "HTTP 404 Not Found: not_found: denied [REDACTED]", nil},
		{"non JSON 404", &http.Response{StatusCode: 404}, `<html>` + groupTestKey, nil, "HTTP 404", nil},
		{"malformed body", &http.Response{StatusCode: 404}, `{`, nil, "HTTP 404", nil},
		{"nil response", nil, "", nil, "missing HTTP response", nil},
		{"nil response with request error", nil, "", fmt.Errorf("transport %s", groupTestKey), "request failed or response could not be decoded", nil},
		{"404 with decoder error", &http.Response{StatusCode: 404}, "", fmt.Errorf("decoder %s", groupTestKey), "request failed or response could not be decoded", nil},
		{"200 with decoder error", &http.Response{StatusCode: 200}, "", errors.New(groupTestKey), "request failed or response could not be decoded", nil},
		{"canceled 404", &http.Response{StatusCode: 404}, "", context.Canceled, "request canceled", nil},
		{"expired 404", &http.Response{StatusCode: 404}, "", context.DeadlineExceeded, "request deadline exceeded", nil},
		{"wait budget", &http.Response{StatusCode: 404}, "", fmt.Errorf("wrapped: %w", errRateLimitWaitBudget), errRateLimitWaitBudget.Error(), errRateLimitWaitBudget},
		{"retry limit", nil, "", fmt.Errorf("wrapped: %w", errRateLimitRetryLimit), errRateLimitRetryLimit.Error(), errRateLimitRetryLimit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := api.checkResponse("endpoint", tc.response, []byte(tc.body), tc.requestErr)
			if err == nil || errors.Is(err, errNotFound) || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), groupTestKey) {
				t.Fatalf("unexpected response error: %v", err)
			}
			if tc.identity != nil && !errors.Is(err, tc.identity) {
				t.Fatalf("lost bounded retry error identity: %v", err)
			}
		})
	}
}

func TestRawEndpoint404NeverEstablishesAbsence(t *testing.T) {
	t.Parallel()
	id, user := uuid.MustParse(groupTestID), uuid.MustParse(userTestID)
	for _, tc := range []struct {
		operation string
		call      func(*apiClient) error
	}{
		{"auth.info", func(a *apiClient) error { _, err := a.requireIAMAdmin(t.Context(), "test"); return err }},
		{"groups.list", func(a *apiClient) error { return a.walkGroups(t.Context(), func(*client.Group) error { return nil }) }},
		{"users.list", func(a *apiClient) error { return a.walkUsers(t.Context(), func(*client.User) error { return nil }) }},
		{"collections.list", func(a *apiClient) error {
			return a.walkCollections(t.Context(), func(*client.Collection) error { return nil })
		}},
		{"groups.info", func(a *apiClient) error { _, err := a.readGroup(t.Context(), id); return err }},
		{"users.info", func(a *apiClient) error { _, err := a.readUser(t.Context(), user); return err }},
		{"collections.info", func(a *apiClient) error { _, err := a.readCollection(t.Context(), id); return err }},
		{"groups.update", func(a *apiClient) error { _, err := a.updateGroup(t.Context(), id, groupTestModel()); return err }},
		{"collections.update", func(a *apiClient) error {
			_, err := a.updateCollection(t.Context(), id, collectionTestModel())
			return err
		}},
		{"users.delete", func(a *apiClient) error { return a.deleteUser(t.Context(), userTestUser()) }},
		{"groups.memberships", func(a *apiClient) error { _, err := a.readGroupMemberPages(t.Context(), id, user); return err }},
		{"groups.add_user", func(a *apiClient) error {
			_, err := a.writeGroupMember(t.Context(), id, user, client.GroupPermissionMember, true)
			return err
		}},
		{"groups.update_user", func(a *apiClient) error {
			_, err := a.writeGroupMember(t.Context(), id, user, client.GroupPermissionMember, false)
			return err
		}},
		{"groups.remove_user", func(a *apiClient) error { return a.removeGroupMember(t.Context(), id, user) }},
		{"collections.group_memberships", func(a *apiClient) error { _, err := a.readCollectionGroupPages(t.Context(), id, id); return err }},
		{"collections.memberships", func(a *apiClient) error { _, err := a.readCollectionUserPages(t.Context(), id, user); return err }},
		{"collections.add_group", func(a *apiClient) error {
			_, err := a.writeCollectionGroup(t.Context(), id, id, client.PermissionRead)
			return err
		}},
		{"collections.add_user", func(a *apiClient) error {
			_, err := a.writeCollectionUser(t.Context(), id, user, client.PermissionRead)
			return err
		}},
		{"collections.remove_group", func(a *apiClient) error { return a.removeCollectionGroup(t.Context(), id, id) }},
		{"collections.remove_user", func(a *apiClient) error { return a.removeCollectionUser(t.Context(), id, user) }},
	} {
		t.Run(tc.operation, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if req.URL.Path == "/api/"+tc.operation {
					w.WriteHeader(http.StatusNotFound)
					groupTestWrite(t, w, `{"ok":false,"status":404,"error":"proxy_not_found","message":"denied `+groupTestKey+`"}`)
					return
				}
				// Info 404s require independent verification. Failed verification
				// must not inherit absence from either endpoint's HTTP status.
				if req.URL.Path == "/api/auth.info" {
					groupTestEncode(t, w, userTestAuth(userTestOwner()))
					return
				}
				if req.URL.Path == "/api/groups.list" || req.URL.Path == "/api/users.list" {
					w.WriteHeader(http.StatusNotFound)
					groupTestWrite(t, w, `{}`)
					return
				}
				t.Errorf("unexpected endpoint: %s", req.URL.Path)
			}))
			t.Cleanup(server.Close)
			api, err := newAPIClient(server.URL+"/api", groupTestKey, 5, "unit")
			if err != nil {
				t.Fatal(err)
			}
			api.httpClient.Transport.(*bearerTransport).limiter = rate.NewLimiter(rate.Inf, 1)
			err = tc.call(api)
			if err == nil || errors.Is(err, errNotFound) || !strings.Contains(err.Error(), tc.operation+": HTTP 404") || strings.Contains(err.Error(), groupTestKey) {
				t.Fatalf("raw endpoint 404 authorized absence or lost diagnostics: %v", err)
			}
		})
	}
}
