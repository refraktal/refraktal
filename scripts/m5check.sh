#!/usr/bin/env bash
# M5 live checks. Free-tier Gemini Decisions API calls only; the key comes
# from the environment or the repository's gitignored .env and is never
# printed. Output goes to .live/m5 (gitignored).
#
#   bash scripts/m5check.sh samples   the spike's 10 samples through the router, compared with the spike's run
#   bash scripts/m5check.sh dry       refraktal run --dry for a few trivial tasks, with and without the key
set -u
export REFRAKTAL_LIVE=1
cd "$(dirname "$0")/.."
ROOT=$PWD
LIVE=$ROOT/.live/m5
mkdir -p "$LIVE" "$ROOT/.live/bin"
go build -o "$ROOT/.live/bin/refraktal" ./cmd/refraktal || exit 1

t_samples() { go run ./scripts/m5check; }

t_dry() {
  local repo="$LIVE/repo"
  rm -rf "$repo" && mkdir -p "$repo" && cd "$repo" || exit 1
  git init -q -b main
  printf 'package calc\n\nfunc Add(a, b int) int { return a - b }\n' > calc.go
  printf 'package calc\n\nimport "testing"\n\nfunc TestAdd(t *testing.T) { if Add(2, 3) != 5 { t.Fail() } }\n' > calc_test.go
  printf '# calc\n' > README.md
  git add -A && git -c user.name=t -c user.email=t@t commit -q -m init
  # The key lives in the refraktal repository's .env; hand it over by environment for this repository.
  local key; key=$(cd "$ROOT" && sed -n 's/^\(export \)\{0,1\}GEMINI_API_KEY=//p' .env 2>/dev/null | tr -d '"'"'" | head -1)
  [ -n "${GEMINI_API_KEY:-}" ] && key=$GEMINI_API_KEY
  for task in "Fix the bug in Add: it subtracts instead of adding." "Add a short usage section to README.md." "Explain why TestAdd fails."; do
    for mode in key nokey; do
      if [ $mode = key ]; then out=$(GEMINI_API_KEY="$key" "$ROOT/.live/bin/refraktal" run --dry --json "$task" </dev/null)
      else out=$(env -u GEMINI_API_KEY "$ROOT/.live/bin/refraktal" run --dry --json "$task" </dev/null); fi
      echo "$out" | tail -n 1 | python3 -c '
import json,sys
r=json.load(sys.stdin); rt=r.get("route") or {}; d=rt.get("decision") or {}; h=d.get("harness") or {}
print("%-6s %-55s -> %-6s %s conf=%.2f policy=%s latency=%.0fms cached=%s exit=%s" % (sys.argv[1], sys.argv[2][:55], r.get("harness"), d.get("decider"), h.get("confidence",0), (rt.get("policy") or {}).get("action"), d.get("latency_ms",0), rt.get("cached"), r.get("exit")))' "$mode" "$task"
    done
  done
  cd "$ROOT"
}

case ${1:-} in
  samples) t_samples ;;
  dry) t_dry ;;
  *) sed -n '2,9p' "$0"; exit 2 ;;
esac
