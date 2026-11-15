# Spikes: ACP and routing

Two throwaway spikes for Refraktal. Each is its own Go module (standard library
only) and is not part of the root module. The findings so far, as a checklist,
are in `CLAIMS.md` in this folder.

- `acp/`: a small ACP client that drives Claude Code, Codex and Antigravity
  over stdio and records usage and cost updates. See `acp/FINDINGS.md`.
- `routing/`: one batched Gemini Decisions API call that picks a harness and
  route vs fan-out, with a deterministic fallback. See `routing/FINDINGS.md`.

Both pass `go vet ./...` and `go test ./...`. **Neither has had a live run yet.**
That is the job below.

## Live run: brief for Claude Code

You are running on Adhish's laptop, where Claude Code, Codex and Antigravity
are logged in. Run both spikes live and report what was actually observed.

### Rules

- Raw output in `results/` is gitignored on purpose. **This repo is public.**
  Raw results can contain account details, local paths and session data. Do not
  commit, paste or upload them anywhere.
- Never print, echo or write an API key. If `GEMINI_API_KEY` is not set, stop
  and ask Adhish to export it in his own shell; do not ask him to paste it.
- Do not change the prompts, raise the time limits or add harnesses. The
  scripts are deliberately cheap: trivial prompts, temp directories, 2 minutes.
- Never invent a result. If a step did not run, say so and say why.
- Do not push anything without showing Adhish the diff first.

### Steps

1. Check the tools: `go version`, `node --version`, and which of `claude`,
   `codex`, `agy` are on PATH. Report what is missing; do not install harnesses.
2. Offline self-test first:
   `cd spikes/acp && go vet ./... && go test ./... && HARNESSES=fake bash run-live.sh`
   and `cd spikes/routing && go vet ./... && go test ./...`. Stop if these fail.
3. ACP live run: `cd spikes/acp && bash run-live.sh --with-tool`.
   It sends each installed harness one trivial prompt, then one prompt that
   writes a single file in a temp directory. Adapters are installed with npm
   into `acp/adapters` (never globally).
   - If the Claude Code run fails with an authentication or nesting error, it
     is probably because it was started from inside your own session. Do not
     work around it: ask Adhish to run that one command in a plain terminal
     with `HARNESSES=claude bash run-live.sh --with-tool`.
4. Routing live run: `cd spikes/routing && bash run-live.sh`
   (10 calls, well under one cent). It refuses to run without the key.
5. Read `results/*.summary.json` in `acp/` and the newest folder under
   `routing/results/`, then write `spikes/LIVE-RESULTS.md` with the answers
   below. Summarise; do not copy raw output. Leave out anything that
   identifies the account or machine.

### What `LIVE-RESULTS.md` must answer

Versions of each harness and adapter used, then for ACP, per harness:

1. How far did it get: initialize, session/new, prompt completed? Stop reason?
2. Did it send `usage_update`? How many, and at what `t_ms`?
3. Did any carry a `cost`? What currency? Only on the last update, or earlier?
4. Was the first usage observation mid-turn or only at the end
   (`first_usage` in the summary)?
5. Did the prompt response itself carry usage?
6. With `--with-tool`: did a permission request reach the client? What mode
   did the session start in? Was the file written?
7. On a subscription login, is a dollar figure reported at all?
8. Anything unexpected: extra notifications, slow shutdown, errors.

For routing:

1. Did all 10 calls succeed against the real API, with none falling back?
2. Was the response shape the documented one? Any field our parser rejected?
3. Latency per call, including the first (cold) one. Input tokens and computed
   cost per call.
4. A table of the model's answers next to the fallback's for the same task,
   with the policy outcome for each.
5. Where the model and the `EXPECTED` column disagree, list those rows.

Then go through `spikes/CLAIMS.md` and mark every claim confirmed,
contradicted or still unknown, with the observation that decides it. That
file is the complete list; there is no other document to look for.

Finish by telling Adhish, in a few lines, which claims are now confirmed,
which are contradicted, and which are still unknown.
