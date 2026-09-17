# CLIProxyAPI から再利用した範囲

出典: [router-for-me/CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI/tree/7bbfeaf8a7acf2cd5a834dcb0842539fe6aabc2b)
（commit `7bbfeaf8a7acf2cd5a834dcb0842539fe6aabc2b`）。
元ファイルのパスと SHA-256 は [sources.json](sources.json)、著作権表示は [LICENSE](LICENSE)。

## そのまま使う知識・実装

- OAuth client ID、token endpoint、PKCE、JWT claims の取り出し方。
- device authorization の要求・ポーリング・code exchange。
- refresh token ごとの singleflight、呼び出し元キャンセルから独立した30秒の更新処理。
- 更新の上限3回の試行、`refresh_token_reused` の再試行停止。
- Codex の quota event → header の読み取りと、その単体テスト。
- WebSocket error の `status` / `status_code` の読み取り（本文の再生成は含めない）。

## このアプリへの適応

- Go package 名を `cpa` に変更。CPA の config、executor、管理SDKへの依存を外した。
- HTTP client をコンストラクタから注入する。device login の対話部分は小さな callback にした。
- CPA の token storage への保存処理は除外。このアプリの専用ファイルへロック付きで保存する。
- OAuth エラーは本文をログに載せず、status と error code だけにする。
- OAuth テストは上記範囲の4件を移植。quota テストは他パッケージとの結合テストを除く。

このアプリ独自の処理は `internal/pool` にある。期限の1分前からの更新判定、
プロセス間ファイルロック、ローテーション後の原子的保存、週リセット順の選択が該当する。
CPA の translator、tool injection、model mapping、generation retry は取り込んでいない。

## CPA 更新時の確認

CPA の更新を本アプリの起動条件にはしない。別途更新した CPA checkout に対して実行する。

```bash
python3 scripts/check-cpa-source.py /path/to/CLIProxyAPI --ref HEAD
```

スクリプトは該当10ファイルの変更だけを表示する。fetch、checkout、上書きは行わない。
変更があれば、このディレクトリに記載した範囲に関係する差分を確認し、必要な部分を移植する。
移植後は `sources.json` の pin と hash を更新し、`go test -race ./...` を実行する。

固定した元版との照合:

```bash
python3 scripts/check-cpa-source.py /path/to/CLIProxyAPI --ref 7bbfeaf8a7acf2cd5a834dcb0842539fe6aabc2b
```
