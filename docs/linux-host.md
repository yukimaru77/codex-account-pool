# Linuxにプールを集約してTailscaleから使う

各PCの透過ブリッジ → Tailscale → Linux上のプール → OpenAI。
アカウント・refresh token・週の残量・選択状態はLinuxの1プロセスで管理する。
通常URLは全てfill-first、明示した専用URLだけround-robin／管理処理になる。

以下の `user@pool-host` は自分のLinuxユーザーとTailscaleホスト名へ置き換える。
実際のIP・ホスト名・配置先は各自の環境で決め、リポジトリへcommitしない。

## 1. Linuxで初期化する

Go 1.26以降を使う。以下はホームディレクトリへ配置する例。

```bash
cd "$HOME"
git clone https://github.com/yukimaru77/codex-account-pool.git
cd codex-account-pool
go build -o bin/codex-pool ./cmd/codex-pool
./bin/codex-pool init --config pool.json
```

`init` はローカルの `pool.json` と `state/` を作る。既存の設定がある場合は再実行しない。
初期待受は `127.0.0.1:18473`。全設定の例は [pool.example.json](../pool.example.json)。

## 2. Tailscale側の接続先を設定する

```bash
tailscale ip -4
```

表示されたLinux側のIPを使い、Git管理外の `pool.json` の `listen` を
`自分のTailscale IP:18473` に編集する。`state_dir` やキーは保持する。
tailnet内のクライアントからこのポートへ接続できるアクセス設定が必要。

別案として、待受をループバックのままにしてTailscale ServeのTCP転送を使える。
既存設定・ポートを確認してから設定する。この場合は待受IPの編集は不要。

```bash
tailscale serve status
tailscale serve --bg --tcp 18473 tcp://127.0.0.1:18473
tailscale serve status
```

## 3. アカウントを登録して起動する

```bash
./bin/codex-pool login --config pool.json
# 必要なNアカウント分繰り返す
./bin/codex-pool probe --config pool.json
./bin/codex-pool serve --config pool.json
```

表示されたURLとdevice codeで、このアプリ用のアカウントを登録する。
Codex本体のログイン情報は変更しない。
`probe` は実際の上流利用量を取得し、`serve` はフォアグラウンドで常駐する。
同じstateを使うプールサーバーは1プロセスだけ起動する。

設定・キー・アカウントファイルは0600、stateディレクトリは0700で扱う。
`pool.json` と `state/` はGit管理外。OAuth情報・管理キーはLinuxに置き、
Macへ渡すのはブリッジ用の `state/client.key` だけ。

## 4. 常駐管理

ホストの管理方式に合わせる。以下はsystemdを使わない環境でのSupervisorの例。
フォアグラウンドの `serve` を終了してから起動する。

```bash
cd "$HOME/codex-account-pool"
uv venv "$HOME/.local/share/codex-account-pool-supervisor"
uv pip install --python "$HOME/.local/share/codex-account-pool-supervisor/bin/python" supervisor==4.3.0

"$HOME/.local/share/codex-account-pool-supervisor/bin/supervisord" -c deploy/supervisord.conf
"$HOME/.local/share/codex-account-pool-supervisor/bin/supervisorctl" -c deploy/supervisord.conf status
```

リポジトリ内の `deploy/supervisord.conf` は配置先に相対的なパスを使う。
SSH切断後も動作し、プールプロセスが終了すると再起動する。
ログ・socketはGit管理外の `state/` へ置き、ログにはサイズ上限がある。

管理コマンドの `status` を、`stop codex-account-pool`、`start codex-account-pool`、
`restart codex-account-pool` に替えると停止・開始・再起動できる。

**ホスト／コンテナ全体の再起動後の自動起動は、この手順では設定しない。**
再起動後はTailscaleとSupervisorを起動するか、利用環境の起動機構へ登録する。

## 5. 各Macを接続する

[Mac導入手順](mac-client.md)に従う。`bridge.json` に実際の号池originを保存し、
Tailscaleの暗号化経路を確認してHTTPを使う場合は `private_http: true` にする。
HTTPSを用意した環境ではHTTPSのoriginを指定する。

Macでは専用CAを個別に作る。他のMacのCA秘密鍵やLinuxのOAuthファイルをコピーしない。
管理用 `/_pool/status` はブリッジ用キーでは401になる。
管理操作はLinux上で実行する。

```bash
./bin/codex-pool status --config pool.json
```

## 更新

通常の公開版同士の更新は、プールを停止してから `git pull --ff-only`、再ビルド、再起動する。
`pool.json` と `state/` は維持する。

公開前の非公開版checkoutは履歴が異なる。初回のみ公開版を別ディレクトリへcloneし、
停止中に既存の `pool.json` と `state/` を権限ごと引き継いでから起動する。
`state_dir` が絶対パスならその保存先を引き続き使う。元のcheckoutは動作確認まで保持する。
同じアカウントstateを使う旧版と新版を同時起動しない。

## 検証範囲

Linuxのx86_64とarm64でビルドし、実LinuxプールでTailscale経由の通信と
Supervisorによるプールプロセスの再起動を確認した。
ホスト全体の再起動試験は未実施。実通信の結果と制限は [検証記録](validation.md) を参照。
