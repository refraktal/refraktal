#!/usr/bin/env bash
# Third round of live tests. Each subcommand is one test; run them in the order
# listed. Results go to results/followup2-<test>-<harness>.*
#
#   bash followup2.sh midtool       cancel 10 s into a turn that is running `sleep 20` (all harnesses)
#   bash followup2.sh cancel-spend  Claude: plain,plain,plain vs plain,CANCELLED,plain in one session each
#   bash followup2.sh modes         Claude in acceptEdits, Codex in workspace-write: echo, then a file write
#   bash followup2.sh turns         Codex and Antigravity: two plain turns in one session
#   bash followup2.sh exit-late     Claude: handshake only; list survivors at 0.2, 1, 5 and 30 s
#   bash followup2.sh mcp           Claude and Antigravity: handshake only, to see which MCP servers start.
#                                   Set SESSION_META='{...}' and/or environment variables to try a way
#                                   of disabling the user's MCP servers; MCP_LABEL names the attempt
#   bash followup2.sh growth        Claude: two plain turns in one session, with the same SESSION_META
#                                   and environment as the mcp attempt that worked
#                                   For Antigravity, AGY_ACP_ARGS adds flags after `agy acp`
#   bash followup2.sh all           midtool, cancel-spend, modes, turns, exit-late (not mcp/growth)
#
#   HARNESSES=fake bash followup2.sh all     offline self-test
set -u
unset CLAUDECODE CLAUDE_CODE_ENTRYPOINT CLAUDE_CODE_SSE_PORT 2>/dev/null || true
cd "$(dirname "$0")"
HERE=$PWD
HARNESSES=${HARNESSES:-"claude codex agy"}
CLAUDE_ACP=@agentclientprotocol/claude-agent-acp@0.107.0
CODEX_ACP=@agentclientprotocol/codex-acp@2.6.0
PLAIN_PROMPT="Reply with the single word OK. Do not use any tools."
TOOL_PROMPT="Create a file named hello.txt in the current directory containing the single word hello, then reply DONE."
ECHO_PROMPT="Run the shell command: echo hi   Then reply with its output only."
SLEEP_PROMPT="First run the shell command: sleep 20   After it finishes, create a file named hello.txt in the current directory containing the single word hello, then reply DONE."
SESSION_META=${SESSION_META:-}
MCP_LABEL=${MCP_LABEL:-default}

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
    agy)    command -v agy >/dev/null && echo "$(command -v agy) acp ${AGY_ACP_ARGS:-}" ;;
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
  local dir; dir=$(mktemp -d "${TMPDIR:-/tmp}/acp-followup2-XXXXXX")
  [ "$seed" = notes ] && echo "refraktal-check-7" > "$dir/notes.txt"
  echo "== $label (cwd $dir)"
  "$HERE/bin/acp-spike" --harness "$h" --cmd "$cmd" --cwd "$dir" "${flags[@]}" ${prompt:+"$prompt"} \
    >"results/followup2-$label.jsonl" 2>"results/followup2-$label.stderr"
  local rc=$? files; files=$(ls -A "$dir" | tr '\n' ' ')
  echo "   exit $rc ; files left in cwd: $files"
  printf 'exit %s\nfiles left in cwd: %s\n' "$rc" "$files" > "results/followup2-$label.files"
  tail -n 1 "results/followup2-$label.jsonl" > "results/followup2-$label.summary.json"
  rm -rf "$dir"
}

one() { # one <wanted harness>: prints it, or fake in the self-test, or nothing if not in HARNESSES
  if [ "$HARNESSES" = fake ]; then echo fake; return; fi
  case " $HARNESSES " in *" $1 "*) echo "$1" ;; esac
}
meta() { [ -n "$SESSION_META" ] && printf '%s\n' --session-meta "$SESSION_META"; }

t_midtool() {
  for h in $HARNESSES; do
    run "$h" "midtool-$h" none --step "10:$SLEEP_PROMPT" --grace 15s --linger 5s --
  done
}
t_cancel_spend() {
  local h; h=$(one claude); [ -z "$h" ] && { echo "== cancel-spend: skipped (claude not in HARNESSES)"; return; }
  run "$h" "spend-control-$h" none --mins 2 --linger 3s \
    --step "0:$PLAIN_PROMPT" --step "0:$PLAIN_PROMPT" --step "0:$PLAIN_PROMPT" --
  run "$h" "spend-cancel-$h" none --mins 2 --grace 15s --linger 3s \
    --step "0:$PLAIN_PROMPT" --step "3:$TOOL_PROMPT" --step "0:$PLAIN_PROMPT" --
}
t_modes() {
  local h
  h=$(one claude); if [ -n "$h" ]; then
    run "$h" "mode-echo-acceptEdits-$h"  none --mins 3 --mode acceptEdits -- "$ECHO_PROMPT"
    run "$h" "mode-write-acceptEdits-$h" none --mins 3 --mode acceptEdits -- "$TOOL_PROMPT"
  fi
  h=$(one codex); if [ -n "$h" ]; then
    run "$h" "mode-echo-workspace-write-$h"  none --mins 3 --mode workspace-write -- "$ECHO_PROMPT"
    run "$h" "mode-write-workspace-write-$h" none --mins 3 --mode workspace-write -- "$TOOL_PROMPT"
  fi
}
t_turns() {
  local h
  for want in codex agy; do
    h=$(one $want); [ -z "$h" ] && continue
    [ "$h" = fake ] && [ "$want" = agy ] && continue   # one fake run is enough
    run "$h" "turns-$want" none --mins 3 --turns 2 --linger 3s -- "$PLAIN_PROMPT"
  done
}
t_exit_late() {
  local h; h=$(one claude); [ -z "$h" ] && return
  run "$h" "exit-late-$h" none --handshake --linger 3s --grace 10s --survivor-samples 200ms,1s,5s,30s --
}
t_mcp() {
  local h
  for want in claude agy; do
    h=$(one $want); [ -z "$h" ] && continue
    [ "$h" = fake ] && [ "$want" = agy ] && continue
    # shellcheck disable=SC2046
    run "$h" "mcp-$MCP_LABEL-$want" none --handshake --linger 5s $(meta) --
  done
}
t_growth() {
  local h; h=$(one claude); [ -z "$h" ] && return
  # shellcheck disable=SC2046
  run "$h" "growth-$MCP_LABEL-$h" none --mins 2 --turns 2 --linger 3s $(meta) -- "$PLAIN_PROMPT"
}

case ${1:-} in
  midtool) t_midtool ;;
  cancel-spend) t_cancel_spend ;;
  modes) t_modes ;;
  turns) t_turns ;;
  exit-late) t_exit_late ;;
  mcp) t_mcp ;;
  growth) t_growth ;;
  all) t_midtool; t_cancel_spend; t_modes; t_turns; t_exit_late ;;
  *) sed -n '2,22p' "$0"; exit 2 ;;
esac
echo "Done. Summaries: results/followup2-*.summary.json"
