#!/usr/bin/env bash
# M1 and M3 open tests at the protocol level, against live harnesses. Uses the harness logins on this
# machine; prompts are trivial and every run has a time limit.
#
#   bash scripts/livecheck.sh kill      `sleep 120 &`, then shutdown (all harnesses)
#   bash scripts/livecheck.sh orphan    Antigravity mid-tool cancel, three times
#   bash scripts/livecheck.sh reject    M3: reject `touch marker.txt` in each harness's gated mode
#   HARNESSES="claude agy" bash scripts/livecheck.sh kill
#   WATCH=10ms bash scripts/livecheck.sh kill      look for the tool more often
#
# Output goes to .live/results (gitignored): it holds local paths. Summarise
# it; never commit it. Adapters are installed into .live/adapters at pinned
# versions, without install scripts.
set -eu
unset CLAUDECODE CLAUDE_CODE_ENTRYPOINT CLAUDE_CODE_SSE_PORT 2>/dev/null || true
export REFRAKTAL_LIVE=1 # live checks start real harnesses; go test never does
cd "$(dirname "$0")/.."
ROOT=$PWD/.live
CLAUDE_ACP=@agentclientprotocol/claude-agent-acp@0.107.0
CODEX_ACP=@agentclientprotocol/codex-acp@2.6.0
mkdir -p "$ROOT/bin"
go build -o "$ROOT/bin/livecheck" ./scripts/livecheck

adapter() { # adapter <package@version> <bin>: prints the local binary
  local bin="$ROOT/adapters/node_modules/.bin/$2"
  if [ ! -x "$bin" ]; then
    npm install --prefix "$ROOT/adapters" --ignore-scripts --no-audit --no-fund "$1" >&2
  fi
  echo "$bin"
}

server_cmd() {
  case $1 in
    claude) adapter "$CLAUDE_ACP" claude-agent-acp ;;
    codex)  adapter "$CODEX_ACP" codex-acp ;;
    agy)    echo "$(command -v agy) acp" ;;
  esac
}

case ${1:-} in
  kill)
    for h in ${HARNESSES:-claude codex agy}; do
      "$ROOT/bin/livecheck" -test kill -harness "$h" -cmd "$(server_cmd "$h")" -root "$ROOT" ${WATCH:+-watch "$WATCH"}
    done ;;
  reject)
    for h in ${HARNESSES:-claude codex agy}; do
      "$ROOT/bin/livecheck" -test reject -harness "$h" -cmd "$(server_cmd "$h")" -root "$ROOT"
    done ;;
  orphan)
    "$ROOT/bin/livecheck" -test orphan -harness agy -cmd "$(server_cmd agy)" -runs 3 -root "$ROOT" ${WATCH:+-watch "$WATCH"} ;;
  *) sed -n '2,12p' "$0"; exit 2 ;;
esac
