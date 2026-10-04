// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/glitchedmob/terraform-provider-outline/internal/client"
	"github.com/google/uuid"
	"github.com/hashicorp/terraform-exec/tfexec"
)

type acceptanceQuota struct {
	Operation string `json:"operation"`
	Points    int    `json:"points"`
	Seconds   int    `json:"seconds"`
}

var acceptanceQuotas = []acceptanceQuota{
	{"groups.create", 10, 60},
	{"users.invite", 50, 3600},
	{"collections.add_user", 100, 3600},
	{"users.delete", 10, 3600},
}

type acceptanceRateLimitFixture struct {
	Version     string            `json:"version"`
	Enabled     bool              `json:"enabled"`
	Multiplier  int               `json:"multiplier"`
	Quotas      []acceptanceQuota `json:"quotas"`
	Groups      int               `json:"groups"`
	Users       int               `json:"users"`
	Collections int               `json:"collections"`
}

func acceptanceRateLimitSetup(t *testing.T, api *acceptanceAPI, action, operation string) acceptanceRateLimitFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	const path = "/opt/outline/acceptance-rate-limit.cjs"
	code, data, failure := runAcceptanceFixture(ctx, api.container, "../../integration/rate-limit-fixture.cjs", path, action, operation)
	if failure != nil && failure.step == "copy" {
		t.Fatal(failure)
	}
	if failure != nil && failure.step == "execute" {
		t.Fatal("execute guarded rate-limit fixture failed")
	}
	if failure != nil || code != 0 {
		// Do not print arbitrary ORM/Redis output, which may contain credentials.
		t.Fatalf("guarded rate-limit fixture failed: exit=%d", code)
	}
	var result acceptanceRateLimitFixture
	const marker = "OUTLINE_ACCEPTANCE_RATE_LIMIT="
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, marker) {
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, marker)), &result); err != nil {
				t.Fatal("decode guarded rate-limit fixture failed")
			}
		}
	}
	if result.Version != "1.10.1" || !result.Enabled || result.Multiplier != 1 || !reflect.DeepEqual(result.Quotas, acceptanceQuotas) {
		t.Fatal("live stack must use Outline 1.10.1 with its enabled limiter and multiplier-1 quota table")
	}
	return result
}

type acceptanceQuotaResponse struct {
	status int
	header http.Header
	body   []byte
}

// The proxy forwards every request to the released server unchanged and never
// manufactures a 429. Only allowlisted response headers and bounded 429 bodies
// are observed. No request headers, keys, or credential hashes are recorded.
func acceptanceQuotaObserver(t *testing.T, api *acceptanceAPI) (string, func(string) []acceptanceQuotaResponse) {
	t.Helper()
	target, err := url.Parse(api.baseURL)
	if err != nil {
		t.Fatal(err)
	}
	target.Path = ""
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ErrorLog = log.New(io.Discard, "", 0)
	var mu sync.Mutex
	responses := make(map[string][]acceptanceQuotaResponse)
	proxy.ModifyResponse = func(response *http.Response) error {
		operation := strings.TrimPrefix(response.Request.URL.Path, "/api/")
		record := acceptanceQuotaResponse{status: response.StatusCode}
		if response.StatusCode == http.StatusTooManyRequests {
			record.header = make(http.Header)
			for _, name := range []string{"Content-Type", "Retry-After", "RateLimit-Limit", "RateLimit-Remaining", "RateLimit-Reset"} {
				record.header[http.CanonicalHeaderKey(name)] = append([]string(nil), response.Header.Values(name)...)
			}
			body, err := io.ReadAll(io.LimitReader(response.Body, 1025))
			if err != nil {
				return errors.New("observe live rate-limit body failed")
			}
			// Preserve the full body for the provider, including any unread bytes.
			response.Body = &restoredResponseBody{Reader: io.MultiReader(bytes.NewReader(body), response.Body), closer: response.Body}
			record.body = body
		}
		mu.Lock()
		responses[operation] = append(responses[operation], record)
		mu.Unlock()
		return nil
	}
	server := httptest.NewServer(proxy)
	t.Cleanup(server.Close)
	return server.URL + "/api", func(operation string) []acceptanceQuotaResponse {
		mu.Lock()
		defer mu.Unlock()
		return append([]acceptanceQuotaResponse(nil), responses[operation]...)
	}
}

func acceptanceCheckQuotaResponse(t *testing.T, response acceptanceQuotaResponse, quota acceptanceQuota) int {
	t.Helper()
	if !preMutationRateLimit(response.body) || len(response.body) > 1024 || response.status != http.StatusTooManyRequests {
		t.Fatal("live middleware did not return the audited four-field pre-mutation 429 envelope; body withheld")
	}
	contentType := response.header.Get("Content-Type")
	delay, parseErr := strconv.Atoi(response.header.Get("Retry-After"))
	if contentType != "application/json" && contentType != "application/json; charset=utf-8" ||
		parseErr != nil || delay < 1 || delay > quota.Seconds ||
		response.header.Get("RateLimit-Limit") != strconv.Itoa(quota.Points) ||
		response.header.Get("RateLimit-Remaining") != "0" ||
		response.header.Get("RateLimit-Reset") != strconv.Itoa(delay) {
		t.Fatal("live middleware quota headers do not match the multiplier-1 contract; headers withheld")
	}
	// Log only after validating every field against the public error contract.
	t.Logf("live %s HTTP 429: Content-Type=%q Retry-After=%d RateLimit-Limit=%d RateLimit-Remaining=0 RateLimit-Reset=%d body=%s",
		quota.Operation, response.header.Get("Content-Type"), delay, quota.Points, delay, response.body)
	return delay
}

func TestAccRateLimitRealMiddleware(t *testing.T) {
	api := newAcceptanceAPIWithOverrides(t, "../../integration/compose.rate-limit.yml")
	initial := acceptanceRateLimitSetup(t, api, "inspect", "")
	if initial.Groups != 0 || initial.Users != 1 || initial.Collections != 0 {
		t.Fatal("rate-limit acceptance requires a bootstrap-only workspace")
	}
	baseURL, observed := acceptanceQuotaObserver(t, api)

	t.Run("eleven groups retry the real minute quota", func(t *testing.T) {
		const address = "registry.terraform.io/glitchedmob/outline"
		reattach := groupPartialCreateProvider(t, address)
		tf := groupPartialCreateTerraform(t, fmt.Sprintf(`
terraform {
  required_providers { outline = { source = %q } }
}
provider "outline" {
  base_url = %q
  api_key = %q
}
resource "outline_group" "batch" {
  count = 11
  name = "Real quota batch ${count.index}"
}
`, address, baseURL, api.apiKey))
		ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
		defer cancel()
		if err := tf.Init(ctx, tfexec.Reattach(reattach)); err != nil {
			t.Fatal(err)
		}
		cleaned := false
		t.Cleanup(func() {
			if !cleaned {
				cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Minute)
				defer cleanupCancel()
				if err := tf.Destroy(cleanupCtx, tfexec.Reattach(reattach)); err != nil {
					t.Errorf("destroy partial live quota batch: %v", err)
				}
			}
		})
		if err := tf.Apply(ctx, tfexec.Reattach(reattach)); err != nil {
			t.Fatal(err)
		}
		calls := observed("groups.create")
		if len(calls) != 12 {
			t.Fatalf("eleven groups must make 11 successful creates and one real 429, got %d attempts", len(calls))
		}
		limited := 0
		for _, call := range calls {
			if call.status == http.StatusTooManyRequests {
				limited++
				if delay := acceptanceCheckQuotaResponse(t, call, acceptanceQuotas[0]); delay <= 30 {
					t.Fatal("minute quota must exercise a wait beyond the default 30-second attempt timeout")
				}
			} else if call.status != http.StatusOK {
				t.Fatalf("unexpected live create status %d", call.status)
			}
		}
		if limited != 1 {
			t.Fatalf("expected one real minute-quota rejection, got %d", limited)
		}
		baseline := groupPartialCreateState(t, ctx, tf)
		if len(baseline.Resources) != 1 || len(baseline.Resources[0].Instances) != 11 {
			t.Fatal("successful batch must retain all eleven groups in Terraform state")
		}
		ids := make(map[string]string)
		for _, instance := range baseline.Resources[0].Instances {
			id, err := uuid.Parse(instance.Attributes.ID)
			if err != nil || id == uuid.Nil || id.String() != instance.Attributes.ID || ids[id.String()] != "" || instance.Status != "" || instance.Deposed != "" {
				t.Fatal("live batch must have eleven distinct canonical UUIDs without tainted or deposed instances")
			}
			ids[id.String()] = instance.Attributes.Name
		}
		remote := 0
		if err := api.walkGroups(ctx, func(group *client.Group) error {
			remote++
			if ids[group.Id.String()] != *group.Name {
				return errors.New("remote group UUID/name differs from Terraform batch")
			}
			return nil
		}); err != nil || remote != 11 {
			t.Fatalf("live batch has extra, missing, or mismatched groups: count=%d err=%v", remote, err)
		}
		if changed, err := tf.Plan(ctx, tfexec.Reattach(reattach)); err != nil || changed {
			t.Fatalf("next plan must be empty: changed=%t err=%v", changed, err)
		}
		if !reflect.DeepEqual(groupPartialCreateState(t, ctx, tf), baseline) || len(observed("groups.create")) != 12 {
			t.Fatal("next plan changed UUIDs, tainted state, or replayed a create")
		}
		if err := tf.Destroy(ctx, tfexec.Reattach(reattach)); err != nil {
			t.Fatal(err)
		}
		cleaned = true
		if len(groupPartialCreateState(t, ctx, tf).Resources) != 0 {
			t.Fatal("destroy retained batch state")
		}
		for id := range ids {
			if err := api.acceptanceGroupAbsent(id); err != nil {
				t.Fatal(err)
			}
		}
		if len(observed("groups.create")) != 12 || acceptanceRateLimitSetup(t, api, "inspect", "").Groups != 0 {
			t.Fatal("destroy left remote groups or replayed a create")
		}
		t.Log("eleven distinct untainted UUIDs, empty next plan with stable state, and empty remote/state after destroy")
	})

	t.Run("zero budget returns the real rejection without retry", func(t *testing.T) {
		acceptanceRateLimitSetup(t, api, "exhaust", "groups.create")
		noWait, err := newAPIClientWithRateLimitWait(baseURL, api.apiKey, 30, 0, "acceptance")
		if err != nil {
			t.Fatal(err)
		}
		before := len(observed("groups.create"))
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		start := time.Now()
		response, err := noWait.GroupsCreateWithResponse(ctx, client.GroupsCreateJSONRequestBody{Name: "Must not be created"})
		if err != nil || response.StatusCode() != http.StatusTooManyRequests || time.Since(start) > 10*time.Second {
			t.Fatal("zero wait budget must return the live 429 immediately")
		}
		calls := observed("groups.create")[before:]
		if len(calls) != 1 {
			t.Fatal("zero budget replayed a rejected create")
		}
		acceptanceCheckQuotaResponse(t, calls[0], acceptanceQuotas[0])
		if acceptanceRateLimitSetup(t, api, "clear", "groups.create").Groups != 0 {
			t.Fatal("rejected zero-budget request mutated groups")
		}
	})

	for _, quota := range acceptanceQuotas[1:] {
		t.Run(quota.Operation+" rejects an hourly wait beyond the default budget", func(t *testing.T) {
			acceptanceRateLimitSetup(t, api, "exhaust", quota.Operation)
			// Quota setup is guarded, but rejection and headers come from real
			// middleware. No 50/100-create loop or hour-long sleep is needed.
			for _, budget := range []int64{0, 120} {
				bounded, err := newAPIClientWithRateLimitWait(baseURL, api.apiKey, 30, budget, "acceptance")
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
				start := time.Now()
				before := len(observed(quota.Operation))
				var response *http.Response
				switch quota.Operation {
				case "users.invite":
					response, err = bounded.UsersInvite(ctx, client.UsersInviteJSONRequestBody{
						Invites:       []client.Invite{{Name: "Must not be invited", Email: "quota-rejected@example.invalid", Role: client.UserRoleMember}},
						SuppressEmail: groupTestPointer(true),
					})
				case "collections.add_user":
					response, err = bounded.CollectionsAddUser(ctx, client.CollectionsAddUserJSONRequestBody{
						Id: uuid.New(), UserId: uuid.New(), Permission: groupTestPointer(client.PermissionRead),
					})
				case "users.delete":
					response, err = bounded.UsersDelete(ctx, client.UsersDeleteJSONRequestBody{Id: uuid.New()})
				}
				cancel()
				if budget == 0 {
					if err != nil || response == nil || response.StatusCode != http.StatusTooManyRequests {
						t.Fatal("zero budget must expose the real hourly 429")
					}
					_ = response.Body.Close()
				} else if !errors.Is(err, errRateLimitWaitBudget) || response != nil {
					t.Fatalf("hourly wait must fail at the default budget, got %v", err)
				}
				calls := observed(quota.Operation)[before:]
				if len(calls) != 1 || time.Since(start) > 10*time.Second {
					t.Fatal("hourly over-budget rejection must not wait or replay")
				}
				if delay := acceptanceCheckQuotaResponse(t, calls[0], quota); delay <= 120 {
					t.Fatal("real hourly bucket must request more than the default wait budget")
				}
			}
			result := acceptanceRateLimitSetup(t, api, "clear", quota.Operation)
			if result.Groups != 0 || result.Users != 1 || result.Collections != 0 {
				t.Fatal("rejected hourly requests mutated the bootstrap workspace")
			}
		})
	}
}
