package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

var fakeBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "acp-fake-")
	if err != nil {
		panic(err)
	}
	fakeBin = filepath.Join(dir, "fakeagent")
	if out, err := exec.Command("go", "build", "-o", fakeBin, "./cmd/fakeagent").CombinedOutput(); err != nil {
		panic(string(out))
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

type line struct {
	Type    string          `json:"type"`
	Phase   string          `json:"phase"`
	TMs     *int64          `json:"t_ms"`
	Method  string          `json:"method"`
	Refused string          `json:"refused"`
	Update  json.RawMessage `json:"update"`
}

// drive runs the client against the fake in the given mode and returns the
// summary, the parsed stdout lines and the session cwd.
func drive(t *testing.T, mode string, limit time.Duration) (Summary, []line, string, error) {
	t.Helper()
	parent := t.TempDir()
	cwd := filepath.Join(parent, "work")
	if err := os.Mkdir(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	cfg := config{harness: "fake", cmd: fakeBin + " " + mode, cwd: cwd, prompt: "hi",
		limit: limit, grace: 300 * time.Millisecond, linger: 150 * time.Millisecond}
	sum, err := run(cfg, &out, &errb)
	var lines []line
	for _, l := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var v line
		if e := json.Unmarshal([]byte(l), &v); e != nil {
			t.Fatalf("stdout line is not JSON: %q", l)
		}
		lines = append(lines, v)
	}
	if last := lines[len(lines)-1]; last.Type != "summary" {
		t.Fatalf("last line is %q, want summary", last.Type)
	}
	return sum, lines, cwd, err
}

func TestHappyPath(t *testing.T) {
	sum, lines, _, err := drive(t, "default", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Stage != "session/prompt" || sum.StopReason != "end_turn" || sum.TimedOut || sum.Killed {
		t.Fatalf("unexpected summary: %+v", sum)
	}
	// 1 available_commands + 2 chunks + 2 usage + tool_call + tool_call_update
	if sum.Updates != 7 || sum.UpdateKinds["agent_message_chunk"] != 2 || sum.UpdateKinds["usage_update"] != 2 {
		t.Fatalf("updates = %d %v", sum.Updates, sum.UpdateKinds)
	}
	if sum.Permissions != 1 {
		t.Fatalf("permissions = %d", sum.Permissions)
	}
	if len(sum.Usage) != 2 {
		t.Fatalf("usage = %+v", sum.Usage)
	}
	first, last := sum.Usage[0], sum.Usage[1]
	if *first.Used != 1200 || *first.Size != 200000 || first.Cost != nil || first.When != "mid_turn" {
		t.Fatalf("first usage = %+v", first)
	}
	var cost struct {
		Amount   float64
		Currency string
	}
	json.Unmarshal(last.Cost, &cost)
	if *last.Used != 1500 || cost.Amount != 0.0123 || cost.Currency != "USD" || last.When != "at_end" {
		t.Fatalf("last usage = %+v", last)
	}
	if sum.FirstUsage != "mid_turn" || sum.FirstCost != "at_end" {
		t.Fatalf("first_usage=%s first_cost=%s", sum.FirstUsage, sum.FirstCost)
	}
	// Every update is one line; turn-phase lines carry a non-decreasing t_ms.
	var n int
	prev := int64(-1)
	for _, l := range lines {
		if l.Type != "update" {
			continue
		}
		n++
		if l.Phase == "setup" {
			continue
		}
		if l.TMs == nil || *l.TMs < prev {
			t.Fatalf("bad t_ms on %s", l.Update)
		}
		prev = *l.TMs
	}
	if n != sum.Updates {
		t.Fatalf("%d update lines, summary says %d", n, sum.Updates)
	}
	// The fake echoes the permission response: allow_once must win over allow_always.
	if !strings.Contains(string(mustJSON(lines)), `\"optionId\":\"once\"`) {
		t.Fatal("client did not select the allow_once option")
	}
}

func readFile(dir, name string) string {
	b, _ := os.ReadFile(filepath.Join(dir, name))
	return string(b)
}

func mustJSON(lines []line) []byte {
	var b bytes.Buffer
	for _, l := range lines {
		b.Write(l.Update)
	}
	return b.Bytes()
}

func TestFilePolicy(t *testing.T) {
	sum, lines, cwd, err := drive(t, "fs", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(cwd, "note.txt")); string(b) != "one\ntwo\nthree\n" {
		t.Fatalf("note.txt = %q", b)
	}
	if _, err := os.Stat(filepath.Join(cwd, "..", "escaped.txt")); err == nil {
		t.Fatal("client wrote outside the session cwd")
	}
	if sum.FSRequests != 2 {
		t.Fatalf("fs_requests = %d, want 2 served", sum.FSRequests)
	}
	if sum.Refused["fs/read_text_file"] != 1 || sum.Refused["fs/write_text_file"] != 1 || sum.Refused["terminal/create"] != 1 {
		t.Fatalf("refused = %v", sum.Refused)
	}
	all := string(mustJSON(lines))
	for _, want := range []string{
		`read={\"jsonrpc\":\"2.0\",\"id\":\"a3\",\"result\":{\"content\":\"two\\n\"}}`, // line=2 limit=1
		`\"error\":{\"code\":-32602,\"message\":\"path is outside the session cwd\"}`,
		`\"error\":{\"code\":-32601,`,
	} {
		if !strings.Contains(all, want) {
			t.Errorf("agent did not see %s\n%s", want, all)
		}
	}
}

func TestUsageOnlyAtEnd(t *testing.T) {
	sum, _, _, err := drive(t, "endusage", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if sum.FirstUsage != "at_end" || sum.FirstCost != "at_end" || len(sum.Usage) != 1 {
		t.Fatalf("%+v", sum)
	}
}

func TestUsageAfterEnd(t *testing.T) {
	sum, _, _, err := drive(t, "late", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(sum.Usage) != 2 || sum.Usage[0].Source != "prompt_response" || sum.Usage[1].Source != "usage_update" {
		t.Fatalf("%+v", sum.Usage)
	}
	if !strings.Contains(string(sum.Usage[0].Tokens), `"totalTokens":30`) {
		t.Fatalf("tokens = %s", sum.Usage[0].Tokens)
	}
	// Usage on the prompt response counts as "at the end"; the trailing update is "after".
	if sum.FirstUsage != "at_end" || sum.Usage[1].When != "after_end" || sum.FirstCost != "none" {
		t.Fatalf("first_usage=%s usage=%+v first_cost=%s", sum.FirstUsage, sum.Usage, sum.FirstCost)
	}
}

func TestTimeLimitCancels(t *testing.T) {
	start := time.Now()
	sum, _, _, err := drive(t, "hang", 300*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if !sum.TimedOut || sum.StopReason != "cancelled" || sum.Stage != "session/prompt" {
		t.Fatalf("%+v", sum)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("took %s", d)
	}
}

func TestTimeLimitKills(t *testing.T) {
	start := time.Now()
	sum, _, cwd, err := drive(t, "deaf", 300*time.Millisecond)
	pid, _ := strconv.Atoi(readFile(cwd, "pid"))
	if pid == 0 || syscall.Kill(pid, 0) == nil {
		t.Fatalf("agent process %d is still alive", pid)
	}
	if err == nil {
		t.Fatal("want an error when the agent ignores session/cancel")
	}
	if !sum.TimedOut || !sum.Killed || sum.StopReason != "killed" || sum.Stage != "session/new" {
		t.Fatalf("%+v", sum)
	}
	if sum.WallMs < 300 || time.Since(start) > 3*time.Second {
		t.Fatalf("wall_ms=%d total=%s", sum.WallMs, time.Since(start))
	}
}

func TestAuthRequiredIsReported(t *testing.T) {
	sum, _, _, err := drive(t, "noauth", 5*time.Second)
	if err == nil || !strings.Contains(err.Error(), "-32000") {
		t.Fatalf("err = %v", err)
	}
	if sum.Stage != "initialize" || sum.FirstUsage != "none" {
		t.Fatalf("%+v", sum)
	}
}

func TestSpawnFailure(t *testing.T) {
	var out bytes.Buffer
	sum, err := run(config{harness: "x", cmd: "/nonexistent/acp-agent", cwd: t.TempDir(), prompt: "hi",
		limit: time.Second, grace: time.Second}, &out, &out)
	if err == nil || sum.Stage != "none" {
		t.Fatalf("stage=%s err=%v", sum.Stage, err)
	}
}

func TestHarnessTable(t *testing.T) {
	for _, name := range []string{"claude", "codex", "agy", "fake"} {
		if argv, err := resolveCmd(config{harness: name}); err != nil || len(argv) == 0 {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := resolveCmd(config{harness: "nope"}); err == nil {
		t.Error("unknown harness accepted")
	}
	if argv, _ := resolveCmd(config{harness: "claude", cmd: "foo bar"}); strings.Join(argv, " ") != "foo bar" {
		t.Errorf("--cmd did not override: %v", argv)
	}
}
