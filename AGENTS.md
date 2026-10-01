# Codex account pool

- This is an independent application. Do not patch CPA or change Codex binaries,
  configuration, login state, environment variables, or tool availability.
- `pool-rr codex exec ...` may set invocation-only provider overrides and the RR client key
  for local RR. Never persist these changes to Codex config.
- The pool is independent of kb-repomap and other RR clients: never launch them, set their
  environment variables, or write files named after them. RR clients read `~/.codex-pool/rr.json`.
- Default native endpoints, including images and compaction, use weekly-reset
  fill-first and stop selecting accounts at configurable M percent remaining.
- Only explicitly configured application-owned endpoints use round-robin and
  may consume the reserved remainder. Account count is arbitrary N.
- Ordinary requests pass through with fill-first regardless of path, method,
  query, or payload schema. Only application-owned endpoints are exceptions.
  Do not add a native route allowlist or reject unfamiliar request fields.
- Relay original bytes. Never regenerate request/response JSON, normalize tools,
  map models, filter unknown fields/events, or replay generation automatically.
- User-requested exception: the two /_pool/rr/images endpoints accept only prompt
  and images, and construct fixed Codex-compatible image requests. Native routes
  and image response bytes remain unchanged. Review constants against the installed
  Codex source when upgrading; do not silently follow changed upstream paths.
- Keep content inspection small and read-only. Any added inspection must directly
  support account selection, continuity, or usage observation.
- Reuse the narrow CPA OAuth/quota code under internal/cpa. Preserve MIT notice,
  pin the source commit, and document adaptations in third_party/cpa.
- Never log credentials or arbitrary OAuth error response bodies.
- Run gofmt, go test -race ./..., and go build ./cmd/codex-pool after changes.
- This repository is local-only: do not add remote pool, Tailscale, Linux-host, or transparent-bridge deployment paths.
- Unit tests use mock upstreams. Report real Mac/OpenAI integration separately.
