# Codex account pool

- This is an independent application. Do not patch CPA or change Codex binaries,
  configuration, login state, environment variables, or tool availability.
- Default native endpoints, including images and compaction, use weekly-reset
  fill-first and stop selecting accounts at configurable M percent remaining.
- Only explicitly configured application-owned endpoints use round-robin and
  may consume the reserved remainder. Account count is arbitrary N.
- Ordinary requests pass through with fill-first regardless of path, method,
  query, or payload schema. Only application-owned endpoints are exceptions.
  Do not add a native route allowlist or reject unfamiliar request fields.
- Relay original bytes. Never regenerate request/response JSON, normalize tools,
  map models, filter unknown fields/events, or replay generation automatically.
- Keep content inspection small and read-only. Any added inspection must directly
  support account selection, continuity, or usage observation.
- Explicit Remote KB sessions are the scoped exception: register immutable KB
  items, inherit the binding for children, and insert them into inference input
  before conversation/compaction history. Exclude them from compaction requests.
  Preserve other JSON fields and response bytes; keep unregistered relay intact.
- Reuse the narrow CPA OAuth/quota code under internal/cpa. Preserve MIT notice,
  pin the source commit, and document adaptations in third_party/cpa.
- Never log credentials or arbitrary OAuth error response bodies.
- Run gofmt, go test -race ./..., and go build ./cmd/codex-pool after changes.
- Unit tests use mock upstreams. Report real Mac/OpenAI integration separately.
