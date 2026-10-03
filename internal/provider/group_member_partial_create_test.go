// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/glitchedmob/terraform-provider-outline/internal/client"
	"github.com/hashicorp/terraform-exec/tfexec"
)

// Terraform Core must persist and taint the trusted pair after a committed
// add returns malformed data. Untaint recovers it without another upsert.
func TestGroupMemberPartialCreateTerraformRecovery(t *testing.T) {
	const address = "outline_group_member.test"
	const id = groupTestID + "/" + userTestID
	var mu sync.Mutex
	var reading atomic.Bool
	members := []client.GroupUser{}
	adds, removes := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if req.Method != http.MethodPost || req.Header.Get("Authorization") != "Bearer "+groupTestKey || req.URL.RawQuery != "" {
			t.Errorf("unexpected request: %s %s", req.Method, req.URL)
		}
		if memberTestParents(t, w, req) {
			return
		}
		switch req.URL.Path {
		case "/api/groups.memberships":
			grantTestOffset(t, w, req, groupTestID, grantTestReadQueryFlag(reading.Load(), userTestUser().Name))
			groupTestEncode(t, w, memberTestEnvelope(members, 0, len(members), false))
		case "/api/groups.add_user":
			memberTestBody(t, w, req, map[string]any{"id": groupTestID, "userId": userTestID, "permission": "admin"})
			adds++
			members = []client.GroupUser{memberTestMember(userTestID, client.GroupPermissionAdmin)}
			// The write committed, but its returned identity cannot be trusted.
			wrong := memberTestMember(userTestOtherID, client.GroupPermissionAdmin)
			groupTestEncode(t, w, memberTestEnvelope([]client.GroupUser{wrong}, 0, 0, true))
		case "/api/groups.remove_user":
			memberTestBody(t, w, req, map[string]any{"id": groupTestID, "userId": userTestID})
			removes++
			members = []client.GroupUser{}
			groupTestEncode(t, w, map[string]any{"ok": true, "status": 200, "data": map[string]any{"groups": []client.Group{*groupTestGroup()}}})
		default:
			t.Errorf("unexpected operation: %s", req.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	const providerAddress = "registry.terraform.io/glitchedmob/outline"
	reattach := groupPartialCreateProvider(t, providerAddress, grantTestTrackReads(&reading))
	tf := groupPartialCreateTerraform(t, fmt.Sprintf(`
terraform {
  required_providers {
    outline = { source = %q }
  }
}
provider "outline" {
  base_url = %q
  api_key = %q
}
resource "outline_group_member" "test" {
  group_id = %q
  user_id = %q
  permission = "admin"
}
`, providerAddress, server.URL+"/api", groupTestKey, groupTestID, userTestID))
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	if err := tf.Init(ctx, tfexec.Reattach(reattach)); err != nil {
		t.Fatal(err)
	}
	cleaned := false
	t.Cleanup(func() {
		if cleaned {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := tf.Destroy(ctx, tfexec.Reattach(reattach)); err != nil {
			t.Errorf("cleanup destroy: %v", err)
		}
	})
	if err := tf.Apply(ctx, tfexec.Reattach(reattach)); err == nil || !strings.Contains(err.Error(), "different ID") || !strings.Contains(err.Error(), "pair ID has been retained") {
		t.Fatalf("apply must fail after retaining the trusted pair: %v", err)
	}
	checkState := func(status string) {
		t.Helper()
		raw, err := tf.StatePull(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var state struct {
			Resources []struct {
				Type      string `json:"type"`
				Instances []struct {
					Status     string            `json:"status"`
					Attributes map[string]string `json:"attributes"`
				} `json:"instances"`
			} `json:"resources"`
		}
		if err := json.Unmarshal([]byte(raw), &state); err != nil {
			t.Fatal(err)
		}
		if len(state.Resources) != 1 || state.Resources[0].Type != "outline_group_member" || len(state.Resources[0].Instances) != 1 {
			t.Fatalf("trusted pair lost: %+v", state)
		}
		i := state.Resources[0].Instances[0]
		if i.Status != status || i.Attributes["id"] != id || i.Attributes["group_id"] != groupTestID || i.Attributes["user_id"] != userTestID || i.Attributes["permission"] != "admin" {
			t.Fatalf("persisted an untrusted pair or wrong taint: %+v", i)
		}
	}
	checkState("tainted")
	if changed, err := tf.Plan(ctx, tfexec.Reattach(reattach), tfexec.Out("tainted.tfplan")); err != nil || !changed {
		t.Fatalf("tainted pair must require replacement: changed=%t err=%v", changed, err)
	}
	plan, err := tf.ShowPlanFile(ctx, "tainted.tfplan", tfexec.Reattach(reattach))
	if err != nil || len(plan.ResourceChanges) != 1 || plan.ResourceChanges[0].Address != address || !plan.ResourceChanges[0].Change.Actions.DestroyBeforeCreate() {
		t.Fatalf("wrong failed-create recovery plan: %v %v", plan, err)
	}
	if err := tf.Untaint(ctx, address); err != nil {
		t.Fatal(err)
	}
	checkState("")
	if changed, err := tf.Plan(ctx, tfexec.Reattach(reattach)); err != nil || changed {
		t.Fatalf("untainted pair must reconcile without another add: changed=%t err=%v", changed, err)
	}
	if err := tf.Apply(ctx, tfexec.Reattach(reattach)); err != nil {
		t.Fatal(err)
	}
	checkState("")
	mu.Lock()
	if adds != 1 || removes != 0 || len(members) != 1 || members[0].UserId.String() != userTestID {
		t.Errorf("recovery replayed add or changed another pair: adds=%d removes=%d", adds, removes)
	}
	mu.Unlock()
	if err := tf.Destroy(ctx, tfexec.Reattach(reattach)); err != nil {
		t.Fatal(err)
	}
	cleaned = true
	mu.Lock()
	defer mu.Unlock()
	if adds != 1 || removes != 1 || len(members) != 0 {
		t.Fatal("destroy did not remove exactly the trusted pair")
	}
}
