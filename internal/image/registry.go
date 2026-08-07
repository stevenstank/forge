package image

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Defaults for ClientConfig. Each is a decision, so each is a named constant
// with the reasoning attached.
const (
	// defaultRequestTimeout bounds a single HTTP request. It has to be generous
	// enough for a large layer over a slow link, because it covers the whole
	// body transfer, not just the response headers.
	defaultRequestTimeout = 10 * time.Minute

	// defaultMaxManifestBytes caps a manifest document. The spec gives no
	// number, and without one a hostile registry could make Forge buffer a
	// gigabyte of "manifest" — manifests are read into memory because they must
	// be hashed before they can be trusted enough to parse.
	defaultMaxManifestBytes = 4 << 20

	// defaultMaxAttempts is the total number of tries for one idempotent GET.
	defaultMaxAttempts = 3

	// defaultBackoff is the delay before the second attempt; it doubles after.
	defaultBackoff = 200 * time.Millisecond

	// maxRetryAfter bounds how long a registry's Retry-After can park a run.
	// Honouring an unbounded one would let a registry hang forge indefinitely.
	maxRetryAfter = 10 * time.Second

	// maxIndexDepth bounds index-to-index indirection. One hop (index →
	// manifest) is normal; more is a loop or a hostile registry.
	maxIndexDepth = 2
)

// defaultUserAgent identifies Forge to registries that log or rate-limit by
// client.
const defaultUserAgent = "forge/0.5 (+https://github.com/stevenstank/forge)"

// ClientConfig configures a Client. The zero value is usable and gives every
// default above.
type ClientConfig struct {
	// HTTPClient is the transport to use. Nil means one with
	// defaultRequestTimeout. Supplying one is how tests point the client at an
	// httptest.Server, and is the reason this package defines no interfaces:
	// the only seam a test needs is already a concrete, injectable type.
	HTTPClient *http.Client

	// UserAgent is sent with every request. Empty means defaultUserAgent.
	UserAgent string

	// MaxManifestBytes caps a manifest document. Zero means the default.
	MaxManifestBytes int64

	// MaxAttempts is the total number of tries for one idempotent request.
	// Zero means the default; one disables retrying.
	MaxAttempts int

	// Backoff is the delay before the second attempt, doubling thereafter.
	// Zero means the default.
	Backoff time.Duration

	// PlainHTTP sends requests over http:// instead of https://. It exists for
	// tests against a loopback registry and for a local registry that has no
	// certificate; it is never a default.
	PlainHTTP bool
}

// Client is an OCI Distribution Spec client.
//
// It speaks HTTP and knows names. It never learns a path on disk: blobs leave
// through an io.Writer the caller supplies, so the client cannot create, and
// therefore cannot leak, a file.
//
// A Client is safe for concurrent use.
type Client struct {
	http      *http.Client
	userAgent string
	logger    *slog.Logger

	maxManifestBytes int64
	maxAttempts      int
	backoff          time.Duration
	plainHTTP        bool

	// tokens caches one anonymous bearer token per registry+repository scope.
	// A pull makes several requests against one scope, and re-running the token
	// dance for each would triple the request count for no benefit.
	mu     sync.Mutex
	tokens map[string]string
}

// New returns a Client.
//
// It performs no I/O: no connection, no DNS lookup, no token. Construction
// cannot fail for a reason the caller could only discover later, which is why
// a Client can be built before Forge knows whether it will pull anything.
func New(logger *slog.Logger, cfg ClientConfig) (*Client, error) {
	if logger == nil {
		logger = slog.Default()
	}

	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultRequestTimeout}
	}

	c := &Client{
		http:             httpClient,
		userAgent:        cfg.UserAgent,
		logger:           logger,
		maxManifestBytes: cfg.MaxManifestBytes,
		maxAttempts:      cfg.MaxAttempts,
		backoff:          cfg.Backoff,
		plainHTTP:        cfg.PlainHTTP,
		tokens:           make(map[string]string),
	}
	if c.userAgent == "" {
		c.userAgent = defaultUserAgent
	}
	if c.maxManifestBytes <= 0 {
		c.maxManifestBytes = defaultMaxManifestBytes
	}
	if c.maxAttempts <= 0 {
		c.maxAttempts = defaultMaxAttempts
	}
	if c.backoff <= 0 {
		c.backoff = defaultBackoff
	}

	return c, nil
}

// endpoint builds a /v2/ URL for a reference's registry.
func (c *Client) endpoint(ref Reference, elem ...string) string {
	scheme := "https"
	if c.plainHTTP {
		scheme = "http"
	}
	return scheme + "://" + ref.Host() + "/v2/" + ref.Repository + "/" + strings.Join(elem, "/")
}

// get performs an authenticated GET, retrying the failures that retrying can
// fix and no others.
//
// The caller owns the returned body and must close it. On a non-2xx status the
// body is drained and closed here, and the error carries the status.
//
// Authentication is lazy: the first request goes out anonymous, and a 401 with
// a Bearer challenge triggers exactly one token acquisition and one retry. That
// ordering is what lets Forge talk to a registry that needs no token at all
// without knowing in advance which kind it is.
func (c *Client) get(ctx context.Context, ref Reference, rawURL string, accept []string) (*http.Response, error) {
	scope := tokenScope(ref)

	resp, err := c.attempt(ctx, rawURL, accept, c.cachedToken(scope))
	if err != nil {
		return nil, err
	}

	if resp.StatusCode == http.StatusUnauthorized {
		challenge := resp.Header.Get("WWW-Authenticate")
		drain(resp, c.logger)

		token, err := c.authenticate(ctx, ref, challenge)
		if err != nil {
			return nil, err
		}
		c.cacheToken(scope, token)

		if resp, err = c.attempt(ctx, rawURL, accept, token); err != nil {
			return nil, err
		}
	}

	if resp.StatusCode/100 != 2 {
		return nil, c.statusError(resp, rawURL)
	}

	return resp, nil
}

// attempt performs one GET with retries for transport failures and 5xx.
//
// Only idempotent GETs reach here, so retrying is always safe. 4xx are never
// retried: a 404 will still be a 404, and a 401 is handled by the caller.
func (c *Client) attempt(ctx context.Context, rawURL string, accept []string, token string) (*http.Response, error) {
	delay := c.backoff

	var lastErr error
	for attempt := 1; attempt <= c.maxAttempts; attempt++ {
		if attempt > 1 {
			if err := sleep(ctx, delay); err != nil {
				return nil, err
			}
			delay *= 2
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			return nil, fmt.Errorf("building a request for %s: %w", rawURL, err)
		}
		req.Header.Set("User-Agent", c.userAgent)
		for _, a := range accept {
			req.Header.Add("Accept", a)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}

		resp, err := c.http.Do(req)
		if err != nil {
			// A cancelled context is the caller's decision, not a transport
			// failure, and must not be retried.
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			lastErr = fmt.Errorf("%w: %s: %w", ErrRegistryUnavailable, rawURL, err)
			c.logger.Debug("registry request failed", "url", rawURL, "attempt", attempt, "error", err)
			continue
		}

		if resp.StatusCode/100 == 5 || resp.StatusCode == http.StatusTooManyRequests {
			retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"))
			lastErr = fmt.Errorf("%w: %s returned %s", ErrRegistryUnavailable, rawURL, resp.Status)
			drain(resp, c.logger)
			if retryAfter > delay {
				delay = retryAfter
			}
			c.logger.Debug("registry returned a retryable status",
				"url", rawURL, "status", resp.StatusCode, "attempt", attempt)
			continue
		}

		return resp, nil
	}

	return nil, lastErr
}

// statusError turns a non-2xx response into the sentinel that describes it,
// consuming the body so the connection can be reused.
func (c *Client) statusError(resp *http.Response, rawURL string) error {
	detail := registryErrorDetail(resp)
	drain(resp, c.logger)

	switch resp.StatusCode {
	case http.StatusNotFound:
		return fmt.Errorf("%w: %s%s", ErrNotFound, rawURL, detail)
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("%w: %s%s (forge pulls anonymously; this image needs credentials)",
			ErrUnauthorized, rawURL, detail)
	default:
		return fmt.Errorf("%w: %s returned %s%s", ErrRegistryUnavailable, rawURL, resp.Status, detail)
	}
}

// registryErrorDetail extracts the human-readable half of a Distribution Spec
// error body, so the message says "manifest unknown" rather than only "404".
func registryErrorDetail(resp *http.Response) string {
	var body struct {
		Errors []struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
	}

	// A best-effort read: this is decoration on an error that is already
	// decided, so a malformed body simply means no detail.
	limited := io.LimitReader(resp.Body, 8<<10)
	if err := json.NewDecoder(limited).Decode(&body); err != nil || len(body.Errors) == 0 {
		return ""
	}

	first := body.Errors[0]
	if first.Message == "" {
		return " (" + first.Code + ")"
	}
	return " (" + first.Message + ")"
}

// authenticate performs the anonymous half of the Bearer token flow.
//
// The registry's 401 carries a challenge naming a token service, the service
// it is for, and the scope being asked about. Forge asks that service for a
// token with no credentials, which public registries grant for public
// repositories. There is no username or password anywhere in this package.
func (c *Client) authenticate(ctx context.Context, ref Reference, challenge string) (string, error) {
	scheme, params := parseChallenge(challenge)
	if !strings.EqualFold(scheme, "bearer") {
		return "", fmt.Errorf("%w: %s asked for %q authentication, which forge does not implement",
			ErrUnauthorized, ref.Host(), scheme)
	}

	realm := params["realm"]
	if realm == "" {
		return "", fmt.Errorf("%w: %s sent an authentication challenge with no realm",
			ErrUnauthorized, ref.Host())
	}

	endpoint, err := url.Parse(realm)
	if err != nil {
		return "", fmt.Errorf("%w: %s sent an unparseable token realm %q: %w",
			ErrUnauthorized, ref.Host(), realm, err)
	}

	query := endpoint.Query()
	if service := params["service"]; service != "" {
		query.Set("service", service)
	}
	scope := params["scope"]
	if scope == "" {
		scope = "repository:" + ref.Repository + ":pull"
	}
	query.Set("scope", scope)
	endpoint.RawQuery = query.Encode()

	resp, err := c.attempt(ctx, endpoint.String(), []string{"application/json"}, "")
	if err != nil {
		return "", err
	}
	defer drain(resp, c.logger)

	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("%w: the token service at %s returned %s", ErrUnauthorized, endpoint.Host, resp.Status)
	}

	var token struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, c.maxManifestBytes)).Decode(&token); err != nil {
		return "", fmt.Errorf("%w: reading the token from %s: %w", ErrUnauthorized, endpoint.Host, err)
	}

	// Registries differ on which field they use; the spec allows both.
	if token.Token != "" {
		return token.Token, nil
	}
	if token.AccessToken != "" {
		return token.AccessToken, nil
	}

	return "", fmt.Errorf("%w: the token service at %s returned no token", ErrUnauthorized, endpoint.Host)
}

// tokenScope names the cache entry a reference's token belongs to. A token is
// issued for one repository on one registry and is useless for another.
func tokenScope(ref Reference) string { return ref.Host() + "/" + ref.Repository }

func (c *Client) cachedToken(scope string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tokens[scope]
}

func (c *Client) cacheToken(scope, token string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tokens[scope] = token
}

// parseChallenge splits a WWW-Authenticate header into its scheme and its
// key="value" parameters.
func parseChallenge(header string) (scheme string, params map[string]string) {
	params = make(map[string]string)

	scheme, rest, found := strings.Cut(strings.TrimSpace(header), " ")
	if !found {
		return scheme, params
	}

	// Parameters are comma-separated, and their values are quoted — no value
	// Forge cares about (realm, service, scope) contains a comma in practice,
	// so a split is enough and a full RFC 7235 parser is not.
	for part := range strings.SplitSeq(rest, ",") {
		key, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		params[strings.ToLower(strings.TrimSpace(key))] = strings.Trim(strings.TrimSpace(value), `"`)
	}

	return scheme, params
}

// parseRetryAfter reads the delay-seconds form of a Retry-After header,
// clamped so a registry cannot park a run indefinitely.
func parseRetryAfter(header string) time.Duration {
	if header == "" {
		return 0
	}
	seconds, err := strconv.Atoi(strings.TrimSpace(header))
	if err != nil || seconds < 0 {
		// The HTTP-date form is legal and rare; ignoring it falls back to the
		// exponential backoff, which is a safe answer rather than a wrong one.
		return 0
	}
	return min(time.Duration(seconds)*time.Second, maxRetryAfter)
}

// sleep waits for d, or returns early if the context is done.
func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// drain consumes and closes a response body so the connection can be reused.
//
// The read is bounded: this runs on error paths, where the body is an error
// document, and an unbounded copy would let a hostile registry make the error
// path the expensive one.
func drain(resp *http.Response, logger *slog.Logger) {
	if _, err := io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10)); err != nil {
		logger.Debug("draining a registry response body", "error", err)
	}
	if err := resp.Body.Close(); err != nil {
		// SSOT §13.7: never discarded, even here.
		logger.Warn("closing a registry response body", "error", err)
	}
}
