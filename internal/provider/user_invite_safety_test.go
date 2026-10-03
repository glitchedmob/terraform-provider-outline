// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"reflect"
	"testing"

	"github.com/glitchedmob/terraform-provider-outline/internal/client"
	"github.com/google/uuid"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/oapi-codegen/nullable"
	openapi_types "github.com/oapi-codegen/runtime/types"
)

func TestUserMalformedInvitationSuspensionCleanup(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, invite, read, cleanup, want string
		retained, verify, suspend, cancel bool
		unknown                           bool
	}{
		{name: "missing sent suspends verified new user", retained: true, verify: true, suspend: true},
		{name: "missing unsent", invite: "missing unsent", retained: true, verify: true, suspend: true},
		{name: "wrong sent email", invite: "wrong sent", retained: true, verify: true, suspend: true},
		{name: "invalid envelope needs readback", invite: "envelope", retained: true, verify: true, suspend: true},
		{name: "incomplete user needs readback", invite: "missing status", retained: true, verify: true, suspend: true},
		{name: "cleanup fails saves active status", cleanup: "denied", want: "restoring suspension also failed", retained: true, verify: true, suspend: true},
		{name: "cleanup response mismatches target", cleanup: "wrong ID", want: "different ID", retained: true, verify: true, suspend: true},
		{name: "cleanup confirms active status after suspended read", read: "suspended", cleanup: "active", want: "did not confirm desired suspension", retained: true, verify: true, suspend: true},
		{name: "readback mismatches UUID", read: "wrong ID", want: "cleanup skipped", retained: true, verify: true},
		{name: "readback mismatches email", read: "wrong email", want: "cleanup skipped", retained: true, verify: true},
		{name: "readback is malformed", read: "malformed", want: "cleanup skipped", retained: true, verify: true},
		{name: "readback denied", read: "denied", want: "cleanup skipped", retained: true, verify: true},
		{name: "unknown suspension is not planned true", invite: "missing status", read: "denied", want: "cleanup skipped", retained: true, verify: true, unknown: true},
		{name: "wrong invitation email never cleaned up", invite: "wrong email"},
		{name: "API owner never cleaned up", invite: "owner"},
		{name: "missing UUID never cleaned up", invite: "missing ID"},
		{name: "preexisting UUID never adopted", invite: "existing ID", want: "existing user ID"},
		{name: "unsent race never cleaned up", invite: "unsent"},
		{name: "multiple candidates never cleaned up", invite: "multiple"},
		{name: "canceled invitation still cleans up", retained: true, verify: true, suspend: true, cancel: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			current := userTestUser()
			var calls []string
			r := &userResource{api: userTestAdminClient(t, func(w http.ResponseWriter, req *http.Request) {
				calls = append(calls, req.URL.Path)
				switch req.URL.Path {
				case "/api/users.list":
					userTestListOffset(t, w, req)
					users := []client.User{}
					if tc.invite == "existing ID" {
						existing := userTestUser()
						existing.Email = nullable.NewNullableWithValue(openapi_types.Email("existing@example.com"))
						users = append(users, *existing)
					}
					groupTestEncode(t, w, userTestList(users, 0, 100, len(users)))
				case "/api/users.invite":
					candidate := *current
					body := userTestInviteEnvelope(&candidate)
					data := body["data"].(map[string]any)
					delete(data, "sent")
					switch tc.invite {
					case "missing unsent":
						delete(data, "unsent")
					case "wrong sent":
						data["sent"] = []client.Invite{{Email: "other@example.com"}}
					case "envelope":
						body["ok"] = false
					case "missing status":
						candidate.IsSuspended = nil
					case "wrong email":
						candidate.Email = nullable.NewNullableWithValue(openapi_types.Email("other@example.com"))
					case "owner":
						candidate.Id = userTestOwner().Id
					case "missing ID":
						candidate.Id = nil
					case "unsent":
						data["unsent"] = []client.Invite{{Email: userTestEmail}}
					}
					data["users"] = []client.User{candidate}
					if tc.invite == "multiple" {
						data["users"] = []client.User{candidate, candidate}
					}
					groupTestEncode(t, w, body)
				case "/api/users.info", "/api/users.suspend":
					var body map[string]any
					if !groupTestDecode(t, w, req, &body) || !reflect.DeepEqual(body, map[string]any{"id": userTestID}) {
						t.Error("cleanup did not use the single new candidate UUID")
						return
					}
					if req.URL.Path == "/api/users.info" && tc.read == "suspended" {
						current.IsSuspended = groupTestPointer(true)
					}
					response := *current
					failure := tc.read
					if req.URL.Path == "/api/users.suspend" {
						failure = tc.cleanup
						if failure == "" {
							current.IsSuspended = groupTestPointer(true)
							response = *current
						}
					}
					switch failure {
					case "denied":
						w.WriteHeader(http.StatusForbidden)
						groupTestWrite(t, w, `{"error":"permission_denied"}`)
						return
					case "active":
						current.IsSuspended = groupTestPointer(false)
						response = *current
					case "malformed":
						response.Role = nil
					case "wrong ID":
						response.Id = groupTestPointer(uuid.MustParse(userTestOtherID))
					case "wrong email":
						response.Email = nullable.NewNullableWithValue(openapi_types.Email("other@example.com"))
					}
					groupTestEncode(t, w, userTestEnvelope(&response))
				default:
					t.Errorf("metadata failure caused reconciliation or replay: %s", req.URL.Path)
				}
			})}
			if tc.cancel {
				// Cancel after the invitation reached the client, not during its
				// transport. Cleanup must detach both readback and suspension.
				r.api.httpClient.Transport = userInviteCancelTransport{RoundTripper: r.api.httpClient.Transport, cancel: cancel}
			}
			model := userTestModel()
			model.ID, model.Suspended = types.StringUnknown(), types.BoolValue(true)
			plan := userTestPlan(t, r, model)
			response := resource.CreateResponse{State: tfsdk.State{Schema: plan.Schema}}
			r.Create(ctx, resource.CreateRequest{Plan: plan}, &response)
			groupTestDiagnostics(t, response.Diagnostics, tc.want)
			if tc.retained {
				got := userTestStateModel(t, response.State)
				wantSuspended := tc.suspend && tc.cleanup == ""
				if got.ID.ValueString() != userTestID || got.Email != model.Email || got.Suspended.IsNull() != tc.unknown || got.Suspended.IsUnknown() || got.Suspended.ValueBool() != wantSuspended {
					t.Fatalf("failed invitation lost identity or claimed planned suspension: %+v", got)
				}
				groupTestDiagnostics(t, response.Diagnostics, "retained in state")
			} else if response.State.Raw.IsKnown() && !response.State.Raw.IsNull() {
				t.Fatalf("untrusted or existing identity was adopted: %v", response.State.Raw)
			}
			want := []string{"/api/users.list", "/api/users.invite"}
			if tc.verify {
				want = append(want, "/api/users.info")
			}
			if tc.suspend {
				want = append(want, "/api/users.suspend")
			}
			if !reflect.DeepEqual(calls, want) {
				t.Fatalf("unsafe cleanup or invitation replay: %v, want %v", calls, want)
			}
		})
	}
}

type userInviteCancelTransport struct {
	http.RoundTripper
	cancel context.CancelFunc
}

func (t userInviteCancelTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	response, err := t.RoundTripper.RoundTrip(req)
	if err == nil && req.URL.Path == "/api/users.invite" {
		// Read the response before canceling so the committed invitation's
		// identity survives cancellation of the original request context.
		body, readErr := io.ReadAll(response.Body)
		closeErr := response.Body.Close()
		if readErr != nil {
			return response, readErr
		}
		if closeErr != nil {
			return response, closeErr
		}
		response.Body = io.NopCloser(bytes.NewReader(body))
		t.cancel()
	}
	return response, err
}
