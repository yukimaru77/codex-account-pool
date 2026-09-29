# 各PCでローカルにプールを動かす

`codex`（ラッパー） → 残量で選んだ `CODEX_HOME` → 素の Codex → OpenAI。
各アカウントは素の Codex で正規にログインした `auth.json` をそのまま使い、
対話セッションはそのアカウント本人として直接通信する。
プール（`codex-pool serve`）はPCごとに1つ、`127.0.0.1:18473` で動かし、
残量監視・`/_pool/rr/*` のround-robin・`kb-repomap` のKB注入を担当する。
PC間でアカウントや残量は共有しない。

Macの透過ブリッジ（mitmdump・専用CA）は使わない。
既存のブリッジ構成から移る場合は「ブリッジからの移行」を参照。

## 1. インストール

Go 1.26以降を使う。macOSとLinuxに対応する。

```bash
git clone https://github.com/yukimaru77/codex-account-pool.git
cd codex-account-pool
scripts/install-local.sh
```

既定の配置先は以下。オプションで変更できる。

| オプション | 既定値 | 内容 |
| --- | --- | --- |
| `--prefix` | `~/.local/bin` | `codex-pool` とラッパー `codex` の置き場所 |
| `--pool-home` | `~/.codex-pool` | `pool.json` と `state/` |
| `--accounts-dir` | `~/.codex-accounts` | アカウントごとの `CODEX_HOME` |
| `--codex-bin` | PATH上の `codex` | ラッパーが最終的に実行する本物のCodex |

インストーラは次を行う。何度実行してもよい。

1. `go build` で `codex-pool` を `--prefix` に置く。
2. `pool.json` が無ければ `codex-pool init` で作る。既にあれば一切変更しない。
   `round_robin_endpoints` は書かず、組み込みの既定入口を使う。
3. ラッパー `--prefix/codex` を置く。同名の別ファイルがあれば
   `codex.pre-pool-日付` へ退避する。ラッパー自身は退避しない。
4. 常駐を登録する。macOSは `~/Library/LaunchAgents/com.local.codex-pool.plist`、
   Linuxは `~/.config/systemd/user/codex-pool.service`。
   既に動いていれば新しいバイナリで再起動する。

`--codex-bin` を省略すると、PATH上の `codex` のうちラッパー自身と
`codex-pool launch` を含むスクリプトを除いた最初のものを記録する。
本物のCodexの場所が決まっている場合は明示する。

```bash
scripts/install-local.sh --codex-bin ~/.local/bin/convenient-codex --prefix ~/bin
```

`--prefix` にあった既存の `codex` が退避された場合、`--codex-bin` にその中身が
実行している本体を指定する。退避先のファイルは戻すときまで残す。

**`--prefix` はPATH上で元の `codex` より前に置く。**
インストーラの最後に表示される `which -a codex` の先頭がラッパーであることを確認する。
シェルの `alias codex=...` や関数はPATHより優先されるため、ラッパーを通らない。
該当する定義があれば削除するか、ラッパーを呼ぶように変更する。

`CODEX_POOL_NO_SERVICE=1` を付けると常駐の登録・再起動をしない。
`serve` は手動で起動する。

```bash
codex-pool serve --config ~/.codex-pool/pool.json
```

## 2. アカウントの登録

今使っている `~/.codex` のアカウントを取り込み、他のアカウントは正規にログインする。

```bash
codex-pool account add main --config ~/.codex-pool/pool.json --from ~/.codex/auth.json
codex-pool account add sub1 --config ~/.codex-pool/pool.json
codex-pool account list --config ~/.codex-pool/pool.json
```

`--from` はファイルをコピーする。refresh token は使い捨てのため、
同じ token を持つファイルが2つあると、後から refresh した側が恒久的に使えなくなる。
そこで `--from ~/.codex/auth.json`（`codex_home` の `auth.json`）を取り込むと、
元の `~/.codex/auth.json` をアカウント側 `auth.json` へのsymlinkに置き換える。
バックアップは残さない（残すと同じ token の持ち主がもう1つ増える）。
それ以外のパスから取り込んだ場合は元ファイルに触れず、
`warning: <PATH> still holds the same refresh token; ...` を表示する。
元ファイルは以後使わないこと。
別の号池（Linuxの号池など）に登録済みのアカウントは、コピーせず
`account add NAME`（`--from` なし）で別途ログインすること。

`--from` の無い `account add` は、そのアカウント用の `CODEX_HOME` で
`codex login` を対話実行する。名前は英数字・`-`・`_`。
各アカウントの `auth.json` と `models_cache.json` は個別、
それ以外の `~/.codex` 直下（`config.toml`、sessions、skills など）はsymlinkで共有する。
`~/.codex` に新しいファイルが増えたら `account relink --all` で張り直す。

```bash
codex-pool account disable sub1 --config ~/.codex-pool/pool.json
codex-pool account enable sub1 --config ~/.codex-pool/pool.json
codex-pool account relink --all --config ~/.codex-pool/pool.json
```

無効化はアカウントのディレクトリに `disabled` を置くだけで、`auth.json` は変更しない。
`auth.json` はCodex本体とプールの両方が期限前に原子的に更新する。

## 3. アカウントの選び方

`codex` を実行するたびに、ラッパーが次の順でアカウントを選ぶ。

1. `--help` や `--version` だけの呼び出しは選択せずそのまま実行する。
2. 有効なアカウントの残量を `serve` の `/_pool/status` から取得する。
   `serve` が応答しなければ各アカウントを直接問い合わせる。
3. 週の残量が `reserve_percent` 以下のものを除き、週リセットが近い順のfill-firstで選ぶ。
   同じ条件なら名前順。
4. 選んだディレクトリを `CODEX_HOME` にして本物のCodexを実行する。
   stderrに `codex-pool: account=名前 remaining=残量%` を1行出す。

全アカウントが予備残量以下でも起動は止めず、残量が最も多いものを警告付きで使う。
固定したい場合は環境変数で指定する。

```bash
CODEX_POOL_ACCOUNT=sub1 codex
```

`kb NAME codex ...` も最終的に `codex` を実行するため、同じ選択を通る。

## 4. kb-repomapからの利用

シェルの設定ファイルに追加する。インストーラの最後にも表示される。

```bash
export KB_POOL_ORIGIN=http://127.0.0.1:18473
export KB_POOL_KEY_FILE="$HOME/.codex-pool/state/client.key"
```

`kb create` の圧縮や画像生成は `/_pool/rr/*` のround-robin入口、
`kb NAME --remote codex` はプールへ直接つなぐKB注入セッションになる。
どちらもループバックなので `private_http` は不要。

## 5. ブリッジからの移行

ローカルのプールで `codex exec 'hi'` と `kb` の利用を確認してから、
mitmdumpのブリッジを止める。

```bash
launchctl bootout gui/$(id -u)/com.local.codex-account-pool-bridge
```

`~/.zshrc` などの `KB_POOL_ORIGIN` をループバックへ書き換える。
`bridge.json` とブリッジ用の専用CAは不要になる。
CAを撤去する場合は [READMEのブリッジの節](../README.md#mac-の透過ブリッジ) の手順に従う。
Linuxのプールは別プロセスとして残る。ローカルのプールと同じアカウントを
両方で使う場合は、Linux側の認証ファイルをコピーせず、
ローカルで `account add NAME`（`--from` なし）により別途ログインすること
（コピーすると refresh token を奪い合い、片方が使えなくなる）。
残量は別々に数えるため予備残量の判断は各自で行う。

## 6. トラブルシューティング

```bash
codex-pool account list --config ~/.codex-pool/pool.json
codex-pool status --config ~/.codex-pool/pool.json
tail -f ~/.codex-pool/state/serve.log
```

- **ラッパーを通らない:** `which -a codex` の先頭と、`alias codex` の有無を確認する。
- **`account list` の残量が空:** `serve` が動いているか、`serve.log` を確認する。
- **アカウントが選ばれない:** 無効化・refresh失敗・予備残量以下のいずれかを `status` で確認する。
- **symlinkが壊れた:** `account relink --all` で作り直す。

常駐の停止・再開は以下。

```bash
# macOS
launchctl bootout gui/$(id -u)/com.local.codex-pool
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.local.codex-pool.plist
# Linux
systemctl --user stop codex-pool
systemctl --user start codex-pool
```

## Codex Appで失われるもの

Codex App（Codex.app・ChatGPT.app）はラッパーを通らず、providerも変更できない。
このため、Appは自分でログインした1アカウントだけで動き、
fill-firstの選択・残量による切替・KB注入の対象外になる。
ブリッジ構成でAppの通信を横取りしていた場合、移行後はその機能が無くなる。
