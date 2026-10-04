// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Only duplicate injection uses the ORM. All reads, pagination, and cleanup use
// the released server's HTTP API and the generated client.
func (a *acceptanceAPI) acceptanceCreateDuplicateGroup(t *testing.T, sourceID string) (string, error) {
	t.Helper()
	if _, err := uuid.Parse(sourceID); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	const fixturePath = "/opt/outline/acceptance-duplicate-group.cjs"
	exitCode, data, failure := runAcceptanceFixture(ctx, a.container, "../../integration/duplicate-group.cjs", fixturePath, sourceID)
	if failure != nil {
		step := failure.step
		if step == "read" {
			return "", fmt.Errorf("read Outline duplicate group fixture output: %w", failure)
		}
		return "", fmt.Errorf("%s Outline duplicate group fixture: %w", step, failure)
	}
	if exitCode != 0 {
		return "", fmt.Errorf("Outline duplicate group fixture exited %d:\n%s", exitCode, data)
	}
	var fixture struct {
		GroupID string `json:"group_id"`
	}
	const marker = "OUTLINE_ACCEPTANCE_DUPLICATE="
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, marker) {
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, marker)), &fixture); err != nil {
				return "", fmt.Errorf("decode Outline duplicate group fixture: %w", err)
			}
		} else if line != "" {
			t.Logf("Outline duplicate group fixture: %s", line)
		}
	}
	if id, err := uuid.Parse(fixture.GroupID); err != nil || id == uuid.Nil || fixture.GroupID == sourceID {
		return "", fmt.Errorf("Outline duplicate group fixture did not return a new group ID")
	}
	return fixture.GroupID, nil
}
