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
- **Prices:** config overrides win over the table under every key tried;
  table entries with negative or non-finite prices are skipped; a cache rate
  of exactly 0 is a real price, and only a missing one falls back to the
  input rate.
- **The ACP layer is hand-rolled** JSON-RPC over stdio on the standard library.
  Do not use an ACP SDK. Pin protocol version 1.

## Budgets

Four budgets: `--usd`, `--turns`, `--mins`, `--depth`. Each is passed down the
tree and only ever shrinks. A child is given at most what its parent has left.
Defaults: `--usd 10`, `--turns 20`, `--mins 30`, `--depth 2`. `--usd` and
`--mins` must be finite, and `--mins` is at most 1440 (24 hours). A child
Refraktal will read its grant from `REFRAKTAL_BUDGET` (JSON: usd, turns,
deadline, depth); `--depth` is how many more levels may nest, and a child gets
one less. When a budget is spent before a turn can start, `run` sends nothing
and exits 2 (`budget_exhausted`); when `--mins` runs out during a turn, it
cancels and exits 2 (`timed_out`).

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
    `model_prices_and_context_window.json`, cached at
    `os.UserCacheDir()/refraktal/prices.json` and fetched again after 24
    hours (a failed fetch falls back to the stale copy, then to "everything
    unpriced"), with per-model overrides in config (US dollars per million
    tokens). The model comes from the session's `model` config option. Keys
    tried: Codex `<model>`, Antigravity `gemini/<model>`.
  - Token accounting: `inputTokens` excludes cached tokens; cached reads and
    writes are priced at their own rates (input rate if the table has none);
    thought tokens are part of `outputTokens`.
  - Bill from the prompt response's `usage`, never from the mid-turn `used`
    field, which is context fill and under-counts.
- **Cost sources.** Every figure is tagged `reported`, `computed`, `unpriced`
  or `estimated`. Reported wins. An unpriced model continues with a loud warning
  (stderr, run log, `--json`) and counts as $0 against `--usd`. Dollars are a
  budget unit, not a bill: on subscription logins they are notional.
- **Cancelled turns.** A turn is cancelled if Refraktal sent `session/cancel`
  or the harness's stop reason is `cancelled` (Codex ends a turn so after a
  denial). Usage for model calls that finished before the cancel is
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
- Any other tool kind: allowed only when its input is absent, or holds
  nothing but known file-path fields (string values) that are all inside the
  worktree. Any other field, a nested value or a non-object input is denied.

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
- **Decided: Codex children may write to `/tmp` (accepted for v1).**
  codex-acp 2.6.0 sends its own `workspace-write` sandbox policy with
  `excludeSlashTmp: false` on every turn, which overrides
  `sandbox_workspace_write.exclude_slash_tmp` in `CODEX_CONFIG`, so `/tmp`
  cannot be excluded per run (M3 open test). This is accepted because
  worktrees and run folders never live under `/tmp`: a Codex child cannot
  reach the repository or a sibling child through it. Asked upstream to
  honour the setting:
  [agentclientprotocol/codex-acp#766](https://github.com/agentclientprotocol/codex-acp/issues/766).
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
- **Kill only by tracked PID, after an identity check:** a process's identity
  is its birth identity, PID and start time, everywhere (tracker, live
  registry, kills, sweeps); the command line is kept for display only, since
  it changes when a process execs. (A PID reused within the same second of
  `ps`'s one-second resolution would look the same; that is the accepted
  limit.) The tree includes the user's MCP
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
- **Session sweep (closes the detach gap).** A process that detaches and is
  reparented between two samples, with its group leader never seen, escapes
  parent-PID tracking: M3's survival test hit it on Claude. So each harness
  starts as the leader of a new session, and at shutdown, after the tree kill
  and the process-group kill, Refraktal kills, with the identity check, every
  process of the current user still in that session, whatever its parent or
  process group. A session ID is inherited through forks, reparenting and new
  process groups, and survives the leader's exit.
  - **A session ID is a PID, and can be reused once the session empties.**
    So: the harness is not reaped until its session has been swept (Refraktal
    learns of the exit with `waitid(WNOWAIT)`, and the zombie keeps the ID
    reserved); every session is polled about once a second and dropped for
    good once it is seen empty (confirmed by a second look), and only
    sessions never seen empty are swept; a leader is matched on PID and start
    time, and if its PID now has another start time the session is not
    swept; a member that started before the leader is skipped. Residual risk:
    a session that empties and has its ID reused within one poll interval.
  - **Dead runs are never swept by session ID.** `run` and `doctor` clean up
    a dead run by killing only the processes its live record names by
    identity; the session poll keeps the members it sees in that record.
  - **Known limit:** a process that calls `setsid()` itself, as
    pseudo-terminal launchers do, leaves the session and escapes.
  - **Claude's tool commands: recorded sessions.** Claude's Bash tool starts
    each command's shell as the leader of a new session (seen live: the tool
    shell's session ID equals its own PID), so a command's background
    children are outside the harness's session. Claude calls
    `CLAUDE_CODE_SHELL_PREFIX`, if set, with the whole command string as one
    argument, from inside that new session. Refraktal sets it per run, in the
    environment only (never a settings file), to its own binary in a helper
    mode selected by `REFRAKTAL_HELPER=shell-prefix` (not a command or a
    flag). The helper appends the session's leader (PID, start time, command
    line) to the run's sessions file, removes its own variables from the
    environment, restores the harness's prefix variable for the command (the
    user's own value, or none, so a nested claude or refraktal is not
    routed through the helper), and execs the command with bash or zsh, as
    Claude Code does (or chains to a prefix the user had set, never to the
    helper itself). Recorded sessions are polled and swept like the
    harness session. The helper records only the leader's PID and start
    time, then checks a shutdown marker, then runs the command; if it cannot
    record, or the marker exists, it says why on stderr and exits 126
    without running the command. Before its final read of the sessions file
    Refraktal creates the marker, so every command that ran was recorded in
    time. The file is read with no line limit, skipping bad lines.
  - **Rejected review finding: forged session records.** A process that
    can append to the sessions file runs as the same user, so it can already
    kill any of the user's processes; a forged record gains it nothing.
    M4 open test: `late.txt` left nothing on Claude (2 escaped processes
    swept). The prefix is opt-in: `RunOptions.ShellPrefix` names a binary
    that calls `proc.MaybeShellPrefix()` first in `main`, and the CLI passes
    its own path. It is never guessed from `os.Executable()`: in a test or a
    library host that is another program, which Claude would then run for
    every tool command (this happened once in M4 review, in a test, and
    started a cascade of nested runs). Limit: a Claude settings file that
    sets `CLAUDE_CODE_SHELL_PREFIX` itself would replace Refraktal's.
  - Hooks also run through the prefix, so their sessions are swept too. A
    side finding: children load the user's Claude settings (the adapter asks
    for user, project and local setting sources), so the user's own Claude
    hooks run inside children. `strictMcpConfig` does not stop hooks.
  - Why not the environment: the first plan tagged children with
    `REFRAKTAL_RUN` and swept by environment. On macOS 27 a process can read
    only its own environment (M4 open test: another process of the same user
    shows none, through `KERN_PROCARGS2` or `ps -E`), so it cannot work there.
- Every child carries `REFRAKTAL_RUN=<run id>-<harness>` in its environment,
  as a tag for logs only.
- **No real harness in `go test`.** Inside a test binary, the launcher
  refuses any executable other than the test binary itself (which is how the
  fake agent and test helpers run) unless `REFRAKTAL_LIVE=1`, which only the
  live scripts set. In a test, the shell prefix may only be the test binary,
  whose `TestMain` calls `proc.MaybeShellPrefix` first.
- **Nesting guard, independent of budgets.** Every child gets
  `REFRAKTAL_NEST` one higher than its parent; `run` and `doctor` refuse to
  start past `--depth` + 1 levels, so no recursion can fan out without bound
  even if budgets are lost.
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
- **Budgets** as in Budgets above; `--mins` covers the whole run, from
  start-up to the end of the turn: during the turn, `session/cancel`, then
  shutdown, the tree kill and the session sweep (`timed_out`); during any
  setup step (price fetch, worktree, starting the harness),
  `budget_exhausted`. Both exit 2. Writes to a harness are bounded (10 s, the
  cancel 2 s), so one that stopped reading cannot hold up the stop.
- **Exit codes:** 0 applied, kept or no changes; 1 the turn failed or the
  result did not apply; 2 a budget ran out (before the turn, or `--mins`
  during it); 3 the harness could not be started or logged in.

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
