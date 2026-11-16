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

The release installer refuses a directory or unwritable destination and checks
that the final installed file is executable before reporting success.

## Harnesses (v1)

| Harness | Launch | Notes |
|---|---|---|
| Claude Code | `@agentclientprotocol/claude-agent-acp`, pinned version | Adapter runs its own bundled `claude` |
| Codex | `@agentclientprotocol/codex-acp`, pinned version | Adapter runs its own bundled `codex app-server` |
| Antigravity | `agy acp` | Native ACP; uses the installed `agy` |
| Refraktal | `refraktal acp` | Recursion: `--harness refraktal` (see ACP server and show) |

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
  reports cost mid-turn. So each grant (each running harness: a candidate,
  the judge, a child Refraktal's own harnesses within its grant) can cross
  its share once, by its final turn's cost. The run-wide bound is
  spent + reserved <= `--usd` + the sum of each grant's final-turn overrun:
  a fan-out of N candidates can exceed `--usd` by up to N final turns (plus
  the judge's), never by "one turn" for the whole run.
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
  A reported cost must contain a finite, non-negative numeric amount; a
  malformed usage update leaves the previous accounting unchanged.
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
requests that reach it by policy. A permission request requires an active,
uncancelled turn for its session; without one, the client answers `cancelled`
without consulting the policy. Pending decisions are cancelled when that turn
ends or is cancelled. Never use `auto`, `acceptEdits`, `agent`,
`bypassPermissions` or `agent-full-access` for children.

| Harness | Mode | Behaviour observed |
|---|---|---|
| Claude | `default` | A partial gate: asked before `touch` and `git push`, but ran `echo` and `sleep` unasked |
| Codex | `workspace-write` | Writes freely in the session folder, its `TMPDIR` and `/tmp`; asks outside them |
| Antigravity | `default` | Asks before writes and shell commands; reads run unasked |

Policy (`internal/policy`); every request is logged with its decision:

- Reads, searches, fetches and thinking: allowed.
- File edits, deletes and moves: allowed only when every resolved path is
  inside the child's worktree. Resolve components in order, including dangling
  symlink targets, and apply `..` after preceding symlinks; missing paths are
  allowed for new files, while other lookup errors and symlink loops are denied.
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

The deny-list is frozen (no new cases; parsing shell text can never close
every bypass). Two further known limits, found in the M4 audit, are left as
they are:

- **A symlinked directory inside the worktree.** Command words are checked
  lexically; a path such as `link/file`, where `link` is a directory symlink
  the child created pointing outside, looks inside the worktree but lands
  outside it. (File-edit requests resolve symlinks; shell words are not
  resolved.)
- **Redirection without spaces.** `echo x>/abs/path` is one word to the
  tokeniser, so the path after `>` is not seen as a separate path.

The real boundaries are the worktree (a separate checkout, applied back only
through a checked `git apply` of the committed result) and the harness's own
sandbox and permission mode; the policy is an extra gate in front of them.

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
  grows or a verified descendant's birth identity changes. Record updates
  are serialized per child and atomically replaced through unique temporary
  files. It is removed on a clean shutdown. `doctor` and `run` clean up the
  entries of dead runs (owner no longer running as recorded): they kill the
  listed processes with the identity check, then remove the file. A failed
  lookup is `unknown`, never proof that a process is gone: it is not signalled,
  and its live record stays for the next cleanup attempt. This also
  covers Refraktal itself being killed. The folder is global because runs in
  different repositories share one machine.
- Keep a process-group kill as well; it is free.
- The kill path is proven against real processes in tests (a child that
  leaves the harness's group, a reparented background child, a reused PID
  left alone). Live, it has not been needed so far: every harness tool ended
  on its own first.
- Tracking also widens through a process group, but only one whose leader
  (PID == PGID) was verified as tracked. After the leader exits, an already
  tracked member must still verify the group's continuity; a group seen empty
  or with a changed leader identity is retired permanently. This keeps a
  tool's backgrounded child after its shell exits, without pulling in
  unrelated members of a shared or reused group.
- **Signals and PID reuse.** Every individual kill first acquires a process
  handle with `os.FindProcess`, then re-checks the target's birth identity, then
  signals through `Process.Signal` and releases the handle. On Linux, Go
  1.23+ uses a pidfd: a PID reused after the check cannot receive the signal.
  If pidfds are unavailable (an old kernel, `ENOSYS`, blocked syscalls, or a
  failed pidfd open), Go falls back to signalling by PID. macOS has no pidfd,
  so its final identity-check-to-signal window remains. Process-group signals
  also use a group ID, with the check-to-signal window on both systems.
  The harness, a process-group and session leader, stays unreaped until its
  group and session have been cleaned up, so neither ID can be reused between the check and the signal.
  (On a system without `waitid(WNOWAIT)` the child is reaped at once and
  that guarantee falls back to the identity checks.)
- **Run events and errors.** Durable run events never carry a harness's raw
  error text, which can hold account details: they log a structured code and
  a coarse byte-length bucket (rounded up to a power of two, capped at
  64 KiB); the raw text goes only to the gitignored stderr capture.
- **ACP frames** are at most 16 MiB; a harness that writes more without a
  newline has its connection closed with a clear error. A response that
  arrives together with the agent's exit is still delivered.
- **Session sweep (closes the detach gap where ownership is provable).** A
  process that detaches and is reparented between samples can escape parent-PID
  tracking. Each harness starts as a new session leader. At shutdown, after
  the tree and process-group kills, the sweep signals only provable members:
  identities recorded while the session was known to be ours, or descendants
  linked by PPID chains to a currently verified tracked or recorded process.
  Each chain link must have valid birth times, with the child born no earlier
  than its parent; stale PPID rows cannot link to a newer replacement parent.
  A matching live leader proves ownership throughout a membership sample;
  the harness's own unreaped leader also reserves its session ID. This keeps
  detached children reachable while that reservation holds. Each individual
  target still passes the birth-identity check before signalling.
  - **A session ID can be reused between polls.** Never having seen a session
    empty does not prove current membership belongs to it. The poller records
    members only with the ownership proof above. Sessions seen empty twice
    are retired permanently. A changed leader identity is never swept.
  - **Occupied is not empty.** A member that forks and exits between snapshots
    can leave no stable members while its session remains occupied. The sweep
    retries that state until emptiness is confirmed twice or the sweep deadline
    expires. If it cannot establish emptiness, it reports `unknown` and keeps
    the live record. An inspection failure also remains unknown.
  - **Unknown means kept.** Members without proof are not signalled. They are
    reported as `unknown`, and the live record is kept. Unresolved session IDs
    are stored separately from trusted process identities: an unknown stranger
    never becomes a future kill target. A missed or reparented child whose
    ownership cannot be proved may remain running; safety takes precedence
    over complete cleanup.
  - **Dead runs are never swept by session ID.** `run` and `doctor` kill only
    the identities listed as trusted in the live record. Unresolved session
    IDs are inspected only to establish emptiness, never to select targets;
    their records stay until two empty observations succeed. A reused ID can
    therefore keep an unresolved record indefinitely while occupied.
  - **Known limit:** a process that calls `setsid()` itself, as
    pseudo-terminal launchers do, leaves the session. It is reachable only
    if its identity or a provable PPID chain was recorded.
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
    time. Cleanup requires a complete, readable sessions inventory with valid
    birth identities. An unreadable or malformed inventory is unknown: shutdown
    reports the error and keeps the live record. Diagnostic reads can still
    skip bad lines and recover later valid records without a file-size limit.
    Inventory uncertainty is persisted in the live record; dead-run cleanup
    reports it and keeps the record too. It cannot reconstruct missing records,
    so this uncertainty may require manual resolution and remain indefinitely.
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
  `REFRAKTAL_NEST` one higher than its parent, and every run passes down
  `REFRAKTAL_NEST_CEILING`: the top run's own nest plus its `--depth`, only
  ever lowered. A run past the inherited ceiling refuses to start, whatever
  its remaining `--depth` (so `--depth 2` allows a grandchild, which may run
  harnesses but not another Refraktal). A run with no inherited ceiling (a
  top run, or one started by hand inside another) refuses past `--depth` + 1
  levels. No recursion can fan out without bound even if budgets are lost.
- **No test touches a real harness.** In a test binary, `run` and `doctor`
  refuse to fall back to the real default harnesses (preparing them can
  install adapters and run their CLIs) unless `REFRAKTAL_LIVE=1`.
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
  than write through it. Creation uses pinned `os.Root` directory handles;
  `.gitignore` is checked with at most three bytes, then removed and recreated
  exclusively if needed, preserving hard-linked files and refusing link swaps.
- The run retains a pinned `os.Root` until `run` returns. Event logs, result
  patches, scratch folders and temp folders are created through that handle,
  so replacing the run's pathname cannot redirect Refraktal's writes. The
  pathname remains available for display and for child-process options.
- **Redact.** Harnesses send the account email, organisation and plan in auth
  notifications. Log an allowlist of known update kinds verbatim; for anything
  else, the method name and size only. Never forward a child's auth
  notifications upstream. Process listings contain local paths: same rule.
- Store large repeated updates (for example `available_commands_update`, tens
  of KB, sent several times a turn) as a size and a hash.

## Configuration

- The config file is JSON, read with the standard library, at
  `os.UserConfigDir()` joined with `refraktal/config.json`. A missing file
  means defaults; unknown fields, bad values and any non-whitespace after the
  first JSON value are errors. Only the user's own config is read: a repository
  cannot change how its children are isolated.
- `checks`: per repository path (`~` allowed), `{"build": [argv],
  "test": [argv], "timeout_mins": N}`, the commands judging runs in each
  candidate. `judge`: `{"margin": 0.10}`.
- Per harness (`claude`, `codex`, `agy`): `mcp` (`isolated`, the default, or
  `inherit`), `executable` (the CLI an adapter runs instead of its bundled
  one, through `CLAUDE_CODE_EXECUTABLE` or `CODEX_PATH`, or the `agy`
  binary) and `pass_env` (provider API keys this harness may receive, from
  `OPENAI_API_KEY`, `ANTHROPIC_API_KEY`, `GEMINI_API_KEY`,
  `GOOGLE_API_KEY`; default none).

## Run (one harness, M3)

- `refraktal run --harness <name> [--no-apply] [--json] [task]`. Flags may
  come before or after the task. The task comes from the argument, stdin, or
  both; stdin is read only when it is a pipe or a regular file, never a
  terminal or a socket (an open socket that never closes would hang). `--`
  ends option parsing; every argument after it is literal task text.
  Ctrl-C interrupts stdin collection even while the producer keeps its pipe
  open, and exits 130.
- **Worktree** at `os.UserCacheDir()` joined with
  `refraktal/worktrees/<run id>`, on branch `refraktal/<run id>`, from the
  repository's `HEAD`: uncommitted changes in the user's tree are not part of
  it. Outside the repository so harnesses walking up from the session folder
  do not find the user's checkout. Never under a temp folder.
- The child shares the repository's `.git` (a linked worktree). Refraktal's
  own git commands discard inherited `GIT_*` variables and disable hooks,
  fsmonitor, signing, filters, external diff, textconv, pager and ssh command.
  Creation uses `git worktree add --no-checkout`, then discovers drivers in
  the new worktree before populating it with `git reset --hard --quiet HEAD`.
  If setup fails after registration, bounded cleanup removes the checkout,
  administrative entry and branch; a failed cleanup reports their identity.
  Conditional `onbranch:` and `gitdir:` includes therefore match the new
  branch and path during discovery. Creation records the new worktree's Git
  directory under the repository's common `worktrees/<id>` directory. Every
  Refraktal command in that worktree, including discovery, passes this recorded
  `--git-dir` and an explicit `--work-tree`. Collect fails if the gitfile no
  longer points at the recorded directory; a rewrite during a command also
  fails without redirecting Git to another repository.
  A read-only `git config --null --get-regexp` lists filter, merge-driver and
  `lfs.*` keys and values from all scopes and includes, using the same sanitized
  environment and static protections. Every command has a before/after
  comparison; within one operation and repository context, a successful,
  compared post-command snapshot supplies the next pre-command snapshot.
  Every command still takes a fresh post-command snapshot. If the
  configuration changes during the command, the operation fails.
  Per-command `-c` options set each filter's `clean`, `smudge` and `process`
  to empty strings and `required=false`. Unconfigured filters are harmless
  pass-throughs. Each configured `merge.<name>.driver` is empty: Git runs no
  program and reports a conflict if a three-way merge reaches it. It does not
  select the built-in text merger. A driver key containing `=` is refused
  because `-c key=value` cannot represent it safely. Git LFS is allowed only when its clean, smudge
  and process values are exactly `git-lfs clean -- %f`, `git-lfs smudge -- %f`
  and `git-lfs filter-process`, with exactly one value for each, and no
  `lfs.extension.*`, `lfs.customtransfer.*` or `lfs.standalonetransferagent`
  key set in any scope. In that case all three commands are pinned with
  explicit `-c` options and its required setting is preserved; otherwise the
  whole LFS driver is disabled like any other filter.
  Collect and Apply set `GIT_LFS_SKIP_SMUDGE=1` on every Git command because
  the child may have rewritten `.lfsconfig`; Create keeps normal smudge, and
  Apply returns a notice naming affected `filter=lfs` paths left as pointers
  until the user restores them with
  `git lfs smudge` (the objects are local; `git lfs checkout` does not apply,
  because the patch is not staged).
  Credential helpers remain allowed as the user's own configuration. Isolation
  never edits shared Git configuration or `info/attributes`, and needs no
  isolation lock, backup or restoration.
  Patches use fixed `a/` and `b/` prefixes without color, external diff or
  textconv. Staging uses normal `git add -A`; staging and repository-mode
  apply honor the user's line-ending, encoding and attribute conversions.
  The policy denies `git config`.
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
  cancel 2 s), including waiting for the writer lock, so one that stopped
  reading cannot hold up the stop.
- **Exit codes:** 0 applied, kept or no changes; 1 the turn failed or the
  result did not apply; 2 a budget ran out (before the turn, or `--mins`
  during it); 3 the harness could not be started or logged in.

## Fan-out and judging (M6)

**Fan-out** runs different harnesses, one child each, in parallel, each in a
worktree of its own on branch `refraktal/<run id>-<harness>`.

- Width: the router's choice, capped by `--fan` (`--fan 1` is one harness).
  `--harness a,b` pins the set and skips the router. A route decision is
  width 1; a fan-out decision runs the policy's harnesses in order; an
  escalation runs the most probable harness (planning is not built yet).
- With one candidate there is nothing to choose: it is applied as in M3, with
  no checks or judge.

Git's worktree-list commands (`worktree add`, `worktree remove`, `branch
-D`) are serialised per repository within a run: in parallel, one can read
another's half-written administrative folder and fail.

**Budgets across children.** Each child reserves what is left divided by the
children still to start, so parallel children split `--usd`, and `--turns`
the same way: each holds a turn allowance (at least one) no other child can
use, and a child Refraktal's own prompts draw on it; unused dollars and
turns return at settlement. `--mins` belongs to the whole run: every child,
check and the judge stop at the run's one deadline. The
judge reserves what is left after the candidates settle, and its cost counts.
Claude's reported cost is cumulative for a session: when a session gets more
than one prompt, each turn is charged the difference from the previous report.
Before every prompt the run-wide settled spend is checked too, so a child
holding a reservation stops once another child's overrun has spent the run.
That check bounds new turns, not ones already running: parallel children
each cross their share by at most their final turn, so the overruns add (see
`--usd` above).
Negative or non-finite usage and cost are rejected: a bad reported cost falls
back to pricing the tokens, a negative token count is not priced (noted), and
the ledger counts any such charge as $0.

**Judging** happens when two or more candidates ended their turn normally with
changes. In order:

1. **Deterministic checks.** Each candidate's diff must apply cleanly to the
   working tree (`git apply --check`). Then the repository's check runs in
   the candidate's worktree. The check command comes from the config file
   (`checks`, keyed by the repository's path), else from safe detection: a
   makefile with a `test:` target (the first of `GNUmakefile`, `makefile`,
   `Makefile`, as make would read, run explicitly with `make -f <file>
   test`), a `go.mod` (`go build ./...`, then `go test ./...`), or a
   `package.json` with a real `test` script (`npm test`). Detection reads only the base commit (the user's
   `HEAD`), never a candidate's files, and the same command runs in every
   candidate. When more than one kind is detected, or none, there is no check
   (a notice says so).
   - A candidate whose diff touches the check's own definition, whatever the
     check command is (a `make test` can run `npm test`, `pytest` or `cargo
     test`), in any folder (matched by base name: `make -C sub`, a
     monorepo's `web/package.json`): any of the three makefiles; a
     test-definition file
     (`package.json`, `pyproject.toml`, `setup.cfg`, `setup.py`, `tox.ini`,
     `pytest.ini`, `noxfile.py`, `conftest.py`, `Cargo.toml`, `Rakefile`,
     `Gemfile`, `build.gradle`, `build.gradle.kts`, `pom.xml`,
     `CMakeLists.txt`, `justfile`, `Taskfile.yml`); the `go` or `toolchain`
     line of `go.mod`; the config file when it is inside the repository; or
     CI files under `.github/` or the tests it runs (`*_test.go`, `test_*.py`,
     `*_test.py`, `*.test.*`, `*.spec.*`, or anything under `test/`,
     `tests/` or `__tests__/`) is **flagged** in the verdict and cannot win
     on checks alone (with no check, nothing is flagged): its checks still run and a failure still rules it out, but a
     pass counts as no check, and it can win only through the judge.
   - Checks run untrusted candidate code, so they run like a harness child:
     in the candidate's worktree, in a session of their own under the process
     tracker and session sweep, inside the run's `--mins` deadline with a
     per-check limit (`timeout_mins`, default 10); a check never starts once
     the deadline has passed. Their environment is an allow-list and nothing
     else: `PATH`, `HOME`, `USER`, `LANG`, `LC_*`, `TERM`, the Go, npm and
     cargo cache variables (`GOPATH`, `GOROOT`, `GOCACHE`, `GOMODCACHE`,
     `GOFLAGS`, `npm_config_cache`, `CARGO_HOME`, `RUSTUP_HOME`), and
     `TMPDIR` in the candidate's run folder. Their output goes only to the run folder
     (`<harness>/check-<name>.log`). A check's cleanup is reported as a
     candidate's is: each check result carries `left_running` and `unknown`
     (processes still running, or whose exit could not be confirmed, after
     its shutdown) and `cleanup_failed`, in the result and the `checked`
     event, and `show` prints them under the candidate. Any of them makes a
     notice and keeps the live record for a retry; the raw cleanup error
     appears only in the raw notice, never in the run log or a safe notice.
     Known limit: this is process control,
     not a sandbox. A check can write outside its worktree or use the
     network, exactly as the candidate harness could during its turn (the
     permission policy is a gate, not a sandbox); containers are not in v1.
   - Candidates that pass rank above candidates with no check (or flagged);
     a candidate that does not apply or fails a check cannot win.
2. **System 1 pre-score**, only when the router ran with a key (never with
   `--harness`, which keeps task text local): one Decisions call
   per remaining candidate, asking whether the diff correctly and completely
   carries out the task (P(yes)). It sends the task and the diff, truncated
   to 32 KiB with a note that it was truncated. Without a key there is no
   pre-score: the checks alone decide, and a tie between candidates in the
   top rank is a close call.
3. **Harness judge**, only for a close call: the top two pre-scores are
   within the margin (`judge.margin` in the config, default 0.10), or there
   is no pre-score and more than one candidate shares the top rank, or the
   leader is flagged. The judge is a harness outside the close call when one
   is available and usable: this run's harnesses that could be started,
   then the other configured harnesses; one that cannot be started (not
   installed, logged out) is skipped. Only when none is left does a
   close-call candidate judge. The verdict records the judge, whether it
   judged its own candidate (`judged_own`, also a notice) and any judges
   that could not be started, in `--json`, the run log and `show`. It runs in a fresh worktree of its own with its own grant,
   read-only twice over: in a judge mode per harness, and under the
   read-only policy, which denies every edit, command and mode switch
   (`switch_mode`: Claude otherwise asks to leave its mode). The modes,
   from a live probe that ordered each harness in its judge setup to write
   a file and run `touch`: Codex `read-only` (codex-acp sends a read-only
   sandbox; Codex asked to run the command and was denied); Antigravity
   `plan` (it took no action; its plan file goes to its own state folder in
   the run's scratch); Claude `default` with `disallowedTools` (`Bash`,
   `BashOutput`, `KillShell`, `Edit`, `MultiEdit`, `Write`, `NotebookEdit`,
   `Task`, `Agent`, `SlashCommand`) and `strictMcpConfig` (no MCP tools,
   even with `mcp: inherit`) in the session's SDK options, so no allow rule in the user's or the repository's settings can
   run a writing tool unasked (live, Claude then attempted no tool at all;
   without them it asked for the edit and the command, both denied).
   Claude's `plan` mode is not used: it writes a plan file into
   `~/.claude/plans` without asking, and asks to leave plan mode before
   acting. A harness whose session does not offer its judge
   mode cannot judge. After the turn the judge's worktree must be
   unchanged (modified, untracked and ignored files alike), with `HEAD` still
   equal to its recorded base commit; if not, its answer is not used, the
   changed paths are recorded, and the checks ranking decides. Its prompt holds
   the task and the candidates' diffs
   under anonymous letters. Only the final line of its reply counts, and
   only exactly `WINNER: <letter>` or `WINNER: none`. Its worktree is never
   collected. A judge turn that did not end normally (cancelled,
   `max_tokens`, an ACP error) picks no winner whatever it said: the checks
   ranking decides, with a notice (a flagged candidate cannot win that
   way).

**Outcome.** The winner is applied (checked with `git apply --check` first)
and every worktree is removed. Nothing is ever applied once `--mins` has
expired, for one candidate or a fan-out winner: the run is `timed_out` and
the result stays on its branch. The deadline is checked just before the
apply starts; the apply itself is never cut halfway, so a deadline reached
during it ends `applied` (exit 0). With `--no-apply` every candidate with
changes stays in its worktree and the branches are printed, the winner
marked. No winner (every candidate failed, or the judge said none or could
not answer) exits 1 with the candidates kept. `--mins` running out during
the candidates or the judge exits 2 (`timed_out`) with the candidates kept;
a budget spent before the judge could start, or before every candidate
could run, exits 2 (`budget_exhausted`) with the candidates kept: when a
planned candidate could not run, nothing is chosen or applied, even from the
candidates that did. This is reported ahead of `no_changes`.

- **Outcome precedence.** When more than one condition holds, the run's
  outcome is the highest row that applies, for one candidate and for a
  fan-out:

  | Rank | Condition | Outcome | Exit | Applied | Kept |
  | ---: | --- | --- | ---: | --- | --- |
  | 1 | `--mins` ran out (during a candidate's turn, the checks, the judge, or before the apply) | `timed_out` | 2 | nothing | every candidate with changes, and every failed collect |
  | 2 | A budget was spent before a candidate or the judge could run, or a child Refraktal sent an untrusted report | `budget_exhausted` | 2 | nothing | every candidate with changes, and every failed collect |
  | 3 | No harness could be started | `unusable` | 3 | nothing | nothing to keep |
  | 4 | A candidate failed with changes (failed turn, failed collect, refused result) and no candidate is applicable | `failed` | 1 | nothing | every candidate with changes |
  | 5 | Otherwise: `applied`, `kept`, `apply_failed`, `no_winner`, `no_changes` as described above | | | | |

  A collect failure or a refused result is recorded beside the candidate's
  outcome: in its error and the `collected` event. It keeps the candidate's
  work. It never changes rank 1 or 2: a candidate that timed out or was
  exhausted stays so, so the run still exits 2 and no sibling is applied.
- **Collect after every child has stopped.** No candidate is collected until
  every child of the run (each candidate harness) has stopped and been swept:
  a still-running harness shares the repository's `.git` and could add a
  filter to its configuration between driver discovery and the collect.
- **A failed collect keeps the work.** If collecting a candidate fails (a
  stale `index.lock`, say), its worktree and branch are kept and reported,
  whatever the run's outcome; a failed collect never deletes work.
- **A candidate that failed with changes is kept.** A candidate whose turn
  or collect failed after it changed files (`max_tokens`, an ACP error, a
  result refused for leaving the session folder) keeps its worktree and
  branch whatever the run's outcome. A fan-out that ends with no applicable
  winner while such a candidate exists ends `failed` (exit 1), never
  `no_changes` or `no_winner`: partial work was produced and not used.
- **Never half-applied.** `git apply` writes file by file, so a failure
  after the first file (a full disk, a folder it cannot write) would leave
  the user's tree half-applied. Before the apply, every path the patch
  touches is snapshotted into `apply-snapshot[-<harness>]/` in the run
  folder, through its pinned root: content, permissions, link target, file
  or folder or absent, and the permissions of the folders above. The
  snapshot is kept until every step after the apply (the LFS attribute
  check) has also passed. On any failure of the apply or of those steps
  every path is restored byte for byte, the files and folders
  the apply created are removed, and each path is checked against the
  snapshot; the run is `apply_failed` (exit 1) and the result stays on its
  branch. The restore works through `os.Root` and never through a symlink
  the failed apply left. If the restore itself fails, the error names the
  paths not restored and the snapshot folder, which is kept with a
  `manifest.json`; otherwise the snapshot is removed. Known limits: a
  folder's ownership and timestamps are not restored, and another process
  writing the same paths during the apply is not detected.
- **A cancel stops before apply.** A cancel (Ctrl-C, or an ACP client's
  `session/cancel`) that arrives during judging or collection stops before
  the apply: the run fails (exit 1) and the result stays on its branch.

## ACP server and show (M7)

**`refraktal acp`** speaks ACP (protocol 1) as an agent on stdio, so any ACP
client, including a parent Refraktal, can drive it.

- `initialize`, `session/new` (an absolute folder), `session/set_mode` (only
  `default`), `session/prompt`, `session/cancel`. No `loadSession`.
  Parameter decode errors are rejected before session state changes or work
  starts.
- Each prompt is one Refraktal run in the session's folder, with the result
  applied there (a parent gives its child the child's worktree, so the
  parent collects the child's result like any harness's). A session whose
  folder is a subfolder of the repository stays in it: each harness works
  in that subfolder of its worktree, its edits are allowed only there, and
  a result that changes any file outside it (collected with rename
  detection off, so a move lists both paths) is refused, at collect and
  again before apply.
- Progress and notices arrive as message chunks, then a summary: the
  outcome, the harness and the files; raw child replies are never forwarded.
  Notices cross only in a safe form:
  one that can carry a raw OS error, URL or path (the price table, the
  decision log, the pre-score, LFS pointers) arrives as fixed wording.
  Error text never crosses, only "the error is in the child's run log": a
  harness's errors can carry account details. Nor do a harness's auth
  notifications or permission requests: the run answers those itself
  under its own policy.
- **Transport:** one writer drains a queue; a write that takes longer than
  10 s (a client that stopped reading), or a full queue, fails the
  transport, which cancels every run (their harnesses are shut down as on
  any cancel) and ends the server with an error. This holds through the
  final drain after stdin closes: the watchdog stays on, and a drain that
  cannot finish within the write timeout ends the server with an error,
  never success for an undelivered response. The read loop never holds
  the server: Ctrl-C ends it at once, even idle. A prompt ends with the
  cost in its own small frame, then the summary in chunks of at most
  256 KiB of text, then the response, so no frame nears the 16 MiB limit.
- The run's actual cost (what it charged, reported and computed) goes out
  as a `usage_update` with a US-dollar `cost`, cumulative for the session
  (as Claude reports it, so the parent charges the difference:
  `reported`). The prompt response's stop reason is `end_turn` (exit 0),
  `max_turn_requests` (a budget ran out), `refusal` (nothing usable) or
  `cancelled`, and `_meta.refraktal` carries the run's outcome, exit code,
  run folder, the prompts it sent to its own harnesses, and
  `retained_usd`: what its cancelled children keep reserved beyond their
  cost. The parent holds that as a `retained` charge: counted against
  `--usd`, not reported as spent.
- **A child's report is untrusted.** The parent accepts it only if the
  turns are within 0 and the child's remaining allowance (the allowance it
  was given), the cost is finite and not negative, and `retained_usd` is
  finite, not negative and within the grant. A cost field that is present
  but malformed (negative, not a number, not an object, no amount, or
  dropped with a malformed `used` or `size` in the same update) is an
  untrusted report, not an absent cost: it is never priced from tokens
  instead. An absent cost (no field, or `null`) is priced from tokens as
  for any harness. Anything else charges the
  child its whole grant (the unspent dollars as a `forfeited` charge, and
  every unused turn; its real costs are charged first and stand in full, so
  an overrun is never capped), marks it `budget_exhausted` with a notice, and the run
  follows the exhaustion rule: exit 2, candidates kept, nothing applied.
  Turn counts saturate rather than wrap.
- **Budgets:** the `acp` flags (`--usd --turns --mins --depth`), shrunk to
  the parent's `REFRAKTAL_BUDGET` (each the smaller; a deadline already
  past refuses the prompt). With a grant, the server keeps one ledger made
  from it on the first prompt, shared by every prompt and session, so the
  grant never resets; without one (a standalone server, an editor's), each
  prompt gets the flags afresh. A parent's grant counts the prompt it is
  sending, so the child gets the turns left after it; the child's reported
  prompts are added to the parent's `--turns`, so `--turns` covers the
  whole tree. `--depth` shrinks by one per level (a grant of depth 0 lets
  the child run harnesses but not another Refraktal), and the nesting
  guard refuses any run past the ceiling the top run set
  (`REFRAKTAL_NEST_CEILING`; see Processes), whatever its grant says.
- **Nested run folders:** every child is given `REFRAKTAL_PARENT_RUN`, its
  scratch folder in the parent's run folder; a child Refraktal makes its
  run folder there (`<parent run>/refraktal/runs/<id>`), through pinned
  directory handles, refusing a symlink, and checks that the folder it
  opened is the one it inspected (a swap between the two is refused).
- **The `refraktal` harness** (`refraktal acp`, the CLI's own binary) runs
  only when `--harness` names it. It has no read-only mode, so it never
  judges. A harness that is not installed, or a refused child, is a failed
  candidate like any other.

**`refraktal show [run] [--json]`** renders a run from its run log: the
latest, or the one with that ID. The task, the route or the pinned harness,
each candidate and the judge with their outcomes (and a child Refraktal's
prompts), the check source, the verdict (winner, method, reason, judge and
whether it judged its own candidate), costs by source with turns, the
result's harness, files and branch, then each nested child run, indented.
`--json` prints the same as one object with nested runs inside. If writing
that object fails, `show` reports the error on stderr and exits nonzero. Runs
are ordered by their first event's timestamp, with the run ID breaking ties;
actual spend and retained reservations are separate fields in text and JSON.

- Run logs are untrusted (a child process wrote the nested ones): show
  walks the tree through pinned handles, opening each folder and log only
  if it is a real directory or regular file (never a symlink, at any
  component, `.refraktal` included) and the same one it inspected. It
  follows nested runs only under the `refraktal` scratch folder. One
  budget covers the whole walk: 64 MiB of log, 200,000 events, 10,000
  folder entries and eight levels; past one, the run is marked truncated
  and show says so. An incomplete enumeration also marks the selected run
  truncated and warns that latest-run selection may be incomplete. It drops
  from every value it prints, and from the run names in its errors, the control
  characters (terminal escapes and newlines, so a value cannot forge a line)
  and the bidi and format characters.

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
  in the docs. (Done in the README.)

**As built (M5):**

- `internal/route` keeps the spike's questions, fallback rules, policy and
  thresholds byte for byte, so the spike's samples route identically: the ten
  recorded live answers replay to the same decisions offline, the fallback
  matches the spike's outputs, and live the ten samples gave the same
  decisions and outcomes as the spike's run.
- The questions name the spike's harness labels; they map to Refraktal's
  harnesses (`claude-code` → claude, `antigravity` → agy). The router never
  chooses `refraktal` (re-routing the same task to a child Refraktal gains
  nothing until planning splits it): the next most probable harness runs,
  with a notice. `--harness refraktal` runs a child Refraktal (see ACP
  server and show).
- A fan-out decision runs its harnesses (see Fan-out and judging); an
  escalation runs the most probable harness until planning is built, with a
  notice. `--harness` skips the router; `--fan N` caps the width. `--dry` routes and plans only (exit 0): no worktree, no
  harness.
- The key comes from `GEMINI_API_KEY` in the environment, else the
  repository's `.env` only if git confirms a work tree and tracks no path
  matching `.env` in any case (any git error means no) (a committed `.env` in a
  repository someone else wrote could carry their key, and task text would go
  to their project); a notice says when it came from `.env`. It goes only in
  the `x-goog-api-key` header, never in a URL, log or output. The decision
  log is opened with `O_NOFOLLOW`, so a committed symlink cannot redirect it. The router's call runs alongside
  the price fetch and worktree setup, under the run's `--mins` deadline.
- Cache: `os.UserCacheDir()/refraktal/decisions/<sha256 of the exact
  request>.json`, 24 hours; only valid Gemini answers are cached, with the
  provider's raw response so a cache hit's record keeps it; written through a
  unique temp file and an atomic rename.
- The Decider follows no redirects, and validates every answer (a
  probability map over known options, finite values in [0, 1], confidence in
  range); a redirect or an invalid answer falls back and is not cached. A
  provider's error body never reaches notices, output, records or doctor:
  only the HTTP status and a short digest.
- **No API key reaches a subprocess.** Every harness child and every tool
  runs without `OPENAI_API_KEY`, `ANTHROPIC_API_KEY`, `GEMINI_API_KEY` or
  `GOOGLE_API_KEY` (so no harness silently bills an API key instead of the
  user's subscription); a harness's `pass_env` in the config opts it in to a
  named key. Refraktal's git commands run without them and without `GIT_*`
  variables, with fsmonitor and hooks off, and ignore replacement refs
  (`--no-replace-objects`, `GIT_NO_REPLACE_OBJECTS=1`): a child shares the
  repository's `.git` and could otherwise `git replace` the pinned base so
  Refraktal reads a stub.
  Worktree and safegit share one sanitized environment through `proc.ScrubEnv`
  and the full static protections; driver-capable commands also share driver
  neutralisation and before/after checks, while `rev-parse`, `ls-files` and
  `check-attr` cannot run filters or drivers and safegit uses the cheaper path.
  Safegit queries and post-turn Git operations have short timeouts, and
  worktree setup runs under the run's deadline.
  A nested Refraktal that should route with Gemini gets the key only when the
  user sets `REFRAKTAL_PASS_GEMINI_KEY=1`, and then only the `refraktal`
  harness gets it: never an ordinary harness (its `pass_env` is the way to
  give one a key).
- Decision log: `.refraktal/decisions.jsonl` in the repository (gitignored),
  one record per decision with the run ID, the request input, every
  probability, the thresholds, the outcome and the provider's response; the
  run log gets a summarised `route` event. It is never appended to if git
  tracks it (or git cannot confirm otherwise): a notice says so.
- `doctor` reports the decider: no key means fallback; with a key, one probe
  decision on a trivial task shows whether the API answers in time (else
  fallback, with the reason).
- `go test` never talks to Google: from a test binary the client accepts only
  a loopback test server, unless `REFRAKTAL_LIVE=1`.

## Not in v1

A server or web console, a database, a model gateway, containers (later, as an
opt-in isolation tier), and a learner (the decision log is kept for it).
