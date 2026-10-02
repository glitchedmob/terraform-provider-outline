// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/glitchedmob/terraform-provider-outline/internal/client"
)

var errNotFound = errors.New("outline object not found")

// HTTP errors are not Go errors in the generated client. Only 404 means absent.
func (a *apiClient) checkResponse(operation string, response *http.Response, body []byte, requestErr error) error {
	if requestErr != nil {
		if errors.Is(requestErr, context.Canceled) {
			return fmt.Errorf("%s: request canceled", operation)
		}
		if errors.Is(requestErr, context.DeadlineExceeded) {
			return fmt.Errorf("%s: request deadline exceeded", operation)
		}
		// Decoder and transport errors can include response bodies or URLs. Do not echo them.
		return fmt.Errorf("%s: request failed or response could not be decoded", operation)
	}
	if response == nil {
		return fmt.Errorf("%s: missing HTTP response", operation)
	}
	if response.StatusCode == http.StatusOK {
		return nil
	}
	if response.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%s: %w", operation, errNotFound)
	}
	detail := ""
	var apiError client.Error
	if json.Unmarshal(body, &apiError) == nil {
		if apiError.Error != nil {
			detail = *apiError.Error
		}
		if apiError.Message != nil {
			detail += ": " + *apiError.Message
		}
	}
	// Errors may echo the Authorization header. Redact before truncating.
	detail = strings.ReplaceAll(detail, a.apiKey, "[REDACTED]")
	if len(detail) > 512 {
		detail = detail[:512]
	}
	if response.StatusCode == http.StatusTooManyRequests {
		// Never replay a write. Tell the caller when the server permits another request.
		retry := strings.ReplaceAll(response.Header.Get("Retry-After"), a.apiKey, "[REDACTED]")
		if len(retry) > 64 {
			retry = retry[:64]
		}
		return fmt.Errorf("%s: HTTP 429 rate limited; Retry-After=%q; no automatic retry. %s", operation, retry, detail)
	}
	return fmt.Errorf("%s: HTTP %d %s: %s", operation, response.StatusCode, http.StatusText(response.StatusCode), detail)
}

func checkEnvelope(operation string, ok *bool, status *int) error {
	if ok == nil || !*ok || (status != nil && *status != http.StatusOK) {
		return fmt.Errorf("%s: malformed or unsuccessful response envelope", operation)
	}
	return nil
}

// The release always returns nextPath, even on the last page. Never follow it.
func nextOffset(p *client.PaginationResponse, offset, count int) (int, bool, error) {
	if p == nil || p.Offset == nil || p.Limit == nil || p.Total == nil ||
		*p.Offset != offset || *p.Limit < 1 || *p.Limit > 100 || *p.Total < 0 || count < 0 || count > *p.Limit || offset < 0 || offset > *p.Total || count > *p.Total-offset {
		return 0, false, errors.New("malformed pagination metadata")
	}
	if offset >= *p.Total || count >= *p.Total-offset {
		return 0, false, nil
	}
	if count == 0 || count != *p.Limit {
		return 0, false, errors.New("pagination stopped before the reported total")
	}
	// Addition is bounded by total, avoiding overflow and non-progressing pages.
	return offset + count, true, nil
}
