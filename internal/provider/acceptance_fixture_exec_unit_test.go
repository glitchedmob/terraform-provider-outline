// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcexec "github.com/testcontainers/testcontainers-go/exec"
)

type fakeAcceptanceFixtureContainer struct {
	testcontainers.Container
	copy func(context.Context, string, string, int64) error
	exec func(context.Context, []string, ...tcexec.ProcessOption) (int, io.Reader, error)
}

func (f fakeAcceptanceFixtureContainer) CopyFileToContainer(ctx context.Context, source, path string, mode int64) error {
	return f.copy(ctx, source, path, mode)
}

func (f fakeAcceptanceFixtureContainer) Exec(ctx context.Context, command []string, options ...tcexec.ProcessOption) (int, io.Reader, error) {
	return f.exec(ctx, command, options...)
}

type fixtureErrorReader struct{ err error }

func (r fixtureErrorReader) Read([]byte) (int, error) { return 0, r.err }

func TestAcceptanceFixtureExecution(t *testing.T) {
	failure := errors.New("fixture operation failed")
	for _, step := range []string{"success", "copy", "execute", "read", "nonzero"} {
		t.Run(step, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			var calls []string
			container := fakeAcceptanceFixtureContainer{
				copy: func(got context.Context, source, path string, mode int64) error {
					calls = append(calls, "copy")
					if got != ctx || source != "../../integration/example.cjs" || path != "/opt/outline/example.cjs" || mode != 0o644 {
						t.Fatalf("copy changed context/path/mode: %v %q %q %o", got, source, path, mode)
					}
					if step == "copy" {
						return failure
					}
					return nil
				},
				exec: func(got context.Context, command []string, options ...tcexec.ProcessOption) (int, io.Reader, error) {
					calls = append(calls, "execute")
					if got != ctx || !reflect.DeepEqual(command, []string{"node", "/opt/outline/example.cjs", "action", "", "id", "target one", "target two"}) {
						t.Fatalf("exec changed context/arguments: %v %q", got, command)
					}
					// Apply the actual option to Docker-framed output, not just its type.
					var framed bytes.Buffer
					for i, text := range []string{"stdout", "stderr"} {
						header := []byte{byte(i + 1), 0, 0, 0, 0, 0, 0, 0}
						binary.BigEndian.PutUint32(header[4:], uint32(len(text)))
						framed.Write(header)
						framed.WriteString(text)
					}
					if len(options) != 1 {
						t.Fatalf("exec options = %d, want just Multiplexed", len(options))
					}
					processed := tcexec.NewProcessOptions(command)
					processed.Reader = &framed
					options[0].Apply(processed)
					if step == "execute" {
						return 7, fixtureErrorReader{failure}, failure
					}
					if step == "read" {
						return 7, io.MultiReader(processed.Reader, fixtureErrorReader{failure}), nil
					}
					if step == "nonzero" {
						return 7, processed.Reader, nil
					}
					return 0, processed.Reader, nil
				},
			}
			code, data, err := runAcceptanceFixture(ctx, container, "../../integration/example.cjs", "/opt/outline/example.cjs", "action", "", "id", "target one", "target two")
			wantCalls := []string{"copy", "execute"}
			if step == "copy" {
				wantCalls = wantCalls[:1]
			}
			if !reflect.DeepEqual(calls, wantCalls) {
				t.Fatalf("calls = %v, want %v", calls, wantCalls)
			}
			switch step {
			case "copy", "execute", "read":
				if err == nil || err.step != step || !errors.Is(err, failure) || err.Error() != failure.Error() {
					t.Fatalf("failure changed: %v", err)
				}
			default:
				if err != nil {
					t.Fatal(err)
				}
			}
			if step == "copy" || step == "execute" {
				if data != nil {
					t.Fatalf("output read after %s failure", step)
				}
			} else if string(data) != "stdoutstderr" {
				t.Fatalf("lost or framed output: %q", data)
			}
			wantCode := 7
			if step == "success" || step == "copy" {
				wantCode = 0
			}
			if code != wantCode {
				t.Fatalf("exit code = %d, want %d", code, wantCode)
			}
		})
	}
}

func TestAcceptanceFixtureCancellation(t *testing.T) {
	for _, step := range []string{"copy", "execute"} {
		t.Run(step, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			container := fakeAcceptanceFixtureContainer{
				copy: func(got context.Context, _, _ string, _ int64) error {
					if got != ctx {
						t.Fatal("copy replaced caller context")
					}
					if step == "copy" {
						cancel()
						return got.Err()
					}
					return nil
				},
				exec: func(got context.Context, _ []string, _ ...tcexec.ProcessOption) (int, io.Reader, error) {
					if got != ctx {
						t.Fatal("exec replaced caller context")
					}
					cancel()
					return 0, nil, got.Err()
				},
			}
			_, _, err := runAcceptanceFixture(ctx, container, "source", "path")
			if err == nil || err.step != step || !errors.Is(err, context.Canceled) {
				t.Fatalf("lost cancellation: %v", err)
			}
		})
	}
}

func TestAcceptanceFixtureDetachedCleanup(t *testing.T) {
	t.Cleanup(func() {
		if t.Context().Err() != context.Canceled {
			t.Fatal("test context was not canceled before cleanup")
		}
		container := fakeAcceptanceFixtureContainer{
			copy: func(ctx context.Context, source, path string, mode int64) error {
				if ctx.Err() != nil || source != "../../integration/grant-refresh-fixture.cjs" || path != "/opt/outline/acceptance-grant-refresh-fixture.cjs" || mode != 0o644 {
					t.Fatal("grant refresh changed detached cleanup copy")
				}
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > time.Minute {
					t.Fatal("grant refresh lost its minute timeout")
				}
				return nil
			},
			exec: func(ctx context.Context, command []string, _ ...tcexec.ProcessOption) (int, io.Reader, error) {
				if ctx.Err() != nil || !reflect.DeepEqual(command, []string{"node", "/opt/outline/acceptance-grant-refresh-fixture.cjs", "remove-duplicate-user", "collection", "group", "target"}) {
					t.Fatal("grant refresh changed detached context or target arguments")
				}
				return 0, strings.NewReader(`OUTLINE_ACCEPTANCE_GRANT_REFRESH={"version":"1.10.1","action":"remove-duplicate-user","collection_id":"collection","group_id":"group"}`), nil
			},
		}
		runGrantRefreshFixture(t, &acceptanceAPI{container: container}, "remove-duplicate-user", "collection", "group", "target")

		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		container.copy = func(context.Context, string, string, int64) error {
			t.Fatal("restore must not recopy the script")
			return nil
		}
		container.exec = func(got context.Context, command []string, _ ...tcexec.ProcessOption) (int, io.Reader, error) {
			if got != ctx || !reflect.DeepEqual(command, []string{"node", "/opt/outline/acceptance-collection-fixture.cjs", "restore", "collection"}) {
				t.Fatal("restore changed detached context or arguments")
			}
			return 0, strings.NewReader("restored"), nil
		}
		if err := acceptanceReleaseRestore(ctx, &acceptanceAPI{container: container}, "collection"); err != nil {
			t.Fatal(err)
		}
	})
}

// Run the real Fatal paths in child test processes so output/error withholding
// is checked at the wrapper boundary, rather than assumed from helper results.
func TestAcceptanceFixtureSensitiveFailures(t *testing.T) {
	const secret = "fixture-secret-must-not-appear"
	const childEnv = "OUTLINE_FIXTURE_FAILURE_TEST"
	if child := os.Getenv(childEnv); child != "" {
		parts := strings.Split(child, "/")
		fixture, step := parts[0], parts[1]
		failure := errors.New(secret)
		container := fakeAcceptanceFixtureContainer{
			copy: func(context.Context, string, string, int64) error {
				if step == "copy" {
					if fixture == "rate" {
						// Copy errors were never withheld for this fixture.
						return errors.New("copy failed")
					}
					return failure
				}
				return nil
			},
			exec: func(context.Context, []string, ...tcexec.ProcessOption) (int, io.Reader, error) {
				switch step {
				case "execute":
					return 0, strings.NewReader(secret), failure
				case "read":
					return 0, io.MultiReader(strings.NewReader(secret), fixtureErrorReader{failure}), nil
				case "nonzero":
					return 9, strings.NewReader(secret), nil
				case "decode":
					marker := "OUTLINE_ACCEPTANCE_OIDC="
					if fixture == "rate" {
						marker = "OUTLINE_ACCEPTANCE_RATE_LIMIT="
					}
					return 0, strings.NewReader(marker + secret), nil
				default:
					return 0, strings.NewReader(secret), nil
				}
			},
		}
		api := &acceptanceAPI{container: container}
		if fixture == "oidc" {
			api.acceptanceInspectOIDC(t, "id")
		} else {
			acceptanceRateLimitSetup(t, api, "inspect", "")
		}
		t.Fatal("wrapper unexpectedly accepted invalid fixture")
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, fixture := range []string{"oidc", "rate"} {
		for _, step := range []string{"copy", "execute", "read", "nonzero", "decode", "missing"} {
			t.Run(fixture+"/"+step, func(t *testing.T) {
				cmd := exec.CommandContext(t.Context(), binary, "-test.run=^TestAcceptanceFixtureSensitiveFailures$", "-test.v")
				cmd.Env = append(os.Environ(), childEnv+"="+fixture+"/"+step)
				output, err := cmd.CombinedOutput()
				if err == nil || strings.Contains(string(output), secret) || strings.Contains(string(output), "unexpectedly accepted") {
					t.Fatalf("wrapper failed to withhold output/error: err=%v output=%s", err, output)
				}
				want := "read-only OIDC observation missing"
				if fixture == "oidc" {
					switch step {
					case "copy", "execute":
						want = step + " read-only OIDC inspection fixture failed"
					case "read", "nonzero":
						want = "read-only OIDC inspection failed, output withheld"
					case "decode":
						want = "decode read-only OIDC observation failed"
					}
				} else {
					switch step {
					case "copy":
						want = "copy failed"
					case "execute":
						want = "execute guarded rate-limit fixture failed"
					case "read", "nonzero":
						code := 0
						if step == "nonzero" {
							code = 9
						}
						want = fmt.Sprintf("guarded rate-limit fixture failed: exit=%d", code)
					case "decode":
						want = "decode guarded rate-limit fixture failed"
					default:
						want = "live stack must use Outline 1.10.1 with its enabled limiter and multiplier-1 quota table"
					}
				}
				if !strings.Contains(string(output), want) {
					t.Fatalf("failure diagnostic changed, want %q: %s", want, output)
				}
			})
		}
	}
}

func TestAcceptanceFixtureMetadataCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	container := fakeAcceptanceFixtureContainer{
		copy: func(got context.Context, _, _ string, _ int64) error { return got.Err() },
	}
	api := &acceptanceAPI{container: container}
	if err := api.acceptanceCollectionGroupFixture(ctx, "archive", "id"); !errors.Is(err, context.Canceled) {
		t.Fatalf("collection group lost parent cancellation: %v", err)
	}
	if _, err := api.acceptanceCollectionUserFixtureMetadata(ctx, "archive", "id"); !errors.Is(err, context.Canceled) {
		t.Fatalf("collection user lost parent cancellation: %v", err)
	}
}

func TestAcceptanceFixtureRateLimitArguments(t *testing.T) {
	data, err := json.Marshal(acceptanceRateLimitFixture{Version: "1.10.1", Enabled: true, Multiplier: 1, Quotas: acceptanceQuotas})
	if err != nil {
		t.Fatal(err)
	}
	container := fakeAcceptanceFixtureContainer{
		copy: func(_ context.Context, source, path string, mode int64) error {
			if source != "../../integration/rate-limit-fixture.cjs" || path != "/opt/outline/acceptance-rate-limit.cjs" || mode != 0o644 {
				t.Fatal("rate-limit copy changed")
			}
			return nil
		},
		exec: func(ctx context.Context, command []string, _ ...tcexec.ProcessOption) (int, io.Reader, error) {
			deadline, ok := ctx.Deadline()
			if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > 30*time.Second || !reflect.DeepEqual(command, []string{"node", "/opt/outline/acceptance-rate-limit.cjs", "exhaust", "users.invite"}) {
				t.Fatal("rate-limit timeout or arguments changed")
			}
			return 0, strings.NewReader("OUTLINE_ACCEPTANCE_RATE_LIMIT=" + string(data)), nil
		},
	}
	acceptanceRateLimitSetup(t, &acceptanceAPI{container: container}, "exhaust", "users.invite")
}
