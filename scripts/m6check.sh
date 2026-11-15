#!/usr/bin/env bash
# M6 live check through the real CLI: one trivial task fanned out to all
# three harnesses with tight budgets, on a throwaway repository under
# .live/m6 (gitignored). Uses the harness logins on this machine; no API key
# is used (--harness pins the set, so there is no router and no pre-score).
#
#   bash scripts/m6check.sh
#
# Prints a summary only. Output stays in .live: never commit it.
set -u
unset CLAUDECODE CLAUDE_CODE_ENTRYPOINT CLAUDE_CODE_SSE_PORT GEMINI_API_KEY GOOGLE_API_KEY 2>/dev/null || true
export REFRAKTAL_LIVE=1 # live checks start real harnesses; go test never does
cd "$(dirname "$0")/.."
LIVE=$PWD/.live/m6
BIN=$PWD/.live/bin/refraktal
mkdir -p "$LIVE" "$PWD/.live/bin"
go build -o "$BIN" ./cmd/refraktal || exit 1

REPO=$LIVE/repo
rm -rf "$REPO" && mkdir -p "$REPO" && cd "$REPO" || exit 1
git init -q -b main && printf 'x\n' > README && git add -A && git -c user.name=t -c user.email=t@t commit -q -m init
cd - >/dev/null

LIVEDIR="$(python3 -c 'import os,sys; print(os.path.expanduser("~/Library/Application Support/refraktal/live") if sys.platform=="darwin" else os.path.join(os.environ.get("XDG_CONFIG_HOME", os.path.expanduser("~/.config")), "refraktal/live"))')"
before=$(ls "$LIVEDIR" 2>/dev/null | wc -l | tr -d ' ')
TASK='Create a file named hello.txt containing the single line: hello. Do nothing else.'
start=$SECONDS
(cd "$REPO" && "$BIN" run --harness claude,codex,agy --usd 1 --mins 3 --json "$TASK") </dev/null > "$LIVE/result.jsonl" 2> "$LIVE/stderr"
rc=$? secs=$((SECONDS - start))
sleep 2
after=$(ls "$LIVEDIR" 2>/dev/null | wc -l | tr -d ' ')
echo "exit $rc after ${secs}s; live records before $before, after $after"
python3 - "$LIVE/result.jsonl" "$REPO" <<'PY'
import json, os, sys
r = json.loads(open(sys.argv[1]).read().strip().splitlines()[-1])
b = r.get("budget") or {}
print("outcome=%s winner=%s files=%s" % (r["outcome"], r.get("harness"), r.get("files")))
for c in r.get("candidates") or []:
    s = c.get("settlement") or {}
    print("  %s %-6s %-16s applies=%s checks=%s grant=%.3f spent=%.4f files=%s" % (c.get("label"), c["harness"], c["outcome"],
          c.get("applies"), [(k["name"], k["status"]) for k in c.get("checks") or []], s.get("spent", 0) + s.get("returned", 0),
          s.get("spent", 0), c.get("files")))
v = r.get("verdict")
if v: print("verdict: winner=%s method=%s judge=%s close_call=%s checks=%s" % (v.get("winner"), v["method"], v.get("judge"), v.get("close_call"), v["checks"]))
j = r.get("judge")
if j: print("judge: %s %s allowed=%d denied=%d" % (j["harness"], j["outcome"], j["permissions_allowed"], j["permissions_denied"]))
print("budget: charged $%.4f of $%.2f, turns %d, charges %s" % (b.get("spent_usd", 0), (b["limits"].get("USD") or b["limits"].get("usd") or 0), b.get("turns", 0),
      [(c["source"], round(c["usd"], 4)) for c in b.get("charges") or []]))
alive, swept = [], 0
for l in open(os.path.join(sys.argv[2], r["run"], "events.jsonl")):
    e = json.loads(l)
    if e["event"].startswith("tracked_"):
        for p in e.get("procs") or []:
            try:
                os.kill(p["pid"], 0); alive.append(p["pid"])
            except ProcessLookupError:
                pass
            except PermissionError:
                pass
    if e["event"] == "child_end": swept += e.get("swept") or 0
print("tracked processes still running: %d; session sweeps killed: %d" % (len(alive), swept))
print("worktrees left: %s" % [c["harness"] for c in r.get("candidates") or [] if c.get("worktree")])
PY
