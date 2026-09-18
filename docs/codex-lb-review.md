# codex-lb比較・取り込み記録

対象: https://github.com/Soju06/codex-lb

確認したcommit: `9637bdee36744ca65e7cd13ffd47b7e79741f551`（2026-09-18に取得）。
参照用checkoutは開発用で、号池の実行依存には加えない。

## 維持する仕様

- Codexの設定・認証・環境変数・バイナリを変更しない。
- Nアカウント。通常の全パスは週間reset時刻順fill-first、残りM%以下を除外。
- 明示した専用URLだけ独立したround-robin、予約分も使用する。
- 通常経路の本文・未知フィールド・イベントを保持する。生成を勝手に再送しない。
- 画像とcompactも通常経路ならfill-first。blobの変換・代替圧縮はしない。
- 明示登録したRemote KBだけ既存の挿入・継承・compact除外を行う。
- MacのApp除外設定、Tailscale上のLinux集約、秘密値をGit管理外に置く構成を維持する。

## 比較対象と判断

| 分野 | 参照元 | 号池での判断・検証対象 |
|---|---|---|
| アカウント選択 | `docs/routing.md`, `app/core/balancer/logic.py` | 重み付け・日単位reset bucket・sticky優先は採用しない。既存の秒精度週間順、M、N、専用RRを回帰確認する。 |
| OAuthと同時更新 | `app/core/auth/refresh.py`, `app/modules/accounts/refresh_claims.py` | 恒久エラーと一時エラー、交換済みか不明な失敗、更新の競合を区別する。既存のファイルロックと原子的保存を生かす。 |
| 利用量・状態更新 | `app/core/usage/refresh_policy.py`, `app/modules/usage/updater.py`, `app/modules/usage/live_ingest.py` | 60秒照会と180秒鮮度は既存で同等。遅れて届く照会とライブ観測、認証失敗の競合を検証・修正する。 |
| HTTP/SSE透過転送 | `app/modules/proxy/downstream_delivery.py`, `app/modules/proxy/_service/streaming/` | 生バイト保持と完了の観測を分離する。JSON再生成・合成完了イベント・生成の再送は採用しない。 |
| WebSocket | `app/modules/proxy/_service/websocket/mixin.py`, `tests/unit/test_websocket_terminal_cancellation.py` | 送信済み要求と未送信要求を分け、進行中の応答を切替で壊さない。新しいアカウントへの再送で二重実行しない。 |
| 会話継続・資源の所有者 | `app/modules/proxy/continuity.py` | 本文やIDを理由に全通常経路を特定アカウントへ固定する処理は仕様と異なる。通常経路には採用しない。 |
| compact・画像 | `app/modules/proxy/_service/compact.py`, `app/modules/proxy/images_service.py`, `app/core/clients/proxy.py` | 画像→Responses変換、画像イベントの再構成、履歴の削減・再送は採用しない。既存の専用RRと通常経路の不変性を確認する。 |
| Remote KB | `app/modules/proxy/continuity.py`, `app/modules/proxy/_service/http_bridge/` | 参照元の会話履歴管理は比較材料。通常経路へ広げず、KBの固定位置・compact除外・子への継承の既存テストを確認する。 |
| Mac透過ブリッジ | 参照元`README.md`のclient setup | 参照元はprovider設定を変更する構成。導入方式は移植せず、MacブリッジとApp除外を維持する。 |
| 管理・監視・配置 | `docs/usage-reporting.md`, `deploy/`, `app/modules/proxy/_service/websocket/mixin.py` | DB・Dashboard・クラスタを新設しない。既存status/usageログと終了時のストリーム扱いを比較する。 |

## 取り込んだ変更

1. **OAuth更新**: 恒久エラーは同じ試行内で繰り返さない。HTTP成功後の本文切断・JSON破損など、
   refresh tokenを消費したか不明な失敗を再送しない。dial失敗と明示的な429/5xxは従来の最大3回を維持。
   毎回の照会を永久停止する仕組みは追加していない。
2. **同時更新**: 401になったaccess tokenとディスクの最新値をロック内で比較し、既に更新済みなら再利用。
   複数プロセスの既存ファイルロックと原子的保存を維持し、ロック待ちのキャンセルに対応。
3. **quotaの整合**: 古い照会結果で新しいライブ観測を巻き戻さない。
   古い接続のquotaイベントでは認証失敗を解除せず、その失敗後に開始した照会の成功で解除。
   Sparkなど別のlimit familyのイベントを通常の週間枠へ混ぜず、429の待機期限を後続の短い値で縮めない。
4. **観測ログ**: `terminal_event` と `usage_observed` を追加。
   終端が観測できたか、usageが本当に0なのか未観測なのかを分ける。
   全イベントを読めたResponsesが終端なくEOFになった場合は失敗を記録し、上流の本文は変えない。
5. **WebSocket切り替え**: Responsesの送信済み要求を追跡し、M%到達などで次要求を止める際も、
   進行中の応答の終端を下流へ書き終えるまで待つ。新要求は別アカウントへ勝手に再送しない。
   イベント種別・応答IDは独立した小さな読み取り処理で追跡し、大きな本文やキー順の違いでも扱う。
   ResponsesのWebSocketは非圧縮フレームを交渉する。未知のWebSocket経路には広げない。

## 既存仕様の回帰確認

次の既存テストも同じ全体テストで成功した。全て模擬上流による検証。

- `TestAllNormalEndpointsUseFillFirstAndOnlyCustomURLsUseRoundRobin`: 画像・compactを含む全通常経路と専用RR。
- `TestFillFirstUsesWeeklyResetOrderUntilConfigurableReserve` / `TestRoundRobinIncludesReserveForNAccountsAndKeepsIndependentCursors`: N=7、M=37、週間順、RRごとの巡回。
- `TestDedicatedRoundRobinURLOnlyChangesPathAndAccount`: RRでも本文を変更しない。
- `TestUnknownMetadataIsPreservedAndNativeSessionHeadersAreObserved`: 未知の情報の保持。
- `TestRemoteKBPrefixEditsOnlyInsertionAndKeepsEveryBlob` / `TestRemoteKBHTTPPassthroughCompactionAndUnknownRoutes`: KBの固定位置・複数blob・compact除外。
- `TestRemoteKBWebSocketIntegrationKeepsResponsesAndNegotiatesPlainFrames`: KB付きWebSocketの継続。
- bridgeの `test_launcher_can_exclude_app_bundles_without_changing_relay`: Codex App除外。

## 検証結果（2026-09-19）

```text
go test -race ./...
ok codex-account-pool/cmd/codex-pool
ok codex-account-pool/internal/cpa
ok codex-account-pool/internal/pool
ok codex-account-pool/scripts/live-check

go build ./cmd/codex-pool
exit 0

bridge/.venv/bin/python -m unittest discover -s bridge -v
Ran 12 tests ... OK

python3 -m unittest discover -s scripts -v
Ran 21 tests ... OK
```

追加試験には、20要求・2つのStoreで更新1回、キャンセルされたロック待ち、失敗後の古い観測、
未知のOAuthエラー本文の非露出、終端なしEOF、usageなしと実0の区別を含む。
WebSocketでは進行中に次要求が来る状況、completed/failed/incompleteの配送後の切断、
複数応答、無関係な終端、1 MiB超の要求・応答、キー順、マスク・分割・pingを確認する。

## 実機確認と採用しなかった範囲

- 今回の変更はローカル検証済み。Linuxへの反映と、実CodexでM%に達した後の自動再接続は未確認。
  SSH環境登録は `remote probe timed out after 20s`。Tailscale SSHの追加認証が必要な状態。
- 完了追跡は既知のResponsesイベントを対象とし、未知のプロトコルを拒否しない。
  usageの観測上限は従来どおり1 MiB。大きなSSE等の未観測を成功完了や実消費0と断定しない。
- 元リポジトリのprovider設定、DB、Dashboard、クラスタ、重み付け、日単位reset bucket、
  owner affinity、本文変換、履歴削減、自動再送は今回の号池へ取り込まない。
- プロセス停止時のgraceful shutdownは、今回のアカウント切り替えとは別。
  参照元には停止時にWebSocketを待つ実装・試験もあるが、この変更は既存の停止処理を変更していない。
  プール再起動中まで進行中の応答を維持する保証は追加していない。

MITの出典とライセンスは [third_party/codex-lb](../third_party/codex-lb/README.md) に記録。
CPA・codex-lbのサーバーを起動したり、実行時依存として追加したりする必要はない。
