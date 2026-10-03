// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/glitchedmob/terraform-provider-outline/internal/client"
	"github.com/google/uuid"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

func cuUnitJSONMap(t *testing.T, value any) map[string]any {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestCollectionUserPaginationCompleteEarlyLateAndAbsent(t *testing.T) {
	for _, target := range []int{-1, 0, 200} {
		for _, operation := range []string{"read", "import", "create", "update", "delete"} {
			t.Run(fmt.Sprintf("%s/target=%d", operation, target), func(t *testing.T) {
				members := cuUnitGrants(201)
				// Creator-admin grants are legitimate unrelated rows, not a
				// reason to stop before checking the remaining pages.
				members[1] = cuUnitGrant(userTestOwnerID, client.PermissionAdmin)
				if target != -1 {
					member := cuUnitGrant(cuUnitUserID, client.PermissionAdmin)
					member.Id = groupTestPointer(uuid.NewString())
					members[target] = member
				}
				var offsets []int
				writes := 0
				r := &collectionUserResource{api: cuUnitClient(t, func(w http.ResponseWriter, req *http.Request) {
					if cuUnitParents(t, w, req) {
						return
					}
					switch req.URL.Path {
					case "/api/collections.memberships":
						offset := cuUnitOffset(t, w, req, grantTestReadQuery(operation, cuUnitParentUser(cuUnitUserID).Name))
						offsets = append(offsets, offset)
						if offset != 0 && offset != 100 && offset != 200 {
							t.Errorf("unexpected offset %d", offset)
							w.WriteHeader(http.StatusBadRequest)
							return
						}
						end := min(offset+100, len(members))
						groupTestEncode(t, w, cuUnitEnvelope(members[offset:end], offset, len(members), false))
					case "/api/collections.add_user":
						writes++
						cuUnitBody(t, w, req, map[string]any{"id": cuUnitCollectionID, "userId": cuUnitUserID, "permission": "admin"})
						groupTestEncode(t, w, cuUnitEnvelope([]client.Membership{cuUnitGrant(cuUnitUserID, client.PermissionAdmin)}, 0, 0, true))
					case "/api/collections.remove_user":
						writes++
						cuUnitBody(t, w, req, map[string]any{"id": cuUnitCollectionID, "userId": cuUnitUserID})
						members = append(members[:target], members[target+1:]...)
						groupTestEncode(t, w, map[string]any{"ok": true, "status": 200, "success": true})
					default:
						t.Errorf("unexpected operation: %s", req.URL.Path)
					}
				})}
				desired := cuUnitModel()
				desired.Permission = types.StringValue("admin")
				diagnostics, state := cuUnitOperation(t, r, operation, cuUnitModel(), desired)
				wantOffsets := []int{0, 100, 200}
				wantWrites := 0
				switch operation {
				case "create":
					if target != -1 {
						groupTestDiagnostics(t, diagnostics, "Import the existing pair")
						if !state.Raw.IsNull() {
							t.Fatal("preexisting late pair was adopted")
						}
					} else {
						wantWrites = 1
						if diagnostics.HasError() || cuUnitStateModel(t, state) != desired {
							t.Fatalf("absent create: %v", diagnostics)
						}
					}
				case "import", "update":
					if target == -1 {
						cuUnitAssertFailureState(t, operation, diagnostics, state, cuUnitModel())
					} else if diagnostics.HasError() || cuUnitStateModel(t, state) != desired {
						t.Fatalf("late pair: %v", diagnostics)
					}
				case "read":
					if target == -1 {
						// A broad response can ignore query. Its filtered miss still
						// needs a complete unfiltered absence check.
						wantOffsets = append(wantOffsets, 0, 100, 200)
					}
					if diagnostics.HasError() || target == -1 && !state.Raw.IsNull() || target != -1 && cuUnitStateModel(t, state) != desired {
						t.Fatalf("refresh: %v", diagnostics)
					}
				case "delete":
					if diagnostics.HasError() {
						t.Fatal(diagnostics)
					}
					if target != -1 {
						wantWrites = 1
						wantOffsets = append(wantOffsets, 0, 100)
					}
				}
				if writes != wantWrites || !reflect.DeepEqual(offsets, wantOffsets) {
					t.Fatalf("incomplete pagination or mutation: offsets=%v writes=%d, want %v/%d", offsets, writes, wantOffsets, wantWrites)
				}
			})
		}
	}
}

func TestCollectionUserLaterMalformedPagesRetainState(t *testing.T) {
	failures := []string{
		"missing JSON", "invalid JSON", "missing ok", "false ok", "missing status", "wrong status", "missing data", "null data",
		"missing grants", "null grants", "missing users", "null users", "missing pagination", "missing total", "wrong offset", "invalid limit", "negative total", "short page", "changed total",
		"duplicate user across pages", "duplicate grant across pages", "duplicate user on page", "duplicate grant on page", "duplicate users presenter", "inconsistent users", "different users count",
		"missing grant ID", "invalid grant UUID", "uppercase grant UUID", "zero grant UUID", "missing user ID", "zero user UUID", "invalid user UUID", "wrong collection", "missing collection", "null collection",
		"missing document", "document grant", "missing source", "inherited grant", "missing permission", "invalid permission", "null permission",
		"missing user name", "null user name", "missing user role", "null user role", "invalid user role", "missing suspension", "null suspension", "invalid suspension",
		"missing presented user ID", "null presented user ID", "invalid presented user ID", "zero presented user ID", "400", "403", "404", "429", "500",
	}
	for _, failure := range failures {
		for _, operation := range []string{"create", "read", "update", "delete", "import"} {
			t.Run(operation+"/"+failure, func(t *testing.T) {
				first := cuUnitGrants(100)
				first[1] = cuUnitGrant(userTestOwnerID, client.PermissionAdmin)
				first[1].Id = groupTestPointer(uuid.NewString())
				// Create must finish its absence check. All other operations must
				// validate the later page even after finding the requested pair.
				if operation != "create" {
					first[0] = cuUnitGrant(cuUnitUserID, client.PermissionRead)
				}
				firstTotal := 101
				if failure == "duplicate user on page" || failure == "duplicate grant on page" || failure == "duplicate users presenter" {
					firstTotal = 102
				}
				calls := 0
				r := &collectionUserResource{api: cuUnitClient(t, func(w http.ResponseWriter, req *http.Request) {
					if cuUnitParents(t, w, req) {
						return
					}
					if req.URL.Path != "/api/collections.memberships" {
						t.Errorf("unsafe page caused mutation: %s", req.URL.Path)
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					offset := cuUnitOffset(t, w, req, grantTestReadQuery(operation, cuUnitParentUser(cuUnitUserID).Name))
					calls++
					if calls == 1 {
						if offset != 0 {
							t.Errorf("first offset %d", offset)
						}
						groupTestEncode(t, w, cuUnitEnvelope(first, 0, firstTotal, false))
						return
					}
					if calls != 2 || offset != 100 {
						t.Errorf("later page was skipped or retried: calls=%d offset=%d", calls, offset)
					}
					member := cuUnitGrants(1)[0]
					page := cuUnitJSONMap(t, cuUnitEnvelope([]client.Membership{member}, 100, 101, false))
					data := page["data"].(map[string]any)
					grant := data["memberships"].([]any)[0].(map[string]any)
					user := data["users"].([]any)[0].(map[string]any)
					pagination := page["pagination"].(map[string]any)
					switch failure {
					case "missing JSON":
						w.Header().Set("Content-Type", "text/plain")
					case "invalid JSON":
						groupTestWrite(t, w, "{bad")
						return
					case "missing ok":
						delete(page, "ok")
					case "false ok":
						page["ok"] = false
					case "missing status":
						delete(page, "status")
					case "wrong status":
						page["status"] = 201
					case "missing data":
						delete(page, "data")
					case "null data":
						page["data"] = nil
					case "missing grants":
						delete(data, "memberships")
					case "null grants":
						data["memberships"] = nil
					case "missing users":
						delete(data, "users")
					case "null users":
						data["users"] = nil
					case "missing pagination":
						delete(page, "pagination")
					case "missing total":
						delete(pagination, "total")
					case "wrong offset":
						pagination["offset"] = 0
					case "invalid limit":
						pagination["limit"] = 101
					case "negative total":
						pagination["total"] = -1
					case "short page":
						pagination["total"] = 103
					case "changed total":
						page = cuUnitJSONMap(t, cuUnitEnvelope(cuUnitGrants(2), 100, 102, false))
					case "duplicate user across pages":
						grant["userId"] = first[0].UserId.String()
						user["id"] = first[0].UserId.String()
					case "duplicate grant across pages":
						grant["id"] = *first[0].Id
					case "duplicate user on page", "duplicate grant on page", "duplicate users presenter":
						page = cuUnitJSONMap(t, cuUnitEnvelope(cuUnitGrants(2), 100, 102, false))
						d := page["data"].(map[string]any)
						gs := d["users"].([]any)
						ms := d["memberships"].([]any)
						switch failure {
						case "duplicate grant on page":
							ms[1].(map[string]any)["id"] = ms[0].(map[string]any)["id"]
						case "duplicate user on page":
							ms[1].(map[string]any)["userId"] = ms[0].(map[string]any)["userId"]
						case "duplicate users presenter":
							gs[1] = gs[0]
						}
					case "inconsistent users":
						user["id"] = uuid.NewString()
					case "different users count":
						data["users"] = []client.User{}
					case "missing grant ID":
						delete(grant, "id")
					case "invalid grant UUID":
						grant["id"] = "not-a-uuid"
					case "uppercase grant UUID":
						grant["id"] = strings.ToUpper(cuUnitGrantID)
					case "zero grant UUID":
						grant["id"] = uuid.Nil.String()
					case "missing user ID":
						delete(grant, "userId")
					case "zero user UUID":
						grant["userId"] = uuid.Nil.String()
					case "invalid user UUID":
						grant["userId"] = "a32c2ee6-fbde-0654-841b-0eabdc71b812"
					case "wrong collection":
						grant["collectionId"] = cuUnitUserID
					case "missing collection":
						delete(grant, "collectionId")
					case "null collection":
						grant["collectionId"] = nil
					case "missing document":
						delete(grant, "documentId")
					case "document grant":
						grant["documentId"] = uuid.NewString()
					case "missing source":
						delete(grant, "sourceId")
					case "inherited grant":
						grant["sourceId"] = uuid.NewString()
					case "missing permission":
						delete(grant, "permission")
					case "invalid permission":
						grant["permission"] = "owner"
					case "null permission":
						grant["permission"] = nil
					case "missing user name":
						delete(user, "name")
					case "null user name":
						user["name"] = nil
					case "missing user role":
						delete(user, "role")
					case "null user role":
						user["role"] = nil
					case "invalid user role":
						user["role"] = "owner"
					case "missing suspension":
						delete(user, "isSuspended")
					case "null suspension":
						user["isSuspended"] = nil
					case "invalid suspension":
						user["isSuspended"] = "false"
					case "missing presented user ID":
						delete(user, "id")
					case "null presented user ID":
						user["id"] = nil
					case "invalid presented user ID":
						user["id"] = "a32c2ee6-fbde-0654-841b-0eabdc71b812"
					case "zero presented user ID":
						user["id"] = uuid.Nil.String()
					case "400", "403", "404", "429", "500":
						status := map[string]int{"400": 400, "403": 403, "404": 404, "429": 429, "500": 500}[failure]
						w.WriteHeader(status)
						page = map[string]any{"ok": false, "status": status, "error": "not_found", "message": groupTestKey}
					}
					groupTestEncode(t, w, page)
				})}
				desired := cuUnitModel()
				desired.Permission = types.StringValue("admin")
				diagnostics, state := cuUnitOperation(t, r, operation, cuUnitModel(), desired)
				cuUnitAssertFailureState(t, operation, diagnostics, state, cuUnitModel())
				if strings.HasPrefix(failure, "duplicate") {
					groupTestDiagnostics(t, diagnostics, "duplicate")
				}
				if failure == "changed total" {
					groupTestDiagnostics(t, diagnostics, "total changed during pagination")
				}
				if calls != 2 || strings.Contains(fmt.Sprint(diagnostics), groupTestKey) {
					t.Fatalf("partial page set accepted or secret leaked: calls=%d diagnostics=%v", calls, diagnostics)
				}
			})
		}
	}
}
