// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/glitchedmob/terraform-provider-outline/internal/client"
	"github.com/google/uuid"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

func TestCollectionGroupBadUpsertResponseRetainsTrustedPair(t *testing.T) {
	failures := []string{"missing JSON", "invalid JSON", "missing ok", "false ok", "null ok", "missing status", "wrong status", "null status", "missing data", "null data", "missing grants", "null grants", "empty grants", "multiple grants",
		"missing grant ID", "composite grant ID", "uppercase grant ID", "zero grant ID", "invalid grant ID", "missing group ID", "zero group ID", "wrong group", "missing collection", "null collection", "wrong collection",
		"missing document", "document grant", "missing source", "inherited grant", "missing permission", "null permission", "unsafe permission", "wrong permission", "closed connection", "truncated JSON", "empty HTTP200", "400", "403", "404", "429", "500"}
	for _, operation := range []string{"create", "update"} {
		for _, failure := range failures {
			t.Run(operation+"/"+failure, func(t *testing.T) {
				var mu sync.Mutex
				writes, lists := 0, 0
				members := []client.GroupMembership{}
				if operation == "update" {
					members = append(members, cgUnitGrant(cgUnitGroupID, client.PermissionRead))
				}
				r := &collectionGroupResource{api: cgUnitClient(t, func(w http.ResponseWriter, req *http.Request) {
					mu.Lock()
					defer mu.Unlock()
					if cgUnitParents(t, w, req) {
						return
					}
					if req.URL.Path == "/api/collections.group_memberships" {
						lists++
						cgUnitOffset(t, w, req)
						groupTestEncode(t, w, cgUnitEnvelope(members, 0, len(members), false))
						return
					}
					if req.URL.Path != "/api/collections.add_group" {
						t.Errorf("unexpected write: %s", req.URL.Path)
						return
					}
					writes++
					cgUnitBody(t, w, req, map[string]any{"id": cgUnitCollectionID, "groupId": cgUnitGroupID, "permission": "admin"})
					// Simulate a committed upsert followed by an untrusted response.
					members = []client.GroupMembership{cgUnitGrant(cgUnitGroupID, client.PermissionAdmin)}
					page := cgUnitJSONMap(t, cgUnitEnvelope(members, 0, 0, true))
					data := page["data"].(map[string]any)
					grant := data["groupMemberships"].([]any)[0].(map[string]any)
					switch failure {
					case "missing JSON":
						w.Header().Set("Content-Type", "text/plain")
					case "closed connection":
						conn, _, err := w.(http.Hijacker).Hijack()
						if err != nil {
							t.Errorf("hijack mutation response: %v", err)
							return
						}
						_ = conn.Close()
						return
					case "truncated JSON":
						groupTestWrite(t, w, `{"ok":true,"status":200,"data":`)
						return
					case "empty HTTP200":
						w.WriteHeader(http.StatusOK)
						return
					case "invalid JSON":
						groupTestWrite(t, w, "{bad")
						return
					case "missing ok":
						delete(page, "ok")
					case "false ok":
						page["ok"] = false
					case "null ok":
						page["ok"] = nil
					case "missing status":
						delete(page, "status")
					case "wrong status":
						page["status"] = 201
					case "null status":
						page["status"] = nil
					case "missing data":
						delete(page, "data")
					case "null data":
						page["data"] = nil
					case "missing grants":
						delete(data, "groupMemberships")
					case "null grants":
						data["groupMemberships"] = nil
					case "empty grants":
						data["groupMemberships"] = []any{}
					case "multiple grants":
						data["groupMemberships"] = []any{grant, grant}
					case "missing grant ID":
						delete(grant, "id")
					case "composite grant ID":
						grant["id"] = cgUnitPairID
					case "uppercase grant ID":
						grant["id"] = strings.ToUpper(cgUnitGrantID)
					case "zero grant ID":
						grant["id"] = "00000000-0000-0000-0000-000000000000"
					case "invalid grant ID":
						grant["id"] = "not-a-uuid"
					case "missing group ID":
						delete(grant, "groupId")
					case "zero group ID":
						grant["groupId"] = "00000000-0000-0000-0000-000000000000"
					case "wrong group":
						grant["groupId"] = cgUnitOtherGroupID
					case "missing collection":
						delete(grant, "collectionId")
					case "null collection":
						grant["collectionId"] = nil
					case "wrong collection":
						grant["collectionId"] = cgUnitGroupID
					case "missing document":
						delete(grant, "documentId")
					case "document grant":
						grant["documentId"] = cgUnitGroupID
					case "missing source":
						delete(grant, "sourceId")
					case "inherited grant":
						grant["sourceId"] = cgUnitGrantID
					case "missing permission":
						delete(grant, "permission")
					case "null permission":
						grant["permission"] = nil
					case "unsafe permission":
						grant["permission"] = "owner"
					case "wrong permission":
						grant["permission"] = "read_write"
					case "400", "403", "404", "429", "500":
						status := map[string]int{"400": 400, "403": 403, "404": 404, "429": 429, "500": 500}[failure]
						w.WriteHeader(status)
						page = map[string]any{"ok": false, "status": status, "error": "not_found", "message": groupTestKey}
					}
					groupTestEncode(t, w, page)
				})}
				desired := cgUnitModel()
				desired.Permission = types.StringValue("admin")
				diagnostics, state := cgUnitOperation(t, r, operation, cgUnitModel(), desired)
				mu.Lock()
				gotWrites, gotLists := writes, lists
				mu.Unlock()
				if !diagnostics.HasError() || gotWrites != 1 || gotLists != 1 {
					t.Fatalf("untrusted mutation accepted, retried, or reconciled silently: writes=%d lists=%d diagnostics=%v", gotWrites, gotLists, diagnostics)
				}
				if operation == "create" {
					groupTestDiagnostics(t, diagnostics, "pair ID has been retained")
					groupTestDiagnostics(t, diagnostics, "terraform untaint")
					if cgUnitStateModel(t, state) != desired {
						t.Fatal("failed create discarded the trusted pair or adopted the response identity")
					}
				} else if cgUnitStateModel(t, state) != cgUnitModel() {
					t.Fatal("failed update changed the saved pair or permission")
				}
				if strings.Contains(fmt.Sprint(diagnostics), groupTestKey) {
					t.Fatal("diagnostics leaked API credentials")
				}
				// Refresh recovers the committed permission without another write.
				diagnostics, state = cgUnitOperation(t, r, "read", cgUnitStateModel(t, state), desired)
				mu.Lock()
				gotWrites = writes
				mu.Unlock()
				if diagnostics.HasError() || cgUnitStateModel(t, state) != desired || gotWrites != 1 {
					t.Fatalf("refresh failed to recover committed pair: %v writes=%d", diagnostics, gotWrites)
				}
			})
		}
	}
}

func TestCollectionGroupUpsertRejectsUnsafePermissionBeforeRequest(t *testing.T) {
	api := cgUnitClient(t, func(http.ResponseWriter, *http.Request) { t.Error("unsafe permission reached Outline") })
	for _, permission := range []client.Permission{"", "member", "owner", "READ", "read-write"} {
		t.Run(string(permission), func(t *testing.T) {
			member, err := api.writeCollectionGroup(t.Context(), uuid.MustParse(cgUnitCollectionID), uuid.MustParse(cgUnitGroupID), permission)
			if err == nil || member != nil || !strings.Contains(err.Error(), "read, read_write, or admin") {
				t.Fatalf("unsafe permission %q was not refused: member=%v err=%v", permission, member, err)
			}
		})
	}
}

func cgUnitAbsentRemove() map[string]any {
	return map[string]any{"ok": false, "status": 400, "error": "invalid_request", "message": "This Group is not a part of the collection"}
}

func TestCollectionGroupRemoveExactAbsenceNeedsCompleteConfirmation(t *testing.T) {
	for _, response := range []string{"success", "exact absent 400"} {
		for _, evidence := range []string{"absent", "still present", "later malformed", "list 403", "list 404"} {
			t.Run(response+"/"+evidence, func(t *testing.T) {
				members := cgUnitGrants(101)
				members[0] = cgUnitGrant(cgUnitGroupID, client.PermissionRead)
				removed := false
				writes := 0
				var offsets []int
				r := &collectionGroupResource{api: cgUnitClient(t, func(w http.ResponseWriter, req *http.Request) {
					if cgUnitParents(t, w, req) {
						return
					}
					switch req.URL.Path {
					case "/api/collections.group_memberships":
						offset := cgUnitOffset(t, w, req)
						offsets = append(offsets, offset)
						if offset != 0 && offset != 100 {
							t.Errorf("unexpected page offset: %d", offset)
							return
						}
						if removed && (evidence == "list 403" || evidence == "list 404") {
							status := 403
							if evidence == "list 404" {
								status = 404
							}
							w.WriteHeader(status)
							groupTestEncode(t, w, map[string]any{"ok": false, "status": status, "error": "not_found"})
							return
						}
						page := cgUnitEnvelope(members[offset:min(offset+100, len(members))], offset, len(members), false)
						if removed && evidence == "later malformed" && offset == 100 {
							delete(page, "pagination")
						}
						groupTestEncode(t, w, page)
					case "/api/collections.remove_group":
						writes++
						removed = true
						cgUnitBody(t, w, req, map[string]any{"id": cgUnitCollectionID, "groupId": cgUnitGroupID})
						if evidence != "still present" {
							// Keep >100 unrelated grants after deleting the pair.
							members[0] = cgUnitGrants(1)[0]
						}
						if response == "exact absent 400" {
							w.WriteHeader(http.StatusBadRequest)
							groupTestEncode(t, w, cgUnitAbsentRemove())
						} else {
							groupTestEncode(t, w, map[string]any{"ok": true, "status": 200, "success": true})
						}
					default:
						t.Errorf("unexpected endpoint: %s", req.URL.Path)
					}
				})}
				diagnostics, state := cgUnitOperation(t, r, "delete", cgUnitModel(), cgUnitModel())
				bad := evidence != "absent"
				if diagnostics.HasError() != bad || writes != 1 || len(offsets) < 3 || offsets[0] != 0 || offsets[1] != 100 || offsets[2] != 0 {
					t.Fatalf("remove must validate a second full list: %v writes=%d offsets=%v", diagnostics, writes, offsets)
				}
				if (evidence == "absent" || evidence == "still present" || evidence == "later malformed") && (len(offsets) != 4 || offsets[3] != 100) {
					t.Fatalf("confirmation stopped before the later page: %v", offsets)
				}
				if bad && cgUnitStateModel(t, state) != cgUnitModel() {
					t.Fatal("unconfirmed remove lost the pair")
				}
			})
		}
	}
}

func TestCollectionGroupRemoveAmbiguousErrorsAndBadSuccessRetainState(t *testing.T) {
	for _, failure := range []string{"missing ok", "null ok", "true ok", "missing status", "null status", "wrong status", "missing error", "wrong error", "missing message", "wrong message", "case changed", "extra whitespace", "last manager", "403 absent body", "404 absent body", "invalid JSON", "not JSON",
		"success missing ok", "success false ok", "success missing status", "success wrong status", "success missing flag", "success false flag", "success null flag"} {
		t.Run(failure, func(t *testing.T) {
			writes, lists := 0, 0
			r := &collectionGroupResource{api: cgUnitClient(t, func(w http.ResponseWriter, req *http.Request) {
				if cgUnitParents(t, w, req) {
					return
				}
				if req.URL.Path == "/api/collections.group_memberships" {
					lists++
					cgUnitOffset(t, w, req)
					groupTestEncode(t, w, cgUnitEnvelope([]client.GroupMembership{cgUnitGrant(cgUnitGroupID, client.PermissionRead)}, 0, 1, false))
					return
				}
				if req.URL.Path != "/api/collections.remove_group" {
					t.Errorf("unexpected mutation: %s", req.URL.Path)
					return
				}
				writes++
				cgUnitBody(t, w, req, map[string]any{"id": cgUnitCollectionID, "groupId": cgUnitGroupID})
				status := 400
				body := cgUnitAbsentRemove()
				if strings.HasPrefix(failure, "success") {
					status = 200
					body = map[string]any{"ok": true, "status": 200, "success": true}
				}
				switch failure {
				case "missing ok", "success missing ok":
					delete(body, "ok")
				case "null ok":
					body["ok"] = nil
				case "true ok":
					body["ok"] = true
				case "success false ok":
					body["ok"] = false
				case "missing status", "success missing status":
					delete(body, "status")
				case "null status":
					body["status"] = nil
				case "wrong status", "success wrong status":
					body["status"] = 201
				case "missing error":
					delete(body, "error")
				case "wrong error":
					body["error"] = "validation_error"
				case "missing message":
					delete(body, "message")
				case "wrong message":
					body["message"] = "Group not found"
				case "case changed":
					body["message"] = "This group is not a part of the collection"
				case "extra whitespace":
					body["message"] = "This Group is not a part of the collection "
				case "last manager":
					body["message"] = "Cannot remove the last collection manager " + groupTestKey
				case "403 absent body":
					status = 403
				case "404 absent body":
					status = 404
				case "not JSON":
					w.Header().Set("Content-Type", "text/plain")
				case "success missing flag":
					delete(body, "success")
				case "success false flag":
					body["success"] = false
				case "success null flag":
					body["success"] = nil
				}
				w.WriteHeader(status)
				if failure == "invalid JSON" {
					groupTestWrite(t, w, "{bad")
				} else {
					groupTestEncode(t, w, body)
				}
			})}
			diagnostics, state := cgUnitOperation(t, r, "delete", cgUnitModel(), cgUnitModel())
			cgUnitAssertFailureState(t, "delete", diagnostics, state, cgUnitModel())
			if writes != 1 || lists != 1 || strings.Contains(fmt.Sprint(diagnostics), groupTestKey) {
				t.Fatalf("ambiguous removal was confirmed/retried or leaked a secret: %v writes=%d lists=%d", diagnostics, writes, lists)
			}
		})
	}
}
