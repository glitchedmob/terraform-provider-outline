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
	"testing"
	"time"

	"github.com/glitchedmob/terraform-provider-outline/internal/client"
	"github.com/hashicorp/terraform-exec/tfexec"
	tfjson "github.com/hashicorp/terraform-json"
)

// Terraform must retain and taint the invited UUID after reconciliation fails.
// Recover it with a real untaint and in-place apply, without another invitation.
func TestUserPartialCreateTerraformRecovery(t *testing.T) {
	const address = "outline_user.test"
	var mu sync.Mutex
	current := userTestUser()
	current.Name = groupTestPointer("Pending user")
	invites, roles, activates, suspends, deletes := 0, 0, 0, 0, 0
	denyRole := true
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
		switch req.URL.Path {
		case "/api/auth.info":
			groupTestEncode(t, w, userTestAuth(userTestOwner()))
			return
		case "/api/users.list":
			userTestListOffset(t, w, req)
			users := []client.User{}
			if invites != 0 {
				users = append(users, *current)
			}
			groupTestEncode(t, w, userTestList(users, 0, 100, len(users)))
			return
		case "/api/users.invite":
			invites++
			var body client.UsersInviteJSONRequestBody
			if !groupTestDecode(t, w, req, &body) {
				return
			}
			if invites != 1 || len(body.Invites) != 1 || body.Invites[0].Email != userTestEmail || body.Invites[0].Name != "Pending user" ||
				body.Invites[0].Role != client.UserRoleGuest || body.SuppressEmail == nil || !*body.SuppressEmail {
				t.Errorf("unexpected invitation #%d: %+v", invites, body)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			// Outline stores a member first, despite the requested guest role.
			groupTestEncode(t, w, userTestInviteEnvelope(current))
			return
		case "/api/users.info", "/api/users.update_role", "/api/users.activate", "/api/users.suspend", "/api/users.delete":
			var body map[string]any
			if !groupTestDecode(t, w, req, &body) {
				return
			}
			want := map[string]any{"id": userTestID}
			if req.URL.Path == "/api/users.update_role" {
				want["role"] = "guest"
			}
			if invites != 1 || deletes != 0 || !reflect.DeepEqual(body, want) {
				t.Errorf("unexpected %s: body=%v invites=%d deletes=%d", req.URL.Path, body, invites, deletes)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			switch req.URL.Path {
			case "/api/users.update_role":
				roles++
				if *current.IsSuspended {
					t.Error("role update did not temporarily activate the suspended user")
				}
				if denyRole {
					w.WriteHeader(http.StatusForbidden)
					groupTestWrite(t, w, `{"ok":false,"status":403,"error":"authorization_error","message":"guest role update denied"}`)
					return
				}
				current.Role = groupTestPointer(client.UserRoleGuest)
			case "/api/users.activate":
				activates++
				current.IsSuspended = groupTestPointer(false)
			case "/api/users.suspend":
				suspends++
				current.IsSuspended = groupTestPointer(true)
			case "/api/users.delete":
				deletes++
				t.Error("default destroy must not permanently delete the invited account")
				groupTestWrite(t, w, `{"ok":true,"status":200,"success":true}`)
				return
			}
		default:
			t.Errorf("unexpected API endpoint: %s", req.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		groupTestEncode(t, w, userTestEnvelope(current))
	}))
	t.Cleanup(server.Close)

	checkCalls := func(wantRoles, wantActivates, wantSuspends int) {
		t.Helper()
		mu.Lock()
		defer mu.Unlock()
		if invites != 1 || roles != wantRoles || activates != wantActivates || suspends != wantSuspends || deletes != 0 || !*current.IsSuspended {
			t.Fatalf("API calls: invite=%d role=%d activate=%d suspend=%d delete=%d suspended=%t; want 1/%d/%d/%d/0/true",
				invites, roles, activates, suspends, deletes, *current.IsSuspended, wantRoles, wantActivates, wantSuspends)
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
resource "outline_user" "test" {
  email = "OIDC@EXAMPLE.COM"
  role = "guest"
  suspended = true
  allow_temporary_activation_for_role_change = true
}
`, providerAddress, server.URL+"/api", groupTestKey))
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
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		if err := tf.Destroy(cleanupCtx, tfexec.Reattach(reattach)); err != nil {
			t.Errorf("terraform cleanup destroy: %v", err)
		}
	})

	if err := tf.Apply(ctx, tfexec.Reattach(reattach)); err == nil {
		t.Fatal("terraform apply succeeded despite the forbidden guest role update")
	} else if got := err.Error(); !strings.Contains(got, "User created but reconciliation failed") || !strings.Contains(got, "HTTP 403") ||
		!strings.Contains(got, "terraform untaint") {
		t.Fatalf("terraform apply failed for the wrong reason: %v", err)
	}
	checkState := func(wantStatus, wantRole string) {
		t.Helper()
		state := userPartialCreateState(t, ctx, tf)
		if len(state.Resources) != 1 {
			t.Fatalf("want one retained resource, got %+v", state.Resources)
		}
		r := state.Resources[0]
		if r.Mode != "managed" || r.Type != "outline_user" || r.Name != "test" || len(r.Instances) != 1 {
			t.Fatalf("unexpected retained resource: %+v", r)
		}
		instance := r.Instances[0]
		want := map[string]any{
			"id": userTestID, "email": "OIDC@EXAMPLE.COM", "name": "Pending user", "role": wantRole,
			"suspended": true, "suppress_email": true, "delete_permanently": false,
			"allow_temporary_activation_for_role_change": true,
		}
		if instance.Status != wantStatus || instance.Deposed != "" || !reflect.DeepEqual(instance.Attributes, want) {
			t.Fatalf("unexpected persisted instance: %+v; want status=%q attributes=%v", instance, wantStatus, want)
		}
	}
	checkState("tainted", "member")
	checkCalls(1, 0, 1)

	mu.Lock()
	denyRole = false
	mu.Unlock()
	plan := userPartialCreatePlan(t, ctx, tf, reattach, "tainted.tfplan")
	if !plan.Change.Actions.DestroyBeforeCreate() {
		t.Fatalf("plain retry must replace the tainted user, got %v", plan.Change.Actions)
	}
	before, ok := plan.Change.Before.(map[string]any)
	if !ok || before["id"] != userTestID || before["suspended"] != true {
		t.Fatalf("replacement plan lost the retained suspended UUID: %+v", plan.Change.Before)
	}
	checkState("tainted", "member")
	checkCalls(1, 0, 1)

	if err := tf.Untaint(ctx, address); err != nil {
		t.Fatalf("terraform untaint %s: %v", address, err)
	}
	checkState("", "member")
	plan = userPartialCreatePlan(t, ctx, tf, reattach, "recovery.tfplan")
	if !plan.Change.Actions.Update() {
		t.Fatalf("untainted recovery must update in place, got %v", plan.Change.Actions)
	}
	before, beforeOK := plan.Change.Before.(map[string]any)
	after, afterOK := plan.Change.After.(map[string]any)
	if !beforeOK || !afterOK || before["id"] != userTestID || after["id"] != userTestID || after["role"] != "guest" || after["suspended"] != true {
		t.Fatalf("recovery plan must retain the UUID and request a suspended guest: %+v", plan.Change)
	}
	checkCalls(1, 0, 1)
	if err := tf.Apply(ctx, tfexec.Reattach(reattach), tfexec.DirOrPlan("recovery.tfplan")); err != nil {
		t.Fatalf("terraform recovery apply: %v", err)
	}
	checkState("", "guest")
	checkCalls(2, 1, 2)
	if changed, err := tf.Plan(ctx, tfexec.Reattach(reattach)); err != nil || changed {
		t.Fatalf("recovered resource must have an empty plan: changed=%t err=%v", changed, err)
	}
	checkCalls(2, 1, 2)

	if err := tf.Destroy(ctx, tfexec.Reattach(reattach)); err != nil {
		t.Fatalf("terraform destroy: %v", err)
	}
	cleanedUp = true
	checkCalls(2, 1, 2)
	if state := userPartialCreateState(t, ctx, tf); len(state.Resources) != 0 {
		t.Fatalf("destroy left resources in state: %+v", state.Resources)
	}
}

// StatePull includes taint, unlike terraform show -json. Decode attributes
// dynamically rather than using the group helper's group-specific model.
type userPartialCreateRawState struct {
	Resources []struct {
		Mode      string `json:"mode"`
		Type      string `json:"type"`
		Name      string `json:"name"`
		Instances []struct {
			Status     string         `json:"status"`
			Deposed    string         `json:"deposed"`
			Attributes map[string]any `json:"attributes"`
		} `json:"instances"`
	} `json:"resources"`
}

func userPartialCreateState(t *testing.T, ctx context.Context, tf *tfexec.Terraform) userPartialCreateRawState {
	t.Helper()
	raw, err := tf.StatePull(ctx)
	if err != nil {
		t.Fatalf("terraform state pull: %v", err)
	}
	var state userPartialCreateRawState
	if err := json.Unmarshal([]byte(raw), &state); err != nil {
		t.Fatalf("decode Terraform state: %v", err)
	}
	return state
}

func userPartialCreatePlan(t *testing.T, ctx context.Context, tf *tfexec.Terraform, reattach tfexec.ReattachInfo, filename string) *tfjson.ResourceChange {
	t.Helper()
	if changed, err := tf.Plan(ctx, tfexec.Reattach(reattach), tfexec.Out(filename)); err != nil || !changed {
		t.Fatalf("terraform plan %s: changed=%t err=%v", filename, changed, err)
	}
	plan, err := tf.ShowPlanFile(ctx, filename, tfexec.Reattach(reattach))
	if err != nil {
		t.Fatalf("terraform show %s: %v", filename, err)
	}
	if len(plan.ResourceChanges) != 1 || plan.ResourceChanges[0].Address != "outline_user.test" || plan.ResourceChanges[0].Change == nil {
		t.Fatalf("unexpected resource changes: %+v", plan.ResourceChanges)
	}
	return plan.ResourceChanges[0]
}
