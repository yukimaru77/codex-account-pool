# ローカル号池（CODEX_HOME 方式）設計

日付: 2026-09-29

## 目的

号池（codex-account-pool）の機能を変えずに、機構だけを次のように差し替える。

- アカウントの認証は **素の Codex で正規にログインして生まれた `auth.json`** をそのまま使う。号池がヘッダを付け替えるのではなく、Codex 自身が最初から最後までそのアカウント本人として動く。
- 号池プロセスは **各ローカル PC で 1 つずつ**動かす。PC 間の共有・同期はしない。
- Mac 側の mitmdump 透過ブリッジ（chatgpt.com 横取り、CA、accounts/check 書き換え）は廃止する。

維持する機能: fill-first 選択、`/_pool/rr/*` の round-robin（画像生成・編集、コンパクション、メッセージ RR）、残量監視（usage ポーリング + 応答ヘッダ + rate_limits イベント）、`/_pool/kb/bind` と推論時 KB 注入、`kb-repomap` からの利用。

仕様として捨てるもの: Codex App（Codex.app / ChatGPT.app）の透過対応。provider 設定を変えられないクライアントは対象外。

## 全体像

```
~/.codex-accounts/                 アカウントディレクトリ（accounts_dir）
  alice/  auth.json  models_cache.json  (共通物は ~/.codex への symlink)
  bob/    auth.json  ...
  carol/  auth.json  disabled          ← 無効化マーカー

codex（ラッパー）                    残量を見てディレクトリを選び CODEX_HOME を設定して素の codex を exec
codex-pool serve                    127.0.0.1:18473。accounts_dir を読んで RR / 圧縮 / 画像 / KB 注入を提供
kb-repomap                          KB_POOL_ORIGIN=http://127.0.0.1:18473 で今まで通り
```

対話セッション（`codex`、`kb NAME codex`）は号池を経由せず、選ばれた CODEX_HOME のアカウントとして直接 chatgpt.com と通信する。号池を経由するのは、単発要求（RR 入口）と `kb --remote` の推論時注入セッション（`kb_pool` provider で号池に直接接続）だけ。この 2 経路は今と同じ。

## コンポーネント

### 1. アカウントディレクトリ（Codex 形式ストア）

`accounts_dir/<name>/` が 1 アカウント。`<name>` はユーザーが付ける短い名前（英数・`-`・`_`）。

- `auth.json`: Codex が書く形式そのまま。`{"auth_mode","OPENAI_API_KEY","tokens":{"id_token","access_token","refresh_token","account_id"},"last_refresh"}`。
- `models_cache.json`: Codex が生成する。号池は触らない。
- `disabled`: 存在すれば号池もラッパーもそのアカウントを使わない（Codex が所有する auth.json に号池の状態を書き込まない）。
- `.pool.lock`: 号池の refresh 用 flock ファイル。
- 共通物: `codex_home`（既定 `~/.codex`）の直下エントリのうち、除外リスト `{auth.json, models_cache.json, log, tmp}` 以外を **symlink** する。config.toml、sessions、skills、memories、rules、plugins、sqlite、state などが共有される。除外したものは各ディレクトリで Codex が個別に作る。

号池内部のアカウント ID は従来通り `sha256(account_id)` の先頭 16 バイト hex。scheduler・quota・usage.jsonl のキーは変わらない。ディレクトリ名は表示用。

### 2. `AccountStore` インターフェースと Codex 形式ストア

現在 `Store`（`internal/pool/store.go`）は `state/accounts/<id>.json` の CPA 形式を読む具象型で、`Handler`・`poll` が直接依存している。これを次のインターフェースに切り出し、既存実装を `legacyStore` として残す。

```go
type AccountStore interface {
    List() ([]Credential, error)
    Token(ctx, id string, force bool) (Credential, error)
    RefreshRejected(ctx, id, accessToken string) (Credential, error)
    SetDisabled(id string, disabled bool) error
}
```

`Credential` に `Name string`（ディレクトリ名、legacy では空）を追加し、`/_pool/status` の応答にも `name` を含める。

新実装 `codexHomeStore`:

- `List`: `accounts_dir` を走査し、`auth.json` を読んで `Credential` に変換する。`account_id` は `tokens.account_id`、無ければ id_token の claim。`email` は id_token の claim から。`expired` は access_token JWT の `exp`。`disabled` はマーカーファイルの有無。壊れた auth.json は該当アカウントをスキップしてログに出す（他のアカウントを止めない）。
- `Token(force=false)`: `.pool.lock` を flock → auth.json を再読込 → `exp` が 1 分以内なら refresh → Codex 形式で `auth.json` を原子的に書き戻す（`tokens.*` と `last_refresh` だけ更新し、他フィールドは保持）。refresh の回転規則は既存 `store.token` と同じ（account_id が変わる応答は拒否、refresh_token が無ければ旧値を保持）。
- `RefreshRejected`: 既存と同じ。拒否された access_token が今もファイル上の値と一致するときだけ強制 refresh。Codex 側が先に更新していれば再読込だけで済む。
- `SetDisabled`: マーカーファイルの作成/削除。

refresh の主体について: Codex 本体も号池も auth.json を原子的に書き換える。両者は「期限が迫ったときだけ更新」「更新前に必ず再読込」なので、同時 refresh はレアケースに留まる。起きた場合は 401 → `RefreshRejected` → 再読込で回復する。これは現行の Linux 号池で複数プロセスが同じ CPA ファイルを共有している状況と同等。

config の切替: `pool.json` に `accounts_dir` があれば `codexHomeStore`、無ければ従来の `state/accounts`（legacy）。Linux 版はそのまま動き、段階移行できる。

### 3. `codex-pool account` サブコマンド

- `account add NAME [--from PATH]`: ディレクトリ作成 → 共通物 symlink → `--from` があればその auth.json をコピー（初回移行で `~/.codex/auth.json` を取り込む用）、無ければ `CODEX_HOME=<dir> <codex_bin> login` を対話実行。終了後 auth.json を読んで account_id/email を表示。refresh token は使い捨てなので、`--from` の元が `<codex_home>/auth.json` なら元ファイルをアカウント側 auth.json への symlink に置き換える（バックアップは残さない）。それ以外の元ファイルは触らず警告を出す。別の号池に登録済みのアカウントはコピーせず `account add NAME` で別途ログインする。
- `account list`: name、email、account_id、disabled、残量（号池が動いていれば `/_pool/status` から、無ければ直接 probe）。
- `account enable|disable NAME`: マーカー操作。
- `account relink NAME|--all`: 共通物 symlink を作り直す（`~/.codex` に新しい共有物が増えたとき用）。

既存の `login`（device flow）と `import`（CPA 形式）は `accounts_dir` 設定時にはエラーにして `account add` を案内する。legacy 設定では従来通り。

### 4. 起動ラッパー `codex-pool launch`

`codex-pool launch --config pool.json [--account NAME] -- <codex args...>`

1. `--help`/`--version` だけなら選択せず即 exec。
2. 候補 = `accounts_dir` の有効アカウント。残量は `/_pool/status`（admin.key）から取得。号池が応答しなければ各アカウントを直接 probe（既存 `probe` の関数を再利用、並列 4、タイムアウト 10 秒）。
3. 選択は scheduler の fill-first と同じ規則（reserve_percent 以下は除外、週次リセットが早い順、同率は名前順）。`--account` または環境変数 `CODEX_POOL_ACCOUNT` で固定できる。
4. `CODEX_HOME=<dir>` を設定し、`codex_bin` を `syscall.Exec` する。stderr に 1 行 `codex-pool: account=<name> remaining=<n>%` を出す（`--quiet` で抑止）。
5. 使えるアカウントが無ければ、残量が最も多いものを選んで警告を出す（起動を止めない）。

ラッパースクリプト `~/.local/bin/codex`（インストーラが置く）:

```sh
#!/bin/sh
exec codex-pool launch --config "$HOME/.codex-pool/pool.json" -- "$@"
```

`codex_bin` は既定で PATH 上のラッパー自身を除いた次の `codex`（現環境では `~/.local/bin/convenient-codex` ランチャー）。pool.json で明示できる。

`kb NAME codex ...` は最終的に `codex` を exec するので、そのままラッパーを通る。`kb --remote` は `kb_pool` provider で号池へ直接つなぐため、選択は号池側で行われる（今と同じ）。

### 5. 号池本体の変更

- `Handler` と `poll` の `*Store` 依存を `AccountStore` に変更。
- `/_pool/status` に各アカウントの `name` を追加。
- **round-robin route の `headers`**: `Route` に `Headers map[string]string` を追加し、上流へ送る前に設定する。kb-repomap の README が前提にしている機能で、現在は未実装。
- `round_robin_endpoints` を省略したときの既定 4 入口（images/generations、images/edits、responses/compact、responses）はそのまま。インストーラは省略形で pool.json を生成し、`/_pool/rr/responses` が抜ける現行 pool.json の不具合を再現させない。
- `private_http` は号池の設定ではなく、ループバック待受では不要。kb-repomap 側は loopback origin を平文で許容している（`kb_api.py`）ので変更なし。

### 6. インストーラと常駐

`scripts/install-local.sh`（macOS / Linux）:

1. `go build -o ~/.local/bin/codex-pool ./cmd/codex-pool`
2. `~/.codex-pool/pool.json` を `codex-pool init` で生成し、`accounts_dir`、`codex_home`、`codex_bin` を追記。`listen` は `127.0.0.1:18473`。
3. 常駐: macOS は `~/Library/LaunchAgents/com.local.codex-pool.plist`（KeepAlive）、Linux は `~/.config/systemd/user/codex-pool.service`。ログは `~/.codex-pool/state/serve.log`。
4. `~/.local/bin/codex` ラッパーを置く（既存の `codex` が PATH 上にあれば `codex_bin` にその絶対パスを記録してから置く）。
5. `kb-repomap` 向けに `KB_POOL_ORIGIN=http://127.0.0.1:18473` と `KB_POOL_KEY_FILE=~/.codex-pool/state/client.key` を表示（`~/.zshrc` は手で更新）。

移行手順（この Mac）:

1. インストール → `account add main --from ~/.codex/auth.json` で現在のアカウントを取り込む → 他アカウントは `account add NAME` で正規ログイン。
2. `codex exec 'hi'` がラッパー経由で動くこと、`kb create` の圧縮が `127.0.0.1:18473` の RR で動くこと、`kb NAME --remote codex` の注入が動くことを確認。
3. mitmdump の LaunchAgent `com.local.codex-account-pool-bridge` を unload。`~/.zshrc` の `KB_POOL_ORIGIN` を loopback に変更。`bridge.json` は不要になる。

## エラー処理

- auth.json が壊れている / 読めない: そのアカウントだけスキップ、`/_pool/status` に `error` を載せる。
- 号池が落ちている: ラッパーは直接 probe にフォールバック。probe も失敗したら残量不明のまま名前順で最初の有効アカウントを選び、警告を出す。
- refresh 失敗（invalid_grant 等）: 既存どおりアカウントを failed にし、次の probe 成功で復帰。ラッパーは failed を除外する。
- symlink 先の `codex_home` エントリが消えた: `account relink` で再作成。ラッパーは壊れた symlink を検出したら警告のみ。

## テスト

- `codexHomeStore`: 一時ディレクトリに Codex 形式 auth.json を置き、List / Token（期限内は無 refresh、期限切れはモック refresh → 書き戻しがフィールド保持と原子性を満たす）/ RefreshRejected（ファイルが先に更新済みなら refresh しない）/ disabled マーカー / 壊れたファイルのスキップ。
- `launch` の選択ロジック: status 応答のモックから fill-first の順序、reserve 以下除外、`--account` 固定、全滅時の警告付きフォールバック。exec 部分は関数注入で置き換え、`CODEX_HOME` と引数を検証。
- `account add`: symlink の集合が除外リストを守ること、`--from` のコピー、既存名の拒否。
- Route headers: RR 入口の要求に headers が付くこと、fill-first 入口には付かないこと（relay の既存テストに追加）。
- 既存テスト（`go test -race ./...`）が legacy ストアで全て通ること。
- 手動 E2E: 上記「移行手順 2」。

## 対象外

- PC 間のアカウント・残量共有。
- 生成要求の途中フェイルオーバー（既存どおり、エラーはクライアントに返し次の要求で別アカウント）。
- Codex 本体（convenient-codex）への切替ロジック組み込み。
- Windows（flock 依存は既存のまま）。
