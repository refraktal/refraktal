package route

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func gitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if out, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	return dir
}

// fakeGit puts a git on PATH that records its arguments and environment.
func fakeGit(t *testing.T) string {
	t.Helper()
	bin := t.TempDir()
	log := filepath.Join(bin, "log")
	os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\n{ echo \"ARGS $*\"; env; } >> "+log+"\necho true\n"), 0o755)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	return log
}

// 1. A git call made for routing never gets the API key or GIT_* variables,
// and runs with fsmonitor and hooks off.
func TestRoutingGitCallsAreSafe(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "must-not-leak")
	t.Setenv("GIT_DIR", "/elsewhere")
	log := fakeGit(t)
	untrackedDotEnv(t.TempDir())
	b, _ := os.ReadFile(log)
	s := string(b)
	if strings.Contains(s, "must-not-leak") || strings.Contains(s, "GIT_DIR=") || !strings.Contains(s, "core.fsmonitor=false") || !strings.Contains(s, "core.hooksPath=/dev/null") {
		t.Fatalf("git call:\n%s", s)
	}
}

func fixtureRaw(t *testing.T) []byte { return liveFixture(t) }

// 2. A redirect is a Decider error: the router falls back.
func TestRedirectFallsBack(t *testing.T) {
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(fixtureRaw(t)) }))
	defer good.Close()
	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, good.URL, http.StatusTemporaryRedirect)
	}))
	defer redir.Close()
	res, _ := New(Options{APIKey: "k", Endpoint: redir.URL, CacheDir: "-"}).Route(context.Background(), bugfix)
	if res.Decision.Decider != "fallback" {
		t.Fatalf("followed a redirect: %+v", res.Decision.Decider)
	}
}

// 3. A decision log git tracks is never appended to.
func TestTrackedDecisionLogIsNotWritten(t *testing.T) {
	dir := gitRepo(t)
	logPath := filepath.Join(dir, "decisions.jsonl")
	os.WriteFile(logPath, []byte("keep\n"), 0o600)
	cmd := exec.Command("sh", "-c", "git add -f decisions.jsonl && git -c user.name=t -c user.email=t@t commit -q -m x")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	res, _ := New(Options{CacheDir: "-", LogPath: logPath}).Route(context.Background(), bugfix)
	if b, _ := os.ReadFile(logPath); string(b) != "keep\n" || !strings.Contains(strings.Join(res.Notes, " "), "tracked") {
		t.Fatalf("log %q notes %v", b, res.Notes)
	}
}

// 4. A provider's error body never reaches the fallback reason.
func TestProviderErrorBodyStaysOut(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"error":{"code":403,"message":"SECRET-DETAIL project 12345"}}`))
	}))
	defer srv.Close()
	res, _ := New(Options{APIKey: "k", Endpoint: srv.URL, CacheDir: "-"}).Route(context.Background(), bugfix)
	if strings.Contains(res.Decision.FallbackReason, "SECRET-DETAIL") || !strings.Contains(res.Decision.FallbackReason, "HTTP 403") {
		t.Fatalf("reason %q", res.Decision.FallbackReason)
	}
}

// 5. Malformed answers fall back and are not cached.
func TestInvalidAnswersFallBackUncached(t *testing.T) {
	var base map[string]any
	json.Unmarshal(fixtureRaw(t), &base)
	mutate := map[string]func(map[string]any){
		"unknown harness":  func(a map[string]any) { a["harness"].(map[string]any)["probabilities"].(map[string]any)["gpt-9"] = 0.1 },
		"probability > 1":  func(a map[string]any) { a["harness"].(map[string]any)["probabilities"].(map[string]any)["codex"] = 1.5 },
		"no probabilities": func(a map[string]any) { delete(a["harness"].(map[string]any), "probabilities") },
		"confidence > 1":   func(a map[string]any) { a["harness"].(map[string]any)["confidence"] = 2.0 },
		"noul > 1":         func(a map[string]any) { a["fan_out"].(map[string]any)["noul"] = 3.0 },
	}
	for name, m := range mutate {
		var body map[string]any
		b, _ := json.Marshal(base)
		json.Unmarshal(b, &body)
		m(body["answers"].(map[string]any))
		raw, _ := json.Marshal(body)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(raw) }))
		cache := t.TempDir()
		res, _ := New(Options{APIKey: "k", Endpoint: srv.URL, CacheDir: cache}).Route(context.Background(), bugfix)
		srv.Close()
		entries, _ := os.ReadDir(cache)
		if res.Decision.Decider != "fallback" || len(entries) != 0 {
			t.Errorf("%s: decider %s, cache entries %d", name, res.Decision.Decider, len(entries))
		}
	}
}

// 6. Cache writes use a unique temp file: a fixed temp name in the way must
// not stop the cache.
func TestCacheWriteUsesAUniqueTempFile(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(fixtureRaw(t)) }))
	defer srv.Close()
	cache := t.TempDir()
	os.Mkdir(filepath.Join(cache, requestHash(bugfix)+".tmp"), 0o700) // a fixed temp name is taken
	New(Options{APIKey: "k", Endpoint: srv.URL, CacheDir: cache}).Route(context.Background(), bugfix)
	if _, err := os.Stat(filepath.Join(cache, requestHash(bugfix)+".json")); err != nil {
		t.Fatal("decision not cached")
	}
}

// 8 and 9. Records carry the run ID, and a cache hit keeps the raw response.
func TestRecordsCarryRunIDAndCachedRaw(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(fixtureRaw(t)) }))
	defer srv.Close()
	dir := gitRepo(t)
	logPath := filepath.Join(dir, "decisions.jsonl")
	r := New(Options{APIKey: "k", Endpoint: srv.URL, CacheDir: t.TempDir(), LogPath: logPath, RunID: "run-42"})
	r.Route(context.Background(), bugfix)
	if res, _ := r.Route(context.Background(), bugfix); !res.Cached {
		t.Fatal("second route not cached")
	}
	b, _ := os.ReadFile(logPath)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 2 {
		t.Fatalf("%d records", len(lines))
	}
	for i, l := range lines {
		var rec map[string]any
		json.Unmarshal([]byte(l), &rec)
		if rec["run_id"] != "run-42" || rec["raw_response"] == nil {
			t.Errorf("record %d: run_id %v, raw present %v", i, rec["run_id"], rec["raw_response"] != nil)
		}
	}
}
