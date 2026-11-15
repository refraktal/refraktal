# ACP spike: can one small Go program drive Claude Code, Codex and Antigravity?

Run on 2026-11-15 in a cloud sandbox with no harness credentials.

**Short answer.** Yes for the protocol: one hand-rolled Go client (standard library only) completed
`initialize` against all three real ACP servers, and `session/new` against Claude's. No prompt
reached a model here, because none of the three is logged in on this machine. So every statement
about what usage and cost data arrives during a real turn is **from adapter source code or docs,
not from a live turn**. `run-live.sh` closes that gap on a logged-in laptop.

Labels used below: **live** = observed here on the wire; **source** = read in the installed
adapter's code or found as strings in the binary; **docs** = vendor documentation; **unknown**.

## 1. Launch commands

| Harness | ACP server command | Native? | Versions checked here |
|---|---|---|---|
| Claude Code | `npx -y @agentclientprotocol/claude-agent-acp` (bin: `claude-agent-acp`) | No. Adapter over the Claude Agent SDK. The `claude` binary has no ACP mode | adapter 0.107.0 (published 2026-11-14), bundles Claude Code 2.1.323 |
| Codex | `npx -y @agentclientprotocol/codex-acp` (bin: `codex-acp`) | No. Adapter that starts the Codex app server. `codex --help` (0.166.0) lists no ACP subcommand | adapter 2.6.0 (published 2026-11-12), bundles `@openai/codex` 0.166.x |
| Antigravity | `agy acp` | Yes, built in | agy 1.5.9 (ACP first shipped in 1.5.0 on 2026-11-04, per a third-party release tracker) |

Notes:

- The old Zed packages are deprecated on npm and point to the new names:
  `@zed-industries/claude-code-acp` and `@zed-industries/claude-agent-acp` were renamed to
  `@agentclientprotocol/claude-agent-acp`; `@zed-industries/codex-acp` was replaced by
  `@agentclientprotocol/codex-acp`. (live, `npm view`)
- Both adapters also ship standalone executables on their GitHub releases, which need no Node
  (docs: adapter READMEs). Not tried.
- Claude adapter: needs Node >= 22. Uses its bundled Claude Code binary unless
  `CLAUDE_CODE_EXECUTABLE` points at another one. Authentication is whatever that Claude Code
  already has (its normal login). (docs + live)
- Codex adapter: uses its bundled Codex unless `CODEX_PATH` is set. Auth is a ChatGPT login,
  or `CODEX_API_KEY` / `OPENAI_API_KEY`. `INITIAL_AGENT_MODE` picks the approval/sandbox mode. (docs)
- agy: reuses the cached login of an interactive `agy` session, or `agy acp --login`. Useful flags:
  `--dangerously-skip-permissions`, `--sandbox`, `--process-per-session`, `--log-file`,
  `--disable-telemetry`. (docs + live `agy acp --help`)
- The harness table in `main.go` pins adapter versions, because unpinned `npx -y` runs whatever was
  published most recently. `run-live.sh` installs the same pinned versions into `./adapters`.

## 2. How far each live attempt got

Raw output is in `live-here/`.

| Harness | Process starts | `initialize` | `session/new` | `session/prompt` | Where it stopped |
|---|---|---|---|---|---|
| Claude (bundled 2.1.323) | yes | yes, protocol 1 | **yes** | sent, failed | `-32000 Authentication required` on the prompt, 544 ms after sending |
| Claude (installed 2.1.325 via `CLAUDE_CODE_EXECUTABLE`) | yes | yes | **yes** | sent, failed | same error |
| Codex | yes | yes, protocol 1 | failed | not sent | `-32000 Authentication required` on `session/new` |
| Antigravity | yes | yes, protocol 1 | failed | not sent | `-32000 Authentication required` on `session/new` |

Two prompts were sent to Claude Code in total (the limit was three). Both were rejected for
authentication before any model call; the third was not used because there is no other credential
path here. The sandbox's own Claude login is not visible to child processes.

What `initialize` returned (live):

- **Claude**: `protocolVersion: 1`, agentInfo `@agentclientprotocol/claude-agent-acp` 0.107.0,
  `authMethods: []`, `loadSession: true`, session capabilities list/resume/fork/close/delete,
  prompt capabilities image + embeddedContext. Right after, two non-spec notifications:
  `_auth/status_update` ("Not logged in") and `auth/status_update` (`unauthenticated`).
  `session/new` returned modes `default`, `acceptEdits`, `plan`, `auto`, `bypassPermissions`,
  config options for mode and model, and a very large `available_commands_update` (about 45 KB,
  sent twice).
- **Codex**: `protocolVersion: 1`, agentInfo `@agentclientprotocol/codex-acp` 2.6.0, one auth
  method `api-key` (I ran it with `NO_BROWSER=1`, which hides the ChatGPT method), `loadSession: true`.
- **Antigravity**: `protocolVersion: 1`, agentInfo `Antigravity ACP Server` 1.5.9, auth methods
  `oauth-personal`, `oauth-business`, `gemini-api-key`, `agent-platform`, `loadSession: true`.

## 3. Usage and cost answers

| Question | Claude Code | Codex | Antigravity |
|---|---|---|---|
| Sends `usage_update`? | Yes (**source**) | Yes (**source**) | Probably: the string and a `UsageUpdate` type are in the binary (**source**, weak). Docs say nothing. Otherwise **unknown** |
| With a `cost`? | Yes, `{amount: total_cost_usd, currency: "USD"}` (**source**) | No. `used` and `size` only (**source**) | **Unknown**. A `cost` JSON field exists in the binary but I cannot tie it to `usage_update` |
| Mid-turn or only at the end? | `used`/`size` mid-turn; `cost` only on the update sent with the turn's final result (**source**) | Mid-turn, on every Codex `thread/tokenUsage/updated` event (**source**) | **Unknown** |
| Usage on the `session/prompt` response? | Yes per a code comment, the unstable `usage` token counts (**source**) | Yes, `usage` plus `_meta.quota.token_count` and `model_usage` (**source**) | Probably (`acp.PromptUsage` type in binary) (**source**, weak) |

**None of the above is verified live.** Every cell is either code I read or unknown.

Field names (from the ACP JSON schema, latest release; repo tags run to v1.18.0; copy in `research/`):

- `usage_update`: `used` (tokens currently in context), `size` (context window), optional
  `cost {amount, currency}` described as the cumulative session cost. `currency` is an ISO 4217
  code or another unit such as `"credits"`.
- `usage` on the prompt response is still marked UNSTABLE in the v1 schema: `totalTokens`,
  `inputTokens`, `outputTokens`, optional `thoughtTokens`, `cachedReadTokens`, `cachedWriteTokens`.
  The stable `PromptResponse` has only `stopReason` and `_meta`.
- Stop reasons: `end_turn`, `max_tokens`, `max_turn_requests`, `refusal`, `cancelled`.
- `session/update` variants (stable): `user_message_chunk`, `agent_message_chunk`,
  `agent_thought_chunk`, `tool_call`, `tool_call_update`, `plan`, `available_commands_update`,
  `current_mode_update`, `config_option_update`, `session_info_update`, `usage_update`, `notice`,
  `compaction_update`, `compaction_summary_chunk`.
- Permission option kinds: `allow_once`, `allow_always`, `reject_once`, `reject_always`. The reply
  is `{outcome: {outcome: "selected", optionId}}` or `{outcome: {outcome: "cancelled"}}`.
- `session/cancel` is a notification with `sessionId`; the agent must then answer the prompt with
  `stopReason: "cancelled"`.

## 4. SDK or hand-rolled

**Hand-rolled**, on the standard library. Zero third-party dependencies.

`github.com/coder/acp-go-sdk` exists and is usable: latest tag v0.13.5 (2026-06-02), no
dependencies of its own, and it already has the `usage_update` variant. I did not use it because:

1. It has not moved in five months. Its README derives its version from the ACP schema version it
   was generated from (0.13.5), while the spec repo is at v1.18.0 and both adapters build on the
   JS SDK 2.4 to 2.6.
2. Generated typed unions hide what this spike is here to see. Refraktal needs to pass through
   and log updates it does not understand (`_meta` quota blocks, `_auth/status_update`). Raw
   `json.RawMessage` does that for free.
3. Its `Client` interface makes you implement all five terminal methods, which Refraktal refuses anyway.
4. The whole JSON-RPC layer is about 100 lines here (`write`, `call`, `await`, `readLoop`).

Reconsider if Refraktal must also be an ACP *agent* (it is one of its own v1 harnesses). Even
then, the fake agent in this spike is 174 lines.

## 5. What was built, and size

| File | Lines | What |
|---|---|---|
| `client.go` | 346 | The core: JSON-RPC, the turn, update streaming, usage capture, time limit, summary |
| `policy.go` | 100 | Answers to agent requests: permissions, file read/write confined to the cwd, refusals |
| `main.go` | 75 | Harness table, `--cmd`, flags |
| `cmd/fakeagent/main.go` | 174 | Scripted fake agent, 7 modes |
| `main_test.go` | 258 | 9 tests against the fake |

The core is 346 lines, a little over the 100 to 300 target; client plus policy plus CLI is 521.
About 70 of the core's lines are type declarations for the messages and the summary.

Decisions I made without asking:

- **Output lines** each carry a `type`: `initialize`, `session`, `update`, `request` (an agent
  request and how it was answered), `notification` (anything other than `session/update`), `summary`.
- **Timestamps**: `t_ms` is milliseconds since `session/prompt` was sent (Go monotonic clock).
  Lines before the prompt carry `setup_ms` (since spawn) instead, so the two are never confused.
- **"Mid-turn" definition**: a usage observation is `mid_turn` if at least one non-usage update
  followed it before the prompt response; `at_end` if nothing but usage followed it (or it came on
  the prompt response itself); `after_end` if it arrived after the response. `first_usage` and
  `first_cost` in the summary use this.
- **Permissions**: pick the first `allow_once` option, else `allow_always`; if neither is offered,
  return an error. After a timeout, answer `cancelled` as the spec requires.
- **Files**: both fs capabilities are advertised. Paths must be absolute and, after resolving
  symlinks, inside the session cwd. Terminal and every other method get `-32601`.
- **Time limit**: `session/cancel`, wait `--grace` (5 s), then SIGKILL the whole process group
  (adapters spawn children). The stop reason is then reported as `killed`.
- No `authenticate` call. The client reports auth failures; logging in is left to each harness.
- POSIX only (`Setpgid`, `kill`). Windows would need a different kill path.
- Antigravity is not on npm, so I downloaded the official 1.5.9 Linux binary into `adapters/agy/`
  (SHA-512 checked against the vendor manifest), did not run its installer, and ran it with `HOME`
  pointed inside this directory.

## 6. What in ACP will be awkward for Refraktal

**Budgets**

1. **ACP has no budget inputs.** Nothing in `session/new` or `session/prompt` carries a USD,
   turn, time or depth limit. Every budget has to be enforced by Refraktal from outside (watch,
   then `session/cancel`), or passed through harness-specific settings.
2. **`used` is not spend.** It is context window occupancy. It drops after compaction and says
   nothing about tokens billed. Billed tokens are only in the unstable `usage` on the prompt
   response, which arrives when the turn is over.
3. **Cost is optional and uneven.** Claude reports USD but, per its source, only with the final
   result of a turn, so a USD cap cannot stop a turn that is in progress. Codex reports no cost at
   all, so Refraktal needs its own price table for Codex tokens. Antigravity is unknown and may
   report credits. `currency` is a free string, so amounts cannot be summed blindly.
4. **Turn limits**: `max_turn_requests` exists as a stop reason, but the protocol offers no way to set the limit.
5. **Depth**: nothing. A child harness can start its own subagents without telling the client
   (subagent reporting is still a draft extension).

**Permissions and safety**

6. **Auto-approving is not a policy.** The request carries a tool call title and raw input that
   differ per harness. Deciding by path or command means parsing each harness's own shape.
7. **The harness decides what gets asked.** Claude's session mode (`default`, `acceptEdits`,
   `auto`, `bypassPermissions`), Codex's `INITIAL_AGENT_MODE` and agy's own permission rules all
   filter requests before the client sees any. Here Claude's new session started in mode `auto`.
8. **The fs methods are not a sandbox.** Agents use their own file and shell tools; the client fs
   methods are optional extras (the agy binary does not even contain the `fs/` method names).
   Confinement to a directory has to come from the OS or the harness's sandbox flag.
9. **Auth surfaces differently.** Claude's adapter accepts `session/new` when logged out and fails
   on the prompt; Codex and agy fail on `session/new`. All use `-32000`.
10. **Children share global state.** Runs wrote session data under `~/.claude` and `~/.codex`
    (`CODEX_HOME` moves the latter; agy follows `HOME`).

**Footprint**

11. Two of three harnesses need Node 22 and npm packages (about 870 MB installed together here),
    or the standalone adapter binaries. Refraktal can stay small; its dependencies will not.
12. Adapters send extension notifications and large updates. A client must ignore unknown methods
    and variants and must not assume lines are short.
13. A draft ACP v2 exists and the Claude adapter already serves it to clients that ask for
    version 2. Pin `protocolVersion: 1`.

## 7. Still unknown until `run-live.sh` is run

- Whether each harness really sends `usage_update` during a turn, how often, and the first
  `t_ms` relative to the end of the turn.
- Claude: confirm that `cost` appears only at the end, and whether it includes subagent spend.
- Codex: confirm there is never a `cost`, and what `_meta.quota` looks like on the response.
- Antigravity: everything about usage and cost, including the unit.
- Which permission option ids and kinds each harness offers, and which actions never ask
  (`./run-live.sh --with-tool` makes each agent write one file in a temp directory to show this).
- Whether each harness answers `session/cancel` with `cancelled` inside the 5 second grace.
- Whether usage updates trail the prompt response (the client keeps reading for 1 s to catch them).
- Whether the pinned adapters work with the Codex and Claude logins on the laptop as they are.

## 8. Housekeeping

- `go vet ./...` and `go test ./...` pass (also with `-race`).
- `adapters/` (1.1 GB: node_modules and the agy binary) is only needed to repeat the handshakes
  here and can be deleted. `run-live.sh` reinstalls the npm adapters where it runs.
- Side effects outside this directory, all from the live attempts: two empty-session folders under
  `~/.claude/projects/` (names contain `acp-live-claude`), and `~/.codex/` created when I ran the
  bundled `codex --version` before isolating `CODEX_HOME`. I left them in place.
- Sources: ACP schema and docs (agentclientprotocol.com and its GitHub release), Antigravity docs
  (`/docs/cli/acp/`, `/docs/ide/extensions/acp`), the npm registry, the Go module proxy, and the
  installed adapter code.
