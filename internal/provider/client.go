// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"

	"github.com/glitchedmob/terraform-provider-outline/internal/client"
	"golang.org/x/time/rate"
)

const maxTimeoutSeconds = int64((1<<63 - 1) / time.Second)

// apiClient exposes generated endpoint methods through the authenticated transport.
type apiClient struct {
	*client.ClientWithResponses
	baseURL    string
	apiKey     string
	httpClient *http.Client
	rateLimits *rateLimitDoer
}

func newAPIClient(baseURL, apiKey string, timeoutSeconds int64, version string) (*apiClient, error) {
	return newAPIClientWithRateLimitWait(baseURL, apiKey, timeoutSeconds, defaultRateLimitWaitSeconds, version)
}

func newAPIClientWithRateLimitWait(baseURL, apiKey string, timeoutSeconds, waitSeconds int64, version string) (*apiClient, error) {
	parsedURL, err := url.Parse(baseURL)
	if err != nil {
		// Do not echo URLs that might contain credentials.
		return nil, errors.New("base_url must be a valid HTTP or HTTPS URL")
	}
	if parsedURL.Scheme != "http" && parsedURL.Scheme != "https" {
		return nil, errors.New("base_url must use HTTP or HTTPS")
	}
	if parsedURL.Hostname() == "" || parsedURL.Opaque != "" {
		return nil, errors.New("base_url must include a host")
	}
	if parsedURL.User != nil {
		return nil, errors.New("base_url must not include user credentials")
	}
	if parsedURL.RawQuery != "" || parsedURL.ForceQuery || parsedURL.Fragment != "" || strings.Contains(baseURL, "#") {
		return nil, errors.New("base_url must not include a query string or fragment")
	}
	if apiKey == "" {
		return nil, errors.New("set api_key or OUTLINE_API_KEY to an Outline API key")
	}
	if strings.IndexFunc(apiKey, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return nil, errors.New("api_key must not contain whitespace or control characters")
	}
	if timeoutSeconds < 1 || timeoutSeconds > maxTimeoutSeconds {
		return nil, errors.New("timeout_seconds must be between 1 and 9223372036")
	}

	if waitSeconds < 0 || waitSeconds > maxRateLimitWaitSeconds {
		return nil, errors.New("rate_limit_wait_seconds must be between 0 and 3600")
	}

	httpClient := &http.Client{
		Timeout: time.Duration(timeoutSeconds) * time.Second,
		Transport: &bearerTransport{
			base:      http.DefaultTransport,
			scheme:    parsedURL.Scheme,
			host:      parsedURL.Host,
			apiKey:    apiKey,
			userAgent: "terraform-provider-outline/" + version,
			limiter:   rate.NewLimiter(rate.Limit(5), 1),
		},
		// Do not forward bearer tokens through redirects, even on the same host.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	baseURL = strings.TrimRight(baseURL, "/")
	rateLimits := &rateLimitDoer{
		client:  httpClient,
		baseURL: baseURL,
		budget:  time.Duration(waitSeconds) * time.Second,
		now:     time.Now,
		wait:    waitForRateLimit,
	}
	generated, err := client.NewClientWithResponses(baseURL, client.WithHTTPClient(rateLimits))
	if err != nil {
		return nil, errors.New("unable to create Outline API client")
	}
	return &apiClient{
		ClientWithResponses: generated,
		baseURL:             baseURL,
		apiKey:              apiKey,
		httpClient:          httpClient,
		rateLimits:          rateLimits,
	}, nil
}

type bearerTransport struct {
	base      http.RoundTripper
	scheme    string
	host      string
	apiKey    string
	userAgent string
	limiter   *rate.Limiter
}

func (t *bearerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL.Scheme != t.scheme || request.URL.Host != t.host {
		return nil, errors.New("refusing to send Outline API credentials to a different origin")
	}
	// Pace each attempt. Quota waits happen outside http.Client's network timeout.
	if t.limiter != nil {
		if err := t.limiter.Wait(request.Context()); err != nil {
			return nil, err
		}
	}
	// A RoundTripper must not modify the caller's request or headers.
	cloned := request.Clone(request.Context())
	cloned.Header.Set("Authorization", "Bearer "+t.apiKey)
	cloned.Header.Set("User-Agent", t.userAgent)
	return t.base.RoundTrip(cloned)
}
