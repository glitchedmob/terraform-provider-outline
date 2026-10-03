// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/glitchedmob/terraform-provider-outline/internal/client"
	"github.com/hashicorp/terraform-exec/tfexec"
	tfjson "github.com/hashicorp/terraform-json"
	"github.com/oapi-codegen/nullable"
)

// Only Terraform Core can prove that a failed create retains and taints the
// UUID, and that untaint lets the next apply update it without another create.
func TestCollectionPartialCreateTerraformRecovery(t *testing.T) {
	const address = "outline_collection.test"
	const description = "# Recovered description\n\nMarkdown landing page."

	var mu sync.Mutex
	current := collectionTestCollection()
	created, updates, deleted := 0, 0, 0
	malformedDescription := true
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if req.Method != http.MethodPost || req.URL.RawQuery != "" || req.Header.Get("Authorization") != "Bearer "+groupTestKey ||
			(req.URL.Path != "/api/auth.info" && req.Header.Get("Content-Type") != "application/json") {
			t.Errorf("unexpected API request: %s %s or missing fixture credentials/JSON header", req.Method, req.URL)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if req.URL.Path == "/api/auth.info" {
			// Collections require an active admin, not merely an authenticated user.
			groupTestEncode(t, w, userTestAuth(userTestOwner()))
			return
		}
		var body map[string]any
		if !groupTestDecode(t, w, req, &body) {
			return
		}
		switch req.URL.Path {
		case "/api/collections.create":
			created++
			want := map[string]any{"name": "Engineering", "description": description, "permission": "read", "sharing": true}
			if created != 1 || !reflect.DeepEqual(body, want) {
				t.Errorf("unexpected create #%d: %v; want %v", created, body, want)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			current.Permission = nullable.NewNullableWithValue(client.PermissionRead)
			current.Sharing = groupTestPointer(true)
			// The collection exists, but its description is still empty and the
			// response contains an object where generated decoding expects text.
		case "/api/collections.update":
			updates++
			want := map[string]any{"id": collectionTestID, "name": "Engineering", "description": description, "permission": "read", "sharing": true}
			if created != 1 || deleted != 0 || malformedDescription || !reflect.DeepEqual(body, want) {
				t.Errorf("unexpected update: %v; creates=%d deletes=%d malformed=%t", body, created, deleted, malformedDescription)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			current.Description = nullable.NewNullableWithValue(description)
		case "/api/collections.info", "/api/collections.delete":
			if created != 1 || deleted != 0 || !reflect.DeepEqual(body, map[string]any{"id": collectionTestID}) {
				t.Errorf("unexpected %s: body=%v creates=%d deletes=%d", req.URL.Path, body, created, deleted)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if req.URL.Path == "/api/collections.delete" {
				deleted++
				groupTestWrite(t, w, `{"ok":true,"status":200,"success":true}`)
				return
			}
		default:
			t.Errorf("unexpected API endpoint: %s", req.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if malformedDescription {
			fields := collectionTestFields()
			fields["description"] = map[string]any{}
			fields["permission"] = current.Permission.GetOrEmpty()
			fields["sharing"] = *current.Sharing
			// Keep valid JSON, HTTP 200, every required field, and the canonical
			// data.id. Only description prevents the generated parser succeeding.
			groupTestEncode(t, w, map[string]any{"ok": true, "status": http.StatusOK, "data": fields})
			return
		}
		groupTestEncode(t, w, collectionTestEnvelope(current))
	}))
	t.Cleanup(server.Close)

	checkCalls := func(wantUpdates, wantDeletes int) {
		t.Helper()
		mu.Lock()
		defer mu.Unlock()
		if created != 1 || updates != wantUpdates || deleted != wantDeletes {
			t.Fatalf("API calls: create=%d update=%d delete=%d; want 1/%d/%d", created, updates, deleted, wantUpdates, wantDeletes)
		}
	}

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
resource "outline_collection" "test" {
  name = "Engineering"
  description = %q
  permission = "read"
  sharing = true
  allow_destroy = true
}
`, providerAddress, server.URL+"/api", groupTestKey, description))
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	if err := tf.Init(ctx, tfexec.Reattach(reattach)); err != nil {
		t.Fatalf("terraform init: %v", err)
	}
	cleanedUp := false
	t.Cleanup(func() {
		if cleanedUp {
			return
		}
		// Also allow cleanup after an early assertion failure. The retained
		// state already has allow_destroy=true, but reads must decode first.
		mu.Lock()
		malformedDescription = false
		mu.Unlock()
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		if err := tf.Destroy(cleanupCtx, tfexec.Reattach(reattach)); err != nil {
			t.Errorf("terraform cleanup destroy: %v", err)
		}
	})

	if err := tf.Apply(ctx, tfexec.Reattach(reattach)); err == nil {
		t.Fatal("terraform apply succeeded despite the malformed description")
	} else if got := err.Error(); !strings.Contains(got, "Unable to create collection") || !strings.Contains(got, "collections.create") ||
		!strings.Contains(got, "could not be decoded") || !strings.Contains(got, "UUID has been retained in state") || !strings.Contains(got, "terraform untaint") {
		t.Fatalf("terraform apply failed for the wrong reason: %v", err)
	}
	checkState := func(wantStatus string) {
		t.Helper()
		// The shared dynamic StatePull helper includes taint; show -json does not.
		state := userPartialCreateState(t, ctx, tf)
		if len(state.Resources) != 1 {
			t.Fatalf("want one retained resource, got %+v", state.Resources)
		}
		r := state.Resources[0]
		if r.Mode != "managed" || r.Type != "outline_collection" || r.Name != "test" || len(r.Instances) != 1 {
			t.Fatalf("unexpected retained resource: %+v", r)
		}
		instance := r.Instances[0]
		want := map[string]any{
			"id": collectionTestID, "name": "Engineering", "description": description,
			"permission": "read", "sharing": true, "allow_destroy": true,
		}
		if instance.Status != wantStatus || instance.Deposed != "" || !reflect.DeepEqual(instance.Attributes, want) {
			t.Fatalf("unexpected persisted instance: %+v; want status=%q attributes=%v", instance, wantStatus, want)
		}
	}
	checkState("tainted")
	checkCalls(0, 0)

	mu.Lock()
	malformedDescription = false
	mu.Unlock()
	plan := collectionPartialCreatePlan(t, ctx, tf, reattach, "tainted.tfplan")
	if !plan.Change.Actions.DestroyBeforeCreate() {
		t.Fatalf("plain retry must replace the tainted collection, got %v", plan.Change.Actions)
	}
	before, ok := plan.Change.Before.(map[string]any)
	if !ok || before["id"] != collectionTestID {
		t.Fatalf("replacement plan lost the retained UUID: %+v", plan.Change.Before)
	}
	checkState("tainted")
	checkCalls(0, 0)

	if err := tf.Untaint(ctx, address); err != nil {
		t.Fatalf("terraform untaint %s: %v", address, err)
	}
	checkState("")
	plan = collectionPartialCreatePlan(t, ctx, tf, reattach, "recovery.tfplan")
	if !plan.Change.Actions.Update() {
		t.Fatalf("untainted recovery must update in place, got %v", plan.Change.Actions)
	}
	before, beforeOK := plan.Change.Before.(map[string]any)
	after, afterOK := plan.Change.After.(map[string]any)
	want := map[string]any{
		"id": collectionTestID, "name": "Engineering", "description": description,
		"permission": "read", "sharing": true, "allow_destroy": true,
	}
	if !beforeOK || !afterOK || before["id"] != collectionTestID || before["description"] != "" || !reflect.DeepEqual(after, want) {
		t.Fatalf("recovery plan must retain the UUID and reconcile the configuration: %+v", plan.Change)
	}
	checkCalls(0, 0)
	if err := tf.Apply(ctx, tfexec.Reattach(reattach), tfexec.DirOrPlan("recovery.tfplan")); err != nil {
		t.Fatalf("terraform recovery apply: %v", err)
	}
	checkState("")
	checkCalls(1, 0)
	if changed, err := tf.Plan(ctx, tfexec.Reattach(reattach)); err != nil || changed {
		t.Fatalf("recovered resource must have an empty plan: changed=%t err=%v", changed, err)
	}
	checkState("")
	checkCalls(1, 0)

	if err := tf.Destroy(ctx, tfexec.Reattach(reattach)); err != nil {
		t.Fatalf("terraform destroy: %v", err)
	}
	cleanedUp = true
	checkCalls(1, 1)
	if state := userPartialCreateState(t, ctx, tf); len(state.Resources) != 0 {
		t.Fatalf("destroy left resources in state: %+v", state.Resources)
	}
}

func collectionPartialCreatePlan(t *testing.T, ctx context.Context, tf *tfexec.Terraform, reattach tfexec.ReattachInfo, filename string) *tfjson.ResourceChange {
	t.Helper()
	if changed, err := tf.Plan(ctx, tfexec.Reattach(reattach), tfexec.Out(filename)); err != nil || !changed {
		t.Fatalf("terraform plan %s: changed=%t err=%v", filename, changed, err)
	}
	plan, err := tf.ShowPlanFile(ctx, filename, tfexec.Reattach(reattach))
	if err != nil {
		t.Fatalf("terraform show %s: %v", filename, err)
	}
	if len(plan.ResourceChanges) != 1 || plan.ResourceChanges[0].Address != "outline_collection.test" || plan.ResourceChanges[0].Change == nil {
		t.Fatalf("unexpected resource changes: %+v", plan.ResourceChanges)
	}
	return plan.ResourceChanges[0]
}
