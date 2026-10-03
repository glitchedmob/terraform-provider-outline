// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/glitchedmob/terraform-provider-outline/internal/client"
	"github.com/google/uuid"
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-plugin"
	"github.com/hashicorp/terraform-exec/tfexec"
	fwprovider "github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6/tf6server"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"github.com/oapi-codegen/nullable"
)

const grantRefreshScaleTargets = 1000
const grantRefreshScaleURL = "http://grant-refresh.invalid/api"

type grantRefreshScaleCase struct {
	typeName, listPath, parentField, targetField, parentID, permission string
	authCalls                                                          int
}

var grantRefreshScaleCases = []grantRefreshScaleCase{
	{"outline_collection_user", "/api/collections.memberships", "collection_id", "user_id", collectionTestID, "read", 2},
	{"outline_collection_group", "/api/collections.group_memberships", "collection_id", "group_id", collectionTestID, "read", 2},
	{"outline_group_member", "/api/groups.memberships", "group_id", "user_id", groupTestID, "member", 1},
}

func grantRefreshScaleID(i int) string {
	return fmt.Sprintf("%08x-1234-4234-8234-123456789abc", i+1)
}

func grantRefreshScaleName(i int) string {
	// Query is a literal name, including whitespace and SQL LIKE metacharacters.
	return fmt.Sprintf(" Scale %%_\\ person %08d ", i)
}

func grantRefreshScaleEncode(v any) []byte {
	body, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return body
}

func (c grantRefreshScaleCase) attributes(i int) map[string]string {
	return map[string]string{
		"id": c.parentID + "/" + grantRefreshScaleID(i), c.parentField: c.parentID,
		c.targetField: grantRefreshScaleID(i), "permission": c.permission,
	}
}

func (c grantRefreshScaleCase) targetPath() string {
	if c.targetField == "group_id" {
		return "/api/groups.info"
	}
	return "/api/users.info"
}

func (c grantRefreshScaleCase) parentPath() string {
	if c.parentField == "collection_id" {
		return "/api/collections.info"
	}
	return "/api/groups.info"
}

func grantRefreshScaleTarget(c grantRefreshScaleCase, i int, name string) []byte {
	if c.targetField == "group_id" {
		group := cgUnitGroup(grantRefreshScaleID(i))
		group.Name = &name
		return grantRefreshScaleEncode(groupTestEnvelope(&group))
	}
	user := cuUnitParentUser(grantRefreshScaleID(i))
	user.Name = &name
	return grantRefreshScaleEncode(userTestEnvelope(&user))
}

func grantRefreshScalePage(c grantRefreshScaleCase, first, end, offset, total int, name func(int) string, permission string) map[string]any {
	switch c.typeName {
	case "outline_collection_user":
		members := make([]client.Membership, 0, end-first)
		for i := first; i < end; i++ {
			member := cuUnitGrant(grantRefreshScaleID(i), client.Permission(permission))
			member.Id = groupTestPointer(grantRefreshScaleID(100000 + i))
			members = append(members, member)
		}
		page := cuUnitEnvelope(members, offset, total, false)
		for i, user := range page["data"].(map[string]any)["users"].([]client.User) {
			*user.Name = name(first + i)
		}
		return page
	case "outline_collection_group":
		members := make([]client.GroupMembership, 0, end-first)
		for i := first; i < end; i++ {
			member := cgUnitGrant(grantRefreshScaleID(i), client.Permission(permission))
			member.Id = groupTestPointer(grantRefreshScaleID(100000 + i))
			members = append(members, member)
		}
		page := cgUnitEnvelope(members, offset, total, false)
		for i, group := range page["data"].(map[string]any)["groups"].([]client.Group) {
			*group.Name = name(first + i)
		}
		return page
	case "outline_group_member":
		members := make([]client.GroupUser, 0, end-first)
		for i := first; i < end; i++ {
			member := memberTestMember(grantRefreshScaleID(i), client.GroupPermission(permission))
			member.User.Name = groupTestPointer(name(i))
			members = append(members, member)
		}
		return memberTestEnvelope(members, offset, total, false)
	default:
		panic("unknown grant resource: " + c.typeName)
	}
}

// Preencoded mock responses keep these tests about request counts, not fixture
// construction. The production transport, generated decoder and validators run.
// No Outline socket is opened; only request pacing is disabled.
type grantRefreshScaleFixture struct {
	mu        sync.Mutex
	resource  grantRefreshScaleCase
	responses map[string][]byte
	pages     map[int][]byte
	filtered  map[string]map[int][]byte
	counts    map[string]int
	queries   map[string]map[int]int
}

func newGrantRefreshScaleFixture(c grantRefreshScaleCase, sameName bool) *grantRefreshScaleFixture {
	f := &grantRefreshScaleFixture{
		resource: c, responses: make(map[string][]byte), pages: make(map[int][]byte),
		filtered: make(map[string]map[int][]byte), counts: make(map[string]int), queries: make(map[string]map[int]int),
	}
	f.responses["/api/auth.info"] = grantRefreshScaleEncode(userTestAuth(userTestOwner()))
	f.responses["/api/collections.info:"+collectionTestID] = grantRefreshScaleEncode(collectionTestEnvelope(collectionTestCollection()))
	f.responses["/api/groups.info:"+groupTestID] = grantRefreshScaleEncode(groupTestEnvelope(groupTestGroup()))
	name := grantRefreshScaleName
	if sameName {
		name = func(int) string { return grantRefreshScaleName(0) }
	}
	for i := 0; i < grantRefreshScaleTargets; i++ {
		f.responses[c.targetPath()+":"+grantRefreshScaleID(i)] = grantRefreshScaleTarget(c, i, name(i))
		if !sameName {
			f.filtered[name(i)] = map[int][]byte{0: grantRefreshScaleEncode(grantRefreshScalePage(c, i, i+1, 0, 1, name, c.permission))}
		}
	}
	for offset := 0; offset < grantRefreshScaleTargets; offset += 100 {
		f.pages[offset] = grantRefreshScaleEncode(grantRefreshScalePage(c, offset, offset+100, offset, grantRefreshScaleTargets, name, c.permission))
	}
	if sameName {
		f.filtered[name(0)] = f.pages
	}
	return f
}

func (f *grantRefreshScaleFixture) RoundTrip(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.counts[req.URL.Path]++
	if req.Method != http.MethodPost || req.URL.Scheme != "http" || req.URL.Host != "grant-refresh.invalid" || req.URL.RawQuery != "" ||
		req.Header.Get("Authorization") != "Bearer "+groupTestKey ||
		(req.URL.Path != "/api/auth.info" && req.Header.Get("Content-Type") != "application/json") {
		return nil, fmt.Errorf("unexpected request or headers: %s %s", req.Method, req.URL)
	}
	var raw []byte
	var err error
	if req.Body != nil {
		raw, err = io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
	}
	var fields map[string]json.RawMessage
	if len(raw) != 0 {
		if err := json.Unmarshal(raw, &fields); err != nil {
			return nil, err
		}
	}
	var body []byte
	switch req.URL.Path {
	case f.resource.listPath:
		var id uuid.UUID
		var limit, offset *int
		var query *string
		var hasPermission bool
		// Decode the generated request models, including the endpoint-specific
		// permission type. Also reject null permission and unknown filter keys.
		switch f.resource.typeName {
		case "outline_collection_user":
			var request client.CollectionsMembershipsJSONRequestBody
			err = json.Unmarshal(raw, &request)
			id, limit, offset, query, hasPermission = request.Id, request.Limit, request.Offset, request.Query, request.Permission != nil
		case "outline_collection_group":
			var request client.CollectionsGroupMembershipsJSONRequestBody
			err = json.Unmarshal(raw, &request)
			id, limit, offset, query, hasPermission = request.Id, request.Limit, request.Offset, request.Query, request.Permission != nil
		case "outline_group_member":
			var request client.GroupsMembershipsJSONRequestBody
			err = json.Unmarshal(raw, &request)
			id, limit, offset, query, hasPermission = request.Id, request.Limit, request.Offset, request.Query, request.Permission != nil
		}
		_, permissionKey := fields["permission"]
		wantFields := 3
		if query != nil {
			wantFields++
		}
		if err != nil || id.String() != f.resource.parentID || limit == nil || *limit != 100 || offset == nil || *offset < 0 || *offset%100 != 0 ||
			hasPermission || permissionKey || len(fields) != wantFields {
			return nil, fmt.Errorf("unexpected %s body %s: %v", req.URL.Path, raw, err)
		}
		key := ""
		body = f.pages[*offset]
		if query != nil {
			key = *query
			body = f.filtered[key][*offset]
		}
		if f.queries[key] == nil {
			f.queries[key] = make(map[int]int)
		}
		f.queries[key][*offset]++
	case "/api/auth.info":
		body = f.responses[req.URL.Path]
	default:
		var id string
		if err := json.Unmarshal(fields["id"], &id); err != nil || len(fields) != 1 {
			return nil, fmt.Errorf("unexpected parent request %s: %s", req.URL.Path, raw)
		}
		body = f.responses[req.URL.Path+":"+id]
	}
	if body == nil {
		return nil, fmt.Errorf("unexpected mock request %s: %s", req.URL.Path, raw)
	}
	return &http.Response{
		StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(bytes.NewReader(body)), Request: req,
	}, nil
}

func (f *grantRefreshScaleFixture) resetCounts() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.counts, f.queries = make(map[string]int), make(map[string]map[int]int)
}

func grantRefreshScaleAssertCounts(t *testing.T, f *grantRefreshScaleFixture, reads, pages int) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.resource
	want := map[string]int{"/api/auth.info": reads * c.authCalls, c.parentPath(): reads, c.targetPath(): reads, c.listPath: pages}
	if !reflect.DeepEqual(f.counts, want) {
		t.Fatalf("request counts = %v, want %v", f.counts, want)
	}
	total := 0
	for _, count := range f.counts {
		total += count
	}
	t.Logf("%s: managed=%d membership_pages=%d total_requests=%d calls=%v", c.typeName, reads, pages, total, f.counts)
}

// Keep the production resource registry and Read methods. Configure changes
// only the underlying HTTP transport, after checking the production rate limit.
type grantRefreshScaleProvider struct {
	*OutlineProvider
	fixture *grantRefreshScaleFixture
	mu      sync.Mutex
	clients map[*apiClient]bool
	configs int
}

func (p *grantRefreshScaleProvider) Configure(ctx context.Context, req fwprovider.ConfigureRequest, resp *fwprovider.ConfigureResponse) {
	p.OutlineProvider.Configure(ctx, req, resp)
	if resp.Diagnostics.HasError() {
		return
	}
	api := resp.ResourceData.(*apiClient)
	transport := api.httpClient.Transport.(*bearerTransport)
	if transport.limiter == nil || transport.limiter.Limit() != 5 || transport.limiter.Burst() != 1 {
		resp.Diagnostics.AddError("Unexpected production pacing", "Expected the shared 5 requests/second limiter with burst 1 before disabling pacing in the mock.")
		return
	}
	transport.base, transport.limiter = p.fixture, nil
	p.mu.Lock()
	defer p.mu.Unlock()
	p.configs++
	p.clients[api] = true
}

func newGrantRefreshScaleProvider(f *grantRefreshScaleFixture) *grantRefreshScaleProvider {
	return &grantRefreshScaleProvider{OutlineProvider: &OutlineProvider{version: "grant-refresh-test"}, fixture: f, clients: make(map[*apiClient]bool)}
}

func grantRefreshScaleAssertClients(t *testing.T, p *grantRefreshScaleProvider, want int) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.configs != want || len(p.clients) != want {
		t.Fatalf("provider configurations=%d distinct clients=%d, want %d", p.configs, len(p.clients), want)
	}
}

func grantRefreshScaleDynamic(t *testing.T, attributes map[string]string) *tfprotov6.DynamicValue {
	t.Helper()
	fields, attrs := make(map[string]tftypes.Value), make(map[string]tftypes.Type)
	for name, value := range attributes {
		attrs[name], fields[name] = tftypes.String, tftypes.NewValue(tftypes.String, value)
	}
	typ := tftypes.Object{AttributeTypes: attrs}
	dynamic, err := tfprotov6.NewDynamicValue(typ, tftypes.NewValue(typ, fields))
	if err != nil {
		t.Fatal(err)
	}
	return &dynamic
}

func grantRefreshScaleAssertState(t *testing.T, got *tfprotov6.DynamicValue, want map[string]string) {
	t.Helper()
	if got == nil {
		t.Fatal("ReadResource returned no state")
	}
	attrs := make(map[string]tftypes.Type)
	for name := range want {
		attrs[name] = tftypes.String
	}
	typ := tftypes.Object{AttributeTypes: attrs}
	value, err := got.Unmarshal(typ)
	if err != nil {
		t.Fatal(err)
	}
	expected, err := grantRefreshScaleDynamic(t, want).Unmarshal(typ)
	if err != nil || !value.Equal(expected) {
		t.Fatalf("read state = %s, want %v: %v", value, want, err)
	}
}

func grantRefreshScaleProtocol(t *testing.T, f *grantRefreshScaleFixture) (tfprotov6.ProviderServer, *grantRefreshScaleProvider) {
	t.Helper()
	p := newGrantRefreshScaleProvider(f)
	server := providerserver.NewProtocol6(p)()
	response, err := server.ConfigureProvider(t.Context(), &tfprotov6.ConfigureProviderRequest{
		Config: testProtocolConfig(t, map[string]any{"base_url": grantRefreshScaleURL, "api_key": groupTestKey}), TerraformVersion: "1.14.5",
	})
	if err != nil || response == nil || protocolHasError(response.Diagnostics) {
		t.Fatalf("configure: %v %v", response, err)
	}
	return server, p
}

func TestGrantRefreshScaleProtocol(t *testing.T) {
	for _, c := range grantRefreshScaleCases {
		for _, sameName := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/same_name=%t", c.typeName, sameName), func(t *testing.T) {
				f := newGrantRefreshScaleFixture(c, sameName)
				server, p := grantRefreshScaleProtocol(t, f)
				for i := 0; i < grantRefreshScaleTargets; i++ {
					attributes := c.attributes(i)
					response, err := server.ReadResource(t.Context(), &tfprotov6.ReadResourceRequest{TypeName: c.typeName, CurrentState: grantRefreshScaleDynamic(t, attributes)})
					if err != nil || response == nil || protocolHasError(response.Diagnostics) {
						t.Fatalf("read %d: %v %v", i, response, err)
					}
					grantRefreshScaleAssertState(t, response.NewState, attributes)
				}
				pages := grantRefreshScaleTargets
				if sameName {
					pages *= 10
				}
				grantRefreshScaleAssertCounts(t, f, grantRefreshScaleTargets, pages)
				grantRefreshScaleAssertClients(t, p, 1)
				f.mu.Lock()
				defer f.mu.Unlock()
				want := make(map[string]map[int]int)
				if sameName {
					want[grantRefreshScaleName(0)] = make(map[int]int)
					for offset := 0; offset < grantRefreshScaleTargets; offset += 100 {
						want[grantRefreshScaleName(0)][offset] = grantRefreshScaleTargets
					}
				} else {
					for i := 0; i < grantRefreshScaleTargets; i++ {
						want[grantRefreshScaleName(i)] = map[int]int{0: 1}
					}
				}
				if !reflect.DeepEqual(f.queries, want) {
					t.Fatal("reads did not scan exactly the expected filtered page sets")
				}
			})
		}
	}
}

func TestGrantRefreshScaleFilteredMissNeedsFullScan(t *testing.T) {
	for _, c := range grantRefreshScaleCases {
		for _, scenario := range []string{"stale-name", "absent", "empty-name"} {
			t.Run(c.typeName+"/"+scenario, func(t *testing.T) {
				f := newGrantRefreshScaleFixture(c, false)
				index, pages := 0, 11
				name := grantRefreshScaleName(index)
				if scenario == "absent" {
					index = grantRefreshScaleTargets
					name = grantRefreshScaleName(index)
				}
				if scenario == "empty-name" {
					name, pages = "", 10
				}
				f.responses[c.targetPath()+":"+grantRefreshScaleID(index)] = grantRefreshScaleTarget(c, index, name)
				f.filtered[name] = map[int][]byte{0: grantRefreshScaleEncode(grantRefreshScalePage(c, 0, 0, 0, 0, grantRefreshScaleName, c.permission))}
				server, p := grantRefreshScaleProtocol(t, f)
				attributes := c.attributes(index)
				response, err := server.ReadResource(t.Context(), &tfprotov6.ReadResourceRequest{TypeName: c.typeName, CurrentState: grantRefreshScaleDynamic(t, attributes)})
				if err != nil || response == nil || protocolHasError(response.Diagnostics) || response.NewState == nil {
					t.Fatalf("read: %v %v", response, err)
				}
				if scenario == "absent" {
					attrs := map[string]tftypes.Type{}
					for key := range attributes {
						attrs[key] = tftypes.String
					}
					value, err := response.NewState.Unmarshal(tftypes.Object{AttributeTypes: attrs})
					if err != nil || !value.IsNull() {
						t.Fatalf("verified absence did not remove state: %v %v", value, err)
					}
				} else {
					grantRefreshScaleAssertState(t, response.NewState, attributes)
				}
				grantRefreshScaleAssertCounts(t, f, 1, pages)
				grantRefreshScaleAssertClients(t, p, 1)
				want := map[string]map[int]int{"": {}}
				for offset := 0; offset < grantRefreshScaleTargets; offset += 100 {
					want[""][offset] = 1
				}
				if scenario != "empty-name" {
					want[name] = map[int]int{0: 1}
				}
				if !reflect.DeepEqual(f.queries, want) {
					t.Fatalf("filtered miss did not scan the complete unfiltered inventory: %v", f.queries)
				}
			})
		}
	}
}

func TestGrantRefreshScaleRepeatedReadsStayFresh(t *testing.T) {
	for _, c := range grantRefreshScaleCases {
		t.Run(c.typeName, func(t *testing.T) {
			f := newGrantRefreshScaleFixture(c, false)
			server, p := grantRefreshScaleProtocol(t, f)
			prior := c.attributes(0)
			read := func(wantError bool, want map[string]string) {
				t.Helper()
				response, err := server.ReadResource(t.Context(), &tfprotov6.ReadResourceRequest{TypeName: c.typeName, CurrentState: grantRefreshScaleDynamic(t, prior)})
				if err != nil || response == nil || protocolHasError(response.Diagnostics) != wantError {
					t.Fatalf("read, want error=%t: %v %v", wantError, response, err)
				}
				grantRefreshScaleAssertState(t, response.NewState, want)
			}
			read(false, prior)
			read(false, prior)
			grantRefreshScaleAssertCounts(t, f, 2, 2)
			f.resetCounts()

			// A later RPC must use the new name and permission, not a cached
			// parent or grant from either successful read above.
			name := " Renamed %_\\ target "
			f.responses[c.targetPath()+":"+grantRefreshScaleID(0)] = grantRefreshScaleTarget(c, 0, name)
			f.filtered[name] = map[int][]byte{0: grantRefreshScaleEncode(grantRefreshScalePage(c, 0, 1, 0, 1, func(int) string { return name }, "admin"))}
			delete(f.filtered, grantRefreshScaleName(0))
			want := c.attributes(0)
			want["permission"] = "admin"
			read(false, want)
			grantRefreshScaleAssertCounts(t, f, 1, 1)
			f.resetCounts()

			// Fresh authorization must still run even when this exact pair was
			// readable moments earlier. Failed refresh keeps the prior state.
			actor := userTestOwner()
			actor.Role = groupTestPointer(client.UserRoleMember)
			f.responses["/api/auth.info"] = grantRefreshScaleEncode(userTestAuth(actor))
			read(true, prior)
			if !reflect.DeepEqual(f.counts, map[string]int{"/api/auth.info": 1}) {
				t.Fatalf("revoked admin reached parents or grants: %v", f.counts)
			}
			f.resetCounts()
			f.responses["/api/auth.info"] = grantRefreshScaleEncode(userTestAuth(userTestOwner()))
			if c.parentField == "collection_id" {
				parent := collectionTestCollection()
				parent.ArchivedAt = nullable.NewNullableWithValue(time.Unix(1, 0).UTC())
				f.responses[c.parentPath()+":"+c.parentID] = grantRefreshScaleEncode(collectionTestEnvelope(parent))
			} else {
				parent := groupTestGroup()
				parent.ExternalId = nullable.NewNullableWithValue("external-sync")
				f.responses[c.parentPath()+":"+c.parentID] = grantRefreshScaleEncode(groupTestEnvelope(parent))
			}
			read(true, prior)
			if !reflect.DeepEqual(f.counts, map[string]int{"/api/auth.info": c.authCalls, c.parentPath(): 1}) {
				t.Fatalf("unsafe fresh parent reached target or grants: %v", f.counts)
			}
			grantRefreshScaleAssertClients(t, p, 1)
		})
	}
}

func grantRefreshScaleCLIProvider(t *testing.T, address string, p *grantRefreshScaleProvider) tfexec.ReattachInfo {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	configCh, closeCh, serveCh := make(chan *plugin.ReattachConfig, 1), make(chan struct{}), make(chan error, 1)
	go func() {
		serveCh <- tf6server.Serve(address, providerserver.NewProtocol6(p),
			tf6server.WithDebug(ctx, configCh, closeCh), tf6server.WithGoPluginLogger(hclog.NewNullLogger()),
			tf6server.WithLoggingSink(t), tf6server.WithoutLogStderrOverride())
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-serveCh:
			if err != nil {
				t.Errorf("provider server: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("provider server did not stop")
		}
	})
	select {
	case config := <-configCh:
		return tfexec.ReattachInfo{address: {
			Protocol: string(config.Protocol), ProtocolVersion: config.ProtocolVersion, Pid: config.Pid, Test: config.Test,
			Addr: tfexec.ReattachConfigAddr{Network: config.Addr.Network(), String: config.Addr.String()},
		}}
	case err := <-serveCh:
		serveCh <- err
		t.Fatalf("provider server stopped before publishing its address: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("provider server did not publish its address")
	}
	return nil
}

func TestGrantRefreshScaleTerraformCLI(t *testing.T) {
	// Match the existing CLI tests' discovery order, but keep this regression
	// optional on machines without Terraform. An explicitly configured path
	// must work. Use TF_ACC_TERRAFORM_PATH to bypass version-manager shims.
	if os.Getenv("TF_ACC_TERRAFORM_PATH") == "" {
		if _, err := exec.LookPath("terraform"); err != nil {
			t.Skip("Terraform is not on PATH; set TF_ACC_TERRAFORM_PATH to run CLI regressions")
		}
	}
	for _, c := range grantRefreshScaleCases {
		for _, sameName := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/same_name=%t", c.typeName, sameName), func(t *testing.T) {
				const address = "registry.terraform.io/glitchedmob/outline"
				f := newGrantRefreshScaleFixture(c, sameName)
				p := newGrantRefreshScaleProvider(f)
				reattach := grantRefreshScaleCLIProvider(t, address, p)
				var config bytes.Buffer
				fmt.Fprintf(&config, "terraform {\n required_providers {\n outline = { source = %q }\n }\n}\nprovider \"outline\" {\n base_url = %q\n api_key = %q\n}\n", address, grantRefreshScaleURL, groupTestKey)
				resources := make([]any, 0, grantRefreshScaleTargets)
				for i := 0; i < grantRefreshScaleTargets; i++ {
					name := fmt.Sprintf("grant%d", i)
					fmt.Fprintf(&config, "resource %q %q {\n %s = %q\n %s = %q\n permission = %q\n}\n", c.typeName, name, c.parentField, c.parentID, c.targetField, grantRefreshScaleID(i), c.permission)
					resources = append(resources, map[string]any{
						"mode": "managed", "type": c.typeName, "name": name, "provider": "provider[\"" + address + "\"]",
						"instances": []any{map[string]any{"schema_version": 0, "attributes": c.attributes(i)}},
					})
				}
				// This helper isolates HOME, CLI config and environment. Reattach
				// uses the local production provider; no registry download or live
				// Outline credentials are needed for these plans.
				tf := groupPartialCreateTerraform(t, config.String())
				state := map[string]any{
					"version": 4, "terraform_version": "1.14.5", "serial": 1, "lineage": uuid.NewString(),
					"outputs": map[string]any{}, "resources": resources,
				}
				if err := os.WriteFile(filepath.Join(tf.WorkingDir(), "terraform.tfstate"), grantRefreshScaleEncode(state), 0600); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
				defer cancel()
				changed, err := tf.Plan(ctx, tfexec.Reattach(reattach), tfexec.Parallelism(10))
				if err != nil || changed {
					t.Fatalf("normal plan changed=%t: %v", changed, err)
				}
				pages := grantRefreshScaleTargets
				if sameName {
					pages *= 10
				}
				grantRefreshScaleAssertCounts(t, f, grantRefreshScaleTargets, pages)
				grantRefreshScaleAssertClients(t, p, 1)
				f.resetCounts()
				changed, err = tf.Plan(ctx, tfexec.Reattach(reattach), tfexec.Refresh(false), tfexec.Parallelism(10))
				if err != nil || changed {
					t.Fatalf("refresh=false plan changed=%t: %v", changed, err)
				}
				f.mu.Lock()
				defer f.mu.Unlock()
				if len(f.counts) != 0 {
					t.Fatalf("refresh=false contacted Outline: %v", f.counts)
				}
				grantRefreshScaleAssertClients(t, p, 2)
			})
		}
	}
}
