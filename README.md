# Codex account pool

標準 Codex → Mac の透過ブリッジ → 専用アカウントプール → OpenAI。
CPA の OAuth・quota 処理を小さく取り込んだ独立アプリ。
Codex の設定・ログイン・環境変数・バイナリを変更する処理は持たない。

**開発中。Linux上のプールで、4アカウントの推論・週間quota取得・
access/refresh/ID tokenの実更新と保存を確認済み。**
同一設定・バイナリの直結／observe／pool比較、標準Codexのツール実行・圧縮後の継続も成功。
通常／専用RRの画像生成・編集と、新方式の圧縮結果を別アカウントで使う試験を実施した。
詳細と未確認事項は [検証記録](docs/validation.md) と
[Linuxの起動・接続手順](docs/linux-host.md) を参照。
資源IDを理由に作成元へ固定したり要求を拒否する処理は持たない。
WebSocket は接続内で固定される。Codex v0.153.4で、検証用quotaをM%にした際の
応答完了・自動再接続・別アカウントでの会話継続を確認済み。保存会話のresumeは未検証。

## Remote KB（任意のセッションのみ）

`kb-repomap` の `kb codex NAME --remote` からKBを登録すると、号池がそのセッションと
サブエージェントの推論要求へ固定KBを挿入する。位置は先頭のsystem/developer項目の後、
会話のcompaction blobより前。コンパクト要求には含めず、圧縮後も同じKBを使う。
通常のセッションは従来どおり素通り。CodexやMacの設定変更は不要。
API・配置先・WebSocketの扱いは [Remote KBの説明](docs/remote-kb.md) を参照。

## ルーティング

| 呼び出した入口 | 選択 | 週の残量 |
| --- | --- | --- |
| 専用 URL を除く全ての通常 URL | 週リセットが近い順の fill-first | M% 以下を除外 |
| アプリで明示設定した `/_pool/rr/...` | round-robin | M% 以下も利用、0% を除外 |

アカウント数 N と M は固定しない。M は `reserve_percent`（初期値10）。
週の窓は604800秒で識別する。短期枠の枯渇・無効化・認証エラー・古い利用量観測は利用不可。
上流の429ではそのルートの `Retry-After` まで待つ。値がない場合は1分待つ。
週リセット後も実際の利用量を再観測してから選ぶ。架空の残量の合算・リセットはしない。
M は新規選択時の閾値であり、実行中の要求を中断しない。観測の遅れと同時実行により残量がMを下回る場合がある。
WebSocket は次のメッセージの開始時にも選択条件を確認する。切替が必要ならそのメッセージを
上流へ送らず、Responsesでは進行中の応答の終端を下流へ書き終えてから接続を終了する。
再接続時に選び直す。中継側からの再送や接続内の認証差し替えはしない。
開始判定は最大14バイトのフレームヘッダーで行う。Responsesの進行状況はイベント種別と応答IDだけを
本文サイズに依存しない小さなメモリで追跡し、本文はストリームのまま通す。
この観測のため、ResponsesのWebSocket接続では `Sec-WebSocket-Extensions` を外して
非圧縮フレームを交渉する。既知のResponses以外のWebSocketではこの交渉・完了追跡を行わない。
Codex v0.153.4で、手動再接続・再入力なしの回復を実機確認した。
実通信と模擬quotaの区別は [検証記録](docs/validation.md#2026-09-19-linux反映と実codexのwebsocket切り替え) を参照。
専用RR URLのWebSocketは接続ごとに巡回し、接続内は同じアカウントを使う。

認証ヘッダーを選択アカウントの Bearer と Account ID に替える。Remote KBを除き本文を再生成せず、
未知フィールド・ツール・モデル名・SSEイベントを保持する。生成の自動再送はしない。
接続先ホストは `https://chatgpt.com` に固定するが、パスの許可リストは持たない。
通常経路は未知のパス・HTTPメソッド・クエリもそのまま通し、本文の形式では拒否しない。
エンコード済みパスとクエリ文字列も保持する。アカウント選択は本文の種類で切り替えない。
通常のfill-firstから除外するのは、明示設定したRR URL、管理用 `/_pool/status`、
KB登録・照会用 `/_pool/kb/bind` だけ。
Mac側も対象プロセスの `chatgpt.com:443` 宛て全パスを中継する。本人情報・コネクタのパスも含む。
`auth.openai.com` など対象外ホストへの接続は元のまま。

## 認証トークンとCodexの表示

プールは各アカウントのアクセストークン・リフレッシュトークン・IDトークンを、
アカウントID・メールアドレス・有効期限とともに専用stateへ保存する。
要求ごとに選択アカウントの保存済み資格情報を読み直し、アクセストークンの期限が
残り1分以内ならCPA由来のOAuth処理で更新してから使う。
更新応答に新しいIDトークンがあれば一緒に保存し、省略された場合は既存値を保つ。
毎要求でOAuth更新を行うわけではない。WebSocketの認証は接続開始時に決まる。
更新はプロセス間ファイルロックで直列化する。利用量照会の401が遅れて届いても、
別要求が既に更新したトークンを再利用し、不要な再更新を避ける。ロック待ちはキャンセル可能。
更新応答の途中切断や不正な成功応答は、交換済みの可能性があるため同じ試行内で再送しない。
恒久OAuthエラーも再試行を止める。接続開始前のdial失敗と明示的な429/5xxは最大3回試行する。
失敗後の次回利用量照会は既存どおり行うため、無効な認証は再ログインが必要になる。

確認したCPA/Codexの通常API認証は `Authorization: Bearer <access_token>` と
`ChatGPT-Account-ID` を使い、IDトークンを添付しない。
したがって、中継がIDトークン用ヘッダーや本文フィールドを追加することはない。
CPAの保存・更新処理は再利用済みで、通常要求のIDトークン差し替え処理は見つからなかった。
確認元: CPA `10d0123595d1ae264d8ac067a1e3bbbb3f40a0a4` の
`internal/runtime/executor/codex_executor_auth.go` と `codex_executor_request.go`。

Codexの `/status` に表示するメールアドレスは、Codex自身が保持するIDトークンの
情報から組み立てる。プールが保持するIDトークンを更新しても、その表示は書き換わらない。
一方、`/backend-api/wham/usage` の利用量照会も通常のfill-firstで転送するため、
中継経由で新たに取得した残量は選択アカウントのものになる。表示には取得済みの値が残る場合がある。
確認元: Codex 0.153.4系の `codex-rs/login/src/auth/manager.rs` (`get_account_email`)、
`codex-rs/model-provider/src/bearer_auth_provider.rs`、
`codex-rs/backend-client/src/client/rate_limit_resets.rs`。

## 起動とアカウント登録

Go 1.26 以降:

```bash
go build -o bin/codex-pool ./cmd/codex-pool
./bin/codex-pool init --config pool.json
./bin/codex-pool login --config pool.json
```

### 各環境の設定と秘密情報

各自の接続先・配置先はローカルの設定ファイルに保存する。
実URL・キー・アカウント情報をソースコードへ書き込む必要はない。
**各端末でcloneした `codex-account-pool` フォルダの直下に、
リモートLinuxでは `pool.json`、Codexを使う各Macでは `bridge.json` を置く。**

| ファイル | 用途 | Git管理 |
| --- | --- | --- |
| `pool.json` | Linuxサーバーの待受、state保存先、残量閾値、RR入口 | 対象外 |
| `bridge.json` | Macから接続する号池のorigin、私設経路のHTTP許可、state保存先 | 対象外 |
| `state/client.key` / `state/admin.key` | クライアント用／管理用キー | 対象外 |
| `state/accounts/` / `state/ca/` / `state/checks/` | OAuth情報、CA秘密鍵、実通信の検証データ | 対象外 |

OAuth情報の `state/accounts/` と管理キー `state/admin.key` はLinuxだけに置く。
Linuxから各Macへ渡すのは `state/client.key`。CAは各Macで個別に作成する。

サーバーは `init` で `pool.json` とキーを作成する。
[pool.example.json](pool.example.json) はサーバー設定例。
Macでは [bridge.example.json](bridge.example.json) を `bridge.json` にコピーし、
`origin` を自分の号池のURLに置き換える。既存設定がある場合は上書きしない。
設定ファイルは `chmod 600 pool.json` または `chmod 600 bridge.json` で保護する。

```bash
python3 bridge/run.py pool --config bridge.json
```

Codex Appは直接接続しCLIだけ号池を使う場合、Macの `bridge.json` に
`"exclude_codex_app": true` を追加してブリッジを再起動する。
起動時の `--exclude-codex-app` でも指定可能。詳細は [Mac導入手順](docs/mac-client.md#codex-appは直接接続しcliだけ号池を使う)。

`origin` の末尾に `/v1` は付けない。Tailscaleなどの暗号化経路を確認してHTTPを使う場合だけ、
`bridge.json` の `private_http` を `true` にする。CLIの `--origin` は設定値より優先される。
既存の `pool.json` の `listen` から接続先を決める起動方法も使える。
初回のCA・キー取得・通信捕捉の準備は [Mac導入手順](docs/mac-client.md) を参照。

### アカウントの登録・管理

表示された device URL と code を使い、このアプリ用にログインする。Nアカウント分繰り返す。
これは Codex 本体の `login` ではない。ブラウザで選んだアカウントを専用 `state/accounts` に保存する。

既存 CPA の OAuth JSON を明示的に取り込む場合:

```bash
./bin/codex-pool import --config pool.json /path/to/cpa-account.json
```

取り込み元は読み取りのみ。ただし同じ refresh token のコピーを CPA と本アプリが両方更新すると
競合するため、通常は専用 `login` を使う。移行する場合は元の認証レコードを使い続けない。
同じアカウントの再インポートは古い token への巻き戻しを避けるため拒否する。再認証は `login`。

```bash
./bin/codex-pool probe --config pool.json
./bin/codex-pool serve --config pool.json
# 別ターミナル
./bin/codex-pool status --config pool.json
./bin/codex-pool disable --config pool.json ACCOUNT_ID_FROM_STATUS
./bin/codex-pool enable --config pool.json ACCOUNT_ID_FROM_STATUS
```

`probe` は実際の上流利用量を取得する。`status` は起動中サーバーの観測値を表示する。
アカウント追加・enable は次の定期照会（初期値60秒）で反映する。
disable は新しい資格情報の取得時にも確認する。実行中要求は継続する。
M・ルート・listen の設定変更はサーバーを再起動する。

## リフレッシュ

- 期限まで1分以内、期限不明、期限切れで更新。定期利用量照会でもこの判定を行う。
- CPA の singleflight と、このアプリのプロセス間ファイルロックで更新を直列化する。
- ロック取得後にファイルを読み直し、ローテーション前の token を再利用しない。
- 新しい access/refresh token を0600の専用ファイルへ原子的に保存する。
- refresh token が応答に省略された場合は元の値を維持する。
- 更新失敗、空 access token、別アカウントへの変化では既存ファイルを上書きしない。
- `refresh_token_reused` はその更新処理内で再試行しない。定期照会の失敗は選択対象から外す。
- 利用量照会の401だけ、同じアカウントを更新して1回再照会する。生成要求は再送しない。

実装は [store.go](internal/pool/store.go)、[CPA由来の更新処理](internal/cpa/oauth.go)、
結合テストは [poll_test.go](internal/pool/poll_test.go)（模擬token endpoint）。
Linuxでの実リフレッシュと保存の確認は [検証記録](docs/validation.md) に記載している。

## round-robin 専用入口

各 `round_robin_endpoints` の `headers` に `User-Agent`、`originator`、`Accept`、
`Content-Type` を設定できる。号池が上流へ送る際に毎回上書きする（通常URLには適用しない）。
実機で観測した値はLinux側のGit管理外 `pool.json` に保存する。認証は従来どおり選択した
アカウントから設定し、認証ヘッダーや一時的なセッションIDはこの設定に入れない。
HTTP/SSEとWebSocketの接続専用ヘッダーは混用しない。Codex更新後は実通信と再照合する。

画像用の `/_pool/rr/images/generations` は `{"prompt":"画像の指示"}`、
`/_pool/rr/images/edits` は `{"prompt":"編集指示","images":[{"image_url":"data:image/png;base64,..."}]}`
のみをPOSTする。編集は1〜5枚。model・quality・size・background・n等の指定は400になる。
内部で Codex CLI 0.155.1 と同じ `model: gpt-image-2`、quality/size/background: auto に固定し、nは省略する。
通常の画像URLは従来どおり素通し。画像以外の専用RRも従来どおり。
画像RRの転送先は `/backend-api/codex/images/generations` と `/backend-api/codex/images/edits`。
転送先設定が変わった場合は503で停止するため、Codex更新時に画像API実装と照合する。

`init` が生成する `pool.json` の設定:

```json
"round_robin_endpoints": {
  "/_pool/rr/images/generations": {"upstream_path": "/backend-api/codex/images/generations"},
  "/_pool/rr/images/edits": {"upstream_path": "/backend-api/codex/images/edits"},
  "/_pool/rr/responses/compact": {"upstream_path": "/backend-api/codex/responses/compact"},
  "/_pool/rr/responses": {"upstream_path": "/backend-api/codex/responses"}
}
```

呼び出し元は `Authorization: Bearer <state/client.key の内容>` を付け、上流と同じメソッド・本文で呼ぶ。
管理専用 `admin.key` は推論に使えない。異なる上流パスの RR は別の巡回位置を持つ。
通常の Codex 通信がこの入口へ自動変換されることはない。
転送先パスも既知APIの一覧に限定しない。専用URLでは設定されたパスへ置き換え、クエリと本文は保持する。
`round_robin_endpoints` を明示した場合はその設定だけを使う。空の `{}` で専用入口を全て無効にできる。

2026-09-16の実試験では旧 `/responses/compact` は上流404だった。
Codex 0.153.4系の新方式は、通常のResponses要求のinputに `{"type":"compaction_trigger"}` を
加えて圧縮する。この形式を専用RRで使う入口は `/_pool/rr/responses`。
呼び出し元が上流形式の本文を送り、プールは本文を変換しない。既存の `pool.json` には
必要に応じて上記のマッピングを追加する。通常Codexの圧縮は引き続きfill-first。

### 整形済みJSONLから暗号化blobを作る

`scripts/compact-jsonl.py` は、**1行＝1つのResponses API input item** に整えたJSONLを
専用RR入口へ送る。履歴の整形は呼び出し元の責任とし、Codexのrollout JSONLの解釈、
`payload` の抽出、過去の圧縮・巻き戻しの復元は行わない。
空行を除いて各行のJSONをそのまま `input` 配列へ並べ、末尾に `compaction_trigger` を追加する。
各itemの未知フィールド・数値表現も保持し、モデル名は指定値をそのまま送る。

入力例 `prepared.jsonl`:

```jsonl
{"type":"message","role":"user","content":[{"type":"input_text","text":"合言葉は SAPPHIRE_724 です。"}]}
{"type":"message","role":"assistant","content":[{"type":"output_text","text":"覚えました。"}]}
```

Tailscaleで接続できるMacから、リポジトリのディレクトリで実行する:

```bash
python3 scripts/compact-jsonl.py prepared.jsonl \
  --model gpt-6-astra \
  --origin https://pool.example:18473 \
  --key-file state/client.key \
  --output compact.json
```

入力ファイルを `-` にすると標準入力から読む。`--output` 省略時は標準出力へ返す。
`--instructions` で圧縮要求のinstructionsを指定できる。
実アカウントの利用量を消費するが、このコマンドにはMacの透過ブリッジ起動は不要。

出力は `encrypted_content` にblobを含む**圧縮item全体のJSON**。次のResponses要求の
`input` にこのitemをそのまま入れて、その後へ追加の会話を並べる。
blob文字列だけが必要なら `jq -r '.encrypted_content' compact.json` で取得する。
テキスト要約へのfallbackや自動再送は行わず、完了イベントと暗号化itemを受信してから
出力ファイルを0600で保存する。失敗時は既存出力を維持し、入力JSONLを変更しない。

## Mac の透過ブリッジ

**既存のLinuxプールへ別のMacから接続する場合は、
[別Mac向けの導入手順](docs/mac-client.md)を使う。**
clone・ブリッジ用設定・キー取得・各MacのCA登録・捕捉許可・起動・停止をまとめている。

公式推奨の [macOS インストール方法](https://docs.mitmproxy.org/stable/overview/installation/):

```bash
brew install --cask mitmproxy
python3 bridge/run.py ca --config pool.json
```

CA ファイルが生成されたら Ctrl+C で終了し、専用 CA を登録する。
以下は `init` の既定値 `state_dir: "state"` の場合。
[mitmproxy の CA 登録手順](https://docs.mitmproxy.org/stable/concepts/certificates/)に従う。

```bash
chmod 600 state/ca/mitmproxy-ca.pem
sudo security add-trusted-cert -d -p ssl -p basic \
  -k /Library/Keychains/System.keychain state/ca/mitmproxy-ca-cert.pem
python3 bridge/run.py observe --config pool.json
```

実行中の作業を終えてから Codex を再起動し、新規会話で通常通信を確認する。
証明書エラーが出た場合は停止し、利用中の版の OS 信頼への対応を調べる。
証明書検証の無効化や Codex の環境変数設定は行わない。

観測モードを停止し、`codex-pool serve` 起動済みの状態で:

```bash
python3 bridge/run.py pool --config pool.json
```

[Local Capture](https://docs.mitmproxy.org/stable/concepts/modes/#local-capture) は既定で
`codex,Codex` というプロセス名、TLS の対象は `chatgpt.com:443` に絞る。
対象名が異なる場合は `--capture PID` またはプロセス名を指定する。
macOS で許可待ちになる場合は「システム設定 → 一般 → ログイン項目と機能拡張 →
ネットワーク機能拡張」で Mitmproxy Redirector を有効にする。
別ホストにプールを置く場合は `--origin https://実際のプールホスト:ポート` を指定する。
loopback 以外の HTTP は、暗号化された私設経路を確認したうえで `--private-http` を付けた場合のみ。
origin に `/v1` は付けない。

Linuxに集約して複数PCから使う構成と、Tailscaleでの入口は
[Linuxホストの導入手順](docs/linux-host.md) を参照。

WebSocket のフレーム加工を避けるため `websocket=false`、HTTP は body streaming を使う。
ブリッジは専用 client key だけを読む。上流アカウントの OAuth token はプール側だけに置く。
中継停止は Ctrl+C の後 Codex を再起動。専用 CA の秘密鍵は共有しない。
CA を撤去する場合はキーチェーンアクセスで、`state/ca/mitmproxy-ca-cert.pem` と指紋が一致する証明書を選んで削除する。

## 検証・CPA更新

```bash
go test -race ./...
go build -o bin/codex-pool ./cmd/codex-pool
uv venv --python 3.13 bridge/.venv
uv pip install --python bridge/.venv/bin/python -r bridge/requirements-test.txt
bridge/.venv/bin/python -m unittest discover -s bridge -v
python3 -m unittest discover -s scripts -v
```

Go は mock upstream、Python は加えて実 mitmdump + テスト専用 TLS 信頼 + loopback mock pool を使う。
Python の12 bridgeテストに Local Capture・OSの信頼変更・実アカウント通信は含まれない。
SSE の最初の断片が終了待ちにならないこと、WS の断片・ping・close の生バイト保持も確認する。

Local Capture だけを実際の Mac で調べる任意のチェック:

```bash
bridge/.venv/bin/python bridge/check-local-capture.py
```

使い捨てプロセスの PID だけを捕捉し、loopback の模擬プールに届くことを確認する。
テスト用 CA の信頼はそのプロセスだけに設定する。OSへのCA登録やCodexの変更はしない。
初回は mitmproxy が `/Applications/Mitmproxy Redirector.app` を用意し、macOSの拡張許可を求める。
2026-09-15 に拡張を有効にし、`systemextensionsctl list` の `[activated enabled]` を確認。
PID限定チェックは `PASS: direct TLS request from probe PID reached the loopback mock pool`。
テスト用プロセスは終了済み。これはテスト専用プロセスの成功であり、標準Codexの成功を証明するものではない。

同じ日に、標準CLIとの比較に使う専用CAもOSへ登録した。このMacでの実施結果は
[検証記録](docs/validation.md) に記載している。

既存のログイン・設定を使い、使い捨てCLIプロセスのPIDだけで比較するスクリプト:

```bash
python3 scripts/check-native-observe.py direct --hash-file /opt/homebrew/bin/codex
python3 scripts/check-native-observe.py observe --hash-file /opt/homebrew/bin/codex
# アカウント登録・probe成功・serve起動後
python3 scripts/check-native-observe.py pool --hash-file /opt/homebrew/bin/codex
```

実際の短い生成要求を送る。設定・実行ファイルの前後ハッシュ、期待した応答、
observe/poolでは捕捉記録を確認する。出力は専用stateの `checks/` に保存する。
このスクリプトのテストは偽CLIと一時設定を使い、本物のCodexは起動しない。

`state/usage.jsonl` は選択アカウントと観測できた usage を記録する。観測は1MiBまで。
巨大・圧縮・未知形式のデータも転送するが、usage を読めなかった記録の token 数0は実消費0を意味しない。
`usage_observed` がusage取得の有無、`terminal_event` が観測した上流の終端を表す。
HTTP 200やEOFだけでは完了を証明しない。全イベントを観測でき、開始後に終端なく切れた場合は失敗を記録する。
観測上限を超えたSSEなどは完了不明のまま扱う。これは上流の観測記録で、下流への配送完了を証明するログではない。
Codexの保存済みログインやキャッシュを書き換える処理は持たない。
全パスを中継するため、本人情報・コネクタAPIも選択アカウントの認証で上流へ送る。
Codexがキャッシュしたプラン・権限との整合は実機で未確認。

CPA の通常実行や更新へのパッチは不要。出典・適応点・元ファイル変更の確認方法は
[third_party/cpa](third_party/cpa/README.md) を参照。
codex-lbから取り込んだ認証・quota・通信処理と、仕様上取り込まない部分は
[比較記録](docs/codex-lb-review.md) と [出典](third_party/codex-lb/README.md) を参照。
要件ごとの検証済み・未実施・許可待ちは [docs/validation.md](docs/validation.md) に記録している。
