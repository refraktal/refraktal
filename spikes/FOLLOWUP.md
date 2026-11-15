# Follow-up live tests: brief for Claude Code

The first live run (`LIVE-RESULTS.md`) left four questions open. This brief
runs one cheap test for each, plus one for how Claude's cost accumulates.
Write the answers to `spikes/FOLLOWUP-RESULTS.md`.

## Rules (same as the first run)

- Raw output in `results/` is gitignored on purpose. **This repo is public.**
  Raw results contain account details, local paths and session data. Do not
  commit, paste or upload them. Summarise in `FOLLOWUP-RESULTS.md` instead.
- Process command lines (`procs_live`, `survivors`) contain local paths. Report
  only which binary and version ran, with the home directory replaced by `~`.
- Never print, echo or write an API key. If `GEMINI_API_KEY` is not set, stop
  and ask Adhish to export it in his own shell.
- Do not change prompts, time limits or harnesses. Never invent a result: if a
  step did not run, say so and why.
- Show Adhish the diff before pushing.

## Steps

1. `git pull`, then the offline checks; stop if they fail:
   ```
   cd spikes/acp && go vet ./... && go test ./... && HARNESSES=fake bash followup.sh all
   cd ../routing && go vet ./... && go test ./...
   ```
2. ACP, in this order. Run from a plain terminal if a Claude run fails with an
   authentication or nesting error, as last time.
   ```
   cd spikes/acp
   bash followup.sh exit           # no prompt, 0 tokens
   bash followup.sh cancel         # 3 partial tool turns
   bash followup.sh agy-perms      # 3 short Antigravity turns
   bash followup.sh claude-turns   # 2 plain Claude turns in one session
   ```
3. Routing latency. About 21 calls and roughly 90k input tokens in total,
   under one cent; about three minutes because of the pause between calls.
   ```
   cd spikes/routing && mkdir -p results
   go run ./cmd/latency -out results/latency.jsonl | tee results/latency.txt
   ```
   If it stops on HTTP 429, re-run once with `-delay 15s`. Do not retry more.
4. Optional, for repeatability: run `bash run-live.sh` in `spikes/routing` two
   more times (20 calls, a fraction of a cent).
5. Write `spikes/FOLLOWUP-RESULTS.md` answering every question below, then set
   the **Live result** column in `spikes/CLAIMS.md` for A14, A17 and R7.

Each ACP run writes `results/followup-<test>-<harness>.jsonl` (every line),
`.summary.json` (the last line), `.stderr`, and `.files` (exit code and files
left in the temp directory). New summary fields: `mode_set`, `turns_done`,
`turn_wall_ms`, `turn_stop_reasons`, `cancel_at_ms`, `stop_after_cancel_ms`,
`procs_live`, `survivors`, `leader_exited`. Each `usage` entry now has `turn`.

## Questions

### Cancel (`followup-cancel-*`), per harness

- **F1.** Did the harness answer `session/cancel` with a prompt response, or
  was it killed after the 15 s grace (`killed`)?
- **F2.** What stop reason did it return? The spec says `cancelled`.
- **F3.** How long from cancel to response (`stop_after_cancel_ms`)?
- **F4.** Did any usage arrive for the partial turn, as `usage_update` or on
  the prompt response? Before or after the cancel (`t_ms` vs `cancel_at_ms`)?
- **F5.** Claude only: was a cost reported for the cancelled turn, and how
  much?
- **F6.** Was `hello.txt` written anyway (`.files`)? Did any permission request
  arrive after the cancel, and was it answered `cancelled`?

### Shutdown (`followup-exit-*`, also `followup-cancel-*`)

- **A17.** After stdin closed, did the spawned process exit on its own
  (`leader_exited`)? Was anything left in the process group (`survivors`)?
  If so, what (binary name only)? Answer for both the handshake runs and the
  cancel runs, since a busy harness may behave differently from an idle one.

### Which binary runs (`procs_live` in `followup-cancel-*`, taken mid-turn)

- **B1.** For Claude and Codex: does the process group contain the adapter's
  bundled harness binary or the CLI installed on the machine? Give the version
  if the path shows it. This replaces the inference in `LIVE-RESULTS.md`.

### Antigravity permissions (`followup-perms-*`)

- **A14.** For each of the three runs (read in `default`, echo in `default`,
  echo in `auto`): how many permission requests reached the client, and for
  which tool kinds (`params.toolCall.kind` on the `request` lines)? Did the
  reply contain the expected output (`refraktal-check-7`, `hi`)?
  Verdict: does Antigravity decide some actions by its own rules without asking
  the client, and does `auto` change that?

### Cost across turns (`followup-turns-claude`)

- **C1.** Turn 1 and turn 2 cost: is the second figure cumulative for the
  session (about turn 1 + turn 2) or for that turn alone?
- **C2.** Same question for the token `usage` on each prompt response.

### Latency (`routing/results/latency.*`)

- **R7.** The median, min and max latency for 1, 5, 25 and 100 questions, with
  input tokens for each. Is latency flat in the number of questions, or does it
  grow? Were any calls rejected?
- **R11** (only if step 4 ran). Across the three runs, did any task get a
  different policy action, harness or task kind?

Finish by telling Adhish in a few lines which answers change a decision in the
project's `SPIKES.md` design list, especially items 1 (`--usd` between turns,
`--mins` as the hard stop), 6 (permission modes) and 11 (routing latency).
