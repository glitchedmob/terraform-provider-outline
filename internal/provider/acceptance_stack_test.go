// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/testcontainers/testcontainers-go"
	tcexec "github.com/testcontainers/testcontainers-go/exec"
	"github.com/testcontainers/testcontainers-go/modules/compose"
)

func acceptanceProviderFactories() map[string]func() (tfprotov6.ProviderServer, error) {
	return map[string]func() (tfprotov6.ProviderServer, error){
		"outline": providerserver.NewProtocol6WithError(New("acceptance")()),
	}
}

type acceptanceAPI struct {
	*apiClient
	providerConfig string
	container      testcontainers.Container
}

func newAcceptanceAPI(t *testing.T) *acceptanceAPI {
	t.Helper()
	if os.Getenv("TF_ACC") != "1" {
		t.Skip("set TF_ACC=1 to start a disposable Outline acceptance stack")
	}
	// Do not use ambient credentials, including credentials inherited by Terraform.
	t.Setenv("OUTLINE_API_KEY", "")
	t.Setenv("OUTLINE_BASE_URL", "")
	baseURL, key, container := startAcceptanceStack(t)
	return acceptanceAPIWithCredentials(t, baseURL, key, container)
}

func acceptanceAPIWithCredentials(t *testing.T, baseURL, key string, container testcontainers.Container) *acceptanceAPI {
	t.Helper()
	api, err := newAPIClient(baseURL, key, 30, "acceptance")
	if err != nil {
		t.Fatal(err)
	}
	return &acceptanceAPI{
		apiClient: api,
		container: container,
		providerConfig: fmt.Sprintf(`
provider "outline" {
  base_url = %q
  api_key = %q
}
`, baseURL, key),
	}
}

func acceptanceLoopbackPort(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("choose Outline test port: %s", err)
	}
	port := fmt.Sprint(listener.Addr().(*net.TCPAddr).Port)
	if err := listener.Close(); err != nil {
		t.Fatalf("release Outline test port: %s", err)
	}
	return port
}

func startAcceptanceStack(t *testing.T) (string, string, testcontainers.Container) {
	return startAcceptanceStackWithOIDC(t, "")
}

func startAcceptanceStackWithOIDC(t *testing.T, oidcPort string) (string, string, testcontainers.Container) {
	t.Helper()
	port := acceptanceLoopbackPort(t)
	files := []string{"../../integration/compose.yml"}
	if oidcPort != "" {
		files = append(files, "../../integration/compose.oidc.yml")
	}
	stack, err := compose.NewDockerComposeWith(compose.WithStackFiles(files...))
	if err != nil {
		t.Fatalf("create Outline stack: %s", err)
	}
	stackEnv := map[string]string{"OUTLINE_TEST_PORT": port}
	if oidcPort != "" {
		stackEnv["OUTLINE_OIDC_TEST_PORT"] = oidcPort
	}
	if version := os.Getenv("OUTLINE_VERSION"); version != "" {
		stackEnv["OUTLINE_VERSION"] = version
	}
	stack.WithEnv(stackEnv)
	// Register before Up so failed and partially started stacks lose their volumes too.
	t.Cleanup(func() {
		if oidcPort == "" {
			captureAcceptanceLogs(t, stack)
		} else {
			// Auth logs may contain codes, claims, and tokens. Retain no service
			// logs for the OIDC stack, even on failure or partial startup.
			t.Log("OIDC stack service logs omitted to protect authentication secrets")
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := stack.Down(ctx, compose.RemoveOrphans(true), compose.RemoveVolumes(true)); err != nil {
			t.Errorf("stop Outline stack: %s", err)
		}
		if err := stack.Close(); err != nil {
			t.Errorf("close Outline stack clients: %s", err)
		}
	})

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	if err := stack.Up(ctx, compose.Wait(true)); err != nil {
		t.Fatalf("start Outline stack: %s", err)
	}
	container, err := stack.ServiceContainer(ctx, "outline")
	if err != nil {
		t.Fatalf("get Outline container: %s", err)
	}
	endpoint, err := container.PortEndpoint(ctx, "3000/tcp", "http")
	if err != nil {
		t.Fatalf("get Outline endpoint: %s", err)
	}
	const fixturePath = "/opt/outline/acceptance-bootstrap.cjs"
	if err := container.CopyFileToContainer(ctx, "../../integration/bootstrap.cjs", fixturePath, 0o644); err != nil {
		t.Fatalf("copy Outline bootstrap fixture: %s", err)
	}
	exitCode, output, err := container.Exec(ctx, []string{"node", fixturePath}, tcexec.Multiplexed())
	if err != nil {
		t.Fatalf("execute Outline bootstrap fixture: %s", err)
	}
	data, err := io.ReadAll(output)
	if err != nil {
		t.Fatalf("read Outline bootstrap fixture output: %s", err)
	}
	if exitCode != 0 {
		t.Fatalf("Outline bootstrap fixture exited %d:\n%s", exitCode, data)
	}
	var fixture struct {
		APIKey string `json:"api_key"`
	}
	const marker = "OUTLINE_ACCEPTANCE_FIXTURE="
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, marker) {
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, marker)), &fixture); err != nil {
				t.Fatalf("decode Outline bootstrap fixture: %s", err)
			}
		} else if line != "" {
			t.Logf("Outline bootstrap fixture: %s", line)
		}
	}
	if fixture.APIKey == "" {
		t.Fatal("Outline bootstrap fixture did not return an API key")
	}
	if oidcPort != "" {
		// Docker may report localhost, but URL and the OAuth callback use
		// 127.0.0.1. Browser cookies must stay on that same hostname.
		endpoint = "http://127.0.0.1:" + port
	}
	return endpoint + "/api", fixture.APIKey, container
}

func captureAcceptanceLogs(t *testing.T, stack compose.ComposeStack) {
	t.Helper()
	logDir := t.ArtifactDir()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// Use known services instead of Services(), which can be empty after failed Up.
	for _, service := range []string{"postgres", "redis", "outline"} {
		container, err := stack.ServiceContainer(ctx, service)
		if err != nil {
			t.Logf("get %s container for logs: %s", service, err)
			continue
		}
		logs, err := container.Logs(ctx)
		if err != nil {
			t.Logf("get %s logs: %s", service, err)
			continue
		}
		data, err := io.ReadAll(logs)
		if closeErr := logs.Close(); closeErr != nil {
			t.Logf("close %s logs: %s", service, closeErr)
		}
		if err != nil {
			t.Logf("read %s logs: %s", service, err)
		}
		if t.Failed() {
			t.Logf("%s logs:\n%s", service, data)
		}
		if err := os.WriteFile(filepath.Join(logDir, service+".log"), data, 0o600); err != nil {
			t.Errorf("save %s logs: %s", service, err)
		}
	}
}
