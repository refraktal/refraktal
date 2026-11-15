#!/usr/bin/env bash
# M4 live checks through the real CLI, on throwaway repositories under
# .live/m4/repos (gitignored). Uses the harness logins on this machine.
#
#   bash scripts/m4check.sh mins     --mins 0.5 stops a turn running `sleep 120`; nothing may survive
#   bash scripts/m4check.sh costs    one plain turn per harness, to show each cost source
#   HARNESSES="claude agy" bash scripts/m4check.sh mins
#
# The late.txt survival test is `bash scripts/m3check.sh survive`, which now
# runs through the session sweep. Output stays in .live: never commit it.
set -u
unset CLAUDECODE CLAUDE_CODE_ENTRYPOINT CLAUDE_CODE_SSE_PORT 2>/dev/null || true
cd "$(dirname "$0")/.."
LIVE=$PWD/.live/m4
BIN=$PWD/.live/bin/refraktal
mkdir -p "$LIVE/repos" "$PWD/.live/bin"
go build -o "$BIN" ./cmd/refraktal || exit 1

MINS_TASK="${MINS_TASK:-Run the shell command: sleep 120   Then reply DONE.}"
PLAIN_TASK='Reply with the single word OK. Do not use any tools.'

mkrepo() {
  local d="$LIVE/repos/$1"
  rm -rf "$d" && mkdir -p "$d" && cd "$d" || exit 1
  git init -q -b main && printf 'x\n' > README && git add -A && git -c user.name=t -c user.email=t@t commit -q -m init
  cd - >/dev/null
}

sleepers() { ps -A -o command= | grep -cx 'sleep 120'; } # read-only: nothing is ever killed by name

summary() { # summary <result.jsonl> <repo>: outcome, budget and the sweep count from the run log
  local run; run=$(tail -n 1 "$1" | python3 -c 'import json,sys; print(json.load(sys.stdin)["run"])')
  tail -n 1 "$1" | python3 -c '
import json,sys
r=json.load(sys.stdin); b=r.get("budget") or {}; s=r.get("settlement") or {}
print("   outcome=%s stop=%s model=%s" % (r["outcome"], r.get("stop_reason"), r.get("model")))
print("   charges=%s" % [(c["source"], round(c["usd"],5), c.get("note","")) for c in b.get("charges") or []])
print("   charged=%.5f estimated=%.5f settlement=%s turns=%s" % (b.get("spent_usd",0), b.get("estimated_usd",0), s, b.get("turns")))'
  python3 - "$2/$run/events.jsonl" <<'EOF'
import json,sys
for l in open(sys.argv[1]):
    e=json.loads(l)
    if e["event"]=="run_end": print("   session sweep killed:", e.get("swept"))
    if e["event"]=="turn": print("   harness exited on its own: %s; left running after the kill: %s" % (e.get("exited_on_its_own"), e.get("left_running")))
EOF
}

t_mins() {
  [ "$(sleepers)" -eq 0 ] || { echo "a 'sleep 120' is already running: results would be ambiguous"; exit 1; }
  for h in ${HARNESSES:-claude codex agy}; do
    mkrepo "mins-$h"
    local repo="$LIVE/repos/mins-$h" out="$LIVE/mins-$h.jsonl" start=$SECONDS
    (cd "$repo" && "$BIN" run --harness "$h" --mins 0.5 --json "$MINS_TASK") </dev/null > "$out" 2> "$LIVE/mins-$h.stderr"
    local rc=$? secs=$((SECONDS - start))
    sleep 2
    echo "mins $h: exit $rc after ${secs}s; 'sleep 120' still running 2 s later: $(sleepers)"
    summary "$out" "$repo"
  done
}

t_costs() {
  for h in ${HARNESSES:-claude codex agy}; do
    mkrepo "costs-$h"
    local repo="$LIVE/repos/costs-$h" out="$LIVE/costs-$h.jsonl"
    (cd "$repo" && "$BIN" run --harness "$h" --json "$PLAIN_TASK") </dev/null > "$out" 2> "$LIVE/costs-$h.stderr"
    echo "costs $h: exit $?; notices: $(grep -c '"type":"notice"' "$out")"
    summary "$out" "$repo"
  done
}

case ${1:-} in
  mins) t_mins ;;
  costs) t_costs ;;
  *) sed -n '2,11p' "$0"; exit 2 ;;
esac
