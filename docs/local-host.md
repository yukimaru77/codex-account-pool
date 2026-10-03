# 各PCでローカルにプールを動かす

`codex`（ラッパー） → 残量で選んだ `CODEX_HOME` → 素の Codex → OpenAI。
各アカウントは素の Codex で正規にログインした `auth.json` をそのまま使い、
対話セッションはそのアカウント本人として直接通信する。
プール（`codex-pool serve`）はPCごとに1つ、`127.0.0.1:18473` で動かし、
残量監視と `/_pool/rr/*` のround-robinを担当する。
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
| `--prefix` | `~/.local/bin` | `codex-pool` とラッパー `codex`・`pool-rr` の置き場所 |
| `--pool-home` | `~/.codex-pool` | `pool.json`・`rr.json` と `state/` |
| `--accounts-dir` | `~/.codex-accounts` | アカウントごとの `CODEX_HOME` |
| `--codex-bin` | PATH上の `codex` | ラッパーが最終的に実行する本物のCodex |

インストーラは次を行う。何度実行してもよい。

1. `go build` で `codex-pool` を `--prefix` に置く。
2. `pool.json` が無ければ `codex-pool init` で作る。既にあれば一切変更しない。
   `round_robin_endpoints` は書かず、組み込みの既定入口を使う。
3. `rr.json` が無ければ作る。内容は `base_url`（`pool.json` の `listen` に `/_pool/rr` を付けたもの）と
   `key_file`（`state/client.key` の絶対パス）の2項目だけ。既にあれば変更しない。
   `pool-rr`、`scripts/compact-jsonl.py` などのRRクライアントが読む。
4. ラッパー `--prefix/codex` を置く。同名の別ファイルがあれば
   `codex.pre-pool-日付` へ退避する。ラッパー自身は退避しない。
5. ラッパー `--prefix/pool-rr` を置く。中身はこのリポジトリの `scripts/pool-rr.py` を
   `--config ~/.codex-pool/rr.json` 付きで実行するだけなので、リポジトリは移動・削除しない。
   同名の別ファイル（`scripts/pool-rr.py` へのsymlink等）は `pool-rr.pre-pool-日付` へ退避する。
6. 常駐を登録する。macOSは `~/Library/LaunchAgents/com.local.codex-pool.plist`、
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
既に別の名前で登録済みのアカウントを追加しようとすると
`account already registered as <名前>` で拒否し、作りかけのディレクトリを消す。
別の号池（Linuxの号池など）に登録済みのアカウントは、コピーせず
`account add NAME`（`--from` なし）で別途ログインすること。

`--from` の無い `account add` は、そのアカウント用の `CODEX_HOME` で
`codex login` を対話実行する。名前は英数字・`-`・`_`。
各アカウントの `auth.json` と `models_cache.json` は個別、
それ以外の `~/.codex` 直下（`config.toml`、sessions、skills など）はsymlinkで共有する。
`~/.codex` に新しいファイルが増えたら `account relink --all` で張り直す。

ログインし直す（refresh tokenが失効した等）ときは `account login` を使う。
既存のディレクトリで `codex login` を実行し、結果の account_id と email を表示する。
以前と別のアカウントでログインした場合は、新しい `auth.json` を残したまま
エラー（終了コード1）で知らせる。

```bash
codex-pool account login sub1 --config ~/.codex-pool/pool.json
```

ラッパー経由の `codex login` / `codex logout` は、どのアカウントに効くか分からないため拒否する。
`codex-pool account login NAME` を使うか、`CODEX_HOME=<dir> <codex_bin> login` で直接実行する。

```bash
codex-pool account disable sub1 --config ~/.codex-pool/pool.json
codex-pool account enable sub1 --config ~/.codex-pool/pool.json
codex-pool account relink --all --config ~/.codex-pool/pool.json
```

無効化はアカウントのディレクトリに `disabled` を置くだけで、`auth.json` は変更しない。
Codex は auth.json をその場で上書きする（原子的ではない）。号池は読み取り中に壊れたファイルを掴んだ場合、短い待ちで数回再読込する。同時 refresh は Codex 側が更新前にディスクを再読込するため稀で、起きても 401 → 再読込で回復する。

## 3. 通常のCodex起動

インストールされた `codex` は通常のCodexバイナリへそのまま引き継ぐ。
号池は `CODEX_HOME`、残量、ログイン状態を変更しない。通常セッションの
アカウント切替はCodex側が担当する。

## 4. round-robin入口（RR）

`/_pool/rr/*` は要求ごとにアカウントを巡回する入口で、`client.key` のBearer認証が要る。
接続先とキーはインストーラが作る `~/.codex-pool/rr.json` にある。

```json
{"base_url": "http://127.0.0.1:18473/_pool/rr", "key_file": "/Users/me/.codex-pool/state/client.key"}
```

kb-repomap などのRRクライアントは、RRのbase URLを `http://127.0.0.1:18473/_pool/rr` に向けて使える。
号池はRRクライアントを起動・設定しない。

### pool-rr で1回の実行だけround-robinにする

```bash
pool-rr codex exec "このリポジトリを調べて"
```

`pool-rr` は `rr.json` の `base_url` と `key_file` を使い、その実行の Codex だけ
provider を `/_pool/rr` に向ける。各推論要求ごとにアカウントを巡回する。
`scripts/pool-rr.py` を直接実行した場合も、`~/.codex-pool/rr.json` を既定の設定として使う。
インストール前に設定が無ければエラーになります。

`pool-rr` はPATH上の通常の `codex` を実行し、推論要求だけを明示的なRR
providerへ向ける。通常の `codex` 起動は号池を経由しない。
モデル一覧は `$CODEX_HOME/models_cache.json`（既定 `~/.codex/models_cache.json`）を使うため、
先に通常の `codex` を一度起動しておく。

### RRの headers

`pool.json` の `round_robin_endpoints` の各入口に `headers` を書くと、
号池が上流へ送る際に毎回上書きする。書けるのは
`User-Agent`・`originator`・`Accept`・`Content-Type` だけで、他の名前は起動時にエラーになる。
`init` が作る `pool.json` は `round_robin_endpoints` 自体を書かず、組み込みの既定入口を使う。
既定入口に `headers` は無い。`round_robin_endpoints` を書くと既定入口を置き換えるため、
必要な場合は4つの入口を全て明示したうえで追加し、`serve` を再起動する。

```json
"round_robin_endpoints": {
  "/_pool/rr/responses": {"upstream_path": "/backend-api/codex/responses",
                          "headers": {"originator": "codex_exec"}}
}
```

値は実機のCodexの通信で観測したものを使う（上の値は例）。認証ヘッダーは書かない。

## 5. トラブルシューティング

```bash
codex-pool account list --config ~/.codex-pool/pool.json
codex-pool status --config ~/.codex-pool/pool.json
tail -f ~/.codex-pool/state/serve.log
```

- **ラッパーを通らない:** `which -a codex` の先頭と、`alias codex` の有無を確認する。
- **`launch re-entered itself; codex_bin points at the wrapper`:** `codex_bin` がラッパー自身に
  戻っている。`pool.json` の `codex_bin` を本物のCodexに直す。
  ラッパーは実行環境に `CODEX_POOL_LAUNCHED=<PID>` を設定し、同じPIDで再び起動されたら止まる。
  セッション内から入れ子で起動した `codex` は別プロセスなので止まらない。
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

## Codex Appについて

Codex App（Codex.app・ChatGPT.app）はラッパーを通らず、providerも変更できない。
このため、Appは自分でログインした1アカウントだけで動き、
fill-firstの選択・残量による切替の対象外になる。
ブリッジ構成でAppの通信を横取りしていた場合、移行後はその機能が無くなる。
