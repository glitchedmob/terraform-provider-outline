// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	tcexec "github.com/testcontainers/testcontainers-go/exec"
)

type acceptanceOIDCObservation struct {
	Pending         bool     `json:"pending"`
	LastSignedIn    bool     `json:"last_signed_in"`
	Authentications []string `json:"authentications"`
	MatchingUsers   int      `json:"matching_users"`
	TotalUsers      int      `json:"total_users"`
}

func (a *acceptanceAPI) acceptanceInspectOIDC(t *testing.T, id string) acceptanceOIDCObservation {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	const path = "/opt/outline/acceptance-oidc-inspect.cjs"
	if err := a.container.CopyFileToContainer(ctx, "../../integration/oidc-inspect.cjs", path, 0o644); err != nil {
		t.Fatal("copy read-only OIDC inspection fixture failed")
	}
	exit, output, err := a.container.Exec(ctx, []string{"node", path, id}, tcexec.Multiplexed())
	if err != nil {
		t.Fatal("execute read-only OIDC inspection fixture failed")
	}
	data, err := io.ReadAll(output)
	if err != nil || exit != 0 {
		t.Fatal("read-only OIDC inspection failed, output withheld")
	}
	const marker = "OUTLINE_ACCEPTANCE_OIDC="
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, marker) {
			var result acceptanceOIDCObservation
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, marker)), &result); err != nil {
				t.Fatal("decode read-only OIDC observation failed")
			}
			return result
		}
	}
	t.Fatal("read-only OIDC observation missing")
	return acceptanceOIDCObservation{}
}

// Keep codes, state, session cookies, token responses, and auth.info's
// collaboration token out of failure messages and artifacts.
func acceptanceOIDCRequest(t *testing.T, browser *http.Client, method, target string, body io.Reader) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, target, body)
	if err != nil {
		t.Fatal("create OIDC fixture request failed")
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := browser.Do(req)
	if err != nil {
		t.Fatal("OIDC fixture HTTP request failed, URL and error withheld")
	}
	t.Cleanup(func() { _ = res.Body.Close() })
	return res
}

func acceptanceOIDCLocation(t *testing.T, res *http.Response, origin, path string) *url.URL {
	t.Helper()
	if res.StatusCode != http.StatusFound {
		t.Fatalf("expected auth redirect, got HTTP %d", res.StatusCode)
	}
	location, err := res.Location()
	if err != nil || location.Scheme+"://"+location.Host != origin || path != "" && location.Path != path || location.User != nil {
		if err == nil {
			t.Fatalf("unexpected auth redirect origin %s://%s path %s, expected %s %s; query withheld", location.Scheme, location.Host, location.Path, origin, path)
		}
		t.Fatal("invalid auth redirect, URL withheld")
	}
	return location
}

func acceptanceOIDCSignIn(t *testing.T, app, idp, name string, verified bool, userID, collectionID string) {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	browser := &http.Client{Jar: jar, Timeout: 30 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	t.Cleanup(browser.CloseIdleConnections)
	start := acceptanceOIDCRequest(t, browser, http.MethodGet, app+"/auth/oidc", nil)
	authorize := acceptanceOIDCLocation(t, start, idp, "/authorize")
	q := authorize.Query()
	q.Set("fixture_case", name) // The browser chooses an identity at the test IdP.
	authorize.RawQuery = q.Encode()
	provider := acceptanceOIDCRequest(t, browser, http.MethodGet, authorize.String(), nil)
	callback := acceptanceOIDCLocation(t, provider, app, "/auth/oidc.callback")
	if callback.Query().Get("code") == "" || callback.Query().Get("state") != q.Get("state") {
		t.Fatal("fixture authorization code or state missing or changed")
	}
	completed := acceptanceOIDCRequest(t, browser, http.MethodGet, callback.String(), nil)
	location := acceptanceOIDCLocation(t, completed, app, "")
	if verified {
		if location.Query().Get("notice") != "" {
			t.Fatal("verified OIDC login returned an error notice")
		}
	} else if location.Path != "/" || location.Query().Get("notice") != "invalid-authentication" {
		t.Fatal("unverified OIDC login did not return invalid-authentication")
	}
	appURL, _ := url.Parse(app)
	hasSession := false
	for _, cookie := range jar.Cookies(appURL) {
		if cookie.Name == "accessToken" && cookie.Value != "" {
			hasSession = true
		}
	}
	if hasSession != verified {
		t.Fatal("auth callback issued an unexpected session-cookie state")
	}
	// Only the cookie minted by Outline's callback authenticates this request.
	// No operator key, ORM-created target key, or locally constructed session.
	info := acceptanceOIDCRequest(t, browser, http.MethodPost, app+"/api/auth.info", strings.NewReader("{}"))
	var envelope struct {
		Error string `json:"error"`
		Data  struct {
			User struct {
				ID          string `json:"id"`
				Role        string `json:"role"`
				IsSuspended bool   `json:"isSuspended"`
			} `json:"user"`
		} `json:"data"`
	}
	if err := json.NewDecoder(info.Body).Decode(&envelope); err != nil {
		t.Fatal("decode session auth.info failed, body withheld")
	}
	if verified {
		if info.StatusCode != http.StatusOK || envelope.Data.User.ID != userID || envelope.Data.User.Role != "viewer" || envelope.Data.User.IsSuspended {
			t.Fatal("real OIDC session did not authenticate as the invited viewer UUID")
		}
		collection := acceptanceOIDCRequest(t, browser, http.MethodPost, app+"/api/collections.info", strings.NewReader(fmt.Sprintf(`{"id":%q}`, collectionID)))
		if collection.StatusCode != http.StatusOK {
			t.Fatalf("OIDC viewer session cannot read the pre-granted private collection: HTTP %d", collection.StatusCode)
		}
	} else if info.StatusCode != http.StatusUnauthorized || envelope.Error != "authentication_required" {
		t.Fatal("unverified login unexpectedly authenticated or failed for a different reason")
	}
	stats := acceptanceOIDCRequest(t, browser, http.MethodGet, idp+"/stats", nil)
	var counts map[string]struct{ Authorize, Token, Userinfo int }
	if stats.StatusCode != http.StatusOK || json.NewDecoder(stats.Body).Decode(&counts) != nil {
		t.Fatal("read OIDC provider counters failed")
	}
	if c := counts[name]; c.Authorize != 1 || c.Token != 1 || c.Userinfo != 1 {
		t.Fatalf("released auth route did not complete code exchange and UserInfo fetch: %+v", c)
	}
}

func TestAccOIDCFirstLogin(t *testing.T) {
	if os.Getenv("TF_ACC") != "1" {
		t.Skip("set TF_ACC=1 to start the disposable Outline and OIDC fixture")
	}
	if version := os.Getenv("OUTLINE_VERSION"); version != "" && version != "1.10.1" {
		t.Skip("OIDC first-login fixture is verified only on Outline 1.10.1")
	}
	t.Setenv("OUTLINE_API_KEY", "")
	t.Setenv("OUTLINE_BASE_URL", "")
	port := acceptanceLoopbackPort(t)
	base, key, container := startAcceptanceStackWithOIDC(t, port)
	api := acceptanceAPIWithCredentials(t, base, key, container)
	// Outline refuses to delete the last collection. Keep an API-created
	// private anchor until the disposable workspace and volumes are removed.
	if _, err := api.acceptanceCreateCollection("OIDC cleanup anchor"); err != nil {
		t.Fatal(err)
	}
	app, idp := strings.TrimSuffix(base, "/api"), "http://127.0.0.1:"+port
	for _, tc := range []struct {
		name     string
		verified bool
	}{
		{"verified", true}, {"id-token", true}, {"string-true", true},
		{"missing", false}, {"false", false}, {"userinfo-false", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := api.providerConfig + fmt.Sprintf(`
resource "outline_user" "login" {
  email = %q
  role = "viewer"
  suspended = false
  suppress_email = true
  delete_permanently = true
}
resource "outline_group" "login" { name = %q }
resource "outline_group_member" "login" {
  group_id = outline_group.login.id
  user_id = outline_user.login.id
  permission = "admin"
}
resource "outline_collection" "login" {
  name = %q
  sharing = false
  allow_destroy = true
}
resource "outline_collection_group" "login" {
  collection_id = outline_collection.login.id
  group_id = outline_group.login.id
  permission = "read"
}
resource "outline_collection_user" "login" {
  collection_id = outline_collection.login.id
  user_id = outline_user.login.id
  permission = "read"
}
`, tc.name+"@example.invalid", "OIDC group "+tc.name, "OIDC collection "+tc.name)
			var userID string
			check := func(state *terraform.State) error {
				id := state.RootModule().Resources["outline_user.login"].Primary.ID
				group := state.RootModule().Resources["outline_group.login"].Primary.ID
				collection := state.RootModule().Resources["outline_collection.login"].Primary.ID
				if userID != "" {
					if id != userID {
						return fmt.Errorf("post-login refresh changed the invited UUID")
					}
					return api.checkAcceptanceUser("outline_user.login")(state)
				}
				userID = id
				if err := resource.ComposeAggregateTestCheckFunc(
					api.checkAcceptanceUser("outline_user.login"),
					api.checkAcceptanceGroupMember("outline_group_member.login"),
					api.checkAcceptanceCollectionUser("outline_collection_user.login"),
					api.checkAcceptanceCollectionGroup("outline_collection_group.login"),
				)(state); err != nil {
					return err
				}
				c, err := api.acceptanceCollection(collection)
				if err != nil || !c.Permission.IsNull() || *c.Sharing {
					return fmt.Errorf("expected a private, unshared collection: %v", err)
				}
				before := api.acceptanceInspectOIDC(t, id)
				if !before.Pending || before.LastSignedIn || len(before.Authentications) != 0 || before.MatchingUsers != 1 {
					return fmt.Errorf("expected a single pending invitation with no authentication")
				}
				invitation := api.acceptanceInspectUser(t, id, "inspect")
				if invitation.InvitationEmailSent || invitation.EmailEnabled || invitation.PasswordAttribute {
					return fmt.Errorf("OIDC fixture must not use SMTP or passwords")
				}
				members, _, err := api.acceptanceGroupMembers(group)
				if err != nil {
					return err
				}
				users, err := api.acceptanceCollectionUsers(collection)
				if err != nil {
					return err
				}
				groups, err := api.acceptanceCollectionGroups(collection)
				if err != nil {
					return err
				}
				acceptanceOIDCSignIn(t, app, idp, tc.name, tc.verified, id, collection)
				after := api.acceptanceInspectOIDC(t, id)
				if after.MatchingUsers != 1 || after.TotalUsers != before.TotalUsers || after.Pending == tc.verified || after.LastSignedIn != tc.verified {
					return fmt.Errorf("login changed account count or invitation/sign-in state unexpectedly")
				}
				if tc.verified {
					if !reflect.DeepEqual(after.Authentications, []string{"fixture-" + tc.name}) {
						return fmt.Errorf("login did not link the provider subject to the invited UUID")
					}
				} else if !reflect.DeepEqual(before, after) {
					return fmt.Errorf("unverified login modified the pending invitation")
				}
				u, err := api.acceptanceUser(id)
				if err != nil {
					return err
				}
				if string(*u.Role) != "viewer" || *u.IsSuspended {
					return fmt.Errorf("login changed workspace role or suspension")
				}
				expectedName := "Pending user"
				if tc.verified {
					expectedName = "OIDC " + tc.name
				}
				if *u.Name != expectedName {
					return fmt.Errorf("unexpected display name after first-login attempt")
				}
				c, err = api.acceptanceCollection(collection)
				if err != nil || !c.Permission.IsNull() || *c.Sharing {
					return fmt.Errorf("login changed private collection defaults: %v", err)
				}
				nextMembers, _, err := api.acceptanceGroupMembers(group)
				if err != nil {
					return err
				}
				// An unmanaged display name may change at first sign-in.
				m := nextMembers[id]
				m.Name = members[id].Name
				nextMembers[id] = m
				if !reflect.DeepEqual(members, nextMembers) {
					return fmt.Errorf("login changed stored group membership, role, or permission")
				}
				nextUsers, err := api.acceptanceCollectionUsers(collection)
				if err != nil {
					return err
				}
				nextGroups, err := api.acceptanceCollectionGroups(collection)
				if err != nil {
					return err
				}
				if !reflect.DeepEqual(users, nextUsers) || !reflect.DeepEqual(groups, nextGroups) {
					return fmt.Errorf("login changed explicit direct or group grants")
				}
				return nil
			}
			resource.Test(t, resource.TestCase{
				ProtoV6ProviderFactories: acceptanceProviderFactories(),
				CheckDestroy:             api.checkAcceptanceUsersDestroyed,
				Steps: []resource.TestStep{
					{Config: config, Check: check},
					{Config: config, ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}}, Check: check},
				},
			})
		})
	}
}
