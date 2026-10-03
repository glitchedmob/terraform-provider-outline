// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/glitchedmob/terraform-provider-outline/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// Drive the resource against the released server and record every real mutation.
// Database timestamps plus zero activation requests establish that refusal never
// reactivates the account. This does not reproduce access with an existing JWT.
func TestAccUserTemporaryActivationConsent(t *testing.T) {
	api := newAcceptanceAPI(t)
	const email = "retired-role-change@example.invalid"
	users, err := api.acceptanceInviteUsers([]client.Invite{{Email: email, Name: "Retired user", Role: client.UserRoleMember}})
	if err != nil {
		t.Fatal(err)
	}
	id := users[0].Id.String()
	if err := api.acceptanceUserSuspended(id, true); err != nil {
		t.Fatal(err)
	}
	before := api.acceptanceInspectUser(t, id, "inspect")
	if before.SuspendedAt == nil || before.SuspendedByID == nil {
		t.Fatal("fixture account must already be suspended")
	}
	var mutations []string
	transport := api.httpClient.Transport.(*bearerTransport)
	base := transport.base
	transport.base = userTestRoundTripper(func(req *http.Request) (*http.Response, error) {
		if strings.HasPrefix(req.URL.Path, "/api/users.") && req.URL.Path != "/api/users.info" && req.URL.Path != "/api/users.list" {
			mutations = append(mutations, strings.TrimPrefix(req.URL.Path, "/api/users."))
		}
		return base.RoundTrip(req)
	})
	r := &userResource{api: api.apiClient}
	model := userTestModel()
	model.ID, model.Email, model.Name = types.StringValue(id), types.StringValue(email), types.StringValue("Refused rename")
	model.Role, model.Suspended = types.StringValue("guest"), types.BoolValue(true)
	response := userTestUpdate(t, r, model, true)
	groupTestDiagnostics(t, response.Diagnostics, "refusing to temporarily activate")
	got := userTestStateModel(t, response.State)
	if len(mutations) != 0 || got.Role.ValueString() != "member" || got.Name.ValueString() != "Retired user" || !got.Suspended.ValueBool() {
		t.Fatalf("default refusal mutated the account: state=%+v writes=%v", got, mutations)
	}
	assertContinuouslySuspended := func() {
		t.Helper()
		observed := api.acceptanceInspectUser(t, id, "inspect")
		if observed.SuspendedAt == nil || *observed.SuspendedAt != *before.SuspendedAt || observed.SuspendedByID == nil || *observed.SuspendedByID != *before.SuspendedByID {
			t.Fatal("retired account's suspension changed without consent")
		}
	}
	assertContinuouslySuspended()
	t.Log("Default refusal: zero mutations, unchanged database suspension timestamp and actor")

	// Safe name-only updates must work on a retired account without activation.
	model.Role, model.Name = types.StringValue("member"), types.StringValue("Renamed retired user")
	response = userTestUpdate(t, r, model, true)
	if response.Diagnostics.HasError() || !reflect.DeepEqual(mutations, []string{"update"}) {
		t.Fatalf("name-only update activated the account: %v writes=%v", response.Diagnostics, mutations)
	}
	assertContinuouslySuspended()
	for _, consent := range []bool{true, false} {
		mutations = nil
		model.AllowTemporaryActivationForRoleChange = types.BoolValue(consent)
		response = userTestUpdate(t, r, model, true)
		if response.Diagnostics.HasError() || len(mutations) != 0 {
			t.Fatalf("option-only update wrote to Outline: %v writes=%v", response.Diagnostics, mutations)
		}
		assertContinuouslySuspended()
	}

	mutations = nil
	model.AllowTemporaryActivationForRoleChange = types.BoolValue(true)
	model.Role, model.Name = types.StringValue("guest"), types.StringValue("Opted-in retired user")
	response = userTestUpdate(t, r, model, true)
	got = userTestStateModel(t, response.State)
	if response.Diagnostics.HasError() || !reflect.DeepEqual(mutations, []string{"activate", "update_role", "update", "suspend"}) || got.Role != model.Role || got.Name != model.Name || !got.Suspended.ValueBool() {
		t.Fatalf("opted-in update did not restore suspension: %v state=%+v writes=%v", response.Diagnostics, got, mutations)
	}
	t.Logf("Explicit consent: real HTTP mutations %v; final account suspended", mutations)

	// Intentionally ending active is not temporary activation and needs no opt-in.
	mutations = nil
	model.AllowTemporaryActivationForRoleChange = types.BoolValue(false)
	model.Role, model.Suspended = types.StringValue("member"), types.BoolValue(false)
	response = userTestUpdate(t, r, model, true)
	got = userTestStateModel(t, response.State)
	if response.Diagnostics.HasError() || !reflect.DeepEqual(mutations, []string{"activate", "update_role"}) || got.Suspended.ValueBool() || got.Role != model.Role {
		t.Fatalf("intended reactivation wrongly required consent: %v state=%+v writes=%v", response.Diagnostics, got, mutations)
	}
	if err := api.acceptanceUserSuspended(id, true); err != nil {
		t.Fatal(err)
	}
}
