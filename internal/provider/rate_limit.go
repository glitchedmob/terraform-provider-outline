// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const defaultRateLimitWaitSeconds int64 = 120
const maxRateLimitWaitSeconds int64 = 3600
const maxRateLimitErrorBytes = 1024
const maxRateLimitRetries = 3

var errRateLimitWaitBudget = errors.New("HTTP 429 rate limited; rate_limit_wait_seconds budget exhausted; wait for the quota to reset or explicitly increase the budget")
var errRateLimitRetryLimit = errors.New("HTTP 429 rate limited; automatic retry limit exhausted after three retries; wait for the quota to reset before applying again")

// rateLimitDoer wraps, rather than runs inside, http.Client. Each network attempt
// retains its timeout; a verified quota wait does not consume that timeout.
// Only the four audited v1.10.1 routes below may replay a rejected write. See
// openapi/README.md for middleware execution order and pinned primary sources.
// All mutable retry state is local to Do, including the total wait budget.
type rateLimitDoer struct {
	client  *http.Client
	baseURL string
	budget  time.Duration
	now     func() time.Time
	wait    func(context.Context, time.Duration) error
}

func (d *rateLimitDoer) Do(request *http.Request) (*http.Response, error) {
	remaining := d.budget
	current := request
	for retries := 0; ; retries++ {
		response, err := d.client.Do(current)
		if err != nil || response.StatusCode != http.StatusTooManyRequests || d.budget == 0 || !d.audited(request) || request.GetBody == nil {
			return response, err
		}
		delay, valid := retryAfter(response.Header.Values("Retry-After"), d.now())
		if !valid || !rateLimitMetadata(response.Header, delay) || !jsonContentType(response.Header.Values("Content-Type")) {
			return response, nil
		}
		// Inspect only a bounded error body. Restore every byte read for callers
		// if it is not the exact pre-mutation error, including truncated bodies.
		body, readErr := io.ReadAll(io.LimitReader(response.Body, maxRateLimitErrorBytes+1))
		response.Body = &restoredResponseBody{Reader: io.MultiReader(bytes.NewReader(body), response.Body), closer: response.Body}
		if readErr != nil {
			_ = response.Body.Close()
			if errors.Is(readErr, context.Canceled) {
				return nil, context.Canceled
			}
			if errors.Is(readErr, context.DeadlineExceeded) {
				return nil, context.DeadlineExceeded
			}
			return nil, errors.New("unable to read Outline rate limit response")
		}
		if len(body) > maxRateLimitErrorBytes || !preMutationRateLimit(body) {
			return response, nil
		}
		// Close before sleeping, so connections and http.Client's per-attempt
		// timeout are released. A body close failure must not replay a write.
		if err := response.Body.Close(); err != nil {
			return nil, errors.New("unable to close Outline rate limit response")
		}
		if delay > remaining {
			return nil, errRateLimitWaitBudget
		}
		if retries >= maxRateLimitRetries {
			return nil, errRateLimitRetryLimit
		}
		start := d.now()
		if err := d.wait(request.Context(), delay); err != nil {
			return nil, err
		}
		// Account for timer overshoot as well as the requested delay. The
		// minimum one-second wait also bounds attempts, without a busy loop.
		elapsed := max(delay, d.now().Sub(start))
		if elapsed > remaining {
			return nil, errRateLimitWaitBudget
		}
		remaining -= elapsed
		if err := request.Context().Err(); err != nil {
			return nil, err
		}
		bodyReader, err := request.GetBody()
		if err != nil {
			return nil, errors.New("unable to rebuild Outline request body")
		}
		current = request.Clone(request.Context())
		current.Body = bodyReader
	}
}

func (d *rateLimitDoer) audited(request *http.Request) bool {
	if request.Method != http.MethodPost || request.URL.RawQuery != "" || request.URL.ForceQuery || request.URL.Fragment != "" {
		return false
	}
	// Match the complete configured URL, not a path suffix supplied by a
	// request editor. bearerTransport independently checks origin on each try.
	for _, operation := range []string{"groups.create", "users.invite", "collections.add_user", "users.delete"} {
		if request.URL.String() == d.baseURL+"/"+operation {
			return true
		}
	}
	return false
}

func waitForRateLimit(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Retry-After supports positive integer seconds and HTTP dates. Zero, past,
// fractional, signed, duplicate and overflowing values are not replay evidence.
func retryAfter(values []string, now time.Time) (time.Duration, bool) {
	if len(values) != 1 {
		return 0, false
	}
	value := strings.TrimSpace(values[0])
	if value == "" {
		return 0, false
	}
	if strings.IndexFunc(value, func(r rune) bool { return r < '0' || r > '9' }) < 0 {
		seconds, err := strconv.ParseInt(value, 10, 64)
		if err != nil || seconds < 1 || seconds > maxTimeoutSeconds {
			return 0, false
		}
		return time.Duration(seconds) * time.Second, true
	}
	date, err := http.ParseTime(value)
	if err != nil || !date.After(now) {
		return 0, false
	}
	return max(time.Second, date.Sub(now)), true
}

// The pinned middleware emits all three quota headers. Require a depleted
// bucket and a reset consistent with Retry-After, rather than trusting a proxy's
// generic 429. Limits may differ because Outline supports a quota multiplier.
func rateLimitMetadata(header http.Header, delay time.Duration) bool {
	limit, ok := positiveHeaderSeconds(header.Values("RateLimit-Limit"))
	if !ok || limit < 1 {
		return false
	}
	remaining := header.Values("RateLimit-Remaining")
	if len(remaining) != 1 || remaining[0] != "0" {
		return false
	}
	reset, ok := positiveHeaderSeconds(header.Values("RateLimit-Reset"))
	if !ok {
		return false
	}
	// HTTP dates lose subsecond precision. Accept only the matching rounded
	// up second, never an earlier reset or a different window.
	resetDelay := time.Duration(reset) * time.Second
	return resetDelay >= delay && resetDelay-delay < time.Second
}

func positiveHeaderSeconds(values []string) (int64, bool) {
	if len(values) != 1 || values[0] == "" || strings.IndexFunc(values[0], func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
		return 0, false
	}
	value, err := strconv.ParseInt(values[0], 10, 64)
	return value, err == nil && value > 0 && value <= maxTimeoutSeconds
}

func jsonContentType(values []string) bool {
	if len(values) != 1 {
		return false
	}
	mediaType, _, err := mime.ParseMediaType(values[0])
	return err == nil && mediaType == "application/json"
}

func preMutationRateLimit(body []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(body))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return false
	}
	want := map[string]string{
		"ok": "false", "status": "429", "error": `"rate_limit_exceeded"`,
		"message": `"Rate limit exceeded for this operation"`,
	}
	seen := make(map[string]bool, len(want))
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return false
		}
		name, ok := key.(string)
		if !ok || seen[name] || want[name] == "" {
			return false
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil || string(value) != want[name] {
			return false
		}
		seen[name] = true
	}
	token, err = decoder.Token()
	if err != nil || token != json.Delim('}') || len(seen) != len(want) {
		return false
	}
	return decoder.Decode(new(any)) == io.EOF
}

type restoredResponseBody struct {
	io.Reader
	closer io.Closer
}

func (b *restoredResponseBody) Close() error { return b.closer.Close() }
