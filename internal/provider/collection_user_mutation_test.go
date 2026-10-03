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

func TestCollectionUserBadUpsertResponseRetainsTrustedPair(t *testing.T) {
	failures := []string{"missing JSON", "invalid JSON", "missing ok", "false ok", "null ok", "missing status", "wrong status", "null status", "missing data", "null data", "missing grants", "null grants", "empty grants", "multiple grants",
		"missing grant ID", "composite grant ID", "uppercase grant ID", "zero grant ID", "invalid grant ID", "missing user ID", "zero user ID", "wrong user", "missing collection", "null collection", "wrong collection",
		"missing document", "document grant", "missing source", "inherited grant", "missing permission", "null permission", "unsafe permission", "wrong permission",
		"missing users", "null users", "empty users", "multiple users", "missing presented user ID", "null presented user ID", "zero presented user ID", "invalid presented user ID", "wrong presented user",
		"missing user name", "null user name", "missing user role", "null user role", "invalid user role", "missing suspension", "null suspension", "invalid suspension",
		"closed connection", "truncated JSON", "empty HTTP200", "400", "403", "404", "429", "500"}
	for _, operation := range []string{"create", "update"} {
		for _, failure := range failures {
			t.Run(operation+"/"+failure, func(t *testing.T) {
				var mu sync.Mutex
				writes, lists := 0, 0
				var readQuery *string
				members := []client.Membership{}
				if operation == "update" {
					members = append(members, cuUnitGrant(cuUnitUserID, client.PermissionRead))
				}
				r := &collectionUserResource{api: cuUnitClient(t, func(w http.ResponseWriter, req *http.Request) {
					mu.Lock()
					defer mu.Unlock()
					if cuUnitParents(t, w, req) {
						return
					}
					if req.URL.Path == "/api/collections.memberships" {
						lists++
						cuUnitOffset(t, w, req, readQuery)
						groupTestEncode(t, w, cuUnitEnvelope(members, 0, len(members), false))
						return
					}
					if req.URL.Path != "/api/collections.add_user" {
						t.Errorf("unexpected write: %s", req.URL.Path)
						return
					}
					writes++
					cuUnitBody(t, w, req, map[string]any{"id": cuUnitCollectionID, "userId": cuUnitUserID, "permission": "admin"})
					// Simulate a committed upsert followed by an untrusted response.
					members = []client.Membership{cuUnitGrant(cuUnitUserID, client.PermissionAdmin)}
					page := cuUnitJSONMap(t, cuUnitEnvelope(members, 0, 0, true))
					data := page["data"].(map[string]any)
					grant := data["memberships"].([]any)[0].(map[string]any)
					user := data["users"].([]any)[0].(map[string]any)
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
						delete(data, "memberships")
					case "null grants":
						data["memberships"] = nil
					case "empty grants":
						data["memberships"] = []any{}
					case "multiple grants":
						data["memberships"] = []any{grant, grant}
					case "missing grant ID":
						delete(grant, "id")
					case "composite grant ID":
						grant["id"] = cuUnitPairID
					case "uppercase grant ID":
						grant["id"] = strings.ToUpper(cuUnitGrantID)
					case "zero grant ID":
						grant["id"] = "00000000-0000-0000-0000-000000000000"
					case "invalid grant ID":
						grant["id"] = "not-a-uuid"
					case "missing user ID":
						delete(grant, "userId")
					case "zero user ID":
						grant["userId"] = "00000000-0000-0000-0000-000000000000"
					case "wrong user":
						grant["userId"] = cuUnitOtherUserID
					case "missing collection":
						delete(grant, "collectionId")
					case "null collection":
						grant["collectionId"] = nil
					case "wrong collection":
						grant["collectionId"] = cuUnitUserID
					case "missing document":
						delete(grant, "documentId")
					case "document grant":
						grant["documentId"] = cuUnitUserID
					case "missing source":
						delete(grant, "sourceId")
					case "inherited grant":
						grant["sourceId"] = cuUnitGrantID
					case "missing permission":
						delete(grant, "permission")
					case "null permission":
						grant["permission"] = nil
					case "unsafe permission":
						grant["permission"] = "owner"
					case "wrong permission":
						grant["permission"] = "read_write"
					case "missing users":
						delete(data, "users")
					case "null users":
						data["users"] = nil
					case "empty users":
						data["users"] = []any{}
					case "multiple users":
						data["users"] = []any{user, user}
					case "missing presented user ID":
						delete(user, "id")
					case "null presented user ID":
						user["id"] = nil
					case "zero presented user ID":
						user["id"] = uuid.Nil.String()
					case "invalid presented user ID":
						user["id"] = "a32c2ee6-fbde-0654-841b-0eabdc71b812"
					case "wrong presented user":
						user["id"] = cuUnitOtherUserID
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
				mu.Lock()
				gotWrites, gotLists := writes, lists
				mu.Unlock()
				if !diagnostics.HasError() || gotWrites != 1 || gotLists != 1 {
					t.Fatalf("untrusted mutation accepted, retried, or reconciled silently: writes=%d lists=%d diagnostics=%v", gotWrites, gotLists, diagnostics)
				}
				if operation == "create" {
					groupTestDiagnostics(t, diagnostics, "pair ID has been retained")
					groupTestDiagnostics(t, diagnostics, "terraform untaint")
					if cuUnitStateModel(t, state) != desired {
						t.Fatal("failed create discarded the trusted pair or adopted the response identity")
					}
				} else if cuUnitStateModel(t, state) != cuUnitModel() {
					t.Fatal("failed update changed the saved pair or permission")
				}
				if strings.Contains(fmt.Sprint(diagnostics), groupTestKey) {
					t.Fatal("diagnostics leaked API credentials")
				}
				// Refresh recovers the committed permission without another write.
				mu.Lock()
				readQuery = cuUnitParentUser(cuUnitUserID).Name
				mu.Unlock()
				diagnostics, state = cuUnitOperation(t, r, "read", cuUnitStateModel(t, state), desired)
				mu.Lock()
				gotWrites = writes
				mu.Unlock()
				if diagnostics.HasError() || cuUnitStateModel(t, state) != desired || gotWrites != 1 {
					t.Fatalf("refresh failed to recover committed pair: %v writes=%d", diagnostics, gotWrites)
				}
			})
		}
	}
}

func TestCollectionUserUpsertRejectsUnsafePermissionBeforeRequest(t *testing.T) {
	api := cuUnitClient(t, func(http.ResponseWriter, *http.Request) { t.Error("unsafe permission reached Outline") })
	for _, permission := range []client.Permission{"", "member", "owner", "READ", "read-write"} {
		t.Run(string(permission), func(t *testing.T) {
			member, err := api.writeCollectionUser(t.Context(), uuid.MustParse(cuUnitCollectionID), uuid.MustParse(cuUnitUserID), permission)
			if err == nil || member != nil || !strings.Contains(err.Error(), "read, read_write, or admin") {
				t.Fatalf("unsafe permission %q was not refused: member=%v err=%v", permission, member, err)
			}
		})
	}
}

func cuUnitAbsentRemove() map[string]any {
	return map[string]any{"ok": false, "status": 400, "error": "invalid_request", "message": "User is not a collection member"}
}

func TestCollectionUserRemoveExactAbsenceNeedsCompleteConfirmation(t *testing.T) {
	for _, response := range []string{"success", "exact absent 400"} {
		for _, evidence := range []string{"absent", "still present", "later malformed", "list 403", "list 404"} {
			t.Run(response+"/"+evidence, func(t *testing.T) {
				members := cuUnitGrants(101)
				members[0] = cuUnitGrant(cuUnitUserID, client.PermissionRead)
				removed := false
				writes := 0
				var offsets []int
				r := &collectionUserResource{api: cuUnitClient(t, func(w http.ResponseWriter, req *http.Request) {
					if cuUnitParents(t, w, req) {
						return
					}
					switch req.URL.Path {
					case "/api/collections.memberships":
						offset := cuUnitOffset(t, w, req)
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
						page := cuUnitEnvelope(members[offset:min(offset+100, len(members))], offset, len(members), false)
						if removed && evidence == "later malformed" && offset == 100 {
							delete(page, "pagination")
						}
						groupTestEncode(t, w, page)
					case "/api/collections.remove_user":
						writes++
						removed = true
						cuUnitBody(t, w, req, map[string]any{"id": cuUnitCollectionID, "userId": cuUnitUserID})
						if evidence != "still present" {
							// Keep >100 unrelated grants after deleting the pair.
							members[0] = cuUnitGrants(1)[0]
						}
						if response == "exact absent 400" {
							w.WriteHeader(http.StatusBadRequest)
							groupTestEncode(t, w, cuUnitAbsentRemove())
						} else {
							groupTestEncode(t, w, map[string]any{"ok": true, "status": 200, "success": true})
						}
					default:
						t.Errorf("unexpected endpoint: %s", req.URL.Path)
					}
				})}
				diagnostics, state := cuUnitOperation(t, r, "delete", cuUnitModel(), cuUnitModel())
				bad := evidence != "absent"
				if diagnostics.HasError() != bad || writes != 1 || len(offsets) < 3 || offsets[0] != 0 || offsets[1] != 100 || offsets[2] != 0 {
					t.Fatalf("remove must validate a second full list: %v writes=%d offsets=%v", diagnostics, writes, offsets)
				}
				if (evidence == "absent" || evidence == "still present" || evidence == "later malformed") && (len(offsets) != 4 || offsets[3] != 100) {
					t.Fatalf("confirmation stopped before the later page: %v", offsets)
				}
				if bad && cuUnitStateModel(t, state) != cuUnitModel() {
					t.Fatal("unconfirmed remove lost the pair")
				}
			})
		}
	}
}

func TestCollectionUserRemoveAmbiguousErrorsAndBadSuccessRetainState(t *testing.T) {
	for _, failure := range []string{"missing ok", "null ok", "true ok", "missing status", "null status", "wrong status", "missing error", "wrong error", "missing message", "wrong message", "case changed", "extra whitespace", "last manager", "403 absent body", "404 absent body", "429 absent body", "500 absent body", "invalid JSON", "not JSON",
		"success missing ok", "success false ok", "success missing status", "success wrong status", "success missing flag", "success false flag", "success null flag"} {
		t.Run(failure, func(t *testing.T) {
			writes, lists := 0, 0
			r := &collectionUserResource{api: cuUnitClient(t, func(w http.ResponseWriter, req *http.Request) {
				if cuUnitParents(t, w, req) {
					return
				}
				if req.URL.Path == "/api/collections.memberships" {
					lists++
					cuUnitOffset(t, w, req)
					groupTestEncode(t, w, cuUnitEnvelope([]client.Membership{cuUnitGrant(cuUnitUserID, client.PermissionRead)}, 0, 1, false))
					return
				}
				if req.URL.Path != "/api/collections.remove_user" {
					t.Errorf("unexpected mutation: %s", req.URL.Path)
					return
				}
				writes++
				cuUnitBody(t, w, req, map[string]any{"id": cuUnitCollectionID, "userId": cuUnitUserID})
				status := 400
				body := cuUnitAbsentRemove()
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
					body["message"] = "User not found"
				case "case changed":
					body["message"] = "user is not a collection member"
				case "extra whitespace":
					body["message"] = "User is not a collection member "
				case "last manager":
					body["message"] = "Cannot remove the last collection manager " + groupTestKey
				case "403 absent body":
					status = 403
				case "404 absent body":
					status = 404
				case "429 absent body":
					status = 429
				case "500 absent body":
					status = 500
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
			diagnostics, state := cuUnitOperation(t, r, "delete", cuUnitModel(), cuUnitModel())
			cuUnitAssertFailureState(t, "delete", diagnostics, state, cuUnitModel())
			if writes != 1 || lists != 1 || strings.Contains(fmt.Sprint(diagnostics), groupTestKey) {
				t.Fatalf("ambiguous removal was confirmed/retried or leaked a secret: %v writes=%d lists=%d", diagnostics, writes, lists)
			}
		})
	}
}
