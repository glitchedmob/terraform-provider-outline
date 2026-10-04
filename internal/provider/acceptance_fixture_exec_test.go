// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"io"

	tcexec "github.com/testcontainers/testcontainers-go/exec"
)

// Only the container operations are shared. Callers own deadlines, exit-code
// checks, output disclosure, and fixture-specific decoding and assertions.
type acceptanceFixtureContainer interface {
	CopyFileToContainer(context.Context, string, string, int64) error
	Exec(context.Context, []string, ...tcexec.ProcessOption) (int, io.Reader, error)
}

// Keep the failing step separate from its error so protected fixtures can
// report a failure without printing either the underlying error or output.
type acceptanceFixtureError struct {
	step string
	err  error
}

func (e *acceptanceFixtureError) Error() string { return e.err.Error() }
func (e *acceptanceFixtureError) Unwrap() error { return e.err }

func runAcceptanceFixture(ctx context.Context, container acceptanceFixtureContainer, source, path string, args ...string) (int, []byte, *acceptanceFixtureError) {
	if err := container.CopyFileToContainer(ctx, source, path, 0o644); err != nil {
		return 0, nil, &acceptanceFixtureError{step: "copy", err: err}
	}
	return execAcceptanceFixture(ctx, container, path, args...)
}

// Restore cleanup can reuse an already copied script without changing its
// detached context or introducing another copy operation.
func execAcceptanceFixture(ctx context.Context, container acceptanceFixtureContainer, path string, args ...string) (int, []byte, *acceptanceFixtureError) {
	command := append([]string{"node", path}, args...)
	code, output, err := container.Exec(ctx, command, tcexec.Multiplexed())
	if err != nil {
		return code, nil, &acceptanceFixtureError{step: "execute", err: err}
	}
	data, err := io.ReadAll(output)
	if err != nil {
		return code, data, &acceptanceFixtureError{step: "read", err: err}
	}
	return code, data, nil
}
