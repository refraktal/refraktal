#!/usr/bin/env bash
# Runs every sample task through the real Gemini Decisions API, once each, and
# writes the decisions and latencies to results/. Needs your own GEMINI_API_KEY.
#
#   GEMINI_API_KEY=... ./run-live.sh
#
# Cost: 10 requests of a few hundred input tokens each at $0.10 per 1M input
# tokens, so well under a cent (free on the free tier).
set -euo pipefail
cd "$(dirname "$0")"

if [ -z "${GEMINI_API_KEY:-}" ]; then
  echo "run-live.sh: GEMINI_API_KEY is not set; refusing to record fallback results as live ones." >&2
  exit 2
fi

stamp="$(date -u +%Y%m%dT%H%M%SZ)"
out="results/$stamp"
mkdir -p "$out"

# -timeout 10s (not the 2s default) so a slow call is measured, not hidden.
go run ./cmd/route -samples samples/tasks.json -log "$out/decisions.jsonl" -timeout 10s | tee "$out/table.txt"

# One line per call: id, decider, latency, tokens, computed cost, policy.
{
  printf 'id\tdecider\tlatency_ms\tinput_tokens\tcost_usd\tpolicy\tfallback_reason\n'
  go run ./cmd/summarize < "$out/decisions.jsonl"
} > "$out/latency.tsv"

echo
echo "Wrote $out/decisions.jsonl (full records incl. raw responses), $out/table.txt, $out/latency.tsv"
if grep -q '"decider":"fallback"' "$out/decisions.jsonl"; then
  echo "WARNING: at least one task fell back; see fallback_reason in $out/latency.tsv. Those rows are NOT live results." >&2
  exit 1
fi
