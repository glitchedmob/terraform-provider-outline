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

func cgUnitJSONMap(t *testing.T, value any) map[string]any {
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

func TestCollectionGroupPaginationCompleteEarlyLateAndAbsent(t *testing.T) {
	for _, target := range []int{-1, 0, 200} {
		for _, operation := range []string{"read", "import", "create", "update", "delete"} {
			t.Run(fmt.Sprintf("%s/target=%d", operation, target), func(t *testing.T) {
				members := cgUnitGrants(201)
				if target != -1 {
					members[target] = cgUnitGrant(cgUnitGroupID, client.PermissionAdmin)
				}
				var offsets []int
				writes := 0
				r := &collectionGroupResource{api: cgUnitClient(t, func(w http.ResponseWriter, req *http.Request) {
					if cgUnitParents(t, w, req) {
						return
					}
					switch req.URL.Path {
					case "/api/collections.group_memberships":
						offset := cgUnitOffset(t, w, req, grantTestReadQuery(operation, cgUnitGroup(cgUnitGroupID).Name))
						offsets = append(offsets, offset)
						if offset != 0 && offset != 100 && offset != 200 {
							t.Errorf("unexpected offset %d", offset)
							w.WriteHeader(http.StatusBadRequest)
							return
						}
						end := min(offset+100, len(members))
						groupTestEncode(t, w, cgUnitEnvelope(members[offset:end], offset, len(members), false))
					case "/api/collections.add_group":
						writes++
						cgUnitBody(t, w, req, map[string]any{"id": cgUnitCollectionID, "groupId": cgUnitGroupID, "permission": "admin"})
						groupTestEncode(t, w, cgUnitEnvelope([]client.GroupMembership{cgUnitGrant(cgUnitGroupID, client.PermissionAdmin)}, 0, 0, true))
					case "/api/collections.remove_group":
						writes++
						cgUnitBody(t, w, req, map[string]any{"id": cgUnitCollectionID, "groupId": cgUnitGroupID})
						members = append(members[:target], members[target+1:]...)
						groupTestEncode(t, w, map[string]any{"ok": true, "status": 200, "success": true})
					default:
						t.Errorf("unexpected operation: %s", req.URL.Path)
					}
				})}
				desired := cgUnitModel()
				desired.Permission = types.StringValue("admin")
				diagnostics, state := cgUnitOperation(t, r, operation, cgUnitModel(), desired)
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
						if diagnostics.HasError() || cgUnitStateModel(t, state) != desired {
							t.Fatalf("absent create: %v", diagnostics)
						}
					}
				case "import", "update":
					if target == -1 {
						cgUnitAssertFailureState(t, operation, diagnostics, state, cgUnitModel())
					} else if diagnostics.HasError() || cgUnitStateModel(t, state) != desired {
						t.Fatalf("late pair: %v", diagnostics)
					}
				case "read":
					if target == -1 {
						// A broad response can ignore query. Its filtered miss still
						// needs a complete unfiltered absence check.
						wantOffsets = append(wantOffsets, 0, 100, 200)
					}
					if diagnostics.HasError() || target == -1 && !state.Raw.IsNull() || target != -1 && cgUnitStateModel(t, state) != desired {
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

func TestCollectionGroupLaterMalformedPagesRetainState(t *testing.T) {
	failures := []string{
		"missing JSON", "invalid JSON", "missing ok", "false ok", "missing status", "wrong status", "missing data", "null data",
		"missing grants", "null grants", "missing groups", "null groups", "missing pagination", "missing total", "wrong offset", "invalid limit", "negative total", "short page", "changed total",
		"duplicate group across pages", "duplicate grant across pages", "duplicate group on page", "duplicate grant on page", "duplicate groups presenter", "inconsistent groups", "different groups count",
		"missing grant ID", "invalid grant UUID", "uppercase grant UUID", "zero grant UUID", "missing group ID", "zero group UUID", "invalid group UUID", "wrong collection", "missing collection", "null collection",
		"missing document", "document grant", "missing source", "inherited grant", "missing permission", "invalid permission", "null permission",
		"missing group name", "missing disable mentions", "missing presented group ID", "invalid presented group ID", "403", "404", "429", "500",
	}
	for _, failure := range failures {
		for _, operation := range []string{"create", "read", "update", "delete", "import"} {
			t.Run(operation+"/"+failure, func(t *testing.T) {
				first := cgUnitGrants(100)
				// Create must finish its absence check. All other operations must
				// validate the later page even after finding the requested pair.
				if operation != "create" {
					first[0] = cgUnitGrant(cgUnitGroupID, client.PermissionRead)
				}
				firstTotal := 101
				if failure == "duplicate group on page" || failure == "duplicate grant on page" || failure == "duplicate groups presenter" {
					firstTotal = 102
				}
				calls := 0
				r := &collectionGroupResource{api: cgUnitClient(t, func(w http.ResponseWriter, req *http.Request) {
					if cgUnitParents(t, w, req) {
						return
					}
					if req.URL.Path != "/api/collections.group_memberships" {
						t.Errorf("unsafe page caused mutation: %s", req.URL.Path)
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					offset := cgUnitOffset(t, w, req, grantTestReadQuery(operation, cgUnitGroup(cgUnitGroupID).Name))
					calls++
					if calls == 1 {
						if offset != 0 {
							t.Errorf("first offset %d", offset)
						}
						groupTestEncode(t, w, cgUnitEnvelope(first, 0, firstTotal, false))
						return
					}
					if calls != 2 || offset != 100 {
						t.Errorf("later page was skipped or retried: calls=%d offset=%d", calls, offset)
					}
					member := cgUnitGrants(1)[0]
					page := cgUnitJSONMap(t, cgUnitEnvelope([]client.GroupMembership{member}, 100, 101, false))
					data := page["data"].(map[string]any)
					grant := data["groupMemberships"].([]any)[0].(map[string]any)
					group := data["groups"].([]any)[0].(map[string]any)
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
						delete(data, "groupMemberships")
					case "null grants":
						data["groupMemberships"] = nil
					case "missing groups":
						delete(data, "groups")
					case "null groups":
						data["groups"] = nil
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
						page = cgUnitJSONMap(t, cgUnitEnvelope(cgUnitGrants(2), 100, 102, false))
					case "duplicate group across pages":
						grant["groupId"] = first[0].GroupId.String()
						group["id"] = first[0].GroupId.String()
					case "duplicate grant across pages":
						grant["id"] = *first[0].Id
					case "duplicate group on page", "duplicate grant on page", "duplicate groups presenter":
						page = cgUnitJSONMap(t, cgUnitEnvelope(cgUnitGrants(2), 100, 102, false))
						d := page["data"].(map[string]any)
						gs := d["groups"].([]any)
						ms := d["groupMemberships"].([]any)
						switch failure {
						case "duplicate grant on page":
							ms[1].(map[string]any)["id"] = ms[0].(map[string]any)["id"]
						case "duplicate group on page":
							ms[1].(map[string]any)["groupId"] = ms[0].(map[string]any)["groupId"]
						case "duplicate groups presenter":
							gs[1] = gs[0]
						}
					case "inconsistent groups":
						group["id"] = uuid.NewString()
					case "different groups count":
						data["groups"] = []client.Group{}
					case "missing grant ID":
						delete(grant, "id")
					case "invalid grant UUID":
						grant["id"] = "not-a-uuid"
					case "uppercase grant UUID":
						grant["id"] = strings.ToUpper(cgUnitGrantID)
					case "zero grant UUID":
						grant["id"] = uuid.Nil.String()
					case "missing group ID":
						delete(grant, "groupId")
					case "zero group UUID":
						grant["groupId"] = uuid.Nil.String()
					case "invalid group UUID":
						grant["groupId"] = "a32c2ee6-fbde-0654-841b-0eabdc71b812"
					case "wrong collection":
						grant["collectionId"] = cgUnitGroupID
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
					case "missing group name":
						delete(group, "name")
					case "missing disable mentions":
						delete(group, "disableMentions")
					case "missing presented group ID":
						delete(group, "id")
					case "invalid presented group ID":
						group["id"] = "a32c2ee6-fbde-0654-841b-0eabdc71b812"
					case "403", "404", "429", "500":
						status := map[string]int{"403": 403, "404": 404, "429": 429, "500": 500}[failure]
						w.WriteHeader(status)
						page = map[string]any{"ok": false, "status": status, "error": "not_found", "message": groupTestKey}
					}
					groupTestEncode(t, w, page)
				})}
				desired := cgUnitModel()
				desired.Permission = types.StringValue("admin")
				diagnostics, state := cgUnitOperation(t, r, operation, cgUnitModel(), desired)
				cgUnitAssertFailureState(t, operation, diagnostics, state, cgUnitModel())
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
