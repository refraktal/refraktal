#!/usr/bin/env bash
# Follow-up tests after the first live run. Each subcommand is one test; run
# them in the order listed. Results go to results/followup-<test>-<harness>.*
#
#   bash followup.sh exit          A17 + which binary runs: handshake only, no prompt, 0 tokens
#   bash followup.sh cancel        session/cancel: the tool prompt, cancelled 3 s into the turn
#   bash followup.sh agy-perms     A14: Antigravity only, read a file and run `echo hi`,
#                                  then `echo hi` again in mode `auto`
#   bash followup.sh claude-turns  does Claude's cost accumulate across turns in one session?
#   bash followup.sh all           all four, in this order
#
#   HARNESSES="claude agy" bash followup.sh cancel     limit to some harnesses
#   HARNESSES=fake bash followup.sh all                offline self-test
#
# Safety: every run gets a fresh empty temp directory as its cwd and a time
# limit. Adapters are installed into ./adapters at pinned versions without
# install scripts, exactly as run-live.sh does.
set -u
# Harnesses inherit a parent Claude Code session's markers otherwise.
unset CLAUDECODE CLAUDE_CODE_ENTRYPOINT CLAUDE_CODE_SSE_PORT 2>/dev/null || true
cd "$(dirname "$0")"
HERE=$PWD
HARNESSES=${HARNESSES:-"claude codex agy"}
CLAUDE_ACP=@agentclientprotocol/claude-agent-acp@0.107.0
CODEX_ACP=@agentclientprotocol/codex-acp@2.6.0
PLAIN_PROMPT="Reply with the single word OK. Do not use any tools."
TOOL_PROMPT="Create a file named hello.txt in the current directory containing the single word hello, then reply DONE."
READ_PROMPT="Read the file notes.txt in the current directory and reply with its contents only."
ECHO_PROMPT="Run the shell command: echo hi   Then reply with its output only."

command -v go >/dev/null || { echo "go is required" >&2; exit 1; }
mkdir -p bin results
go build -o bin/acp-spike . && go build -o bin/fakeagent ./cmd/fakeagent || exit 1

adapter() { # adapter <package@version> <bin name>: prints the local binary path
  local bin="$HERE/adapters/node_modules/.bin/$2"
  if [ ! -x "$bin" ]; then
    mkdir -p "$HERE/adapters"
    npm install --prefix "$HERE/adapters" --ignore-scripts --no-audit --no-fund "$1" >&2 || return 1
  fi
  echo "$bin"
}

server_cmd() { # server_cmd <harness>: prints the ACP server command line, or nothing if unavailable
  case $1 in
    claude) command -v claude >/dev/null && command -v npm >/dev/null && adapter "$CLAUDE_ACP" claude-agent-acp ;;
    codex)  command -v codex >/dev/null && command -v npm >/dev/null && adapter "$CODEX_ACP" codex-acp ;;
    agy)    command -v agy >/dev/null && echo "$(command -v agy) acp" ;;
    fake)   echo "$HERE/bin/fakeagent" ;;
  esac
}

# run <harness> <label> <seed: none|notes> <client flags...> -- <prompt or empty>
run() {
  local h=$1 label=$2 seed=$3; shift 3
  local flags=() prompt=""
  while [ $# -gt 0 ] && [ "$1" != "--" ]; do flags+=("$1"); shift; done
  [ "${1:-}" = "--" ] && { shift; prompt="${1:-}"; }
  local cmd; cmd=$(server_cmd "$h") || cmd=""
  if [ -z "$cmd" ]; then echo "== $label: skipped ($h not available)"; return; fi
  local dir; dir=$(mktemp -d "${TMPDIR:-/tmp}/acp-followup-XXXXXX")
  [ "$seed" = notes ] && echo "refraktal-check-7" > "$dir/notes.txt"
  echo "== $label (cwd $dir)"
  "$HERE/bin/acp-spike" --harness "$h" --cmd "$cmd" --cwd "$dir" "${flags[@]}" ${prompt:+"$prompt"} \
    >"results/followup-$label.jsonl" 2>"results/followup-$label.stderr"
  local rc=$? files; files=$(ls -A "$dir" | tr '\n' ' ')
  echo "   exit $rc ; files left in cwd: $files"
  printf 'exit %s\nfiles left in cwd: %s\n' "$rc" "$files" > "results/followup-$label.files"
  tail -n 1 "results/followup-$label.jsonl" > "results/followup-$label.summary.json"
  rm -rf "$dir"
}

t_exit() {     # zero tokens: initialize + session/new, then close stdin and see what is left
  for h in $HARNESSES; do run "$h" "exit-$h" none --handshake --linger 3s --grace 10s --; done
}
t_cancel() {   # cancel 3 s into the tool turn; wait up to 15 s for the harness to stop
  for h in $HARNESSES; do run "$h" "cancel-$h" none --mins 0.05 --grace 15s --linger 5s -- "$TOOL_PROMPT"; done
}
t_agy_perms() { # Antigravity only (fake for the self-test)
  local h=agy; [ "$HARNESSES" = fake ] && h=fake
  run "$h" "perms-read-$h"      notes --mins 3 -- "$READ_PROMPT"
  run "$h" "perms-echo-$h"      none  --mins 3 -- "$ECHO_PROMPT"
  run "$h" "perms-echo-auto-$h" none  --mins 3 --mode auto -- "$ECHO_PROMPT"
}
t_claude_turns() { # Claude only (fake for the self-test): the same plain prompt twice in one session
  local h=claude; [ "$HARNESSES" = fake ] && h=fake
  run "$h" "turns-$h" none --mins 2 --turns 2 --linger 3s -- "$PLAIN_PROMPT"
}

case ${1:-} in
  exit) t_exit ;;
  cancel) t_cancel ;;
  agy-perms) t_agy_perms ;;
  claude-turns) t_claude_turns ;;
  all) t_exit; t_cancel; t_agy_perms; t_claude_turns ;;
  *) sed -n '2,15p' "$0"; exit 2 ;;
esac
echo "Done. Summaries: results/followup-*.summary.json"
