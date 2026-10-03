// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/glitchedmob/terraform-provider-outline/internal/client"
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-plugin"
	"github.com/hashicorp/terraform-exec/tfexec"
	tfjson "github.com/hashicorp/terraform-json"
	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6/tf6server"
	"github.com/oapi-codegen/nullable"
)

// A framework CreateResponse alone cannot show whether Terraform will keep the
// identity or taint it. Drive the CLI, including a real untaint, against the
// production provider and a local API instead of relying on acceptance fixtures.
func TestGroupPartialCreateTerraformRecovery(t *testing.T) {
	const address = "outline_group.test"
	const description = "Recovered description"

	var mu sync.Mutex
	current := groupTestGroup()
	created, updates, deleted := 0, 0, 0
	denyUpdate := true
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if req.Method != http.MethodPost || req.URL.RawQuery != "" ||
			req.Header.Get("Authorization") != "Bearer "+groupTestKey || req.Header.Get("Content-Type") != "application/json" {
			t.Errorf("unexpected API request: %s %s or missing fixture credentials/JSON header", req.Method, req.URL)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		switch req.URL.Path {
		case "/api/groups.create":
			created++
			var body client.GroupsCreateJSONRequestBody
			if !groupTestDecode(t, w, req, &body) {
				return
			}
			if created != 1 || body.Name != "Engineering" || body.DisableMentions == nil || !*body.DisableMentions {
				t.Errorf("unexpected create #%d: %+v", created, body)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			current.DisableMentions = body.DisableMentions
		case "/api/groups.update":
			updates++
			var body client.GroupsUpdateJSONRequestBody
			if !groupTestDecode(t, w, req, &body) {
				return
			}
			if body.Id.String() != groupTestID || body.Name == nil || *body.Name != "Engineering" ||
				body.Description == nil || *body.Description != description || body.DisableMentions == nil || !*body.DisableMentions {
				t.Errorf("unexpected update: %+v", body)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if denyUpdate {
				w.WriteHeader(http.StatusForbidden)
				groupTestWrite(t, w, `{"ok":false,"status":403,"error":"authorization_error","message":"description update denied"}`)
				return
			}
			current.Description = nullable.NewNullableWithValue(*body.Description)
		case "/api/groups.info", "/api/groups.delete":
			var body struct {
				ID string `json:"id"`
			}
			if !groupTestDecode(t, w, req, &body) {
				return
			}
			if created != 1 || deleted != 0 || body.ID != groupTestID {
				t.Errorf("unexpected %s: ID=%q, creates=%d, deletes=%d", req.URL.Path, body.ID, created, deleted)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if req.URL.Path == "/api/groups.delete" {
				deleted++
				groupTestWrite(t, w, `{"ok":true,"status":200,"success":true}`)
				return
			}
		default:
			t.Errorf("unexpected API endpoint: %s", req.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		groupTestEncode(t, w, groupTestEnvelope(current))
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
resource "outline_group" "test" {
  name = "Engineering"
  description = %q
  disable_mentions = true
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
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		if err := tf.Destroy(cleanupCtx, tfexec.Reattach(reattach)); err != nil {
			t.Errorf("terraform cleanup destroy: %v", err)
		}
	})

	if err := tf.Apply(ctx, tfexec.Reattach(reattach)); err == nil {
		t.Fatal("terraform apply succeeded despite the forbidden description update")
	} else if got := err.Error(); !strings.Contains(got, "Group created but description update failed") || !strings.Contains(got, "HTTP 403") {
		t.Fatalf("terraform apply failed for the wrong reason: %v", err)
	}
	checkState := func(wantStatus, wantDescription string) {
		t.Helper()
		state := groupPartialCreateState(t, ctx, tf)
		if len(state.Resources) != 1 {
			t.Fatalf("want one retained resource, got %+v", state.Resources)
		}
		r := state.Resources[0]
		if r.Mode != "managed" || r.Type != "outline_group" || r.Name != "test" || len(r.Instances) != 1 {
			t.Fatalf("unexpected retained resource: %+v", r)
		}
		instance := r.Instances[0]
		if instance.Status != wantStatus || instance.Deposed != "" || instance.Attributes.ID != groupTestID ||
			instance.Attributes.Name != "Engineering" || instance.Attributes.Description != wantDescription || !instance.Attributes.DisableMentions {
			t.Fatalf("unexpected persisted instance: %+v; want ID=%s status=%q description=%q", instance, groupTestID, wantStatus, wantDescription)
		}
	}
	checkState("tainted", "")
	checkCalls(1, 0)

	// Fixing API permissions does not clear Terraform's taint. A plain retry
	// would destroy the retained group and create another one.
	mu.Lock()
	denyUpdate = false
	mu.Unlock()
	plan := groupPartialCreatePlan(t, ctx, tf, reattach, "tainted.tfplan")
	if !plan.Change.Actions.DestroyBeforeCreate() {
		t.Fatalf("next apply must replace the tainted group, got %v", plan.Change.Actions)
	}
	before, ok := plan.Change.Before.(map[string]any)
	if !ok || before["id"] != groupTestID {
		t.Fatalf("replacement plan lost the retained ID: %+v", plan.Change.Before)
	}
	checkState("tainted", "")
	checkCalls(1, 0)

	if err := tf.Untaint(ctx, address); err != nil {
		t.Fatalf("terraform untaint %s: %v", address, err)
	}
	checkState("", "")
	plan = groupPartialCreatePlan(t, ctx, tf, reattach, "recovery.tfplan")
	if !plan.Change.Actions.Update() {
		t.Fatalf("untainted recovery must update in place, got %v", plan.Change.Actions)
	}
	before, beforeOK := plan.Change.Before.(map[string]any)
	after, afterOK := plan.Change.After.(map[string]any)
	if !beforeOK || !afterOK || before["id"] != groupTestID || after["id"] != groupTestID || after["description"] != description {
		t.Fatalf("recovery plan must retain the UUID and set the description: %+v", plan.Change)
	}
	checkCalls(1, 0)
	if err := tf.Apply(ctx, tfexec.Reattach(reattach), tfexec.DirOrPlan("recovery.tfplan")); err != nil {
		t.Fatalf("terraform recovery apply: %v", err)
	}
	checkState("", description)
	checkCalls(2, 0)
	if changed, err := tf.Plan(ctx, tfexec.Reattach(reattach)); err != nil || changed {
		t.Fatalf("recovered resource must have an empty plan: changed=%t err=%v", changed, err)
	}
	checkCalls(2, 0)

	if err := tf.Destroy(ctx, tfexec.Reattach(reattach)); err != nil {
		t.Fatalf("terraform destroy: %v", err)
	}
	cleanedUp = true
	checkCalls(2, 1)
	if state := groupPartialCreateState(t, ctx, tf); len(state.Resources) != 0 {
		t.Fatalf("destroy left resources in state: %+v", state.Resources)
	}
}

func groupPartialCreateProvider(t *testing.T, address string, wrap ...func(tfprotov6.ProviderServer) tfprotov6.ProviderServer) tfexec.ReattachInfo {
	t.Helper()
	factory := providerserver.NewProtocol6(New("unit")())
	if len(wrap) != 0 {
		original := factory
		factory = func() tfprotov6.ProviderServer { return wrap[0](original()) }
	}
	// Keep the server alive through cleanup, after testing cancels t.Context().
	ctx, cancel := context.WithCancel(context.Background())
	configCh := make(chan *plugin.ReattachConfig, 1)
	closeCh := make(chan struct{})
	serveCh := make(chan error, 1)
	go func() {
		serveCh <- tf6server.Serve(address, factory,
			tf6server.WithDebug(ctx, configCh, closeCh),
			tf6server.WithGoPluginLogger(hclog.NewNullLogger()),
			tf6server.WithLoggingSink(t), tf6server.WithoutLogStderrOverride())
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-serveCh:
			if err != nil {
				t.Errorf("provider server: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("provider server did not stop")
		}
	})
	select {
	case config := <-configCh:
		return tfexec.ReattachInfo{address: {
			Protocol: string(config.Protocol), ProtocolVersion: config.ProtocolVersion,
			Pid: config.Pid, Test: config.Test,
			Addr: tfexec.ReattachConfigAddr{Network: config.Addr.Network(), String: config.Addr.String()},
		}}
	case err := <-serveCh:
		// Leave the result available for the cleanup above.
		serveCh <- err
		t.Fatalf("provider server stopped before publishing its address: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("provider server did not publish its address")
	}
	return nil
}

func groupPartialCreateTerraform(t *testing.T, config string) *tfexec.Terraform {
	t.Helper()
	binary := os.Getenv("TF_ACC_TERRAFORM_PATH")
	if binary == "" {
		var err error
		binary, err = exec.LookPath("terraform")
		if err != nil {
			t.Fatalf("this CLI unit test requires Terraform on PATH or TF_ACC_TERRAFORM_PATH: %v", err)
		}
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.tf"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	cliConfig := filepath.Join(dir, "terraform.rc")
	if err := os.WriteFile(cliConfig, nil, 0600); err != nil {
		t.Fatal(err)
	}
	tf, err := tfexec.NewTerraform(dir, binary)
	if err != nil {
		t.Fatal(err)
	}
	// Do not inherit TF_VAR_*, credentials, CLI arguments, or the user's
	// Terraform configuration. The only provider is the reattached local server.
	if err := tf.SetEnv(map[string]string{
		"HOME": dir, "TF_CLI_CONFIG_FILE": cliConfig, "CHECKPOINT_DISABLE": "1",
		"PATH": os.Getenv("PATH"), "SystemRoot": os.Getenv("SystemRoot"),
	}); err != nil {
		t.Fatal(err)
	}
	return tf
}

// `terraform show -json` omits taint, so inspect `terraform state pull` instead.
type groupPartialCreateRawState struct {
	Resources []struct {
		Mode      string `json:"mode"`
		Type      string `json:"type"`
		Name      string `json:"name"`
		Instances []struct {
			Status     string `json:"status"`
			Deposed    string `json:"deposed"`
			Attributes struct {
				ID              string `json:"id"`
				Name            string `json:"name"`
				Description     string `json:"description"`
				DisableMentions bool   `json:"disable_mentions"`
			} `json:"attributes"`
		} `json:"instances"`
	} `json:"resources"`
}

func groupPartialCreateState(t *testing.T, ctx context.Context, tf *tfexec.Terraform) groupPartialCreateRawState {
	t.Helper()
	raw, err := tf.StatePull(ctx)
	if err != nil {
		t.Fatalf("terraform state pull: %v", err)
	}
	var state groupPartialCreateRawState
	if err := json.Unmarshal([]byte(raw), &state); err != nil {
		t.Fatalf("decode Terraform state: %v", err)
	}
	return state
}

func groupPartialCreatePlan(t *testing.T, ctx context.Context, tf *tfexec.Terraform, reattach tfexec.ReattachInfo, filename string) *tfjson.ResourceChange {
	t.Helper()
	if changed, err := tf.Plan(ctx, tfexec.Reattach(reattach), tfexec.Out(filename)); err != nil || !changed {
		t.Fatalf("terraform plan %s: changed=%t err=%v", filename, changed, err)
	}
	plan, err := tf.ShowPlanFile(ctx, filename, tfexec.Reattach(reattach))
	if err != nil {
		t.Fatalf("terraform show %s: %v", filename, err)
	}
	if len(plan.ResourceChanges) != 1 || plan.ResourceChanges[0].Address != "outline_group.test" || plan.ResourceChanges[0].Change == nil {
		t.Fatalf("unexpected resource changes: %+v", plan.ResourceChanges)
	}
	return plan.ResourceChanges[0]
}
