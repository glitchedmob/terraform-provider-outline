// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/glitchedmob/terraform-provider-outline/internal/client"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
)

func TestGroupDelete404RequiresVerifiedAbsence(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, auth, list string
		infoStatus       int
		ok               bool
	}{
		{"concurrent deletion with info 404", "admin", "absent", 404, true},
		{"concurrent deletion with release 403", "admin", "absent", 403, true},
		{"group still in full list", "admin", "present", 404, false},
		{"auth denied", "denied", "", 404, false},
		{"non admin cannot prove absence", "member", "", 404, false},
		{"malformed auth", "malformed", "", 404, false},
		{"list denied", "admin", "denied", 404, false},
		{"list route missing", "admin", "missing", 404, false},
		{"list malformed", "admin", "malformed", 404, false},
		{"pagination missing", "admin", "unprovable", 404, false},
		{"malformed info", "", "", 200, false},
		{"untyped forbidden info", "", "", 403, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var calls []string
			infos := 0
			r := &groupResource{api: groupTestClient(t, func(w http.ResponseWriter, req *http.Request) {
				calls = append(calls, req.URL.Path)
				switch req.URL.Path {
				case "/api/groups.info":
					infos++
					if infos == 1 {
						groupTestEncode(t, w, groupTestEnvelope(groupTestGroup()))
						return
					}
					w.WriteHeader(tc.infoStatus)
					if tc.infoStatus == 403 && tc.auth != "" {
						groupTestWrite(t, w, `{"error":"authorization_error"}`)
					} else {
						groupTestWrite(t, w, `{}`)
					}
				case "/api/groups.delete":
					w.WriteHeader(http.StatusNotFound)
					groupTestWrite(t, w, `{}`)
				case "/api/auth.info":
					switch tc.auth {
					case "admin":
						groupTestEncode(t, w, userTestAuth(userTestOwner()))
					case "member":
						groupTestEncode(t, w, userTestAuth(userTestUser()))
					case "denied":
						w.WriteHeader(http.StatusUnauthorized)
						groupTestWrite(t, w, `{}`)
					default:
						groupTestWrite(t, w, `{}`)
					}
				case "/api/groups.list":
					if _, ok := groupTestListOffset(t, w, req); !ok {
						return
					}
					switch tc.list {
					case "absent":
						groupTestEncode(t, w, groupTestList([]client.Group{}, 0, 100, 0))
					case "present":
						groupTestEncode(t, w, groupTestList([]client.Group{*groupTestGroup()}, 0, 100, 1))
					case "denied":
						w.WriteHeader(http.StatusForbidden)
						groupTestWrite(t, w, `{}`)
					case "missing":
						w.WriteHeader(http.StatusNotFound)
						groupTestWrite(t, w, `{}`)
					case "unprovable":
						groupTestWrite(t, w, `{"ok":true,"data":{"groups":[]}}`)
					default:
						groupTestWrite(t, w, `{}`)
					}
				default:
					t.Errorf("unexpected request: %s", req.URL.Path)
				}
			})}
			state := tfsdk.State(groupTestPlan(t, r, groupTestModel()))
			response := resource.DeleteResponse{State: state}
			r.Delete(t.Context(), resource.DeleteRequest{State: state}, &response)
			if response.Diagnostics.HasError() == tc.ok || !response.State.Raw.Equal(state.Raw) {
				t.Fatalf("delete must retain state unless absence is verified: %v", response.Diagnostics)
			}
			if !tc.ok {
				groupTestDiagnostics(t, response.Diagnostics, "groups.delete: HTTP 404")
			}
			want := []string{"/api/groups.info", "/api/groups.delete", "/api/groups.info"}
			if tc.auth != "" {
				want = append(want, "/api/auth.info")
				if tc.auth == "admin" {
					want = append(want, "/api/groups.list")
				}
			}
			if !reflect.DeepEqual(calls, want) {
				t.Fatalf("delete repeated or absence not independently verified: %v, want %v", calls, want)
			}
		})
	}
}

// Inject failures at the generated client's HTTP boundary without bypassing its
// decoder. Even a 404 response cannot override request or body-decoding errors.
type groupDeleteFailureClient struct {
	client.ClientInterface
	response *http.Response
	err      error
}

func (c groupDeleteFailureClient) GroupsDelete(context.Context, client.GroupsDeleteJSONRequestBody, ...client.RequestEditorFn) (*http.Response, error) {
	return c.response, c.err
}

func TestGroupDeleteRequestAndDecodeErrorsRetainState(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, body string
		status     int
		requestErr error
	}{
		{"nil response with transport error", "", 0, errors.New("transport " + groupTestKey)},
		{"404 with request error", `{}`, 404, errors.New("request " + groupTestKey)},
		{"404 with malformed JSON", `{"message":"` + groupTestKey, 404, nil},
		{"404 with wrong error type", `{"error":42}`, 404, nil},
		{"200 with malformed JSON", `{`, 200, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			infos := 0
			r := &groupResource{api: groupTestClient(t, func(w http.ResponseWriter, req *http.Request) {
				if req.URL.Path != "/api/groups.info" {
					t.Errorf("failure must not verify or list: %s", req.URL.Path)
				}
				infos++
				groupTestEncode(t, w, groupTestEnvelope(groupTestGroup()))
			})}
			var httpResponse *http.Response
			if tc.status != 0 {
				httpResponse = &http.Response{StatusCode: tc.status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(tc.body))}
				t.Cleanup(func() { _ = httpResponse.Body.Close() })
			}
			r.api.ClientInterface = groupDeleteFailureClient{r.api.ClientInterface, httpResponse, tc.requestErr}
			state := tfsdk.State(groupTestPlan(t, r, groupTestModel()))
			response := resource.DeleteResponse{State: state}
			r.Delete(t.Context(), resource.DeleteRequest{State: state}, &response)
			groupTestDiagnostics(t, response.Diagnostics, "request failed or response could not be decoded")
			if !response.State.Raw.Equal(state.Raw) || infos != 1 {
				t.Fatalf("request/decode failure authorized absence or verification: %v, infos=%d", response.Diagnostics, infos)
			}
		})
	}
}
