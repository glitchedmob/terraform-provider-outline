// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/glitchedmob/terraform-provider-outline/internal/client"
	"github.com/google/uuid"
	"github.com/hashicorp/terraform-exec/tfexec"
)

// Exercise Terraform's concurrent resource graph with the production provider.
// The fixture has a ten-create bucket and a one-second reset, not a live Redis
// limiter. The next stack layer owns proof against the released Outline server.
func TestRateLimitElevenGroupsTerraform(t *testing.T) {
	for _, tc := range []struct {
		name, config, seconds, wantError string
		wantGroups, wantAttempts         int
	}{
		{"default budget completes", "", "1", "", 11, 12},
		{"disabled budget fails", "rate_limit_wait_seconds = 0", "1", "no automatic retry", 10, 11},
		{"hourly quota fails within default budget", "", "3600", "rate_limit_wait_seconds budget exhausted", 10, 11},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			groups := make(map[string]*client.Group)
			attempts, limited := 0, false
			var resetAt time.Time
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				if req.Header.Get("Authorization") != "Bearer "+groupTestKey {
					t.Error("wrong fixture credentials")
				}
				switch req.URL.Path {
				case "/api/groups.create":
					attempts++
					var body client.GroupsCreateJSONRequestBody
					if !groupTestDecode(t, w, req, &body) {
						return
					}
					if len(groups) == 10 && (!limited || time.Now().Before(resetAt)) {
						if !limited {
							resetAt = time.Now().Add(time.Second)
							limited = true
						}
						for key, values := range rateLimitHeaders(tc.seconds) {
							w.Header()[key] = values
						}
						w.WriteHeader(http.StatusTooManyRequests)
						groupTestWrite(t, w, verifiedRateLimitBody)
						return
					}
					for _, group := range groups {
						if *group.Name == body.Name {
							t.Errorf("write replay created duplicate group %q", body.Name)
						}
					}
					group := groupTestGroup()
					group.Id = groupTestPointer(uuid.New())
					group.Name = groupTestPointer(body.Name)
					group.DisableMentions = body.DisableMentions
					groups[group.Id.String()] = group
					groupTestEncode(t, w, groupTestEnvelope(group))
				case "/api/groups.info", "/api/groups.delete":
					var body struct {
						ID string `json:"id"`
					}
					if !groupTestDecode(t, w, req, &body) {
						return
					}
					group := groups[body.ID]
					if group == nil {
						t.Errorf("unexpected missing group %s", body.ID)
						w.WriteHeader(404)
						return
					}
					if req.URL.Path == "/api/groups.delete" {
						delete(groups, body.ID)
						groupTestWrite(t, w, `{"ok":true,"status":200,"success":true}`)
						return
					}
					groupTestEncode(t, w, groupTestEnvelope(group))
				default:
					t.Errorf("unexpected endpoint %s", req.URL.Path)
					w.WriteHeader(404)
				}
			}))
			defer server.Close()
			const address = "registry.terraform.io/glitchedmob/outline"
			reattach := groupPartialCreateProvider(t, address)
			tf := groupPartialCreateTerraform(t, fmt.Sprintf(`
terraform {
  required_providers {
    outline = { source = %q }
  }
}
provider "outline" {
  base_url = %q
  api_key = %q
  %s
}
resource "outline_group" "batch" {
  count = 11
  name = "Quota batch ${count.index}"
}
`, address, server.URL+"/api", groupTestKey, tc.config))
			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			defer cancel()
			if err := tf.Init(ctx, tfexec.Reattach(reattach)); err != nil {
				t.Fatal(err)
			}
			err := tf.Apply(ctx, tfexec.Reattach(reattach))
			if tc.wantError == "" && err != nil || tc.wantError != "" && (err == nil || !strings.Contains(strings.Join(strings.Fields(err.Error()), " "), tc.wantError)) {
				t.Fatalf("apply: %v", err)
			}
			state := groupPartialCreateState(t, ctx, tf)
			if len(state.Resources) != 1 || len(state.Resources[0].Instances) != tc.wantGroups {
				t.Fatalf("lost batch state: %+v", state)
			}
			mu.Lock()
			if attempts != tc.wantAttempts || len(groups) != tc.wantGroups || !limited {
				t.Errorf("quota not exercised: attempts=%d groups=%d limited=%t", attempts, len(groups), limited)
			}
			for _, instance := range state.Resources[0].Instances {
				group := groups[instance.Attributes.ID]
				if group == nil || instance.Status != "" || instance.Deposed != "" || *group.Name != instance.Attributes.Name {
					t.Errorf("bad group identity/taint: %+v", instance)
				}
			}
			mu.Unlock()
			if tc.wantError == "" {
				if changed, err := tf.Plan(ctx, tfexec.Reattach(reattach)); err != nil || changed {
					t.Fatalf("batch not stable: changed=%t err=%v", changed, err)
				}
			}
			if err := tf.Destroy(ctx, tfexec.Reattach(reattach)); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(groups) != 0 || attempts != tc.wantAttempts {
				t.Errorf("destroy retained groups or replayed create: groups=%d attempts=%d", len(groups), attempts)
			}
		})
	}
}
