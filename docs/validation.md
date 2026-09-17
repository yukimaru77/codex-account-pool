# 検証状況（2026-09-16）

## 今回の結果

Macの標準Codex、Tailscale経由のLinuxプール、登録済み4アカウントで確認した。
実通信の成功と、模擬上流による境界条件の検証を区別する。

| 検証 | 結果・証拠 |
| --- | --- |
| 同一設定・バイナリで直結／observe／pool | 3経路とも `NATIVE_RELAY_CHECK_OK`。各前後・3経路間で設定と実行ファイルのSHA-256が一致 |
| 実Codexのツール | shellで専用作業ディレクトリにファイル作成・読み戻し。`POOL_TOOL_OK` と実ファイルを確認 |
| 実Codexの圧縮・継続 | `contextCompaction` 完了後、記憶させた `SAPPHIRE_POOL_724` を復元 |
| 実Codexの中断・回復 | 応答ストリームのreasoningイベント受信後に中断。同じ会話の次の要求で `POOL_RECOVERY_OK` |
| 4アカウントの推論 | 全4件でHTTP 200、SSEの `response.completed` と期待テキスト `POOL_LIVE_OK` |
| 週リセット順fill-first | 検証用プロセス内だけで使用済みアカウントを順次除外し、4件の選択順と実quota GET成功を確認 |
| 専用RR | 検証用quota URLで4件を2周。実上流への8要求が成功し、記録上のアカウントが巡回 |
| 実トークン更新 | 全4件でaccess・refresh・ID tokenが変化。本人情報不変、保存一致、更新後の実quota GET成功 |
| 通常／専用RRの画像生成・編集 | 各入口で青い円を生成し、赤い円へ編集。4要求成功、PNGを完全デコードし4枚を目視確認 |
| 新方式の圧縮・アカウント跨ぎ | 通常／RRで圧縮データを取得し、それぞれ4アカウントで再利用。計8回で合言葉を復元 |
| 稼働サーバーのキー分離 | 管理キーでstatus 200、client→管理401、管理→中継401、未認証→中継401、client→quota 200 |
| 秘密情報の権限 | Linuxのアカウント4ファイル＋2キーが全て0600、stateディレクトリ0700 |

週間リセットの近い順に4アカウントが選ばれた。
実アカウント識別子・残量・リセット日時を含む詳細はGit管理外の検証記録に保存する。
このページの接続先は例示であり、実行時は自分のローカル設定を使う。

## 再実行コマンドと保存先

### インストール済みCodexの試験

Codex CLI 0.153.4、標準OpenAI provider、既存のChatGPTログインを使用。
モデルやreasoning設定も既存のまま。試験対象PIDだけをLocal Captureで捕捉する。
専用CAは以前にOSへ登録済み。試験はCodex向け環境変数を追加しない。

```bash
python3 scripts/check-native-observe.py direct --hash-file /opt/homebrew/bin/codex --timeout 120
python3 scripts/check-native-observe.py observe --hash-file /opt/homebrew/bin/codex --timeout 120
python3 scripts/check-native-observe.py pool --config bridge.json \
  --hash-file /opt/homebrew/bin/codex --timeout 150

python3 scripts/check-native-session.py pool --scenario tools \
  --config bridge.json
python3 scripts/check-native-session.py pool --scenario compact \
  --config bridge.json
python3 scripts/check-native-session.py pool --scenario interrupt \
  --config bridge.json
```

Macの `state/checks/` 以下に0600で詳細を保存。Gitには登録しない。

| 試験 | 結果ファイル |
| --- | --- |
| 直結 | `direct-x22t_5ij/result.json` |
| observe | `observe-pu4j_9cj/result.json` |
| pool | `pool-9nlpcsog/result.json` |
| ツール | `session-pool-tools-bqpraevu/result.json` |
| 圧縮 | `session-pool-compact-auunp9j3/result.json` |
| ストリーム開始後の中断 | `session-pool-interrupt-l9v76tsr/result.json` |

全成功レポートで `hashes_unchanged:true`。
3経路の `before_sha256` 辞書を比較した結果も
`all_three_used_identical_config_and_binaries=True` だった。
各スクリプトは終了時に自身のCodexとmitmdumpを停止する。
Macの全Codexを捕捉する常駐ブリッジを、この試験で有効化することはない。

### Linuxの実アカウント試験

`scripts/live-check` は本番と同じStore・Relay・Schedulerを使う独立した検証用プロセス。
本番の設定や有効／無効フラグを変更せず、実資格情報だけを共有する。
refreshモードは実トークンを更新・保存するため、通常のStoreと同じプロセス間ロックを使用する。
推論・画像・圧縮モードは実利用量を消費する。トークン値は出力しない。

```bash
go build -o state/checks/live-check ./scripts/live-check
state/checks/live-check --config pool.json --mode routing
state/checks/live-check --config pool.json --mode responses
state/checks/live-check --config pool.json --mode refresh
state/checks/live-check --config pool.json --mode compact-v2
state/checks/live-check --config pool.json --mode images --output state/checks/images
```

LinuxのGit管理外の `state/checks/live-20260916/` に以下を保存した。

- `routing.jsonl`: FF4件、RR8件の選択と上流HTTP 200。
- `refresh.jsonl`: 全4件で `access_changed/refresh_changed/id_token_changed/identity_unchanged/persisted/ok=true`。
- `responses-diagnostic.jsonl`: 全4件で `expected_text:true, ok:true`。
- `compact-v2-final.jsonl`: `passed:12, failed:0`。暗号化された圧縮itemを元のまま別アカウントへ渡し、合言葉を復元。
- `images-final.jsonl`: `passed:8, failed:0`。4個の画像artifactを保存・デコード。
- `images-final/*.png`: 通常／RRそれぞれの青い円と赤い円を目視確認。

画像編集には生成画像のdata URIを渡した。同一アカウント内で生成→編集しており、
上流の画像資源IDを別アカウントで参照できることの証明ではない。
上流はsize=1024x1024の指定に対し1254x1254を返した。寸法を記録し、画像自体は変更していない。

## 発見した不具合・経路の違い

### 認証以外のTrailerが落ちていたため修正

HTTP Trailerは本文の後に送られる追加ヘッダー。従来の
`pr.Out.Trailer = nil` は未知の非認証メタデータも削除していた。
追加した実HTTPテストは修正前に次の理由で失敗した。

```text
go test -race ./internal/pool -run TestRequestTrailersPreserveMetadataAndRemoveCredentials -count=1
request metadata trailers were dropped: map[]
```

本文を読み終えた時点で認証以外のTrailerを転送するよう修正。
元アカウント・ActorのTrailerは除去し、選択先の認証は通常ヘッダーへ設定する。
HTTP/1.1とHTTP/2の両方で、複数値Trailer・本文・応答Trailerの保持を確認した。

### 旧compact URLは上流404、新方式は成功

`live-check --mode compact` の通常URLと専用RR URLは、
いずれも上流 `/backend-api/codex/responses/compact` から404
`{"detail":"Not Found"}` を受信した。プールが事前拒否した結果ではない。

参照したCodex 0.153.4系の
`codex-rs/core/src/compact_remote_v2_attempt.rs` はinputに
`{"type":"compaction_trigger"}` を追加し、通常のResponsesクライアントを使用する。
`compact_remote_v2.rs` は `response.output_item.done` の圧縮itemを収集する。
実Codexの圧縮試験でも通信記録には `/responses` があり、旧compact URLはなかった。

この新方式を専用RRから使えるよう、既存の設定機構に
`/_pool/rr/responses` → `/backend-api/codex/responses` を追加した。
旧URLの自動変換・リクエストJSONの加工は行わない。
通常Codexが呼ぶResponsesは引き続きfill-first。

参照ソース:
`yukimaru77/codex` のローカル0.153.4系 checkout
`7924c33f55aade67076d782a9e814f29ae630048`。
インストール済み独自ビルドとの完全なcommit一致を意味しない。

### 試験コードの判定を修正した点

- ResponsesのSSE本文が `text/plain` またはContent-Typeなしで返る実例があった。
  最初の試験はHTTP 200を失敗判定したため、試験コードは本文のSSE形式でも判定するよう修正。
- 圧縮itemは `response.completed.response.output` だけでなく、
  `response.output_item.done` から収集する必要があった。
- app-serverの完了通知がRPC応答より先に届く場合も扱い、失敗時は非0で終了するよう検証した。
- 中断試験の1回は通信前にCodexのSQLite初期化に失敗した。
  `session-pool-interrupt-jn5m06qy/stderr.log` に
  `failed to initialize sqlite state runtime`。原因は未特定で、CodexのDBや設定は変更していない。
  その後の再試験は成功した。
- 直結の中断試験1回は本文の最初の文字だけを待ち、reasoningイベントが流れている間も待ち続けた。
  試験プロセスを停止し、reasoning完了もストリーム開始と扱ってpoolで再試験した。

これらの判定修正は検証コードに限定される。プールの本文加工を増やしていない。

## 自動テスト

### 整形済みJSONL → 暗号化blobの追加確認

履歴の整形は呼び出し元の責任とする指定に従い、`scripts/compact-jsonl.py` は
1行1input itemのJSONLを配列へまとめ、`compaction_trigger` を追加して専用RRへ送る。
Codexのrollout復元・巻き戻し処理・itemのフィールド削除は実装していない。

```bash
python3 scripts/compact-jsonl.py state/checks/jsonl-compact-28ixdths/prepared.jsonl \
  --model gpt-6-astra --origin https://pool.example:18473 \
  --output state/checks/jsonl-compact-28ixdths/compact.json
```

実サーバーで `type=compaction`、空でない `encrypted_content`（1,356文字）を取得。
続けて同じRR入口に圧縮itemと追加質問だけを送り、元の合言葉 `AMBER_JSONL_916` を復元した。
結果はMacの `state/checks/jsonl-compact-28ixdths/result.json` に保存。

```text
encrypted_content_present=true
continued_http_status=200 response_completed=true marker_restored=true
input_unchanged=true output_mode=0o600
```

追加した13テストは、元のitemのJSONバイト保持、未知フィールド、rolloutを解釈しないこと、
SSEの分割・複数data行、暗号化itemなし・未完了・HTTP失敗の扱い、再送／redirectなし、
入力不変・出力の原子的保存と0600権限を確認する。

### 全体のテスト結果

```text
go test -race ./...
ok codex-account-pool/cmd/codex-pool
ok codex-account-pool/internal/cpa
ok codex-account-pool/internal/pool
ok codex-account-pool/scripts/live-check

go build -o bin/codex-pool ./cmd/codex-pool
exit 0

bridge/.venv/bin/python -m unittest discover -s bridge -v
Ran 10 tests ... OK

python3 -m unittest discover -s scripts -v
Ran 21 tests ... OK
```

Goの模擬上流テストは、N=7、M=37、週間窓の位置違い、同時選択、古いquota、
RPM cooldown、期限前更新、30並列の更新競合、省略されたID/refresh tokenの保持、
401/429の保持と生成非再送、未知パス・メソッド・クエリ・非JSON・大きい本文・SSE、
WebSocketの断片・圧縮・ping・close、M%到達時の次メッセージ停止を含む。

Pythonのbridgeテストは実mitmdumpと模擬TLS上流を使い、SSEの逐次転送と
WebSocket生バイト保持も検証する。scriptsのテストは偽CLI/app-serverで実行し、
設定変更を復元せず検出すること、Codexへの引数・環境を追加しないこと、
応答不一致を失敗にすること、早着した完了通知と保存権限を確認する。
Goのlive-checkテストは不完全SSE・応答不一致・HTTPエラー・壊れたPNGを成功扱いしないことを確認する。

## 残っている制限・未確認事項

- 実CodexのWebSocketをM%到達で切断した後の自動再接続・別アカウントでの回復は未実施。
  境界動作は模擬上流で確認した。中断APIによる回復試験とは別。
- 保存会話のresume、検索、コネクタ実操作、上流資源IDを使うアカウント跨ぎの画像編集は未実施。
- 一部SSEと実CodexのWebSocketで、プール側のトークン使用数は0と記録された。
  観測できない場合に0のままにする現在の記録上の制限であり、正確な課金集計には使えない。
  アカウント選択は別途取得する週間quotaを使う。
- 3経路比較のpool試験でモデル一覧取得の子プロセスタイムアウト／502が1回発生。
  後続の実Codex試験では同URLが200。終了付近のplugins/listにも502があり、原因は未特定。
  全ての起動時バックグラウンド通信が常に成功するとは確認できていない。
- LinuxはSupervisorで常駐し、プロセス再起動は確認済み。
  ホスト／コンテナ全体の再起動後の自動起動は未設定。
- 初期試験では試験中・試験間に設定ハッシュが変わった履歴があり、変更元は未特定。
  当時の記録は非公開の検証バックアップに保管。今回の3経路比較と成功した機能試験では前後一致を確認した。

完成・全機能の完全互換とは判定しない。
MacとLinuxの導入手順は [mac-client.md](mac-client.md) と [linux-host.md](linux-host.md) を参照。
