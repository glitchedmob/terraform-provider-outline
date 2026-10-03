// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/glitchedmob/terraform-provider-outline/internal/client"
	"github.com/google/uuid"
	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

const verifiedRateLimitBody = `{"ok":false,"error":"rate_limit_exceeded","status":429,"message":"Rate limit exceeded for this operation"}`

type rateLimitRoundTripper func(*http.Request) (*http.Response, error)

func (f rateLimitRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func rateLimitHeaders(seconds string) http.Header {
	return http.Header{
		"Content-Type": {"application/json; charset=utf-8"}, "Retry-After": {seconds},
		"Ratelimit-Limit": {"10"}, "Ratelimit-Remaining": {"0"}, "Ratelimit-Reset": {seconds},
	}
}

func rateLimitResponse(body string) *http.Response {
	return &http.Response{StatusCode: 429, Header: rateLimitHeaders("1"), Body: io.NopCloser(strings.NewReader(body))}
}

func rateLimitRequest(t *testing.T) *http.Request {
	t.Helper()
	r, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://outline.example/proxy/api/groups.create", strings.NewReader(`{"name":"Engineering"}`))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func testRateLimitDoer(transport rateLimitRoundTripper) *rateLimitDoer {
	return &rateLimitDoer{
		client:  &http.Client{Transport: transport, Timeout: 30 * time.Second},
		baseURL: "https://outline.example/proxy/api", budget: 120 * time.Second,
		now:  func() time.Time { return time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC) },
		wait: func(context.Context, time.Duration) error { return nil },
	}
}

func TestRetryAfter(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 23, 0, 0, 0, 500_000_000, time.UTC)
	for name, tc := range map[string]struct {
		values []string
		want   time.Duration
	}{
		"seconds":          {[]string{"60"}, 60 * time.Second},
		"whitespace":       {[]string{" 60 "}, 60 * time.Second},
		"date":             {[]string{now.Add(60 * time.Second).Format(http.TimeFormat)}, 59500 * time.Millisecond},
		"near future date": {[]string{now.Add(time.Second).Format(http.TimeFormat)}, time.Second},
		"missing":          {}, "empty": {[]string{""}, 0}, "zero": {[]string{"0"}, 0},
		"negative": {[]string{"-1"}, 0}, "signed": {[]string{"+1"}, 0},
		"fractional": {[]string{"0.1"}, 0}, "overflow": {[]string{"9223372037"}, 0},
		"integer overflow": {[]string{"999999999999999999999999999"}, 0},
		"duplicate":        {[]string{"1", "1"}, 0}, "combined": {[]string{"1, 2"}, 0},
		"past":    {[]string{now.Add(-time.Second).Format(http.TimeFormat)}, 0},
		"invalid": {[]string{"tomorrow"}, 0},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, valid := retryAfter(tc.values, now)
			if got != tc.want || valid != (tc.want > 0) {
				t.Fatalf("got %s/%t, want %s", got, valid, tc.want)
			}
		})
	}
}

func TestRateLimitDeclinesAmbiguousResponses(t *testing.T) {
	t.Parallel()
	for name, change := range map[string]func(*http.Request, *http.Response, *rateLimitDoer){
		"unknown 429": func(_ *http.Request, r *http.Response, _ *rateLimitDoer) {
			r.Body = io.NopCloser(strings.NewReader(`{"error":"rate_limit_exceeded"}`))
		},
		"proxy HTML": func(_ *http.Request, r *http.Response, _ *rateLimitDoer) {
			r.Header.Set("Content-Type", "text/html")
			r.Body = io.NopCloser(strings.NewReader("<h1>Too many requests</h1>"))
		},
		"proxy message": func(_ *http.Request, r *http.Response, _ *rateLimitDoer) {
			r.Body = io.NopCloser(strings.NewReader(strings.Replace(verifiedRateLimitBody, "Rate limit exceeded for this operation", "Proxy quota", 1)))
		},
		"missing ok": func(_ *http.Request, r *http.Response, _ *rateLimitDoer) {
			r.Body = io.NopCloser(strings.NewReader(strings.Replace(verifiedRateLimitBody, `"ok":false,`, "", 1)))
		},
		"wrong status": func(_ *http.Request, r *http.Response, _ *rateLimitDoer) {
			r.Body = io.NopCloser(strings.NewReader(strings.Replace(verifiedRateLimitBody, "429", "500", 1)))
		},
		"success flag": func(_ *http.Request, r *http.Response, _ *rateLimitDoer) {
			r.Body = io.NopCloser(strings.NewReader(strings.Replace(verifiedRateLimitBody, "false", "true", 1)))
		},
		"null flag": func(_ *http.Request, r *http.Response, _ *rateLimitDoer) {
			r.Body = io.NopCloser(strings.NewReader(strings.Replace(verifiedRateLimitBody, "false", "null", 1)))
		},
		"extra data": func(_ *http.Request, r *http.Response, _ *rateLimitDoer) {
			r.Body = io.NopCloser(strings.NewReader(strings.TrimSuffix(verifiedRateLimitBody, "}") + `,"data":{}}`))
		},
		"duplicate field": func(_ *http.Request, r *http.Response, _ *rateLimitDoer) {
			r.Body = io.NopCloser(strings.NewReader(strings.TrimSuffix(verifiedRateLimitBody, "}") + `,"ok":false}`))
		},
		"trailing JSON": func(_ *http.Request, r *http.Response, _ *rateLimitDoer) {
			r.Body = io.NopCloser(strings.NewReader(verifiedRateLimitBody + `{}`))
		},
		"truncated JSON": func(_ *http.Request, r *http.Response, _ *rateLimitDoer) {
			r.Body = io.NopCloser(strings.NewReader(verifiedRateLimitBody[:30]))
		},
		"oversized body": func(_ *http.Request, r *http.Response, _ *rateLimitDoer) {
			r.Body = io.NopCloser(strings.NewReader(strings.Repeat(" ", 2048) + verifiedRateLimitBody))
		},
		"missing header":       func(_ *http.Request, r *http.Response, _ *rateLimitDoer) { r.Header.Del("Retry-After") },
		"zero header":          func(_ *http.Request, r *http.Response, _ *rateLimitDoer) { r.Header.Set("Retry-After", "0") },
		"duplicate header":     func(_ *http.Request, r *http.Response, _ *rateLimitDoer) { r.Header.Add("Retry-After", "1") },
		"missing metadata":     func(_ *http.Request, r *http.Response, _ *rateLimitDoer) { r.Header.Del("RateLimit-Limit") },
		"malformed limit":      func(_ *http.Request, r *http.Response, _ *rateLimitDoer) { r.Header.Set("RateLimit-Limit", "-1") },
		"remaining quota":      func(_ *http.Request, r *http.Response, _ *rateLimitDoer) { r.Header.Set("RateLimit-Remaining", "1") },
		"mismatched reset":     func(_ *http.Request, r *http.Response, _ *rateLimitDoer) { r.Header.Set("RateLimit-Reset", "2") },
		"missing content type": func(_ *http.Request, r *http.Response, _ *rateLimitDoer) { r.Header.Del("Content-Type") },
		"unaudited operation":  func(r *http.Request, _ *http.Response, _ *rateLimitDoer) { r.URL.Path = "/proxy/api/groups.update" },
		"other base path":      func(r *http.Request, _ *http.Response, _ *rateLimitDoer) { r.URL.Path = "/other/api/groups.create" },
		"query":                func(r *http.Request, _ *http.Response, _ *rateLimitDoer) { r.URL.RawQuery = "test=1" },
		"GET":                  func(r *http.Request, _ *http.Response, _ *rateLimitDoer) { r.Method = http.MethodGet },
		"nonreplayable body":   func(r *http.Request, _ *http.Response, _ *rateLimitDoer) { r.GetBody = nil },
		"disabled":             func(_ *http.Request, _ *http.Response, d *rateLimitDoer) { d.budget = 0 },
		"500":                  func(_ *http.Request, r *http.Response, _ *rateLimitDoer) { r.StatusCode = 500 },
		"503":                  func(_ *http.Request, r *http.Response, _ *rateLimitDoer) { r.StatusCode = 503 },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			request, response := rateLimitRequest(t), rateLimitResponse(verifiedRateLimitBody)
			calls := 0
			d := testRateLimitDoer(func(*http.Request) (*http.Response, error) { calls++; return response, nil })
			d.wait = func(context.Context, time.Duration) error { t.Error("ambiguous response caused a wait"); return nil }
			change(request, response, d)
			want, _ := io.ReadAll(response.Body)
			response.Body = io.NopCloser(strings.NewReader(string(want)))
			got, err := d.Do(request)
			if err != nil || calls != 1 || got != response {
				t.Fatalf("replayed or lost response: %v calls=%d", err, calls)
			}
			body, err := io.ReadAll(got.Body)
			_ = got.Body.Close()
			if err != nil || string(body) != string(want) {
				t.Fatalf("changed response body: %q err=%v", body, err)
			}
		})
	}
}

func TestRateLimitBudget(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, seconds string
		budget        time.Duration
		wantCalls     int
		wantWaits     []time.Duration
	}{
		{"repeated limits", "1", 2 * time.Second, 3, []time.Duration{time.Second, time.Second}},
		{"hourly limit fails immediately", "3600", 120 * time.Second, 1, nil},
		{"short budget fails immediately", "60", 59 * time.Second, 1, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			calls := 0
			var waits []time.Duration
			d := testRateLimitDoer(func(*http.Request) (*http.Response, error) {
				calls++
				r := rateLimitResponse(verifiedRateLimitBody)
				r.Header = rateLimitHeaders(tc.seconds)
				return r, nil
			})
			d.budget = tc.budget
			d.wait = func(_ context.Context, delay time.Duration) error { waits = append(waits, delay); return nil }
			_, err := d.Do(rateLimitRequest(t))
			if !errors.Is(err, errRateLimitWaitBudget) || calls != tc.wantCalls || !reflect.DeepEqual(waits, tc.wantWaits) {
				t.Fatalf("budget not bounded: err=%v calls=%d waits=%v", err, calls, waits)
			}
			api := &apiClient{apiKey: "secret"}
			if diag := api.checkResponse("groups.create", nil, nil, err); !strings.Contains(diag.Error(), "HTTP 429") || !strings.Contains(diag.Error(), "rate_limit_wait_seconds budget exhausted") {
				t.Fatalf("missing actionable budget diagnostic: %v", diag)
			}
		})
	}
}

type rateLimitTrackedBody struct {
	io.Reader
	closed   bool
	closeErr error
}

func (b *rateLimitTrackedBody) Close() error { b.closed = true; return b.closeErr }

func TestRateLimitAttemptLimit(t *testing.T) {
	t.Parallel()
	calls, waits := 0, 0
	d := testRateLimitDoer(func(*http.Request) (*http.Response, error) {
		calls++
		return rateLimitResponse(verifiedRateLimitBody), nil
	})
	d.budget = time.Hour
	d.wait = func(context.Context, time.Duration) error { waits++; return nil }
	_, err := d.Do(rateLimitRequest(t))
	if !errors.Is(err, errRateLimitRetryLimit) || calls != 4 || waits != 3 {
		t.Fatalf("unbounded retry attempts: err=%v calls=%d waits=%d", err, calls, waits)
	}
	api := &apiClient{apiKey: "secret"}
	if diag := api.checkResponse("groups.create", nil, nil, err); !strings.Contains(diag.Error(), "automatic retry limit exhausted") {
		t.Fatalf("missing retry-limit diagnostic: %v", diag)
	}
}

func TestRateLimitBodyLifecycle(t *testing.T) {
	t.Parallel()
	for _, failure := range []string{"", "close", "rebuild", "read", "transport", "deadline"} {
		t.Run(failure, func(t *testing.T) {
			t.Parallel()
			calls, waits := 0, 0
			body := &rateLimitTrackedBody{Reader: strings.NewReader(verifiedRateLimitBody)}
			if failure == "close" {
				body.closeErr = errors.New("close failed")
			}
			if failure == "read" {
				body.Reader = rateLimitFailReader{}
			}
			d := testRateLimitDoer(func(r *http.Request) (*http.Response, error) {
				calls++
				if failure == "transport" {
					return nil, errors.New("ambiguous transport")
				}
				if failure == "deadline" {
					return nil, context.DeadlineExceeded
				}
				payload, err := io.ReadAll(r.Body)
				_ = r.Body.Close()
				if err != nil || string(payload) != `{"name":"Engineering"}` {
					t.Errorf("request body not rebuilt: %q %v", payload, err)
				}
				response := rateLimitResponse(verifiedRateLimitBody)
				if calls == 1 {
					response.Body = body
				} else {
					response.StatusCode = 200
				}
				return response, nil
			})
			d.wait = func(context.Context, time.Duration) error {
				waits++
				if !body.closed {
					t.Error("response body open during wait")
				}
				return nil
			}
			request := rateLimitRequest(t)
			if failure == "rebuild" {
				request.GetBody = func() (io.ReadCloser, error) { return nil, errors.New("cannot rebuild") }
			}
			response, err := d.Do(request)
			if response != nil {
				_ = response.Body.Close()
			}
			if failure == "" {
				if err != nil || calls != 2 || waits != 1 || !body.closed {
					t.Fatalf("retry lifecycle: %v calls=%d waits=%d closed=%t", err, calls, waits, body.closed)
				}
			} else if err == nil || calls != 1 {
				t.Fatalf("replayed ambiguous outcome: %v calls=%d", err, calls)
			}
		})
	}
}

type rateLimitFailReader struct{}

func (rateLimitFailReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestRateLimitWaitOutsideAttemptTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		d := testRateLimitDoer(func(*http.Request) (*http.Response, error) {
			calls++
			time.Sleep(time.Second)
			r := rateLimitResponse(verifiedRateLimitBody)
			r.Header = rateLimitHeaders("60")
			if calls == 2 {
				r.StatusCode = 200
			}
			return r, nil
		})
		d.now, d.wait = time.Now, waitForRateLimit
		start := time.Now()
		response, err := d.Do(rateLimitRequest(t))
		if err != nil || calls != 2 || time.Since(start) != 62*time.Second {
			t.Fatalf("network timeout cut off quota wait: %v calls=%d elapsed=%s", err, calls, time.Since(start))
		}
		_ = response.Body.Close()
	})
}

func TestRateLimitWaitCancellation(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(fmt.Sprint(deadline), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				calls := 0
				d := testRateLimitDoer(func(*http.Request) (*http.Response, error) {
					calls++
					r := rateLimitResponse(verifiedRateLimitBody)
					r.Header = rateLimitHeaders("60")
					return r, nil
				})
				d.now, d.wait = time.Now, waitForRateLimit
				var ctx context.Context
				var cancel context.CancelFunc
				want := context.Canceled
				if deadline {
					ctx, cancel = context.WithTimeout(t.Context(), time.Second)
					want = context.DeadlineExceeded
				} else {
					ctx, cancel = context.WithCancel(t.Context())
					go func() { time.Sleep(time.Second); cancel() }()
				}
				defer cancel()
				_, err := d.Do(rateLimitRequest(t).WithContext(ctx))
				if !errors.Is(err, want) || calls != 1 {
					t.Fatalf("canceled wait replayed: %v calls=%d", err, calls)
				}
			})
		})
	}
}

func TestRateLimitGeneratedOperationsConcurrent(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	counts := make(map[string]int)
	var waits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Error("lost bearer key")
		}
		key := r.URL.Path + string(body)
		mu.Lock()
		counts[key]++
		count := counts[key]
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if count == 1 {
			for key, values := range rateLimitHeaders("1") {
				w.Header()[key] = values
			}
			w.WriteHeader(429)
			groupTestWrite(t, w, verifiedRateLimitBody)
			return
		}
		groupTestWrite(t, w, `{"ok":true,"status":200,"success":true}`)
	}))
	defer server.Close()
	api, err := newAPIClient(server.URL+"/proxy/api", "test-key", 30, "test")
	if err != nil {
		t.Fatal(err)
	}
	api.httpClient.Transport.(*bearerTransport).limiter = nil
	// All twelve calls can spend this one-second per-call budget. A shared
	// counter would incorrectly fail eleven of them.
	api.rateLimits.budget = time.Second
	api.rateLimits.wait = func(_ context.Context, delay time.Duration) error {
		if delay != time.Second {
			t.Error("wrong wait")
		}
		waits.Add(1)
		return nil
	}
	var wg sync.WaitGroup
	for i := range 3 {
		for _, op := range []string{"groups.create", "users.invite", "collections.add_user", "users.delete"} {
			wg.Go(func() {
				var err error
				switch op {
				case "groups.create":
					_, err = api.GroupsCreateWithResponse(t.Context(), client.GroupsCreateJSONRequestBody{Name: fmt.Sprint(i)})
				case "users.invite":
					_, err = api.UsersInviteWithResponse(t.Context(), client.UsersInviteJSONRequestBody{Invites: []client.Invite{{Name: fmt.Sprint(i), Email: fmt.Sprintf("%d@example.com", i), Role: client.UserRoleMember}}})
				case "collections.add_user":
					_, err = api.CollectionsAddUserWithResponse(t.Context(), client.CollectionsAddUserJSONRequestBody{Id: uuid.New(), UserId: uuid.New()})
				case "users.delete":
					_, err = api.UsersDeleteWithResponse(t.Context(), client.UsersDeleteJSONRequestBody{Id: uuid.New()})
				}
				if err != nil {
					t.Error(err)
				}
			})
		}
	}
	wg.Wait()
	if len(counts) != 12 || waits.Load() != 12 {
		t.Fatalf("concurrent calls shared retry state: requests=%v waits=%d", counts, waits.Load())
	}
	for key, count := range counts {
		if count != 2 {
			t.Errorf("body replay changed %s: %d attempts", key, count)
		}
	}
}

func TestRateLimitDateAndInstanceIsolation(t *testing.T) {
	t.Parallel()
	calls := 0
	d := testRateLimitDoer(func(*http.Request) (*http.Response, error) {
		calls++
		r := rateLimitResponse(verifiedRateLimitBody)
		r.Header.Set("Retry-After", time.Date(2026, 9, 23, 0, 1, 0, 0, time.UTC).Format(http.TimeFormat))
		r.Header.Set("RateLimit-Reset", "60")
		if calls == 2 {
			r.StatusCode = 200
		}
		return r, nil
	})
	var delay time.Duration
	d.wait = func(_ context.Context, wait time.Duration) error { delay = wait; return nil }
	r, err := d.Do(rateLimitRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	_ = r.Body.Close()
	if calls != 2 || delay != 60*time.Second {
		t.Fatalf("HTTP date wait: calls=%d delay=%s", calls, delay)
	}
	first, err := newAPIClientWithRateLimitWait("https://first.example/api", "first-key", 30, 0, "test")
	if err != nil {
		t.Fatal(err)
	}
	second, err := newAPIClient("https://second.example/api", "second-key", 30, "test")
	if err != nil {
		t.Fatal(err)
	}
	if first.rateLimits == second.rateLimits || first.rateLimits.budget != 0 || second.rateLimits.budget != 120*time.Second || first.rateLimits.client == second.rateLimits.client {
		t.Fatal("provider instances shared retry configuration")
	}
	for _, invalid := range []int64{-1, 3601} {
		if api, err := newAPIClientWithRateLimitWait(defaultBaseURL, "test-key", 30, invalid, "test"); api != nil || err == nil {
			t.Fatal("invalid retry budget accepted")
		}
	}
}

func TestRateLimitDoesNotReplayAfterVerifiedRejection(t *testing.T) {
	t.Parallel()
	for _, failure := range []string{"transport", "timeout", "500", "503", "malformed 429"} {
		t.Run(failure, func(t *testing.T) {
			t.Parallel()
			calls, waits := 0, 0
			d := testRateLimitDoer(func(*http.Request) (*http.Response, error) {
				calls++
				if calls == 1 {
					return rateLimitResponse(verifiedRateLimitBody), nil
				}
				switch failure {
				case "transport":
					return nil, errors.New("connection lost after write")
				case "timeout":
					return nil, context.DeadlineExceeded
				case "500", "503":
					response := rateLimitResponse(verifiedRateLimitBody)
					if failure == "500" {
						response.StatusCode = 500
					} else {
						response.StatusCode = 503
					}
					return response, nil
				default:
					return rateLimitResponse(`{}`), nil
				}
			})
			d.wait = func(context.Context, time.Duration) error { waits++; return nil }
			response, err := d.Do(rateLimitRequest(t))
			if response != nil {
				_ = response.Body.Close()
			}
			if calls != 2 || waits != 1 || (failure == "transport" || failure == "timeout") && err == nil {
				t.Fatalf("replayed ambiguous second attempt: calls=%d waits=%d err=%v", calls, waits, err)
			}
		})
	}
}

func TestRateLimitWaitOvershootAndCancellationBeforeReplay(t *testing.T) {
	t.Parallel()
	for _, canceled := range []bool{false, true} {
		t.Run(fmt.Sprint(canceled), func(t *testing.T) {
			t.Parallel()
			calls := 0
			d := testRateLimitDoer(func(*http.Request) (*http.Response, error) {
				calls++
				return rateLimitResponse(verifiedRateLimitBody), nil
			})
			d.budget = time.Second
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			now := d.now()
			d.now = func() time.Time { return now }
			d.wait = func(context.Context, time.Duration) error {
				if canceled {
					cancel()
				} else {
					now = now.Add(2 * time.Second)
				}
				return nil
			}
			_, err := d.Do(rateLimitRequest(t).WithContext(ctx))
			want := errRateLimitWaitBudget
			if canceled {
				want = context.Canceled
			}
			if !errors.Is(err, want) || calls != 1 {
				t.Fatalf("replayed after cancellation/overshoot: %v calls=%d", err, calls)
			}
		})
	}
}

func TestProtocol6RateLimitWaitBudget(t *testing.T) {
	t.Parallel()
	for _, value := range []any{nil, 0, 120, 3600, tftypes.UnknownValue, -1, 3601, 1.5} {
		t.Run(fmt.Sprint(value), func(t *testing.T) {
			t.Parallel()
			server := providerserver.NewProtocol6(New("test")())()
			response, err := server.ValidateProviderConfig(t.Context(), &tfprotov6.ValidateProviderConfigRequest{Config: testProtocolConfig(t, map[string]any{"rate_limit_wait_seconds": value})})
			wantError := value == -1 || value == 3601 || value == 1.5
			if err != nil || protocolHasError(response.Diagnostics) != wantError {
				t.Fatalf("unexpected budget validation: %v %v", response, err)
			}
		})
	}
}
