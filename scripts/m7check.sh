#!/usr/bin/env bash
# M7 live check through the real CLI: a Refraktal driving a child Refraktal
# over ACP (`refraktal run --harness refraktal` starts `refraktal acp`), with
# tight budgets, on throwaway repositories under .live/m7 (gitignored). The
# child routes with local rules: no API key reaches it.
#
#   bash scripts/m7check.sh          nest, depth and guard
#     nest   the child drives a harness; show renders the tree
#     depth  --depth 0: the child's depth is spent, it runs nothing
#     guard  at an ancestor's nesting ceiling, --depth generous: the guard stops the child
#
# Prints a summary only. Output stays in .live: never commit it.
set -u
unset CLAUDECODE CLAUDE_CODE_ENTRYPOINT CLAUDE_CODE_SSE_PORT GEMINI_API_KEY GOOGLE_API_KEY REFRAKTAL_NEST 2>/dev/null || true
export REFRAKTAL_LIVE=1 # live checks start real harnesses; go test never does
cd "$(dirname "$0")/.."
LIVE=$PWD/.live/m7
BIN=$PWD/.live/bin/refraktal
mkdir -p "$LIVE" "$PWD/.live/bin"
go build -o "$BIN" ./cmd/refraktal || exit 1
LIVEDIR="$(python3 -c 'import os,sys; print(os.path.expanduser("~/Library/Application Support/refraktal/live") if sys.platform=="darwin" else os.path.join(os.environ.get("XDG_CONFIG_HOME", os.path.expanduser("~/.config")), "refraktal/live"))')"
TASK='Create a file named hello.txt containing the single line: hello. Do nothing else.'

mkrepo() {
  rm -rf "$1" && mkdir -p "$1" && cd "$1" || exit 1
  git init -q -b main && printf 'x\n' > README && git add -A && git -c user.name=t -c user.email=t@t commit -q -m init
  cd - >/dev/null
}

summary() { # summary <name> <repo> <rc> <secs>
  echo "== $1: exit $3 after ${4}s; live records now $(ls "$LIVEDIR" 2>/dev/null | wc -l | tr -d ' '); adapter processes running: $(ps -A -o command= | grep -c '[a]gy acp\|[c]laude-agent-acp\|[c]odex-acp'); refraktal acp children running: $(ps -A -o command= | grep -c "^$BIN acp")"
  python3 - "$LIVE/$1.jsonl" "$2" <<'PY'
import json, os, sys
r = json.loads(open(sys.argv[1]).read().strip().splitlines()[-1]); repo = sys.argv[2]
b = r.get("budget") or {}
print("outcome=%s harness=%s files=%s; charged $%.4f of $%.2f; turns %d of %d (depth %s)" % (r["outcome"], r.get("harness"), r.get("files"),
      b.get("spent_usd", 0), b["limits"]["usd"], b.get("turns", 0), b["limits"]["turns"], b["limits"].get("depth")))
print("charges:", [(c["source"], round(c["usd"], 4)) for c in b.get("charges") or []])
for l in open(os.path.join(repo, r["run"], "events.jsonl")):
    e = json.loads(l)
    if e["event"] == "child_run": print("child Refraktal reported: outcome=%s exit=%s prompts=%s" % (e.get("outcome"), e.get("exit"), e.get("turns")))
print("hello.txt in the user tree:", os.path.exists(os.path.join(repo, "hello.txt")), "; left_running=%s unknown=%s" % (r.get("left_running"), r.get("unknown")))
PY
  echo "-- refraktal show:"
  (cd "$2" && "$BIN" show) | sed "s#$HOME#~#g"
}

one() { # one <name> <extra env...> -- <run flags...>
  local name=$1; shift
  local repo=$LIVE/$name; mkrepo "$repo"
  local env=()
  while [ "$1" != "--" ]; do env+=("$1"); shift; done; shift
  local start=$SECONDS
  (cd "$repo" && env ${env[@]+"${env[@]}"} "$BIN" run --harness refraktal --json "$@" "$TASK") </dev/null > "$LIVE/$name.jsonl" 2> "$LIVE/$name.stderr"
  local rc=$?
  sleep 2
  summary "$name" "$repo" $rc $((SECONDS - start))
}

case ${1:-all} in
  nest) one nest -- --usd 1 --mins 4 --turns 6 --depth 1 ;;
  depth) one depth -- --usd 1 --mins 2 --turns 6 --depth 0 ;;
  guard) one guard REFRAKTAL_NEST=6 REFRAKTAL_NEST_CEILING=6 -- --usd 1 --mins 2 --turns 6 --depth 5 ;;
  all)
    one nest -- --usd 1 --mins 4 --turns 6 --depth 1
    one depth -- --usd 1 --mins 2 --turns 6 --depth 0
    one guard REFRAKTAL_NEST=6 REFRAKTAL_NEST_CEILING=6 -- --usd 1 --mins 2 --turns 6 --depth 5 ;;
  *) sed -n '2,12p' "$0"; exit 2 ;;
esac
