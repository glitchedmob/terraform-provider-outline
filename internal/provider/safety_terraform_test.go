// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/glitchedmob/terraform-provider-outline/internal/client"
	"github.com/hashicorp/terraform-exec/tfexec"
)

// Inject failures after real or fixture API writes. The proxy never redirects
// credentials outside the disposable test API and never retries a request.
type iamSafetyProxy struct {
	server      *httptest.Server
	delete404   atomic.Bool
	suspend403  atomic.Bool
	read403     atomic.Bool
	invites     atomic.Int32
	unknownUser bool
}

func newIAMSafetyProxy(t *testing.T, api *apiClient, malformedInvite, unknownUser bool) *iamSafetyProxy {
	t.Helper()
	target, err := url.Parse(strings.TrimSuffix(api.baseURL, "/api"))
	if err != nil {
		t.Fatal(err)
	}
	p := &iamSafetyProxy{unknownUser: unknownUser}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ModifyResponse = func(resp *http.Response) error {
		if !malformedInvite || resp.Request.URL.Path != "/api/users.invite" || resp.StatusCode != http.StatusOK {
			return nil
		}
		var body map[string]any
		decodeErr := json.NewDecoder(resp.Body).Decode(&body)
		closeErr := resp.Body.Close()
		if decodeErr != nil {
			return decodeErr
		}
		if closeErr != nil {
			return closeErr
		}
		data := body["data"].(map[string]any)
		delete(data, "sent")
		if p.unknownUser {
			delete(data["users"].([]any)[0].(map[string]any), "isSuspended")
		}
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		resp.Body = io.NopCloser(bytes.NewReader(encoded))
		resp.ContentLength = int64(len(encoded))
		resp.Header.Set("Content-Length", fmt.Sprint(len(encoded)))
		return nil
	}
	p.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/api/groups.delete" && p.delete404.Load() {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			groupTestWrite(t, w, `{}`)
			return
		}
		if req.URL.Path == "/api/users.suspend" && p.suspend403.Load() || req.URL.Path == "/api/users.info" && p.read403.Load() {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			groupTestWrite(t, w, `{"error":"permission_denied"}`)
			return
		}
		if req.URL.Path == "/api/users.invite" {
			p.invites.Add(1)
		}
		proxy.ServeHTTP(w, req)
	}))
	t.Cleanup(p.server.Close)
	return p
}

func safetyTerraform(t *testing.T, proxy *iamSafetyProxy, api *apiClient, config string) (*tfexec.Terraform, tfexec.ReattachInfo, context.Context) {
	t.Helper()
	const providerAddress = "registry.terraform.io/glitchedmob/outline"
	reattach := groupPartialCreateProvider(t, providerAddress)
	tf := groupPartialCreateTerraform(t, fmt.Sprintf(`
terraform {
  required_providers {
    outline = { source = %q }
  }
}
provider "outline" {
  base_url = %q
  api_key = %q
  timeout_seconds = 5
}
%s
`, providerAddress, proxy.server.URL+"/api", api.apiKey, config))
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	t.Cleanup(cancel)
	if err := tf.Init(ctx, tfexec.Reattach(reattach)); err != nil {
		t.Fatalf("terraform init: %v", err)
	}
	t.Cleanup(func() {
		proxy.delete404.Store(false)
		proxy.suspend403.Store(false)
		proxy.read403.Store(false)
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		if err := tf.Destroy(cleanupCtx, tfexec.Reattach(reattach)); err != nil {
			t.Errorf("terraform cleanup destroy: %v", err)
		}
	})
	return tf, reattach, ctx
}

func groupDeleteRoute404Terraform(t *testing.T, api *apiClient, checkRemote func(string)) {
	t.Helper()
	proxy := newIAMSafetyProxy(t, api, false, false)
	proxy.delete404.Store(true)
	tf, reattach, ctx := safetyTerraform(t, proxy, api, `resource "outline_group" "test" { name = "Terraform delete 404 regression" }`)
	if err := tf.Apply(ctx, tfexec.Reattach(reattach)); err != nil {
		t.Fatalf("terraform group create: %v", err)
	}
	before := groupPartialCreateState(t, ctx, tf)
	if len(before.Resources) != 1 || len(before.Resources[0].Instances) != 1 {
		t.Fatalf("expected one managed group: %+v", before)
	}
	id := before.Resources[0].Instances[0].Attributes.ID
	if err := tf.Destroy(ctx, tfexec.Reattach(reattach)); err == nil || !strings.Contains(err.Error(), "groups.delete: HTTP 404") {
		t.Fatalf("wrong-route delete should fail: %v", err)
	}
	if after := groupPartialCreateState(t, ctx, tf); !reflect.DeepEqual(after, before) {
		t.Fatalf("failed group delete forgot or changed state: before=%+v after=%+v", before, after)
	}
	checkRemote(id)
	proxy.delete404.Store(false)
	if err := tf.Destroy(ctx, tfexec.Reattach(reattach)); err != nil {
		t.Fatalf("terraform group destroy after fixing route: %v", err)
	}
	if after := groupPartialCreateState(t, ctx, tf); len(after.Resources) != 0 {
		t.Fatalf("successful group destroy retained state: %+v", after)
	}
}

func userMalformedInviteTerraform(t *testing.T, api *apiClient, email, failure string, checkRemote func(string, bool)) {
	t.Helper()
	proxy := newIAMSafetyProxy(t, api, true, failure == "unknown")
	proxy.suspend403.Store(failure == "suspend")
	proxy.read403.Store(failure == "unknown")
	tf, reattach, ctx := safetyTerraform(t, proxy, api, fmt.Sprintf(`resource "outline_user" "test" {
  email = %q
  suspended = true
}`, email))
	if err := tf.Apply(ctx, tfexec.Reattach(reattach)); err == nil || !strings.Contains(err.Error(), "Unable to validate invited user") || !strings.Contains(err.Error(), "terraform untaint") {
		t.Fatalf("malformed invite should fail with UUID recovery instructions: %v", err)
	} else if failure == "suspend" && !strings.Contains(strings.Join(strings.Fields(err.Error()), " "), "restoring suspension also failed") {
		t.Fatalf("failed cleanup diagnostic was lost: %v", err)
	}
	state := userPartialCreateState(t, ctx, tf)
	if len(state.Resources) != 1 || len(state.Resources[0].Instances) != 1 {
		t.Fatalf("failed invitation lost state: %+v", state)
	}
	instance := state.Resources[0].Instances[0]
	id, ok := instance.Attributes["id"].(string)
	var wantSuspended any = failure == ""
	if failure == "unknown" {
		wantSuspended = nil
	}
	if !ok || id == "" || instance.Status != "tainted" || instance.Attributes["suspended"] != wantSuspended || proxy.invites.Load() != 1 {
		t.Fatalf("failed create did not save a tainted UUID with observed suspension: %+v invites=%d", instance, proxy.invites.Load())
	}
	checkRemote(id, failure == "")
	proxy.suspend403.Store(false)
	proxy.read403.Store(false)
	if err := tf.Untaint(ctx, "outline_user.test"); err != nil {
		t.Fatalf("terraform untaint: %v", err)
	}
	if err := tf.Apply(ctx, tfexec.Reattach(reattach)); err != nil {
		t.Fatalf("terraform in-place recovery: %v", err)
	}
	state = userPartialCreateState(t, ctx, tf)
	instance = state.Resources[0].Instances[0]
	if instance.Status != "" || instance.Attributes["id"] != id || instance.Attributes["suspended"] != true || proxy.invites.Load() != 1 {
		t.Fatalf("recovery replaced or reinvited the user: %+v invites=%d", instance, proxy.invites.Load())
	}
	checkRemote(id, true)
	if changed, err := tf.Plan(ctx, tfexec.Reattach(reattach)); changed || err != nil {
		t.Fatalf("recovery should leave an empty plan: changed=%t err=%v", changed, err)
	}
	if err := tf.Destroy(ctx, tfexec.Reattach(reattach)); err != nil {
		t.Fatalf("terraform default user destroy: %v", err)
	}
	if state := userPartialCreateState(t, ctx, tf); len(state.Resources) != 0 || proxy.invites.Load() != 1 {
		t.Fatalf("destroy left state or reinvited the user: %+v invites=%d", state, proxy.invites.Load())
	}
	checkRemote(id, true)
}

func TestGroupDeleteRoute404TerraformRetainsState(t *testing.T) {
	var mu sync.Mutex
	created := false
	api := groupTestClient(t, func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch req.URL.Path {
		case "/api/groups.create":
			created = true
		case "/api/groups.info":
			if !created {
				t.Error("read after the group was deleted")
			}
		case "/api/groups.delete":
			created = false
			groupTestWrite(t, w, `{"ok":true,"success":true}`)
			return
		default:
			t.Errorf("unexpected request: %s", req.URL.Path)
		}
		group := groupTestGroup()
		group.Name = groupTestPointer("Terraform delete 404 regression")
		groupTestEncode(t, w, groupTestEnvelope(group))
	})
	groupDeleteRoute404Terraform(t, api, func(id string) {
		mu.Lock()
		defer mu.Unlock()
		if id != groupTestID || !created {
			t.Fatal("proxy 404 destroyed the group or lost its identity")
		}
	})
}

func TestUserMalformedInviteTerraformRetainsObservedSuspension(t *testing.T) {
	for _, failure := range []string{"", "suspend", "unknown"} {
		t.Run("cleanup_"+failure, func(t *testing.T) {
			var mu sync.Mutex
			current := userTestUser()
			current.Name = groupTestPointer("Pending user")
			created := false
			api := userTestAdminClient(t, func(w http.ResponseWriter, req *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				switch req.URL.Path {
				case "/api/users.list":
					userTestListOffset(t, w, req)
					users := []client.User{}
					if created {
						users = append(users, *current)
					}
					groupTestEncode(t, w, userTestList(users, 0, 100, len(users)))
				case "/api/users.invite":
					if created {
						t.Error("reinvited a known account")
					}
					created = true
					groupTestEncode(t, w, userTestInviteEnvelope(current))
				case "/api/users.info", "/api/users.suspend":
					if req.URL.Path == "/api/users.suspend" {
						current.IsSuspended = groupTestPointer(true)
					}
					groupTestEncode(t, w, userTestEnvelope(current))
				default:
					t.Errorf("unexpected request: %s", req.URL.Path)
				}
			})
			userMalformedInviteTerraform(t, api, userTestEmail, failure, func(id string, suspended bool) {
				mu.Lock()
				defer mu.Unlock()
				if id != userTestID || !created || *current.IsSuspended != suspended {
					t.Fatalf("state did not match the stored user: id=%s created=%t suspended=%t want=%t", id, created, *current.IsSuspended, suspended)
				}
			})
		})
	}
}
