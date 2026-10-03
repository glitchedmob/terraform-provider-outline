// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/glitchedmob/terraform-provider-outline/internal/client"
	"github.com/google/uuid"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/oapi-codegen/nullable"
)

func TestCollectionUserTargetRolesSuspensionAndPublicPresenters(t *testing.T) {
	for _, role := range []client.UserRole{client.UserRoleAdmin, client.UserRoleMember, client.UserRoleGuest, client.UserRoleViewer} {
		for _, suspended := range []bool{false, true} {
			for _, email := range []string{"omitted", "null"} {
				for _, operation := range []string{"create", "read", "update", "delete", "import"} {
					t.Run(fmt.Sprintf("%s/%s/suspended=%t/email=%s", operation, role, suspended, email), func(t *testing.T) {
						target := cuUnitParentUser(cuUnitUserID)
						target.Role, target.IsSuspended = &role, &suspended
						// Pending guests have no activity timestamp. The grant must
						// not require activation or change the target's account.
						target.LastActiveAt = nullable.NewNullNullable[time.Time]()
						before := target
						public := target
						public.Email = nil
						members := []client.Membership{}
						if operation != "create" {
							members = append(members, cuUnitGrant(cuUnitUserID, client.PermissionRead))
						}
						envelope := func(rows []client.Membership, mutation bool) map[string]any {
							page := cuUnitEnvelope(rows, 0, len(rows), mutation)
							if len(rows) != 0 {
								presenter := cuUnitJSONMap(t, public)
								if email == "null" {
									presenter["email"] = nil
								}
								page["data"].(map[string]any)["users"] = []any{presenter}
							}
							return page
						}
						writes, reads := 0, 0
						r := &collectionUserResource{api: cuUnitClient(t, func(w http.ResponseWriter, req *http.Request) {
							switch req.URL.Path {
							case "/api/users.info":
								reads++
								cuUnitBody(t, w, req, map[string]any{"id": cuUnitUserID})
								groupTestEncode(t, w, userTestEnvelope(&target))
							case "/api/collections.memberships":
								cuUnitOffset(t, w, req, grantTestReadQuery(operation, target.Name))
								groupTestEncode(t, w, envelope(members, false))
							case "/api/collections.add_user":
								writes++
								permission := client.PermissionRead
								if operation == "update" {
									permission = client.PermissionAdmin
								}
								cuUnitBody(t, w, req, map[string]any{"id": cuUnitCollectionID, "userId": cuUnitUserID, "permission": string(permission)})
								members = []client.Membership{cuUnitGrant(cuUnitUserID, permission)}
								groupTestEncode(t, w, envelope(members, true))
							case "/api/collections.remove_user":
								writes++
								cuUnitBody(t, w, req, map[string]any{"id": cuUnitCollectionID, "userId": cuUnitUserID})
								members = []client.Membership{}
								groupTestEncode(t, w, map[string]any{"ok": true, "status": 200, "success": true})
							default:
								if !cuUnitParents(t, w, req) {
									t.Errorf("grant attempted account or group mutation: %s", req.URL.Path)
								}
							}
						})}
						desired := cuUnitModel()
						if operation == "update" {
							desired.Permission = types.StringValue("admin")
						}
						diagnostics, state := cuUnitOperation(t, r, operation, cuUnitModel(), desired)
						wantWrites := 0
						if operation == "create" || operation == "update" || operation == "delete" {
							wantWrites = 1
						}
						if diagnostics.HasError() || reads != 1 || writes != wantWrites || !reflect.DeepEqual(target, before) {
							t.Fatalf("valid target refused or account changed: %v reads=%d writes=%d", diagnostics, reads, writes)
						}
						if operation != "delete" && cuUnitStateModel(t, state) != desired {
							t.Fatal("grant returned the wrong state")
						}
					})
				}
			}
		}
	}
}

func TestCollectionUserRefusesAPIKeyOwnerEveryOperation(t *testing.T) {
	for _, operation := range []string{"create", "read", "update", "delete", "import"} {
		t.Run(operation, func(t *testing.T) {
			model := cuUnitModel()
			model.UserID = types.StringValue(userTestOwnerID)
			model.ID = types.StringValue(cuUnitCollectionID + "/" + userTestOwnerID)
			calls := 0
			r := &collectionUserResource{api: cuUnitClient(t, func(w http.ResponseWriter, req *http.Request) {
				calls++
				if req.URL.Path != "/api/auth.info" {
					t.Errorf("owner grant reached target or mutation: %s", req.URL.Path)
					return
				}
				groupTestEncode(t, w, userTestAuth(userTestOwner()))
			})}
			desired := model
			if operation == "update" {
				desired.Permission = types.StringValue("admin")
			}
			diagnostics, state := cuUnitOperation(t, r, operation, model, desired)
			groupTestDiagnostics(t, diagnostics, "API-key owner's own direct collection grant")
			cuUnitAssertFailureState(t, operation, diagnostics, state, model)
			if calls != 1 {
				t.Fatalf("owner refusal made %d requests", calls)
			}
		})
	}
}

func TestCollectionUserArchivedCollectionsRefused(t *testing.T) {
	for _, operation := range []string{"create", "read", "update", "delete", "import"} {
		t.Run(operation, func(t *testing.T) {
			parentReads := 0
			r := &collectionUserResource{api: cuUnitClient(t, func(w http.ResponseWriter, req *http.Request) {
				switch req.URL.Path {
				case "/api/auth.info":
					groupTestEncode(t, w, userTestAuth(userTestOwner()))
				case "/api/collections.info":
					parentReads++
					cuUnitBody(t, w, req, map[string]any{"id": cuUnitCollectionID})
					collection := collectionTestCollection()
					collection.ArchivedAt = nullable.NewNullableWithValue(time.Now().UTC())
					groupTestEncode(t, w, collectionTestEnvelope(collection))
				default:
					t.Errorf("archived collection reached user/grant endpoint: %s", req.URL.Path)
				}
			})}
			diagnostics, state := cuUnitOperation(t, r, operation, cuUnitModel(), cuUnitModel())
			groupTestDiagnostics(t, diagnostics, "archived collections")
			cuUnitAssertFailureState(t, operation, diagnostics, state, cuUnitModel())
			if parentReads != 1 {
				t.Fatal("archived parent was retried")
			}
		})
	}
}

func TestCollectionUserRequiresActiveAdminBeforeTargetOperations(t *testing.T) {
	for _, failure := range []string{"member", "viewer", "guest", "suspended admin", "missing role", "missing suspension", "missing auth user", "403", "404"} {
		for _, operation := range []string{"create", "read", "update", "delete", "import"} {
			t.Run(operation+"/"+failure, func(t *testing.T) {
				calls := 0
				r := &collectionUserResource{api: cuUnitClient(t, func(w http.ResponseWriter, req *http.Request) {
					calls++
					if req.URL.Path != "/api/auth.info" {
						t.Errorf("unsafe API key reached target: %s", req.URL.Path)
						return
					}
					user := userTestOwner()
					switch failure {
					case "member":
						user.Role = groupTestPointer(client.UserRoleMember)
					case "viewer":
						user.Role = groupTestPointer(client.UserRoleViewer)
					case "guest":
						user.Role = groupTestPointer(client.UserRoleGuest)
					case "suspended admin":
						user.IsSuspended = groupTestPointer(true)
					case "missing role":
						user.Role = nil
					case "missing suspension":
						user.IsSuspended = nil
					case "missing auth user":
						user = nil
					case "403", "404":
						status := 403
						if failure == "404" {
							status = 404
						}
						w.WriteHeader(status)
						groupTestEncode(t, w, map[string]any{"ok": false, "status": status, "error": "not_found", "message": groupTestKey})
						return
					}
					groupTestEncode(t, w, userTestAuth(user))
				})}
				diagnostics, state := cuUnitOperation(t, r, operation, cuUnitModel(), cuUnitModel())
				cuUnitAssertFailureState(t, operation, diagnostics, state, cuUnitModel())
				if calls != 1 || strings.Contains(fmt.Sprint(diagnostics), groupTestKey) {
					t.Fatalf("auth failure retried or leaked credentials: calls=%d diagnostics=%v", calls, diagnostics)
				}
			})
		}
	}
}

func TestCollectionUserCollectionParentAbsenceOnlyVerifiedRelease404(t *testing.T) {
	for _, failure := range []string{"verified 404", "403", "route 404", "404 wrong error", "404 missing status", "404 wrong status", "404 missing ok", "404 true ok", "404 not JSON"} {
		for _, operation := range []string{"create", "read", "update", "delete", "import"} {
			t.Run(operation+"/"+failure, func(t *testing.T) {
				parentCalls := 0
				r := &collectionUserResource{api: cuUnitClient(t, func(w http.ResponseWriter, req *http.Request) {
					if req.URL.Path == "/api/auth.info" {
						groupTestEncode(t, w, userTestAuth(userTestOwner()))
						return
					}
					if req.URL.Path != "/api/collections.info" {
						t.Errorf("collection error reached list/user/grant endpoint: %s", req.URL.Path)
						return
					}
					parentCalls++
					cuUnitBody(t, w, req, map[string]any{"id": cuUnitCollectionID})
					body := map[string]any{"ok": false, "status": 404, "error": "not_found", "message": groupTestKey}
					status := 404
					switch failure {
					case "403":
						status = 403
						body["status"], body["error"] = 403, "authorization_error"
					case "route 404":
						body = map[string]any{"message": "unknown route"}
					case "404 wrong error":
						body["error"] = "authorization_error"
					case "404 missing status":
						delete(body, "status")
					case "404 wrong status":
						body["status"] = 403
					case "404 missing ok":
						delete(body, "ok")
					case "404 true ok":
						body["ok"] = true
					case "404 not JSON":
						w.Header().Set("Content-Type", "text/plain")
					}
					w.WriteHeader(status)
					groupTestEncode(t, w, body)
				})}
				diagnostics, state := cuUnitOperation(t, r, operation, cuUnitModel(), cuUnitModel())
				if failure == "verified 404" && (operation == "read" || operation == "delete") {
					if diagnostics.HasError() || operation == "read" && !state.Raw.IsNull() {
						t.Fatalf("verified parent absence: %v", diagnostics)
					}
				} else {
					cuUnitAssertFailureState(t, operation, diagnostics, state, cuUnitModel())
				}
				if parentCalls != 1 || strings.Contains(fmt.Sprint(diagnostics), groupTestKey) {
					t.Fatalf("parent response was retried or leaked a secret: %v", diagnostics)
				}
			})
		}
	}
}

func TestCollectionUserUserParentAbsenceNeedsCompleteAllUsersList(t *testing.T) {
	for _, status := range []int{403, 404} {
		for _, evidence := range []string{"absent", "present early", "present late", "list 403", "list 404", "later malformed", "later malformed after present", "changed total", "duplicate user", "missing email"} {
			for _, operation := range []string{"create", "read", "update", "delete", "import"} {
				t.Run(fmt.Sprintf("%s/%d/%s", operation, status, evidence), func(t *testing.T) {
					users := make([]client.User, 101)
					for i := range users {
						users[i] = cuUnitParentUser(uuid.NewString())
						users[i].IsSuspended = groupTestPointer(true)
					}
					if evidence == "present early" || evidence == "later malformed after present" {
						users[0] = cuUnitParentUser(cuUnitUserID)
						users[0].IsSuspended = groupTestPointer(true)
					}
					if evidence == "present late" {
						users[100] = cuUnitParentUser(cuUnitUserID)
						users[100].IsSuspended = groupTestPointer(true)
					}
					var offsets []int
					r := &collectionUserResource{api: cuUnitClient(t, func(w http.ResponseWriter, req *http.Request) {
						switch req.URL.Path {
						case "/api/users.info":
							cuUnitBody(t, w, req, map[string]any{"id": cuUnitUserID})
							w.WriteHeader(status)
							groupTestEncode(t, w, map[string]any{"ok": false, "status": status, "error": "authorization_error", "message": "target denied " + groupTestKey})
						case "/api/users.list":
							offset := userTestListOffset(t, w, req)
							if offset != 0 && offset != 100 {
								t.Errorf("unexpected user-list offset %d", offset)
								return
							}
							offsets = append(offsets, offset)
							if evidence == "list 403" || evidence == "list 404" {
								code := 403
								if evidence == "list 404" {
									code = 404
								}
								w.WriteHeader(code)
								groupTestEncode(t, w, map[string]any{"error": "authorization_error", "message": groupTestKey})
								return
							}
							page := userTestList(users[offset:min(offset+100, len(users))], offset, 100, len(users))
							if offset == 100 {
								switch evidence {
								case "later malformed", "later malformed after present":
									delete(page, "pagination")
								case "changed total":
									page = userTestList([]client.User{users[100], cuUnitParentUser(uuid.NewString())}, offset, 100, 102)
								case "duplicate user":
									page = userTestList([]client.User{users[0]}, offset, 100, len(users))
								case "missing email":
									user := users[100]
									user.Email = nil
									page = userTestList([]client.User{user}, offset, 100, len(users))
								}
							}
							groupTestEncode(t, w, page)
						default:
							if !cuUnitParents(t, w, req) {
								t.Errorf("unverified user absence reached grant endpoint: %s", req.URL.Path)
							}
						}
					})}
					diagnostics, state := cuUnitOperation(t, r, operation, cuUnitModel(), cuUnitModel())
					if evidence == "absent" && (operation == "read" || operation == "delete") {
						if diagnostics.HasError() || operation == "read" && !state.Raw.IsNull() {
							t.Fatalf("verified user absence: %v", diagnostics)
						}
					} else {
						cuUnitAssertFailureState(t, operation, diagnostics, state, cuUnitModel())
						if evidence == "present early" || evidence == "present late" {
							// A suspended user in the full list is present. Keep
							// the original forbidden/not-found diagnostic.
							if status == http.StatusForbidden {
								groupTestDiagnostics(t, diagnostics, "HTTP 403")
							} else {
								groupTestDiagnostics(t, diagnostics, "users.info: outline object not found")
							}
						}
					}
					wantOffsets := []int{0, 100}
					if evidence == "list 403" || evidence == "list 404" {
						wantOffsets = []int{0}
					}
					if !reflect.DeepEqual(offsets, wantOffsets) || strings.Contains(fmt.Sprint(diagnostics), groupTestKey) {
						t.Fatalf("incomplete parent check or leaked secret: offsets=%v diagnostics=%v", offsets, diagnostics)
					}
				})
			}
		}
	}
}

func TestCollectionUserMalformedParentsRetainStateAndNeverMutate(t *testing.T) {
	for _, endpoint := range []string{"collections.info", "users.info"} {
		failures := []string{"missing JSON", "invalid JSON", "missing data", "null data", "missing ok", "false ok", "wrong status", "id missing", "id null", "id zero", "id invalid", "id wrong", "name missing", "name null"}
		if endpoint == "users.info" {
			failures = append(failures, "email missing", "email null", "email empty", "role missing", "role null", "role invalid", "isSuspended missing", "isSuspended null", "isSuspended invalid")
		} else {
			failures = append(failures, "name empty", "description missing", "permission missing", "permission invalid", "sharing missing", "sharing null", "archivedAt missing", "deletedAt missing", "deletedAt nonnull")
		}
		for _, failure := range failures {
			for _, operation := range []string{"create", "read", "update", "delete", "import"} {
				t.Run(operation+"/"+endpoint+"/"+failure, func(t *testing.T) {
					reads := 0
					r := &collectionUserResource{api: cuUnitClient(t, func(w http.ResponseWriter, req *http.Request) {
						if req.URL.Path != "/api/"+endpoint {
							if endpoint == "collections.info" && req.URL.Path != "/api/auth.info" ||
								endpoint == "users.info" && req.URL.Path != "/api/auth.info" && req.URL.Path != "/api/collections.info" {
								t.Errorf("malformed parent reached grant/absence endpoint: %s", req.URL.Path)
								return
							}
							if !cuUnitParents(t, w, req) {
								t.Errorf("unexpected parent endpoint: %s", req.URL.Path)
							}
							return
						}
						reads++
						id := cuUnitCollectionID
						page := cuUnitJSONMap(t, collectionTestEnvelope(collectionTestCollection()))
						if endpoint == "users.info" {
							id = cuUnitUserID
							page = cuUnitJSONMap(t, userTestEnvelope(groupTestPointer(cuUnitParentUser(cuUnitUserID))))
						}
						cuUnitBody(t, w, req, map[string]any{"id": id})
						data := page["data"].(map[string]any)
						switch failure {
						case "missing JSON":
							w.Header().Set("Content-Type", "text/plain")
						case "invalid JSON":
							groupTestWrite(t, w, "{bad")
							return
						case "missing data":
							delete(page, "data")
						case "null data":
							page["data"] = nil
						case "missing ok":
							delete(page, "ok")
						case "false ok":
							page["ok"] = false
						case "wrong status":
							page["status"] = 201
						default:
							parts := strings.SplitN(failure, " ", 2)
							field, kind := parts[0], parts[1]
							switch kind {
							case "missing":
								delete(data, field)
							case "null":
								data[field] = nil
							case "zero":
								data[field] = uuid.Nil.String()
							case "wrong":
								data[field] = cuUnitOtherUserID
							case "empty":
								data[field] = ""
							case "nonnull":
								data[field] = time.Now().UTC()
							case "invalid":
								data[field] = "invalid"
							}
						}
						groupTestEncode(t, w, page)
					})}
					diagnostics, state := cuUnitOperation(t, r, operation, cuUnitModel(), cuUnitModel())
					cuUnitAssertFailureState(t, operation, diagnostics, state, cuUnitModel())
					if reads != 1 {
						t.Fatal("malformed parent was retried")
					}
				})
			}
		}
	}
}

func TestCollectionUserUnverifiedUserErrorsNeverTriggerAbsenceList(t *testing.T) {
	for _, failure := range []string{"ordinary 403", "missing error 403", "not JSON 403", "400", "401", "429", "500"} {
		for _, operation := range []string{"create", "read", "update", "delete", "import"} {
			t.Run(operation+"/"+failure, func(t *testing.T) {
				reads := 0
				r := &collectionUserResource{api: cuUnitClient(t, func(w http.ResponseWriter, req *http.Request) {
					if req.URL.Path == "/api/users.info" {
						reads++
						cuUnitBody(t, w, req, map[string]any{"id": cuUnitUserID})
						status := map[string]int{"ordinary 403": 403, "missing error 403": 403, "not JSON 403": 403, "400": 400, "401": 401, "429": 429, "500": 500}[failure]
						body := map[string]any{"ok": false, "status": status, "error": "permission_denied", "message": groupTestKey}
						if failure == "missing error 403" {
							delete(body, "error")
						}
						if failure == "not JSON 403" {
							w.Header().Set("Content-Type", "text/plain")
							body["error"] = "authorization_error"
						}
						w.WriteHeader(status)
						groupTestEncode(t, w, body)
						return
					}
					if !cuUnitParents(t, w, req) {
						t.Errorf("unverified error listed users or mutated grants: %s", req.URL.Path)
					}
				})}
				diagnostics, state := cuUnitOperation(t, r, operation, cuUnitModel(), cuUnitModel())
				cuUnitAssertFailureState(t, operation, diagnostics, state, cuUnitModel())
				if reads != 1 || strings.Contains(fmt.Sprint(diagnostics), groupTestKey) {
					t.Fatalf("user error was retried or leaked secret: %v", diagnostics)
				}
			})
		}
	}
}
