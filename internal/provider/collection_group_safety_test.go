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

func TestCollectionGroupExternalSynchronizedGroupsAllowedArchivedCollectionsRefused(t *testing.T) {
	for _, kind := range []string{"external ID", "external group", "both", "archived"} {
		for _, operation := range []string{"create", "read", "update", "delete", "import"} {
			t.Run(operation+"/"+kind, func(t *testing.T) {
				group := cgUnitGroup(cgUnitGroupID)
				if kind == "external ID" || kind == "both" {
					group.ExternalId = nullable.NewNullableWithValue("idp-group")
				}
				if kind == "external group" || kind == "both" {
					group.ExternalGroup = nullable.NewNullableWithValue(map[string]any{"id": "idp-group", "provider": "oidc"})
				}
				collection := collectionTestCollection()
				if kind == "archived" {
					collection.ArchivedAt = nullable.NewNullableWithValue(time.Now().UTC())
				}
				members := []client.GroupMembership{}
				if operation != "create" {
					members = append(members, cgUnitGrant(cgUnitGroupID, client.PermissionRead))
				}
				writes, groupReads, lists := 0, 0, 0
				r := &collectionGroupResource{api: cgUnitClient(t, func(w http.ResponseWriter, req *http.Request) {
					switch req.URL.Path {
					case "/api/collections.info":
						cgUnitBody(t, w, req, map[string]any{"id": cgUnitCollectionID})
						groupTestEncode(t, w, collectionTestEnvelope(collection))
					case "/api/groups.info":
						groupReads++
						cgUnitBody(t, w, req, map[string]any{"id": cgUnitGroupID})
						groupTestEncode(t, w, map[string]any{"ok": true, "status": 200, "data": group})
					case "/api/collections.group_memberships":
						lists++
						cgUnitOffset(t, w, req)
						page := cgUnitEnvelope(members, 0, len(members), false)
						if len(members) > 0 {
							page["data"].(map[string]any)["groups"] = []client.Group{group}
						}
						groupTestEncode(t, w, page)
					case "/api/collections.add_group":
						writes++
						permission := client.PermissionRead
						if operation == "update" {
							permission = client.PermissionAdmin
						}
						cgUnitBody(t, w, req, map[string]any{"id": cgUnitCollectionID, "groupId": cgUnitGroupID, "permission": string(permission)})
						page := cgUnitEnvelope([]client.GroupMembership{cgUnitGrant(cgUnitGroupID, permission)}, 0, 0, true)
						page["data"].(map[string]any)["groups"] = []client.Group{group}
						groupTestEncode(t, w, page)
					case "/api/collections.remove_group":
						writes++
						cgUnitBody(t, w, req, map[string]any{"id": cgUnitCollectionID, "groupId": cgUnitGroupID})
						members = []client.GroupMembership{}
						groupTestEncode(t, w, map[string]any{"ok": true, "status": 200, "success": true})
					default:
						if !cgUnitParents(t, w, req) {
							t.Errorf("grant attempted to edit group synchronization or another endpoint: %s", req.URL.Path)
						}
					}
				})}
				desired := cgUnitModel()
				if operation == "update" {
					desired.Permission = types.StringValue("admin")
				}
				diagnostics, state := cgUnitOperation(t, r, operation, cgUnitModel(), desired)
				if kind == "archived" {
					groupTestDiagnostics(t, diagnostics, "archived collections")
					cgUnitAssertFailureState(t, operation, diagnostics, state, cgUnitModel())
					if groupReads != 0 || lists != 0 || writes != 0 {
						t.Fatalf("archived target reached a group/grant operation: %d/%d/%d", groupReads, lists, writes)
					}
				} else {
					wantWrites := 0
					if operation == "create" || operation == "update" || operation == "delete" {
						wantWrites = 1
					}
					if diagnostics.HasError() || groupReads != 1 || writes != wantWrites {
						t.Fatalf("externally synchronized group refused: %v group reads=%d writes=%d", diagnostics, groupReads, writes)
					}
					if operation != "delete" && cgUnitStateModel(t, state) != desired {
						t.Fatal("allowed external grant produced the wrong state")
					}
				}
			})
		}
	}
}

func TestCollectionGroupRequiresActiveAdminBeforeTargetOperations(t *testing.T) {
	for _, failure := range []string{"member", "viewer", "suspended admin", "missing role", "missing suspension", "missing auth user", "403", "404"} {
		for _, operation := range []string{"create", "read", "update", "delete", "import"} {
			t.Run(operation+"/"+failure, func(t *testing.T) {
				calls := 0
				r := &collectionGroupResource{api: cgUnitClient(t, func(w http.ResponseWriter, req *http.Request) {
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
				diagnostics, state := cgUnitOperation(t, r, operation, cgUnitModel(), cgUnitModel())
				cgUnitAssertFailureState(t, operation, diagnostics, state, cgUnitModel())
				if calls != 1 || strings.Contains(fmt.Sprint(diagnostics), groupTestKey) {
					t.Fatalf("auth failure retried or leaked credentials: calls=%d diagnostics=%v", calls, diagnostics)
				}
			})
		}
	}
}

func TestCollectionGroupCollectionParentAbsenceOnlyVerifiedRelease404(t *testing.T) {
	for _, failure := range []string{"verified 404", "403", "route 404", "404 wrong error", "404 missing status", "404 wrong status", "404 missing ok", "404 true ok", "404 not JSON"} {
		for _, operation := range []string{"create", "read", "update", "delete", "import"} {
			t.Run(operation+"/"+failure, func(t *testing.T) {
				parentCalls := 0
				r := &collectionGroupResource{api: cgUnitClient(t, func(w http.ResponseWriter, req *http.Request) {
					if req.URL.Path == "/api/auth.info" {
						groupTestEncode(t, w, userTestAuth(userTestOwner()))
						return
					}
					if req.URL.Path != "/api/collections.info" {
						t.Errorf("collection error reached list/group/grant endpoint: %s", req.URL.Path)
						return
					}
					parentCalls++
					cgUnitBody(t, w, req, map[string]any{"id": cgUnitCollectionID})
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
				diagnostics, state := cgUnitOperation(t, r, operation, cgUnitModel(), cgUnitModel())
				if failure == "verified 404" && (operation == "read" || operation == "delete") {
					if diagnostics.HasError() || operation == "read" && !state.Raw.IsNull() {
						t.Fatalf("verified parent absence: %v", diagnostics)
					}
				} else {
					cgUnitAssertFailureState(t, operation, diagnostics, state, cgUnitModel())
				}
				if parentCalls != 1 || strings.Contains(fmt.Sprint(diagnostics), groupTestKey) {
					t.Fatalf("parent response was retried or leaked a secret: %v", diagnostics)
				}
			})
		}
	}
}

func TestCollectionGroupGroupParentAbsenceNeedsCompleteAdminList(t *testing.T) {
	for _, status := range []int{403, 404} {
		for _, evidence := range []string{"absent", "present early", "present late", "list 403", "list 404", "later malformed"} {
			for _, operation := range []string{"read", "delete", "import"} {
				t.Run(fmt.Sprintf("%s/%d/%s", operation, status, evidence), func(t *testing.T) {
					groups := make([]client.Group, 101)
					for i := range groups {
						groups[i] = cgUnitGroup(uuid.NewString())
					}
					if evidence == "present early" {
						groups[0] = cgUnitGroup(cgUnitGroupID)
					}
					if evidence == "present late" {
						groups[100] = cgUnitGroup(cgUnitGroupID)
					}
					var offsets []int
					r := &collectionGroupResource{api: cgUnitClient(t, func(w http.ResponseWriter, req *http.Request) {
						switch req.URL.Path {
						case "/api/groups.info":
							cgUnitBody(t, w, req, map[string]any{"id": cgUnitGroupID})
							w.WriteHeader(status)
							groupTestEncode(t, w, map[string]any{"ok": false, "status": status, "error": "authorization_error", "message": groupTestKey})
						case "/api/groups.list":
							var body map[string]any
							if !groupTestDecode(t, w, req, &body) {
								return
							}
							offset, ok := body["offset"].(float64)
							if !ok || !reflect.DeepEqual(body, map[string]any{"offset": offset, "limit": float64(100)}) || (offset != 0 && offset != 100) {
								t.Errorf("incomplete workspace group absence check: %v", body)
								return
							}
							offsets = append(offsets, int(offset))
							if evidence == "list 403" || evidence == "list 404" {
								code := 403
								if evidence == "list 404" {
									code = 404
								}
								w.WriteHeader(code)
								groupTestEncode(t, w, map[string]any{"error": "authorization_error", "message": groupTestKey})
								return
							}
							page := map[string]any{"ok": true, "status": 200, "data": map[string]any{"groups": groups[int(offset):min(int(offset)+100, len(groups))]},
								"pagination": client.PaginationResponse{Limit: groupTestPointer(100), Offset: groupTestPointer(int(offset)), Total: groupTestPointer(101)}}
							if evidence == "later malformed" && offset == 100 {
								delete(page, "pagination")
							}
							groupTestEncode(t, w, page)
						default:
							if !cgUnitParents(t, w, req) {
								t.Errorf("unverified parent absence reached grant endpoint: %s", req.URL.Path)
							}
						}
					})}
					diagnostics, state := cgUnitOperation(t, r, operation, cgUnitModel(), cgUnitModel())
					if evidence == "absent" && operation != "import" {
						if diagnostics.HasError() || operation == "read" && !state.Raw.IsNull() {
							t.Fatalf("verified group absence: %v", diagnostics)
						}
					} else {
						cgUnitAssertFailureState(t, operation, diagnostics, state, cgUnitModel())
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
