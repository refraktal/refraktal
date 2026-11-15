# Refraktal v1 design

The approved design for v1, in one place. Evidence for each decision is in
`spikes/` (four rounds of live tests, summarised in `spikes/*RESULTS.md`).
Items marked **Open** are decided in principle but wait on a test, which the
build plan attaches to a milestone.

## What it is

A meta-harness: one task in, one or more coding-agent harnesses run as child
processes, their results judged, the winner applied to the working tree.
Refraktal speaks the Agent Client Protocol (ACP) to every harness, and speaks
ACP itself, so a Refraktal can run under another Refraktal.

Principles: a tiny core loop (plan, dispatch, collect, judge); a good Unix
citizen (stdin, stdout, JSONL); files, not infrastructure (no server, no
database); library and CLI are the same API; Go standard library first.

## CLI surface (fixed: do not add to it without asking)

```
usage: refraktal <command> [flags]        task comes from the arg, stdin, or both
  run [task]      plan, dispatch, collect, judge; apply the winner to the tree
  acp             speak ACP on stdio; any client, including a Refraktal, can drive it
  show [run]      print a run: routing decisions, verdicts, cost (default: latest)
  doctor          check harnesses, auth and the Decider; exit nonzero if unusable
  --harness a,b --fan N        run: override the router (pin harnesses, force width)
  --no-apply                   run: leave candidates in worktrees, print branches
  --dry                        run: plan and route only, spawn nothing
  --usd N --turns N --mins N --depth N   run, acp: budgets; children get less
  --json                       run, show, doctor: JSONL on stdout
```

Exit codes: 0 winner applied (or kept with `--no-apply`), 1 no candidate passed
the judge, 2 budget exhausted, 3 nothing usable. `judge` is an internal step,
not a command.

## Harnesses (v1)

| Harness | Launch | Notes |
|---|---|---|
| Claude Code | `@agentclientprotocol/claude-agent-acp`, pinned version | Adapter runs its own bundled `claude` |
| Codex | `@agentclientprotocol/codex-acp`, pinned version | Adapter runs its own bundled `codex app-server` |
| Antigravity | `agy acp` | Native ACP; uses the installed `agy` |
| Refraktal | `refraktal acp` | Recursion |

- **Pin adapter versions.** Keep the adapter's bundled harness binary by
  default; `doctor` reports both bundled and installed versions; config may
  point an adapter at the installed CLI (`CLAUDE_CODE_EXECUTABLE`,
  `CODEX_PATH`).
- **The ACP layer is hand-rolled** JSON-RPC over stdio on the standard library.
  Do not use an ACP SDK. Pin protocol version 1.

## Budgets

Four budgets: `--usd`, `--turns`, `--mins`, `--depth`. Each is passed down the
tree and only ever shrinks. A child is given at most what its parent has left.

- **`--mins`** is wall-clock time and the only hard stop inside a turn. When it
  runs out: send `session/cancel`, wait a short grace, shut the child down,
  then kill its whole process tree (see Processes). Cancel alone is not
  enough: it ends the turn, but Codex's running tool keeps going, and
  Antigravity's once outlived the harness. `--mins` always has a default and is
  never unlimited.
- **`--turns`** counts prompts Refraktal sends to harnesses, across the tree.
  Not an agent's internal iterations.
- **`--usd`** is checked before each turn starts, never inside one: no harness
  reports cost mid-turn.
  - Claude reports a cumulative session cost: a turn's cost is the new figure
    minus the previous one.
  - Codex and Antigravity report tokens only, per turn, on the prompt
    response's `usage`. Refraktal prices them from LiteLLM's
    `model_prices_and_context_window.json`, cached on disk, with per-model
    overrides in config.
  - Bill from the prompt response's `usage`, never from the mid-turn `used`
    field, which is context fill and under-counts.
- **Cost sources.** Every figure is tagged `reported`, `computed`, `unpriced`
  or `estimated`. Reported wins. An unpriced model continues with a loud warning
  (stderr, run log, `--json`) and counts as $0 against `--usd`. Dollars are a
  budget unit, not a bill: on subscription logins they are notional.
- **Cancelled turns.** Usage for model calls that finished before the cancel is
  reported as normal; charge it. A call the cancel cut off is never reported.
  For enforcement, a cancelled child keeps its whole reservation. For
  reporting, record an `estimated` amount: the last `used` figure priced as
  input tokens.
- **Reservation accounting.** A live child holds its whole grant; a finished
  child returns what it did not spend.

## Permissions

Refraktal sets each child's mode explicitly and answers the permission
requests that reach it by policy. Never use `auto`, `acceptEdits`, `agent`,
`bypassPermissions` or `agent-full-access` for children.

| Harness | Mode | Behaviour observed |
|---|---|---|
| Claude | `default` | Sends shell commands to the client |
| Codex | `workspace-write` | Writes freely in the session folder, `$TMPDIR` and `/tmp`; asks outside them |
| Antigravity | `default` | Asks before writes and shell commands; reads run unasked |

Policy: allow file edits inside the child's worktree; allow shell commands
except a short deny-list (for example `git push`, publishing commands, paths
outside the worktree); log every request with its decision. This is a gate,
not a sandbox: Refraktal sees a command's text, not everything it touches.

- **Codex temp folders.** Give each Codex child a private `TMPDIR` inside its
  run folder and exclude `/tmp` (`exclude_slash_tmp`). **Open:** whether Codex
  takes its writable roots from the child's `TMPDIR`, and how to set the
  exclusion per run without touching global config.
- **Never put worktrees or run folders under `$TMPDIR` or `/tmp`.**
- **Open:** the reject path (`reject_once`) has not been exercised on any
  harness. The deny-list depends on it.
- No harness uses the client's `fs/*` methods: do not implement a client-side
  file policy.

## MCP and user configuration

Isolate children from the user's own MCP servers and plugins by default, with
an explicit per-harness `inherit` option.

| Harness | Per-run isolation |
|---|---|
| Claude | `session/new` `_meta`: `{"claudeCode":{"options":{"strictMcpConfig":true}}}` (keeps the login; also stops context growth between turns) |
| Antigravity | `agy acp --gemini-dir <folder Refraktal manages>` (hides all of `~/.gemini`) |
| Codex | **Open:** no per-run mechanism yet; it loads the most. Until one exists, Codex children inherit the user's setup and `doctor` and the run log say so |

Never edit, move or copy the user's global harness configuration or
credentials. An empty `mcpServers` list in `session/new` does not isolate.

## Processes

Tools run in process groups of their own, outside the harness's group, so a
process-group kill does not reach them.

- While a child runs, record its descendants by parent PID, whatever their
  process group. Walk the tree once more just before shutdown, while lineage is
  still visible, then kill every tracked descendant and the harness.
- **Kill only by tracked PID, after an identity check:** command line and start
  time must still match what was recorded. The tree includes the user's MCP
  servers, plugin hooks and helpers from other apps' bundles; never kill by
  name or pattern.
- Record tracked processes in the run folder; `run` and `doctor` clean up
  leftovers from dead runs, which also covers Refraktal itself being killed.
- Keep a process-group kill as well; it is free.
- **Open:** the kill path has been proven only against a fake agent; live tools
  had always ended first. Known gap: a process that detaches and is reparented
  between two samples escapes.
- Scrub the child's environment of the parent's session markers
  (`CLAUDECODE`, `CLAUDE_CODE_ENTRYPOINT`, `CLAUDE_CODE_SSE_PORT`).

## Run folder and logging

- One folder per run; child runs nest inside it. JSONL for events, decisions,
  verdicts and costs.
- **Redact.** Harnesses send the account email, organisation and plan in auth
  notifications. Log an allowlist of known update kinds verbatim; for anything
  else, the method name and size only. Never forward a child's auth
  notifications upstream. Process listings contain local paths: same rule.
- Store large repeated updates (for example `available_commands_update`, tens
  of KB, sent several times a turn) as a size and a hash.

## Routing

A System 1 decision model chooses; harnesses are System 2.

- `Decider` interface. Default: Gemini Decisions API (`POST
  https://generativelanguage.googleapis.com/v1beta/decisions`, header
  `x-goog-api-key`, model `gemini-decision-1`). Deterministic fallback when
  there is no key or the call fails.
- One request, five questions: task kind, best harness, fan out (yes/no),
  fan-out width, needs decomposition. `confidence` is just the top probability:
  threshold one number per answer; gate yes/no answers on P(yes).
- Latency is about 0.5 s per decision plus about 280 ms cold. Timeout 2 s.
  Start the call alongside other setup. Answers are deterministic for identical
  requests: cache by a hash of the exact request, with expiry.
- Log every decision with its probabilities, the thresholds and the outcome:
  this is the tuning dataset.
- Starting thresholds are in `spikes/routing`; untuned. The fan-out questions
  are the weak part: do not retune on a handful of samples.
- On the free tier, Google uses request data to improve its products. Say so
  in the docs.

## Not in v1

A server or web console, a database, a model gateway, containers (later, as an
opt-in isolation tier), and a learner (the decision log is kept for it).
