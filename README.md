# refraktal

**One prompt in, a spectrum of agents out.**

Refraktal is a meta-harness. Give it a coding task and it runs coding-agent
harnesses (Claude Code, Codex, Antigravity, or a child Refraktal) as child
processes over the Agent Client Protocol, each in a git worktree of its own,
judges their results, and applies the winner to your working tree.

It uses the logins you already have: your Claude, ChatGPT and Antigravity
subscriptions. It never needs an API key for the harnesses, and by default it
gives them none.

Licensed under the [Apache License 2.0](LICENSE). Website: https://refraktal.dev

## Requirements

- macOS or Linux, arm64 or amd64, and `git`.
- Node.js (with `npm`): Refraktal installs the pinned ACP adapters for Claude
  Code and Codex from npm on first use.
- The harnesses you want to use, installed and logged in: `claude`, `codex`,
  `agy`. `refraktal doctor` checks them.

## Install

Releases are on [GitHub](https://github.com/refraktal/refraktal/releases).
Pick one route; replace `v0.1.0` with the release you want.

```sh
# The install script, fetched from the release itself: verifies the release's
# SHA256SUMS, downloads over HTTPS only, installs to ~/.local/bin, no sudo.
curl -fsSL --proto '=https' https://github.com/refraktal/refraktal/releases/download/v0.1.0/install.sh | sh

# Go 1.24 or later
go install refraktal.dev/refraktal/cmd/refraktal@v0.1.0
```

Or download the archive for your machine and `SHA256SUMS` from the release
page, check that one archive, and unpack it:

```sh
grep ' refraktal_0.1.0_darwin_arm64.tar.gz$' SHA256SUMS | shasum -a 256 -c -   # Linux: sha256sum -c -
tar -xzf refraktal_0.1.0_darwin_arm64.tar.gz refraktal && mv refraktal ~/.local/bin/
```

## Quickstart

```sh
refraktal doctor                       # the version, and which harnesses are installed and logged in
cd your-repo
refraktal run "Fix the off-by-one error in pagination"            # the router picks a harness
refraktal run --harness claude,codex "Add a --verbose flag"       # fan out to two, judge, apply the winner
refraktal run --no-apply --harness codex "Rename Foo to Bar"      # leave the result on a branch
refraktal run --usd 2 --mins 10 --turns 6 "..."                   # tighter budgets
refraktal show                          # the last run: route, candidates, verdict, cost
```

Every run happens in worktrees outside your checkout, from your `HEAD`
(uncommitted changes are not part of it). The winner is applied to your working
tree with a checked `git apply`, unstaged, so you review it with `git diff`.
Run logs are in `.refraktal/runs/` in the repository, which is gitignored.

Exit codes: 0 applied (or kept with `--no-apply`), 1 no candidate passed, 2 a
budget ran out, 3 nothing usable.

## Commands

```
refraktal run [task]      plan, dispatch, collect, judge; apply the winner to the tree
refraktal acp             speak ACP on stdio; any client, including a Refraktal, can drive it
refraktal show [run]      print a run: routing decisions, verdicts, cost (default: latest)
refraktal doctor          check harnesses, auth and the Decider; exit nonzero if unusable
  --harness a,b --fan N        run: override the router (pin harnesses, cap the width)
  --no-apply                   run: leave candidates in worktrees, print branches
  --dry                        run: plan and route only, spawn nothing
  --usd N --turns N --mins N --depth N   run, acp: budgets; children get less
  --json                       run, show, doctor: JSON on stdout
```

The [guide](docs/GUIDE.md) covers configuration, budgets, routing and your
data, judging, nesting and the known limits. The full specification is
[docs/DESIGN.md](docs/DESIGN.md); maintainers, see
[docs/RELEASING.md](docs/RELEASING.md).

## Routing and your data

Without `--harness`, Refraktal asks Google's Gemini Decisions API which harness
should take the task, if `GEMINI_API_KEY` is set (in the environment or a
gitignored `.env`); otherwise it routes with local rules. On Google's free
tier, Google uses the data in these requests (your task text, a short
description of the repository and, when several harnesses' results are
compared, each candidate's diff, up to 32 KiB) to improve its products. Use
`--harness` or leave the key unset to keep task text and diffs local.
