// acp-spike: a minimal ACP (Agent Client Protocol) client. It spawns a coding
// harness, runs initialize -> session/new -> session/prompt, prints every
// session/update as a JSON line, and ends with one summary line that says what
// usage and cost data the harness reported, and when.
//
//	main.go    harness table and flags
//	client.go  the core: JSON-RPC over stdio, the turn, usage capture, time limit
//	policy.go  how agent-to-client requests are answered with no human present
//
// The JSON-RPC layer is hand-rolled on the standard library; see FINDINGS.md.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// harnesses maps a name to the command line that starts its ACP server on stdio.
// Adapter versions are pinned: npx would otherwise run whatever is newest.
var harnesses = map[string]string{
	"claude": "npx -y @agentclientprotocol/claude-agent-acp@0.107.0",
	"codex":  "npx -y @agentclientprotocol/codex-acp@2.6.0",
	"agy":    "agy acp",
	"fake":   "fakeagent", // built from ./cmd/fakeagent; looked up beside this binary, then on PATH
}

func resolveCmd(cfg config) ([]string, error) {
	line := cfg.cmd
	if line == "" {
		line = harnesses[cfg.harness]
	}
	argv := strings.Fields(line)
	if len(argv) == 0 {
		return nil, fmt.Errorf("unknown harness %q and no --cmd", cfg.harness)
	}
	if self, err := os.Executable(); err == nil && !strings.Contains(argv[0], "/") {
		if beside := filepath.Join(filepath.Dir(self), argv[0]); isFile(beside) {
			argv[0] = beside
		}
	}
	return argv, nil
}

func isFile(p string) bool { fi, err := os.Stat(p); return err == nil && fi.Mode().IsRegular() }

func main() {
	var cfg config
	var mins float64
	flag.StringVar(&cfg.harness, "harness", "fake", "claude | codex | agy | fake")
	flag.StringVar(&cfg.cmd, "cmd", "", "command line of an ACP server; overrides --harness")
	flag.StringVar(&cfg.cwd, "cwd", ".", "session working directory")
	flag.Float64Var(&mins, "mins", 2, "wall-clock limit for the prompt turn, in minutes")
	flag.DurationVar(&cfg.grace, "grace", 5*time.Second, "wait after session/cancel before killing")
	flag.DurationVar(&cfg.linger, "linger", time.Second, "keep reading after the prompt response")
	flag.BoolVar(&cfg.handshake, "handshake", false, "stop after session/new: send no prompt, spend no tokens")
	flag.StringVar(&cfg.mode, "mode", "", "session mode id to set with session/set_mode before prompting")
	flag.IntVar(&cfg.turns, "turns", 1, "number of times to send the prompt in the same session")
	flag.Func("step", "repeatable: SECONDS:PROMPT, one turn of a scripted session (0 = use --mins); a cancelled step does not end the session",
		func(v string) error {
			secs, prompt, ok := strings.Cut(v, ":")
			f, err := strconv.ParseFloat(secs, 64)
			if !ok || err != nil || f < 0 || strings.TrimSpace(prompt) == "" {
				return fmt.Errorf("want SECONDS:PROMPT, got %q", v)
			}
			cfg.steps = append(cfg.steps, Step{Limit: time.Duration(f * float64(time.Second)), Prompt: prompt})
			return nil
		})
	flag.Func("session-meta", "JSON object sent as _meta on session/new", func(v string) error {
		var o map[string]any
		if err := json.Unmarshal([]byte(v), &o); err != nil {
			return fmt.Errorf("not a JSON object: %w", err)
		}
		cfg.sessionMeta = json.RawMessage(v)
		return nil
	})
	flag.Func("survivor-samples", "comma-separated delays after the harness exits at which to list survivors (default 200ms)", func(v string) error {
		for _, f := range strings.Split(v, ",") {
			d, err := time.ParseDuration(strings.TrimSpace(f))
			if err != nil || d < 0 {
				return fmt.Errorf("bad duration %q", f)
			}
			cfg.survivorSamples = append(cfg.survivorSamples, d)
		}
		return nil
	})
	flag.BoolVar(&cfg.killTree, "kill-tree", false, "track the harness's descendants by parent PID, in any process group, and kill them all at shutdown")
	flag.DurationVar(&cfg.pollEvery, "poll-every", 500*time.Millisecond, "how often to record descendants with --kill-tree")
	flag.Parse()
	cfg.limit = time.Duration(mins * float64(time.Minute))
	for i := range cfg.steps {
		if cfg.steps[i].Limit == 0 {
			cfg.steps[i].Limit = cfg.limit
		}
	}
	cfg.prompt = strings.Join(flag.Args(), " ")
	if cfg.prompt == "" && !cfg.handshake && len(cfg.steps) == 0 {
		b, _ := io.ReadAll(os.Stdin)
		cfg.prompt = strings.TrimSpace(string(b))
	}
	if cfg.prompt == "" && !cfg.handshake && len(cfg.steps) == 0 {
		fmt.Fprintln(os.Stderr, "usage: acp-spike [flags] <prompt>   (or prompt on stdin)")
		os.Exit(2)
	}
	if _, err := run(cfg, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "acp-spike:", err)
		os.Exit(1)
	}
}
