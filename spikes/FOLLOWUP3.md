# Round 4 of live tests: brief for Claude Code

The third round (`FOLLOWUP2-RESULTS.md`) showed that cancel does not stop a
running tool on Codex or Antigravity, and that tools run outside the harness's
process group. The client now has `--kill-tree`, which tracks every descendant
of the harness by parent PID and kills them all at shutdown. This round checks
that it works, plus three cheap questions about isolation. Write the answers to
`spikes/FOLLOWUP3-RESULTS.md`.

## Rules (same as round 3)

- Raw output in `results/` is gitignored. **This repo is public.** Summarise;
  do not commit, paste or upload raw results.
- Report binary names and versions only, with the home directory as `~`.
- Never edit, move or rename the user's global harness configuration, and never
  copy credentials. The Antigravity test uses a fresh temporary `--gemini-dir`,
  which the script creates and deletes itself.
- Do not change prompts or time limits. Never invent a result. Show Adhish the
  diff before pushing.
- Before the `treekill` step, make sure no `sleep 20` is already running on
  the machine (the script prints any it finds before each run).

## Steps

1. `git pull`, then the offline checks; stop if they fail:
   ```
   cd spikes/acp && go vet ./... && go test ./... && HARNESSES=fake bash followup3.sh all
   ```
2. The tests, in this order (from a plain terminal if a Claude run fails with
   an authentication or nesting error):
   ```
   cd spikes/acp
   bash followup3.sh treekill   # 3 partial turns, cancelled 10 s in
   bash followup3.sh agy-iso    # 2 short Antigravity turns
   bash followup3.sh gated      # 2 short turns
   bash followup3.sh confine    # 1 short Codex turn
   ```
   The script itself checks for surviving `sleep 20` processes after each
   `treekill` run, so a separate watcher is optional. If you run one as in
   round 3, say so and report what it saw.

Each run writes `results/followup3-<test>-<harness>.jsonl`, `.summary.json`,
`.stderr` and `.files`. New summary fields with `--kill-tree`:
`tree_tracked` (every descendant seen while the harness ran, in any process
group), `tree_killed` (those still alive at shutdown, killed by PID) and
`tree_alive_after_kill` (any still alive 300 ms after the kill).

## Questions

### Process-tree kill (`treekill-*`), per harness

- **H1.** Did `tree_tracked` include the tool's `sleep 20`?
- **H2.** Was it in `tree_killed`? Was `tree_alive_after_kill` empty?
- **H3.** Was any `sleep 20` alive right after the run, or 3 s later
  (`.files`)? Compare with round 3, where Codex's and Antigravity's survived.
- **H4.** Anything killed that should not have been: any process in
  `tree_killed` that is not part of the harness or its tools?

### Antigravity with `--gemini-dir` (`agy-iso-*`)

- **H5.** Did both turns complete with `end_turn` (login works on a real turn)?
- **H6.** Did the plain turn reply `OK` and the write turn create `hello.txt`?
- **H7.** Model and mode reported by `session/new`, compared with a normal run.
  Did it start any MCP processes? Anything else behave differently (a
  first-run prompt, a sign-in request, an error)?

### Gated modes (`gated-*`)

- **H8.** Claude in `default` and Codex in `read-only`, asked to run
  `touch marker.txt`: did `session/set_mode` succeed? How many permission
  requests reached the client, and of what tool kind? The client approves
  them; was `marker.txt` created? If Codex refused instead of asking, say so.

### Codex confinement (`confine-*`)

- **H9.** Codex in `workspace-write`, asked to write `../outside.txt`: was the
  file created above the session directory (`files left above cwd`)? Did it
  ask permission, refuse, or report a sandbox error? Quote its reply in a few
  words.

Finish by telling Adhish in a few lines what these answers change in the
project's `SPIKES.md` design list, especially item 6 (permission modes and MCP
isolation) and item 14 (process-tree kill).
