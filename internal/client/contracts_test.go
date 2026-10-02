// SPDX-License-Identifier: MPL-2.0

package client_test

import (
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/oapi-codegen/nullable"

	"github.com/glitchedmob/terraform-provider-outline/internal/client"
)

const userID = "a32c2ee6-fbde-4654-841b-0eabdc71b812"
const groupID = "b32c2ee6-fbde-4654-841b-0eabdc71b812"
const collectionID = "c32c2ee6-fbde-4654-841b-0eabdc71b812"

func ptr[T any](value T) *T { return &value }

func fixture(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func assertJSON(t *testing.T, value any, expected string) {
	t.Helper()
	actual, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var got, want any
	if err := json.Unmarshal(actual, &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(expected), &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %s, want %s", actual, expected)
	}
}

func TestGeneratedInviteContract(t *testing.T) {
	t.Parallel()
	request := client.UsersInviteJSONRequestBody{
		Invites:       []client.Invite{{Name: "OIDC User", Email: "oidc@example.com", Role: client.UserRoleMember}},
		SuppressEmail: ptr(true),
	}
	assertJSON(t, request, `{"invites":[{"name":"OIDC User","email":"oidc@example.com","role":"member"}],"suppressEmail":true}`)
	response, err := client.ParseUsersInviteResponse(fixture(http.StatusOK, `{
		"ok":true,"status":200,"data":{
			"sent":[{"name":"OIDC User","email":"oidc@example.com","role":"member"}],
			"unsent":[{"name":"Existing User","email":"existing@example.com","role":"member"}],
			"users":[{"id":"`+userID+`","name":"OIDC User","email":"oidc@example.com","role":"member","avatarUrl":null,"lastActiveAt":null,"isSuspended":false}]
		}}`))
	if err != nil {
		t.Fatal(err)
	}
	if response.JSON200 == nil || response.JSON200.Data == nil || response.JSON200.Ok == nil || !*response.JSON200.Ok {
		t.Fatal("missing success envelope")
	}
	data := response.JSON200.Data
	if data.Sent == nil || len(*data.Sent) != 1 || data.Unsent == nil || len(*data.Unsent) != 1 || data.Users == nil || len(*data.Users) != 1 {
		t.Fatal("sent, unsent, and users must be separate typed arrays")
	}
	user := (*data.Users)[0]
	if user.Id == nil || user.Id.String() != userID || user.Role == nil || *user.Role != client.UserRoleMember || !user.AvatarUrl.IsNull() || !user.LastActiveAt.IsNull() {
		t.Fatalf("unexpected invited user: %#v", user)
	}
	if email, err := user.Email.Get(); err != nil || string(email) != "oidc@example.com" {
		t.Fatalf("unexpected user email: %q, %v", email, err)
	}
}

func TestGeneratedGroupContracts(t *testing.T) {
	t.Parallel()
	assertJSON(t, client.GroupsCreateJSONRequestBody{
		Name: "Engineering", ExternalId: ptr("oidc:engineering"), DisableMentions: ptr(false),
	}, `{"name":"Engineering","externalId":"oidc:engineering","disableMentions":false}`)
	assertJSON(t, client.GroupsUpdateJSONRequestBody{
		Id: uuid.MustParse(groupID), Description: ptr(""), DisableMentions: ptr(false),
	}, `{"id":"`+groupID+`","description":"","disableMentions":false}`)
	assertJSON(t, client.GroupsListJSONRequestBody{Query: ptr("Engineering"), ExternalId: ptr("oidc:engineering"), Limit: ptr(100), Offset: ptr(0)},
		`{"query":"Engineering","externalId":"oidc:engineering","limit":100,"offset":0}`)
	response, err := client.ParseGroupsMembershipsResponse(fixture(http.StatusOK, `{
		"ok":true,"status":200,"pagination":{"offset":0,"limit":25,"total":1,"nextPath":"/api/groups.memberships?limit=25&offset=25"},
		"data":{"users":[],"groupMemberships":[{"id":"`+userID+`-`+groupID+`","userId":"`+userID+`","groupId":"`+groupID+`","permission":"admin","user":{"id":"`+userID+`"}}]}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if response.JSON200 == nil || response.JSON200.Data == nil || response.JSON200.Data.GroupMemberships == nil {
		t.Fatal("missing group users")
	}
	members := *response.JSON200.Data.GroupMemberships
	if len(members) != 1 || members[0].UserId == nil || members[0].UserId.String() != userID || members[0].Permission == nil || *members[0].Permission != client.GroupPermissionAdmin || members[0].User == nil {
		t.Fatalf("unexpected group users: %#v", members)
	}
	page := response.JSON200.Pagination
	if page == nil || page.Total == nil || *page.Total != 1 || page.NextPath == nil || *page.NextPath == "" || page.Offset == nil || *page.Offset != 0 {
		t.Fatalf("unexpected pagination: %#v", page)
	}
}

func TestGeneratedCollectionNullAndPermissionContracts(t *testing.T) {
	t.Parallel()
	id := uuid.MustParse(collectionID)
	assertJSON(t, client.CollectionsUpdateJSONRequestBody{Id: id}, `{"id":"`+collectionID+`"}`)
	assertJSON(t, client.CollectionsUpdateJSONRequestBody{
		Id: id, Permission: nullable.NewNullNullable[client.Permission](), Icon: nullable.NewNullNullable[string](),
	}, `{"id":"`+collectionID+`","permission":null,"icon":null}`)
	assertJSON(t, client.CollectionsUpdateJSONRequestBody{
		Id: id, Permission: nullable.NewNullableWithValue(client.PermissionAdmin), Sharing: ptr(false),
		Sort: &client.CollectionSort{Field: client.CollectionSortFieldTitle, Direction: client.Asc}, TemplateManagement: ptr(client.TemplateManagementAdmin),
	}, `{"id":"`+collectionID+`","permission":"admin","sharing":false,"sort":{"field":"title","direction":"asc"},"templateManagement":"admin"}`)
	assertJSON(t, client.CollectionsAddUserJSONRequestBody{Id: id, UserId: uuid.MustParse(userID), Permission: ptr(client.PermissionAdmin)},
		`{"id":"`+collectionID+`","userId":"`+userID+`","permission":"admin"}`)
	assertJSON(t, client.CollectionsAddGroupJSONRequestBody{Id: id, GroupId: uuid.MustParse(groupID), Permission: ptr(client.PermissionReadWrite)},
		`{"id":"`+collectionID+`","groupId":"`+groupID+`","permission":"read_write"}`)
	response, err := client.ParseCollectionsInfoResponse(fixture(http.StatusOK, `{"ok":true,"status":200,"data":{"id":"`+collectionID+`","name":"Private","permission":null,"description":"","templateManagement":"admin","sharing":false}}`))
	if err != nil {
		t.Fatal(err)
	}
	if response.JSON200 == nil || response.JSON200.Data == nil || !response.JSON200.Data.Permission.IsNull() || response.JSON200.Data.TemplateManagement == nil || *response.JSON200.Data.TemplateManagement != client.TemplateManagementAdmin {
		t.Fatal("private collection and admin template permission did not decode")
	}
}

func TestGeneratedCollectionMembershipResponses(t *testing.T) {
	t.Parallel()
	body := `{"ok":true,"status":200,"pagination":{"offset":0,"limit":25,"total":1,"nextPath":"/api/collections.group_memberships?offset=25&limit=25"},"data":{"groups":[],"groupMemberships":[{"id":"membership-id","groupId":"` + groupID + `","collectionId":"` + collectionID + `","documentId":null,"sourceId":null,"permission":"admin"}]}}`
	response, err := client.ParseCollectionsGroupMembershipsResponse(fixture(http.StatusOK, body))
	if err != nil {
		t.Fatal(err)
	}
	if response.JSON200 == nil || response.JSON200.Data == nil || response.JSON200.Data.GroupMemberships == nil {
		t.Fatal("release response uses groupMemberships, not collectionGroupMemberships")
	}
	members := *response.JSON200.Data.GroupMemberships
	if len(members) != 1 || members[0].Permission == nil || *members[0].Permission != client.PermissionAdmin || !members[0].SourceId.IsNull() {
		t.Fatalf("unexpected collection group memberships: %#v", members)
	}
	id, err := members[0].CollectionId.Get()
	if err != nil || id.String() != collectionID {
		t.Fatalf("unexpected collection ID: %v, %v", id, err)
	}
	added, err := client.ParseCollectionsAddGroupResponse(fixture(http.StatusOK, body))
	if err != nil || added.JSON200 == nil || added.JSON200.Data == nil || added.JSON200.Data.GroupMemberships == nil || len(*added.JSON200.Data.GroupMemberships) != 1 {
		t.Fatalf("add_group must decode the same membership shape: %v", err)
	}
	users, err := client.ParseCollectionsMembershipsResponse(fixture(http.StatusOK, `{"ok":true,"status":200,"data":{"users":[],"memberships":[{"id":"membership-id","userId":"`+userID+`","collectionId":"`+collectionID+`","permission":"admin"}]},"pagination":{"limit":25,"offset":0,"total":1}}`))
	if err != nil || users.JSON200 == nil || users.JSON200.Data == nil || users.JSON200.Data.Memberships == nil || len(*users.JSON200.Data.Memberships) != 1 {
		t.Fatalf("unexpected user membership response: %v", err)
	}
}

func TestGeneratedSuccessErrorsAndMissingData(t *testing.T) {
	t.Parallel()
	deleted, err := client.ParseUsersDeleteResponse(fixture(http.StatusOK, `{"success":true,"ok":true,"status":200}`))
	if err != nil || deleted.JSON200 == nil || deleted.JSON200.Success == nil || !*deleted.JSON200.Success || deleted.JSON200.Status == nil || *deleted.JSON200.Status != 200 {
		t.Fatalf("unexpected delete envelope: %v", err)
	}
	missing, err := client.ParseUsersInfoResponse(fixture(http.StatusOK, `{"ok":true,"status":200}`))
	if err != nil || missing.JSON200 == nil || missing.JSON200.Data != nil {
		t.Fatalf("missing data must remain detectable: %v", err)
	}
	notFound, err := client.ParseUsersInfoResponse(fixture(http.StatusNotFound, `{"ok":false,"status":404,"error":"not_found","message":"Resource not found"}`))
	if err != nil || notFound.JSON200 != nil || notFound.JSON404 == nil || notFound.StatusCode() != 404 || notFound.JSON404.Error == nil || *notFound.JSON404.Error != "not_found" {
		t.Fatalf("unexpected typed HTTP error: %v", err)
	}
	rateLimited, err := client.ParseUsersInfoResponse(&http.Response{
		StatusCode: http.StatusTooManyRequests,
		Header:     http.Header{"Content-Type": {"application/json"}, "Retry-After": {"60"}},
		Body:       io.NopCloser(strings.NewReader(`{"ok":false,"error":"rate_limit_exceeded","status":429}`)),
	})
	if err != nil || rateLimited.JSON429 == nil || rateLimited.JSON429.Status == nil || *rateLimited.JSON429.Status != http.StatusTooManyRequests || rateLimited.HTTPResponse.Header.Get("Retry-After") != "60" {
		t.Fatalf("rate limit response must retain Retry-After: %v", err)
	}
	if _, err := client.ParseUsersInfoResponse(fixture(http.StatusOK, `{"data":`)); err == nil {
		t.Fatal("malformed success JSON must fail parsing")
	}
}
