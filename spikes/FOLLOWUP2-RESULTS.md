# Third-round results

Run on 2026-11-15, on the same laptop and logins as before, from `FOLLOWUP2.md`. Offline checks
passed first. Every step ran: the five main tests, the MCP baseline, one disabling attempt per
harness (both worked), and the context-growth pair. Raw output stays in the gitignored `results/`
folders. No global harness configuration was edited, moved or renamed. Paths below use `~`.

One addition to the brief: the client lists only the harness's own process group, and the tools'
`sleep` ran outside it. So I re-ran `midtool` once with a separate watcher that recorded every
`sleep 20` process on the machine every 0.6 s. Prompts and time limits were unchanged, at a cost of
3 more partial turns. The first run's files are kept as `followup2-midtool-run1-*`.

**Short answer.**
- Cancel stops the turn on all three harnesses, but it stops the running tool only on Claude.
  Antigravity's tool outlived the harness itself.
- A model call aborted by a cancel never appears in Claude's cumulative cost.
- `acceptEdits` (Claude) and `workspace-write` (Codex) still sent no permission request for a shell
  command or a file write.
- Usage on the response is per turn on all three harnesses.
- The user's MCP servers can be switched off per run on both Claude and Antigravity. On Claude, that
  removes the 15k-token growth between turns.

## Cancel during a running tool (`midtool-*`)

The prompt runs `sleep 20`, then writes `hello.txt`. The client cancels 10 s into the turn. Figures
are from the watched re-run, with the first run in brackets where it differs.

| | Claude Code | Codex | Antigravity |
|---|---|---|---|
| G1. Tool running at cancel? | Yes. `sleep` started about 3.3 s into the turn | Yes. Started at 7.2 s | Yes. Started at 8.2 s |
| G2. Stop reason, cancel to response | `cancelled`, 13 ms [15 ms] | `cancelled`, 23 ms [22 ms] | `cancelled`, 2 ms [15 ms] |
| G3. `sleep` after the cancel | **Gone** within one watcher interval (about 0.6 s) of the cancel | **Still running** about 6 s after the cancel response; ended when Codex shut down | **Still running.** When `agy` exited, it was reparented to PID 1 and ran the full 20 s |
| Tool status sent after cancel | `tool_call_update: failed` (both runs) | none | `failed` in the first run, none in the re-run |
| G4. `hello.txt` written? | No | No | No |
| G5. Usage for the partial turn | `usage` on the response (31,999 tokens), cost USD 0.1696 | `usage_update` 5 ms after the cancel; `usage` on the response (28,376 tokens) | `usage` on the response (23,187 tokens) |
| G5. Permission requests | None | None | 1, at 8,158 ms, **before** the cancel (answered `allow`) |

- **G1 and G3.** `procs_at_cancel` and `procs_after_cancel` cannot answer these: in every harness the
  `sleep` ran in a process group of its own, so neither list contains it. The answers come from the
  watcher. The watcher's clock was aligned to each turn through the `sleep` process's elapsed time,
  so the alignment is good to about ±1 s.
- **Antigravity orphan.** The orphaned `sleep` had its own process group, so the client's
  process-group kill did not reach it. A long-running tool started by Antigravity can outlive both
  the cancel and the harness.
- **G5, compared with the first follow-up.** There, a cancel 3 s into the turn reported no usage at
  all. Here, the model call that started the tool had finished before the cancel, and all three
  reported it. Usage appears for completed model calls only.

## Does a cancelled turn's spend show up later? (`spend-*`)

**G6.** Claude, three one-word turns per session. In the test session, turn 2 is the file-write
prompt, cancelled at 3,072 ms. Its stop reason was `cancelled`, 6 ms after the cancel.

| Cumulative cost (USD) after | Turn 1 | Turn 2 | Turn 3 |
|---|---|---|---|
| Control (plain, plain, plain) | 0.16732 | 0.41353 | 0.53841 |
| Test (plain, **cancelled**, plain) | 0.16732 | 0.16732 | 0.41386 |

| Response `usage` | Turn 1 | Turn 2 | Turn 3 |
|---|---|---|---|
| Control: cached read / cache write / total | 11,256 / 20,607 / 31,869 | 16,307 / 30,357 / 46,670 | 31,886 / 14,801 / 46,695 |
| Test: cached read / cache write / total | 11,256 / 20,607 / 31,869 | **0 / 0 / 0** | 16,307 / 30,399 / 46,712 |

**G7. The reported cost looks as if the cancelled turn never happened.**
- The test's figure after turn 3 (0.41386) matches the control's after turn 2 (0.41353). It is 0.125
  below the control's after turn 3.
- The cancelled turn's response carries all-zero tokens, and the cost did not move.
- The test's turn 3 has the same token profile as the control's turn 2: the same 16,307 cached read,
  and about 30.4k written to the cache again. So no cache entry from the cancelled turn was reused.

Confidence:
- **High** that Claude's reported cost leaves out a model call aborted by a cancel.
- **Unknown** whether that call cost anything real. A `usage_update` in the cancelled turn reported
  `used: 46,694` at 2,876 ms, so a request with about 46.7k tokens of context was in flight before
  the cancel. On a subscription login there is no bill to check against.

## Permission modes (`mode-*`)

**G8.** `session/set_mode` succeeded in all four runs. Each returned no error, and the harness
confirmed it with a `config_option_update` showing the new mode.

| Harness, mode | Prompt | Tool kind | Permission requests | Result |
|---|---|---|---|---|
| Claude, `acceptEdits` | `echo hi` | `execute` | 0 | `hi` (expected) |
| Claude, `acceptEdits` | write `hello.txt` | `edit` | 0 | `hello.txt` written (expected) |
| Codex, `workspace-write` | `echo hi` | `execute` | 0 | `hi` (expected) |
| Codex, `workspace-write` | write `hello.txt` | `execute` (`printf hello > hello.txt`) | 0 | `hello.txt` written (expected) |

The user's Claude settings contain no permission allow rules, so they do not explain why
`acceptEdits` ran `echo` without asking. Claude Code may treat `echo` as a read-only command that
needs no approval. That was not tested with a command that changes anything.

## Usage across turns (`turns-*`)

**G9. On both harnesses, the response `usage` is for the turn only.**

| | Turn 1 total | Turn 2 total | Turn 2 final `used` | Cumulative would be at least |
|---|---|---|---|---|
| Codex | 28,303 | 45,210 | 45,210 | 56,606 (turn 1 + a turn 2 whose context already holds turn 1) |
| Antigravity | 22,896 | 23,315 | 23,315 | 46,211 |

Turn 2's total equals that turn's own context (`used`) on both harnesses. Neither harness reported a
cost. Codex's context also grew by 16,907 tokens between two one-word turns: 17,045 uncached input
on turn 2, against 14,858 on turn 1. Codex started no MCP processes, so MCP does not explain that
growth, and its cause is unknown.

## Shutdown (`exit-late-claude`)

**G10.** The bundled `claude` was in the process group at 257 ms. It was gone at 1,069 ms and
stayed gone at 5,072 ms and 30,069 ms. It exits on its own within about 1 s of the adapter. The
200 ms sample in the first follow-up caught it on the way out.

## MCP isolation (`mcp-*`, `growth-*`)

**G11. Baseline.**
- Claude starts 2 of the user's MCP servers, a reverse-engineering toolkit and a PDF viewer. That is
  4 processes: a package launcher and a server for each.
- Antigravity starts 1, the same reverse-engineering toolkit, as 2 processes.

**G12. Candidates.** One attempt per harness was enough.

| Harness | Candidate | Login survived | MCP processes | Works? |
|---|---|---|---|---|
| Claude | `session/new` `_meta`: `{"claudeCode":{"options":{"strictMcpConfig":true}}}` | Yes. `auth/status_update` `authenticated`, and two full turns completed with it | 0 | **Yes** |
| Antigravity | `agy acp --gemini-dir <new empty temp dir>` | Yes, by inference. `session/new` succeeded, and `agy` fails `session/new` when logged out. No prompt was sent with this flag | 0 | **Yes**, with side effects |

- **Where the Claude option comes from.** The adapter merges `_meta.claudeCode.options` into the
  Agent SDK options (`dist/v1/acp-agent.js`). The SDK documents `strictMcpConfig` as using only the
  MCP servers passed in, and ignoring user settings, plugins and project `.mcp.json`. It maps to
  `--strict-mcp-config`. Settings, model and mode were unchanged (`opus`, `auto`).
- **Antigravity side effects.** `agy` filled the temp directory with a fresh configuration: an empty
  MCP config, a new installation id and its own conversation database. The flag therefore hides
  everything under `~/.gemini`, not only MCP: settings, customisations and history. Model and mode
  were unchanged (`gemini-4-carbon`, `default`). The temp directory was in the session scratchpad,
  not the user's configuration.

**G13. Context growth, with and without MCP servers.** Claude, two one-word turns in one session.

| | Control (MCP servers on) | `strictMcpConfig` (MCP servers off) |
|---|---|---|
| Final `used`, turn 1 → turn 2 | 31,869 → 46,670 (**+14,801**) | 31,319 → 31,344 (**+25**) |
| Cache write, turn 1 / turn 2 | 20,607 / 30,357 | 20,057 / 15,029 |
| Cumulative cost (USD), turn 1 / turn 2 | 0.1673 / 0.4135 | 0.1629 / 0.2865 |
| Turn 2's own cost (USD) | 0.2462 | 0.1236 |

Disabling the user's MCP servers removes the growth. It also halves turn 2's cost. The likely
mechanism is that the servers' tool definitions join the context after turn 1, but this is an
inference: the definitions themselves were not captured. With MCP off, turn 2 still wrote 15,029
tokens to the cache, for an unknown reason.

## Not done, and why

- Cancelling while a permission request is pending: Antigravity's request came and was answered
  about 2 s before the cancel.
- Whether an aborted Claude model call is billed: not observable on a subscription login.
- Claude in `default` and Codex in `read-only`, to find a mode that does route shell commands and
  writes to the client: outside this brief.
- An Antigravity prompt with `--gemini-dir`, to confirm the login on a real turn: the brief asks for a
  handshake only.
