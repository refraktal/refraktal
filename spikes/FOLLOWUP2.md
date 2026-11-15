# Third round of live tests: brief for Claude Code

The follow-up run (`FOLLOWUP-RESULTS.md`) left questions that decide the budget
and permission design. This brief tests them. Write the answers to
`spikes/FOLLOWUP2-RESULTS.md`.

## Rules (same as before, plus one)

- Raw output in `results/` is gitignored on purpose. **This repo is public.**
  Do not commit, paste or upload raw results. Summarise instead.
- Process command lines contain local paths: report binary names and versions
  only, with the home directory as `~`. Report MCP servers by count and by a
  generic description (for example "a chat integration"), not by name or path.
- **New: never edit, move or rename the user's global harness configuration**
  (anything under `~/.claude`, `~/.codex`, `~/.gemini`, `~/.antigravitycli` or
  similar), and never copy credentials. Only per-run environment variables,
  flags or `_meta` options are allowed. If the only way to disable MCP servers
  is a global edit, stop that step and report it.
- Never print an API key. Do not change prompts or time limits. Never invent a
  result: if a step did not run, say so and why. Show Adhish the diff before
  pushing.

## Steps

1. `git pull`, then the offline checks; stop if they fail:
   ```
   cd spikes/acp && go vet ./... && go test ./... && HARNESSES=fake bash followup2.sh all
   ```
2. The main tests, in this order (from a plain terminal if a Claude run fails
   with an authentication or nesting error):
   ```
   cd spikes/acp
   bash followup2.sh midtool        # 3 partial turns, cancelled 10 s in
   bash followup2.sh cancel-spend   # Claude, 6 one-word turns in 2 sessions
   bash followup2.sh modes          # 4 short turns
   bash followup2.sh turns          # 4 one-word turns
   bash followup2.sh exit-late      # 0 tokens, about 35 s
   ```
3. MCP isolation. First a baseline, which costs nothing:
   `bash followup2.sh mcp` (label `default`).
   Then look for a per-run way to stop the user's own MCP servers from
   loading, separately for Claude and Antigravity:
   - **Claude:** read the installed adapter in
     `acp/adapters/node_modules/@agentclientprotocol/claude-agent-acp/` for
     options it accepts in `session/new` `_meta` (for example Agent SDK options
     such as setting sources or MCP configuration) and for environment
     variables it or the bundled Claude Code read.
   - **Antigravity:** `agy acp --help` and its docs, for a flag or variable.
   Try each candidate with a zero-token handshake, for example
   `SESSION_META='{...}' MCP_LABEL=try1 bash followup2.sh mcp` or
   `AGY_ACP_ARGS='--some-flag' MCP_LABEL=try2 bash followup2.sh mcp`.
   A candidate **works** if `session/new` still succeeds (login intact) and
   `procs_live` shows no MCP server processes. Stop after five attempts per
   harness.
4. Context growth, only if a Claude candidate worked: run
   `MCP_LABEL=default bash followup2.sh growth` (control), then the same with
   the working setting and its label.

Each run writes `results/followup2-<test>-<harness>.jsonl`, `.summary.json`,
`.stderr` and `.files`. New summary fields: `procs_at_cancel` (just before the
cancel), `procs_after_cancel` (1 s after the cancel response) and
`survivor_samples` (each with `at_ms` and `procs`). In scripted sessions
(`--step`), a cancelled turn does not end the session, and every `usage` entry
carries its `turn`.

## Questions

### Cancel during a running tool (`midtool-*`), per harness

- **G1.** Was `sleep 20` running when the cancel was sent (`procs_at_cancel`)?
  If not, what was the harness doing (last updates before the cancel)?
- **G2.** Stop reason and time from cancel to response.
- **G3.** Was `sleep` still running 1 s after the cancel response
  (`procs_after_cancel`)? This is whether cancel stops the tool, not just the
  turn.
- **G4.** Was `hello.txt` written anyway (`.files`)?
- **G5.** Any usage or cost for the partial turn? Any permission request, and
  when relative to the cancel?

### Does a cancelled turn's spend show up later? (`spend-*`)

- **G6.** Cumulative cost after each turn in both sessions (control: plain,
  plain, plain; test: plain, cancelled, plain). Turn 2's stop reason in the
  test session must be `cancelled`; say if it was not.
- **G7.** Compare the test session's cost after turn 3 with the control's
  after turn 3 and after turn 2. Does the test's cumulative figure include
  spend for the cancelled turn, or does it look as if that turn never
  happened? Use the token and cache figures on each turn's response to
  support the answer, and say how confident the comparison is.

### Permission modes (`mode-*`)

- **G8.** For Claude in `acceptEdits` and Codex in `workspace-write`: did
  `session/set_mode` succeed? How many permission requests reached the client
  for the shell command and for the file write, and of which tool kinds? Did
  each run produce the expected result (`hi`; `hello.txt` written)?

### Usage across turns (`turns-*`)

- **G9.** For Codex and Antigravity: is turn 2's response `usage` for that
  turn only, or cumulative for the session? Show the figures it rests on.

### Shutdown (`exit-late-claude`)

- **G10.** At each sample (about 0.2, 1, 5 and 30 s), was the bundled `claude`
  still in the process group?

### MCP isolation (`mcp-*`, `growth-*`)

- **G11.** Baseline: how many MCP server processes start for Claude and for
  Antigravity?
- **G12.** Each candidate tried, per harness: what it was, whether the login
  survived, and how many MCP processes started. Which one works, if any?
- **G13.** If step 4 ran: context growth between turn 1 and turn 2 (final
  `used`, cache write tokens, cost) with and without MCP servers. Does
  disabling them remove the growth?

Finish by telling Adhish in a few lines what these answers change in the
project's `SPIKES.md` design list, especially item 1 (budgets and cancelled
turns), item 6 (permission modes and MCP isolation) and item 14 (orphans).
