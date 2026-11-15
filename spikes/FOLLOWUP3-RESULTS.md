# Round 4 results

Run on 2026-11-15, on the same laptop and logins as before, from `FOLLOWUP3.md`. Offline checks
passed first, and no `sleep 20` was running on the machine before any `treekill` run. All four
tests completed. Raw output stays in the gitignored `results/` folder. No global harness
configuration was edited. Paths below use `~`.

Two additions to the brief:

1. **Watcher.** As in round 3, a separate watcher recorded every `sleep 20` on the machine every
   0.6 s during the three `treekill` runs.
2. **Extra confinement run.** The scripted `confine` run puts the session folder under `$TMPDIR`.
   Codex's `workspace-write` sandbox appears to treat `$TMPDIR` as writable (see H9), so that run
   cannot tell confinement from designed behaviour. I ran the same prompt once more, with the same
   mode and time limit, with the session folder under the gitignored `spikes/acp/results/`
   instead. The folder was deleted afterwards.

**Short answer.**
- `--kill-tree` tracked every tool `sleep` in all three harnesses. In this run it never had to kill
  anything: each `sleep` was already gone at shutdown. No `sleep 20` survived any run.
- Antigravity with a fresh `--gemini-dir` works on real turns.
- Claude `default` and Codex `read-only` both route a shell command to the client as a permission
  request.
- Codex `workspace-write` writes freely inside the session folder and `$TMPDIR`. Outside them, it
  asks.

## Process-tree kill (`treekill-*`)

Same prompt as round 3: `sleep 20`, then write `hello.txt`. Cancelled 10 s in, with `--kill-tree`.

| | Claude Code | Codex | Antigravity |
|---|---|---|---|
| Stop reason, cancel to response | `cancelled`, 13 ms | `cancelled`, 24 ms | `cancelled`, 14 ms |
| H1. `sleep 20` in `tree_tracked`? | Yes | Yes | Yes |
| H2. `sleep 20` in `tree_killed`? | No: already gone | No: already gone | No: already gone |
| H2. `tree_killed` / `tree_alive_after_kill` | empty / empty | empty / empty | empty / empty |
| H3. `sleep 20` right after / 3 s after the run | none / none | none / none | none / none |
| What ended the `sleep` (watcher) | The cancel | Codex's own shutdown, about 6.5 s after the cancel | **The cancel** |
| `hello.txt` written? | No | No | No |

- **H3, compared with round 3.**
  - Codex behaved the same: cancel did not stop the tool, and the tool ended when Codex shut down.
  - **Antigravity behaved differently.** In round 3, its `sleep` outlived the cancel and `agy`
    itself. Here, it ended at the cancel. Each harness ran once per round, so this run does not
    tell whether Antigravity is inconsistent or the round-3 orphan was a one-off.
  - `--kill-tree` was never the thing that stopped a process, so this run does not prove it stops
    a surviving tool. It does prove the tracking: the tool was in the tracked set every time,
    although it ran in a process group of its own.
- **H4. Nothing was killed wrongly.** `tree_killed` was empty in all three runs. Every tracked
  process descends from the harness by parent PID, and a kill also requires the command line to
  match. The tracked trees are much larger than the process groups, though. Had they still been
  alive at shutdown, `--kill-tree` would have killed all of these:
  - **Claude:** the bundled `claude`, the user's 2 MCP servers, a Python hook from a Claude plugin
    the user installed, and the tool shell.
  - **Codex:** a nested `codex app-server`, a code-mode host, Codex's computer-use helper, two
    JavaScript REPL processes from the desktop ChatGPT app, and six MCP server processes. These
    come from Codex plugins and the user's configuration: a security-review plugin, an Xcode build
    server, the reverse-engineering toolkit and others.
  - **Antigravity:** the user's 1 MCP server (2 processes) and the tool.
- **Correction to round 3.** `FOLLOWUP2-RESULTS.md` (G9) says Codex started no MCP processes. That
  is wrong. Round 3 listed only the harness's process group, and Codex runs its MCP servers outside
  it. The tracked tree shows them. MCP therefore remains a possible cause of Codex's growth between
  turns. Round 3's MCP counts for Claude and Antigravity still hold: the tree shows the same
  servers.
- Claude reported a cost (USD 0.1702) and Codex reported usage for these cancelled turns. In both,
  a model call had completed before the cancel, as in round 3.

## Antigravity with `--gemini-dir` (`agy-iso-*`)

Each run used a fresh temporary `--gemini-dir`, which the script created and deleted.

| | Plain turn | Write turn |
|---|---|---|
| H5. Stop reason | `end_turn` | `end_turn` |
| H6. Result | Replied `OK` | Replied `DONE`; `hello.txt` written |
| Permission requests | 0 | 1 (`edit`, options `allow_once`/`reject_once`, approved) |
| Wall time | 4.7 s | 15.5 s |

- **H5.** The login works on a real turn with a fresh `--gemini-dir`. This confirms the round-3
  inference.
- **H7.**
  - Model `gemini-4-carbon`, thought level `medium`, mode `default`: the same as a normal run.
  - The process group held only `agy`, with no MCP processes. In round 3 the user's MCP server
    appeared inside Antigravity's group, so its absence here is meaningful.
  - No first-run prompt, sign-in request, notice or error appeared, and stderr was empty.
  - Permission behaviour matched a normal run: a write was asked.
  - The plain turn was faster than the 8.1 s seen in the first live run. One sample cannot say why.

## Gated modes (`gated-*`)

**H8.** Prompt: `touch marker.txt`. The client approves every request.

| | Claude, `default` | Codex, `read-only` |
|---|---|---|
| `session/set_mode` | Succeeded (`config_option_update` shows `default`) | Succeeded (shows `read-only`) |
| Permission requests | 1, tool kind `execute` (`touch marker.txt`) | 1, tool kind `execute` (`touch marker.txt`) |
| Options offered | `allow_once`, `reject_once` | `allow_once`, `allow_always`, `reject_once` |
| `marker.txt` created | Yes | Yes |

Codex asked rather than refused. These are the first modes seen that route a shell command to the
client on Claude and on Codex.

## Codex confinement (`confine-*`)

**H9.** Codex in `workspace-write`, asked to write `../outside.txt`.

| Session folder | Written above cwd? | Permission request | Reply |
|---|---|---|---|
| Under `$TMPDIR` (scripted run) | Yes | None. It ran `printf hi > ../outside.txt`, then checked the file | "I'll create the file and verify its contents. DONE" |
| Outside `$TMPDIR` and `/tmp` (extra run) | Yes, **after approval** | 1, `execute` (`printf 'hi' > ../outside.txt`), options `allow_once`/`allow_always`/`reject_once`; the client approved | "I'll attempt to create the file at the requested path. DONE" |

Neither run reported a sandbox error, and neither refused.

**Reading.** `workspace-write` confines writes to the session folder plus `$TMPDIR` and `/tmp`. A
write outside those roots becomes a permission request, which the client can reject. The scripted
run's write was allowed because its parent folder sits inside `$TMPDIR`. The `$TMPDIR` part is an
inference, not a documented fact checked here: the bundled Codex 0.166.0 has `exclude_tmpdir_env_var`
and `exclude_slash_tmp` sandbox settings, which only make sense if both are writable by default. In
the extra run the file was written only because this test client approves every request.

## Not done, and why

- A run where a tool is still alive at shutdown, which would show `--kill-tree` killing it. All
  three tools had ended first. One option is a longer `sleep` with a shorter linger, but the brief
  fixes prompts and timing.
- Rejecting a request: the client approves everything, so the `reject_once` path was not tested on
  any harness.
- Repeating the Antigravity cancel, to tell whether the round-3 orphan recurs.
