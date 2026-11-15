#!/usr/bin/env bash
# Round 4 of live tests. Results go to results/followup3-<test>-<harness>.*
#
#   bash followup3.sh treekill    cancel 10 s into `sleep 20` with --kill-tree (all harnesses);
#                                 then check whether any `sleep 20` is still alive
#   bash followup3.sh agy-iso     Antigravity with --gemini-dir <fresh temp dir>: a plain turn and a file write
#   bash followup3.sh gated       Claude in `default`, Codex in `read-only`: `touch marker.txt`
#   bash followup3.sh confine     Codex in `workspace-write`: try to write ../outside.txt
#   bash followup3.sh all         all four, in this order
#
#   HARNESSES=fake bash followup3.sh all     offline self-test
set -u
unset CLAUDECODE CLAUDE_CODE_ENTRYPOINT CLAUDE_CODE_SSE_PORT 2>/dev/null || true
cd "$(dirname "$0")"
HERE=$PWD
HARNESSES=${HARNESSES:-"claude codex agy"}
CLAUDE_ACP=@agentclientprotocol/claude-agent-acp@0.107.0
CODEX_ACP=@agentclientprotocol/codex-acp@2.6.0
PLAIN_PROMPT="Reply with the single word OK. Do not use any tools."
TOOL_PROMPT="Create a file named hello.txt in the current directory containing the single word hello, then reply DONE."
SLEEP_PROMPT="First run the shell command: sleep 20   After it finishes, create a file named hello.txt in the current directory containing the single word hello, then reply DONE."
TOUCH_PROMPT="Run the shell command: touch marker.txt   Then reply DONE."
OUTSIDE_PROMPT="Create a file at the path ../outside.txt (one directory above the current directory) containing the single word hi, then reply DONE. If you cannot, reply with the reason."
AGY_ACP_ARGS=""

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

server_cmd() {
  case $1 in
    claude) command -v claude >/dev/null && command -v npm >/dev/null && adapter "$CLAUDE_ACP" claude-agent-acp ;;
    codex)  command -v codex >/dev/null && command -v npm >/dev/null && adapter "$CODEX_ACP" codex-acp ;;
    agy)    command -v agy >/dev/null && echo "$(command -v agy) acp $AGY_ACP_ARGS" ;;
    fake)   echo "$HERE/bin/fakeagent" ;;
  esac
}

sleepers() { # PIDs of any `sleep 20` on the machine
  ps -A -o pid= -o command= | awk '$2 == "sleep" && $3 == "20" && NF == 3 {print $1}' | tr '\n' ' '
}

# run <harness> <label> <layout: flat|nested> <client flags...> -- <prompt or empty>
# nested: the session cwd is <tmp>/work, so a write to ../ lands in <tmp>.
run() {
  local h=$1 label=$2 layout=$3; shift 3
  local flags=() prompt=""
  while [ $# -gt 0 ] && [ "$1" != "--" ]; do flags+=("$1"); shift; done
  [ "${1:-}" = "--" ] && { shift; prompt="${1:-}"; }
  local cmd; cmd=$(server_cmd "$h") || cmd=""
  if [ -z "$cmd" ]; then echo "== $label: skipped ($h not available)"; return; fi
  local top dir; top=$(mktemp -d "${TMPDIR:-/tmp}/acp-followup3-XXXXXX"); dir=$top
  [ "$layout" = nested ] && { dir="$top/work"; mkdir "$dir"; }
  echo "== $label (cwd $dir)"
  "$HERE/bin/acp-spike" --harness "$h" --cmd "$cmd" --cwd "$dir" "${flags[@]}" ${prompt:+"$prompt"} \
    >"results/followup3-$label.jsonl" 2>"results/followup3-$label.stderr"
  local rc=$?
  {
    echo "exit $rc"
    echo "files left in cwd: $(ls -A "$dir" | tr '\n' ' ')"
    [ "$layout" = nested ] && echo "files left above cwd: $(ls -A "$top" | grep -v '^work$' | tr '\n' ' ')"
  } > "results/followup3-$label.files"
  sed 's/^/   /' "results/followup3-$label.files"
  tail -n 1 "results/followup3-$label.jsonl" > "results/followup3-$label.summary.json"
  rm -rf "$top"
}

one() { if [ "$HARNESSES" = fake ]; then echo fake; return; fi; case " $HARNESSES " in *" $1 "*) echo "$1" ;; esac; }

t_treekill() {
  for h in $HARNESSES; do
    local before; before=$(sleepers)
    run "$h" "treekill-$h" flat --step "10:$SLEEP_PROMPT" --grace 15s --linger 5s --kill-tree --
    { echo "sleep 20 before the run: ${before:-none}"
      local now; now=$(sleepers); echo "sleep 20 right after the run: ${now:-none}"
      sleep 3
      now=$(sleepers); echo "sleep 20 3 s after the run: ${now:-none}"; } | tee -a "results/followup3-treekill-$h.files" | sed 's/^/   /'
  done
}
t_agy_iso() {
  local h; h=$(one agy); [ -z "$h" ] && return
  local gdir; gdir=$(mktemp -d "${TMPDIR:-/tmp}/agy-gemini-dir-XXXXXX")
  [ "$h" = agy ] && AGY_ACP_ARGS="--gemini-dir $gdir"
  run "$h" "agy-iso-plain" flat --mins 3 -- "$PLAIN_PROMPT"
  run "$h" "agy-iso-write" flat --mins 3 -- "$TOOL_PROMPT"
  AGY_ACP_ARGS=""
  rm -rf "$gdir"
}
t_gated() {
  local h
  h=$(one claude); [ -n "$h" ] && run "$h" "gated-default-$h" flat --mins 3 --mode default -- "$TOUCH_PROMPT"
  h=$(one codex);  [ -n "$h" ] && [ "$h" != fake ] && run "$h" "gated-read-only-$h" flat --mins 3 --mode read-only -- "$TOUCH_PROMPT"
}
t_confine() {
  local h; h=$(one codex); [ -z "$h" ] && return
  [ "$h" = fake ] && { run fake "confine-fake" nested --mins 1 -- "$OUTSIDE_PROMPT"; return; }
  run "$h" "confine-workspace-write-$h" nested --mins 3 --mode workspace-write -- "$OUTSIDE_PROMPT"
}

case ${1:-} in
  treekill) t_treekill ;;
  agy-iso) t_agy_iso ;;
  gated) t_gated ;;
  confine) t_confine ;;
  all) t_treekill; t_agy_iso; t_gated; t_confine ;;
  *) sed -n '2,11p' "$0"; exit 2 ;;
esac
echo "Done. Summaries: results/followup3-*.summary.json"
