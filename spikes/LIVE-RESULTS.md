# Live results: ACP and routing spikes

Run on 2026-11-15 on a laptop where Claude Code, Codex and Antigravity are logged in with
subscription accounts. Both spikes ran as written: no changes to prompts, time limits or harnesses.
Raw output stays in the gitignored `acp/results/` and `routing/results/`. This file summarises it
and leaves out account and machine details.

**Short answer.** All three harnesses completed a full turn over ACP, including a file write. The
usage and cost claims from adapter source hold for Claude Code and Codex. Antigravity sends
`usage_update` but no cost. All 10 routing calls succeeded against the real Decisions API with no
fallback. The one claim the data contradicts is latency: 328 to 661 ms end to end, not 150 to 250 ms.

## Setup

| Item | Version | How known |
|---|---|---|
| Go | 1.27.4 (installed with Homebrew for this run; it was missing) | observed |
| Node / npm | 22.22.3 / 10.9.8 | observed |
| `@agentclientprotocol/claude-agent-acp` | 0.107.0 (bundles `claude-agent-sdk` 0.3.323, Claude Code 2.1.323) | observed (`initialize`, `package.json`) |
| `@agentclientprotocol/codex-acp` | 2.6.0 (bundles `@openai/codex` 0.166.0) | observed (`initialize`, `package.json`) |
| ACP JS SDK in the adapters | 2.6.0 | observed (`package.json`) |
| Antigravity | `agy` 1.5.8, native `agy acp` | observed (`initialize` agentInfo matches `agy --version`) |
| Installed CLIs, for comparison | `claude` 2.1.325, `codex` 0.169.0 | observed |

**Which harness binary each adapter ran.** Probably the adapters' bundled copies (Claude Code 2.1.323
and Codex 0.166.0), not the installed CLIs. Neither `CLAUDE_CODE_EXECUTABLE` nor `CODEX_PATH` was
set, and the adapters use their bundled binary when these are unset. This is an **inference**: no
line in the raw output names the harness binary or its version. The bundled copies still used the
existing logins.

## ACP

Every run reached `initialize`, `session/new` and a completed `session/prompt` with `stopReason:
end_turn`. No run timed out or was killed. `t_ms` is milliseconds after `session/prompt` was sent.

| | Claude Code | Codex | Antigravity |
|---|---|---|---|
| Turn wall time, plain / tool | 3,087 / 5,512 ms | 5,808 / 9,252 ms | 8,123 / 34,139 ms |
| `session/new` ready after spawn, 1st / 2nd run | 3,593 / 669 ms | 3,039 / 654 ms | 3,853 / 2,817 ms |
| Model, as configured by the session | `opus`, effort high | `gpt-6.1-sol`, reasoning `xhigh` | `gemini-4-carbon`, thought medium |
| Login reported | Claude subscription (Max) | ChatGPT subscription (Pro) | none reported over ACP |

### 1. How far each run got

All six runs: `initialize`, `session/new`, prompt completed, `end_turn`.

### 2. `usage_update` count and timing

| Run | Count | `t_ms` of each (turn end) |
|---|---|---|
| Claude plain | 3 | 3,042 · 3,053 · 3,086 (end 3,087) |
| Claude tool | 6 | 2,329 · 3,381 · 3,382 · 5,089 · 5,472 · 5,511 (end 5,512) |
| Codex plain | 1 | 5,767 (end 5,808) |
| Codex tool | 2 | 7,443 · 9,245 (end 9,252) |
| Antigravity plain | 2 | 40 (`used: 0`) · 8,118 (end 8,123) |
| Antigravity tool | 3 | 38 (`used: 0`) · 20,501 · 34,131 (end 34,139) |

No usage arrived after the prompt response (`after_end`: none).

### 3. Cost

| | Claude Code | Codex | Antigravity |
|---|---|---|---|
| Any `cost`? | Yes | No | No |
| Currency | `USD` | n/a | n/a |
| Amount, plain / tool | 0.1676 / 0.2971 | n/a | n/a |
| Where | Only the last `usage_update`, 1 ms before the prompt response | never | never |

Each run is a new session, so the "cumulative session cost" equals the turn cost here. Cost
accumulating across turns was not tested.

### 4. First usage observation: mid-turn or end

The summary says `first_usage: mid_turn` for all six runs. That label is accurate by the client's
definition (some non-usage update followed), but it overstates things for turns with no tools:

- **Plain turns:** effectively at the end. Claude's first update came 45 ms before the end, followed
  only by the reply text. Codex's came 41 ms before the end, followed only by `session_info_update`.
  Antigravity's "mid-turn" update at 40 ms carries `used: 0`; its first real figure came 5 ms
  before the end.
- **Tool turns:** genuinely mid-turn. The first nonzero `used` came at 2,329 ms of 5,512 (Claude,
  before the tool call), 7,443 of 9,252 (Codex, after the tool call) and 20,501 of 34,139
  (Antigravity, before the tool call).

### 5. Usage on the prompt response

Yes for all three. All three return the unstable `usage` block (`inputTokens`, `outputTokens`,
cached counts, `totalTokens`). Claude and Codex also return `_meta.quota` with `token_count` and
per-model `model_usage`. Antigravity's response has no `_meta`.

Billed totals are larger than the final `used` whenever a turn makes more than one model call:

| Tool run | Final `used` | Response `totalTokens` |
|---|---|---|
| Claude | 46,907 | 78,904 |
| Codex | 28,467 | 56,872 |
| Antigravity | 23,425 | 46,757 |

### 6. With `--with-tool`

| | Claude Code | Codex | Antigravity |
|---|---|---|---|
| Starting mode | `auto` | `agent` | `default` |
| Modes offered | default, acceptEdits, plan, auto, bypassPermissions | read-only, workspace-write, agent, agent-full-access | default, accept-edits, auto, plan |
| How it wrote the file | `edit` tool | `execute` tool (shell) | `write_file` tool (kind `edit`) |
| Permission request reached client | No | No | Yes, one |
| File written | Yes | Yes | Yes |

Antigravity's request offered `allow` (`allow_once`) and `deny` (`reject_once`). The client chose
`allow`. No harness used the client's `fs/*` methods (`fs_requests: 0` in every run).

### 7. Dollar figure on a subscription login

Claude Code: yes. It reports USD amounts (0.17 and 0.30) on a subscription login, so the figure is
notional, not a charge. Codex and Antigravity report no money figure at all.

### 8. Unexpected

- **Account identity on the wire.** Claude sends `_auth/status_update` and `auth/status_update`
  (each three times per run). Codex sends `_auth/status_update` once. These carry the account email,
  organisation and plan. Refraktal must redact them before it logs or forwards protocol traffic.
- **Large repeated updates.** Claude sent `available_commands_update` (about 78 KB) four times in a
  one-word turn, so roughly 310 KB per run. Codex sent one 42 KB copy during the turn and a 128 KB
  copy after the prompt response. Antigravity sent one 47 KB copy.
- **Hidden second model.** Claude's `model_usage` shows a Haiku call (about 1,200 tokens) beside
  Opus in every run, in line with the session title it generates. Whether the reported USD includes
  it is unknown.
- **High default effort.** A one-word reply cost Claude 31,899 tokens (20,637 of them cache writes)
  and a notional USD 0.17. Codex defaulted to reasoning effort `xhigh`.
- **Antigravity.** It is slow (34 s for the one-file write). It echoes the prompt as
  `user_message_chunk` and sends no auth notifications. Its permission request's `toolCall` has
  non-spec `name` and `args` fields. `initialize` carries duplicate snake_case capability keys
  (`load_session`, `session_capabilities`) and `_meta.antigravity.tier`. It titled the plain session
  "Ignoring Instructions And Rules".
- **Shutdown.** Every adapter's main process exited within the 5 s grace after stdin closed
  (`killed: false`). The client always sends a process-group SIGKILL afterwards, so whether child
  processes would have lingered was not observed. Exit time was not measured more precisely.
- `session/cancel` was not exercised: no turn reached the 2 minute limit.

## Routing

### 1. Did all 10 calls succeed?

Yes. 10 of 10 returned answers from `gemini-decision-1`. None fell back and `run-live.sh` exited 0.

### 2. Response shape

It is the documented shape: `answers` keyed by our question keys and `usage` with
`total_input_tokens`, `input_tokens_by_modality` (text only), `total_output_tokens` and
`total_tokens`. Choice and score answers carry `probabilities`; score answers also carry `score` and
`legend`. Score probabilities and the legend are keyed by level as strings (`"0"`, `"1"`, `"2"`).
The parser rejected nothing.

`confidence` equals the top probability, or max(p, 1 - p) for yes/no, in **all 50 answers**, with a
maximum difference of 0. `total_output_tokens` was 0 in every call.

### 3. Latency, tokens and cost per call

The calls ran in one process with one HTTP client, in this order. Only the first paid a cold TLS
handshake. Latency is measured end to end by the client, so it includes the network.

| Task | Latency (ms) | Input tokens | Cost (USD, computed) |
|---|---|---|---|
| one-line-bugfix (cold) | 661.4 | 771 | 0.0000771 |
| cross-cutting-refactor | 598.1 | 779 | 0.0000779 |
| add-tests | 328.0 | 761 | 0.0000761 |
| ambiguous-faster | 486.0 | 745 | 0.0000745 |
| docs | 512.5 | 761 | 0.0000761 |
| ui-feature | 612.4 | 765 | 0.0000765 |
| investigation | 604.2 | 768 | 0.0000768 |
| multi-part-feature | 571.0 | 774 | 0.0000774 |
| open-design | 579.5 | 755 | 0.0000755 |
| no-tests-bugfix | 607.4 | 758 | 0.0000758 |

Median 589 ms. Warm calls were no faster than the cold one in a consistent way, so most of the
latency is not the handshake. All 10 calls together cost about USD 0.00076.

### 4. Model versus fallback

Each cell shows the top answer and its probability. The policy outcome follows the slash.

| Task | Model: kind · harness · P(fan) · P(decomp) / policy | Fallback: kind · harness · P(fan) · P(decomp) / policy | Expected |
|---|---|---|---|
| one-line-bugfix | bugfix .98 · codex .96 · .04 · .01 / route codex | bugfix .80 · codex .75 · .15 · .10 / route codex | bugfix / route |
| cross-cutting-refactor | refactor .99 · claude-code .77 · .46 · .72 / escalate | refactor .80 · refraktal .60 · .60 · .80 / escalate | refactor / escalate |
| add-tests | test .99 · codex .96 · .10 · .02 / route codex | test .80 · codex .75 · .15 · .10 / route codex | test / route |
| ambiguous-faster | refactor .70 · claude-code .67 · .87 · .73 / escalate | feature .30 · claude-code .35 · .75 · .10 / escalate | investigation / escalate |
| docs | docs 1.00 · codex .98 · .09 · .02 / route codex | docs .80 · codex .70 · .15 · .10 / route codex | docs / route |
| ui-feature | feature .99 · antigravity .83 · .30 · .10 / route antigravity | feature .80 · antigravity .70 · .15 · .10 / route antigravity | feature / route |
| investigation | investigation .83 · claude-code .92 · .59 · .14 / route claude-code | investigation .80 · claude-code .70 · .15 · .10 / route claude-code | investigation / route |
| multi-part-feature | feature .97 · refraktal .70 · .58 · .91 / escalate | feature .50 · refraktal .60 · .15 · .80 / escalate | feature / escalate |
| open-design | feature .45 · codex .77 · .22 · .06 / route codex | feature .30 · claude-code .35 · .75 · .10 / escalate | feature / fan_out |
| no-tests-bugfix | bugfix .99 · codex .94 · .06 · .02 / route codex | bugfix .80 · codex .70 · .15 · .10 / route codex | bugfix / route |

Agreement between model and fallback: policy action 9 of 10, harness 8 of 10, task kind 9 of 10.

Observations on the bands:

- Harness confidence ranged from 0.67 to 0.98. None fell under the 0.40 escalate floor.
- `fan_out` never reached 0.70 on a task that the policy did not escalate first. The policy chose
  fan-out zero times.
- `fan_out_width` confidence was low throughout (0.40 to 0.75).
- Both decomposition escalations were clear (0.91), or just over the bar (0.72 for the migration).

### 5. Where the model disagrees with `EXPECTED`

| Task | Model | Expected | Note |
|---|---|---|---|
| ambiguous-faster | kind `refactor` (0.70) | `investigation` | The action (escalate) still matches |
| open-design | route to codex (0.77) | fan_out | P(fan) 0.22: the model sees "improve error messages" as one clear change |

The model matched `EXPECTED` on action 9 of 10 and on kind 9 of 10.

## Claims in `CLAIMS.md`

### ACP

| # | Claim | Verdict | Deciding observation |
|---|---|---|---|
| A1 | Claude Code has no built-in ACP; driven through the adapter | **Confirmed** | `claude --help` (2.1.325) lists no ACP mode; the adapter drove a full turn |
| A2 | Codex has no built-in ACP; driven through the adapter | **Confirmed** | `codex --help` (0.169.0) lists no ACP mode; the adapter drove a full turn |
| A3 | Antigravity speaks ACP natively with `agy acp` | **Confirmed** | Full turns, including a file write, over `agy acp` |
| A4 | Claude Code sends `usage_update` during a turn | **Confirmed** | 3 and 6 updates; tool run's first at 2,329 of 5,512 ms |
| A5 | Claude Code's `usage_update` carries a cost in USD | **Confirmed** | `{amount: 0.1676, currency: "USD"}` and 0.2971 |
| A6 | Claude cost only at the end; mid-turn updates carry context size only | **Confirmed** | Cost only on the last update, 1 ms before the response; all earlier ones `used`/`size` only |
| A7 | Codex sends `usage_update` during a turn | **Confirmed** | Tool run: update at 7,443 of 9,252 ms, after the tool call. Plain run's only update was 41 ms before the end |
| A8 | Codex never sends a cost | **Confirmed** (for the runs observed) | No `cost` on any update or the response in either run |
| A9 | Antigravity sends `usage_update` | **Confirmed** | 2 and 3 updates; the first always `used: 0` at about 40 ms |
| A10 | Antigravity sends a cost | **Contradicted** | No `cost` on any update or the response in either run |
| A11 | The prompt response carries token usage (Claude, Codex) | **Confirmed** | `usage` plus `_meta.quota` on both. Antigravity also returns `usage` |
| A12 | Mid-turn `used` is context fill, not spend | **Confirmed** | Final `used` vs billed `totalTokens`: 46,907 vs 78,904 (Claude), 28,467 vs 56,872 (Codex), 23,425 vs 46,757 (agy) |
| A13 | Claude starts in `auto`, so permission requests may never reach the client | **Confirmed** | Mode `auto`; the file write raised no permission request |
| A14 | Antigravity applies its own permission rules before any ACP request | **Still unknown** | In mode `default` the write did reach the client as a request; no action was tried that its rules might filter |
| A15 | A cached Codex login is enough to get past `session/new` | **Confirmed** | `session/new` and the prompt succeeded on the existing ChatGPT login with no `authenticate` call |
| A16 | On a subscription login a notional dollar figure is still reported | **Confirmed for Claude only** | USD 0.17 and 0.30 on a subscription login. Codex and agy report no money figure |
| A17 | Adapters need a process-group kill to shut down cleanly | **Still unknown** | All main processes exited within 5 s of stdin closing; the unconditional group kill hides whether children lingered |
| A18 | Harnesses send non-standard notifications and updates before the first prompt | **Confirmed for Claude and Codex, not agy** | Claude and Codex send `_auth/status_update` (Claude also `auth/status_update`) before `session/new` returns. agy sends nothing before the prompt, though its `initialize` result has non-standard fields |

**USD budgets.** The live data supports the conclusion that a USD budget cannot stop a turn in
progress on any harness. Claude's only cost figure arrives 1 ms before the turn ends. Codex and
Antigravity report no cost at all. One qualification: mid-turn `used` values do arrive during tool
turns on all three, so Refraktal could build an *estimated* spend guard from them with its own price
table. That would be an estimate, not a reported figure. A time limit is still the only hard stop.

**Harness binary actually run.** Probably the adapters' bundled Claude Code 2.1.323 and Codex
0.166.0, not the installed 2.1.325 and 0.169.0 (inferred, see Setup). Antigravity ran the installed
1.5.8, not the 1.5.9 that `acp/FINDINGS.md` describes.

### Routing

| # | Claim | Verdict | Deciding observation |
|---|---|---|---|
| R1 | `POST /v1beta/decisions`, `x-goog-api-key`, `gemini-decision-1` | **Confirmed** | 10 of 10 calls answered |
| R2 | One request carries all five questions over one shared input | **Confirmed** | Every response answered all five keys |
| R3 | Each answer returns a distribution and a `confidence` | **Confirmed** | `probabilities` on choice and score, `noul` on yes/no, `confidence` on all 50 |
| R4 | `confidence` equals the top probability, so it is not a second signal | **Confirmed** | Exact match in 50 of 50 answers (20 yes/no, 20 choice, 10 score) |
| R5 | Usage is returned and output tokens are always 0 | **Confirmed** | `total_output_tokens: 0` in 10 of 10 |
| R6 | About 700 input tokens, about USD 0.00007 per decision | **Confirmed** | 745 to 779 tokens, USD 0.0000745 to 0.0000779 |
| R7 | Latency 150 to 250 ms, flat in the number of questions | **Contradicted** (latency); flatness **still unknown** | 328 to 661 ms end to end, median 589 ms. Every call had five questions, so flatness was not tested |
| R8 | Hyphenated option keys such as `claude-code` are accepted | **Confirmed** | `claude-code` came back as a scored option and as the top choice |
| R9 | The parser accepts the real response with no rejected fields | **Confirmed** | No fallback in 10 calls |
| R10 | The fallback agrees with the model on most sample tasks | **Confirmed** | Action 9/10, harness 8/10, kind 9/10 |

## Not done, and why

- `session/cancel` behaviour: no turn hit the time limit, and the brief forbids changing it.
- Repeatability of the routing answers: the script runs each task once.
- R7 flatness, A14, A17 and Claude's cost across several turns: the scripts do not exercise them.
