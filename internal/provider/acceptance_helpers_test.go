// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/glitchedmob/terraform-provider-outline/internal/client"
	"github.com/google/uuid"
)

func TestGroupAcceptanceAbsentHelper(t *testing.T) {
	for _, test := range []struct {
		name                string
		infoStatus          int
		infoError           string
		listStatus          int
		presentOnSecondPage bool
		malformedPagination bool
		wantError           string
		wantOffsets         []int
	}{
		{name: "403 requires all pages", infoStatus: 403, infoError: "authorization_error", listStatus: 200, wantOffsets: []int{0, 100}},
		{name: "404 requires all pages", infoStatus: 404, infoError: "not_found", listStatus: 200, wantOffsets: []int{0, 100}},
		{name: "present on second page", infoStatus: 403, infoError: "authorization_error", listStatus: 200, presentOnSecondPage: true, wantError: "still exists", wantOffsets: []int{0, 100}},
		{name: "list forbidden is not absence", infoStatus: 403, infoError: "authorization_error", listStatus: 403, wantError: "groups.list absence check: HTTP 403", wantOffsets: []int{0}},
		{name: "unrecognized forbidden error", infoStatus: 403, infoError: "another_error", wantError: "groups.info returned HTTP 403"},
		{name: "missing pagination", infoStatus: 403, infoError: "authorization_error", listStatus: 200, malformedPagination: true, wantError: "malformed pagination metadata", wantOffsets: []int{0}},
		{name: "successful info means present", infoStatus: 200, wantError: "groups.info returned HTTP 200"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var offsets []int
			api := &acceptanceAPI{apiClient: groupTestClient(t, func(w http.ResponseWriter, req *http.Request) {
				switch req.URL.Path {
				case "/api/groups.info":
					w.WriteHeader(test.infoStatus)
					groupTestEncode(t, w, map[string]any{"ok": false, "status": test.infoStatus, "error": test.infoError})
				case "/api/groups.list":
					var body client.GroupsListJSONRequestBody
					if !groupTestDecode(t, w, req, &body) {
						return
					}
					//nolint:staticcheck // The deprecated name field must also stay unset.
					if body.Query != nil || body.Name != nil || body.UserId != nil || body.ExternalId != nil || body.Source != nil {
						t.Error("absence check must not filter groups.list")
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					if body.Limit == nil || *body.Limit != 100 || body.Offset == nil {
						t.Error("absence check must send a limit and offset")
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					offset := *body.Offset
					offsets = append(offsets, offset)
					w.WriteHeader(test.listStatus)
					if test.listStatus != http.StatusOK {
						groupTestEncode(t, w, map[string]any{"ok": false, "status": test.listStatus, "error": "authorization_error"})
						return
					}
					count := 100
					if offset != 0 {
						count = 1
					}
					groups := make([]client.Group, count)
					for i := range groups {
						groups[i].Id = groupTestPointer(uuid.New())
					}
					if test.presentOnSecondPage && offset == 100 {
						groups[0].Id = groupTestPointer(uuid.MustParse(groupTestID))
					}
					response := map[string]any{
						"ok": true, "status": 200, "data": map[string]any{"groups": groups},
					}
					if !test.malformedPagination {
						response["pagination"] = client.PaginationResponse{
							Limit: groupTestPointer(100), Offset: &offset, Total: groupTestPointer(101),
						}
					}
					groupTestEncode(t, w, response)
				default:
					t.Errorf("unexpected endpoint %s", req.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			})}
			err := api.acceptanceGroupAbsent(groupTestID)
			if test.wantError == "" && err != nil {
				t.Fatal(err)
			}
			if test.wantError != "" && (err == nil || !strings.Contains(err.Error(), test.wantError)) {
				t.Fatalf("expected %q, got %v", test.wantError, err)
			}
			if !reflect.DeepEqual(offsets, test.wantOffsets) {
				t.Fatalf("requested offsets %v, expected %v", offsets, test.wantOffsets)
			}
		})
	}
}
