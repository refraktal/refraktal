# Refraktal guide

How to configure and use Refraktal, and what it does not protect against.
The specification, with the evidence for each decision, is
[DESIGN.md](DESIGN.md).

## Configuration

One JSON file, read only from your own config folder:
`~/Library/Application Support/refraktal/config.json` on macOS,
`~/.config/refraktal/config.json` on Linux (`$XDG_CONFIG_HOME` if set). A
missing file means the defaults. Unknown fields and bad values are errors, so
a typo never silently changes isolation. A repository cannot change how
Refraktal runs its children.

```json
{
  "harnesses": {
    "claude": {"mcp": "inherit"},
    "codex":  {"executable": "/usr/local/bin/codex", "pass_env": ["OPENAI_API_KEY"]}
  },
  "prices": {"some-model": {"input": 2, "output": 10, "cache_read": 0.2}},
  "checks": {"~/src/app": {"test": ["make", "test"], "timeout_mins": 5}},
  "judge": {"margin": 0.10}
}
```

| Setting | Meaning | Default |
|---|---|---|
| `harnesses.<name>.mcp` | `isolated` hides your MCP servers and plugins from that harness for each run; `inherit` uses them | `isolated` |
| `harnesses.<name>.executable` | the harness CLI the adapter runs instead of its bundled one (Claude, Codex), or the `agy` binary | bundled |
| `harnesses.<name>.pass_env` | provider API keys that harness may receive: `OPENAI_API_KEY`, `ANTHROPIC_API_KEY`, `GEMINI_API_KEY`, `GOOGLE_API_KEY` | none |
| `prices.<model>` | US dollars per million tokens, overriding the price table | the LiteLLM table |
| `checks.<repo path>` | the build and test commands judging runs in each candidate, as argv lists (no shell) | detected |
| `judge.margin` | pre-scores closer than this are a close call for the judge | 0.10 |

Harness names are `claude`, `codex`, `agy` and `refraktal`.

## Budgets

| Flag | Budget | Default |
|---|---|---|
| `--usd N` | dollars the run may charge. On subscriptions they are notional: a budget unit, not a bill. Checked before each turn, never inside one (no harness reports cost mid-turn), so a turn can overrun; the overrun is reported. | 10 |
| `--turns N` | prompts Refraktal sends to harnesses, across the whole tree (a child Refraktal's prompts count) | 20 |
| `--mins N` | wall-clock minutes for the whole run, setup included. The only hard stop inside a turn: when it runs out, the turn is cancelled, the harness shut down, and its process tree killed. Never unlimited; at most 1440. | 30 |
| `--depth N` | how many levels of Refraktal may nest below this run | 2 |

Each budget only shrinks down the tree. Parallel candidates split `--usd`
and `--turns`: each takes what is left divided by the children still to
start, and what it does not use comes back when it finishes. Costs are
tagged `reported` (the harness said so, as Claude does), `computed` (tokens
priced from the table, for Codex and Antigravity), `unpriced` (no price:
counts as $0, with a warning), `estimated` (a call a cancel cut off;
reporting only) or `retained` (money a child Refraktal's cancelled children
keep reserved: held against `--usd`, not spent). `refraktal show` lists them
by source.

## Routing and your data

Without `--harness`, a System 1 decision model picks the harness, and whether
to fan out:

- With `GEMINI_API_KEY` in the environment, or in the repository's `.env` when
  git confirms it is untracked, Refraktal asks Google's Gemini Decisions API
  (about half a second, two-second timeout, answers cached for 24 hours by a
  hash of the exact request). The key goes only in a request header, never in
  a URL, log or output.
- Otherwise, or if the call fails, local rules decide.
- On Google's free tier, Google uses request data to improve its products.
  Requests carry your task text, a short description of the repository
  (languages, size, whether it has tests) and, when judging, each candidate's
  diff (up to 32 KiB). `--harness` sends nothing.
- Every decision is logged to `.refraktal/decisions.jsonl` (gitignored), with
  its probabilities, for tuning.
- No harness, tool or check gets your API keys, unless `pass_env` opts one
  harness in to a named key.

## Fan-out and judging

`--harness a,b` runs those harnesses in parallel, one worktree each. Without
it, the router decides the width; `--fan N` caps it. With two or more results:

1. Each result must apply cleanly to your working tree. Then the repository's
   check runs in each candidate's worktree: from `checks` in the config, else
   detected from your `HEAD` only (a makefile `test:` target, `go.mod`, or a
   `package.json` test script). A candidate that changes the check's own
   definition or the tests is flagged: it can win only through the judge.
2. With a routing key, one Decisions call per candidate scores whether its
   diff does the task.
3. Only a close call goes to a judge: a harness that runs read-only, in its
   harness's read-only mode and under a policy that denies every edit,
   command and mode switch. A judge that changed its worktree anyway is not
   trusted.

`refraktal show` prints the verdict, including which harness judged and
whether it judged its own candidate.

## A child Refraktal

`refraktal run --harness refraktal` starts `refraktal acp` as a child, which
routes the task itself. Its budgets are the parent's grant (one grant for the
child's whole life, however many prompts it gets), its prompts count against
the parent's `--turns`, its run folder nests in the parent's, and `refraktal
show` renders the tree. `--depth` and a nesting guard stop recursion.

Any ACP client, such as an editor, can drive `refraktal acp [--usd --turns
--mins --depth]` the same way: each prompt is a run in the session's folder,
budgeted afresh from the flags. A session opened in a subfolder of a
repository stays there: a result that changes anything outside that folder
is refused. Only Refraktal's own summary and safe notices reach the client,
never a harness's error text, auth messages or local paths.

## Known limits

- **The permission policy is a gate, not a sandbox.** Refraktal sees a
  command's text, not everything it touches: a command built at run time (an
  interpreter's code, `$(...)`, a script the child wrote), a symlinked folder
  inside the worktree, or a redirection written without spaces is not seen
  through. The real boundaries are the separate worktree (applied back only
  through a checked `git apply`) and each harness's own sandbox and
  permission mode.
- **Checks run candidate code without a sandbox**, exactly as the candidate
  harness could during its turn: a check can write outside its worktree or use
  the network. Containers are not in v1.
- **Codex children may write to `/tmp`**: codex-acp sends its own sandbox
  policy on every turn, which cannot be narrowed per run.
- **Your own Claude hooks run inside Claude children**, and a repository's
  harness hooks run too (Refraktal prints a notice when a repository has
  them); they run outside Refraktal's permission policy.
- **A process that calls `setsid()` itself escapes the session sweep**, so it
  can outlive a run. Everything else a harness starts is tracked and killed.
- **Two Refraktal runs on one repository at the same time** are not
  serialised against each other.
- **macOS binaries are not signed or notarized.** The install script and
  Homebrew do not mark them quarantined; a browser download would be.
