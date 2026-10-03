// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"testing"

	"github.com/testcontainers/testcontainers-go"
)

func startAcceptanceStackWithOIDC(t *testing.T, oidcPort string, overrides ...string) (string, string, testcontainers.Container) {
	t.Helper()
	return startAcceptanceStackWithOptions(t, acceptanceStackOptions{
		overrides: append([]string{"../../integration/compose.oidc.yml"}, overrides...),
		env: map[string]string{
			"OUTLINE_OIDC_TEST_PORT": oidcPort,
		},
		// Omit all service logs, including failed and partially started stacks.
		omitServiceLogs: true,
		// Match the IdP's exact callback and keep browser cookies on 127.0.0.1.
		loopbackEndpoint: true,
	})
}
