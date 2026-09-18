# 別のMacから既存のLinuxプールを使う

Linuxのプールを先に [Linux導入手順](linux-host.md) で用意する。
MacにはPython製のブリッジだけを置く。GoのビルドやMacでのプール用ログインは不要。

```text
このMacのCodex → このMacの透過ブリッジ → Tailscale → Linuxのプール → OpenAI
```

Codexの設定・ログイン・環境変数・バイナリは編集しない。
Mac側では専用CAの信頼登録と、mitmproxyの通信捕捉拡張の有効化を行う。
通常URLは画像・compactも含めてLinux側のfill-first、明示した専用URLだけround-robinになる。

以下の `user@pool-host` は自分のLinuxユーザーとTailscaleホスト名へ置き換える。
実接続先はGit管理外の `bridge.json` に保存する。

## 1. Tailscaleとリポジトリを用意する

- [TailscaleのMac版](https://tailscale.com/download/mac)で、Linuxプールと同じtailnetに接続する。
- [Homebrew](https://brew.sh/)を導入済みのターミナルを使う。
- Codexは標準OpenAI接続・通常のChatGPTログインで使える状態を前提とする。
  以前のカスタムprovider等がある場合、この手順では自動変更しない。

```bash
brew install uv
git clone https://github.com/yukimaru77/codex-account-pool.git
cd codex-account-pool

tailscale status
tailscale ssh user@pool-host true
```

SSHに追加認証URLが表示されたら、そのURLで認証する。
`tailscale` が見つからない場合は [Mac導入案内](https://tailscale.com/docs/install/mac) を参照。
アプリ版のCLI実体がある場合は `/Applications/Tailscale.app/Contents/MacOS/tailscale` も使える。

以下は `codex-account-pool` フォルダ内で実行する。

## 2. ブリッジ環境とローカル設定を作る

実機試験に使ったPython 3.13・mitmproxy 12.2.3を専用環境へ入れる。

```bash
uv venv --python 3.13 bridge/.venv
uv pip install --python bridge/.venv/bin/python 'mitmproxy==12.2.3'
mkdir -p state
chmod 700 state
```

新規clone後、`bridge.example.json` を `bridge.json` にコピーする。
既存の `bridge.json` がある場合はコピーし直さない。

```bash
cp -n bridge.example.json bridge.json
chmod 600 bridge.json
```

`bridge.json` の `origin` を自分の号池のoriginへ編集する。末尾に `/v1` は付けない。
キーそのものは設定に書かず、次の手順で `state/client.key` に保存する。

```json
{
  "state_dir": "state",
  "origin": "https://pool.example:18473",
  "private_http": false,
  "exclude_codex_app": false
}
```

Tailscale経由のHTTPを使う場合は、LinuxのTailscale IPまたはホスト名をoriginへ指定する。
`tailscale status` と `route -n get 自分のLinuxのTailscale_IP` で対象を確認し、
Tailscaleの暗号化経路であることを確認した場合だけ `private_http` を `true` にする。
HTTPSを用意した環境では `false` のまま使う。

`bridge.json`、`pool.json`、`state/`、`bridge/.venv/` はGit管理外。
`state_dir` は設定ファイルの置かれたディレクトリを基準に解決する。

## 3. Linuxからブリッジ用キーを取得する

以下はLinuxの配置先が `$HOME/codex-account-pool` の例。
異なる配置先を使う場合は、その `state/client.key` を指定する。

```bash
(
  umask 077
  tailscale ssh user@pool-host \
    'cat "$HOME/codex-account-pool/state/client.key"' > state/client.key.tmp &&
  chmod 600 state/client.key.tmp &&
  mv state/client.key.tmp state/client.key
)
```

取得するのはクライアントキー1つだけ。現在の実装では各Macが同じキーを使う。
`state/accounts/`、`admin.key`、他のMacのCA秘密鍵はコピーしない。

## 4. このMac専用のCAを作って信頼登録する

```bash
bridge/.venv/bin/python bridge/run.py ca --config bridge.json \
  --mitmdump bridge/.venv/bin/mitmdump
```

別のターミナルから `state/ca/mitmproxy-ca-cert.pem` の作成を確認し、Ctrl+Cで終了する。
[mitmproxyのCA手順](https://docs.mitmproxy.org/stable/concepts/certificates/)に従い登録する。

```bash
chmod 600 state/ca/mitmproxy-ca.pem
sudo security add-trusted-cert -d -p ssl -p basic \
  -k /Library/Keychains/System.keychain state/ca/mitmproxy-ca-cert.pem
security verify-cert -c state/ca/mitmproxy-ca-cert.pem -p basic
```

成功表示は `certificate verification successful.`。

## 5. 通信捕捉を許可して公式接続で確認する

```bash
bridge/.venv/bin/python bridge/run.py observe --config bridge.json \
  --mitmdump bridge/.venv/bin/mitmdump
```

初回はMitmproxy Redirectorの導入・有効化を求められる場合がある。
「システム設定 → 一般 → ログイン項目と機能拡張 → ネットワーク機能拡張」で有効にする。
OSによって画面名は異なる。許可後、ブリッジを停止して同じコマンドで起動し直す。

```bash
systemextensionsctl list
```

`org.mitmproxy.macos-redirector.network-extension` の `[activated enabled]` を確認する。
Codexの作業を終えて再起動し、新規会話で短い要求を送る。
observeは公式の接続先・認証を維持する。証明書エラーなら停止し、検証の無効化では回避しない。

[Local Capture](https://docs.mitmproxy.org/stable/concepts/modes/#local-capture) は既定で
プロセス名 `codex,Codex` の `chatgpt.com:443` 宛て通信を捕捉する。
対象が異なる場合は `--capture 対象PID` またはプロセス名を指定する。

```bash
ps -axo pid,comm,args | grep -i '[c]odex'
```

## 6. Linuxプールへの中継を起動する

observeをCtrl+Cで終了し、起動する。接続先とHTTP許可は `bridge.json` から読む。

```bash
bridge/.venv/bin/python bridge/run.py pool --config bridge.json \
  --mitmdump bridge/.venv/bin/mitmdump
```

**実行中のCodexは作業を終えてから再起動する。** 起動前の接続はそのまま残る場合がある。
ブリッジが動作している間、中継が有効になる。Mac起動時の自動起動は設定しない。

### Codex Appは直接接続し、CLIだけ号池を使う

Macの `bridge.json` に `"exclude_codex_app": true` を設定し、ブリッジを再起動する。
一度の起動だけ指定する場合は次のオプションを使う。

```bash
bridge/.venv/bin/python bridge/run.py pool --config bridge.json \
  --exclude-codex-app --mitmdump bridge/.venv/bin/mitmdump
```

Codex App（`Codex.app` または `ChatGPT.app`）内の実行ファイルを、Local Captureの
捕捉対象から外す。App内の `codex` やhelperも対象。Appの通信は号池・透過ブリッジを
経由せず、元の接続先・ログインを使う。通常のCLIは引き続き号池へ送る。
Codex本体の設定・認証・環境変数は変更しない。Linux側の変更も不要。

既定値は `false`（従来どおりAppも捕捉）。CLIの `--no-exclude-codex-app` で
設定ファイルの `true` を一度だけ上書きできる。切り替え後、Appの実行中の作業を終えて
Appを再起動すると、既存の接続も切り替わる。

判定はプロセス名だけでなく実行ファイルのパスに基づく。
App内蔵の `codex` をターミナルから直接実行した場合も除外される。
Appバンドルを別名に変更した環境では `--capture 'codex,Codex,!/変更後の名前.app/'`
で明示できる。除外中のAppには、号池が行うRemote KBの挿入も適用されない。

仕組みはmitmproxyの [Local Captureの除外指定](https://docs.mitmproxy.org/stable/concepts/modes/#intercept-specs)
と、macOS実装の [実行ファイルパスの部分一致](https://github.com/mitmproxy/mitmproxy_rs/blob/main/mitmproxy-macos/redirector/network-extension/InterceptConf.swift)
を使う。HTTPヘッダーやリクエスト本文でAppを推測する処理は追加しない。

2026-09-18のMac実機確認では、同名 `codex` のTLS試験用実行ファイルをCLI相当の場所と
`Codex.app/Contents/MacOS/`、`ChatGPT.app/Contents/Resources/` に配置した。
CLI側だけmitmproxyの証明書、App側2件は公式サイトの証明書を受け取り、いずれもTLS検証に成功した。
この試験はHTTP本文・認証情報を送信していない。別途、標準Codex CLIの実要求が号池へ転送され、
`CLI_POOL_OK` と応答することも確認した。Appの全機能の動作試験ではない。

### CLIの実通信テスト

全プロセス対象のブリッジを停止した状態で、次のPID限定の実通信テストも使える。

```bash
bridge/.venv/bin/python scripts/check-native-observe.py pool --config bridge.json \
  --mitmdump bridge/.venv/bin/mitmdump
```

このスクリプト自身がブリッジを起動・終了する。Codexの設定・実行ファイルの前後ハッシュ、
応答、捕捉の有無を `state/checks/` へ保存する。
Codexの実行ファイルが独自の場所にある場合は `--codex /path/to/codex` を指定する。

アカウント登録・残量確認はLinux側で行う。管理用 `/_pool/status` はクライアントキーでは401になる。

## 停止・撤去・更新

- 停止はCtrl+Cの後、Codexを再起動する。
- CAの信頼と通信捕捉拡張は停止後も残る。
- CAの撤去は次の指紋を確認し、キーチェーンアクセスの「システム」で一致する証明書を削除する。
  通信捕捉拡張が不要になった場合はシステム設定で無効化する。

```bash
openssl x509 -in state/ca/mitmproxy-ca-cert.pem -noout -fingerprint -sha256
```

公開版同士の更新は、ブリッジを停止して `git pull --ff-only`。
既存の `bridge.json`・`pool.json`・キー・CAは維持し、手順6で起動する。
公開前の非公開版は履歴が異なるため、公開版を別ディレクトリへcloneし、
停止中に設定と `state/` を権限ごと引き継ぐ。専用Python環境は手順2で作り直す。

2026-09-15の実機確認はmacOS 26.4.1／Apple Silicon／Codex CLI 0.153.4の1台。
全MacやCodex Appの全機能まで検証済みという意味ではない。[検証記録](validation.md)を参照。
