# Fixtures

- `docs_multi_request.json`, `docs_multi_response.json`: copied verbatim from the
  "Ask multiple questions" REST example in the official docs,
  https://aistudio.google.com/docs/decisions-api#multiple-questions (fetched 2026-11-15).
  A documented example, not a captured live call.
- `routing_response_synthetic.json`: NOT from Google. Hand-written in the documented
  response shape, using this spike's five question keys, so the routing mapping can be
  tested. The numbers are invented.
- `unauthenticated_error.json`: the real body of the one unauthenticated POST made to the
  live endpoint on 2026-11-15 (HTTP 403).
