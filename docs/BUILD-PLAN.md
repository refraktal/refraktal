# v1 build plan

Milestones in order. Each one is a pull request that leaves `main` working.
`docs/DESIGN.md` is the specification; where this plan and the design
disagree, the design wins and the plan gets fixed.

**Working rules for every milestone**

- Behaviour is tested against the fake ACP agent (moved from
  `spikes/acp/cmd/fakeagent`), never against a live harness, in `go test`.
- Live checks are scripts that write to gitignored folders, like the spike
  rounds. Summarise their results in the PR; never commit raw output.
- A milestone's **Open test** (from `DESIGN.md`) runs at the end of that
  milestone. If the result contradicts the design, stop and report before
  building on it.
- Lift code from `spikes/` where it fits, but rewrite it to the product's
  standard: smaller, tested, no spike-only flags.

## Package layout (proposed)

| Package | Holds |
|---|---|
| `refraktal` (root) | The library API: `Run`, options, results. The CLI is a thin front door over it |
| `cmd/refraktal` | Flags and the four commands |
| `internal/acp` | JSON-RPC over stdio, the ACP client, cancel, usage capture |
| `internal/acp/acptest` | The fake agent, for tests |
| `internal/proc` | Spawning, environment scrubbing, process-tree tracking and identity-checked kill, leftover cleanup |
| `internal/harness` | Per-harness facts: launch command, pinned adapter, mode, MCP isolation, version detection |
| `internal/policy` | Answers permission requests: worktree rule, deny-list, logging |
| `internal/budget` | The four budgets, reservations, cost sources |
| `internal/price` | LiteLLM price table: fetch, cache, overrides |
| `internal/runlog` | Run folder, JSONL events, redaction |
| `internal/worktree` | Git worktrees: create, diff, apply, prune |
| `internal/route` | `Decider`, Gemini client, fallback, policy, cache |
| `internal/judge` | Scoring and choosing a winner |

## M0. Groundwork

- `CLAUDE.md` and these docs (this PR).
- CI on pull requests and `main`: `gofmt -l`, `go vet`, `go test -race`, for
  the root module and both spike modules. Do not touch the publish workflows.
- Raise `go.mod` to the Go version the spikes already use (1.24).

**Done when:** CI runs green on a trivial PR.

## M1. ACP client and process control

- `internal/acp` from the spike client, without the client-side file policy.
- `internal/proc`: process-tree tracking by parent PID; kill by tracked PID
  with a command-line and start-time identity check; process-group kill as
  well; environment scrubbing.
- `internal/runlog`: JSONL, redaction allowlist, size-and-hash for large
  updates.
- Fake agent in `acptest`, including a child that escapes its process group.

**Done when:** fake-agent tests cover a full turn, cancel, kill on timeout, a
detached child killed by the tree kill, redaction, and a PID-reuse case that
must not be killed.

**Open tests at the end:**
- A live kill of a surviving tool: a prompt that starts `sleep 120 &`, then
  shutdown. Record whether it was tracked and killed.
- Antigravity's orphan: repeat the mid-tool cancel three times; count
  survivors.

## M2. `doctor`

- Which harnesses are installed; adapter pins; bundled and installed versions.
- Login check by a zero-token handshake up to `session/new`, per harness.
- MCP isolation status per harness (`isolated`, `inherit`, `unavailable` for
  Codex until solved).
- Decider: key present, or deterministic fallback.
- Clean up leftover processes from dead runs.
- `--json`.

**Done when:** `doctor` gives a correct report on a machine with all three
harnesses logged in, and exits nonzero when nothing is usable.

## M3. `run` with one harness

- `refraktal run --harness <one> "<task>"`: worktree (never under a temp
  folder), child environment (scrubbed; private `TMPDIR` for Codex), mode set
  per `DESIGN.md`, MCP isolation, permission policy, run folder.
- Apply the child's result to the working tree; `--no-apply` leaves the
  worktree and prints its branch. Exit codes.

**Done when:** one real task completes end to end on each harness, from a
script, with the run folder redacted.

**Open tests at the end:**
- The reject path: deny `touch marker.txt` in each harness's gated mode;
  confirm the file is not created and record how each harness reports it.
- Codex private `TMPDIR` and `exclude_slash_tmp` set per run.
- Codex MCP isolation: look for a per-run switch that keeps the login.

## M4. Budgets

- `--usd`, `--turns`, `--mins`, `--depth` with defaults; cost sources;
  LiteLLM price table with cache and overrides; cancel, shut down and tree-kill
  on `--mins`; reservation accounting (single child for now).
- Exit code 2 on exhaustion.

**Done when:** fake-agent tests cover each budget and the cancelled-turn
accounting rule, and a live script shows `--mins` stopping a long tool turn on
each harness with no surviving process.

## M5. Routing

- `internal/route` from the spike: Gemini decider, fallback, policy, decision
  log, 2 s timeout, request cache.
- `run` without `--harness` routes to one harness. Fan-out decisions fall back
  to the top harness until M6; escalations are reported.
- `--dry` prints the plan and spends nothing on harnesses.

**Done when:** the spike's sample tasks route identically through the product,
and `--dry` works with and without a key.

## M6. Fan-out and judging

- Parallel children in separate worktrees; reservation accounting across
  them; `--fan N` and `--harness a,b`.
- Judge: score candidates (checks such as tests passing first, then a System 1
  rubric pre-score, then a harness run as judge only for close calls); pick a
  winner or report none (exit 1).

**Decide before building:** whether fan-out means different harnesses (the
spike's default) or several attempts on one; the judging rubric.

## M7. `acp` and `show`

- `refraktal acp`: Refraktal as an ACP agent, budgets from flags, depth
  decremented, nested run folders.
- `show`: render a run, including nested runs, costs by source, decisions and
  verdicts.

**Done when:** a Refraktal drives a child Refraktal that drives a harness, and
`show` renders the whole tree.

## M8. Release

GoReleaser, Homebrew tap, install script at `refraktal.dev/install`, npm and
PyPI wrappers that fetch the binary. Uses the existing trusted-publishing
workflows; their filenames must not change.
