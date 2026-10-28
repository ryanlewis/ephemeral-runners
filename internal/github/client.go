// Package github is a small client for the parts of the GitHub REST API that
// ephemeral-runners uses, authenticated as a GitHub App installation.
package github

import (
	"bytes"
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	apiVersion = "2022-11-28"
	userAgent  = "ephemeral-runners"
	// maxBody caps how much of a response body is read.
	maxBody = 16 << 20
	// maxCached caps the number of URLs kept in the ETag cache.
	maxCached = 512
)

// Options configures a Client.
type Options struct {
	// BaseURL is the API root, such as https://api.github.com.
	BaseURL        string
	AppID          int64
	InstallationID int64
	Key            *rsa.PrivateKey
	// HTTPClient defaults to a client with a 30 second timeout.
	HTTPClient *http.Client
	// Now defaults to time.Now. Tests set it to control token expiry.
	Now func() time.Time
}

// Client calls the GitHub API as one App installation. It is safe for
// concurrent use.
type Client struct {
	baseURL        string
	appID          int64
	installationID int64
	key            *rsa.PrivateKey
	http           *http.Client
	now            func() time.Time

	tokenMu     sync.Mutex
	token       string
	tokenExpiry time.Time

	cache etagCache

	rateMu sync.Mutex
	rate   RateLimit

	requests    atomic.Int64
	notModified atomic.Int64
}

// New returns a Client.
func New(o Options) (*Client, error) {
	u, err := url.Parse(o.BaseURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, fmt.Errorf("base URL %q is not an http(s) URL", o.BaseURL)
	}
	if o.AppID <= 0 || o.InstallationID <= 0 {
		return nil, errors.New("app ID and installation ID are required")
	}
	if o.Key == nil {
		return nil, errors.New("private key is required")
	}
	c := &Client{
		baseURL:        strings.TrimRight(o.BaseURL, "/"),
		appID:          o.AppID,
		installationID: o.InstallationID,
		key:            o.Key,
		http:           o.HTTPClient,
		now:            o.Now,
	}
	if c.http == nil {
		c.http = &http.Client{Timeout: 30 * time.Second}
	}
	if c.now == nil {
		c.now = time.Now
	}
	return c, nil
}

// RateLimit is the rate limit state GitHub reported most recently.
type RateLimit struct {
	Limit     int
	Remaining int
	Used      int
	Reset     time.Time
	Resource  string
}

// RateLimit returns the rate limit headers from the latest response that
// had them, leaving out 304s. It is the zero value before any such response.
func (c *Client) RateLimit() RateLimit {
	c.rateMu.Lock()
	defer c.rateMu.Unlock()
	return c.rate
}

func (c *Client) noteRateLimit(h http.Header) {
	rl, ok := parseRateLimit(h)
	if !ok {
		return
	}
	c.rateMu.Lock()
	c.rate = rl
	c.rateMu.Unlock()
}

func parseRateLimit(h http.Header) (RateLimit, bool) {
	rem, err := strconv.Atoi(h.Get("X-RateLimit-Remaining"))
	if err != nil {
		return RateLimit{}, false
	}
	limit, _ := strconv.Atoi(h.Get("X-RateLimit-Limit"))
	used, _ := strconv.Atoi(h.Get("X-RateLimit-Used"))
	reset, _ := strconv.ParseInt(h.Get("X-RateLimit-Reset"), 10, 64)
	return RateLimit{
		Limit:     limit,
		Remaining: rem,
		Used:      used,
		Reset:     time.Unix(reset, 0),
		Resource:  h.Get("X-RateLimit-Resource"),
	}, true
}

// Stats counts the API responses the client has received.
type Stats struct {
	// Requests is every response, including 304s.
	Requests int64
	// NotModified is the 304 responses, which GitHub does not count
	// against the rate limit.
	NotModified int64
}

// Stats returns the response counts so far.
func (c *Client) Stats() Stats {
	return Stats{Requests: c.requests.Load(), NotModified: c.notModified.Load()}
}

// Errors that an *APIError matches with errors.Is.
var (
	ErrUnauthorized  = errors.New("unauthorized")
	ErrForbidden     = errors.New("forbidden")
	ErrNotFound      = errors.New("not found")
	ErrUnprocessable = errors.New("unprocessable")
	ErrRateLimited   = errors.New("rate limited")
	ErrServer        = errors.New("server error")
)

// APIError is a response from GitHub with a status that is not a success.
type APIError struct {
	Method     string
	URL        string
	StatusCode int
	// Message is GitHub's error message, if the body had one.
	Message string
	// RateLimited is set for a 429, or a 403 with no requests remaining or
	// with a Retry-After header, which GitHub sends for a secondary limit.
	RateLimited bool
	// RetryAfter is the wait a Retry-After header asks for, or zero.
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	msg := e.Message
	if msg == "" {
		msg = http.StatusText(e.StatusCode)
	}
	return fmt.Sprintf("%s %s: %d %s", e.Method, e.URL, e.StatusCode, msg)
}

// Is matches the Err* values by status code.
func (e *APIError) Is(target error) bool {
	switch target {
	case ErrUnauthorized:
		return e.StatusCode == http.StatusUnauthorized
	case ErrForbidden:
		return e.StatusCode == http.StatusForbidden
	case ErrNotFound:
		return e.StatusCode == http.StatusNotFound
	case ErrUnprocessable:
		return e.StatusCode == http.StatusUnprocessableEntity
	case ErrRateLimited:
		return e.RateLimited
	case ErrServer:
		return e.StatusCode >= 500
	}
	return false
}

func newAPIError(req *http.Request, resp *http.Response, body []byte) *APIError {
	e := &APIError{Method: req.Method, URL: req.URL.Redacted(), StatusCode: resp.StatusCode}
	var m struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &m) == nil {
		e.Message = m.Message
	}
	if len(e.Message) > 200 {
		e.Message = e.Message[:200]
	}
	if secs, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && secs > 0 {
		e.RetryAfter = time.Duration(secs) * time.Second
	}
	e.RateLimited = resp.StatusCode == http.StatusTooManyRequests ||
		(resp.StatusCode == http.StatusForbidden &&
			(resp.Header.Get("X-RateLimit-Remaining") == "0" || resp.Header.Get("Retry-After") != ""))
	return e
}

// do sends an API request with the installation token. path is either a
// path below the base URL or a full URL that starts with it.
func (c *Client) do(ctx context.Context, method, path string, body, out any) (http.Header, error) {
	u := path
	if strings.HasPrefix(path, "/") {
		u = c.baseURL + path
	} else if !strings.HasPrefix(path, c.baseURL+"/") {
		return nil, fmt.Errorf("refusing to send credentials to %q, outside %s", path, c.baseURL)
	}
	tok, err := c.installationToken(ctx)
	if err != nil {
		return nil, err
	}
	h, err := c.send(ctx, method, u, "token "+tok, body, out)
	if errors.Is(err, ErrUnauthorized) {
		c.dropToken(tok)
	}
	return h, err
}

// send makes one HTTP request and decodes a JSON response into out. A GET
// sends the ETag of its last 200 response for the same URL, and a 304
// answer decodes the body cached with it. GitHub leaves the Link header off
// a 304, so the returned headers carry the cached one instead.
func (c *Client) send(ctx context.Context, method, u, auth string, body, out any) (http.Header, error) {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, r)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Authorization", auth)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	var cached *etagEntry
	if method == http.MethodGet {
		if cached = c.cache.get(u); cached != nil {
			req.Header.Set("If-None-Match", cached.etag)
		}
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, fmt.Errorf("%s %s: read body: %w", method, req.URL.Redacted(), err)
	}
	c.requests.Add(1)
	// A 304 is not counted, and its rate limit headers do not match the
	// core limit that counted responses report, so only those update it.
	// Requests signed with the App JWT draw on the App's own limit, not
	// the installation's, so they are left out too.
	if resp.StatusCode != http.StatusNotModified && !strings.HasPrefix(auth, "Bearer ") {
		c.noteRateLimit(resp.Header)
	}

	switch {
	case resp.StatusCode == http.StatusNotModified && cached != nil:
		c.notModified.Add(1)
		data = cached.body
		if cached.link != "" {
			resp.Header.Set("Link", cached.link)
		}
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		if etag := resp.Header.Get("ETag"); method == http.MethodGet && etag != "" {
			c.cache.put(u, etag, resp.Header.Get("Link"), data)
		}
	default:
		return resp.Header, newAPIError(req, resp, data)
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return resp.Header, fmt.Errorf("%s %s: decode: %w", method, req.URL.Redacted(), err)
		}
	}
	return resp.Header, nil
}

// getAll follows the Link rel="next" headers from path and calls page with
// each page's decoded body.
func getAll[T any](ctx context.Context, c *Client, path string, page func(T)) error {
	for path != "" {
		var v T
		h, err := c.do(ctx, http.MethodGet, path, nil, &v)
		if err != nil {
			return err
		}
		page(v)
		path = nextLink(h.Get("Link"))
	}
	return nil
}

// nextLink returns the rel="next" URL from a Link header, or "".
func nextLink(link string) string {
	for part := range strings.SplitSeq(link, ",") {
		target, params, ok := strings.Cut(part, ";")
		if !ok {
			continue
		}
		for p := range strings.SplitSeq(params, ";") {
			if strings.TrimSpace(p) == `rel="next"` {
				return strings.Trim(strings.TrimSpace(target), "<>")
			}
		}
	}
	return ""
}

type etagEntry struct {
	etag string
	link string
	body []byte
	used int64
}

// etagCache holds the latest ETag, Link header and body for each GET URL, dropping the
// least recently used URL once it holds maxCached.
type etagCache struct {
	mu      sync.Mutex
	entries map[string]*etagEntry
	clock   int64
}

func (c *etagCache) get(u string) *etagEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.entries[u]
	if e != nil {
		c.clock++
		e.used = c.clock
	}
	return e
}

func (c *etagCache) put(u, etag, link string, body []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = map[string]*etagEntry{}
	}
	if _, ok := c.entries[u]; !ok && len(c.entries) >= maxCached {
		var oldest string
		for k, e := range c.entries {
			if oldest == "" || e.used < c.entries[oldest].used {
				oldest = k
			}
		}
		delete(c.entries, oldest)
	}
	c.clock++
	c.entries[u] = &etagEntry{etag: etag, link: link, body: body, used: c.clock}
}

func (c *etagCache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}
