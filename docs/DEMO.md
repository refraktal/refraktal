# Refraktal demo

Five commands that show what Refraktal does, using the harnesses already on
your Mac. Allow about 5 minutes. On subscriptions the whole demo costs about
$0.80 notional; that is a budget unit Refraktal tracks, not a bill.

## Before you start

- macOS on Apple silicon or Intel, or Linux, with `git` and Go installed.
- At least one harness installed and logged in: Claude Code (`claude`), Codex
  (`codex`) or Antigravity (`agy`). Node.js (`npx`) is needed for the Claude
  and Codex adapters.
- Optional: `GEMINI_API_KEY` in your environment, so the router asks Gemini.
  Without it, Refraktal routes with local rules. On Google's free tier, Google
  may use the task text it receives to improve its products.

## Install from the release

Download with `curl`, check the archive against `SHA256SUMS`, then unpack it.
The steps are chained, so a checksum mismatch stops before anything is
unpacked:

```sh
base=https://github.com/refraktal/refraktal/releases/download/v0.1.0
curl -fsSLO "$base/SHA256SUMS" && curl -fsSLO "$base/refraktal_0.1.0_darwin_arm64.tar.gz" &&
  grep ' refraktal_0.1.0_darwin_arm64.tar.gz$' SHA256SUMS | shasum -a 256 -c - &&
  tar -xzf refraktal_0.1.0_darwin_arm64.tar.gz refraktal &&
  mkdir -p ~/.local/bin && mv refraktal ~/.local/bin/
```

Use `darwin_amd64` on an Intel Mac. Make sure `~/.local/bin` is on your `PATH`.
If you downloaded in a browser, run
`xattr -d com.apple.quarantine ~/.local/bin/refraktal`.

## A scratch repository

Every run needs a Git repository. This makes a tiny Go module to work in:

```sh
mkdir ~/refraktal-demo && cd ~/refraktal-demo && git init -q
printf 'module example.com/demo\n\ngo 1.24\n' > go.mod
printf 'package demo\n\n// Add returns a + b.\nfunc Add(a, b int) int { return a + b }\n' > add.go
printf '.refraktal/\n' > .gitignore
git add -A && git commit -qm initial
```

## The demo

### 1. Check the setup

```sh
refraktal doctor
```

Shows the version, which harnesses are installed and logged in, whether the
Gemini router is reachable, and that no earlier run left anything behind.
It exits nonzero if no harness is usable. Takes about 5 seconds.

### 2. See where a task would go, without running it

```sh
refraktal run --dry "add a hello function"
```

Prints the routing decision (which harness, why, and how confident) and
starts nothing. Free. Compare with `env -u GEMINI_API_KEY refraktal run --dry
"add a hello function"` to see the local fallback router.

### 3. One harness makes a change

```sh
refraktal run --harness claude --usd 1 --mins 5 \
  "Add Hello(name string) string returning \"Hello, <name>!\", with a table-driven test"
git status && git diff
```

The harness works in its own worktree outside your checkout. The result is
applied to your working tree, unstaged, so you review it before committing.
New files show in `git status` but not in `git diff`; run `git add -N .`
first to include them. About 40 seconds and $0.25 notional.

### 4. Three harnesses race, and the best result wins

```sh
git add -A && git commit -qm "hello"
refraktal run --harness claude,codex,agy --usd 1 --mins 5 \
  "Add Reverse(s string) string that reverses by runes, with a table-driven test"
```

Each harness works in parallel in its own worktree. Refraktal runs `go build`
and `go test` in each, compares the candidates and, in a close call, asks a
judge harness to choose. The winner is applied. About 2 to 3 minutes and
$0.50 notional.

### 5. See what happened

```sh
refraktal show
refraktal show --json
```

Prints the last run: the route, each candidate's outcome, the check results,
the verdict (and who judged), the files applied and the cost. `--json`
prints the same as one JSON record.

## Rough edges in v0.1.0

- New files from a run are untracked: review them with `git status`, or run `git add -N .` before `git diff`.
- A harness's final message is printed without line breaks between its parts.
- Fan-out output can link to a file in a worktree that has already been deleted.
- The budget line at the end of `run` is hard to read; `refraktal show` prints the cost more clearly.
- When every harness is a candidate, the judge notice says "no other harness was available and usable"; it means no non-candidate harness was left to judge.
- `refraktal doctor` writes its run log to `.refraktal/runs/` in the current folder, even outside a repository.
- A denied shell request during a run is expected: Refraktal's policy refused it, and the harness recovers by another route.

## Budgets

`--usd` is checked before each turn, so each harness can go over its share by
at most its final turn. `--mins` is a hard stop. Exit codes: 0 applied, 1 no
candidate passed, 2 a budget ran out, 3 nothing usable. See the
[guide](GUIDE.md) for details.
