// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"testing"

	"github.com/glitchedmob/terraform-provider-outline/internal/client"
)

func TestPaginationStatePages(t *testing.T) {
	for _, test := range []struct {
		name      string
		state     paginationState
		offset    int
		limit     int
		total     int
		count     int
		wantMore  bool
		wantState paginationState
	}{
		{name: "first page", limit: 100, total: 201, count: 100, wantMore: true, wantState: paginationState{offset: 100, total: 201, started: true}},
		{name: "server lowers limit", limit: 2, total: 3, count: 2, wantMore: true, wantState: paginationState{offset: 2, total: 3, started: true}},
		{name: "single full page", limit: 100, total: 100, count: 100, wantState: paginationState{total: 100, started: true}},
		{name: "single short page", limit: 100, total: 1, count: 1, wantState: paginationState{total: 1, started: true}},
		{name: "empty list", limit: 100, wantState: paginationState{started: true}},
		{name: "final page", state: paginationState{offset: 100, total: 101, started: true}, offset: 100, limit: 100, total: 101, count: 1, wantState: paginationState{offset: 100, total: 101, started: true}},
		{name: "empty at total", state: paginationState{offset: 100, total: 100, started: true}, offset: 100, limit: 100, total: 100, wantState: paginationState{offset: 100, total: 100, started: true}},
	} {
		t.Run(test.name, func(t *testing.T) {
			// nextPath is untrusted and may be present even on the final page.
			path := "https://untrusted.invalid/next"
			page := &client.PaginationResponse{Offset: &test.offset, Limit: &test.limit, Total: &test.total, NextPath: &path}
			more, err := test.state.advance(page, test.count)
			if err != nil || more != test.wantMore || test.state != test.wantState {
				t.Fatalf("advance = %v, %v, state %+v; want %v, %+v", more, err, test.state, test.wantMore, test.wantState)
			}
		})
	}
}

func TestPaginationStateRejectsInvalidPages(t *testing.T) {
	const malformed = "malformed pagination metadata"
	const premature = "pagination stopped before the reported total"
	const changed = "total changed during pagination; retry when the list is stable"
	maxInt := int(^uint(0) >> 1)
	for _, test := range []struct {
		name    string
		state   paginationState
		offset  int
		limit   int
		total   int
		count   int
		missing string
		want    string
	}{
		{name: "missing pagination", missing: "page", want: malformed},
		{name: "missing offset", limit: 100, missing: "offset", want: malformed},
		{name: "missing limit", missing: "limit", want: malformed},
		{name: "missing total", limit: 100, missing: "total", want: malformed},
		{name: "zero limit", want: malformed},
		{name: "negative limit", limit: -1, want: malformed},
		{name: "limit above maximum", limit: 101, want: malformed},
		{name: "negative total", limit: 100, total: -1, want: malformed},
		{name: "negative count", limit: 100, count: -1, want: malformed},
		{name: "count exceeds limit", limit: 100, total: 101, count: 101, want: malformed},
		{name: "count exceeds total", limit: 100, total: 1, count: 2, want: malformed},
		{name: "wrong offset", offset: 1, limit: 100, total: 2, count: 1, want: malformed},
		{name: "negative offset", state: paginationState{offset: -1}, offset: -1, limit: 100, want: malformed},
		{name: "offset beyond total", state: paginationState{offset: 2}, offset: 2, limit: 100, total: 1, want: malformed},
		{name: "empty first page before total", limit: 100, total: 1, want: premature},
		{name: "short first page before total", limit: 100, total: 2, count: 1, want: premature},
		{name: "empty later page before total", state: paginationState{offset: 100, total: 101, started: true}, offset: 100, limit: 100, total: 101, want: premature},
		{name: "changed nonfinal total", state: paginationState{offset: 100, total: 201, started: true}, offset: 100, limit: 100, total: 202, count: 100, want: changed},
		{name: "changed final total decreases", state: paginationState{offset: 100, total: 201, started: true}, offset: 100, limit: 100, total: 101, count: 1, want: changed},
		{name: "changed final total increases", state: paginationState{offset: 100, total: 101, started: true}, offset: 100, limit: 100, total: 102, count: 2, want: changed},
		{name: "changed empty final total", state: paginationState{offset: 100, total: 101, started: true}, offset: 100, limit: 100, total: 100, want: changed},
		{name: "zero total is remembered", state: paginationState{started: true}, limit: 100, total: 1, count: 1, want: changed},
		{name: "addition would overflow", state: paginationState{offset: maxInt - 1}, offset: maxInt - 1, limit: 100, total: maxInt, count: 2, want: malformed},
		{name: "count would overflow", limit: 100, total: maxInt, count: maxInt, want: malformed},
	} {
		t.Run(test.name, func(t *testing.T) {
			page := &client.PaginationResponse{Offset: &test.offset, Limit: &test.limit, Total: &test.total}
			switch test.missing {
			case "page":
				page = nil
			case "offset":
				page.Offset = nil
			case "limit":
				page.Limit = nil
			case "total":
				page.Total = nil
			}
			before := test.state
			more, err := test.state.advance(page, test.count)
			if more || err == nil || err.Error() != test.want || test.state != before {
				t.Fatalf("advance = %v, %v, state %+v; want false, %q, unchanged %+v", more, err, test.state, test.want, before)
			}
		})
	}
}

func TestPaginationStateRepeatedProgression(t *testing.T) {
	var state paginationState
	limit, total := 2, 5
	for _, offset := range []int{0, 2, 4} {
		if state.offset != offset {
			t.Fatalf("offset = %d, want %d", state.offset, offset)
		}
		count := min(limit, total-offset)
		page := &client.PaginationResponse{Offset: &offset, Limit: &limit, Total: &total}
		more, err := state.advance(page, count)
		if err != nil || more != (offset < 4) {
			t.Fatalf("offset %d: advance = %v, %v", offset, more, err)
		}
		if more {
			before := state
			if more, err = state.advance(page, count); more || err == nil || err.Error() != "malformed pagination metadata" || state != before {
				t.Fatalf("repeated offset %d: advance = %v, %v, state %+v", offset, more, err, state)
			}
		}
	}
}

func TestPaginationStateBoundedArithmetic(t *testing.T) {
	maxInt := int(^uint(0) >> 1)
	state := paginationState{offset: maxInt - 2}
	limit, total, offset := 1, maxInt, maxInt-2
	page := &client.PaginationResponse{Offset: &offset, Limit: &limit, Total: &total}
	if more, err := state.advance(page, 1); !more || err != nil || state.offset != maxInt-1 {
		t.Fatalf("near maximum: advance = %v, %v, state %+v", more, err, state)
	}
	offset = state.offset
	if more, err := state.advance(page, 1); more || err != nil || state.offset != maxInt-1 {
		t.Fatalf("at maximum: advance = %v, %v, state %+v", more, err, state)
	}
}
