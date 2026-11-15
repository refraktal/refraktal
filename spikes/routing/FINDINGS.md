# Routing spike: findings

Spike date: 2026-11-15. Question: can one batched decision call pick the harness and decide
route versus fan-out for a task, and what does that code look like?

**Short answer: yes.** The Gemini Decisions API is real, documented, and its shape fits this
use almost exactly. Five questions go in one POST. Nothing has been run against the live API
yet (no key here), so every statement about answer quality is still open.

## 1. What the API really is

### Sources (all fetched 2026-11-15)

| What | URL |
|---|---|
| API guide and reference (primary) | https://aistudio.google.com/docs/decisions-api (raw markdown: add `.md`) |
| Model page | https://aistudio.google.com/docs/models/gemini-decision-1 |
| Pricing | https://aistudio.google.com/docs/pricing#gemini-decision |
| Rate limits | https://aistudio.google.com/docs/rate-limits#gemini-decision |
| Launch post, 2026-11-11 | https://blog.google/innovation-and-ai/technology/developers-tools/gemini-decisions-api/ |
| Docs index | https://aistudio.google.com/llms.txt |

`ai.google.dev/gemini-api/docs/...` now redirects to `aistudio.google.com/docs/...`; the guessed
URL `ai.google.dev/gemini-api/docs/decisions` does not exist.

### Confirmed from the official docs

- **Endpoint:** `POST https://generativelanguage.googleapis.com/v1beta/decisions`
- **Auth:** header `x-goog-api-key: $GEMINI_API_KEY`, plus `Content-Type: application/json`.
- **Model id:** `gemini-decision-1` (the only model the endpoint accepts).
- **Request:** `model`, `input` (a string, or content blocks such as `{"type":"text","text":...}`),
  and `questions`, a map from your own key to `{type, instructions, criteria}`.
  1 to 1,000 questions per request. Keys match `[A-Za-z0-9_-]+`, up to 64 characters.
- **Three question types:**
  - `noul` (yes/no). Optional criteria `{true_description, false_description}`.
    Answer: `{"type":"noul","noul":0.94,"confidence":0.94}`. `noul` is P(yes), always within 0.01 to 0.99.
  - `choice`. Criteria is a map of option key to description (2 to 255 options).
    Answer: `choice` (top key), `probabilities` (per option, sum to 1), `confidence`.
  - `score`. Criteria is an ordered array of level descriptions (2 to 255, indexed from 0).
    Answer: `score` (probability-weighted level index), `legend`, `probabilities` (per level), `confidence`.
- **Response:** `answers` (same keys as `questions`) and `usage`
  (`total_input_tokens`, `input_tokens_by_modality`, `total_output_tokens` always 0, `total_tokens`).
- **Several questions in one request:** yes, mixed types, and the shared input is billed once.
- **Errors:** safety blocks and limit or validation failures return HTTP 400 with
  `{"error":{"code":"safety","message":...}}`.
- **Limits:** 262,144-token context shared by input and all questions; option key up to 128 characters.
- **Price:** $0.10 per 1M input tokens on the paid tier, free on the free tier, no output charge.
- **Rate limits:** free tier 200 RPM / 50,000 TPM / 10,000 RPD; tier 1 1,500 RPM / 1M TPM / 50,000 RPD.
- **Latency (launch post):** 150 to 250 ms end to end, 50 to 100 ms median model compute, "nearly flat
  from 1 to 100 questions", for contexts up to 16K tokens.
- **Not supported:** context caching, Batch API, Flex or Priority inference, conversation history,
  Vertex AI (the launch post has a "coming soon" form for Google Cloud).
- **Stability:** served on `v1beta` only. The docs carry no preview, GA or deprecation statement.

### Things the docs say that change the design

These were not in the project notes and matter more than the endpoint details.

1. **Confidence is not a second signal.** The docs define it as the top outcome's probability, or
   `max(noul, 1 - noul)` for yes/no. "A calibrated probability plus a confidence per answer" is one
   number seen twice. The `Answer` type keeps both fields because the brief asks for them, but the
   policy can only ever threshold one thing per question.
2. **Questions cannot see each other.** Each is scored independently against the shared input. So
   "fan-out width, if fanning out" cannot be a conditional question. The docs' own advice is to ask
   both and ignore the one you do not need, which is what this spike does.
3. **The model never sees question keys**, only instructions and option keys. Every instruction has
   to be a complete question.
4. **A choice question always spreads probability over the listed options, even when none fits.**
   Low confidence is the only tell. This is the reason the policy has an "escalate" floor.
5. **Option keys are sorted alphabetically before scoring**, and exact ties break to the first key.
   Harmless here, but it rules out positional catch-alls.
6. **Input is untrusted.** A task description can argue for its own routing ("this needs four
   agents"). The docs say to keep a second check on high-stakes actions. For Refraktal the stake
   is spend, and the budget flags already cap that.
7. **Compare numbers in code and pass a named bucket.** So the request carries `repo.size`
   ("tiny" to "large"), not the raw file count.

### Observed live (one unauthenticated request, as allowed)

`POST` to the real endpoint with no key returned **HTTP 403** in 0.24 s:

```json
{"error":{"code":403,"message":"Method doesn't allow unregistered callers (callers without established identity). Please use API Key or other form of API consumer identity to call this API.","status":"PERMISSION_DENIED"}}
```

This confirms the endpoint exists. It also shows that `error.code` is a **number** on Google's
generic errors but a **string** (`"safety"`) on the documented Decisions errors, so the client
keeps it as raw JSON. Saved as `testdata/unauthenticated_error.json`.

### Not verified

- Any real answer, latency or token count for these questions. No authenticated call was made.
- Whether a 200 response can omit an answer for some question. The code treats that as malformed
  and falls back.
- 429 and 5xx body shapes for this endpoint. The code needs only the status code.
- How well calibrated the probabilities are for coding-task routing. Google's calibration claims
  are about its own benchmarks.
- OpenAI Decisions, TypeSafe Jev and Vercel AI Gateway as alternatives: **not researched**. A search
  result shows Vercel lists `gemini-decision-1` on AI Gateway; I did not read the page.

## 2. The request, and why it is one call

`Gemini.Decide` makes exactly one `POST`. The body for
`route -request -langs go -files 140 -tests "Fix the off-by-one error in pagination"`:

```json
{
  "model": "gemini-decision-1",
  "input": "{\"repo\":{\"has_tests\":true,\"languages\":[\"go\"],\"size\":\"small\"},\"task\":\"Fix the off-by-one error in pagination\"}",
  "questions": {
    "fan_out": {
      "type": "noul",
      "instructions": "Should several coding agents attempt `task` independently in parallel, so the best result can be picked, rather than one agent attempting it once?",
      "criteria": {
        "false_description": "The task is clear and one correct attempt is the likely outcome.",
        "true_description": "The task is ambiguous, open to materially different valid solutions, or risky enough that comparing attempts is worth the extra cost."
      }
    },
    "fan_out_width": {
      "type": "score",
      "instructions": "If `task` were attempted by several coding agents in parallel, how many independent attempts would be worth their cost?",
      "criteria": [
        "Two attempts: one second opinion is enough.",
        "Three attempts: several plausible approaches are worth comparing.",
        "Four attempts: the task is highly ambiguous or high-stakes and wide exploration is warranted."
      ]
    },
    "harness": {
      "type": "choice",
      "instructions": "Which coding agent is the best single choice to carry out `task` in the repository described by `repo`?",
      "criteria": {
        "antigravity": "Front-end, UI, visual or browser-verified work, and work on Google-stack projects.",
        "claude-code": "Changes that span many files or need a lot of existing code read and kept consistent: refactors, migrations, investigations, loosely specified work.",
        "codex": "Well-specified, self-contained changes with a clear check: a localized bug fix, a small feature, adding tests, a documentation edit.",
        "refraktal": "Work that is really several separable tasks, best split up and delegated to several agents by an orchestrator."
      }
    },
    "needs_decomposition": {
      "type": "noul",
      "instructions": "Does `task` need to be broken into several separate subtasks before a coding agent starts work?",
      "criteria": {
        "false_description": "The task is one coherent change, even if it touches many files.",
        "true_description": "The task bundles several distinct deliverables or spans independent areas that would each be a separate change."
      }
    },
    "task_kind": {
      "type": "choice",
      "instructions": "`task` is a request to a coding agent working in the repository described by `repo`. What kind of work is `task` mainly asking for?",
      "criteria": {
        "bugfix": "Fix behaviour that is wrong, crashing or failing.",
        "docs": "Write or update documentation, comments or a README.",
        "feature": "Add new behaviour or a new capability.",
        "investigation": "Find out why something happens, measure, profile or explain, where what to change is not yet known.",
        "refactor": "Restructure, rename, migrate or clean up code without changing what it does.",
        "test": "Write or improve automated tests."
      }
    }
  }
}
```

Print it for any task with `go run ./cmd/route -request "<task>"` (no call is made).

It is one call because the API takes a map of questions over one shared input and answers them in
one pass, and because none of our questions needs another's answer first. The only dependent pair
(width depends on fan-out) is handled by asking both and ignoring width when routing. The test
`TestGeminiDecideOneRequest` asserts the server sees one request with five questions.

Size: 2.7 KB of JSON. My rough estimate is 600 to 700 input tokens (the docs' three-question example
is 324). That is an estimate; `usage.total_input_tokens` from the live run will give the real figure.

## 3. Question wording

Five questions, in `questions.go`:

| Key | Type | Asks | Maps to |
|---|---|---|---|
| `task_kind` | choice, 6 options | What kind of work is `task` mainly asking for? | `Decision.TaskKind` |
| `harness` | choice, 4 options | Which coding agent is the best single choice for `task` in `repo`? | `Decision.Harness` |
| `fan_out` | noul | Should several agents attempt `task` in parallel rather than one, once? | `Decision.FanOut` (`route` or `fan_out`) |
| `fan_out_width` | score, 3 levels | If attempted in parallel, how many attempts are worth their cost? | level 0, 1, 2 becomes width 2, 3, 4 |
| `needs_decomposition` | noul | Does `task` need breaking into separate subtasks first? | `Decision.NeedsDecomposition` |

Choices made:

- Every option and level has a one-line description, as the docs recommend.
- The yes/no questions are phrased positively and carry `true_description` and `false_description`,
  because "worth fanning out" is ambiguous on its own.
- Width is a `score` (an ordered rubric) rather than a `choice`, so the expected value is meaningful.
  The discrete width is the most probable level, which is what the docs advise.
- **The harness descriptions are my assumptions**, not measurements: claude-code for wide or loosely
  specified work, codex for well-specified changes with a clear check, antigravity for front-end and
  Google-stack work, refraktal for work that is really several tasks. The model knows nothing about
  these products except those four sentences. **The harness answer can only be as good as these
  descriptions.** This is the weakest part of the design and the first thing the log should correct.
- No "other" option on either choice. For `harness` an "other" has no action; low confidence already
  leads to escalation.

## 4. Thresholds and reasoning

In `policy.go`, as a `Thresholds` struct with `DefaultThresholds`. `Policy` is a pure function.

| Name | Value | Why |
|---|---|---|
| `RouteConfidence` | 0.70 | The docs' own routing example uses 0.7. Over four options the winner then has more than twice the mass of the rest combined. |
| `WideFanOutConfidence` | 0.55 | Midpoint. Above it the runner-up is a second opinion (width 2); below it, width 3. |
| `EscalateConfidence` | 0.40 | Chance over four harnesses is 0.25. Under 0.40 the top option is close to a guess, and since a choice question always picks something, this is the "question did not fit" signal. |
| `FanOutProbability` | 0.70 | Act on "fan out" only when it is as sure as a routing answer must be. A false yes costs extra harness runs. |
| `DecomposeProbability` | 0.70 | Same bar. A false yes costs a System 2 planning pass. |

Order of evaluation:

1. Harness confidence under 0.40: **escalate** to the System 2 planner.
2. P(needs decomposition) at or over 0.70: **escalate**.
3. P(fan out) at or over 0.70: **fan out**, using the model's width.
4. Harness confidence at or over 0.70: **route** to the one harness.
5. Otherwise **fan out**: width 2 if confidence is at least 0.55, else 3.

Fan-out picks the N most probable harnesses, chosen one first.

These are starting values with no data behind them. The docs say to set thresholds from labelled
examples of your own traffic. The decision log exists for that.

## 5. Cost and latency per decision

| | Value | Source |
|---|---|---|
| Price | $0.10 per 1M input tokens, output free | official pricing page |
| Tokens per decision | about 600 to 700 | **my estimate** from request size |
| Cost per decision | about $0.00007, so roughly 14,000 decisions per dollar | computed from the two rows above |
| Latency | 150 to 250 ms end to end | Google's launch post, not measured |
| Client timeout | 2 s | my choice, see `gemini.go` |
| Fallback latency | about 0.02 ms | measured here |

The API returns tokens, not dollars. `Usage.CostUSD` is computed from list price and tagged
`cost_source: "computed"`, in line with the project's reported, computed, unpriced convention.
No caching exists, so the 2 KB of fixed question text is paid on every call. That is about 80%
of each request. It is still a rounding error next to one harness run.

## 6. The fallback on the sample tasks

`go run ./cmd/route -fallback -log "" -samples samples/tasks.json`. The EXPECTED column is my own
label of the right kind and action. `(!)` marks a mismatch.

```
ID                      DECIDER   KIND                HARNESS           P(FAN)  WIDTH  P(DECOMP)  POLICY        HARNESSES    MS   EXPECTED
one-line-bugfix         fallback  bugfix 0.80         codex 0.75        0.15    2      0.10       route         codex        0.0  bugfix/route
cross-cutting-refactor  fallback  refactor 0.80       refraktal 0.60    0.60    2      0.80       escalate                   0.0  refactor/escalate
add-tests               fallback  test 0.80           codex 0.75        0.15    2      0.10       route         codex        0.0  test/route
ambiguous-faster        fallback  feature 0.30 (!)    claude-code 0.35  0.75    3      0.10       escalate                   0.0  investigation/escalate
docs                    fallback  docs 0.80           codex 0.70        0.15    2      0.10       route         codex        0.0  docs/route
ui-feature              fallback  feature 0.80        antigravity 0.70  0.15    2      0.10       route         antigravity  0.0  feature/route
investigation           fallback  investigation 0.80  claude-code 0.70  0.15    2      0.10       route         claude-code  0.0  investigation/route
multi-part-feature      fallback  feature 0.50        refraktal 0.60    0.15    2      0.80       escalate                   0.0  feature/escalate
open-design             fallback  feature 0.30        claude-code 0.35  0.75    2      0.10       escalate (!)               0.0  feature/fan_out
no-tests-bugfix         fallback  bugfix 0.80         codex 0.70        0.15    2      0.10       route         codex        0.0  bugfix/route
```

**Right (8 of 10 on both labels):** the clear cases. A bug with a failure word routes to codex,
tests and docs route to codex, a UI task goes to antigravity, an investigation to claude-code,
and both bundled tasks are caught as needing decomposition.

**Wrong or lucky:**

- `ambiguous-faster`: the kind is wrong ("feature" at 0.30, a default). The action is right only
  because knowing nothing leads to escalation.
- `open-design`: I wanted a fan-out; it escalates. The rules see "improve" as vague but find no task
  kind, so harness confidence is 0.35 and the floor wins. Defensible, but not what I labelled.
- `cross-cutting-refactor`: right action for a shaky reason. It is flagged as multi-part because of
  two "and"s, not because the rules understand a migration.
- While building this, "last page of results" matched a UI keyword and sent a pagination bug to
  antigravity. I removed that keyword. It is a fair picture of how brittle keyword rules are.

**The fallback's numbers are not probabilities.** They are four fixed levels (0.80, 0.70, 0.50,
0.30) chosen so the same policy runs over them. The log records `decider: "fallback"` so the tuning
work can exclude or separately treat those rows. Ten samples labelled by the person who wrote the
rules prove nothing about accuracy.

## 7. What was built

| File | Purpose |
|---|---|
| `decision.go` | `Input`, `Answer`, `Decision`, the `Decider` interface, `WithFallback` |
| `questions.go` | The shared input and the five questions (provider-neutral wording, Gemini's three types) |
| `gemini.go` | Everything Gemini-specific. `net/http` and `encoding/json` only |
| `fallback.go` | Deterministic keyword and repo-shape rules |
| `policy.go` | `Thresholds` and the pure `Policy` function |
| `record.go` | The JSONL decision record |
| `cmd/route` | CLI: task from arguments or stdin, JSON on stdout; `-samples` prints the table |
| `cmd/summarize` | Decision log to TSV, so `run-live.sh` needs no `jq` |
| `testdata/` | The documented request and response pair, the real 403 body, one synthetic routing response |

The decision record (`schema: 1`) holds: id, UTC timestamp, task, repo context, the prompt input as
sent, the full question set, each answer with probability, confidence and the full distribution,
decider, model, fallback reason, latency, usage and computed cost, the thresholds in force, the
policy outcome, and the provider's raw response body. Verified outcomes are meant to be joined on
later by `id`.

Size: the library is 559 lines of code (718 with comments), plus 137 for the two commands.
**That misses the "well under 400" aim.** The excess is question text (64 lines), strict response
validation in `gemini.go`, and the fallback rules. No dependencies outside the standard library.

## 8. Open questions, and what `run-live.sh` settles

`GEMINI_API_KEY=... ./run-live.sh` sends each of the 10 samples once and writes
`results/<timestamp>/decisions.jsonl`, `table.txt` and `latency.tsv`. It refuses to run without a
key and exits non-zero if any task fell back, so fallback rows cannot pass as live ones.

It will settle:

1. Whether the request is accepted as built (field names, hyphenated option keys, noul criteria).
2. Real latency from Pune, including the cold TLS handshake a one-shot CLI pays every time. If the
   first call is far over 250 ms, the 2 s timeout and the "150 to 250 ms" planning figure need revisiting.
3. Real token count and cost per decision.
4. Whether the answers look sane on the ten tasks, and how often confidence lands in each band.
   If almost everything is over 0.9 or under 0.4, the thresholds are in the wrong place.
5. Whether `fan_out` and `fan_out_width` ever say anything other than "route, 2".

It will not settle:

- **Whether the harness choice is right.** That needs outcomes: run the task on several harnesses
  and see which wins. Until then the harness question measures agreement with my four sentences.
- Whether fan-out should mean N different harnesses (what the policy does now) or N attempts on one
  harness. The width question does not distinguish them.
- Whether the harness question should be one `choice` or four independent `noul` questions ("would
  codex do this well?"). The second form gives absolute scores and a natural fan-out set, and costs
  almost nothing extra given flat latency. Worth an A/B once there is a key.
- Whether to add `refraktal` as a harness option at all, given `needs_decomposition` asks nearly the
  same thing. The two may be redundant.
- Repeatability: does the same request return the same probabilities every time? One run per task
  cannot tell. Run the script twice and diff.
- Data use: on the free tier Google may use inputs to improve its products (reported by a secondary
  source, flaviocopes.com; I did not confirm it on Google's terms page). Task descriptions from
  private repos should go through a paid-tier key until that is checked.
- The alternative providers behind the `Decider` interface.
