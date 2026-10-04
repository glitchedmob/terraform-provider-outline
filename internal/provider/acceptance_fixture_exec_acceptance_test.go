// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
)

// Test Docker's real exec output handling without needing a database or API key.
func TestAccAcceptanceFixtureExecution(t *testing.T) {
	if os.Getenv("TF_ACC") != "1" {
		t.Skip("set TF_ACC=1 to start a disposable fixture execution container")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:       "docker.getoutline.com/outlinewiki/outline:1.10.1",
			Entrypoint:  []string{"node"},
			Cmd:         []string{"-e", "setInterval(() => {}, 1000)"},
			NetworkMode: "none",
		},
		Started: true,
	})
	if container != nil {
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			if err := container.Terminate(ctx); err != nil {
				t.Errorf("remove fixture execution container: %v", err)
			}
		})
	}
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "fixture.cjs")
	const script = `process.stdout.write(JSON.stringify(process.argv.slice(2)) + "\n"); process.stderr.write("stderr\n"); process.exitCode = Number(process.argv[2]);`
	if err := os.WriteFile(source, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	const path = "/tmp/acceptance-execution-fixture.cjs"
	executionCtx, executionCancel := context.WithTimeout(t.Context(), time.Minute)
	defer executionCancel()
	code, data, failure := runAcceptanceFixture(executionCtx, container, source, path, "0", "target one", "")
	if failure != nil || code != 0 || string(data) != "[\"0\",\"target one\",\"\"]\nstderr\n" {
		t.Fatalf("copy/run: exit=%d output=%q err=%v", code, data, failure)
	}
	// Reuse the copied script, just as detached restore cleanup does.
	code, data, failure = execAcceptanceFixture(executionCtx, container, path, "9", "target two", "")
	if failure != nil || code != 9 || string(data) != "[\"9\",\"target two\",\"\"]\nstderr\n" {
		t.Fatalf("exec-only: exit=%d output=%q err=%v", code, data, failure)
	}
}
