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

## Install

macOS or Linux, arm64 or amd64. Pick one:

```sh
# The install script: verifies the release's SHA256SUMS, installs to ~/.local/bin, no sudo.
curl -fsSL https://refraktal.dev/install | sh

# Homebrew
brew install refraktal/tap/refraktal

# Go 1.24 or later
go install refraktal.dev/refraktal/cmd/refraktal@latest
```

Then install and log in to the harnesses you want to use (`claude`, `codex`,
`agy`); Refraktal installs the pinned ACP adapters for Claude Code and Codex
itself, on first use.

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
