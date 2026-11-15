# Refraktal

A meta-harness in Go: one task in, coding-agent harnesses (Claude Code, Codex,
Antigravity, Refraktal itself) run as children over ACP, results judged, the
winner applied. Module `refraktal.dev/refraktal`.

- **What to build:** `docs/DESIGN.md` (the specification) and
  `docs/BUILD-PLAN.md` (milestones, in order). Work one milestone per PR.
- **Evidence:** `spikes/` holds throwaway prototypes and four rounds of live
  results (`spikes/*RESULTS.md`). Read them; do not import from them.

## Commands

```
gofmt -l .            # must print nothing
go vet ./...
go test -race ./...
```

The spikes are separate modules: run the same commands inside `spikes/acp` and
`spikes/routing`.

## This repository is public

- Never commit secrets. `.env` is gitignored; keep it that way. Never print an
  API key in output, logs or commits.
- Never commit raw harness output (`results/`, JSONL logs, stderr captures).
  It contains account emails, local paths and session data. Summarise instead.
- In anything committed, write the home directory as `~` and do not name the
  user's MCP servers, plugins or other apps.

## Rules

- **Go standard library only.** Ask before adding any dependency.
- **The CLI surface in `docs/DESIGN.md` is fixed.** Ask before adding a
  command or flag.
- **Never edit, move or copy the user's global harness configuration or
  credentials** (`~/.claude`, `~/.codex`, `~/.gemini` and similar). Isolation
  uses per-run options only.
- **Processes:** kill children by tracked PID with an identity check, plus the
  process group. Never kill by name or pattern. Never put worktrees or run
  folders under `$TMPDIR` or `/tmp`.
- **Tests:** every behaviour is covered against the fake ACP agent in
  `go test`. Live harness checks are scripts writing to gitignored folders,
  with trivial prompts and short time limits, never part of `go test`.
- **Never invent a result.** If a check did not run, say so.
- **Publishing:** `.github/workflows/publish-npm.yml` and `publish-pypi.yml`
  are pinned by the npm and PyPI trusted-publisher configs. Do not rename or
  edit them, or `npm/` and `python/` packaging, without asking.
- **Commits** are authored as `Adhish Thite <hello@refraktal.dev>`. Show the
  diff before pushing; never push to `main` directly, use a branch and PR.
