// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/glitchedmob/terraform-provider-outline/internal/client"
	"github.com/google/uuid"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// Omission means unfiltered. Read fixtures can permit one exact fresh-name
// query, but cannot silently permit permission filters or other request fields.
func grantTestOffset(t *testing.T, w http.ResponseWriter, req *http.Request, parent string, allowedQuery ...*string) int {
	t.Helper()
	var body map[string]any
	if !groupTestDecode(t, w, req, &body) {
		return -1
	}
	offset, ok := body["offset"].(float64)
	want := map[string]any{"id": parent, "limit": float64(100), "offset": offset}
	if len(allowedQuery) == 1 && allowedQuery[0] != nil {
		if _, present := body["query"]; present {
			want["query"] = *allowedQuery[0]
		}
	}
	if !ok || offset != float64(int(offset)) || !reflect.DeepEqual(body, want) {
		t.Errorf("filtered or incomplete grant list request: %v, want %v", body, want)
		return -1
	}
	return int(offset)
}

func grantTestProtocolDiagnostics(diagnostics []*tfprotov6.Diagnostic) string {
	var messages []string
	for _, diagnostic := range diagnostics {
		messages = append(messages, diagnostic.Summary, diagnostic.Detail)
	}
	return strings.Join(messages, "\n")
}

func grantTestReadQuery(operation string, name *string) *string {
	return grantTestReadQueryFlag(operation == "read", name)
}

func grantTestReadQueryFlag(reading bool, name *string) *string {
	if reading {
		return name
	}
	return nil
}

// Terraform CLI operations can mix Read with mutation or import RPCs. Track
// the actual Read RPC rather than allowing query throughout an apply/destroy.
// These recovery fixtures each manage a single resource and issue serial RPCs.
type grantTestReadServer struct {
	tfprotov6.ProviderServer
	reading *atomic.Bool
}

func (s *grantTestReadServer) ReadResource(ctx context.Context, req *tfprotov6.ReadResourceRequest) (*tfprotov6.ReadResourceResponse, error) {
	s.reading.Store(true)
	defer s.reading.Store(false)
	return s.ProviderServer.ReadResource(ctx, req)
}

func grantTestTrackReads(reading *atomic.Bool) func(tfprotov6.ProviderServer) tfprotov6.ProviderServer {
	return func(server tfprotov6.ProviderServer) tfprotov6.ProviderServer {
		return &grantTestReadServer{ProviderServer: server, reading: reading}
	}
}

type grantSafetyProvider struct {
	provider.Provider
	api *apiClient
}

func (p *grantSafetyProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	p.Provider.Configure(ctx, req, resp)
	if !resp.Diagnostics.HasError() {
		// Keep the production resources and decoder, with the fixture's unpaced
		// client already checked by cuUnitClient.
		resp.ResourceData, resp.DataSourceData = p.api, p.api
	}
}

type grantRefreshCase struct {
	name, endpoint, parentAttribute, targetAttribute, parentID, targetID string
	membersKey, principalsKey, principalIDKey, permission                string
}

var grantRefreshCases = []grantRefreshCase{
	{"outline_collection_user", "/api/collections.memberships", "collection_id", "user_id", cuUnitCollectionID, cuUnitUserID, "memberships", "users", "userId", "read_write"},
	{"outline_collection_group", "/api/collections.group_memberships", "collection_id", "group_id", cgUnitCollectionID, cgUnitGroupID, "groupMemberships", "groups", "groupId", "read_write"},
	{"outline_group_member", "/api/groups.memberships", "group_id", "user_id", groupTestID, userTestID, "groupMemberships", "users", "userId", "admin"},
}

func (c grantRefreshCase) parents(t *testing.T, w http.ResponseWriter, req *http.Request, name *string) bool {
	t.Helper()
	switch req.URL.Path {
	case "/api/auth.info":
		groupTestEncode(t, w, userTestAuth(userTestOwner()))
	case "/api/collections.info":
		cuUnitBody(t, w, req, map[string]any{"id": c.parentID})
		groupTestEncode(t, w, collectionTestEnvelope(collectionTestCollection()))
	case "/api/users.info":
		cuUnitBody(t, w, req, map[string]any{"id": c.targetID})
		user := cuUnitParentUser(c.targetID)
		user.Name = name
		groupTestEncode(t, w, userTestEnvelope(&user))
	case "/api/groups.info":
		id := c.targetID
		if c.name == "outline_group_member" {
			id = c.parentID
		}
		cuUnitBody(t, w, req, map[string]any{"id": id})
		group := cgUnitGroup(id)
		if c.name == "outline_collection_group" {
			group.Name = name
		}
		groupTestEncode(t, w, map[string]any{"ok": true, "status": 200, "data": group})
	default:
		return false
	}
	return true
}

// All rows have the same public name. Identity must come from the exact UUID,
// not name equality, row order, permission, or a grant's own API ID.
func (c grantRefreshCase) page(t *testing.T, offset, total, target int) map[string]any {
	t.Helper()
	count := min(100, total-offset)
	var envelope map[string]any
	ids := make([]string, count)
	for i := range ids {
		ids[i] = uuid.NewSHA1(uuid.NameSpaceOID, []byte(fmt.Sprintf("%s/%d", c.name, offset+i))).String()
		if offset+i == target {
			ids[i] = c.targetID
		}
	}
	switch c.name {
	case "outline_collection_user":
		members := make([]client.Membership, count)
		for i, id := range ids {
			members[i] = cuUnitGrant(id, client.PermissionAdmin)
			members[i].Id = groupTestPointer(uuid.NewSHA1(uuid.NameSpaceOID, []byte("grant/"+id)).String())
			if offset+i == target {
				members[i].Permission = groupTestPointer(client.Permission(c.permission))
			}
		}
		envelope = cuUnitEnvelope(members, offset, total, false)
	case "outline_collection_group":
		members := make([]client.GroupMembership, count)
		for i, id := range ids {
			members[i] = cgUnitGrant(id, client.PermissionAdmin)
			members[i].Id = groupTestPointer(uuid.NewSHA1(uuid.NameSpaceOID, []byte("grant/"+id)).String())
			if offset+i == target {
				members[i].Permission = groupTestPointer(client.Permission(c.permission))
			}
		}
		envelope = cgUnitEnvelope(members, offset, total, false)
	case "outline_group_member":
		members := make([]client.GroupUser, count)
		for i, id := range ids {
			members[i] = memberTestMember(id, client.GroupPermissionMember)
			if offset+i == target {
				members[i].Permission = groupTestPointer(client.GroupPermission(c.permission))
			}
		}
		envelope = memberTestEnvelope(members, offset, total, false)
	}
	page := cuUnitJSONMap(t, envelope)
	data := page["data"].(map[string]any)
	for _, user := range data[c.principalsKey].([]any) {
		user.(map[string]any)["name"] = "Same name"
	}
	if c.name == "outline_group_member" {
		for _, member := range data[c.membersKey].([]any) {
			member.(map[string]any)["user"].(map[string]any)["name"] = "Same name"
		}
	}
	return page
}

func (c grantRefreshCase) reader(t *testing.T, api *apiClient) (func() (*tfprotov6.ReadResourceResponse, tftypes.Value), tftypes.Value) {
	t.Helper()
	server := providerserver.NewProtocol6(&grantSafetyProvider{Provider: New("unit")(), api: api})()
	configured, err := server.ConfigureProvider(t.Context(), &tfprotov6.ConfigureProviderRequest{
		Config: testProtocolConfig(t, map[string]any{"base_url": api.baseURL, "api_key": groupTestKey}), TerraformVersion: "1.14.5",
	})
	if err != nil || protocolHasError(configured.Diagnostics) {
		t.Fatalf("configure: %v %v", configured, err)
	}
	attrs := map[string]tftypes.Type{"id": tftypes.String, c.parentAttribute: tftypes.String, c.targetAttribute: tftypes.String, "permission": tftypes.String}
	permission := "read"
	if c.name == "outline_group_member" {
		permission = "member"
	}
	values := map[string]string{"id": c.parentID + "/" + c.targetID, c.parentAttribute: c.parentID, c.targetAttribute: c.targetID, "permission": permission}
	fields := make(map[string]tftypes.Value)
	for key, value := range values {
		fields[key] = tftypes.NewValue(tftypes.String, value)
	}
	prior := tftypes.NewValue(tftypes.Object{AttributeTypes: attrs}, fields)
	dynamic, err := tfprotov6.NewDynamicValue(prior.Type(), prior)
	if err != nil {
		t.Fatal(err)
	}
	return func() (*tfprotov6.ReadResourceResponse, tftypes.Value) {
		resp, err := server.ReadResource(t.Context(), &tfprotov6.ReadResourceRequest{TypeName: c.name, CurrentState: &dynamic})
		if err != nil || resp == nil || resp.NewState == nil {
			t.Fatalf("ReadResource: %v %v", resp, err)
		}
		state, err := resp.NewState.Unmarshal(prior.Type())
		if err != nil {
			t.Fatal(err)
		}
		return resp, state
	}, prior
}

func (c grantRefreshCase) assertPresent(t *testing.T, resp *tfprotov6.ReadResourceResponse, state, prior tftypes.Value) {
	t.Helper()
	var want map[string]tftypes.Value
	if err := prior.As(&want); err != nil {
		t.Fatal(err)
	}
	want["permission"] = tftypes.NewValue(tftypes.String, c.permission)
	if protocolHasError(resp.Diagnostics) || !state.Equal(tftypes.NewValue(prior.Type(), want)) {
		t.Fatalf("ReadResource adopted a different pair or lost permission drift: %v state=%v", resp.Diagnostics, state)
	}
}

func TestGrantRefreshQueryNameSelection(t *testing.T) {
	for _, test := range []struct {
		label string
		name  *string
	}{
		{"nil", nil}, {"empty", groupTestPointer("")}, {"spaces", groupTestPointer(" \t\n\r")},
		{"unicode whitespace", groupTestPointer("\u2003\u00a0")}, {"NUL", groupTestPointer("Name\x00suffix")},
		{"invalid UTF-8", groupTestPointer(string([]byte{'n', 0xff}))},
	} {
		t.Run(test.label, func(t *testing.T) {
			if query := membershipRefreshQuery(test.name); query != nil {
				t.Fatalf("unusable name became query %q", *query)
			}
		})
	}
	for _, name := range []string{"Same name", "  Renamed_%\\日本語  ", "O'Reilly", "\ufffd"} {
		if query := membershipRefreshQuery(&name); query == nil || *query != name {
			t.Fatalf("query changed the fresh name %q: %v", name, query)
		}
	}
}

func TestGrantRefreshProtocolExactUUIDAndFreshName(t *testing.T) {
	for _, c := range grantRefreshCases {
		t.Run(c.name, func(t *testing.T) {
			name := "Same name"
			var offsets []int
			api := cuUnitClient(t, func(w http.ResponseWriter, req *http.Request) {
				if c.parents(t, w, req, &name) {
					return
				}
				if req.URL.Path != c.endpoint {
					t.Errorf("refresh mutated or listed a parent: %s", req.URL.Path)
					w.WriteHeader(500)
					return
				}
				offset := len(offsets) % 2 * 100
				cuUnitBody(t, w, req, map[string]any{"id": c.parentID, "limit": float64(100), "offset": float64(offset), "query": name})
				offsets = append(offsets, offset)
				// A same-name row on page one is not the target. The server may
				// also ignore query entirely; every returned page still matters.
				groupTestEncode(t, w, c.page(t, offset, 101, 100))
			})
			read, prior := c.reader(t, api)
			resp, state := read()
			c.assertPresent(t, resp, state, prior)
			name = "  Renamed_%\\日本語  "
			resp, state = read()
			c.assertPresent(t, resp, state, prior)
			if !reflect.DeepEqual(offsets, []int{0, 100, 0, 100}) {
				t.Fatalf("filtered scan skipped pages or used cached results: %v", offsets)
			}
		})
	}
}

func TestGrantRefreshProtocolFilteredMissNeedsFullFallback(t *testing.T) {
	for _, c := range grantRefreshCases {
		for _, test := range []struct{ target, filteredTotal int }{{-1, 0}, {0, 0}, {200, 0}, {-1, 101}, {0, 101}, {200, 101}} {
			target, filteredTotal := test.target, test.filteredTotal
			t.Run(fmt.Sprintf("%s/target=%d/filtered=%d", c.name, target, filteredTotal), func(t *testing.T) {
				filteredPages := max(1, (filteredTotal+99)/100)
				name := "Name before concurrent rename"
				calls := 0
				api := cuUnitClient(t, func(w http.ResponseWriter, req *http.Request) {
					if c.parents(t, w, req, &name) {
						return
					}
					if req.URL.Path != c.endpoint || calls >= filteredPages+3 {
						t.Errorf("unexpected fallback endpoint or retry: %s calls=%d", req.URL.Path, calls)
						w.WriteHeader(500)
						return
					}
					filtered := calls < filteredPages
					offset := calls * 100
					want := map[string]any{"id": c.parentID, "limit": float64(100)}
					if filtered {
						want["query"] = name
					} else {
						offset = (calls - filteredPages) * 100
					}
					want["offset"] = float64(offset)
					cuUnitBody(t, w, req, want)
					calls++
					if filtered {
						groupTestEncode(t, w, c.page(t, offset, filteredTotal, -1))
					} else {
						// The target now has a different name, or really is absent.
						groupTestEncode(t, w, c.page(t, offset, 201, target))
					}
				})
				read, prior := c.reader(t, api)
				resp, state := read()
				if target == -1 {
					if protocolHasError(resp.Diagnostics) || !state.IsNull() {
						t.Fatalf("complete fallback did not prove absence: %v %v", resp.Diagnostics, state)
					}
				} else {
					c.assertPresent(t, resp, state, prior)
				}
				if calls != filteredPages+3 {
					t.Fatalf("filtered miss skipped complete unfiltered fallback: %d", calls)
				}
			})
		}
	}
}

func TestGrantRefreshProtocolUnusableNamesStayUnfiltered(t *testing.T) {
	for _, c := range grantRefreshCases {
		for _, name := range []string{"", " \t\n", "\u2003\u00a0", "Name\x00suffix"} {
			t.Run(fmt.Sprintf("%s/%q", c.name, name), func(t *testing.T) {
				calls := 0
				api := cuUnitClient(t, func(w http.ResponseWriter, req *http.Request) {
					if c.parents(t, w, req, &name) {
						return
					}
					if req.URL.Path != c.endpoint || calls > 1 {
						t.Errorf("unexpected unfiltered request: %s", req.URL.Path)
						w.WriteHeader(500)
						return
					}
					offset := calls * 100
					cuUnitBody(t, w, req, map[string]any{"id": c.parentID, "limit": float64(100), "offset": float64(offset)})
					calls++
					groupTestEncode(t, w, c.page(t, offset, 101, 0))
				})
				read, prior := c.reader(t, api)
				resp, state := read()
				c.assertPresent(t, resp, state, prior)
				if calls != 2 {
					t.Fatalf("unusable name skipped full scan: %d", calls)
				}
			})
		}
		// A nil name is also unusable for query, but parent presenters already
		// require it. Keep that stricter safety contract rather than weakening it.
		t.Run(c.name+"/missing parent name", func(t *testing.T) {
			api := cuUnitClient(t, func(w http.ResponseWriter, req *http.Request) {
				if !c.parents(t, w, req, nil) {
					t.Errorf("malformed parent reached membership endpoint: %s", req.URL.Path)
					w.WriteHeader(500)
				}
			})
			read, prior := c.reader(t, api)
			resp, state := read()
			if !protocolHasError(resp.Diagnostics) || !state.Equal(prior) {
				t.Fatalf("missing parent name changed trusted state: %v %v", resp.Diagnostics, state)
			}
		})
	}
}

func TestGrantRefreshProtocolFilteredFailureNeverFallsBack(t *testing.T) {
	for _, c := range grantRefreshCases {
		failures := []string{"transport", "invalid JSON", "missing JSON", "empty HTTP200", "400", "403", "404", "429", "500", "false ok", "wrong status", "null data", "missing members", "missing principals", "wrong principal", "invalid principal", "missing principal name", "missing permission", "invalid permission", "missing identity", "invalid identity", "wrong parent", "missing pagination", "wrong offset", "invalid limit", "short page", "duplicate", "invalid nested user"}
		if c.name != "outline_group_member" {
			failures = append(failures, "document grant", "inherited grant")
		}
		for _, failure := range failures {
			t.Run(c.name+"/"+failure, func(t *testing.T) {
				name := "Same name"
				var calls atomic.Int32
				api := cuUnitClient(t, func(w http.ResponseWriter, req *http.Request) {
					if c.parents(t, w, req, &name) {
						return
					}
					call := calls.Add(1)
					if req.URL.Path != c.endpoint || call != 1 {
						t.Errorf("filtered failure caused fallback, mutation, or parent absence lookup: %s calls=%d", req.URL.Path, call)
						w.WriteHeader(500)
						return
					}
					cuUnitBody(t, w, req, map[string]any{"id": c.parentID, "limit": float64(100), "offset": float64(0), "query": name})
					page := c.page(t, 0, 2, 0)
					data := page["data"].(map[string]any)
					// Poison the row after the exact target, not the target itself.
					member := data[c.membersKey].([]any)[1].(map[string]any)
					principal := data[c.principalsKey].([]any)[1].(map[string]any)
					pagination := page["pagination"].(map[string]any)
					switch failure {
					case "transport":
						conn, _, err := w.(http.Hijacker).Hijack()
						if err != nil {
							t.Errorf("hijack: %v", err)
							return
						}
						_ = conn.Close()
						return
					case "invalid JSON":
						groupTestWrite(t, w, "{bad")
						return
					case "missing JSON":
						w.Header().Set("Content-Type", "text/plain")
					case "empty HTTP200":
						return
					case "400", "403", "404", "429", "500":
						status := map[string]int{"400": 400, "403": 403, "404": 404, "429": 429, "500": 500}[failure]
						w.WriteHeader(status)
						page = map[string]any{"ok": false, "status": status, "error": "not_found", "message": groupTestKey}
					case "false ok":
						page["ok"] = false
					case "wrong status":
						page["status"] = 201
					case "null data":
						page["data"] = nil
					case "missing members":
						delete(data, c.membersKey)
					case "missing principals":
						delete(data, c.principalsKey)
					case "wrong principal":
						principal["id"] = uuid.NewString()
					case "invalid principal":
						principal["id"] = uuid.Nil.String()
					case "missing principal name":
						delete(principal, "name")
					case "missing permission":
						delete(member, "permission")
					case "invalid permission":
						member["permission"] = "owner"
					case "missing identity":
						delete(member, "id")
					case "invalid identity":
						member["id"] = "not-a-grant-id"
					case "wrong parent":
						key := "collectionId"
						if c.name == "outline_group_member" {
							key = "groupId"
						}
						member[key] = uuid.NewString()
					case "missing pagination":
						delete(page, "pagination")
					case "wrong offset":
						pagination["offset"] = 100
					case "invalid limit":
						pagination["limit"] = 101
					case "short page":
						pagination["total"] = 3
					case "duplicate":
						first := data[c.membersKey].([]any)[0].(map[string]any)
						member[c.principalIDKey] = first[c.principalIDKey]
						if c.name == "outline_group_member" {
							member["id"], member["user"] = first["id"], first["user"]
						}
					case "invalid nested user":
						if c.name == "outline_group_member" {
							member["user"].(map[string]any)["id"] = uuid.NewString()
						} else {
							delete(principal, "id")
						}
					case "document grant":
						member["documentId"] = uuid.NewString()
					case "inherited grant":
						member["sourceId"] = uuid.NewString()
					}
					groupTestEncode(t, w, page)
				})
				read, prior := c.reader(t, api)
				resp, state := read()
				if !protocolHasError(resp.Diagnostics) || !state.Equal(prior) || calls.Load() != 1 {
					t.Fatalf("filtered failure proved absence or changed state: %v state=%v calls=%d", resp.Diagnostics, state, calls.Load())
				}
				if strings.Contains(grantTestProtocolDiagnostics(resp.Diagnostics), groupTestKey) {
					t.Fatal("diagnostics leaked credentials")
				}
			})
		}
	}
}

func TestGrantRefreshProtocolBroadQueryResponseValidatesAllPages(t *testing.T) {
	for _, c := range grantRefreshCases {
		for _, result := range []string{"valid", "late conflicting duplicate", "changed total"} {
			t.Run(c.name+"/"+result, func(t *testing.T) {
				name := "A narrow query the server ignores"
				var offsets []int
				api := cuUnitClient(t, func(w http.ResponseWriter, req *http.Request) {
					if c.parents(t, w, req, &name) {
						return
					}
					if req.URL.Path != c.endpoint || len(offsets) >= 3 {
						t.Errorf("unexpected fallback, mutation, or retry: %s offsets=%v", req.URL.Path, offsets)
						w.WriteHeader(500)
						return
					}
					offset := len(offsets) * 100
					cuUnitBody(t, w, req, map[string]any{"id": c.parentID, "limit": float64(100), "offset": float64(offset), "query": name})
					offsets = append(offsets, offset)
					page := c.page(t, offset, 201, 0)
					if result == "changed total" && offset == 100 {
						// The page itself is complete; only the cross-page total is bad.
						page = c.page(t, offset, 202, 0)
					}
					if result == "late conflicting duplicate" && offset == 200 {
						page = c.page(t, offset, 201, 200)
						member := page["data"].(map[string]any)[c.membersKey].([]any)[0].(map[string]any)
						if c.name == "outline_group_member" {
							member["permission"] = "member"
						} else {
							// A second grant row for the same pair can have its own UUID
							// and permission. The first-page match must not hide it.
							member["id"], member["permission"] = uuid.NewString(), "read"
						}
					}
					groupTestEncode(t, w, page)
				})
				read, prior := c.reader(t, api)
				resp, state := read()
				wantOffsets := []int{0, 100, 200}
				if result == "valid" {
					c.assertPresent(t, resp, state, prior)
				} else {
					if !protocolHasError(resp.Diagnostics) || !state.Equal(prior) {
						t.Fatalf("first-page match hid a later unsafe page: %v state=%v", resp.Diagnostics, state)
					}
					message := "duplicate"
					if result == "changed total" {
						message = "total changed during pagination"
						wantOffsets = []int{0, 100}
					}
					if !strings.Contains(grantTestProtocolDiagnostics(resp.Diagnostics), message) {
						t.Fatalf("missing %q diagnostic: %v", message, resp.Diagnostics)
					}
				}
				if !reflect.DeepEqual(offsets, wantOffsets) {
					t.Fatalf("broad query response skipped pages or fell back: %v, want %v", offsets, wantOffsets)
				}
			})
		}
	}
}

func TestGrantRefreshProtocolUnsafeFallbackRetainsState(t *testing.T) {
	for _, c := range grantRefreshCases {
		for _, failure := range []string{"malformed", "400", "403", "404"} {
			t.Run(c.name+"/"+failure, func(t *testing.T) {
				name := "Same name"
				calls := 0
				api := cuUnitClient(t, func(w http.ResponseWriter, req *http.Request) {
					if c.parents(t, w, req, &name) {
						return
					}
					if req.URL.Path != c.endpoint || calls >= 3 {
						t.Errorf("unexpected fallback request: %s calls=%d", req.URL.Path, calls)
						w.WriteHeader(500)
						return
					}
					want := map[string]any{"id": c.parentID, "limit": float64(100), "offset": float64(0)}
					switch calls {
					case 0:
						want["query"] = name
					case 2:
						want["offset"] = float64(100)
					}
					cuUnitBody(t, w, req, want)
					calls++
					switch calls {
					case 1:
						groupTestEncode(t, w, c.page(t, 0, 0, -1))
					case 2:
						groupTestEncode(t, w, c.page(t, 0, 101, 0))
					case 3:
						page := c.page(t, 100, 101, -1)
						if failure == "malformed" {
							delete(page, "pagination")
						} else {
							status := map[string]int{"400": 400, "403": 403, "404": 404}[failure]
							w.WriteHeader(status)
							page = map[string]any{"ok": false, "status": status, "error": "not_found"}
						}
						groupTestEncode(t, w, page)
					}
				})
				read, prior := c.reader(t, api)
				resp, state := read()
				if !protocolHasError(resp.Diagnostics) || !state.Equal(prior) || calls != 3 {
					t.Fatalf("unsafe fallback changed state or proved absence: %v state=%v calls=%d", resp.Diagnostics, state, calls)
				}
			})
		}
	}
}
