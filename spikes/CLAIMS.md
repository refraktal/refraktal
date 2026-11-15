# Claims to check against the live run

These are the pre-live findings from the spikes (2026-11-15). Each one is
marked with how it was known **before** any live run. `LIVE-RESULTS.md` should
mark each claim **confirmed**, **contradicted** or **still unknown**, with the
observation that decides it.

Source key: **verified** (observed running, but without logins), **source**
(read from an adapter's source code), **docs** (vendor docs), **unknown**.

## ACP

| # | Claim | Known from |
|---|---|---|
| A1 | Claude Code has no built-in ACP; it is driven through `@agentclientprotocol/claude-agent-acp` | verified |
| A2 | Codex has no built-in ACP; it is driven through `@agentclientprotocol/codex-acp` | verified |
| A3 | Antigravity speaks ACP natively with `agy acp` | verified (handshake only) |
| A4 | Claude Code sends `usage_update` during a turn | source |
| A5 | Claude Code's `usage_update` carries a cost in USD | source |
| A6 | Claude Code's cost arrives only at the end of the turn; mid-turn updates carry context size only | source |
| A7 | Codex sends `usage_update` during a turn | source |
| A8 | Codex never sends a cost | source |
| A9 | Antigravity sends `usage_update` | unknown |
| A10 | Antigravity sends a cost | unknown |
| A11 | The prompt response itself carries token usage (Claude Code, Codex) | source |
| A12 | The mid-turn `used` field is context-window fill, not spend | docs |
| A13 | Claude Code sessions start in `auto` permission mode, so permission requests may never reach the client | verified (no login) |
| A14 | Antigravity applies its own permission rules before any ACP permission request | docs |
| A15 | A cached Codex login is enough to get past `session/new` | unknown |
| A16 | On a subscription login a dollar figure is still reported (a notional one) | unknown |
| A17 | Adapters need a process-group kill to shut down cleanly | verified |
| A18 | Harnesses send non-standard notifications and updates before the first prompt | verified |

Conclusion that depends on A6 and A8: **a USD budget cannot stop a turn in
progress on any harness; only a time limit can.** Say whether the live data
supports it.

One thing the pre-live work did not anticipate, so record it explicitly: which
harness binary each adapter actually ran (its own bundled copy, or the CLI
installed on the machine) and the version.

## Routing

| # | Claim | Known from |
|---|---|---|
| R1 | Endpoint `POST /v1beta/decisions`, header `x-goog-api-key`, model `gemini-decision-1` | docs |
| R2 | One request can carry all five questions over one shared input | docs |
| R3 | Each answer returns a probability distribution and a `confidence` | docs |
| R4 | `confidence` equals the top probability (or max(p, 1 - p) for yes/no), so it is not a second signal | docs |
| R5 | Usage is returned and output tokens are always 0 | docs |
| R6 | A decision is roughly 700 input tokens, about $0.00007 | estimate |
| R7 | Latency is 150 to 250 ms and flat in the number of questions | docs (launch post) |
| R8 | Hyphenated option keys such as `claude-code` are accepted | assumed |
| R9 | The parser accepts the real response with no rejected fields | unknown |
| R10 | The deterministic fallback agrees with the model on most sample tasks | unknown |

Check R4 directly from the data: for every answer, compare `confidence` with
the top probability.
