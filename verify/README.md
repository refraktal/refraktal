# Bounded model checks

Run the checks without network or toolchain downloads:

```sh
GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off go test -race ./verify -count=1 -v -timeout=60s
```

All checker and model sources are `_test.go` files. Nothing is added to the
shipped binary. `TestProductionDoesNotImportVerify` checks the transitive
production dependency graphs of the root package and `cmd/refraktal`, and
rejects non-test Go source in this directory. The implementation uses only the
Go standard library. The existing CI's `go test -race ./...` includes these tests.

## What the checker establishes

The checker performs breadth-first search, deduplicating complete canonical
state keys. Each model uses value structs and arrays, serialized in field order;
there are no hash collisions, map-order dependencies, or shared mutable states.
Search has no depth or state-count cutoff. It exhausts the finite reachable
graph or stops at the first invariant violation. Predecessors reconstruct a
shortest trace by transition count, numbered from the initial state at step 0.

Verbose test output reports discovered states, maximum shortest-path depth, and
elapsed search time. Counts on a counterexample describe only the explored
prefix. Checker unit tests cover cycles, deduplication, shortest traces and an
invalid initial state. Reachability witnesses prevent the passing model runs
from silently excluding successful work or relevant boundary states.

These are **bounded checks**, not proofs for arbitrary inputs or all OS behavior.
The bounds are sufficient to expose the particular races and removed guards
below; there is no claimed cutoff theorem extending passing results to larger
systems. Every transition names the production function and file it mirrors.

## Models and bounds

| Model | Bounds | Why these bounds are useful |
| --- | --- | --- |
| Kill safety | Three PID slots: pinned harness, recorded tool-session leader, member; two birth identities; reusable group/session IDs | Covers descendants, PID reuse, tracker polls, verified group continuity/retirement, group broadcast and per-target checks. A replacement session leader can fork and exit, leaving an unrelated member. |
| Budget ledger | Two concurrent candidate grants plus a later judge grant; four USD units; four turns; per-turn costs `{0,1,3}` | Covers overlapping reservations, returned shares, multiple cumulative reports, cancellation, duplicate settlement and final-turn overrun. Separate accepted-input exploration permits an oversized nested turn report. |
| Run lifecycle | One or two candidates; one optional judge; one collect/check result per candidate; two patch files; monotonic cancel/deadline | Two children expose a missing stop/sweep barrier; two files expose partial apply. Includes no-apply, failed collection, judge, admission guards and cancellation/deadline during apply. |
| Nesting | Initial nest 0–3 plus invalid/negative values; depth 0–3; inherited ceilings −1–4 plus absent/malformed values; child flags 0–3 | Covers equality/refusal boundaries, ceiling lowering, shrinking grants, lost grants with reset flags, and malformed grants. At most four runs are admitted in a modeled chain. |

Kill safety is false in the faithful model. Separate, explicitly restricted
runs assume atomic identity-check/signal and exclude unobserved session reuse
to isolate guard mutations. The harness PID/group/session remain pinned until
cleanup. Birth times are distinct; the accepted one-second `ps` identity alias,
lookup failures, arbitrary process trees, and platform fallback reaping are not
proved safe.

The passing budget exploration assumes truthful nested reports within the
passed allowance and nondecreasing cumulative reports. It checks conservation,
turn allowances, cancellation retention and exactly one settlement in every
terminal state. There is no fairness/liveness claim for a child that never
returns. Integer amounts avoid floating-point rounding; fractional grants,
arbitrary fan-out and decreasing cumulative reports are outside these bounds.

There is **no finite absolute USD overrun cap in the product**. A turn's cost is
unknown until it ends. For grant `g_i`, its exact final-turn crossing is
`O_i = max(0, cost_before_final_turn + final_turn_cost - g_i)`. Each grant can
cross once, with `spent + reserved <= USD_limit + sum(settled O_i)`. Live exposure
includes the corresponding live overruns too. With the model's integer costs,
`O_i <= 3 - 1 = 2` per grant; the limit is not one overrun for the whole parallel
run. A cancelled grant enforces `max(grant, actual cost)`.

The lifecycle control-flow run excludes filesystem write failures, external
process death and the independent five-minute Git operation timeout. The
faithful filesystem-error run includes a later file write failure and finds
partial application. Applicable checks mean the repository checks selected for
fan-out, plus the final Git apply check. As designed, single-candidate runs skip
repository checks, and a fan-out can have no detected repository check. A
stopped/swept child means `Shutdown` returned; this model does not prove that
every OS process has disappeared when cleanup reports `Alive` or `Unknown`.

Nesting edges are managed launches through `proc.Start`, which increments
`REFRAKTAL_NEST`. Direct shell relaunches that bypass it, deliberate replacement
of inherited environment values, and integer overflow are excluded. A manual
invocation is represented by an initial state using the fallback guard.
`verify_nesting_test.go` anchors normalization, ceilings and grant-depth behavior
to the real code.

## Mutation checks

Each mutation must produce a counterexample, otherwise its test fails. The
known faithful findings remain enabled in separate explorations, rather than
being used to make an unrelated mutation appear effective.

| Model | Removed or broken protection | Result |
| --- | --- | --- |
| Kill | Per-target identity guard; permanent empty-session retirement | Both find counterexamples in isolated guard scenarios. |
| Budget | Held-dollar reservation; turn guards; cancelled-grant retention; one-time settlement; cumulative-cost delta | All five find counterexamples. |
| Lifecycle | Cancel guard; deadline guard; all-children barrier; failed-collect retention; failed-check exclusion; cancellation-independent apply | All six find counterexamples. |
| Nesting | Inherited-ceiling refusal | Finds a counterexample after a grant is lost and flags reset. |

## Faithful findings and production reproductions

No product code was changed by the pull request that added this package. Each
finding was first reproduced as a failing Go test against production
functions. A reproduction whose finding is not fixed yet requires
`REFRAKTAL_VERIFY_FINDINGS=1`, and ordinary CI skips it; once a fix lands, its
reproduction loses the opt-in and CI enforces it. All four findings below are
now fixed. The model tests always explore and assert the known counterexamples
of the modelled behaviour. A green test run therefore does **not** mean all
requested safety invariants hold.

1. **PID reuse after recheck.** `proc.Kill` calls `recheckPID` and `signal`
   separately. A tracked process can exit and have its PID reused between them.
   `internal/proc/verify_findings_test.go:TestVerifyFindingPIDReuseAfterRecheck`
   intercepts both operations and demonstrates one signal to an unrelated
   identity. It does not rely on the same-second identity alias.
   **Status: fixed on Linux** by commit `32ddb68`: `Kill` now obtains a pidfd
   (via `os.FindProcess`) before the identity re-check and signals through
   it, so a PID reused after the check cannot be signalled. The reproduction
   runs in CI without the opt-in on Linux. On macOS it skips: there is no
   pidfd, and the one-syscall window remains a known limit documented in
   `docs/DESIGN.md`, as does the process-group signal.
2. **Recorded session reuse between polls.** A tool session can empty, be reused,
   and lose its replacement leader before the next poll. Its unrelated surviving
   member then passes the sweep's current-identity check. This is the residual
   risk already documented in `docs/DESIGN.md`.
   `TestVerifyFindingRecordedSessionReusedBetweenPolls` in the same file
   demonstrates one unrelated signal. Both process reproductions intercept all
   signals; neither sends an OS signal.
   **Status: fixed** by commits `be6157b` and `e2e7160`: the sweep signals only
   members proved to be ours (recorded while the session was ours, or a
   descendant of a tracked or recorded process); any other member is reported
   as unknown and the live record is kept. The reproduction runs in CI without
   the opt-in on every platform.

   The kill-safety model above still describes the behaviour before these
   fixes, so its tests keep asserting those counterexamples. Updating the model
   to the fixed code is a follow-up.
3. **Oversized nested turn report.** The ACP response accepts
   `ChildReport.Turns` without comparing it to the child's allowance;
   `runEnv.child` forwards it to `Ledger.AddTurns`, which deliberately counts
   reports even past the limit. `TestBudgetUntrustedTurnsFindingReproduction`
   in `verify/budget_test.go` calls the real ledger and reaches five turns against
   a limit of four. This establishes a report trust limitation, not that an
   honest nested execution overspends its grant. Clamping the report would not
   undo any prompts that already ran.
   **Fixed in caab87e** ("Validate child reports; forfeit the grant on an
   untrusted one"): `AddTurns` refuses a report outside 0..the grant's
   remaining allowance and charges the child its whole grant instead, and
   `runEnv.child` validates every child report (turns, cost, retained) the
   same way. The reproduction no longer needs the opt-in: CI enforces it.
4. **Partial apply on filesystem error.** `Worktree.Apply` checks the patch, then
   runs `git apply --binary` without rollback. The production reproduction
   `internal/worktree/verify_apply_atomicity_test.go:TestVerifyApplyAtomicityFilesystemError`
   passes `CheckApply`, changes `a.txt` from `one` to `two`, then fails to create
   `z/b.txt` under an unwritable directory. The earlier change remains. It needs
   normal user filesystem permissions and skips root, which can bypass them.
   **Fixed in 9ef7651** ("Never leave the tree half-applied: snapshot and roll
   back"): `Apply` snapshots every touched path into the run folder and
   restores them on any failure. The reproduction no longer needs the opt-in:
   CI enforces it (it still skips as root);
   `TestFailedApplyIsRolledBackViaSeam` injects the failure through a seam so
   the same invariant is checked as root and on CI.

Reproduce the findings without any harness or network access (all four are
fixed, so these now pass; their reproductions also run in ordinary CI):

```sh
REFRAKTAL_VERIFY_FINDINGS=1 GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off \
  go test ./internal/proc ./internal/worktree ./verify \
  -run '^(TestVerifyFinding.*|TestVerifyApplyAtomicityFilesystemError|TestBudgetUntrustedTurnsFindingReproduction)$' \
  -count=1 -v
```

## Observed validation

The state table below comes from one run of `go test -race ./verify -count=1
-v -timeout=60s` on macOS (Go 1.27.4, darwin/arm64), which passed in 15.47 s,
including all models, the 14 mutation checks, the checker tests and the
production dependency check. The 60-second limit comes from `-timeout=60s` in
that command.

| Exploration | States | Maximum depth | Search elapsed with race detection |
| --- | ---: | ---: | ---: |
| Kill, faithful, first counterexample | 3,227 | 10 | 0.236 s |
| Kill, restricted guard isolation, exhaustive | 2,335 | 19 | 0.127 s |
| Budget, truthful reports, exhaustive | 185,881 | 24 | 12.682 s |
| Budget, oversized report counterexample | 34 | 4 | 0.001 s |
| Lifecycle, control flow, exhaustive | 1,559 | 21 | 0.043 s |
| Lifecycle, filesystem-error counterexample | 303 | 10 | 0.009 s |
| Nesting, exhaustive | 1,654 | 10 | 0.031 s |

Mutation trace lengths (transitions, excluding initial state): kill identity 8,
kill empty-session guard 4; budget reservation 2, turns 5, cancellation 4,
settlement 4, cumulative delta 5; lifecycle cancel 9, deadline 9, wait barrier 4,
retention 8, failed checks 12, interruptible apply 11; nesting ceiling 5.

Verified on the pull request that added this package:

- **macOS** (Go 1.27.4, darwin/arm64, under CPU load: 16 busy loops on 8 CPUs):
  `gofmt` and `go vet ./...` clean; `go test -race ./verify/...` in 16 s; the
  full `go test -race ./...` passed three consecutive times; with
  `REFRAKTAL_VERIFY_FINDINGS=1`, all four reproductions fail at their intended
  assertions.
- **Linux** (Go 1.24.7, as root): verify suite 18.4 s; full race suite 3/3;
  three reproductions fail with the flag, the apply one skips as root.
- **CI**: all seven checks passed (Go on ubuntu-latest and macos-latest for the
  root module and both spike modules, and the release dry-run build).

All four findings are fixed and their reproductions are enforced by CI:
findings 1 and 2 by #26, findings 3 and 4 by #25. The kill-safety model still
describes the behaviour before the fixes for findings 1 and 2; updating it is a
follow-up.
