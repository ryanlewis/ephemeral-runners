package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
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
	testRunnerID = 99
	testJIT      = "secret-jit-config"
	testToken    = "tok-secret"
)

var testExpiry = time.Date(2027, 1, 2, 3, 4, 5, 0, time.UTC)

// fakeGitHub stands in for the GitHub API: one repo, example-org/example-repo,
// with one queued run holding a job for this host, a job for other runners
// and a job already running. GETs carry an ETag and answer a matching
// If-None-Match with a 304. Minting creates runner 99, which stays listed
// until it is deleted.
type fakeGitHub struct {
	t *testing.T

	// fail maps "METHOD /path" to a status to answer with instead.
	fail map[string]int
	// getAfterDelete, if set, is the status for GET runner 99 once deleted.
	getAfterDelete int
	// keepOnDelete leaves runner 99 in place when it is deleted.
	keepOnDelete bool
	// unlisted leaves runner 99 out of the runner list.
	unlisted bool
	// hook, if set, sees each request before it is answered.
	hook func(r *http.Request)

	mu      sync.Mutex
	calls   []string
	minted  string // name of the minted runner
	exists  bool
	deleted bool
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	call := r.Method + " " + r.URL.RequestURI()
	f.calls = append(f.calls, call)
	if f.hook != nil {
		f.hook(r)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-RateLimit-Limit", "5000")
	w.Header().Set("X-RateLimit-Remaining", "4990")
	w.Header().Set("X-RateLimit-Used", "10")
	w.Header().Set("X-RateLimit-Reset", "1800000000")

	if code, ok := f.fail[r.Method+" "+r.URL.Path]; ok {
		w.WriteHeader(code)
		if _, err := fmt.Fprintf(w, `{"message":"injected %d"}`, code); err != nil {
			f.t.Errorf("write injected %d: %v", code, err)
		}
		return
	}
	if r.URL.Path != "/app/installations/7/access_tokens" && r.Header.Get("Authorization") != "token "+testToken {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	const rp = "/repos/example-org/example-repo/actions"
	var body any
	switch r.Method + " " + r.URL.Path {
	case "POST /app/installations/7/access_tokens":
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		body = map[string]any{"token": testToken, "expires_at": testExpiry}
	case "GET /rate_limit":
		body = map[string]any{"resources": map[string]any{"core": map[string]any{
			"limit": 5000, "remaining": 4999, "used": 1, "reset": 1700000000}}}
	case "GET " + rp + "/runs":
		runs := []map[string]any{}
		if r.URL.Query().Get("status") == "queued" {
			runs = append(runs, map[string]any{"id": 10})
		}
		body = map[string]any{"workflow_runs": runs}
	case "GET " + rp + "/runs/10/jobs":
		created := time.Now().Add(-90 * time.Second)
		body = map[string]any{"jobs": []map[string]any{
			{"id": 1, "run_id": 10, "name": "build", "status": "queued", "labels": []string{"self-hosted", "example", "large"}, "created_at": created},
			{"id": 2, "run_id": 10, "name": "lint", "status": "queued", "labels": []string{"ubuntu-latest"}, "created_at": created},
			{"id": 3, "run_id": 10, "name": "test", "status": "in_progress", "labels": []string{"self-hosted", "example"}, "created_at": created},
		}}
	case "POST " + rp + "/runners/generate-jitconfig":
		var in struct {
			Name   string   `json:"name"`
			Labels []string `json:"labels"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			f.t.Errorf("generate-jitconfig body: %v", err)
		}
		if got := strings.Join(in.Labels, " "); got != "self-hosted example" {
			f.t.Errorf("generate-jitconfig labels [%s], want the configured labels", got)
		}
		f.minted, f.exists = in.Name, true
		body = map[string]any{"runner": map[string]any{"id": testRunnerID, "name": in.Name}, "encoded_jit_config": testJIT}
	case "GET " + rp + "/runners/99":
		if !f.exists {
			code := http.StatusNotFound
			if f.deleted && f.getAfterDelete != 0 {
				code = f.getAfterDelete
			}
			w.WriteHeader(code)
			return
		}
		body = f.runner()
	case "GET " + rp + "/runners":
		runners := []any{map[string]any{"id": 5, "name": "someone-else", "labels": []any{}}}
		if f.exists && !f.unlisted {
			runners = append(runners, f.runner())
		}
		body = map[string]any{"total_count": len(runners), "runners": runners}
	case "DELETE " + rp + "/runners/99":
		if !f.exists {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		f.deleted = true
		f.exists = f.keepOnDelete
		w.WriteHeader(http.StatusNoContent)
		return
	default:
		f.t.Errorf("unexpected request %s", call)
		w.WriteHeader(http.StatusNotFound)
		return
	}

	// This runs off the test goroutine, so a failure is reported with
	// t.Errorf rather than t.Fatal.
	b, err := json.Marshal(body)
	if err != nil {
		f.t.Errorf("encode %s: %v", call, err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if r.Method == http.MethodGet {
		sum := sha256.Sum256(b)
		etag := `"` + hex.EncodeToString(sum[:8]) + `"`
		// The jobs body holds a fresh timestamp, so its ETag is fixed.
		if strings.HasSuffix(r.URL.Path, "/jobs") {
			etag = `"jobs"`
		}
		w.Header().Set("ETag", etag)
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	if _, err := w.Write(b); err != nil {
		f.t.Errorf("write %s: %v", call, err)
	}
}

func (f *fakeGitHub) runner() map[string]any {
	return map[string]any{"id": testRunnerID, "name": f.minted, "os": "linux", "status": "offline", "busy": false,
		"labels": []map[string]string{{"name": "self-hosted"}, {"name": "example"}}}
}

func (f *fakeGitHub) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// setup starts f and writes a key file and a config file that points at it,
// returning the config path.
func setup(t *testing.T, f *fakeGitHub) string {
	t.Helper()
	f.t = t
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	keyFile := filepath.Join(dir, "app.pem")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(testKey)})
	if err := os.WriteFile(keyFile, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := fmt.Sprintf(`{
  "github": {"app_id": 42, "installation_id": 7, "private_key_file": %q, "api_url": %q},
  "repos": ["example-org/example-repo"],
  "labels": ["self-hosted", "example"],
  "name_prefix": "example"
}`, keyFile, srv.URL)
	cfgFile := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgFile, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfgFile
}

func check(t *testing.T, f *fakeGitHub, args ...string) (string, error) {
	t.Helper()
	cfgFile := setup(t, f)
	var out strings.Builder
	err := runCheck(context.Background(), append([]string{"-config", cfgFile}, args...), &out)
	return out.String(), err
}

var (
	queuedAgo  = regexp.MustCompile(`queued \S+ ago`)
	runnerName = regexp.MustCompile(`example-check-[0-9a-f]{4}`)
)

// normalise replaces the parts of the output that vary from run to run.
func normalise(s string) string {
	s = queuedAgo.ReplaceAllString(s, "queued AGO ago")
	return runnerName.ReplaceAllString(s, "example-check-XXXX")
}

const pollOutput = `authenticated: installation token valid until 2027-01-02T03:04:05Z
rate limit: 4999 of 5000 remaining, resets 2023-11-14T22:13:20Z

poll 1
example-org/example-repo: 1 queued for this host, 1 for other runners
  large    job 1 run 10 "build" labels [self-hosted example large], queued AGO ago
poll 1: 3 requests, 0 not modified (304)

poll 2
example-org/example-repo: 1 queued for this host, 1 for other runners
  large    job 1 run 10 "build" labels [self-hosted example large], queued AGO ago
poll 2: 3 requests, 3 not modified (304)
rate limit: 4990 of 5000 remaining, resets 2027-01-15T08:00:00Z
`

var pollCalls = []string{
	"POST /app/installations/7/access_tokens",
	"GET /rate_limit",
	"GET /repos/example-org/example-repo/actions/runs?status=queued&per_page=100",
	"GET /repos/example-org/example-repo/actions/runs?status=in_progress&per_page=100",
	"GET /repos/example-org/example-repo/actions/runs/10/jobs?per_page=100",
	"GET /repos/example-org/example-repo/actions/runs?status=queued&per_page=100",
	"GET /repos/example-org/example-repo/actions/runs?status=in_progress&per_page=100",
	"GET /repos/example-org/example-repo/actions/runs/10/jobs?per_page=100",
}

const mintOutput = `
mint in example-org/example-repo
minted runner 99 example-check-XXXX with labels [self-hosted example] (JIT config 17 bytes, not shown)
get runner 99: name example-check-XXXX, status offline, busy false, labels [self-hosted example]
list runners: 2 registered, example-check-XXXX listed: true
deleted runner 99
get runner 99: not found, it is gone
`

var mintCalls = []string{
	"POST /repos/example-org/example-repo/actions/runners/generate-jitconfig",
	"GET /repos/example-org/example-repo/actions/runners/99",
	"GET /repos/example-org/example-repo/actions/runners?per_page=100",
	"DELETE /repos/example-org/example-repo/actions/runners/99",
	"GET /repos/example-org/example-repo/actions/runners/99",
}

func assertCalls(t *testing.T, got []string, want ...[]string) {
	t.Helper()
	var all []string
	for _, w := range want {
		all = append(all, w...)
	}
	if strings.Join(got, "\n") != strings.Join(all, "\n") {
		t.Errorf("calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(all, "\n"))
	}
}

func assertNoSecrets(t *testing.T, out string) {
	t.Helper()
	for _, s := range []string{testJIT, testToken, "PRIVATE KEY"} {
		if strings.Contains(out, s) {
			t.Errorf("output shows %q:\n%s", s, out)
		}
	}
}

func TestCheck(t *testing.T) {
	f := &fakeGitHub{}
	out, err := check(t, f)
	if err != nil {
		t.Fatal(err)
	}
	if got := normalise(out); got != pollOutput {
		t.Errorf("output:\n%s\nwant:\n%s", got, pollOutput)
	}
	assertCalls(t, f.Calls(), pollCalls)
	assertNoSecrets(t, out)
}

func TestCheckMint(t *testing.T) {
	f := &fakeGitHub{}
	out, err := check(t, f, "-mint", "example-org/example-repo")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := normalise(out), pollOutput+mintOutput; got != want {
		t.Errorf("output:\n%s\nwant:\n%s", got, want)
	}
	assertCalls(t, f.Calls(), pollCalls, mintCalls)
	assertNoSecrets(t, out)
	if !runnerName.MatchString(f.minted) || len(f.minted) != len("example-check-0000") {
		t.Errorf("minted name %q, want example-check- and 4 hex digits", f.minted)
	}
}

func TestCheckArgs(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"unknown flag", []string{"-bogus"}, "flag provided but not defined: -bogus"},
		{"extra argument", []string{"-config", "x", "extra"}, "unexpected arguments: [extra]"},
		{"bad mint repo", []string{"-mint", "no-slash"}, `-mint "no-slash" is not in owner/name form`},
		{"missing config", []string{"-config", filepath.Join(t.TempDir(), "none.json")}, "none.json: no such file or directory"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out strings.Builder
			err := runCheck(context.Background(), tc.args, &out)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to contain %q", err, tc.want)
			}
			if tc.name != "unknown flag" && out.Len() != 0 {
				t.Errorf("output %q, want none", out.String())
			}
		})
	}
}

func TestCheckMissingKey(t *testing.T) {
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "config.json")
	cfg := fmt.Sprintf(`{
  "github": {"app_id": 42, "installation_id": 7, "private_key_file": %q},
  "repos": ["example-org/example-repo"],
  "labels": ["self-hosted"],
  "name_prefix": "example"
}`, filepath.Join(dir, "none.pem"))
	if err := os.WriteFile(cfgFile, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	err := runCheck(context.Background(), []string{"-config", cfgFile}, &out)
	if err == nil || !strings.Contains(err.Error(), "none.pem") {
		t.Errorf("err = %v, want the missing key file named", err)
	}
	if out.Len() != 0 {
		t.Errorf("output %q, want none", out.String())
	}
}

func TestCheckAPIErrors(t *testing.T) {
	for _, tc := range []struct {
		name, fail string
		wantErr    string
		wantOut    string
	}{
		{"token", "POST /app/installations/7/access_tokens",
			"get installation token: POST " + "SRV" + "/app/installations/7/access_tokens: 500 injected 500", ""},
		{"rate limit", "GET /rate_limit", "GET SRV/rate_limit: 500 injected 500", ""},
		{"queued jobs", "GET /repos/example-org/example-repo/actions/runs",
			"example-org/example-repo: GET SRV/repos/example-org/example-repo/actions/runs?status=queued&per_page=100: 500 injected 500",
			"authenticated: installation token valid until 2027-01-02T03:04:05Z\n" +
				"rate limit: 4999 of 5000 remaining, resets 2023-11-14T22:13:20Z\n\npoll 1\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeGitHub{fail: map[string]int{tc.fail: http.StatusInternalServerError}}
			cfgFile := setup(t, f)
			var out strings.Builder
			err := runCheck(context.Background(), []string{"-config", cfgFile, "-mint", "example-org/example-repo"}, &out)
			srv := serverURL(t, cfgFile)
			if want := strings.ReplaceAll(tc.wantErr, "SRV", srv); err == nil || err.Error() != want {
				t.Errorf("err = %v\nwant %s", err, want)
			}
			if out.String() != tc.wantOut {
				t.Errorf("output:\n%s\nwant:\n%s", out.String(), tc.wantOut)
			}
		})
	}
}

// serverURL reads the fake's URL back from the config file.
func serverURL(t *testing.T, cfgFile string) string {
	t.Helper()
	b, err := os.ReadFile(cfgFile)
	if err != nil {
		t.Fatal(err)
	}
	var c struct {
		GitHub struct {
			APIURL string `json:"api_url"`
		} `json:"github"`
	}
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	return c.GitHub.APIURL
}

func TestMintErrors(t *testing.T) {
	const runner = "/repos/example-org/example-repo/actions/runners/99"
	const list = "/repos/example-org/example-repo/actions/runners"
	minted := mintCalls[:1]
	got := mintCalls[:2]
	listed := mintCalls[:3]
	deleted := mintCalls[:4]
	cleanup := []string{"DELETE " + runner}
	for _, tc := range []struct {
		name    string
		f       *fakeGitHub
		wantErr string
		// wantOut is the number of mint output lines printed, after the
		// leading blank line.
		wantOut int
		calls   [][]string
	}{
		{"generate", &fakeGitHub{fail: map[string]int{"POST " + list + "/generate-jitconfig": 422}},
			"generate JIT config: POST SRV" + list + "/generate-jitconfig: 422 injected 422", 1, [][]string{minted}},
		{"get runner, cleaned up", &fakeGitHub{fail: map[string]int{"GET " + runner: 500}},
			"get runner: GET SRV" + runner + ": 500 injected 500", 2, [][]string{got, cleanup}},
		{"get runner, cleanup finds it gone", &fakeGitHub{fail: map[string]int{"GET " + runner: 500, "DELETE " + runner: 404}},
			"get runner: GET SRV" + runner + ": 500 injected 500", 2, [][]string{got, cleanup}},
		{"get runner, cleanup fails", &fakeGitHub{fail: map[string]int{"GET " + runner: 500, "DELETE " + runner: 503}},
			"get runner: GET SRV" + runner + ": 500 injected 500\n" +
				"delete runner 99: DELETE SRV" + runner + ": 503 injected 503", 2, [][]string{got, cleanup}},
		{"list runners", &fakeGitHub{fail: map[string]int{"GET " + list: 500}},
			"list runners: GET SRV" + list + "?per_page=100: 500 injected 500", 3, [][]string{listed, cleanup}},
		{"not listed", &fakeGitHub{unlisted: true},
			"runner 99 missing from the list", 4, [][]string{listed, cleanup}},
		{"delete", &fakeGitHub{fail: map[string]int{"DELETE " + runner: 500}},
			"delete runner: DELETE SRV" + runner + ": 500 injected 500\n" +
				"delete runner 99: DELETE SRV" + runner + ": 500 injected 500", 4, [][]string{deleted, cleanup}},
		{"get deleted runner", &fakeGitHub{getAfterDelete: 500},
			"get deleted runner: GET SRV" + runner + ": 500 Internal Server Error", 5, [][]string{mintCalls}},
		{"still exists", &fakeGitHub{keepOnDelete: true},
			"runner 99 still exists after delete", 5, [][]string{mintCalls}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfgFile := setup(t, tc.f)
			var out strings.Builder
			err := runCheck(context.Background(), []string{"-config", cfgFile, "-mint", "example-org/example-repo"}, &out)
			if want := strings.ReplaceAll(tc.wantErr, "SRV", serverURL(t, cfgFile)); err == nil || err.Error() != want {
				t.Errorf("err = %v\nwant %s", err, want)
			}
			lines := strings.SplitAfter(mintOutput, "\n")
			want := pollOutput + strings.Join(lines[:1+tc.wantOut], "")
			if tc.f.unlisted {
				want = strings.Replace(want, "2 registered, example-check-XXXX listed: true",
					"1 registered, example-check-XXXX listed: false", 1)
			}
			if got := normalise(out.String()); got != want {
				t.Errorf("output:\n%s\nwant:\n%s", got, want)
			}
			assertNoSecrets(t, out.String())
			assertCalls(t, tc.f.Calls(), append([][]string{pollCalls}, tc.calls...)...)
		})
	}
}

// TestMintCancelled cancels the context once the runner exists, as Ctrl-C
// would, and checks that the runner is still deleted.
func TestMintCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	const runner = "/repos/example-org/example-repo/actions/runners/99"
	f := &fakeGitHub{hook: func(r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == runner {
			cancel()
		}
	}}
	cfgFile := setup(t, f)
	var out strings.Builder
	err := runCheck(ctx, []string{"-config", cfgFile, "-mint", "example-org/example-repo"}, &out)
	if !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "delete runner") {
		t.Errorf("err = %v, want only the cancelled call", err)
	}
	calls := f.Calls()
	if last := calls[len(calls)-1]; last != "DELETE "+runner {
		t.Errorf("last call %q, want the cleanup delete", last)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.exists {
		t.Error("runner 99 still exists")
	}
}
