// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/glitchedmob/terraform-provider-outline/internal/client"
	"github.com/google/uuid"
	"github.com/hashicorp/terraform-exec/tfexec"
)

// Drive Terraform Core, not just CreateResponse, to check that a committed
// upsert's trusted pair survives a bad response and can recover after untaint.
func TestCollectionUserPartialCreateTerraformRecovery(t *testing.T) {
	const address = "outline_collection_user.test"
	var mu sync.Mutex
	var reading atomic.Bool
	other := cuUnitGrant(cuUnitOtherUserID, client.PermissionReadWrite)
	other.Id = groupTestPointer(uuid.NewString())
	members := []client.Membership{other}
	adds, removes := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		cuUnitAssertRequest(t, req)
		w.Header().Set("Content-Type", "application/json")
		if cuUnitParents(t, w, req) {
			return
		}
		switch req.URL.Path {
		case "/api/collections.memberships":
			cuUnitOffset(t, w, req, grantTestReadQueryFlag(reading.Load(), cuUnitParentUser(cuUnitUserID).Name))
			groupTestEncode(t, w, cuUnitEnvelope(members, 0, len(members), false))
		case "/api/collections.add_user":
			cuUnitBody(t, w, req, map[string]any{"id": cuUnitCollectionID, "userId": cuUnitUserID, "permission": "admin"})
			adds++
			members = []client.Membership{other, cuUnitGrant(cuUnitUserID, client.PermissionAdmin)}
			wrong := cuUnitGrant(cuUnitOtherUserID, client.PermissionAdmin)
			groupTestEncode(t, w, cuUnitEnvelope([]client.Membership{wrong}, 0, 0, true))
		case "/api/collections.remove_user":
			cuUnitBody(t, w, req, map[string]any{"id": cuUnitCollectionID, "userId": cuUnitUserID})
			removes++
			members = []client.Membership{other}
			groupTestEncode(t, w, map[string]any{"ok": true, "status": 200, "success": true})
		default:
			t.Errorf("unexpected endpoint: %s", req.URL.Path)
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
resource "outline_collection_user" "test" {
  collection_id = %q
  user_id = %q
  permission = "admin"
}
`, providerAddress, server.URL+"/api", groupTestKey, cuUnitCollectionID, cuUnitUserID))
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
	if err := tf.Apply(ctx, tfexec.Reattach(reattach)); err == nil {
		t.Fatal("apply succeeded despite the untrusted mutation response")
	} else {
		message := strings.Join(strings.Fields(err.Error()), " ")
		if !strings.Contains(message, "collection grant user returned a different ID") || !strings.Contains(message, "pair ID has been retained") {
			t.Fatalf("apply must fail after retaining the trusted pair: %v", err)
		}
	}
	checkState := func(status string) {
		t.Helper()
		raw, err := tf.StatePull(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var state struct {
			Resources []struct {
				Mode      string `json:"mode"`
				Type      string `json:"type"`
				Name      string `json:"name"`
				Instances []struct {
					Status     string            `json:"status"`
					Deposed    string            `json:"deposed"`
					Attributes map[string]string `json:"attributes"`
				} `json:"instances"`
			} `json:"resources"`
		}
		if err := json.Unmarshal([]byte(raw), &state); err != nil {
			t.Fatal(err)
		}
		if len(state.Resources) != 1 || state.Resources[0].Mode != "managed" || state.Resources[0].Type != "outline_collection_user" || state.Resources[0].Name != "test" || len(state.Resources[0].Instances) != 1 {
			t.Fatalf("trusted pair lost: %+v", state)
		}
		instance := state.Resources[0].Instances[0]
		want := map[string]string{"id": cuUnitPairID, "collection_id": cuUnitCollectionID, "user_id": cuUnitUserID, "permission": "admin"}
		if instance.Status != status || instance.Deposed != "" || !reflect.DeepEqual(instance.Attributes, want) {
			t.Fatalf("persisted an untrusted pair or wrong taint: %+v", instance)
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
	before, ok := plan.ResourceChanges[0].Change.Before.(map[string]any)
	if !ok || before["id"] != cuUnitPairID {
		t.Fatalf("replacement plan lost the retained pair: %v", before)
	}
	if err := tf.Untaint(ctx, address); err != nil {
		t.Fatal(err)
	}
	checkState("")
	if changed, err := tf.Plan(ctx, tfexec.Reattach(reattach)); err != nil || changed {
		t.Fatalf("untainted pair must recover without another upsert: changed=%t err=%v", changed, err)
	}
	if err := tf.Apply(ctx, tfexec.Reattach(reattach)); err != nil {
		t.Fatal(err)
	}
	checkState("")
	mu.Lock()
	if adds != 1 || removes != 0 || len(members) != 2 || !reflect.DeepEqual(members[0], other) || members[1].UserId.String() != cuUnitUserID || *members[1].Permission != client.PermissionAdmin {
		t.Errorf("recovery replayed an upsert or changed another pair: adds=%d removes=%d members=%v", adds, removes, members)
	}
	mu.Unlock()
	// Losing local state does not authorize a second upsert. Import the
	// committed pair and discover its actual permission without writing.
	if err := tf.StateRm(ctx, address); err != nil {
		t.Fatal(err)
	}
	if err := tf.Import(ctx, address, cuUnitPairID, tfexec.Reattach(reattach)); err != nil {
		t.Fatal(err)
	}
	checkState("")
	if changed, err := tf.Plan(ctx, tfexec.Reattach(reattach)); err != nil || changed {
		t.Fatalf("imported recovery pair must be stable: changed=%t err=%v", changed, err)
	}
	if err := tf.Apply(ctx, tfexec.Reattach(reattach)); err != nil {
		t.Fatal(err)
	}
	if err := tf.Destroy(ctx, tfexec.Reattach(reattach)); err != nil {
		t.Fatal(err)
	}
	cleaned = true
	mu.Lock()
	defer mu.Unlock()
	if adds != 1 || removes != 1 || !reflect.DeepEqual(members, []client.Membership{other}) {
		t.Fatal("destroy did not remove exactly the retained pair")
	}
	raw, err := tf.StatePull(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var state struct {
		Resources []any `json:"resources"`
	}
	if err := json.Unmarshal([]byte(raw), &state); err != nil || len(state.Resources) != 0 {
		t.Fatalf("destroy left a resource in state: %s %v", raw, err)
	}
}
