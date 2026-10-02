// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"net/http"
	"strings"
	"testing"

	"github.com/glitchedmob/terraform-provider-outline/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

func TestOutlineStringLength(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		value    types.String
		min, max int
		invalid  bool
	}{
		{"ASCII name limit", types.StringValue(strings.Repeat("a", 255)), 1, 255, false},
		{"ASCII name overflow", types.StringValue(strings.Repeat("a", 256)), 1, 255, true},
		{"BMP counts once", types.StringValue(strings.Repeat("é", 255)), 1, 255, false},
		{"supplementary name limit", types.StringValue(strings.Repeat("😀", 127) + "a"), 1, 255, false},
		{"supplementary name overflow", types.StringValue(strings.Repeat("😀", 128)), 1, 255, true},
		{"supplementary description limit", types.StringValue(strings.Repeat("😀", 1000)), 0, 2000, false},
		{"supplementary description overflow", types.StringValue(strings.Repeat("😀", 1001)), 0, 2000, true},
		{"empty name", types.StringValue(""), 1, 255, true},
		{"empty description", types.StringValue(""), 0, 2000, false},
		{"null", types.StringNull(), 1, 255, false},
		{"unknown", types.StringUnknown(), 1, 255, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := outlineStringLength{min: tc.min, max: tc.max}
			var resp validator.StringResponse
			v.ValidateString(t.Context(), validator.StringRequest{Path: path.Root("name"), ConfigValue: tc.value}, &resp)
			if resp.Diagnostics.HasError() != tc.invalid {
				t.Fatalf("validation: %v", resp.Diagnostics)
			}
			if v.Description(t.Context()) != v.MarkdownDescription(t.Context()) || !strings.Contains(v.Description(t.Context()), "UTF-16") {
				t.Fatal("missing length semantics")
			}
		})
	}
}

func TestGroupExactNameChangedDuringLookup(t *testing.T) {
	t.Parallel()
	api := groupTestClient(t, func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/api/groups.list" {
			groupTestEncode(t, w, groupTestList([]client.Group{*groupTestGroup()}, 0, 100, 1))
			return
		}
		if req.URL.Path != "/api/groups.info" {
			t.Errorf("unexpected endpoint: %s", req.URL.Path)
		}
		group := groupTestGroup()
		group.Name = groupTestPointer("Renamed concurrently")
		groupTestEncode(t, w, groupTestEnvelope(group))
	})
	group, err := api.findGroup(t.Context(), "Engineering")
	if group != nil || err == nil || !strings.Contains(err.Error(), "name changed") {
		t.Fatalf("lookup accepted a nonmatching name: %v %v", group, err)
	}
}
