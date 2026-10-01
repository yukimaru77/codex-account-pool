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

## round-robin

`/_pool/rr/*` は要求ごとにアカウントを巡回する入口です。
インストーラが作る `~/.codex-pool/rr.json` に、そのbase URLとクライアントキーのパスがあります。

```json
{"base_url": "http://127.0.0.1:18473/_pool/rr", "key_file": "/Users/me/.codex-pool/state/client.key"}
```

```bash
pool-rr codex exec "このリポジトリを調べて"
```

kb-repomap などのRRクライアントは、RRのbase URLを `http://127.0.0.1:18473/_pool/rr` に向けて使えます。
号池はRRクライアントを起動・設定しません。

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
