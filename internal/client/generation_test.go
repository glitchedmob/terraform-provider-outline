// SPDX-License-Identifier: MPL-2.0

package client_test

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestUpstreamSpecificationUnmodified(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("../../openapi/outline.openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	// Raw spec3.json at outline/openapi commit 40f51b75efad84e3e860af3e1c8b5a58fd74bb48.
	const expected = "71c211078aff5b950df1b478aed628a242df26fe94c3b5f055fef9b1d46f2731"
	if got := fmt.Sprintf("%x", sha256.Sum256(data)); got != expected {
		t.Fatalf("upstream source changed: got %s, want %s; update the pin and provenance deliberately", got, expected)
	}
}

func TestGeneratedClientReproducible(t *testing.T) {
	t.Parallel()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	temporary := t.TempDir()
	output := filepath.Join(temporary, "client.gen.go")
	config, err := os.ReadFile("../../openapi/oapi-codegen.yaml")
	if err != nil {
		t.Fatal(err)
	}
	// Config output takes precedence over the CLI -o flag. Redirect it without
	// changing any generation options or writing into the source directory.
	const originalOutput = "\noutput: internal/client/client.gen.go\n"
	if strings.Count(string(config), originalOutput) != 1 {
		t.Fatal("expected one generated output in oapi-codegen.yaml")
	}
	config = []byte(strings.Replace(string(config), originalOutput, "\noutput: "+strconv.Quote(output)+"\n", 1))
	configPath := filepath.Join(temporary, "oapi-codegen.yaml")
	if err := os.WriteFile(configPath, config, 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(t.Context(), "go", "tool", "oapi-codegen",
		"--config", configPath, "openapi/outline.openapi.json")
	command.Dir = root
	// Dependencies may be downloaded normally, but the specification must stay local.
	if log, err := command.CombinedOutput(); err != nil {
		t.Fatalf("generate: %v\n%s", err, log)
	}
	actual, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	expected, err := os.ReadFile("client.gen.go")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(actual, expected) {
		t.Fatal("generated client differs from committed source; run make generate")
	}
}
