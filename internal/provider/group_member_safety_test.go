// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"net/http"
	"testing"

	"github.com/glitchedmob/terraform-provider-outline/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

func TestGroupMemberGroupRoute404NeedsVerifiedAbsence(t *testing.T) {
	for _, evidence := range []string{"absent", "present", "list forbidden", "list 404", "list malformed"} {
		t.Run(evidence, func(t *testing.T) {
			lists := 0
			r := &groupMemberResource{api: memberTestClient(t, func(w http.ResponseWriter, req *http.Request) {
				switch req.URL.Path {
				case "/api/auth.info":
					groupTestEncode(t, w, userTestAuth(userTestOwner()))
				case "/api/groups.info":
					// A proxy can return this for a parent that still exists.
					w.WriteHeader(404)
					groupTestEncode(t, w, map[string]any{"error": "not_found"})
				case "/api/groups.list":
					lists++
					memberTestBody(t, w, req, map[string]any{"limit": float64(100), "offset": float64(0)})
					if evidence == "list forbidden" || evidence == "list 404" {
						status := 403
						if evidence == "list 404" {
							status = 404
						}
						w.WriteHeader(status)
						groupTestEncode(t, w, map[string]any{"error": "authorization_error"})
						return
					}
					groups := []client.Group{}
					if evidence == "present" {
						groups = append(groups, *groupTestGroup())
					}
					page := map[string]any{"ok": true, "data": map[string]any{"groups": groups}, "pagination": client.PaginationResponse{Limit: groupTestPointer(100), Offset: groupTestPointer(0), Total: groupTestPointer(len(groups))}}
					if evidence == "list malformed" {
						delete(page, "pagination")
					}
					groupTestEncode(t, w, page)
				default:
					t.Errorf("unverified 404 triggered a membership operation: %s", req.URL.Path)
				}
			})}
			state := tfsdk.State(memberTestPlan(t, r, memberTestModel()))
			read := resource.ReadResponse{State: state}
			r.Read(t.Context(), resource.ReadRequest{State: state}, &read)
			absent := evidence == "absent"
			if read.Diagnostics.HasError() == absent || read.State.Raw.IsNull() != absent || lists != 1 {
				t.Fatalf("unverified absence=%s: %v lists=%d", evidence, read.Diagnostics, lists)
			}
			if !absent && !read.State.Raw.Equal(state.Raw) {
				t.Fatal("route 404 lost the membership pair")
			}
			deleted := resource.DeleteResponse{State: state}
			r.Delete(t.Context(), resource.DeleteRequest{State: state}, &deleted)
			if deleted.Diagnostics.HasError() == absent || lists != 2 {
				t.Fatalf("delete did not verify parent absence: %v lists=%d", deleted.Diagnostics, lists)
			}
		})
	}
}

func TestGroupMemberMissingUpdateAndReplacementNeverUpsert(t *testing.T) {
	for _, change := range []string{"absent", "group", "user"} {
		t.Run(change, func(t *testing.T) {
			r := &groupMemberResource{api: memberTestClient(t, func(w http.ResponseWriter, req *http.Request) {
				if change != "absent" {
					t.Errorf("pair replacement attempted an API call: %s", req.URL.Path)
					return
				}
				if memberTestParents(t, w, req) {
					return
				}
				if req.URL.Path == "/api/groups.memberships" {
					groupTestEncode(t, w, memberTestEnvelope([]client.GroupUser{}, 0, 0, false))
					return
				}
				t.Errorf("missing membership update attempted upsert: %s", req.URL.Path)
			})}
			state := tfsdk.State(memberTestPlan(t, r, memberTestModel()))
			model := memberTestModel()
			model.Permission = types.StringValue("admin")
			if change != "absent" {
				model.ID = types.StringUnknown()
				if change == "group" {
					model.GroupID = types.StringValue(groupTestOtherID)
				} else {
					model.UserID = types.StringValue(userTestOtherID)
				}
			}
			resp := resource.UpdateResponse{State: state}
			r.Update(t.Context(), resource.UpdateRequest{State: state, Plan: memberTestPlan(t, r, model)}, &resp)
			if !resp.Diagnostics.HasError() || !resp.State.Raw.Equal(state.Raw) {
				t.Fatal("missing/replaced pair changed state during Update")
			}
		})
	}
}

func TestGroupMemberModifyPlanUnknownParentsAndDestroy(t *testing.T) {
	for _, change := range []string{"group", "user", "destroy"} {
		t.Run(change, func(t *testing.T) {
			r := &groupMemberResource{}
			model := memberTestModel()
			model.ID = types.StringUnknown()
			switch change {
			case "group":
				model.GroupID = types.StringUnknown()
			case "user":
				model.UserID = types.StringUnknown()
			}
			plan := memberTestPlan(t, r, model)
			if change == "destroy" {
				plan.Raw = tftypes.NewValue(plan.Raw.Type(), nil)
			}
			resp := resource.ModifyPlanResponse{Plan: plan}
			r.ModifyPlan(t.Context(), resource.ModifyPlanRequest{Plan: plan}, &resp)
			if resp.Diagnostics.HasError() || !resp.Plan.Raw.Equal(plan.Raw) {
				t.Fatalf("unknown/destroy plan changed identity: %v", resp.Diagnostics)
			}
			if change != "destroy" && !memberTestState(t, tfsdk.State(resp.Plan)).ID.IsUnknown() {
				t.Fatal("unknown replacement pair retained the old ID")
			}
		})
	}
}
