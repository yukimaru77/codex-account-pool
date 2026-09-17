# Remote KB

`kb-repomap` の `kb codex NAME --remote` と組み合わせて使う、任意のセッションだけのKB挿入機能。
Codexの設定・ログイン・環境変数・バイナリは変更しない。Macの透過ブリッジとLinuxの号池は従来どおり使う。

## 動作

1. kbコマンドが保存先Gitから選んだJSONLを取得し、`response_item` の配列を号池へ登録する。
   複数の独立したcompaction blobとKB憲章を、元の順序のまま含める。
2. 号池は内容のSHA-256でスナップショットを固定し、新しいCodexセッションIDに紐付ける。
   同じIDへの同じ内容の登録は再実行可能。別内容への変更は409になる。
3. 登録済みセッションの `/backend-api/codex/responses` に限り、推論時の `input` にKBを挿入する。

```text
instructions（変更しない）
input:
  先頭のsystem/developer項目（変更しない）
  固定したKB配列
  会話のcompaction blob（ある場合）や会話履歴
  新しいユーザー入力
```

KBを末尾に追記しない。各セッションで内容・順序・挿入位置を固定する。
ただし、Codex自身が先頭の指示・ツール等を変更した場合まで、KVキャッシュのヒットを保証するものではない。
KB本体はCodexのJSONLへ書き込まず、号池から上流へ送る際にだけ加える。
`latest.jsonl` の変更や別名KBの再作成は、開始済みセッションに影響しない。

親子関係は `x-codex-parent-thread-id` / `x-codex-turn-metadata` と
`client_metadata` で読み、子・孫セッションも同じスナップショットを継承する。
Codexの `Session-Id` / `Thread-Id` で接続を特定する。これらの識別情報を送らない別クライアントには
自動適用されない。手動forkは親子情報を送るサブエージェントとは異なり、自動継承の対象ではない。

## コンパクトとWebSocket

- `/backend-api/codex/responses/compact`、`input` の `compaction_trigger`、
  メタデータの `request_kind: compaction` はKBを挿入しない。
- 通常のWebSocket差分要求は `previous_response_id` を保持し、上流に既にあるKBを再利用する。
- 前回応答の参照がKBを含む状態からコンパクトへ切り替わる場合は、接続内で保持した
  **Codex自身の履歴と上流応答**から完全なinputを作り、参照を解除して送る。KBは含めない。
- 圧縮後は、新しい会話blobより前に同じKB配列を置く。
- 接続上で確認していない `previous_response_id` は復元できないので、そのRemote KB接続を終了する。
  HTTPのRemote KB要求では完全なinputを使う。現在のCodexのHTTP経路はこの形式。
- Remote KB接続だけWebSocket拡張圧縮を交渉せず、通常フレームで通信する。
  WebSocket自体は維持し、分割フレーム・ping・closeも扱う。HTTP要求のgzip/zstdは復号に対応する。
  編集したHTTP本文は非圧縮で上流へ送り、編集不要なら元の圧縮bytesも保持する。

変更対象はKB挿入と必要な履歴参照の展開だけ。その他のJSON値・未知フィールド・ツール定義は保持し、
応答本文とイベントは編集しない。登録されていない通信、画像等の別ルート、未知のルートは従来どおり素通り。
アカウント選択は従来どおり通常fill-first、明示設定のRR URLだけround-robin。

## APIと保存場所

`Authorization: Bearer <state/client.keyの値>` で以下を呼ぶ。管理キーは不要。

```text
POST /_pool/kb/bind
{"session_id":"CodexのセッションID","items":[<元のResponses API項目>, ...]}

GET /_pool/kb/bind?session_id=CodexのセッションID
```

成功時は `session_id`, `snapshot_id`, `item_count` を返す。GETはKB本文を返さない。
新規登録・継承結果はLinux側の `state/remote-kb/sessions/`、固定したKB本体は
`state/remote-kb/snapshots/` に保存する。ファイルは0600、ディレクトリは0700、Git管理対象外。
号池を再起動しても登録は残る。新しい設定ファイルやMacブリッジの変更は不要。
KBの内容を変更して使うときは、新しい `kb codex NAME --remote` セッションを開始する。

起動直後の先行接続を避けるため、kbコマンドは空のネイティブセッションを作成・登録・永続化し、
一度app-serverを終了してから再開する。その後に最初の質問やGit差分を送る。

## 検証

```bash
go test -race ./...
go build -o bin/codex-pool ./cmd/codex-pool
```

`internal/pool/remote_kb_test.go` は登録・再起動・子孫への継承・不変性・挿入位置・全blobの保持・
圧縮要求・コンパクトの除外・WebSocket差分の再利用/展開・分割/制御フレーム・応答保持を
モック上流で検証する。ネイティブプロトコルの確認元はCodex 0.153.4系の `responses_metadata.rs`、
`client.rs`、`compact_remote_v2_attempt.rs`、`session_startup_prewarm.rs`。

### 2026-09-17の実通信

MacからTailscale経由でLinuxの稼働プールに接続し、上流の実APIで確認した。

- 異なる2つの架空の値をv2の暗号化KB blobにし、同じセッションへ登録。
- WebSocketの完全入力・`previous_response_id` による差分・HTTPでKB内の値に正答。
- WebSocketの差分でコンパクトを要求し、正規の暗号化compaction blobを取得。
- そのblobをKB未登録の要求へ渡すと、未質問だったKB内の値への回答は `UNKNOWN`。
- 登録セッションの圧縮後の要求では、その未質問だった値に正答。
- 親IDを指定した別接続でも同じKBを継承して正答。

この実試験はネイティブと同じAPI形式の直接要求で実施した。Macの `codex app-server` は
KB登録前のSQLite初期化で停止したため、`kb codex --remote` のネイティブ起動から通す
実試験は未完了。CodexのDB・設定・バイナリは修正していない。CLIの起動順序・登録失敗時の
停止・既存コマンドとの互換性はkb-repomap側のユニットテストで検証している。
