// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"fmt"
	"net/http"
	"reflect"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
)

func TestCollectionCreateAndUpdateHTTPFailuresNeverReplay(t *testing.T) {
	t.Parallel()
	for _, operation := range []string{"create", "update"} {
		for _, status := range []int{400, 401, 403, 404, 429, 500} {
			t.Run(fmt.Sprintf("%s/%d", operation, status), func(t *testing.T) {
				t.Parallel()
				var calls []string
				r := &collectionResource{api: collectionTestAdminClient(t, func(w http.ResponseWriter, req *http.Request) {
					calls = append(calls, req.URL.Path)
					if req.URL.Path == "/api/collections.info" {
						groupTestEncode(t, w, collectionTestEnvelope(collectionTestCollection()))
						return
					}
					w.Header().Set("Retry-After", "0")
					w.WriteHeader(status)
					groupTestWrite(t, w, `{"ok":false,"error":"authorization_error","message":"Bearer `+groupTestKey+`"}`)
				})}
				model := collectionTestModel()
				diagnostics, state := collectionTestOperation(t, r, operation, model)
				groupTestDiagnostics(t, diagnostics, "collections."+operation)
				want := []string{"/api/collections.create"}
				if operation == "create" {
					collectionTestAssertNoState(t, state)
				} else {
					want = []string{"/api/collections.info", "/api/collections.update"}
					if !state.Raw.Equal(tfsdk.State(collectionTestPlan(t, r, model)).Raw) {
						t.Fatal("failed write removed or changed prior state")
					}
				}
				if !reflect.DeepEqual(calls, want) {
					t.Fatalf("write was replayed or error triggered a list: %v want %v", calls, want)
				}
			})
		}
	}
}
