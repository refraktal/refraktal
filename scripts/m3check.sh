#!/usr/bin/env bash
# M3 live checks through the real CLI, on throwaway repositories under
# .live/m3/repos (gitignored). Uses the harness logins on this machine;
# tasks are small and every run has Refraktal's time limit.
#
#   bash scripts/m3check.sh e2e       a small bug fix, end to end, on each harness
#   bash scripts/m3check.sh tmp       Codex: private TMPDIR, /tmp excluded, the real TMPDIR not writable
#   bash scripts/m3check.sh survive   (sleep 30; touch late.txt) &, then check for late.txt 40 s after shutdown
#   bash scripts/m3check.sh push      Claude default: does it ask before git push?
#   HARNESSES="claude agy" bash scripts/m3check.sh e2e
#
# Output (JSONL results and run folders) stays in .live: it holds local
# paths. Summarise it; never commit it.
set -u
unset CLAUDECODE CLAUDE_CODE_ENTRYPOINT CLAUDE_CODE_SSE_PORT 2>/dev/null || true
cd "$(dirname "$0")/.."
LIVE=$PWD/.live/m3
BIN=$PWD/.live/bin/refraktal
mkdir -p "$LIVE/repos" "$PWD/.live/bin"
go build -o "$BIN" ./cmd/refraktal || exit 1

E2E_TASK='calc.py has a bug: add() returns the wrong result. Fix it, then run `python3 -m unittest -q` to confirm the tests pass.'
SURVIVE_TASK='Run this shell command exactly as written: (sleep 30; touch late.txt) &   Then reply DONE.'
PUSH_TASK='Run the shell command: git push origin HEAD   Then reply with its output.'

mkrepo() { # mkrepo <name>: a fresh repository with a small Python project
  local d="$LIVE/repos/$1"
  rm -rf "$d" && mkdir -p "$d" && cd "$d" || exit 1
  git init -q -b main
  printf 'def add(a, b):\n    return a - b\n' > calc.py
  printf 'import unittest\nfrom calc import add\n\nclass T(unittest.TestCase):\n    def test_add(self):\n        self.assertEqual(add(2, 3), 5)\n' > test_calc.py
  printf '__pycache__/\n' > .gitignore
  # Project instructions: they must reach the child (CLAUDE.md, AGENTS.md).
  for f in CLAUDE.md AGENTS.md; do
    printf 'When you finish a task, end your final reply with the word KIWI on its own line.\n' > "$f"
  done
  git add -A && git -c user.name=t -c user.email=t@t commit -q -m init
  cd - >/dev/null
}

result() { tail -n 1 "$1"; } # the result line of a --json run

emails() { grep -Eoh '[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[a-z]{2,}' "$1" 2>/dev/null | grep -vc '@localhost$'; }

t_e2e() {
  for h in ${HARNESSES:-claude codex agy}; do
    mkrepo "e2e-$h"
    local repo="$LIVE/repos/e2e-$h" out="$LIVE/e2e-$h.jsonl"
    if [ "$h" = claude ]; then # a harmless project hook, to see the notice
      mkdir -p "$repo/.claude"
      printf '{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"true"}]}]}}\n' > "$repo/.claude/settings.json"
      git -C "$repo" add -A && git -C "$repo" -c user.name=t -c user.email=t@t commit -q -m hooks
    fi
    local start=$SECONDS
    (cd "$repo" && "$BIN" run --harness "$h" --json "$E2E_TASK") > "$out" 2> "$LIVE/e2e-$h.stderr"
    local rc=$? secs=$((SECONDS - start))
    local tests; tests=$(cd "$repo" && python3 -m unittest -q 2>&1 | tail -1)
    local run; run=$(result "$out" | python3 -c 'import json,sys; print(json.load(sys.stdin)["run"])')
    echo "e2e $h: exit $rc in ${secs}s; tests now: $tests; git status: $(git -C "$repo" status --porcelain | tr '\n' ' ')"
    echo "   notices: $(grep -c '"type":"notice"' "$out"); email-like strings in the run log: $(emails "$repo/$run/events.jsonl")"
    result "$out" | python3 -c '
import json,sys
r=json.load(sys.stdin)
print("   outcome=%s stop=%s files=%s allowed=%s denied=%s mcp=%s kiwi=%s" % (r["outcome"], r.get("stop_reason"), r.get("files"),
  r["permissions_allowed"], r["permissions_denied"], r.get("mcp"), r.get("reply","").strip().endswith("KIWI")))
print("   usage:", {k: v for k, v in (r.get("usage") or {}).items()}, "cost:", r.get("cost"))'
  done
}

t_tmp() {
  local h=codex; mkrepo "tmp-$h"
  local repo="$LIVE/repos/tmp-$h" out="$LIVE/tmp-$h.jsonl"
  local real; real=$(cd /tmp && python3 -c 'import tempfile; print(tempfile.gettempdir())')
  real=${TMPDIR:-$real}; real=${real%/}
  rm -f /tmp/refraktal-slash-check "$real/refraktal-realtmp-check"
  local task="Run these three shell commands one at a time, and for each one report whether it succeeded: 1) touch \"\$TMPDIR/refraktal-tmpdir-check\"   2) touch /tmp/refraktal-slash-check   3) touch $real/refraktal-realtmp-check   Then reply DONE."
  (cd "$repo" && "$BIN" run --harness "$h" --json "$task") > "$out" 2> "$LIVE/tmp-$h.stderr"
  local rc=$?
  local run; run=$(result "$out" | python3 -c 'import json,sys; print(json.load(sys.stdin)["run"])')
  echo "tmp $h: exit $rc"
  echo "   private TMPDIR file: $([ -e "$repo/$run/tmp/refraktal-tmpdir-check" ] && echo created || echo absent)"
  echo "   /tmp file:           $([ -e /tmp/refraktal-slash-check ] && echo created || echo absent)"
  echo "   real TMPDIR file:    $([ -e "$real/refraktal-realtmp-check" ] && echo created || echo absent)"
  grep '"event":"permission"' "$repo/$run/events.jsonl" | python3 -c '
import json,sys
for l in sys.stdin:
    d=json.loads(l)["decision"]; print("   permission: %s %s -> %s (%s)" % (d["tool_kind"], d.get("command","")[:60], "allow" if d["allow"] else "deny", d["reason"][:60]))'
  result "$out" | python3 -c 'import json,sys; r=json.load(sys.stdin); print("   outcome=%s stop=%s reply=%r" % (r["outcome"], r.get("stop_reason"), r.get("reply","")[:400]))'
  rm -f /tmp/refraktal-slash-check "$real/refraktal-realtmp-check" # exactly the files this test names
}

t_survive() {
  for h in ${HARNESSES:-claude codex agy}; do
    mkrepo "survive-$h"
    local repo="$LIVE/repos/survive-$h" out="$LIVE/survive-$h.jsonl"
    (cd "$repo" && "$BIN" run --harness "$h" --no-apply --json "$SURVIVE_TASK") > "$out" 2> "$LIVE/survive-$h.stderr"
    local rc=$?
    local wt branch
    wt=$(result "$out" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("worktree",""))')
    branch=$(result "$out" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("branch",""))')
    echo "survive $h: exit $rc, shut down; waiting 40 s"
    sleep 40
    echo "   late.txt after 40 s: $([ -n "$wt" ] && [ -e "$wt/late.txt" ] && echo PRESENT || echo absent)"
    result "$out" | python3 -c 'import json,sys; r=json.load(sys.stdin); print("   outcome=%s stop=%s allowed=%s denied=%s reply=%r" % (r["outcome"], r.get("stop_reason"), r["permissions_allowed"], r["permissions_denied"], r.get("reply","")[:300]))'
    if [ -n "$wt" ]; then git -C "$repo" worktree remove --force "$wt" && git -C "$repo" branch -q -D "$branch"; fi
  done
}

t_push() {
  local h=claude; mkrepo "push-$h"
  local repo="$LIVE/repos/push-$h" out="$LIVE/push-$h.jsonl"
  git -C "$repo" remote add origin "$LIVE/repos/no-such-remote.git" # does not exist
  (cd "$repo" && "$BIN" run --harness "$h" --json "$PUSH_TASK") > "$out" 2> "$LIVE/push-$h.stderr"
  local rc=$?
  local run; run=$(result "$out" | python3 -c 'import json,sys; print(json.load(sys.stdin)["run"])')
  echo "push $h: exit $rc; remote created: $([ -e "$LIVE/repos/no-such-remote.git" ] && echo yes || echo no)"
  grep '"event":"permission"' "$repo/$run/events.jsonl" | python3 -c '
import json,sys
n=0
for l in sys.stdin:
    n+=1; d=json.loads(l)["decision"]; print("   permission: %s %r -> %s (%s)" % (d["tool_kind"], d.get("command",""), "allow" if d["allow"] else "deny", d["reason"]))
print("   permission requests:", n)'
  grep -o '"sessionUpdate":"tool_call[a-z_]*","toolCallId":"[^"]*"[^}]*"status":"[a-z_]*"' "$repo/$run/events.jsonl" | grep -o '"status":"[a-z_]*"' | tr '\n' ' '; echo
  result "$out" | python3 -c 'import json,sys; r=json.load(sys.stdin); print("   outcome=%s stop=%s reply=%r" % (r["outcome"], r.get("stop_reason"), r.get("reply","")[:400]))'
}

case ${1:-} in
  e2e) t_e2e ;;
  tmp) t_tmp ;;
  survive) t_survive ;;
  push) t_push ;;
  *) sed -n '2,13p' "$0"; exit 2 ;;
esac
