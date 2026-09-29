# Codex account pool（ローカル専用）

このリポジトリは、1台のMacまたはLinux上で複数のCodexアカウントを切り替えるローカル号池です。
外部号池、Tailscale、Linux集約、mitmdump透過ブリッジは使いません。

## インストール

```bash
scripts/install-local.sh
```

インストーラは `codex-pool`、`codex` ラッパー、`pool-rr`、`~/.codex-pool/`、およびOSの常駐サービスを作ります。
号池は `127.0.0.1:18473` だけで待ち受けます。

## アカウント登録

```bash
codex-pool account add main --config ~/.codex-pool/pool.json --from ~/.codex/auth.json
codex-pool account add sub1 --config ~/.codex-pool/pool.json
codex-pool account list --config ~/.codex-pool/pool.json
```

`--from` を省略すると、そのアカウント専用の `CODEX_HOME` で通常の Codex device login を実行します。
各アカウントの認証は `~/.codex-accounts/` に保存されます。

## 起動と選択

```bash
codex
codex exec "質問"
codex-pool status --config ~/.codex-pool/pool.json
```

`codex` ラッパーは週の残量とリセット時刻でアカウントを選び、選択した `CODEX_HOME` で本物のCodexを起動します。
通常のCodex設定やログイン状態を共有上書きしません。

## KB / round-robin

インストーラが作る `~/.codex-pool/kb-pool.json` を `kb-repomap` の `--pool-config` に指定します。
`kb-repomap` は `http://127.0.0.1:18473/_pool/rr` を使います。

```bash
pool-rr codex exec "このリポジトリを調べて"
pool-rr kb paper-demo --remote codex exec "この論文の要点を説明して"
```

`Remote KB` は外部サーバーを意味せず、ローカル号池へのKB登録機能です。
`kb NAME claude` は復号済み資料をClaude Codeへ渡すだけで、号池は使いません。

## 設定例

[pool.example.json](pool.example.json) はローカル待受、アカウント別 `CODEX_HOME`、RR入口の例です。
実際の認証情報とstateはGit管理外に置きます。

## 検証

```bash
go test ./...
go build -o ~/.local/bin/codex-pool ./cmd/codex-pool
scripts/install-local.sh
codex-pool account list --config ~/.codex-pool/pool.json
```
