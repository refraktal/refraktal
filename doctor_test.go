package refraktal

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"refraktal.dev/refraktal/internal/acp/acptest"
	"refraktal.dev/refraktal/internal/harness"
	"refraktal.dev/refraktal/internal/proc"
)

func TestDoctorTextReportsUncertainCleanup(t *testing.T) {
	rep := DoctorReport{
		Harnesses: []HarnessCheck{{Name: "claude", Status: StatusOK, Unknown: 2}},
		Cleanup:   CleanupCheck{Dead: 1, Alive: 1, Unknown: 2},
	}
	var out bytes.Buffer
	WriteDoctorText(&out, rep)
	for _, want := range []string{"incomplete", "1 still running", "2 unknown", "2 processes could not be checked after shutdown"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("doctor text missing %q: %s", want, out.String())
		}
	}
}

func TestShutdownCountsIncludeSweepsWithoutDuplicatePIDs(t *testing.T) {
	alive, unknown := shutdownCounts(proc.ShutdownReport{
		Kill:  proc.KillReport{Alive: []proc.Proc{{PID: 10}}, Unknown: []proc.Proc{{PID: 11}}},
		Swept: proc.KillReport{Alive: []proc.Proc{{PID: 10}, {PID: 12}}, Unknown: []proc.Proc{{PID: 11}, {PID: 13}}},
	})
	if alive != 2 || unknown != 2 {
		t.Fatalf("shutdown counts: alive %d, unknown %d; want 2 each", alive, unknown)
	}
}

func TestMain(m *testing.M) {
	// A fake agent is named explicitly (ACPTEST_MODE) and plays the harness,
	// which carries the helper switch for its shell prefix: check it first.
	runForgedAgent() // a fake agent that forges its cost or child report
	acptest.RunIfAgent()
	proc.MaybeShellPrefix()
	if len(os.Args) > 1 && os.Args[1] == fakeCheckArg {
		os.Exit(fakeCheck(os.Args[2:]))
	}
	acptest.RunIfTorture()
	if len(os.Args) > 2 && os.Args[1] == childACPArg {
		os.Exit(childACP(os.Args[2]))
	}
	os.Exit(m.Run())
}

// fake is a harness backed by the fake ACP agent in the given mode.
type fake struct {
	name, agentMode, mcp string
	prepErr              error
	prompts              string   // file the fake agent appends to on session/prompt
	env                  []string // extra environment for the fake agent
	prefixVar            string   // the shell-prefix variable the fake harness declares
}

func (f fake) Name() string         { return f.name }
func (f fake) Mode() string         { return "default" }
func (f fake) ReadOnlyMode() string { return "plan" }
func (f fake) Prepare(_ context.Context, o harness.Options) (harness.Launch, error) {
	if f.prepErr != nil {
		return harness.Launch{Installed: ""}, f.prepErr
	}
	argv, env := acptest.Command(f.agentMode)
	mcp := cmp.Or(f.mcp, o.MCP)
	return harness.Launch{Argv: argv, Env: append(append(env, "ACPTEST_PROMPTS="+f.prompts), f.env...), MCP: mcp, ShellPrefixVar: f.prefixVar,
		Adapter: "fake-acp@1.0.0", Bundled: "1.0.0", Installed: "1.0.1"}, nil
}

type rig struct {
	root, live, prompts, neutral string
}

func newRig(t *testing.T) rig {
	t.Helper()
	d := t.TempDir()
	return rig{root: filepath.Join(d, "repo"), live: filepath.Join(d, "live"), prompts: filepath.Join(d, "prompts"),
		neutral: filepath.Join(d, "neutral")}
}

func (r rig) doctor(t *testing.T, hs ...harness.Harness) DoctorReport {
	t.Helper()
	if hs == nil {
		hs = []harness.Harness{} // never the real defaults (nil) in a test
	}
	os.MkdirAll(r.root, 0o755)
	rep, err := Doctor(context.Background(), DoctorOptions{Root: r.root, LiveDir: r.live, Harnesses: hs,
		Getenv: func(string) string { return "" }, Timeout: 10 * time.Second, Neutral: r.neutral,
		ConfigPath: filepath.Join(r.root, "no-config.json")})
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func TestDoctorAllUsable(t *testing.T) {
	r := newRig(t)
	rep := r.doctor(t,
		fake{name: "claude", agentMode: "turn", mcp: harness.MCPIsolated, prompts: r.prompts},
		fake{name: "codex", agentMode: "turn", mcp: harness.MCPInherit, prompts: r.prompts},
		fake{name: "agy", agentMode: "auth", mcp: harness.MCPIsolated, prompts: r.prompts})
	if rep.ExitCode() != ExitOK || strings.Join(rep.Usable, ",") != "claude,codex,agy" {
		t.Fatalf("report %+v", rep)
	}
	for _, c := range rep.Harnesses {
		if c.Status != StatusOK || !c.ModeOK || c.Agent != "acptest 1.0.0" || c.Adapter != "fake-acp@1.0.0" ||
			c.Bundled != "1.0.0" || c.Installed != "1.0.1" || c.LeftRunning != 0 {
			t.Errorf("check %+v", c)
		}
	}
	if _, err := os.Stat(r.prompts); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("doctor sent a prompt")
	}
	if left, _ := filepath.Glob(filepath.Join(r.live, "*.json")); len(left) != 0 {
		t.Fatalf("live records left after clean shutdowns: %v", left)
	}
	if b, _ := os.ReadFile(filepath.Join(r.root, ".refraktal", ".gitignore")); string(b) != "*\n" {
		t.Fatalf(".refraktal/.gitignore = %q", b)
	}
	if !strings.HasPrefix(rep.Run, filepath.Join(".refraktal", "runs")+string(filepath.Separator)) {
		t.Fatalf("run folder %q", rep.Run)
	}
	events, err := os.ReadFile(filepath.Join(r.root, rep.Run, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(events), acptest.AuthEmail) {
		t.Fatal("account email reached the run log")
	}
	// Each session folder must be the neutral folder, never the repository.
	if cwds, _ := os.ReadFile(r.prompts + ".cwd"); strings.Contains(string(cwds), r.root) || !strings.Contains(string(cwds), r.neutral) {
		t.Fatalf("session folders %q: want under %s, never %s", cwds, r.neutral, r.root)
	}
	if left, _ := os.ReadDir(r.neutral); len(left) != 0 {
		t.Fatalf("neutral session folders not removed: %d left", len(left))
	}
	if rep.Decider.Kind != "fallback" {
		t.Fatalf("decider %+v", rep.Decider)
	}
}

func TestDoctorReportsEachFailure(t *testing.T) {
	r := newRig(t)
	rep := r.doctor(t,
		fake{name: "claude", agentMode: "noauth", prompts: r.prompts},
		fake{name: "codex", prepErr: errors.New("npm not found"), prompts: r.prompts},
		fake{name: "agy", agentMode: "oldproto", prompts: r.prompts},
		fake{name: "spare", agentMode: "turn", prompts: r.prompts})
	want := map[string]string{"claude": StatusLoggedOut, "codex": StatusUnavailable, "agy": StatusError, "spare": StatusOK}
	for _, c := range rep.Harnesses {
		if c.Status != want[c.Name] || (c.Status != StatusOK && c.Reason == "") {
			t.Errorf("%s: status %q reason %q, want %q", c.Name, c.Status, c.Reason, want[c.Name])
		}
	}
	if rep.ExitCode() != ExitOK || strings.Join(rep.Usable, ",") != "spare" {
		t.Fatalf("usable %v", rep.Usable)
	}
}

func TestDoctorExitsNonzeroWhenNothingIsUsable(t *testing.T) {
	r := newRig(t)
	rep := r.doctor(t, fake{name: "claude", agentMode: "noauth", prompts: r.prompts},
		fake{name: "agy", prepErr: errors.New("agy not installed")})
	if rep.ExitCode() != ExitNothingUsable || len(rep.Usable) != 0 {
		t.Fatalf("report %+v", rep)
	}
}

func TestDoctorCleansUpDeadRuns(t *testing.T) {
	r := newRig(t)
	os.MkdirAll(r.live, 0o700)
	owner := exec.Command("sleep", "30")
	owner.Start()
	left := exec.Command("sleep", "30")
	left.Start()
	defer func() { left.Process.Kill(); left.Wait() }()
	all, _ := proc.List()
	rec := proc.Record{Owner: all[owner.Process.Pid], Child: all[left.Process.Pid]}
	owner.Process.Kill()
	owner.Wait()
	b, _ := json.Marshal(rec)
	os.WriteFile(filepath.Join(r.live, "1-2.json"), b, 0o600)

	rep := r.doctor(t, fake{name: "claude", agentMode: "turn", prompts: r.prompts})
	if rep.Cleanup.Dead != 1 || rep.Cleanup.Killed != 1 {
		t.Fatalf("cleanup %+v", rep.Cleanup)
	}
	if all, _ := proc.List(); all[left.Process.Pid].Same(rec.Child) {
		t.Fatal("leftover process still running")
	}
}

func TestDoctorDeciderAndJSON(t *testing.T) {
	r := newRig(t)
	os.MkdirAll(r.root, 0o755)
	rep, err := Doctor(context.Background(), DoctorOptions{Root: r.root, LiveDir: r.live, Neutral: r.neutral,
		ConfigPath:      filepath.Join(r.root, "no-config.json"),
		Harnesses:       []harness.Harness{fake{name: "claude", agentMode: "auth", mcp: harness.MCPIsolated, prompts: r.prompts}},
		Getenv:          func(k string) string { return map[string]string{"GEMINI_API_KEY": "secret-value"}[k] },
		DeciderEndpoint: deciderEndpoint(t)})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := WriteDoctorJSON(&out, rep); err != nil {
		t.Fatal(err)
	}
	var types []string
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("not JSONL: %q", line)
		}
		types = append(types, m["type"].(string))
	}
	if strings.Join(types, ",") != "harness,decider,cleanup,summary" {
		t.Fatalf("line types %v", types)
	}
	for _, secret := range []string{"secret-value", acptest.AuthEmail} {
		if strings.Contains(out.String(), secret) {
			t.Fatalf("output contains %q", secret)
		}
	}
	if !strings.Contains(out.String(), `"kind":"gemini"`) || !strings.Contains(out.String(), `"exit":0`) {
		t.Fatalf("output:\n%s", out.String())
	}
	var text bytes.Buffer
	WriteDoctorText(&text, rep)
	if !strings.Contains(text.String(), "usable: claude") || strings.Contains(text.String(), "secret-value") {
		t.Fatalf("text:\n%s", text.String())
	}
}

func TestDefaultRootRefusesTemp(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("TMPDIR", dir)
	_, err := Doctor(context.Background(), DoctorOptions{LiveDir: t.TempDir(), Harnesses: []harness.Harness{}})
	if err == nil || !strings.Contains(err.Error(), "temporary") {
		t.Fatalf("want a refusal for a run folder under a temp folder, got %v", err)
	}
}

// deciderEndpoint is a local Decisions API answering with a recorded response.
func deciderEndpoint(t *testing.T) string {
	srv, _ := geminiServer(t)
	return srv.URL
}

// The decider check: no key means fallback; an unreachable API means
// fallback with the reason; a key in the repository's .env is found.
func TestDoctorDeciderCheck(t *testing.T) {
	r := newRig(t)
	os.MkdirAll(r.root, 0o755)
	opts := DoctorOptions{Root: r.root, LiveDir: r.live, Neutral: r.neutral, ConfigPath: filepath.Join(r.root, "none.json"),
		Harnesses: []harness.Harness{}, Getenv: func(string) string { return "" }}
	if d := checkDecider(context.Background(), opts); d.Kind != "fallback" || !strings.Contains(d.Reason, "not set") {
		t.Fatalf("no key: %+v", d)
	}
	exec.Command("git", "-C", r.root, "init", "-q").Run() // .env counts only in a git work tree, untracked
	os.WriteFile(filepath.Join(r.root, ".env"), []byte("GEMINI_API_KEY=from-dotenv\n"), 0o600)
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "down", 503) }))
	defer down.Close()
	opts.DeciderEndpoint = down.URL
	if d := checkDecider(context.Background(), opts); d.Kind != "fallback" || d.KeySource != ".env" || !strings.Contains(d.Reason, "unreachable") {
		t.Fatalf("unreachable: %+v", d)
	}
	opts.DeciderEndpoint = deciderEndpoint(t)
	if d := checkDecider(context.Background(), opts); d.Kind != "gemini" || d.KeySource != ".env" || d.LatencyMS <= 0 {
		t.Fatalf("reachable: %+v", d)
	}
}

func TestDefaultRootIgnoresGitEnvironment(t *testing.T) {
	r, other := newRunRig(t), newRunRig(t)
	t.Chdir(r.repo)
	t.Setenv("GIT_DIR", filepath.Join(other.repo, ".git"))
	t.Setenv("GIT_WORK_TREE", other.repo)
	want, err := filepath.EvalSymlinks(r.repo)
	if err != nil {
		t.Fatal(err)
	}
	got, err := defaultRoot()
	if err != nil || got != want {
		t.Fatalf("default root = %q, %v; want %q", got, err, want)
	}
}

type inspectingHarness struct {
	prepare func(harness.Options) (harness.Launch, error)
}

func (inspectingHarness) Name() string         { return "claude" }
func (inspectingHarness) Mode() string         { return "default" }
func (inspectingHarness) ReadOnlyMode() string { return "plan" }
func (h inspectingHarness) Prepare(_ context.Context, o harness.Options) (harness.Launch, error) {
	return h.prepare(o)
}

func TestDoctorRunFolderSwapDoesNotRedirectWrites(t *testing.T) {
	r := newRig(t)
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(outside, "claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(outside, "claude", "stderr")
	if err := os.WriteFile(victim, []byte("keep\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(r.root, 0o755); err != nil {
		t.Fatal(err)
	}
	var held string
	h := inspectingHarness{prepare: func(o harness.Options) (harness.Launch, error) {
		if o.ScratchRoot == nil {
			t.Error("doctor did not pass a pinned scratch root")
		}
		// Fail at exec, after opening stderr, without needing process listings.
		return harness.Launch{Argv: []string{filepath.Join(r.root, "missing-agent")}}, nil
	}}
	rep, err := Doctor(context.Background(), DoctorOptions{
		Root: r.root, LiveDir: r.live, Harnesses: []harness.Harness{h}, Neutral: r.neutral,
		ConfigPath: filepath.Join(r.root, "no-config.json"),
		Getenv: func(string) string {
			paths, err := filepath.Glob(filepath.Join(r.root, ".refraktal", "runs", "*"))
			if err != nil || len(paths) != 1 {
				t.Fatalf("run paths = %q, %v", paths, err)
			}
			held = paths[0] + "-held"
			if err := os.Rename(paths[0], held); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, paths[0]); err != nil {
				t.Fatal(err)
			}
			return ""
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Harnesses) != 1 || rep.Harnesses[0].Status != StatusUnavailable {
		t.Fatalf("unexpected report: %+v", rep)
	}
	if b, err := os.ReadFile(victim); err != nil || string(b) != "keep\n" {
		t.Errorf("outside stderr overwritten: %q, %v", b, err)
	}
	if entries, err := os.ReadDir(filepath.Join(outside, "claude")); err != nil || len(entries) != 1 {
		t.Errorf("scratch writes escaped: %v, %v", entries, err)
	}
	for _, name := range []string{"events.jsonl", "claude/tmp", "claude/stderr"} {
		if _, err := os.Stat(filepath.Join(held, name)); err != nil {
			t.Errorf("%s missing from pinned run: %v", name, err)
		}
	}
	if entries, err := os.ReadDir(r.neutral); err != nil || len(entries) != 0 {
		t.Errorf("neutral folder not cleaned: %v, %v", entries, err)
	}
}

func TestSessionRequiresScratchRoot(t *testing.T) {
	_, err := openSession(context.Background(), sessionSpec{h: fake{prepErr: errors.New("should not prepare")}})
	if err == nil || statusOf(err) != StatusError || !strings.Contains(err.Error(), "scratch root required") {
		t.Fatalf("missing root accepted: %v", err)
	}
}
