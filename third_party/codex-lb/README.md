# codex-lb references

Source: https://github.com/Soju06/codex-lb

Pinned reference: `9637bdee36744ca65e7cd13ffd47b7e79741f551`.
License: MIT, copyright 2025 Soju06; see [LICENSE](LICENSE).

This project does not import or execute codex-lb. Its relevant behavior is
adapted to the existing small Go relay, retaining our native passthrough policy.

- `app/core/balancer/logic.py`: permanent OAuth failure code set, adapted in
  `internal/cpa/adaptations.go`.
- `app/core/auth/refresh.py`: distinguish errors before dispatch from ambiguous
  failures while reading token-exchange responses. A dial failure may retry;
  a read failure may have consumed the rotating refresh token and must not.
- `app/modules/accounts/refresh_claims.py`: serialize exchange and persistence,
  then re-read credentials. We retain local cross-process flock/atomic rename
  instead of adding a database. A delayed 401 refresh uses the rejected access
  token as a comparison value so another successful rotation is reused.
- `app/core/usage/refresh_policy.py`, `app/modules/usage/updater.py`: freshness
  belongs to the observation, and account recovery needs fresh evidence.
- `app/core/usage/live_snapshots.py`: model-specific quota events must not
  overwrite the default Codex weekly budget. Header windows remain selected
  by their `X-Codex-Primary/Secondary-` family.
- `app/modules/proxy/downstream_delivery.py`: successful HTTP headers or clean
  EOF alone do not establish a completed Responses stream. Our bounded observer
  records upstream terminal events separately from token-usage availability;
  it does not claim downstream delivery and never changes response bytes.
- `app/modules/proxy/_service/websocket/mixin.py`,
  `tests/unit/test_websocket_terminal_cancellation.py`: distinguish admitted
  requests from the next unsent message, and track terminal delivery before
  closing for rotation. The bounded-memory lifecycle scanner and raw frame
  gate are our Go implementation, not a port of the full bridge/replay layer.
- `CHANGELOG.md` / upstream commit `2e4a580c1c1834f00b6d4c62598caf1ffd0446ee`:
  disable permessage-deflate negotiation on the known Responses WebSocket.
  Other native WebSocket routes retain their original negotiation.

Full comparison and validation: [docs/codex-lb-review.md](../../docs/codex-lb-review.md).
