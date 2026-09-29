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
| `--prefix` | `~/.local/bin` | `codex-pool` とラッパー `codex`・`pool-rr` の置き場所 |
| `--pool-home` | `~/.codex-pool` | `pool.json`・`kb-pool.json` と `state/` |
| `--accounts-dir` | `~/.codex-accounts` | アカウントごとの `CODEX_HOME` |
| `--codex-bin` | PATH上の `codex` | ラッパーが最終的に実行する本物のCodex |

インストーラは次を行う。何度実行してもよい。

1. `go build` で `codex-pool` を `--prefix` に置く。
2. `pool.json` が無ければ `codex-pool init` で作る。既にあれば一切変更しない。
   `round_robin_endpoints` は書かず、組み込みの既定入口を使う。
3. `kb-pool.json` が無ければ作る。内容は `origin`（`pool.json` の `listen`）と
   `key_file`（`state/client.key` の絶対パス）の2項目だけ。既にあれば変更しない。
   `kb-repomap` の `--pool-config`、`pool-rr`、`scripts/compact-jsonl.py` が読む。
4. ラッパー `--prefix/codex` を置く。同名の別ファイルがあれば
   `codex.pre-pool-日付` へ退避する。ラッパー自身は退避しない。
5. ラッパー `--prefix/pool-rr` を置く。中身はこのリポジトリの `scripts/pool-rr.py` を
   `--config ~/.codex-pool/kb-pool.json` 付きで実行するだけなので、リポジトリは移動・削除しない。
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

## 3. アカウントの選び方

`codex` を実行するたびに、ラッパーが次の順でアカウントを選ぶ。

1. `CODEX_HOME` が既に設定されている場合、それが `accounts_dir` 直下のディレクトリなら
   そのアカウントに固定する（プールで起動したセッション内から入れ子で `codex` を
   実行しても同じアカウントを使う）。それ以外の場所なら選択せず、環境を変えずに本物のCodexを実行する。
   `--help` や `--version` だけの呼び出しも選択せずそのまま実行する。
2. 有効なアカウントの残量を `serve` の `/_pool/status` から取得する。
   `serve` が応答しなければ各アカウントを直接問い合わせる。
3. 週の残量が `reserve_percent` 以下のものを除き、週リセットが近い順のfill-firstで選ぶ。
   同じ条件なら名前順。
4. 選んだディレクトリを `CODEX_HOME` にして本物のCodexを実行する。
   stderrに `codex-pool: account=名前 remaining=残量%` を1行出す。

全アカウントが予備残量以下でも起動は止めず、残量が最も多いものを警告付きで使う。
固定したい場合は環境変数で指定する（`--account` > `CODEX_POOL_ACCOUNT` > 継承した `CODEX_HOME` の順に優先）。

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

`kb create` の圧縮には `~/.config/kb/config.json` の `build_args` で
インストーラが作った `kb-pool.json` を指定する。既存の `stores` は残す。
`~` は展開されるが、インストーラの表示どおり絶対パスで書いてもよい。

```json
{
  "stores": [],
  "build_args": ["--pool-config", "~/.codex-pool/kb-pool.json", "--workers", "12"]
}
```

`kb create` の圧縮や画像生成は `/_pool/rr/*` のround-robin入口、
`kb NAME --remote codex` はプールへ直接つなぐKB注入セッションになる。
どちらもループバックなので `private_http` は不要。

### pool-rr で1回の実行だけround-robinにする

```bash
pool-rr codex exec "このリポジトリを調べて"
pool-rr kb paper-demo --remote codex exec "この論文の要点を説明して"
pool-rr kb paper-demo --remote codex        # TUI
```

`pool-rr` は `kb-pool.json` の接続先と `client.key` を使い、その実行の Codex だけ
provider を `/_pool/rr` に向ける。各推論要求ごとにアカウントを巡回する。
`scripts/pool-rr.py` を直接実行した場合も、`~/.codex-pool/kb-pool.json` があれば
それを既定の設定として使う（無ければ従来どおりリポジトリの `bridge.json`）。

`pool-rr` はPATH上の `codex` を実行するため、ラッパー経由になる。
このためラッパーの `codex-pool: account=名前 remaining=残量%` は表示されるが、
推論はRRのproviderを通るので、実際に使うアカウントは要求ごとに号池が選ぶ。
表示されたアカウントは起動時の `CODEX_HOME` の選択にすぎない。
モデル一覧は `$CODEX_HOME/models_cache.json`（既定 `~/.codex/models_cache.json`）を使うため、
先に通常の `codex` を一度起動しておく。

### RRで使うアカウントを固定する（X-Pool-Account）

`/_pool/rr/*` への要求に `X-Pool-Account: <auth_index>` を付けると、
そのアカウントだけを使う（通常RRの順番は進めない）。
値は32桁16進の完全な `auth_index` で、メールアドレスや名前ではない。
名前から調べるには `account list` の `AUTH_INDEX` 列を見る（管理キー不要）。
`codex-pool status` の出力（`/_pool/status` と同じJSON）の `auth_index` でもよい。

```bash
codex-pool account list --config ~/.codex-pool/pool.json
codex-pool status --config ~/.codex-pool/pool.json
```

指定したアカウントが不明・無効・利用枠不足・クールダウン中なら503を返し、
別アカウントへ切り替えない。空値や複数指定は400。
このヘッダーは上流へ送らず、通常のCodex用URLでは無視する。

### 画像RRの形式

`/_pool/rr/images/generations` と `/_pool/rr/images/edits` はJSONだけを受け付ける。

```json
{"prompt": "画像の指示"}
{"prompt": "編集指示", "images": [{"image_url": "data:image/png;base64,..."}]}
```

上が生成、下が編集。編集の `images` は1〜5枚。
model・quality・size・background・n などは号池が固定する（`gpt-image-2`、auto）ため、
指定すると400になる。multipartはRR入口では400になる。
上流の編集APIもmultipartには400 `Unsupported content type` を返すため、
画像は `data:` URLにしてJSONで送る。

### RRの headers

`pool.json` の `round_robin_endpoints` の各入口に `headers` を書くと、
号池が上流へ送る際に毎回上書きする。書けるのは
`User-Agent`・`originator`・`Accept`・`Content-Type` だけで、他の名前は起動時にエラーになる。
`init` が作る `pool.json` は `round_robin_endpoints` 自体を書かず、組み込みの既定入口を使う。
既定入口に `headers` は無い。`round_robin_endpoints` を書くと既定入口を置き換えるため、
必要な場合は4つの入口を全て明示したうえで追加し、`serve` を再起動する。

```json
"round_robin_endpoints": {
  "/_pool/rr/images/generations": {"upstream_path": "/backend-api/codex/images/generations"},
  "/_pool/rr/images/edits": {"upstream_path": "/backend-api/codex/images/edits"},
  "/_pool/rr/responses/compact": {"upstream_path": "/backend-api/codex/responses/compact"},
  "/_pool/rr/responses": {"upstream_path": "/backend-api/codex/responses",
                          "headers": {"originator": "codex_exec"}}
}
```

値は実機のCodexの通信で観測したものを使う（上の値は例）。認証ヘッダーは書かない。

### kb decrypt の消費

`kb decrypt NAME` はblobの数をNとすると、各波で high と max を N 本ずつ、
合計 2N 本を並列に投げる。波は最大4回（1blobあたり最大8回）。
全てプールのアカウントの週の利用枠を使うため、blobが多いKBでは消費が大きい。
実行前に `account list` で残量を確認する。

### Claude Code から使う

`kb NAME claude` は復号済みの平文（`dev.txt` と選択済みの `raw.txt`）を
システムプロンプトとして渡し、ローカルのClaude Codeセッションを起動する。
号池は使わない。先に `kb decrypt NAME` が必要。
`--remote` はCodex専用で、Claude Codeでは使えない。
号池はOpenAI向けの通信にしかKBを注入できず、Anthropicの通信には介在できないため。

## 5. ブリッジからの移行

ローカルのプールで `codex exec 'hi'` と `kb` の利用を確認してから、
mitmdumpのブリッジを止める。

```bash
launchctl bootout gui/$(id -u)/com.local.codex-account-pool-bridge
```

`~/.zshrc` などの `KB_POOL_ORIGIN` をループバックへ書き換える。
`~/.config/kb/config.json` の `build_args` の `--pool-config` を
`~/.codex-pool/kb-pool.json` に変える。`bridge.json` はコピーしない。
`pool-rr` はインストーラが置いたものを使う（以前のsymlinkは退避される）。
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

## Codex Appで失われるもの

Codex App（Codex.app・ChatGPT.app）はラッパーを通らず、providerも変更できない。
このため、Appは自分でログインした1アカウントだけで動き、
fill-firstの選択・残量による切替・KB注入の対象外になる。
ブリッジ構成でAppの通信を横取りしていた場合、移行後はその機能が無くなる。
