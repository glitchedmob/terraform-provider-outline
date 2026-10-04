// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"errors"

	"github.com/glitchedmob/terraform-provider-outline/internal/client"
)

// paginationState starts at offset zero. Call advance only after validating the
// page's envelope and rows; a match is not usable until advance returns false.
type paginationState struct {
	offset  int
	total   int
	started bool
}

func (s *paginationState) advance(page *client.PaginationResponse, count int) (bool, error) {
	next, more, err := nextOffset(page, s.offset, count)
	if err != nil {
		return false, err
	}
	// Check stability even when nextOffset reports the final page.
	if s.started && s.total != *page.Total {
		return false, errors.New("total changed during pagination; retry when the list is stable")
	}
	s.total, s.started = *page.Total, true
	if more {
		s.offset = next
	}
	return more, nil
}
