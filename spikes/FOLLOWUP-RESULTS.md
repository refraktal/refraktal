# Follow-up results

Run on 2026-11-15, on the same laptop and logins as `LIVE-RESULTS.md`, from `FOLLOWUP.md` as
written: no changes to prompts, time limits or harnesses. Offline checks passed first. All ten ACP
runs, the latency test (21 calls) and the optional two repeat routing runs completed. Raw output
stays in the gitignored `results/` folders. Paths below use `~` for the home directory.

**Short answer.**
- All three harnesses answer `session/cancel` correctly within 146 ms, but a cancelled turn reports
  no usage and no cost.
- Claude's cost is cumulative for the session. Its token `usage` is for the turn only.
- Claude leaves its bundled `claude` binary running after the adapter exits. Codex and Antigravity
  leave nothing.
- Antigravity runs reads without asking, asks before shell commands in `default`, and stops asking
  in `auto`.
- Routing latency is flat from 1 to 25 questions and 1.8 times higher at 100. The answers are
  exactly repeatable.

## Cancel (`followup-cancel-*`)

The client sent the file-write prompt and then `session/cancel` about 3.08 s into the turn. No harness
had started a tool call by then. These runs therefore test cancelling while the model is thinking,
not while a tool is running.

| | Claude Code | Codex | Antigravity |
|---|---|---|---|
| F1. Answered cancel, or killed? | Answered (`killed: false`) | Answered | Answered |
| F2. Stop reason | `cancelled` | `cancelled` | `cancelled` |
| F3. Cancel to response (`stop_after_cancel_ms`) | 1 ms | 146 ms | 2 ms |
| F4. Usage for the partial turn | None | None | Only the `used: 0` update at 44 ms, before the cancel |
| F5. Cost for the cancelled turn | None | n/a | n/a |
| F6. `hello.txt` written? | No | No | No |
| F6. Permission requests | None | None | None |

- **F4.** No harness sent a `usage_update` with real figures for the cancelled turn, and no prompt
  response carried `usage`. Whatever the model consumed before the cancel is not reported.
- **F6.** No permission request arrived before or after the cancel, so answering a request with
  `cancelled` was not exercised.
- Nothing arrived from any harness in the 5 s after the cancel response.

## Shutdown (A17)

After the client closed stdin, it waited for the spawned process. It listed what was left in the
process group 200 ms after that process exited, and then killed the group.

| Run | Claude Code | Codex | Antigravity |
|---|---|---|---|
| Handshake only (`exit`) | Adapter exited; **bundled `claude` still running** | Adapter exited; nothing left | Exited; nothing left |
| Cancelled turn (`cancel`) | Adapter exited; **bundled `claude` still running** | Adapter exited; nothing left | Exited; nothing left |
| Two turns (`turns`) | Adapter exited; **bundled `claude` still running** | not run | not run |

The survivor in all three Claude runs is
`~/…/spikes/acp/adapters/node_modules/@anthropic-ai/claude-agent-sdk-darwin-arm64/claude`. The
client samples once, at 200 ms, so whether it would have exited later is not known. The Claude
child's MCP server processes (see below) did not survive. Busy and idle runs behaved the same way.

## Which binary runs (B1)

The process group in the middle of the turn shows the adapters run their **bundled** harness
binaries, not the CLIs installed on the machine. This replaces the inference in `LIVE-RESULTS.md`.

| Harness | Binary in the process group | Version |
|---|---|---|
| Claude Code | `~/…/adapters/node_modules/@anthropic-ai/claude-agent-sdk-darwin-arm64/claude --output-format stream-json` | 2.1.323 (from its `--version`; the installed CLI is 2.1.325) |
| Codex | `~/…/adapters/node_modules/@openai/codex-darwin-arm64/vendor/aarch64-apple-darwin/bin/codex app-server` | 0.166.0 (from its `--version`; the installed CLI is 0.169.0) |
| Antigravity | `~/.local/bin/agy acp` (the installed CLI) | 1.5.8 |

The paths do not contain the version. The versions come from running each binary with `--version`.

**Unexpected: the user's own MCP servers run inside the harness.** Claude's process group held two
MCP servers from the global Claude configuration on this machine. Antigravity's held one from its
own configuration. Codex's held none. A harness started by Refraktal therefore inherits the user's
global tools, unless Refraktal isolates its configuration.

## Antigravity permissions (A14)

| Run | Mode | Tool kind | Permission requests | Reply | Expected | Wall time |
|---|---|---|---|---|---|---|
| Read `notes.txt` | `default` | `read` | 0 | `refraktal-check-7` | yes | 17.2 s |
| `echo hi` | `default` | `execute` | 1 (`execute`, options `allow`/`allow_once`, `deny`/`reject_once`) | `hi` | yes | 15.2 s |
| `echo hi` | `auto` (set with `session/set_mode`) | `execute` | 0 | `hi` | yes | 21.1 s |

`session/set_mode` to `auto` worked: Antigravity answered with `current_mode_update` and
`config_option_update`, both showing `auto`.

**Verdict.** Antigravity decides some actions by its own rules without asking the client. In
`default` it runs a file read without asking, but asks before a shell command (and, in the first
run, before a file write). In `auto` it no longer asks before the shell command.

## Cost across turns (`followup-turns-claude`)

Two plain prompts in one session. Both replies were `OK`.

| | Turn 1 | Turn 2 |
|---|---|---|
| Wall time | 5,302 ms | 3,423 ms |
| `cost` on the last `usage_update` | USD 0.1673 | USD 0.4135 |
| Response `usage.totalTokens` | 31,867 | 46,668 |
| Cached read / cache write tokens | 11,256 / 20,605 | 16,306 / 30,356 |
| Final `used` | 31,867 | 46,668 |

- **C1. Cost is cumulative for the session.** Turn 2 on its own, priced in proportion to turn 1
  from its token mix, comes to about USD 0.246. 0.1673 + 0.246 = 0.413, which matches the reported
  0.4135. A turn-only figure would be about 0.246. This uses the usual cache multipliers (read 0.1×
  and write 1.25× the input price) as an assumption. A write multiplier of 2× gives the same sum to
  three decimals, so the conclusion does not depend on it.
- **C2. Token `usage` is for the turn only.** Turn 2's `totalTokens` (46,668) equals turn 2's own
  `used`. A cumulative figure would be at least 31,867 + 46,668 = 78,535. In `LIVE-RESULTS.md`, a
  tool turn's `totalTokens` was larger than its `used`, so `usage` sums the model calls *within* a
  turn but not *across* turns.
- **Unexpected.** For a one-word exchange, context grew by 14,801 tokens between turns. Turn 2 wrote
  30,356 tokens to the cache again. The cause is not known.

## Latency (R7)

One process with one HTTP client. One warm-up call, then 5 rounds of 1, 5, 25 and 100 questions in
rotating order, 8 s apart. All 21 calls returned HTTP 200 with every answer present. None was
rejected and none hit HTTP 429.

| Questions | Calls | Median (ms) | Min (ms) | Max (ms) | Input tokens |
|---|---|---|---|---|---|
| 1 | 5 | 478 | 450 | 774 | 208 |
| 5 | 5 | 508 | 332 | 713 | 761 |
| 25 | 5 | 543 | 505 | 897 | 3,469 |
| 100 | 5 | 876 | 785 | 1,269 | 13,624 |

The warm-up call (1 question) took 755 ms, about 280 ms more than the warm median for one question.

**Verdict.** Latency is close to flat from 1 to 25 questions (+65 ms at the median). It is not flat at
100 questions: the median is 1.8 times higher, at 13,624 input tokens. That is still under the
16K-token context the launch post's "nearly flat" claim covers. Every size is well above the 150 to
250 ms figure.

## Repeatability (R11)

`run-live.sh` ran two more times, so there are three runs of the 10 tasks (30 calls), all decided by
Gemini. **No task changed** policy action, harness or task kind. Every probability in every answer
was identical across the three runs (maximum difference 0). Input tokens were identical as well.
Median latency per run was 589, 524 and 567 ms (all 30 calls: 268 to 683 ms).

## Claims updated in `CLAIMS.md`

| # | New verdict | Deciding observation |
|---|---|---|
| A14 | **Confirmed** | Read ran in `default` with no request. `echo` asked in `default` and did not ask in `auto` |
| A17 | **Confirmed for Claude, not Codex or Antigravity** | Bundled `claude` still in the group 200 ms after the adapter exited, in 3 of 3 runs. Codex and agy left nothing |
| R7 | **Contradicted** | Medians of 478 to 876 ms. Flat from 1 to 25 questions, 1.8 times higher at 100 |

## Not done, and why

- Cancelling during a running tool, or during a pending permission request: at 3 s no harness had
  started a tool, and the brief fixes the timing.
- Whether Claude's surviving child exits on its own after more than 200 ms: the client samples once.
- Whether a cancelled turn's spend appears in Claude's next cumulative cost: no turn followed a
  cancel.
