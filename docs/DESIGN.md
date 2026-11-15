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
- **Adapters are installed on first use** into `os.UserCacheDir()` joined with
  `refraktal/adapters/<package>-<version>/`, with `npm install
  --ignore-scripts`. A failed install leaves nothing behind.
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
| Claude | `default` | A partial gate: asked before `touch` and `git push`, but ran `echo` and `sleep` unasked |
| Codex | `workspace-write` | Writes freely in the session folder, its `TMPDIR` and `/tmp`; asks outside them |
| Antigravity | `default` | Asks before writes and shell commands; reads run unasked |

Policy (`internal/policy`); every request is logged with its decision:

- Reads, searches, fetches and thinking: allowed.
- File edits, deletes and moves: allowed only when every path, cleaned and
  with symlinks resolved, is inside the child's worktree.
- Shell commands: allowed unless the deny-list matches anywhere in the
  command after quotes are removed (pushing, publishing, releasing,
  `git config`, `git -c`, `--config-env`, `GIT_*`, `HOME` or
  `XDG_CONFIG_HOME` assignments, an unknown git global option), or a word is
  a path outside the worktree (absolute, `~` or `..`, existing or not; only
  root-owned system folders and `/dev/null`-like devices are exempt), or a
  variable is expanded inside a path or a `cd`, `pushd` or `ln` target, or a
  bare `cd` leaves the worktree.
- Any other tool kind: allowed only if it names paths and all are inside
  the worktree; otherwise denied.

This is a gate, not a sandbox: Refraktal sees a command's text, not
everything it touches. A command built at run time (an interpreter's code,
`$(...)`, a script file the child wrote) is not seen through, and a harness
decides by its own rules which actions to ask about at all.

- **The reject path works on all three harnesses** (M3 open test: deny
  `touch marker.txt` in each gated mode). The file is never created and the
  tool call is reported `failed`. Claude ends the turn normally and tells the
  user the command was denied; Antigravity ends it normally with no reply;
  **Codex ends the whole turn with stop reason `cancelled`**. So in `run`, a
  denial ends a Codex child's turn: the run is `failed` (exit 1) and its
  worktree is kept.
- **Claude in `default` asked before `git push`** (M3 open test), so the
  deny-list reached it: denied, nothing sent, and Claude told the user.
  Whether it asks before every deny-list command is not known.
- **Codex temp folders.** Each Codex child gets a private `TMPDIR` inside its
  run folder. Codex takes its writable temp root from it: writes there run
  unasked, and the user's own `TMPDIR` is no longer writable (blocked by
  Codex's sandbox) (M3 open test).
- **Open (contradicts this design): `/tmp` cannot be excluded per run.**
  codex-acp 2.6.0 sends its own `workspace-write` sandbox policy with
  `excludeSlashTmp: false` on every turn, which overrides
  `sandbox_workspace_write.exclude_slash_tmp` in `CODEX_CONFIG`. A Codex
  child can write to `/tmp` without asking. Options, not yet decided: accept
  it; run Codex in `read-only` (every write is asked, and a denial ends the
  turn); a Codex permission profile defined per run (untested); or a change
  upstream in codex-acp.
- **Never put worktrees or run folders under `$TMPDIR` or `/tmp`.**
- No harness uses the client's `fs/*` methods: do not implement a client-side
  file policy.

## MCP and user configuration

Isolate children from the user's own MCP servers and plugins by default, with
an explicit per-harness `inherit` option in the config. With `inherit`,
`doctor` and `run` print a notice.

| Harness | Per-run isolation |
|---|---|
| Claude | `session/new` `_meta`: `{"claudeCode":{"options":{"strictMcpConfig":true}}}` (keeps the login; also stops context growth between turns) |
| Antigravity | `agy acp --gemini-dir <folder Refraktal manages>` (hides all of `~/.gemini`) |
| Codex | `CODEX_CONFIG` (codex-acp merges it into each session's config): `features.plugins = false`, and `mcp_servers.<name>.enabled = false` for every enabled server that Codex itself lists for the session folder (`codex -c features.plugins=false mcp list --json`, run with the adapter's bundled Codex). Keeps the login (M3 open test: a real turn completed, with no MCP or plugin process in the tree). An empty `mcp_servers` table does not work: the override is merged |

Never edit, move or copy the user's global harness configuration or
credentials. An empty `mcpServers` list in `session/new` does not isolate.
Refraktal never reads the harnesses' config files either: Codex's server list
comes from Codex.

**Project settings are inherited.** Children run in a worktree of the
repository and load its harness project settings, so `CLAUDE.md` and
`AGENTS.md` apply (M3 end-to-end runs: all three harnesses followed a project
instruction). Consequences:

- A repository's harness hooks run inside the child, with its permissions
  and outside Refraktal's policy. `run` prints a one-line notice when the
  worktree configures hooks for the harness: `.claude/settings.json` or
  `.claude/settings.local.json` (a `hooks` key), `.codex/hooks.json` or a
  `hooks` table in `.codex/config.toml`, and for Antigravity, whose project
  hook location is not documented, `.gemini/settings.json`,
  `.gemini/config/hooks.json` and `.agents/hooks.json`.
- Project settings can pre-approve commands (for example Claude permission
  allow rules); a pre-approved command never reaches the policy.
- `doctor` is the exception: it runs in an empty folder outside the
  repository, so it never runs a repository's code.

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
- **Live registry.** Every live child is registered in a global folder,
  `os.UserConfigDir()` joined with `refraktal/live/`, one file per child. The
  file names the owning Refraktal process (PID, start time, command line), the
  child and every descendant tracked so far, and is rewritten as tracking
  grows. It is removed on a clean shutdown. `doctor` and `run` clean up the
  entries of dead runs (owner no longer running as recorded): they kill the
  listed processes with the identity check, then remove the file. This also
  covers Refraktal itself being killed. The folder is global because runs in
  different repositories share one machine.
- Keep a process-group kill as well; it is free.
- The kill path is proven against real processes in tests (a child that
  leaves the harness's group, a reparented background child, a reused PID
  left alone). Live, it has not been needed so far: every harness tool ended
  on its own first.
- Tracking also widens through a process group, but only one whose leader
  (PID == PGID) is tracked. This keeps a tool's backgrounded child after its
  shell exits, without pulling in unrelated members of a shared group.
- **Open (confirmed live, contradicts "no surviving process"): the detach
  gap.** A process that detaches and is reparented between two samples, with
  its group leader never seen, escapes tracking. M3's `ps`-free survival test
  (`(sleep 30; touch late.txt) &`, then shutdown, then wait 40 s) hit it on
  Claude: `late.txt` appeared after shutdown, the `sleep` was never in the
  tracked set (11 processes tracked, none of them it), and Claude itself
  exited cleanly. Codex and Antigravity left nothing (their tool runners end
  the command's background children). So the tree kill does not guarantee
  that nothing survives, which the `--mins` design (M4) relies on. Not yet
  decided how to close it.
- Scrub the child's environment of the parent's session markers
  (`CLAUDECODE`, `CLAUDE_CODE_ENTRYPOINT`, `CLAUDE_CODE_SSE_PORT`).

## Run folder and logging

- One folder per run; child runs nest inside it. JSONL for events, decisions,
  verdicts and costs.
- **Run folders live in the repository** at `.refraktal/runs/<id>/` (the git
  top level, or the current folder outside a repository). Refraktal writes
  `.refraktal/.gitignore` containing `*`, so nothing there is committed. The
  ID is the start time plus a random suffix.
- A repository may not be trusted: if `.refraktal`, `.refraktal/runs` or
  `.refraktal/.gitignore` is a symlink, Refraktal refuses to continue rather
  than write through it. `.gitignore` is replaced by renaming a new file over
  it.
- **Redact.** Harnesses send the account email, organisation and plan in auth
  notifications. Log an allowlist of known update kinds verbatim; for anything
  else, the method name and size only. Never forward a child's auth
  notifications upstream. Process listings contain local paths: same rule.
- Store large repeated updates (for example `available_commands_update`, tens
  of KB, sent several times a turn) as a size and a hash.

## Configuration

- The config file is JSON, read with the standard library, at
  `os.UserConfigDir()` joined with `refraktal/config.json`. A missing file
  means defaults; unknown fields and bad values are errors. Only the user's
  own config is read: a repository cannot change how its children are
  isolated.
- Per harness (`claude`, `codex`, `agy`): `mcp` (`isolated`, the default, or
  `inherit`) and `executable` (the CLI an adapter runs instead of its bundled
  one, through `CLAUDE_CODE_EXECUTABLE` or `CODEX_PATH`, or the `agy`
  binary).

## Run (one harness, M3)

- `refraktal run --harness <name> [--no-apply] [--json] [task]`. Flags may
  come before or after the task. The task comes from the argument, stdin, or
  both; stdin is read only when it is a pipe or a regular file, never a
  terminal or a socket (an open socket that never closes would hang).
- **Worktree** at `os.UserCacheDir()` joined with
  `refraktal/worktrees/<run id>`, on branch `refraktal/<run id>`, from the
  repository's `HEAD`: uncommitted changes in the user's tree are not part of
  it. Outside the repository so harnesses walking up from the session folder
  do not find the user's checkout. Never under a temp folder.
- The child shares the repository's `.git` (a linked worktree). Refraktal's
  own git commands therefore run with hooks, fsmonitor, external diff,
  textconv, pager and ssh command turned off, and the policy denies
  `git config`.
- **Result.** After the turn, everything the child changed is committed on
  the branch and applied to the working tree with `git apply`, checked first
  so a diff that does not apply cleanly changes nothing. The diff is kept in
  the run folder as `result.diff`. With `--no-apply` (or when the apply
  fails, or the turn did not end normally) the worktree and branch stay and
  are printed; otherwise they are removed.
- **Time limit** 30 minutes until `--mins` arrives (M4): `session/cancel`,
  then shutdown and the tree kill.
- **Exit codes:** 0 applied, kept or no changes; 1 the turn failed or the
  result did not apply; 2 the time limit ran out; 3 the harness could not be
  started or logged in.

## Doctor

- The login check is a zero-token handshake: `initialize`, `session/new` with
  the run's isolation options, and `session/set_mode` to the harness's mode,
  then shutdown. No prompt is ever sent.
- Each handshake runs in an empty folder outside any repository
  (`os.UserCacheDir()` joined with `refraktal/doctor/`, removed afterwards).
  Harnesses load project settings from the session folder, and Claude runs a
  project's hooks at session start: `doctor` must never run a repository's
  code. (`run` works in the repository by design, so this applies to `doctor`
  only.)
- A harness is `ok`, `logged_out` (`session/new` refused for
  authentication), `unavailable` (not installed, or its adapter could not be
  prepared) or `error`. Exit code 3 when no harness is `ok`.

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
