# refraktal

**One prompt in, a spectrum of agents out.**

Refraktal is a meta-harness: give it a task and it fans the work out to subagents that each drive a different agent harness (Claude Code, Codex, Gemini CLI, and others), then judges and merges the results.

This repository and the `refraktal` packages on npm and PyPI are early placeholders. Nothing to use yet.

Licensed under the [Apache License 2.0](LICENSE).

- Website: https://refraktal.dev
- Go: `go install refraktal.dev/refraktal/cmd/refraktal@latest`

## Routing and your data

Without `--harness`, Refraktal asks Google's Gemini Decisions API which harness
should take the task, if `GEMINI_API_KEY` is set (in the environment or a
gitignored `.env`); otherwise it routes with local rules. On Google's free
tier, Google uses the data in these requests (your task text and a short
description of the repository) to improve its products. Use `--harness` or
leave the key unset to keep task text local.

