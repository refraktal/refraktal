#!/usr/bin/env bash
# Runs one trivial prompt against each installed harness over ACP and stores,
# per harness, results/<name>.jsonl (every line), results/<name>.summary.json
# (the last line) and results/<name>.stderr.
#
#   ./run-live.sh                 claude, codex and agy, whichever are installed
#   ./run-live.sh --with-tool     also a second prompt that makes the agent write
#                                 one file in the temp dir (shows permission
#                                 requests and whether usage arrives mid-turn)
#   HARNESSES="claude agy" ./run-live.sh
#   HARNESSES=fake ./run-live.sh  offline self-test against the fake agent
#
# Safety: each run gets a fresh empty temp directory as its cwd, a 2 minute
# limit (MINS), and the client refuses file access outside that directory.
# Adapters are installed with npm into ./adapters (never globally) at pinned
# versions, without running package install scripts.
set -u
# When this script is started from inside a Claude Code session, the harnesses
# would inherit that session's markers and may refuse to start or attach to it.
# Unsetting a variable that is not set is harmless.
unset CLAUDECODE CLAUDE_CODE_ENTRYPOINT CLAUDE_CODE_SSE_PORT 2>/dev/null || true
cd "$(dirname "$0")"
HERE=$PWD
HARNESSES=${HARNESSES:-"claude codex agy"}
MINS=${MINS:-2}
PROMPT=${PROMPT:-"Reply with the single word OK. Do not use any tools."}
TOOL_PROMPT="Create a file named hello.txt in the current directory containing the single word hello, then reply DONE."
CLAUDE_ACP=@agentclientprotocol/claude-agent-acp@0.107.0
CODEX_ACP=@agentclientprotocol/codex-acp@2.6.0
WITH_TOOL=0
[ "${1:-}" = "--with-tool" ] && WITH_TOOL=1

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

run() { # run <harness> <label> <prompt> <command line of the ACP server>
  local dir; dir=$(mktemp -d "${TMPDIR:-/tmp}/acp-live-$1-XXXXXX")
  echo "== $2: $4 (cwd $dir)"
  "$HERE/bin/acp-spike" --harness "$1" --cmd "$4" --cwd "$dir" --mins "$MINS" "$3" \
    >"results/$2.jsonl" 2>"results/$2.stderr"
  echo "   exit $? ; files left in cwd: $(ls -A "$dir" | tr '\n' ' ')"
  tail -n 1 "results/$2.jsonl" | tee "results/$2.summary.json"
  rm -rf "$dir"
}

for h in $HARNESSES; do
  case $h in
    claude)
      command -v claude >/dev/null && command -v npm >/dev/null || { echo "== claude: skipped (claude or npm not on PATH)"; continue; }
      cmd=$(adapter "$CLAUDE_ACP" claude-agent-acp) || { echo "== claude: skipped (adapter install failed)"; continue; } ;;
    codex)
      command -v codex >/dev/null && command -v npm >/dev/null || { echo "== codex: skipped (codex or npm not on PATH)"; continue; }
      cmd=$(adapter "$CODEX_ACP" codex-acp) || { echo "== codex: skipped (adapter install failed)"; continue; } ;;
    agy)
      command -v agy >/dev/null || { echo "== agy: skipped (agy not on PATH)"; continue; }
      cmd="$(command -v agy) acp" ;;
    fake)
      cmd="$HERE/bin/fakeagent" ;;
    *) echo "== $h: unknown harness, skipped"; continue ;;
  esac
  run "$h" "$h" "$PROMPT" "$cmd"
  [ "$WITH_TOOL" = 1 ] && run "$h" "$h-tool" "$TOOL_PROMPT" "$cmd"
done
echo "Done. See results/ and compare with FINDINGS.md."
