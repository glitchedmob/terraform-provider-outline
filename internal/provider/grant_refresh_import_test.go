// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/hashicorp/terraform-exec/tfexec"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

func grantRefreshImportFixture(t *testing.T, malformed bool) *grantRefreshScaleFixture {
	t.Helper()
	f := newGrantRefreshScaleFixture(grantRefreshScaleCases[2], false)
	if malformed {
		// The query would include only the valid first-page target. The import
		// discovery must still reject an unrelated malformed final-page row.
		page := cuUnitJSONMap(t, grantRefreshScalePage(f.resource, 900, 1000, 900, 1000, grantRefreshScaleName, f.resource.permission))
		page["data"].(map[string]any)["groupMemberships"].([]any)[99].(map[string]any)["permission"] = "invalid"
		f.pages[900] = grantRefreshScaleEncode(page)
	}
	return f
}

func TestGrantRefreshGroupMemberImportReadsUnfiltered(t *testing.T) {
	for _, malformed := range []bool{false, true} {
		t.Run(fmt.Sprintf("malformed=%t", malformed), func(t *testing.T) {
			f := grantRefreshImportFixture(t, malformed)
			c := f.resource
			server, _ := grantRefreshScaleProtocol(t, f)
			imported, err := server.ImportResourceState(t.Context(), &tfprotov6.ImportResourceStateRequest{
				TypeName: c.typeName, ID: c.attributes(0)["id"],
			})
			if err != nil || imported == nil || protocolHasError(imported.Diagnostics) || len(imported.ImportedResources) != 1 {
				t.Fatalf("import: %v %v", imported, err)
			}
			if len(f.counts) != 0 {
				t.Fatalf("pair-only import unexpectedly called the API: %v", f.counts)
			}
			initial := imported.ImportedResources[0].State
			typ := tftypes.Object{AttributeTypes: map[string]tftypes.Type{
				"id": tftypes.String, "group_id": tftypes.String, "user_id": tftypes.String, "permission": tftypes.String,
			}}
			value, err := initial.Unmarshal(typ)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]tftypes.Value
			if err := value.As(&fields); err != nil || !fields["permission"].IsNull() {
				t.Fatalf("import must leave permission for discovery: %v %v", fields, err)
			}
			read, err := server.ReadResource(t.Context(), &tfprotov6.ReadResourceRequest{TypeName: c.typeName, CurrentState: initial})
			if err != nil || read == nil || read.NewState == nil || protocolHasError(read.Diagnostics) != malformed {
				t.Fatalf("import discovery: %v %v", read, err)
			}
			grantRefreshScaleAssertCounts(t, f, 1, 10)
			if len(f.queries) != 1 || len(f.queries[""]) != 10 {
				t.Fatalf("import discovery did not use every unfiltered page: %v", f.queries)
			}
			if malformed {
				unchanged, err := read.NewState.Unmarshal(typ)
				if err != nil || !unchanged.Equal(value) {
					t.Fatalf("failed import read changed its state: %v %v", unchanged, err)
				}
				return
			}
			grantRefreshScaleAssertState(t, read.NewState, c.attributes(0))
			f.resetCounts()
			// Once discovery has populated permission, ordinary refresh may query.
			refreshed, err := server.ReadResource(t.Context(), &tfprotov6.ReadResourceRequest{TypeName: c.typeName, CurrentState: read.NewState})
			if err != nil || refreshed == nil || protocolHasError(refreshed.Diagnostics) {
				t.Fatalf("subsequent refresh: %v %v", refreshed, err)
			}
			grantRefreshScaleAssertState(t, refreshed.NewState, c.attributes(0))
			grantRefreshScaleAssertCounts(t, f, 1, 1)
		})
	}
}

func TestGrantRefreshGroupMemberTerraformImport(t *testing.T) {
	if os.Getenv("TF_ACC_TERRAFORM_PATH") == "" {
		if _, err := exec.LookPath("terraform"); err != nil {
			t.Skip("Terraform is not on PATH; set TF_ACC_TERRAFORM_PATH to run CLI regressions")
		}
	}
	for _, malformed := range []bool{false, true} {
		t.Run(fmt.Sprintf("malformed=%t", malformed), func(t *testing.T) {
			const address = "registry.terraform.io/glitchedmob/outline"
			f := grantRefreshImportFixture(t, malformed)
			p := newGrantRefreshScaleProvider(f)
			reattach := grantRefreshScaleCLIProvider(t, address, p)
			// Include only an inert fixture key in this isolated CLI configuration.
			config := fmt.Sprintf(`terraform {
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
  permission = "member"
}
`, address, grantRefreshScaleURL, groupTestKey, f.resource.parentID, grantRefreshScaleID(0))
			tf := groupPartialCreateTerraform(t, config)
			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			defer cancel()
			err := tf.Import(ctx, "outline_group_member.test", f.resource.attributes(0)["id"], tfexec.Reattach(reattach))
			if (err != nil) != malformed {
				t.Fatalf("import malformed=%t: %v", malformed, err)
			}
			grantRefreshScaleAssertCounts(t, f, 1, 10)
			if len(f.queries) != 1 || len(f.queries[""]) != 10 {
				t.Fatalf("CLI import discovery did not use every unfiltered page: %v", f.queries)
			}
			if malformed {
				if state, err := os.ReadFile(filepath.Join(tf.WorkingDir(), "terraform.tfstate")); err == nil {
					t.Fatalf("failed import unexpectedly saved state: %s", state)
				} else if !os.IsNotExist(err) {
					t.Fatal(err)
				}
				return
			}
			f.resetCounts()
			changed, err := tf.Plan(ctx, tfexec.Reattach(reattach))
			if err != nil || changed {
				t.Fatalf("post-import plan changed=%t: %v", changed, err)
			}
			grantRefreshScaleAssertCounts(t, f, 1, 1)
		})
	}
}
