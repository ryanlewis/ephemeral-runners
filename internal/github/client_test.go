package github

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestNewRejectsBadOptions(t *testing.T) {
	ok := Options{BaseURL: "https://api.example.test", AppID: 1, InstallationID: 2, Key: testKey}
	tests := []struct {
		name string
		edit func(*Options)
		want string
	}{
		{"empty URL", func(o *Options) { o.BaseURL = "" }, "not an http(s) URL"},
		{"no scheme", func(o *Options) { o.BaseURL = "api.example.test" }, "not an http(s) URL"},
		{"ftp scheme", func(o *Options) { o.BaseURL = "ftp://api.example.test" }, "not an http(s) URL"},
		{"no host", func(o *Options) { o.BaseURL = "https://" }, "not an http(s) URL"},
		{"unparsable", func(o *Options) { o.BaseURL = "http://[::1" }, "not an http(s) URL"},
		{"plain http", func(o *Options) { o.BaseURL = "http://github.example.com" }, "requires https"},
		{"plain http, private IP", func(o *Options) { o.BaseURL = "http://10.0.0.1:8080/api/v3" }, "requires https"},
		{"plain http, localhost lookalike", func(o *Options) { o.BaseURL = "http://localhost.example.com" }, "requires https"},
		{"zero app ID", func(o *Options) { o.AppID = 0 }, "app ID and installation ID"},
		{"negative app ID", func(o *Options) { o.AppID = -1 }, "app ID and installation ID"},
		{"zero installation ID", func(o *Options) { o.InstallationID = 0 }, "app ID and installation ID"},
		{"nil key", func(o *Options) { o.Key = nil }, "private key is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := ok
			tt.edit(&o)
			c, err := New(o)
			if err == nil || c != nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("New = %v, %v; want error containing %q", c, err, tt.want)
			}
		})
	}
}

func TestNewAcceptsLoopbackHTTP(t *testing.T) {
	for _, base := range []string{
		"http://127.0.0.1:8080", "http://127.1.2.3", "http://localhost:9", "http://LOCALHOST", "http://[::1]:8080",
		"https://api.example.test",
	} {
		if _, err := New(Options{BaseURL: base, AppID: 1, InstallationID: 2, Key: testKey}); err != nil {
			t.Errorf("New(%q) = %v, want nil", base, err)
		}
	}
}

func TestDefaultClientRefusesInsecureRedirect(t *testing.T) {
	c, err := New(Options{BaseURL: "https://api.example.test", AppID: 1, InstallationID: 2, Key: testKey})
	if err != nil {
		t.Fatal(err)
	}
	check := c.http.CheckRedirect
	req := func(u string) *http.Request {
		r, err := http.NewRequest(http.MethodGet, u, nil)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	if err := check(req("http://api.example.test/x"), nil); err == nil || !strings.Contains(err.Error(), "requires https") {
		t.Errorf("redirect to plain http = %v, want requires https error", err)
	}
	for _, ok := range []string{"https://api.example.test/x", "http://127.0.0.1:8080/x"} {
		if err := check(req(ok), nil); err != nil {
			t.Errorf("redirect to %s = %v, want nil", ok, err)
		}
	}
	if err := check(req("https://api.example.test/x"), make([]*http.Request, 10)); err == nil {
		t.Error("11th redirect allowed, want error")
	}
}

func TestNewDefaults(t *testing.T) {
	c, err := New(Options{BaseURL: "http://127.0.0.1:1///", AppID: 1, InstallationID: 2, Key: testKey})
	if err != nil {
		t.Fatal(err)
	}
	if c.baseURL != "http://127.0.0.1:1" {
		t.Errorf("baseURL = %q, want trailing slashes trimmed", c.baseURL)
	}
	if c.http == nil || c.http.Timeout != 30*time.Second {
		t.Errorf("default http client = %+v, want a 30s timeout", c.http)
	}
	if c.now == nil || time.Since(c.now()) > time.Minute {
		t.Error("default clock is not time.Now")
	}
	if !c.TokenExpiry().IsZero() {
		t.Errorf("TokenExpiry before any token = %v", c.TokenExpiry())
	}

	hc := &http.Client{Timeout: time.Second}
	at := time.Unix(1_800_000_000, 0)
	c, err = New(Options{BaseURL: "https://api.example.test", AppID: 1, InstallationID: 2, Key: testKey, HTTPClient: hc, Now: func() time.Time { return at }})
	if err != nil {
		t.Fatal(err)
	}
	if c.http != hc || !c.now().Equal(at) {
		t.Error("New replaced the supplied HTTP client or clock")
	}
}

func TestAPIErrorText(t *testing.T) {
	e := &APIError{Method: "GET", URL: "https://x.test/a", StatusCode: 404, Message: "Not Found!"}
	if got, want := e.Error(), "GET https://x.test/a: 404 Not Found!"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	e.Message = ""
	if got, want := e.Error(), "GET https://x.test/a: 404 Not Found"; got != want {
		t.Errorf("Error() with no message = %q, want %q", got, want)
	}
	if errors.Is(e, errors.New("other")) {
		t.Error("APIError matches an unrelated error")
	}
}

func TestErrorBodies(t *testing.T) {
	long := strings.Repeat("x", 300)
	tests := []struct {
		name, body, message string
	}{
		{"long message is cut at 200 bytes", `{"message":"` + long + `"}`, long[:200]},
		{"non-JSON body leaves no message", "<html>bad gateway</html>", ""},
		{"empty body leaves no message", "", ""},
		{"JSON without a message", `{"error":"x"}`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, srv := newFake(t, time.Now())
			f.HandleFunc("DELETE /repos/example-org/example-repo/actions/runners/9", func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusBadGateway)
				writeBody(t, w, tt.body)
			})
			err := newClient(t, srv, nil).DeleteRunner(context.Background(), "example-org/example-repo", 9)
			var apiErr *APIError
			if !errors.As(err, &apiErr) || apiErr.Message != tt.message || !errors.Is(err, ErrServer) {
				t.Fatalf("err = %#v", err)
			}
			if tt.message == "" && !strings.HasSuffix(err.Error(), "502 Bad Gateway") {
				t.Errorf("Error() = %q, want the status text", err)
			}
		})
	}
}

func TestSendHeaders(t *testing.T) {
	var got, post http.Header
	var postBody string
	f, srv := newFake(t, time.Now())
	f.HandleFunc("GET /h", func(w http.ResponseWriter, r *http.Request) { got = r.Header.Clone() })
	f.HandleFunc("POST /h", func(w http.ResponseWriter, r *http.Request) {
		post = r.Header.Clone()
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		postBody = string(b)
	})
	c := newClient(t, srv, nil)
	ctx := context.Background()
	if _, err := c.send(ctx, http.MethodGet, srv.URL+"/h", "token abc", nil, nil); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{
		"Accept":               "application/vnd.github+json",
		"X-Github-Api-Version": apiVersion,
		"User-Agent":           userAgent,
		"Authorization":        "token abc",
	} {
		if got.Get(k) != v {
			t.Errorf("GET header %s = %q, want %q", k, got.Get(k), v)
		}
	}
	if got.Get("Content-Type") != "" {
		t.Errorf("a GET with no body sent Content-Type %q", got.Get("Content-Type"))
	}
	if _, err := c.send(ctx, http.MethodPost, srv.URL+"/h", "token abc", map[string]int{"a": 1}, nil); err != nil {
		t.Fatal(err)
	}
	if post.Get("Content-Type") != "application/json" || postBody != `{"a":1}` {
		t.Errorf("POST Content-Type %q body %q", post.Get("Content-Type"), postBody)
	}
}

// Only a GET is cached and revalidated; a POST to a URL that answered with an
// ETag neither sends If-None-Match nor stores one.
func TestSendOnlyCachesGET(t *testing.T) {
	f, srv := newFake(t, time.Now())
	var ifNoneMatch []string
	f.HandleFunc("/e", func(w http.ResponseWriter, r *http.Request) {
		ifNoneMatch = append(ifNoneMatch, r.Method+":"+r.Header.Get("If-None-Match"))
		w.Header().Set("ETag", `"e1"`)
		writeBody(t, w, `{}`)
	})
	c := newClient(t, srv, nil)
	ctx := context.Background()
	for _, m := range []string{"POST", "POST", "GET", "POST", "GET"} {
		if _, err := c.send(ctx, m, srv.URL+"/e", "token abc", nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	want := "POST: POST: GET: POST: GET:\"e1\""
	if got := strings.Join(ifNoneMatch, " "); got != want {
		t.Errorf("requests = %q, want %q", got, want)
	}
}

// A 304 for a request that sent no If-None-Match has no cached body to serve,
// so it is reported as an error like any other non-2xx.
func TestSendNotModifiedWithoutCache(t *testing.T) {
	f, srv := newFake(t, time.Now())
	f.HandleFunc("GET /n", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotModified) })
	c := newClient(t, srv, nil)
	var out map[string]any
	_, err := c.send(context.Background(), http.MethodGet, srv.URL+"/n", "token abc", nil, &out)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusNotModified {
		t.Fatalf("err = %#v", err)
	}
	if s := c.Stats(); s.NotModified != 0 || s.Requests != 1 {
		t.Errorf("stats = %+v", s)
	}
}

func TestSendResponseEdges(t *testing.T) {
	f, srv := newFake(t, time.Now())
	f.HandleFunc("GET /empty", func(w http.ResponseWriter, r *http.Request) {})
	f.HandleFunc("GET /junk", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Link", "marker")
		writeBody(t, w, "not json")
	})
	f.HandleFunc("GET /torn", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		writeBody(t, w, "abc")
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler)
	})
	c := newClient(t, srv, nil)
	ctx := context.Background()

	// An empty 200 leaves out untouched and is not a decode error.
	out := map[string]int{"keep": 1}
	if _, err := c.send(ctx, http.MethodGet, srv.URL+"/empty", "token abc", nil, &out); err != nil || out["keep"] != 1 {
		t.Errorf("empty body: out = %v, err = %v", out, err)
	}

	// A body that is not JSON fails to decode, and the headers still come back.
	h, err := c.send(ctx, http.MethodGet, srv.URL+"/junk", "token abc", nil, &out)
	if err == nil || !strings.Contains(err.Error(), "decode") || h.Get("Link") != "marker" {
		t.Errorf("junk body: header %v, err = %v", h, err)
	}

	// A body cut short is a read error, not a decode error, and is not counted.
	before := c.Stats().Requests
	_, err = c.send(ctx, http.MethodGet, srv.URL+"/torn", "token abc", nil, &out)
	if err == nil || !strings.Contains(err.Error(), "read body") {
		t.Errorf("torn body: err = %v", err)
	}
	if after := c.Stats().Requests; after != before {
		t.Errorf("a failed read counted %d requests", after-before)
	}
}

func TestSendRequestErrors(t *testing.T) {
	_, live := newFake(t, time.Now())
	c := newClient(t, live, nil)
	ctx := context.Background()

	if _, err := c.send(ctx, http.MethodPost, "http://127.0.0.1:1/x", "token abc", make(chan int), nil); err == nil || !strings.Contains(err.Error(), "unsupported type") {
		t.Errorf("unmarshalable body: err = %v", err)
	}
	if _, err := c.send(ctx, "BAD METHOD", "http://127.0.0.1:1/x", "token abc", nil, nil); err == nil || !strings.Contains(err.Error(), "invalid method") {
		t.Errorf("bad method: err = %v", err)
	}

	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	if _, err := c.send(ctx, http.MethodGet, url+"/x", "token abc", nil, nil); err == nil {
		t.Error("send to a closed server succeeded")
	}
	if s := c.Stats(); s.Requests != 0 {
		t.Errorf("failed sends counted %d requests", s.Requests)
	}

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := c.send(cancelled, http.MethodGet, url+"/x", "token abc", nil, nil); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled context: err = %v", err)
	}
}

func TestDoTokenFailures(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
		want    error
		text    string
	}{
		{"server error", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }, ErrServer, "get installation token"},
		{"bad credentials", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(401) }, ErrUnauthorized, "get installation token"},
		{"empty token", func(w http.ResponseWriter, r *http.Request) { writeBody(t, w, `{"token":""}`) }, nil, "empty token in response"},
		{"token response is not JSON", func(w http.ResponseWriter, r *http.Request) { writeBody(t, w, `nope`) }, nil, "decode"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mux := http.NewServeMux()
			var apiCalls atomic.Int64
			mux.HandleFunc("POST /app/installations/7/access_tokens", tt.handler)
			mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { apiCalls.Add(1) })
			srv := httptest.NewServer(mux)
			t.Cleanup(srv.Close)
			c := newClient(t, srv, nil)
			err := c.DeleteRunner(context.Background(), "example-org/example-repo", 9)
			if err == nil || !strings.Contains(err.Error(), tt.text) || (tt.want != nil && !errors.Is(err, tt.want)) {
				t.Fatalf("err = %v", err)
			}
			if apiCalls.Load() != 0 || !c.TokenExpiry().IsZero() {
				t.Errorf("API calls %d, token expiry %v after a failed token fetch", apiCalls.Load(), c.TokenExpiry())
			}
		})
	}
}

func TestTokenExpiry(t *testing.T) {
	start := time.Unix(1_800_000_000, 0)
	f, srv := newFake(t, start)
	f.HandleFunc("DELETE /repos/example-org/example-repo/actions/runners/9", func(w http.ResponseWriter, r *http.Request) {})
	c := newClient(t, srv, func() time.Time { return start })
	if err := c.DeleteRunner(context.Background(), "example-org/example-repo", 9); err != nil {
		t.Fatal(err)
	}
	if got, want := c.TokenExpiry(), start.Add(time.Hour); !got.Equal(want) {
		t.Errorf("TokenExpiry = %v, want %v", got, want)
	}
}

// 2xx is success and nothing else is: a 300 is an error, and a 299 would be
// success if a server sent one.
func TestSendSuccessRange(t *testing.T) {
	f, srv := newFake(t, time.Now())
	f.HandleFunc("GET /s/{code}", func(w http.ResponseWriter, r *http.Request) {
		code, err := strconv.Atoi(r.PathValue("code"))
		if err != nil {
			t.Error(err)
		}
		w.WriteHeader(code)
	})
	c := newClient(t, srv, nil)
	for code, wantErr := range map[int]bool{200: false, 204: false, 299: false, 300: true, 399: true} {
		_, err := c.send(context.Background(), http.MethodGet, srv.URL+"/s/"+strconv.Itoa(code), "token abc", nil, nil)
		var apiErr *APIError
		if gotErr := errors.As(err, &apiErr); gotErr != wantErr || (gotErr && apiErr.StatusCode != code) {
			t.Errorf("status %d: err = %v", code, err)
		}
	}
}
