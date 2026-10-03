// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"net/http"
	"reflect"
	"testing"

	"github.com/glitchedmob/terraform-provider-outline/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

func TestUserTemporaryActivationRefusalUsesFreshAccount(t *testing.T) {
	t.Parallel()
	for _, consent := range []struct {
		name  string
		value types.Bool
	}{
		{"default false", types.BoolValue(false)},
		{"null", types.BoolNull()},
		{"unknown", types.BoolUnknown()},
	} {
		for _, suspendedAfterPlan := range []bool{false, true} {
			t.Run(consent.name+"/external suspension="+types.BoolValue(suspendedAfterPlan).String(), func(t *testing.T) {
				t.Parallel()
				current := userTestUser()
				current.IsSuspended = groupTestPointer(true)
				var calls []string
				r := &userResource{api: userTestClient(t, func(w http.ResponseWriter, req *http.Request) {
					calls = append(calls, req.URL.Path)
					switch req.URL.Path {
					case "/api/auth.info":
						groupTestEncode(t, w, userTestAuth(userTestOwner()))
					case "/api/users.info":
						groupTestEncode(t, w, userTestEnvelope(current))
					default:
						t.Errorf("refusal must make zero mutations, including cleanup: %s", req.URL.Path)
					}
				})}
				model := userTestModel()
				model.Role, model.Name, model.Suspended = types.StringValue("guest"), types.StringValue("Refused rename"), types.BoolValue(true)
				model.AllowTemporaryActivationForRoleChange = consent.value
				plan := userTestPlan(t, r, model)
				prior := userTestModel()
				prior.Suspended = types.BoolValue(!suspendedAfterPlan)
				state := tfsdk.State(userTestPlan(t, r, prior))
				response := resource.UpdateResponse{State: state}
				r.Update(t.Context(), resource.UpdateRequest{Plan: plan, Config: tfsdk.Config(plan), State: state}, &response)
				groupTestDiagnostics(t, response.Diagnostics, "refusing to temporarily activate")
				groupTestDiagnostics(t, response.Diagnostics, "allow_temporary_activation_for_role_change = true")
				got := userTestStateModel(t, response.State)
				if got.Role.ValueString() != "member" || got.Name.ValueString() != "OIDC User" || !got.Suspended.ValueBool() ||
					got.AllowTemporaryActivationForRoleChange != consent.value || !reflect.DeepEqual(calls, []string{"/api/auth.info", "/api/users.info"}) {
					t.Fatalf("refusal changed the account or lost the fresh observation: %+v calls=%v", got, calls)
				}
			})
		}
	}
}

func TestUserTemporaryActivationSafePaths(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                                      string
		initialSuspended, finalSuspended, consent bool
		role, displayName                         string
		want                                      []string
	}{
		{"retired unchanged", true, true, false, "member", "OIDC User", nil},
		{"retired name only", true, true, false, "member", "Renamed", []string{"update"}},
		{"enable option only", true, true, true, "member", "OIDC User", nil},
		{"explicit reactivation", true, false, false, "member", "OIDC User", []string{"activate"}},
		{"explicit reactivation with role and name", true, false, false, "guest", "Renamed", []string{"activate", "update_role", "update"}},
		{"active role and name then suspend", false, true, false, "guest", "Renamed", []string{"update_role", "update", "suspend"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			current := userTestUser()
			current.IsSuspended = groupTestPointer(tc.initialSuspended)
			var mutations []string
			r := &userResource{api: userTestAdminClient(t, func(w http.ResponseWriter, req *http.Request) {
				op := req.URL.Path[len("/api/users."):]
				if op != "info" {
					mutations = append(mutations, op)
				}
				switch op {
				case "info":
				case "activate":
					current.IsSuspended = groupTestPointer(false)
				case "suspend":
					current.IsSuspended = groupTestPointer(true)
				case "update_role":
					current.Role = groupTestPointer(client.UserRole(tc.role))
				case "update":
					current.Name = groupTestPointer(tc.displayName)
				default:
					t.Errorf("unexpected write: %s", op)
				}
				groupTestEncode(t, w, userTestEnvelope(current))
			})}
			model := userTestModel()
			model.Role, model.Name, model.Suspended = types.StringValue(tc.role), types.StringValue(tc.displayName), types.BoolValue(tc.finalSuspended)
			model.AllowTemporaryActivationForRoleChange = types.BoolValue(tc.consent)
			response := userTestUpdate(t, r, model, true)
			got := userTestStateModel(t, response.State)
			if response.Diagnostics.HasError() || !reflect.DeepEqual(mutations, tc.want) || got.Role != model.Role || got.Name != model.Name || got.Suspended != model.Suspended || got.AllowTemporaryActivationForRoleChange != model.AllowTemporaryActivationForRoleChange {
				t.Fatalf("safe path: diagnostics=%v state=%+v mutations=%v want=%v", response.Diagnostics, got, mutations, tc.want)
			}
		})
	}
}

func TestUserCreateRefusesTemporaryActivationAfterFreshSuspension(t *testing.T) {
	t.Parallel()
	current := userTestUser()
	var calls []string
	r := &userResource{api: userTestAdminClient(t, func(w http.ResponseWriter, req *http.Request) {
		calls = append(calls, req.URL.Path)
		switch req.URL.Path {
		case "/api/users.list":
			groupTestEncode(t, w, userTestList([]client.User{}, 0, 100, 0))
		case "/api/users.invite":
			groupTestEncode(t, w, userTestInviteEnvelope(current))
		case "/api/users.info":
			// Another administrator retires the account after invitation.
			current.IsSuspended = groupTestPointer(true)
			groupTestEncode(t, w, userTestEnvelope(current))
		default:
			t.Errorf("fresh suspension must refuse every follow-up mutation: %s", req.URL.Path)
		}
	})}
	model := userTestModel()
	model.Role, model.Suspended = types.StringValue("guest"), types.BoolValue(true)
	response := userTestCreate(t, r, model)
	groupTestDiagnostics(t, response.Diagnostics, "refusing to temporarily activate")
	groupTestDiagnostics(t, response.Diagnostics, "user ID has been retained")
	got := userTestStateModel(t, response.State)
	if got.ID.ValueString() != userTestID || got.Role.ValueString() != "member" || !got.Suspended.ValueBool() || got.AllowTemporaryActivationForRoleChange != types.BoolValue(false) ||
		!reflect.DeepEqual(calls, []string{"/api/users.list", "/api/users.invite", "/api/users.info"}) {
		t.Fatalf("failed create lost the retired account observation: %+v calls=%v", got, calls)
	}
}
