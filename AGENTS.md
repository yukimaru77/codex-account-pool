# Codex account pool

- This is an independent application. Do not patch CPA or change Codex binaries,
  configuration, login state, environment variables, or tool availability.
- `pool-rr codex exec ...` may set invocation-only provider overrides and the RR client key
  for local RR. Never persist these changes to Codex config.
- The pool is independent of kb-repomap and other RR clients: never launch them, set their
  environment variables, or write files named after them. RR clients read `~/.codex-pool/rr.json`.
- The pool server is an explicit round-robin relay only. Normal Codex traffic
  must not depend on this repository; account rotation for ordinary Codex
  sessions belongs to Codex itself.
- Only explicitly configured `/_pool/rr/*` endpoints are routed by the pool.
  Account count is arbitrary N.
- Relay original bytes. Never regenerate request/response JSON, normalize tools,
  map models, filter unknown fields/events, or replay generation automatically.
- Images, compaction, and web search stay ordinary Codex requests. Do not add
  special request constructors or standalone routes to the pool.
- Keep content inspection small and read-only. Any added inspection must directly
  support RR account selection or continuity.
- Reuse the narrow CPA OAuth/quota code under internal/cpa. Preserve MIT notice,
  pin the source commit, and document adaptations in third_party/cpa.
- Never log credentials or arbitrary OAuth error response bodies.
- Run gofmt, go test -race ./..., and go build ./cmd/codex-pool after changes.
- This repository is local-only: do not add remote pool, Tailscale, Linux-host, or transparent-bridge deployment paths.
- Unit tests use mock upstreams. Report real Mac/OpenAI integration separately.
