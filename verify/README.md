# Bounded model checks for the fixed code

These test-only models describe the fixed production code on main at `7ab1b1d`,
plus the run-5 fixes for findings 5–7 below. PR D extends the KILL model to
cover tracker enrollment from mixed process snapshots (finding 8 below).
Every FAITHFUL exploration must exhaust its reachable graph without an invariant
violation. Old failures are mutation tests: each must find its counterexample.
The separate no-pidfd exploration must find the documented platform limit.
A green suite means those expectations hold, not that every platform is safe.

Run without network or toolchain downloads:

```sh
GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off \
  go test -race ./verify/... -count=1 -v -timeout=60s
```

All Go files here are `_test.go` files and use only the standard library plus
this module's production packages. Nothing enters the shipped binary.
`TestProductionDoesNotImportVerify` checks the transitive dependencies of the
root package and CLI and rejects non-test Go files in this directory. Existing
CI's `go test -race ./...` includes these checks.

## What is established

The checker uses breadth-first search with complete canonical state keys,
immutable value states and first-discovery predecessors. There is no depth or
state-count cutoff. It exhausts the finite graph or returns a shortest trace to
an invariant violation. Counts for failing explorations describe the explored
prefix. Checker unit tests cover cycles, deduplication, shortest traces and an
invalid initial state. Model reachability witnesses check that relevant success
and boundary paths were explored. Each modeled action has a comment naming the
production function and file it mirrors.

These are **bounded checks**, with no cutoff theorem for larger systems. They
check the transition systems below; they do not prove that the abstraction is
complete or that future production changes preserve its mapping.

### Kill

Individual signals acquire a handle before the birth-identity recheck, then
signal that handle, matching `Kill` in `internal/proc/tracker.go`. The Linux
pidfd variant allows exit and PID reuse between those actions and must never
signal an unrelated birth identity. This is a simulated usable-pidfd model even
when tests run on macOS. Process-group signals remain PID-based.

Session membership alone is insufficient. The model records identities while
the session belongs to us, or follows PPID links from identities matching in the
same snapshot, requiring the child to be no older than its parent. Unproved
members are unknown, are not promoted to trusted targets, and keep the live
record. Two consecutive unoccupied observations are required for emptiness;
occupied snapshots without stable members are not empty. This mirrors
`provenMembers`, `provenDescendants` and `sweepSession` in
`internal/proc/session.go`, session polling in `internal/proc/child.go`, and live
record retention in `internal/proc/live.go`.

The former tracker-enrollment gap is closed within the new bounds. Tracker
discovery uses `killPoll`, shared with the exit/reuse model, and follows a PPID
edge only from a current verified parent row with valid birth times and child
birth >= parent birth. The separate mixed-snapshot exploration varies current
rows and historical records independently, then acquires a handle, rechecks the
enrolled identity and signals it. This checks ownership before the identity
check, which alone cannot reject a wrongly enrolled process. Missing or
malformed births are represented by rank zero; real tracker tests exercise
the timestamp parser with missing and malformed strings.

The separate **no pidfd** variant represents macOS and Linux when Go falls back
to PID signalling (old kernels, failed pidfd open or blocked syscalls). It must
find the identity-check-to-signal window. This is a documented platform limit,
not a passing safety model.

### Budget

The concurrent ledger checks held dollars and turns, returned shares, one-time
settlement, cumulative cost deltas, cancellation retention and exact final-turn
overruns. A separate report-boundary exploration checks child-report validation
and forfeiture without multiplying the concurrent ledger graph by every wire
input. Invalid turns, cost or retained amounts must consume the grant's unused
dollars and turns; genuine costs are charged first and an overrun is not capped.
The exploration includes the wire forms `acp.parseUpdate`
(`internal/acp/types.go`) sees: a cost that is present but not a number, not an
object or negative must reach validation as invalid (`CostInvalid`), never as
absent. An absent cost is valid and priced from tokens. It also includes the
forms `acp.decodePromptResult` sees: a report with a field that cannot be
decoded (a number outside its field's range or of the wrong type) and an
undecodable usage beside a report must reach validation as untrusted
(`ChildInvalid`, `UsageInvalid`), never as no report. A refraktal child that
ends without a valid report keeps its whole grant: untrusted after `end_turn`,
kept with its failed outcome after an ACP error or crash.
The mapping is `Ledger` in `internal/budget/budget.go` and `runEnv.child` in
`fanout.go`; report values correspond to `server.prompt`/`finish` in
`acpserver.go`. Repeated ACP prompts sharing an inherited ledger are not
explicitly modeled.

There is **no finite absolute USD overrun cap in production**. For grant `g_i`,
the exact final-turn crossing is
`O_i = max(0, cost_before_final_turn + final_turn_cost - g_i)`.
`spent + reserved <= USD_limit + sum(settled O_i)`; live exposure includes live
overruns too. Each grant can cross once. Cancellation enforces
`max(grant, actual cost)`. These bounds apply per grant, not once per whole run.

### Lifecycle

The model covers stop/sweep barriers before collection, applicable checks,
judging, cancel/deadline admission, no-apply and candidate retention in `Run`
(`run.go`) and `runEnv.fanOut` (`fanout.go`). It distinguishes a failed collect
from a failed turn that nevertheless changed files. Failed candidates with work
survive even when another candidate wins; if no applicable winner exists while
failed work remains, the run fails rather than reporting no changes or no winner.
A candidate whose turn ended for budget or `--mins` keeps that outcome through a
failed collect: the run exits 2, applies nothing and keeps the work (the outcome
precedence table in `docs/DESIGN.md`).

`Worktree.Apply` (`internal/worktree/worktree.go`) snapshots touched paths before
writes. A modeled write failure restores the snapshot before returning, and so
does a failure of the post-apply LFS attribute check: the snapshot is held until
that check passes. Parent
cancellation cannot interrupt an admitted operation (`runEnv.opCtx` in
`fanout.go`). Atomicity means the tree at operation return; intermediate Git
writes can be partial. Snapshot refusal leaves the original tree unchanged.

### Nesting

The independent inherited ceiling is unchanged. `proc.Nest` normalizes invalid
or negative nest values, `proc.Start` increments the managed edge, `nestCeiling`
(`run.go`) lowers and enforces ceilings, and `childLimits` (`acpserver.go`)
intersects flags with grants. Lost grants cannot bypass the ceiling.
`verify_nesting_test.go` anchors the source mapping with real production guards.

## Bounds and limits

| Exploration | Bounds | Useful cases |
| --- | --- | --- |
| Kill exit/reuse | Three PID slots: pinned harness, recorded tool leader, member; two distinct births per reusable PID; reusable session/group IDs; empty count saturated at two | Exit/reuse around handle acquisition, recheck and signal; session replacement leader can fork and exit; unknown record retention; occupied versus empty. |
| Kill snapshot provenance | Three rows: absent leader, parent outside session, child in session; current and recorded births 1..2; child record absent/1/2; parent presence and PPID link independent | Stale parent identity, child older than replacement parent, direct historical identity and ancestry. |
| Kill tracker enrollment | Three PID rows with separate PGIDs; root birth 1..3; parent/child birth 0 (invalid) or 1..3; historical parent absent/1..3; root/parent presence and child PPID link independent: 1,536 input snapshots | Current verified parent, missing/invalid births, equal births, stale PPID after parent reuse, then handle acquisition, successful recheck and signal. |
| Concurrent ledger | Two candidates plus later judge; four USD units/four turns; turn costs `{0,1,3}` | Overlapping reservations, returned shares, cumulative reports, duplicate settlement, cancellation and per-grant final-turn crossing of at most two units. |
| Child-report boundary | USD/turn grants 1..4; report turns -1..5; costs `{0,1,3,6}` or negative/NaN/infinite/non-USD classes; retained `{0,1,3,5}` or negative/NaN/infinite classes; non-numeric and absent wire costs; decodable, undecodable-report and undecodable-usage responses, and a child ending without a report after `end_turn` or a failed turn; cancelled or normal | Every validation branch, a malformed wire cost, an undecodable report or usage, a missing report forfeited or kept, grant forfeiture and idempotence, valid retained charges, real costs above the grant. Invalid-cost token pricing uses the zero-cost fallback. |
| Lifecycle | One/two candidates, each optionally stopped for budget or `--mins`; optional judge; one collect/check result each; two touched files; monotonic cancel/deadline | Failed turn with work versus failed collect, a budget stop with a failed collect, snapshot refusal, sequential writes, later write error, rollback, and a post-apply LFS error that rolls back. |
| Nesting | Nest invalid/negative/0..3; depth/child flags 0..3; ceilings absent/malformed/-1..4; shrinking/lost/malformed grants | Equality/refusal, ceiling lowering and grant loss; at most four admitted runs per chain. |

The bounds exercise the races and guards listed below. They establish no
fairness or liveness: processes may never exit, children may never report, and
unresolved live records may remain indefinitely.

- **Processes:** no proof for arbitrary trees, arbitrary session creation,
  process-list errors, signal failures, or all OS schedules. The known-identity
  set retains historical births across polls, slightly overapproximating the
  production latest-birth map; births never return in these bounds. Birth identities have distinct
  start times; the accepted one-second `ps` start-time alias is unproven.
  Tracker snapshots abstract timestamp validity and order into four ranks;
  arbitrary encodings and invalid root births are outside this model's bounds
  (the real tracker tests cover invalid root/parent birth strings).
  Process-group signals have no pidfd and retain the check-to-signal limit.
  The modeled harness stays unreaped, reserving its IDs; fallback immediate
  reaping on systems without `waitid(WNOWAIT)` is unproven. A process that
  escapes all recorded identity/PPID evidence can remain running. The model
  does not prove all processes disappear when shutdown returns.
- **Accounting:** integer USD avoids floating-point rounding. Arbitrary costs,
  fan-out, malformed JSON transport, integer overflow and decreasing cumulative
  provider reports are outside the ledger bounds. Report validation cannot
  verify whether an otherwise valid report is truthful or undo prompts already
  sent by a child. It bounds the parent's accepted accounting.
- **Apply:** snapshot contents and successful restoration are abstracted as two
  file values. Restore failures, concurrent writes, external process death,
  all file types/permissions/symlink layouts, and the independent Git-operation
  timeout are unproven. Production reports unrestored paths and keeps originals
  when rollback fails. Ownership and timestamps are not restored. Repository
  checks are inapplicable for a single candidate and may be absent for fan-out.
- **Nesting:** managed launches only. Direct shell relaunches bypassing
  `proc.Start`, deliberate environment replacement and arithmetic overflow are
  outside the model. Manual invocations enter through initial fallback states.

## Mutations

| Model | Mutation | Expected result | Trace length |
| --- | --- | --- | ---: |
| Kill | PID-based signal after recheck | Unrelated identity signalled | 12 |
| Kill | Remove per-target identity guard | Unrelated identity signalled | 12 |
| Kill | Remove tracker PPID birth-order guard | Older unrelated child passes identity recheck and is signalled | 5 |
| Kill | Sweep current members of a never-seen-empty session | Unrelated member signalled after session reuse | 22 |
| Kill | Treat one unoccupied snapshot as empty | Premature emptiness declared | 2 |
| Kill | Remove permanent retirement | Retired session inspected | 1 |
| Kill, restricted legacy fixture | Remove permanent retirement with old unsafe membership selection | Original unrelated-signal counterexample | 6 |
| Budget | Ignore held-dollar reservations | Exposure exceeds limit plus final-turn crossings | 2 |
| Budget | Remove turn guards | Turn limit exceeded | 5 |
| Budget | Return cancelled grant's unused share | Cancelled grant undercharged | 4 |
| Budget | Settle grant twice | Duplicate settlement | 4 |
| Budget | Charge cumulative total rather than delta | Charge differs from actual cost | 5 |
| Budget | Accept unvalidated child report | Five turns accepted against limit four | 4 |
| Budget | Parser drops a malformed cost as absent | Malformed nested cost accepted | 3 |
| Budget | Client reads an undecodable report as absent | Undecodable nested report's grant returned | 3 |
| Budget | Child without a report returns its unused grant | Refraktal child's unreported spending escapes `--usd` | 3 |
| Lifecycle | Remove cancel guard | Invalid apply admission | 9 |
| Lifecycle | Remove deadline guard | Invalid apply admission | 9 |
| Lifecycle | Remove all-children barrier | Collection before every shutdown returned | 4 |
| Lifecycle | Remove failed-collect retention | Failed collect's work deleted | 8 |
| Lifecycle | Remove failed-check exclusion | Invalid winner admitted | 12 |
| Lifecycle | Remove cancellation-independent apply | Parent interrupts admitted operation | 13 |
| Lifecycle | Apply without rollback | Partial tree at apply return | 12 |
| Lifecycle | Delete failed turn's changes | Failed sibling's work deleted after winner applies | 17 |
| Lifecycle | Suppress unused-failed-work outcome | Failed/clean fan-out incorrectly reports no changes | 11 |
| Lifecycle | Collect failure erases budget exhaustion | Budget-stopped run ends failed (exit 1), not exit 2 | 8 |
| Lifecycle | Clear snapshot before the post-apply check | Failure reported over a changed tree | 12 |
| Nesting | Remove inherited-ceiling refusal | Ceiling exceeded after grant loss/reset flags | 5 |

All 28 checks reproduce the stated counterexample. Trace lengths count
transitions, excluding the initial state. The legacy retirement fixture retains
the original mutation under explicitly restricted pre-fix membership selection;
its guarded baseline passes only that fixture and is not a FAITHFUL claim about
the shipped code. Fixed membership provenance independently prevents that old
signal trace, so the current-code retirement mutation checks the retirement
contract directly.

Every mutation is checked separately and must violate its intended invariant.
The no-pidfd limit is reported separately from mutation results.

## Findings and production reproductions

Findings 1–8 are fixed on main and finding 9 on this branch. Their former
faithful counterexamples now live in mutations, while updated FAITHFUL models
must pass exhaustive exploration. PR D also fixes the production tracker
enrollment guard and adds its model (finding 8).

1. **PID reuse after recheck:** fixed on Linux with pidfd acquisition before
   recheck (`32ddb68`). The PID-signal mutation reproduces the old unrelated
   signal. `internal/proc/verify_findings_test.go` contains
   `TestVerifyFindingPIDReuseAfterRecheck`; the Linux reproduction is enforced
   and macOS skips because its platform limit remains.
2. **Recorded session reused between polls:** fixed with membership provenance
   (`be6157b`, `e2e7160`). The unsafe-current-members mutation reproduces the old
   unrelated signal. `TestVerifyFindingRecordedSessionReusedBetweenPolls` in
   the same production test file is enforced. Single-empty retirement is
   checked independently by its own mutation.
3. **Oversized nested turn report:** fixed by validation and forfeiture
   (`caab87e`). The unvalidated-report mutation reproduces the old turn-budget
   violation. `TestBudgetUntrustedTurnsFindingReproduction` here calls the real
   ledger and is enforced.
4. **Partial apply on filesystem error:** fixed by snapshot and rollback
   (`9ef7651`). The no-rollback mutation reproduces the old half-applied tree.
   `internal/worktree/verify_apply_atomicity_test.go` contains the filesystem
   reproduction (skips root); `TestFailedApplyIsRolledBackViaSeam` also checks
   the fix without relying on filesystem permissions.
5. **A collect failure erased budget exhaustion** (Astra run 5 #3): fixed by
   keeping a budget-stopped candidate's outcome through a failed collect
   (`7a1fda7`). The collect-erases-budget mutation reproduces the old exit 1.
   `TestCollectFailureNeverOverridesExhaustion` (root package) is enforced.
6. **A malformed nested cost was priced from tokens** (Astra run 5 #4): fixed
   by marking a present but unusable cost and forfeiting the grant
   (`572e64c`). The parser-drop mutation reproduces the acceptance.
   `TestMalformedNestedCostForfeitsTheGrant` (root package) sends the
   malformed cost over the wire; `TestCostInvalidMarksAPresentUnusableCost`
   (`internal/acp`) covers the parser. Both are enforced.
7. **The snapshot was cleared before the post-apply check** (Astra run 5 #5):
   fixed by clearing it only after that check passes (`3f729d1`). The
   clear-before-check mutation reproduces the changed tree.
   `TestPostApplyFailureIsRolledBack` (`internal/worktree`) is enforced and
   runs as root.

Findings 5–7 were missed by the previous FAITHFUL models, and each model now
covers its case. The lifecycle model had no per-candidate budget stop, and it
accepted a post-apply failure over a fully applied tree as correct. The budget
report model assumed a negative cost reached validation, but the parser dropped
it as absent. The no-pidfd counterexample is the already documented platform
limit.

8. **Mixed tracker snapshot with a stale PPID (PR D, run-5 finding 2):** the real
   `TestReviewTrackerStalePPIDTargetsUnrelatedProcess` uses deterministic rows
   and intercepted signals. Before the fix it enrolled and signalled an older
   unrelated child through a replacement parent. The fixed tracker requires
   valid birth times and child birth >= parent birth. The exact model fixture
   `TestKillTrackerStalePPIDReproduction` now passes, and the new birth-order
   mutation reproduces the ownership error even with the identity recheck and
   handle signalling retained. `TestTrackerPPIDEnrollmentNeedsBirthOrder`
   also covers root/tracked parents, absent rows and invalid births.

9. **An undecodable child report was read as absent** (Astra run 6 #1):
   turns one past the largest 64-bit integer failed the client's single
   decode of the prompt response, so the parent returned the unused grant.
   Fixed by decoding field by field and forfeiting a present but
   undecodable report or usage. The undecodable-report mutation reproduces
   the returned grant. `TestUndecodableChildReportForfeitsTheGrant` (root
   package) sends each form over the wire;
   `TestDecodePromptResultFlagsAPresentUndecodableReport` (`internal/acp`)
   covers the decoder. Both are enforced. The report model had no response
   class: it assumed every report it validated had decoded.
   In the same class, a refraktal child whose turn ended without a valid
   report (no `_meta`, no report, a `_meta` that is not an object, an ACP
   error, a crash) settled like a plain harness and returned its unused
   grant, though it may have spent it on its own harnesses. Now its whole
   grant is kept: untrusted (exit 2) after `end_turn`, failed with a notice
   otherwise. The no-report mutation reproduces the returned grant.
   `TestRefraktalChildWithoutReportForfeits` and
   `TestFailedRefraktalChildKeepsItsGrant` (root package) are enforced.

11. **A cancelled worktree create waited for the repository lock** (Astra
   run 7 #3): `CreateAt` took the lock with `context.Background`, so a
   cancelled or expired create waited for another create or removal on the
   same repository. Fixed by waiting through the caller's context, as
   `Remove` already did. Outside the models (the lock is not modeled).
   `TestCreateHonorsContextWhileRepositoryLocked` (`internal/worktree`) is
   enforced.

The new production
tracker reproduction runs in this sandbox because all signals are intercepted;
the older process-level reproductions still need the orchestrator.

The older process-level reproductions were not run in this sandbox, which blocks
`/bin/ps`; the orchestrator can run them. Existing reproduction commands:

```sh
GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off \
  go test ./internal/proc ./internal/worktree ./verify \
  -run '^(TestVerifyFinding.*|TestVerifyApplyAtomicityFilesystemError|TestFailedApplyIsRolledBackViaSeam|TestBudgetUntrustedTurnsFindingReproduction|TestPostApplyFailureIsRolledBack|TestCreateHonorsContextWhileRepositoryLocked)$' \
  -count=1 -v
GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off \
  go test . ./internal/acp \
  -run '^(TestCollectFailureNeverOverridesExhaustion|TestMalformedNestedCostForfeitsTheGrant|TestCostInvalidMarksAPresentUnusableCost|TestUndecodableChildReportForfeitsTheGrant|TestDecodePromptResultFlagsAPresentUndecodableReport|TestRefraktalChildWithoutReportForfeits|TestFailedRefraktalChildKeepsItsGrant)$' \
  -count=1 -v
```

## Measured validation

Measured on macOS, Go 1.27.4, darwin/arm64, using the command above with
`GOCACHE=~/Projects/.refraktal-gocache`. The complete suite passed in **34.387 s**
(package time), **34.58 s wall time**, below one minute. The budget report and
lifecycle rows were measured again after findings 5–7 (default cache, same
machine): the complete suite then passed in **36.427 s** (package time),
**36.93 s wall time**. After PR D's tracker model was
added and rebased onto #28 (`4f7c5be`), the complete suite passed in
**36.662 s** (package time), **38 s wall time**. After finding 9 and its follow-up added the response classes, the report
validation row was measured again (default cache, same machine) and the
complete suite passed in **48.814 s** (package time), **49.31 s wall time**. Timings are observations,
not performance guarantees. Every faithful row below exhausted the full graph;
the platform-limit row stopped at its expected counterexample.

| Exploration | States | Maximum depth | Race search time | Result |
| --- | ---: | ---: | ---: | --- |
| Kill, FAITHFUL usable pidfd | 79,492 | 33 | 6.658 s | Exhaustive, no violation |
| Kill, FAITHFUL snapshot provenance | 193 | 2 | 0.007 s | Exhaustive, no violation |
| Kill, FAITHFUL tracker enrollment | 7,105 | 5 | 0.257 s | Exhaustive, no violation |
| Kill, exact stale-PPID fixture | 5 | 4 | 0.00021 s | Exhaustive, no violation |
| Kill, fixed retirement fixture | 8 | 3 | 0.00036 s | Exhaustive, no violation |
| Budget, FAITHFUL concurrent ledger | 185,881 | 24 | 12.577 s | Exhaustive, no violation |
| Budget, FAITHFUL report validation | 548,801 | 6 | 7.639 s | Exhaustive, no violation |
| Lifecycle, FAITHFUL control flow | 5,675 | 24 | 0.220 s | Exhaustive, no violation |
| Lifecycle, FAITHFUL filesystem errors | 5,791 | 24 | 0.249 s | Exhaustive, no violation |
| Nesting, FAITHFUL | 1,654 | 10 | 0.030 s | Exhaustive, no violation |
| Kill, no pidfd platform limit | 21,941 | 12 | 1.991 s | Expected unrelated-signal counterexample |
| Restricted legacy retirement fixture, guard retained | 8 | 3 | 0.00035 s | Exhaustive only for this pre-fix fixture |

All 28 mutation checks passed by reproducing their intended violations; their
trace lengths are in the mutation table. The real ledger witnesses and the
production dependency isolation check passed in the complete suite. The real
`TestVerifyNestingSourceWitness` also passed separately with race detection.

The new tracker birth-order mutation explored a 5,835-state prefix in 0.214 s
and found a shortest five-transition trace: sample child birth 1 with replacement
parent birth 2, enroll through its stale PPID, acquire the child handle, pass the
identity recheck, and signal the unrelated child. Only the birth-order check is
removed; timestamp validity and handle signalling remain. The faithful tracker
exploration covers all 1,536 input snapshots without a cutoff. All existing
invariants and mutation expectations are retained.

The production tracker reproduction and 16 birth-order cases passed with race
detection, using deterministic snapshots and intercepted signals. Neither this
run nor the model signals an unrelated real process.

Final checks (wall times):

- `gofmt -l .`: 0.07 s, no paths printed.
- `go vet ./...`: 1.30 s, passed on darwin/arm64.
- `GOOS=linux go vet ./...`: 1.34 s, passed; no Linux execution claimed.
- ACP spike native/Linux vet: 0.22 s / 0.20 s, passed.
- `go test -race ./verify/... -count=1 -v -timeout=60s`: 35.56 s, passed.
- `git diff --check`: passed.

Full process integration remains for the orchestrator because this sandbox
blocks `/bin/ps`; local listeners are also blocked. An overly broad ACP test
selection included `TestKillWhenCancelIsIgnored`, which failed with its fake
agent still running, then the selection timed out at 60 s. The explicit
in-memory writer/permission regression selection passed three times with race
detection. No full-suite, real-harness, Linux runtime or fresh CI success is
claimed. The optional broad worktree race run in staging also exceeded its
60 s timeout; the active driver-configuration test passed separately.

Three subagents implemented safegit, tracker/KILL, and ACP/CLI in isolated
staging copies. The primary agent implemented sessions-inventory retention and
the spike oracle, reviewed all changes, integrated the fixes, and ran the final
checks. Integration review also added persistent inventory uncertainty so a
dead-run cleanup cannot discard a record retained at shutdown. Cumulative
snapshots contain complete changed files, `DELETED.txt`, and `COMMIT.txt` under
`~/Projects/.refraktal-gocache/fixd-snapshots/`. No commit was made.
