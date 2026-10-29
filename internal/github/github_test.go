package github

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var testKey = func() *rsa.PrivateKey {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return k
}()

const (
	testAppID          = 42
	testInstallationID = 7
)

// fakeGitHub serves the token endpoint and whatever routes a test adds.
type fakeGitHub struct {
	*http.ServeMux
	tokens  atomic.Int64
	expires atomic.Int64 // Unix seconds
}

func newFake(t *testing.T, now time.Time) (*fakeGitHub, *httptest.Server) {
	f := &fakeGitHub{ServeMux: http.NewServeMux()}
	f.expires.Store(now.Add(time.Hour).Unix())
	f.HandleFunc("POST /app/installations/7/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			http.Error(w, `{"message":"no JWT"}`, http.StatusUnauthorized)
			return
		}
		n := f.tokens.Add(1)
		encodeBody(t, w, map[string]any{"token": fmt.Sprintf("tok-%d", n), "expires_at": time.Unix(f.expires.Load(), 0)})
	})
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv
}

func newClient(t *testing.T, srv *httptest.Server, now func() time.Time) *Client {
	c, err := New(Options{BaseURL: srv.URL, AppID: testAppID, InstallationID: testInstallationID, Key: testKey, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestParsePrivateKey(t *testing.T) {
	pkcs8, err := x509.MarshalPKCS8PrivateKey(testKey)
	if err != nil {
		t.Fatal(err)
	}
	for name, b := range map[string][]byte{
		"pkcs1": pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(testKey)}),
		"pkcs8": pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8}),
	} {
		k, err := ParsePrivateKey(b)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !k.Equal(testKey) {
			t.Errorf("%s: key differs", name)
		}
	}
	if _, err := ParsePrivateKey([]byte("not a key")); err == nil {
		t.Error("garbage parsed")
	}
}

func TestAppJWT(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	tok, err := appJWT(testKey, testAppID, now)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("JWT has %d parts", len(parts))
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&testKey.PublicKey, crypto.SHA256, sum[:], sig); err != nil {
		t.Fatalf("signature does not verify: %v", err)
	}

	var header struct{ Alg, Typ string }
	decodePart(t, parts[0], &header)
	if header.Alg != "RS256" || header.Typ != "JWT" {
		t.Errorf("header = %+v", header)
	}
	var claims struct {
		IAT int64  `json:"iat"`
		EXP int64  `json:"exp"`
		ISS string `json:"iss"`
	}
	decodePart(t, parts[1], &claims)
	if claims.ISS != "42" {
		t.Errorf("iss = %q", claims.ISS)
	}
	if claims.IAT != now.Unix()-60 {
		t.Errorf("iat = %d, want now-60", claims.IAT)
	}
	if claims.EXP <= now.Unix() || claims.EXP > now.Unix()+600 {
		t.Errorf("exp = now%+d, want within 10 minutes", claims.EXP-now.Unix())
	}
}

func decodePart(t *testing.T, part string, v any) {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(part)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatal(err)
	}
}

// writeBody and encodeBody are for handlers that run off the test goroutine,
// so a failed write is reported with t.Error rather than t.Fatal.
func writeBody(t *testing.T, w http.ResponseWriter, s string) {
	t.Helper()
	if _, err := io.WriteString(w, s); err != nil {
		t.Error(err)
	}
}

func encodeBody(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Error(err)
	}
}

func TestTokenRefresh(t *testing.T) {
	start := time.Unix(1_800_000_000, 0)
	var mu sync.Mutex
	now := start
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	f, srv := newFake(t, start)
	var seen []string
	f.HandleFunc("GET /rate_limit", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("Authorization"))
		mu.Unlock()
		writeBody(t, w, `{"resources":{"core":{"limit":5000,"remaining":4999,"used":1,"reset":1800003600}}}`)
	})
	c := newClient(t, srv, clock)
	ctx := context.Background()

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if _, err := c.GetRateLimit(ctx); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if n := f.tokens.Load(); n != 1 {
		t.Fatalf("concurrent calls fetched %d tokens, want 1", n)
	}

	mu.Lock()
	now = start.Add(54 * time.Minute)
	mu.Unlock()
	if _, err := c.GetRateLimit(ctx); err != nil {
		t.Fatal(err)
	}
	if n := f.tokens.Load(); n != 1 {
		t.Fatalf("token refreshed 6 minutes before expiry; %d fetches", n)
	}

	mu.Lock()
	now = start.Add(56 * time.Minute)
	f.expires.Store(now.Add(time.Hour).Unix())
	mu.Unlock()
	rl, err := c.GetRateLimit(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n := f.tokens.Load(); n != 2 {
		t.Fatalf("token not refreshed 4 minutes before expiry; %d fetches", n)
	}
	if got := seen[len(seen)-1]; got != "token tok-2" {
		t.Errorf("last request used %q", got)
	}
	if rl.Remaining != 4999 || rl.Limit != 5000 {
		t.Errorf("rate limit = %+v", rl)
	}
}

func TestUnauthorizedDropsToken(t *testing.T) {
	f, srv := newFake(t, time.Now())
	f.HandleFunc("GET /repos/example-org/example-repo/actions/runners/1", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "token tok-1" {
			http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
			return
		}
		writeBody(t, w, `{"id":1,"name":"r"}`)
	})
	c := newClient(t, srv, nil)
	ctx := context.Background()
	if _, err := c.GetRunner(ctx, "example-org/example-repo", 1); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized", err)
	}
	if _, err := c.GetRunner(ctx, "example-org/example-repo", 1); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if n := f.tokens.Load(); n != 2 {
		t.Errorf("%d token fetches, want 2", n)
	}
}

func TestNotModified(t *testing.T) {
	f, srv := newFake(t, time.Now())
	var hits atomic.Int64
	f.HandleFunc("GET /repos/example-org/example-repo/actions/runners/5", func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Remaining", "4990")
		w.Header().Set("X-RateLimit-Used", "10")
		w.Header().Set("X-RateLimit-Reset", "1800003600")
		w.Header().Set("X-RateLimit-Resource", "core")
		if r.Header.Get("If-None-Match") == `"v1"` {
			// Headers on a 304 must not replace the counted ones.
			w.Header().Set("X-RateLimit-Remaining", "4999")
			w.Header().Set("X-RateLimit-Reset", "1800009999")
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		writeBody(t, w, `{"id":5,"name":"example-a","status":"online","busy":true,"labels":[{"name":"self-hosted"},{"name":"small"}]}`)
	})
	c := newClient(t, srv, nil)
	ctx := context.Background()
	first, err := c.GetRunner(ctx, "example-org/example-repo", 5)
	if err != nil {
		t.Fatal(err)
	}
	before := c.Stats()
	second, err := c.GetRunner(ctx, "example-org/example-repo", 5)
	if err != nil {
		t.Fatal(err)
	}
	after := c.Stats()
	if second.Name != "example-a" || !second.Busy || len(second.Labels) != 2 || second.Labels[1] != "small" {
		t.Errorf("cached runner = %+v", second)
	}
	if fmt.Sprint(first) != fmt.Sprint(second) {
		t.Errorf("first %+v, second %+v", first, second)
	}
	if hits.Load() != 2 {
		t.Errorf("server hit %d times", hits.Load())
	}
	if after.Requests-before.Requests != 1 || after.NotModified-before.NotModified != 1 {
		t.Errorf("stats before %+v after %+v, want one more request, counted as not modified", before, after)
	}
	rl := c.RateLimit()
	if rl.Remaining != 4990 || rl.Limit != 5000 || rl.Used != 10 || rl.Reset.Unix() != 1800003600 || rl.Resource != "core" {
		t.Errorf("rate limit = %+v", rl)
	}
}

func TestListRunnersPaginates(t *testing.T) {
	f, srv := newFake(t, time.Now())
	f.HandleFunc("GET /repos/example-org/example-repo/actions/runners", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("page") {
		case "":
			w.Header().Set("Link", fmt.Sprintf(`<%s/repos/example-org/example-repo/actions/runners?per_page=100&page=2>; rel="next", <%[1]s/x?page=2>; rel="last"`, "http://"+r.Host))
			writeBody(t, w, `{"total_count":3,"runners":[{"id":1,"name":"a"},{"id":2,"name":"b"}]}`)
		case "2":
			w.Header().Set("Link", fmt.Sprintf(`<%s/repos/example-org/example-repo/actions/runners?per_page=100&page=1>; rel="prev"`, "http://"+r.Host))
			writeBody(t, w, `{"total_count":3,"runners":[{"id":3,"name":"c","status":"offline"}]}`)
		}
	})
	c := newClient(t, srv, nil)
	rs, err := c.ListRunners(context.Background(), "example-org/example-repo")
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 3 || rs[2].Name != "c" || rs[2].Status != "offline" {
		t.Errorf("runners = %+v", rs)
	}
}

// GitHub leaves the Link header off a 304, so the next page has to come
// from the cached response.
func TestPaginatesAfterNotModified(t *testing.T) {
	f, srv := newFake(t, time.Now())
	f.HandleFunc("GET /repos/example-org/example-repo/actions/runners", func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		etag := `"p` + page + `"`
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", etag)
		switch page {
		case "":
			w.Header().Set("Link", fmt.Sprintf(`<%s/repos/example-org/example-repo/actions/runners?per_page=100&page=2>; rel="next"`, "http://"+r.Host))
			writeBody(t, w, `{"runners":[{"id":1,"name":"a"},{"id":2,"name":"b"}]}`)
		case "2":
			writeBody(t, w, `{"runners":[{"id":3,"name":"c"}]}`)
		}
	})
	c := newClient(t, srv, nil)
	for i := 1; i <= 2; i++ {
		rs, err := c.ListRunners(context.Background(), "example-org/example-repo")
		if err != nil {
			t.Fatal(err)
		}
		if len(rs) != 3 {
			t.Errorf("call %d: %d runners, want 3", i, len(rs))
		}
	}
	if n := c.Stats().NotModified; n != 2 {
		t.Errorf("%d not modified, want 2", n)
	}
}

func TestNextLinkToOtherHostRefused(t *testing.T) {
	f, srv := newFake(t, time.Now())
	f.HandleFunc("GET /repos/example-org/example-repo/actions/runners", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Link", `<https://elsewhere.example/steal?page=2>; rel="next"`)
		writeBody(t, w, `{"runners":[]}`)
	})
	c := newClient(t, srv, nil)
	if _, err := c.ListRunners(context.Background(), "example-org/example-repo"); err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Errorf("err = %v", err)
	}
}

// secondaryMessage is the message GitHub sends with a secondary rate limit.
const secondaryMessage = "You have exceeded a secondary rate limit. Please wait a few minutes before you try again."

func TestErrorMapping(t *testing.T) {
	tests := []struct {
		status  int
		headers map[string]string
		message string
		want    error
	}{
		{401, nil, "", ErrUnauthorized},
		{403, nil, "", ErrForbidden},
		{404, nil, "", ErrNotFound},
		{422, nil, "", ErrUnprocessable},
		{500, nil, "", ErrServer},
		{502, nil, "", ErrServer},
		{429, nil, "", ErrRateLimited},
		{403, map[string]string{"X-RateLimit-Remaining": "0"}, "", ErrRateLimited},
		// A secondary rate limit leaves requests remaining and sets Retry-After.
		{403, map[string]string{"X-RateLimit-Remaining": "4000", "Retry-After": "60"}, "", ErrRateLimited},
		// GitHub can send a secondary rate limit with neither Retry-After nor
		// an empty primary limit; only the message says what it is.
		{403, map[string]string{"X-RateLimit-Remaining": "4000"}, secondaryMessage, ErrRateLimited},
		{429, map[string]string{"X-RateLimit-Remaining": "4000"}, secondaryMessage, ErrRateLimited},
		// A permission error with requests remaining stays plain forbidden.
		{403, map[string]string{"X-RateLimit-Remaining": "4000"}, "Resource not accessible by integration", ErrForbidden},
	}
	all := []error{ErrUnauthorized, ErrForbidden, ErrNotFound, ErrUnprocessable, ErrRateLimited, ErrServer}
	for _, tt := range tests {
		if tt.message == "" {
			tt.message = "nope"
		}
		t.Run(fmt.Sprint(tt.status, tt.headers, tt.message), func(t *testing.T) {
			f, srv := newFake(t, time.Now())
			f.HandleFunc("DELETE /repos/example-org/example-repo/actions/runners/9", func(w http.ResponseWriter, r *http.Request) {
				for k, v := range tt.headers {
					w.Header().Set(k, v)
				}
				w.WriteHeader(tt.status)
				writeBody(t, w, fmt.Sprintf(`{"message":%q}`, tt.message))
			})
			c := newClient(t, srv, nil)
			err := c.DeleteRunner(context.Background(), "example-org/example-repo", 9)
			var apiErr *APIError
			if !errors.As(err, &apiErr) || apiErr.StatusCode != tt.status || apiErr.Message != tt.message {
				t.Fatalf("err = %#v", err)
			}
			if want := tt.headers["Retry-After"] == "60"; (apiErr.RetryAfter == time.Minute) != want {
				t.Errorf("RetryAfter = %s", apiErr.RetryAfter)
			}
			for _, e := range all {
				want := e == tt.want || (e == ErrForbidden && tt.status == 403)
				if errors.Is(err, e) != want {
					t.Errorf("errors.Is(%v, %v) = %v", err, e, !want)
				}
			}
		})
	}
}

func TestQueuedJobs(t *testing.T) {
	f, srv := newFake(t, time.Now())
	f.HandleFunc("GET /repos/example-org/example-repo/actions/runs", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("status") {
		case "queued":
			writeBody(t, w, `{"workflow_runs":[{"id":100}]}`)
		case "in_progress":
			writeBody(t, w, `{"workflow_runs":[{"id":100},{"id":200}]}`)
		default:
			http.Error(w, "bad status", 400)
		}
	})
	var runCalls atomic.Int64
	f.HandleFunc("GET /repos/example-org/example-repo/actions/runs/100/jobs", func(w http.ResponseWriter, r *http.Request) {
		runCalls.Add(1)
		writeBody(t, w, `{"jobs":[
			{"id":1,"run_id":100,"name":"late","status":"queued","labels":["self-hosted","large"],"created_at":"2026-01-01T00:02:00Z"},
			{"id":2,"run_id":100,"name":"done","status":"completed","labels":["self-hosted"],"created_at":"2026-01-01T00:00:00Z"}]}`)
	})
	f.HandleFunc("GET /repos/example-org/example-repo/actions/runs/200/jobs", func(w http.ResponseWriter, r *http.Request) {
		writeBody(t, w, `{"jobs":[
			{"id":3,"run_id":200,"name":"early","status":"queued","labels":["self-hosted","small"],"created_at":"2026-01-01T00:01:00Z"},
			{"id":4,"run_id":200,"name":"running","status":"in_progress","labels":["self-hosted"],"created_at":"2026-01-01T00:00:00Z"}]}`)
	})
	c := newClient(t, srv, nil)
	jobs, err := c.QueuedJobs(context.Background(), "example-org/example-repo")
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 2 || jobs[0].ID != 3 || jobs[1].ID != 1 {
		t.Fatalf("jobs = %+v", jobs)
	}
	j := jobs[0]
	if j.RunID != 200 || j.Name != "early" || strings.Join(j.Labels, ",") != "self-hosted,small" || j.CreatedAt.Minute() != 1 {
		t.Errorf("job = %+v", j)
	}
	if runCalls.Load() != 1 {
		t.Errorf("run 100 listed %d times, want once", runCalls.Load())
	}

	// Asking for queued runs only leaves out run 200's jobs.
	jobs, err = c.QueuedJobs(context.Background(), "example-org/example-repo", "queued")
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].ID != 1 {
		t.Errorf("queued runs only: jobs = %+v", jobs)
	}
}

// A run deleted between listing the runs and listing its jobs is skipped.
func TestQueuedJobsSkipsDeletedRun(t *testing.T) {
	f, srv := newFake(t, time.Now())
	f.HandleFunc("GET /repos/example-org/example-repo/actions/runs", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("status") == "queued" {
			writeBody(t, w, `{"workflow_runs":[{"id":100},{"id":200}]}`)
			return
		}
		writeBody(t, w, `{"workflow_runs":[]}`)
	})
	f.HandleFunc("GET /repos/example-org/example-repo/actions/runs/100/jobs", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	})
	f.HandleFunc("GET /repos/example-org/example-repo/actions/runs/200/jobs", func(w http.ResponseWriter, r *http.Request) {
		writeBody(t, w, `{"jobs":[{"id":3,"run_id":200,"name":"a","status":"queued","labels":["self-hosted"],"created_at":"2026-01-01T00:01:00Z"}]}`)
	})
	c := newClient(t, srv, nil)
	jobs, err := c.QueuedJobs(context.Background(), "example-org/example-repo")
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].ID != 3 {
		t.Errorf("jobs = %+v", jobs)
	}
}

// The token endpoint reports the App's own rate limit, not the
// installation's, so it must not replace the installation's state.
func TestTokenResponseKeepsRateLimit(t *testing.T) {
	start := time.Unix(1_800_000_000, 0)
	var mu sync.Mutex
	now := start
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	f := &fakeGitHub{ServeMux: http.NewServeMux()}
	f.HandleFunc("POST /app/installations/7/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Limit", "60")
		w.Header().Set("X-RateLimit-Remaining", "59")
		encodeBody(t, w, map[string]any{"token": "tok", "expires_at": clock().Add(time.Hour)})
	})
	f.HandleFunc("GET /repos/example-org/example-repo/actions/runners/5", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"v1"`)
		if r.Header.Get("If-None-Match") == `"v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Remaining", "4990")
		writeBody(t, w, `{"id":5}`)
	})
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c := newClient(t, srv, clock)
	ctx := context.Background()
	if _, err := c.GetRunner(ctx, "example-org/example-repo", 5); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	now = start.Add(56 * time.Minute)
	mu.Unlock()
	if _, err := c.GetRunner(ctx, "example-org/example-repo", 5); err != nil {
		t.Fatal(err)
	}
	if rl := c.RateLimit(); rl.Limit != 5000 || rl.Remaining != 4990 {
		t.Errorf("rate limit = %+v, want the installation's 4990 of 5000", rl)
	}
}

func TestGenerateJITConfig(t *testing.T) {
	f, srv := newFake(t, time.Now())
	f.HandleFunc("POST /repos/example-org/example-repo/actions/runners/generate-jitconfig", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Name          string   `json:"name"`
			RunnerGroupID int      `json:"runner_group_id"`
			Labels        []string `json:"labels"`
			WorkFolder    string   `json:"work_folder"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || r.Header.Get("Authorization") != "token tok-1" {
			http.Error(w, "bad request", 400)
			return
		}
		if body.Name != "example-check-ab12" || body.RunnerGroupID != 1 || body.WorkFolder != "_work" || strings.Join(body.Labels, ",") != "self-hosted,example" {
			http.Error(w, fmt.Sprintf(`{"message":"bad body %+v"}`, body), http.StatusUnprocessableEntity)
			return
		}
		w.WriteHeader(http.StatusCreated)
		writeBody(t, w, `{"runner":{"id":77,"name":"example-check-ab12"},"encoded_jit_config":"ZW5jb2RlZA=="}`)
	})
	c := newClient(t, srv, nil)
	jit, err := c.GenerateJITConfig(context.Background(), "example-org/example-repo", "example-check-ab12", []string{"self-hosted", "example"})
	if err != nil {
		t.Fatal(err)
	}
	if jit.RunnerID != 77 || jit.Name != "example-check-ab12" || jit.EncodedConfig != "ZW5jb2RlZA==" {
		t.Errorf("jit = %+v", jit)
	}
}

func TestBadRepo(t *testing.T) {
	_, srv := newFake(t, time.Now())
	c := newClient(t, srv, nil)
	for _, repo := range []string{"", "example-repo", "a/b/c", "/b", "a/"} {
		if _, err := c.QueuedJobs(context.Background(), repo); err == nil {
			t.Errorf("QueuedJobs(%q) succeeded", repo)
		}
	}
}

func TestETagCacheEvicts(t *testing.T) {
	var c etagCache
	for i := range maxCached + 10 {
		c.put(fmt.Sprint(i), "e", "", nil)
		c.get("0") // keep the first entry in use
	}
	if c.len() != maxCached {
		t.Errorf("cache holds %d, want %d", c.len(), maxCached)
	}
	if c.get("0") == nil || c.get("1") != nil {
		t.Error("evicted the wrong entry")
	}
}
