// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/glitchedmob/terraform-provider-outline/internal/client"
	"github.com/google/uuid"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	tcexec "github.com/testcontainers/testcontainers-go/exec"
)

type grantRefreshPrincipal struct {
	Name    string `json:"name"`
	UserID  string `json:"user_id"`
	GroupID string `json:"group_id"`
}

type grantRefreshFixture struct {
	Version      string                           `json:"version"`
	Action       string                           `json:"action"`
	CollectionID string                           `json:"collection_id"`
	GroupID      string                           `json:"group_id"`
	Cases        map[string]grantRefreshPrincipal `json:"cases"`
	DuplicateID  string                           `json:"duplicate_id"`
	RemovedID    string                           `json:"removed_id"`
	Archived     bool                             `json:"archived"`
}

func runGrantRefreshFixture(t *testing.T, api *acceptanceAPI, action, collection, group string, target ...string) grantRefreshFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), time.Minute)
	defer cancel()
	const fixturePath = "/opt/outline/acceptance-grant-refresh-fixture.cjs"
	if err := api.container.CopyFileToContainer(ctx, "../../integration/grant-refresh-fixture.cjs", fixturePath, 0o644); err != nil {
		t.Fatal(err)
	}
	command := append([]string{"node", fixturePath, action, collection, group}, target...)
	code, output, err := api.container.Exec(ctx, command, tcexec.Multiplexed())
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(output)
	if err != nil || code != 0 {
		t.Fatalf("grant refresh fixture exited %d: %s, %v", code, data, err)
	}
	var result grantRefreshFixture
	const marker = "OUTLINE_ACCEPTANCE_GRANT_REFRESH="
	found := false
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, marker) {
			if found {
				t.Fatal("grant refresh fixture repeated its result marker")
			}
			found = true
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, marker)), &result); err != nil {
				t.Fatal(err)
			}
		}
	}
	if !found || result.Version != "1.10.1" || result.Action != action || result.CollectionID != collection || result.GroupID != group {
		t.Fatalf("grant refresh fixture did not confirm its release and parents: %s", data)
	}
	return result
}

type grantRefreshPage struct {
	Query  *string `json:"query"`
	Offset int     `json:"offset"`
	Limit  int     `json:"limit"`
	Total  int
	IDs    []string
}

// The recorder forwards every request to the released server. It never invents
// responses or disables production pacing. before makes a rename race repeatable.
type grantRefreshRecorder struct {
	base   http.RoundTripper
	pages  []grantRefreshPage
	calls  []string
	before func() error
}

func (r *grantRefreshRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	r.calls = append(r.calls, req.URL.Path)
	switch req.URL.Path {
	case "/api/auth.info", "/api/collections.info", "/api/users.info", "/api/groups.info":
		return r.base.RoundTrip(req)
	case "/api/collections.memberships", "/api/collections.group_memberships", "/api/groups.memberships":
	default:
		return nil, fmt.Errorf("refresh attempted an unexpected endpoint: %s", req.URL.Path)
	}
	if req.GetBody == nil {
		return nil, fmt.Errorf("membership request has no replayable body")
	}
	body, err := req.GetBody()
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(body)
	_ = body.Close()
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, err
	}
	for key := range fields {
		if key != "id" && key != "query" && key != "limit" && key != "offset" {
			return nil, fmt.Errorf("refresh sent unsafe membership filter %q", key)
		}
	}
	var page grantRefreshPage
	if err := json.Unmarshal(data, &page); err != nil {
		return nil, err
	}
	if page.Query != nil && r.before != nil {
		before := r.before
		r.before = nil
		if err := before(); err != nil {
			return nil, err
		}
	}
	response, err := r.base.RoundTrip(req)
	if err != nil {
		return response, err
	}
	data, err = io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil {
		return nil, err
	}
	response.Body = io.NopCloser(bytes.NewReader(data))
	var envelope struct {
		Pagination struct{ Total int }
		Data       struct {
			Memberships []struct {
				UserID string `json:"userId"`
			}
			GroupMemberships []struct {
				UserID  string `json:"userId"`
				GroupID string `json:"groupId"`
			} `json:"groupMemberships"`
		}
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return nil, err
	}
	page.Total = envelope.Pagination.Total
	for _, member := range envelope.Data.Memberships {
		page.IDs = append(page.IDs, member.UserID)
	}
	for _, member := range envelope.Data.GroupMemberships {
		id := member.UserID
		if req.URL.Path == "/api/collections.group_memberships" {
			id = member.GroupID
		}
		page.IDs = append(page.IDs, id)
	}
	r.pages = append(r.pages, page)
	return response, nil
}

func grantRefreshClient(t *testing.T, api *acceptanceAPI, key string) (*apiClient, *grantRefreshRecorder) {
	t.Helper()
	reader, err := newAPIClient(api.baseURL, key, 30, "acceptance-refresh")
	if err != nil {
		t.Fatal(err)
	}
	transport := reader.httpClient.Transport.(*bearerTransport)
	recorder := &grantRefreshRecorder{base: transport.base}
	transport.base = recorder
	return reader, recorder
}

// Exercise Resource.Read and real framework state, rather than only the list
// helper. This focused acceptance case needs neither a CLI nor dozens of applies.
func readGrantRefresh(t *testing.T, api *apiClient, kind, collection, group string, principal grantRefreshPrincipal, permission, wantError string, missing bool) {
	t.Helper()
	var reader resource.Resource
	var state tfsdk.State
	switch kind {
	case "collection_user":
		r := &collectionUserResource{api: api}
		model := collectionUserModel{ID: types.StringValue(collection + "/" + principal.UserID), CollectionID: types.StringValue(collection), UserID: types.StringValue(principal.UserID), Permission: types.StringValue("read")}
		state = tfsdk.State(cuUnitPlan(t, r, model))
		reader = r
	case "collection_group":
		r := &collectionGroupResource{api: api}
		model := collectionGroupModel{ID: types.StringValue(collection + "/" + principal.GroupID), CollectionID: types.StringValue(collection), GroupID: types.StringValue(principal.GroupID), Permission: types.StringValue("read")}
		state = tfsdk.State(cgUnitPlan(t, r, model))
		reader = r
	case "group_member":
		r := &groupMemberResource{api: api}
		model := groupMemberModel{ID: types.StringValue(group + "/" + principal.UserID), GroupID: types.StringValue(group), UserID: types.StringValue(principal.UserID), Permission: types.StringValue("member")}
		state = tfsdk.State(memberTestPlan(t, r, model))
		reader = r
	default:
		t.Fatalf("unknown refresh resource %q", kind)
	}
	response := resource.ReadResponse{State: state}
	reader.Read(t.Context(), resource.ReadRequest{State: state}, &response)
	if wantError != "" {
		if !response.Diagnostics.HasError() || !strings.Contains(fmt.Sprint(response.Diagnostics), wantError) {
			t.Fatalf("expected %q diagnostic, got %v", wantError, response.Diagnostics)
		}
		if !response.State.Raw.Equal(state.Raw) {
			t.Fatal("failed refresh changed or removed prior state")
		}
		return
	}
	if response.Diagnostics.HasError() {
		t.Fatal(response.Diagnostics)
	}
	if missing {
		if !response.State.Raw.IsNull() {
			t.Fatal("proven missing pair stayed in state")
		}
		return
	}
	if response.State.Raw.IsNull() {
		t.Fatal("existing pair was falsely removed from state")
	}
	var actual types.String
	if diagnostics := response.State.GetAttribute(t.Context(), path.Root("permission"), &actual); diagnostics.HasError() || actual.ValueString() != permission {
		t.Fatalf("permission drift was not observed: %v, want %q, diagnostics %v", actual, permission, diagnostics)
	}
	var oldID, newID types.String
	if diagnostics := state.GetAttribute(t.Context(), path.Root("id"), &oldID); diagnostics.HasError() {
		t.Fatal(diagnostics)
	}
	if diagnostics := response.State.GetAttribute(t.Context(), path.Root("id"), &newID); diagnostics.HasError() || newID != oldID {
		t.Fatalf("refresh changed the exact pair identity: %v", diagnostics)
	}
}

func checkGrantRefreshPages(t *testing.T, recorder *grantRefreshRecorder, query string, filtered, unfiltered int) {
	t.Helper()
	if len(recorder.pages) != filtered+unfiltered {
		t.Fatalf("got %d membership requests, want %d filtered and %d unfiltered: %+v", len(recorder.pages), filtered, unfiltered, recorder.pages)
	}
	for i, page := range recorder.pages {
		index := i
		if i < filtered {
			if page.Query == nil || *page.Query != query {
				t.Fatalf("request %d did not send the literal fresh principal name %q: %+v", i, query, page)
			}
		} else {
			index -= filtered
			if page.Query != nil {
				t.Fatalf("absence fallback remained filtered: %+v", page)
			}
		}
		if page.Limit != 100 || page.Offset != index*100 {
			t.Fatalf("refresh skipped or repeated a page: %+v", page)
		}
	}
	if !strings.Contains(strings.Join(recorder.calls, ","), "/api/auth.info") {
		t.Fatal("refresh did not authenticate freshly")
	}
	t.Logf("real Outline requests: %d filtered, %d unfiltered", filtered, unfiltered)
}

func TestAccGrantRefreshQueries(t *testing.T) {
	api := newAcceptanceAPI(t)
	stackTest := t
	collection, err := api.acceptanceCreateCollection("Terraform grant refresh query")
	if err != nil {
		t.Fatal(err)
	}
	group, err := api.acceptanceCreateGroup("Terraform group member grant refresh query")
	if err != nil {
		t.Fatal(err)
	}
	fixture := runGrantRefreshFixture(t, api, "seed", collection, group)
	for _, name := range []string{"percent", "percent_decoy", "underscore", "underscore_decoy", "backslash", "backslash_decoy", "shared", "rename", "absent", "same_name_absent", "external"} {
		principal, exists := fixture.Cases[name]
		if !exists || principal.Name == "" || uuid.Validate(principal.UserID) != nil || uuid.Validate(principal.GroupID) != nil {
			t.Fatalf("fixture omitted a valid %s principal: %+v", name, principal)
		}
	}
	// QueryHelper.likeContains at pinned commit 4a5a616 escapes %, _, and \\.
	// Near-matching decoys make accidental SQL wildcard interpretation fail.
	for _, kind := range []string{"collection_user", "collection_group", "group_member"} {
		for _, literal := range []string{"percent", "underscore", "backslash"} {
			t.Run(kind+"/literal "+literal, func(t *testing.T) {
				principal := fixture.Cases[literal]
				reader, recorder := grantRefreshClient(t, api, api.apiKey)
				permission := "read"
				if kind == "group_member" {
					permission = "member"
				}
				readGrantRefresh(t, reader, kind, collection, group, principal, permission, "", false)
				checkGrantRefreshPages(t, recorder, principal.Name, 1, 0)
				wantID := principal.UserID
				if kind == "collection_group" {
					wantID = principal.GroupID
				}
				if recorder.pages[0].Total != 1 || !reflect.DeepEqual(recorder.pages[0].IDs, []string{wantID}) {
					t.Fatalf("query did not match only the literal principal: %+v", recorder.pages[0])
				}
			})
		}
		t.Run(kind+"/same name UUID and early permission drift with complete pagination", func(t *testing.T) {
			principal := fixture.Cases["shared"]
			reader, recorder := grantRefreshClient(t, api, api.apiKey)
			permission := "read_write"
			if kind == "group_member" {
				permission = "admin"
			}
			readGrantRefresh(t, reader, kind, collection, group, principal, permission, "", false)
			checkGrantRefreshPages(t, recorder, principal.Name, 2, 0)
			target := principal.UserID
			if kind == "collection_group" {
				target = principal.GroupID
			}
			if recorder.pages[0].Total != 102 || recorder.pages[1].Total != 102 || len(recorder.pages[1].IDs) != 2 ||
				!slices.Contains(recorder.pages[0].IDs, target) || slices.Contains(recorder.pages[1].IDs, target) {
				t.Fatalf("refresh must validate page two after an exact first-page match: %+v", recorder.pages)
			}
		})
		t.Run(kind+"/concurrent rename falls back without false absence", func(t *testing.T) {
			principal := fixture.Cases["rename"]
			reader, recorder := grantRefreshClient(t, api, api.apiKey)
			permission := "read"
			if kind == "group_member" {
				permission = "member"
			}
			rename := func(name string) error {
				if kind != "collection_group" {
					return api.acceptanceUserName(principal.UserID, name)
				}
				r, err := api.GroupsUpdateWithResponse(stackTest.Context(), client.GroupsUpdateJSONRequestBody{Id: uuid.MustParse(principal.GroupID), Name: &name})
				if err != nil {
					return err
				}
				if r.StatusCode() != http.StatusOK || r.JSON200 == nil || r.JSON200.Data == nil || r.JSON200.Data.Name == nil || *r.JSON200.Data.Name != name {
					return fmt.Errorf("rename did not confirm group name: HTTP %d", r.StatusCode())
				}
				return nil
			}
			recorder.before = func() error { return rename("Grant refresh renamed after parent read") }
			t.Cleanup(func() {
				if err := rename(principal.Name); err != nil {
					t.Error(err)
				}
			})
			readGrantRefresh(t, reader, kind, collection, group, principal, permission, "", false)
			checkGrantRefreshPages(t, recorder, principal.Name, 1, 2)
			if recorder.before != nil || recorder.pages[0].Total != 0 {
				t.Fatal("test did not produce a real filtered miss after the rename")
			}
		})
		for _, missing := range []string{"absent", "same_name_absent"} {
			t.Run(kind+"/true missing "+missing, func(t *testing.T) {
				principal := fixture.Cases[missing]
				reader, recorder := grantRefreshClient(t, api, api.apiKey)
				readGrantRefresh(t, reader, kind, collection, group, principal, "", "", true)
				filtered := 1
				if missing == "same_name_absent" {
					filtered = 2
				}
				checkGrantRefreshPages(t, recorder, principal.Name, filtered, 2)
			})
		}
	}
	for _, kind := range []string{"collection_user", "collection_group"} {
		t.Run(kind+"/late conflicting pair retains state", func(t *testing.T) {
			principal := fixture.Cases["shared"]
			suffix, target := "user", principal.UserID
			if kind == "collection_group" {
				suffix, target = "group", principal.GroupID
			}
			duplicate := runGrantRefreshFixture(t, api, "duplicate-"+suffix, collection, group, target)
			if uuid.Validate(duplicate.DuplicateID) != nil {
				t.Fatal("duplicate fixture did not return a grant UUID")
			}
			t.Cleanup(func() {
				removed := runGrantRefreshFixture(t, api, "remove-duplicate-"+suffix, collection, group, duplicate.DuplicateID)
				if removed.RemovedID != duplicate.DuplicateID {
					t.Error("duplicate cleanup did not confirm the exact row")
				}
			})
			reader, recorder := grantRefreshClient(t, api, api.apiKey)
			readGrantRefresh(t, reader, kind, collection, group, principal, "", "duplicate", false)
			checkGrantRefreshPages(t, recorder, principal.Name, 2, 0)
			if recorder.pages[0].Total != 103 || recorder.pages[1].Total != 103 ||
				!slices.Contains(recorder.pages[0].IDs, target) || !slices.Contains(recorder.pages[1].IDs, target) {
				t.Fatalf("conflicting pair was not on the second real filtered page: %+v", recorder.pages)
			}
		})
	}
	t.Run("fresh admin authorization", func(t *testing.T) {
		principal := fixture.Cases["percent"]
		key := api.acceptanceInspectUser(t, principal.UserID, "key").APIKey
		for _, kind := range []string{"collection_user", "collection_group", "group_member"} {
			reader, recorder := grantRefreshClient(t, api, key)
			readGrantRefresh(t, reader, kind, collection, group, principal, "", "active admin-owned API key", false)
			checkGrantRefreshPages(t, recorder, "", 0, 0)
		}
	})
	t.Run("direct grant owner refusal", func(t *testing.T) {
		actor, err := api.acceptanceUserActor()
		if err != nil {
			t.Fatal(err)
		}
		reader, recorder := grantRefreshClient(t, api, api.apiKey)
		readGrantRefresh(t, reader, "collection_user", collection, group, grantRefreshPrincipal{UserID: actor.Id.String()}, "", "API-key owner's own direct collection grant", false)
		checkGrantRefreshPages(t, recorder, "", 0, 0)
	})
	t.Run("archived collection retains both grants", func(t *testing.T) {
		if !runGrantRefreshFixture(t, api, "archive", collection, group).Archived {
			t.Fatal("archive fixture did not confirm metadata")
		}
		t.Cleanup(func() { runGrantRefreshFixture(t, api, "restore", collection, group) })
		for _, kind := range []string{"collection_user", "collection_group"} {
			reader, recorder := grantRefreshClient(t, api, api.apiKey)
			readGrantRefresh(t, reader, kind, collection, group, fixture.Cases["percent"], "", "archived collections", false)
			checkGrantRefreshPages(t, recorder, "", 0, 0)
		}
	})
	t.Run("externalGroup-only collection grant is readable", func(t *testing.T) {
		principal := fixture.Cases["external"]
		if err := api.acceptanceCollectionGroupFixture(t.Context(), "sync", principal.GroupID); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := api.acceptanceCollectionGroupFixture(stackTest.Context(), "unsync", principal.GroupID); err != nil {
				t.Error(err)
			}
		})
		reader, recorder := grantRefreshClient(t, api, api.apiKey)
		readGrantRefresh(t, reader, "collection_group", collection, group, principal, "read", "", false)
		checkGrantRefreshPages(t, recorder, principal.Name, 1, 0)
	})
	t.Run("externalGroup-only membership remains refused", func(t *testing.T) {
		if err := api.acceptanceGroupMemberSync(t, group, true); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := api.acceptanceGroupMemberSync(stackTest, group, false); err != nil {
				t.Error(err)
			}
		})
		reader, recorder := grantRefreshClient(t, api, api.apiKey)
		readGrantRefresh(t, reader, "group_member", collection, group, fixture.Cases["percent"], "", "externally linked or synchronized", false)
		checkGrantRefreshPages(t, recorder, "", 0, 0)
	})
}
