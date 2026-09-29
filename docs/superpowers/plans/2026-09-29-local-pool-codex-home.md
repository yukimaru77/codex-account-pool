# Local Pool (CODEX_HOME account dirs) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Run the account pool on each local PC, reading genuine Codex `auth.json` files from per-account `CODEX_HOME` directories, with a `codex` launcher that picks the account by remaining quota; keep RR / compaction / image / KB-injection behaviour unchanged and drop the mitmdump bridge.

**Architecture:** `internal/pool` gains an `AccountStore` interface with two implementations: the existing `state/accounts` CPA store (legacy) and a new `codexHomeStore` that treats `accounts_dir/<name>/auth.json` (Codex format) as the credential, refreshing under a per-directory flock and writing back atomically in Codex format. `cmd/codex-pool` gains `account` (add/list/enable/disable/relink) and `launch` (fill-first selection → `CODEX_HOME=<dir>` → exec real codex) subcommands, plus a local installer that sets up the service and the `codex` wrapper.

**Tech Stack:** Go 1.26 (stdlib, `golang.org/x/sys/unix` flock, existing `internal/cpa` JWT/OAuth), POSIX sh for installer/wrapper, launchd (macOS) / systemd --user (Linux).

**Spec:** `docs/superpowers/specs/2026-09-29-local-pool-codex-home-design.md`

## Global Constraints

- Go 1.26; no new third-party dependencies.
- All existing tests must keep passing with the legacy store: `go test -race ./...`.
- Never write pool-owned state (disabled flag, locks) into `auth.json`; only `tokens.*` and `last_refresh` may be rewritten there, all other fields preserved byte-for-byte in meaning (re-marshal is fine, values must not change).
- All file writes use `AtomicWrite` (temp + fsync + rename).
- Account internal id stays `sha256(account_id)[:16]` hex (`Credential.ID()`); scheduler / usage.jsonl keys are unchanged.
- Directory names: `^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`.
- Default round-robin routes (images/generations, images/edits, responses/compact, responses) must apply when `round_robin_endpoints` is omitted; the installer must omit it.
- Codex App transparent interception is out of scope; nothing in `bridge/` is modified.
- Commit after each task; commit messages in English imperative, ending with `Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>`.

## Review Focus

1. `auth.json` written by Codex while the pool holds it open: the pool must reread under lock before refreshing and must not clobber a newer refresh token (Task 4 test `TestCodexHomeTokenRereadsBeforeRefresh`).
2. `auth.json` with `auth_mode` != `chatgpt` or missing `tokens` (API-key mode): the account must be skipped with a status error, not crash `List` (Task 3 test `TestCodexHomeListSkipsBrokenAuth`).
3. Two `auth.json` directories holding the same `account_id` (user logged the same account in twice): `List` must report a duplicate error for the second and keep the first (Task 3 test `TestCodexHomeListRejectsDuplicateAccount`).
4. Launcher when the pool is down and every probe fails: must still exec codex with a warning and a deterministic account (Task 7 test `TestLaunchFallsBackWhenNoQuota`).
5. Round-robin route `headers` must never leak onto fill-first routes and must override client-sent values of the same name (Task 2 tests).

---

### Task 1: `AccountStore` interface and `Credential.Name`

**Files:**
- Modify: `internal/pool/store.go` (add interface, `Name` field)
- Modify: `internal/pool/relay.go:57-70` (`Handler.Store` type, `NewHandler` param)
- Modify: `internal/pool/scheduler.go:18-25,46-64` (`AccountStatus.Name`, `Sync` copies it)
- Modify: `internal/pool/poll.go` (no code change expected; compile check)
- Test: `internal/pool/scheduler_test.go`

**Interfaces:**
- Produces:
  ```go
  type AccountStore interface {
      List() ([]Credential, error)
      Token(ctx context.Context, id string, force bool) (Credential, error)
      RefreshRejected(ctx context.Context, id, accessToken string) (Credential, error)
      SetDisabled(id string, disabled bool) error
  }
  // Credential gains: Name string `json:"name,omitempty"`
  // AccountStatus gains: Name string `json:"name,omitempty"`
  func NewHandler(cfg Config, store AccountStore, transport http.RoundTripper, clientKey, adminKey string) *Handler
  ```

- [ ] **Step 1: Write the failing test** in `internal/pool/scheduler_test.go`:

```go
func TestSyncCopiesAccountName(t *testing.T) {
	s := NewScheduler(DefaultConfig())
	c := Credential{Name: "alice"}
	c.AccountID = "acct-a"
	c.Email = "a@example.com"
	s.Sync([]Credential{c})
	st := s.Status()
	if len(st) != 1 || st[0].Name != "alice" {
		t.Fatalf("status %+v", st)
	}
}
```

- [ ] **Step 2: Run** `go test ./internal/pool -run TestSyncCopiesAccountName` → FAIL (unknown field `Name`).

- [ ] **Step 3: Implement.** In `store.go` add the `AccountStore` interface (above) after `type Store struct`, add `Name string \`json:"name,omitempty"\`` to `Credential`, and add `var _ AccountStore = (*Store)(nil)`. In `scheduler.go` add `Name string \`json:"name,omitempty"\`` to `AccountStatus` and `a.Name = c.Name` in `Sync`. In `relay.go` change `Store *Store` → `Store AccountStore` and the `NewHandler` parameter type.

- [ ] **Step 4: Run** `go build ./... && go test -race ./...` → PASS.

- [ ] **Step 5: Commit** `git commit -am "Introduce AccountStore interface and account names"`.

---

### Task 2: Round-robin route `headers`

**Files:**
- Modify: `internal/pool/config.go:13-15` (`Route.Headers`)
- Modify: `internal/pool/relay.go` (apply headers in `Rewrite`)
- Test: `internal/pool/relay_test.go`, `internal/pool/config_test.go`

**Interfaces:**
- Produces: `type Route struct { Path string \`json:"upstream_path"\`; Headers map[string]string \`json:"headers,omitempty"\` }`

- [ ] **Step 1: Write failing tests.** Look at an existing relay test that spins up a fake upstream (`relay_test.go`) and copy its harness. Add:

```go
func TestRoundRobinRouteAddsConfiguredHeaders(t *testing.T) {
	// harness: fake upstream records r.Header; cfg.RoundRobin["/_pool/rr/responses"] =
	// Route{Path: "/backend-api/codex/responses", Headers: map[string]string{"Originator": "kb-pool", "User-Agent": "pool-ua"}}
	// client sends User-Agent: client-ua to /_pool/rr/responses
	// assert upstream saw Originator=kb-pool and User-Agent=pool-ua
}
func TestFillFirstRouteDoesNotAddRouteHeaders(t *testing.T) {
	// same cfg; client posts to /backend-api/codex/responses directly
	// assert upstream Originator header is empty
}
```

Add to `config_test.go` a case that `LoadConfig` rejects a header name containing a colon or empty name (`fmt.Errorf("invalid header name in route %q", endpoint)`).

- [ ] **Step 2: Run** the three tests → FAIL.

- [ ] **Step 3: Implement.** In `relay.go` `ServeHTTP`, when the RR route matches keep the `route` value: `var routeHeaders map[string]string; if route, ok := ...; ok { ...; routeHeaders = route.Headers }`. In `Rewrite`, after `stripIdentity(pr.Out.Header)`: `for k, v := range routeHeaders { pr.Out.Header.Set(k, v) }` (before Authorization is set, so identity headers still win). In `LoadConfig` validate each header key with `http.CanonicalHeaderKey(k) != "" && !strings.ContainsAny(k, ": \t")`.

- [ ] **Step 4: Run** `go test -race ./internal/pool` → PASS.

- [ ] **Step 5: Commit** `"Add optional upstream headers to round-robin routes"`.

---

### Task 3: `codexHomeStore` – reading accounts

**Files:**
- Create: `internal/pool/codex_home_store.go`
- Create: `internal/pool/codex_home_store_test.go`

**Interfaces:**
- Consumes: `Credential`, `AccountStore`, `cpa.ParseJWTToken(token) (*cpa.JWTClaims, error)` (`Exp int`, `Email`, `GetAccountID()`), `AtomicWrite`, `withLock(path, fn)` (already in `store.go`).
- Produces:
  ```go
  type codexAuthFile struct {
      AuthMode    string          `json:"auth_mode"`
      APIKey      json.RawMessage `json:"OPENAI_API_KEY"`
      Tokens      *codexTokens    `json:"tokens"`
      LastRefresh string          `json:"last_refresh,omitempty"`
      Extra       map[string]json.RawMessage `json:"-"` // preserved unknown fields
  }
  type codexTokens struct{ IDToken, AccessToken, RefreshToken, AccountID string } // json tags id_token, access_token, refresh_token, account_id
  type CodexHomeStore struct {
      Dir     string // accounts_dir
      Refresh func(context.Context, string) (*cpa.CodexTokenData, error)
      Logf    func(string, ...any)
  }
  func OpenCodexHomeStore(dir string, refresh func(context.Context, string) (*cpa.CodexTokenData, error)) (*CodexHomeStore, error)
  var ErrDuplicateAccount = errors.New("account already present in another directory")
  func ValidAccountName(name string) bool
  func (s *CodexHomeStore) Names() ([]string, error)           // sorted dir names with auth.json
  func (s *CodexHomeStore) Path(name string) string           // Dir/name
  func (s *CodexHomeStore) List() ([]Credential, error)       // AccountStore
  func (s *CodexHomeStore) SetDisabled(id string, disabled bool) error
  func (s *CodexHomeStore) Errors() map[string]string          // name -> last read error (for status/list)
  func readCodexAuth(path string) (codexAuthFile, Credential, error)
  ```
  `List` maps: `Name`=dir name, `AccountID`=`tokens.account_id` or id_token claim, `Email`=id_token claim, `Expire`=`time.Unix(accessClaims.Exp,0).UTC().Format(time.RFC3339)`, `Type="codex"`, `Disabled`=exists(`<dir>/disabled`), `LastRefresh` copied. Unknown top-level JSON fields must round-trip: unmarshal into `map[string]json.RawMessage`, pull known keys, keep the rest in `Extra`; marshal by merging.

- [ ] **Step 1: Write failing tests** (`codex_home_store_test.go`). Add a helper `writeAuth(t, dir, name, accountID, email, exp time.Time, refresh string)` that builds unsigned JWTs (`header.payload.sig` with base64url JSON `{"exp":..., "email":..., "https://api.openai.com/auth":{"chatgpt_account_id":accountID}}`) and writes `dir/name/auth.json` in Codex format with an extra field `"custom":{"x":1}`.

```go
func TestCodexHomeListReadsCodexAuth(t *testing.T)          // Name, Email, AccountID, Expire, ID() stable, Disabled false
func TestCodexHomeListHonoursDisabledMarker(t *testing.T)   // touch dir/bob/disabled → Disabled true
func TestCodexHomeListSkipsBrokenAuth(t *testing.T)         // dir/broken/auth.json = "{" ; dir/apikey/auth.json = {"auth_mode":"apikey","OPENAI_API_KEY":"sk"} → both absent from List, present in Errors()
func TestCodexHomeListRejectsDuplicateAccount(t *testing.T) // two dirs same account_id → first kept, Errors()[second] contains ErrDuplicateAccount.Error()
func TestCodexHomeListIgnoresDirsWithoutAuth(t *testing.T)  // dir/empty/ → not listed, no error
func TestValidAccountName(t *testing.T)                     // "alice", "a-b_1" ok; "", ".x", "a/b", 65 chars → false
func TestCodexHomeSetDisabledCreatesAndRemovesMarker(t *testing.T)
```

- [ ] **Step 2: Run** `go test ./internal/pool -run CodexHome` → FAIL (undefined).

- [ ] **Step 3: Implement** `codex_home_store.go` per the interface. `List` iterates `os.ReadDir(Dir)` sorted, skips non-dirs and dirs without `auth.json`, records per-name errors in a mutex-guarded map returned by `Errors()` (reset at the start of each `List`). Duplicate detection: keep a `seen map[accountID]name`. `SetDisabled(id)` resolves id → name via a fresh `List()` (ids are hashes), then `os.WriteFile(dir/disabled, nil, 0600)` or `os.Remove`.

- [ ] **Step 4: Run** tests → PASS. `go vet ./...` clean.

- [ ] **Step 5: Commit** `"Add Codex-home account store reading genuine auth.json"`.

---

### Task 4: `codexHomeStore` – Token / RefreshRejected with write-back

**Files:**
- Modify: `internal/pool/codex_home_store.go`
- Test: `internal/pool/codex_home_store_test.go`

**Interfaces:**
- Produces: `Token`, `RefreshRejected` on `*CodexHomeStore` satisfying `AccountStore`; `var _ AccountStore = (*CodexHomeStore)(nil)`.
- Behaviour (mirror `Store.token` in `store.go:213-262`): lock `<dir>/.pool.lock` via `withLock`; reread `auth.json`; if `rejected != ""` and current access token != rejected → return current without refresh; refresh when `force` or `exp - now < 1 minute`; on refresh result, reject if returned account id (from new id_token claim or `AccountID`) is non-empty and differs; keep old refresh token if empty; write back Codex format with `tokens.*` updated, `last_refresh = now RFC3339Nano UTC`, other fields preserved; return the new `Credential`.

- [ ] **Step 1: Write failing tests**

```go
func TestCodexHomeTokenNoRefreshWhenValid(t *testing.T)          // exp = +1h → refresh func not called, file unchanged (compare bytes)
func TestCodexHomeTokenRefreshesNearExpiryAndWritesBack(t *testing.T) // exp = +30s → refreshed; reread file: tokens updated, last_refresh changed, "custom" field preserved, auth_mode preserved
func TestCodexHomeTokenRereadsBeforeRefresh(t *testing.T)         // refresh func rewrites auth.json to "rotated" tokens with exp +1h *before* returning (simulating Codex); store must have reread under lock: second Token call returns rotated, refresh called exactly once
func TestCodexHomeRefreshRejectedSkipsWhenTokenChanged(t *testing.T) // RefreshRejected(id, "old") when file already has "newer" → no refresh call, returns newer
func TestCodexHomeRefreshRejectsIdentityChange(t *testing.T)     // refresh returns id_token for other account → error, file unchanged
func TestCodexHomeTokenConcurrentRefreshOnce(t *testing.T)       // 20 goroutines RefreshRejected same old token across two store instances → refresh called once (copy pattern from store_test.go TestRejectedTokenRefreshReusesConcurrentRotationAcrossStores)
```

- [ ] **Step 2: Run** → FAIL.

- [ ] **Step 3: Implement** `token(ctx, id, force, rejected)` + the two exported methods. Resolve id → name by scanning `Names()` and matching `Credential.ID()` (cache `map[id]name` refreshed on miss). Write-back via `AtomicWrite(dir/auth.json, marshal(file))` where marshal merges `Extra` and known keys with `json.MarshalIndent(..., "", "  ")`.

- [ ] **Step 4: Run** `go test -race ./internal/pool` → PASS.

- [ ] **Step 5: Commit** `"Refresh Codex-home credentials under lock and write back auth.json"`.

---

### Task 5: Config fields and store selection in `main`

**Files:**
- Modify: `internal/pool/config.go` (fields + validation)
- Modify: `cmd/codex-pool/main.go:36-74` (store selection, guard `login`/`import`)
- Modify: `cmd/codex-pool/main.go:203-245` (`initialize` accepts optional flags)
- Test: `internal/pool/config_test.go`, `cmd/codex-pool/main_test.go`

**Interfaces:**
- Produces on `Config`:
  ```go
  AccountsDir string `json:"accounts_dir,omitempty"` // when set → CodexHomeStore; "~/" expanded; made absolute relative to config dir
  CodexHome   string `json:"codex_home,omitempty"`   // default "~/.codex" (expanded) when AccountsDir set
  CodexBin    string `json:"codex_bin,omitempty"`    // real codex executable for launch / account add
  func (c Config) UsesCodexHome() bool { return c.AccountsDir != "" }
  ```
  In `main.go`: `var store pool.AccountStore`; if `cfg.UsesCodexHome()` → `pool.OpenCodexHomeStore(cfg.AccountsDir, refresh)`, else `pool.OpenStore(cfg.StateDir, refresh)`. `login` and `import` return `fmt.Errorf("this pool reads Codex account directories; use: codex-pool account add NAME")` when `UsesCodexHome()`. `enable|disable` accept either an id or a directory name when `UsesCodexHome()` (resolve name → id via `List()`).
  `initialize(configPath, out, opts initOptions)` where `initOptions{AccountsDir, CodexHome, CodexBin, Listen string}`; `init` parses flags `--accounts-dir --codex-home --codex-bin --listen` and writes them into the generated JSON (omit empty). The generated JSON must not contain `round_robin_endpoints`.

- [ ] **Step 1: Write failing tests.** `config_test.go`: `TestLoadConfigExpandsAccountsDir` (`~/x` → `$HOME/x`; relative → relative to config dir; `codex_home` defaults to `$HOME/.codex`). `main_test.go`: `TestInitWritesAccountsDirAndOmitsRoutes` (run `main` with `init --config p --accounts-dir d`, read file, assert keys), `TestLoginRefusedWithAccountsDir`.

- [ ] **Step 2: Run** → FAIL.

- [ ] **Step 3: Implement.** Add a small `expandHome(p string) string` in `config.go` (replace leading `~/` with `os.UserHomeDir()`). Validation: if `AccountsDir` set, `CodexHome` defaults; `CodexBin` if set must be absolute or bare name (no validation of existence at load).

- [ ] **Step 4: Run** `go test -race ./...` → PASS.

- [ ] **Step 5: Commit** `"Select the Codex-home store from accounts_dir and extend init"`.

---

### Task 6: `codex-pool account` subcommands

**Files:**
- Create: `cmd/codex-pool/account.go`
- Create: `cmd/codex-pool/account_test.go`
- Modify: `cmd/codex-pool/main.go` (dispatch `account`, usage string)
- Modify: `internal/pool/codex_home_store.go` (add `LinkShared`)

**Interfaces:**
- Consumes: `*pool.CodexHomeStore` (`Names`, `Path`, `List`, `Errors`, `SetDisabled`), `Config.CodexHome`, `Config.CodexBin`.
- Produces:
  ```go
  // internal/pool/codex_home_store.go
  var SharedExclude = map[string]bool{"auth.json": true, "models_cache.json": true, "log": true, "tmp": true}
  // LinkShared symlinks every top-level entry of codexHome (except SharedExclude and names starting with ".write-")
  // into dir, replacing existing symlinks, never touching regular files/dirs already in dir. Returns linked names.
  func LinkShared(dir, codexHome string) ([]string, error)

  // cmd/codex-pool/account.go
  func runAccount(ctx context.Context, cfg pool.Config, store *pool.CodexHomeStore, statusFn func(context.Context) ([]pool.AccountStatus, error), execLogin func(dir string) error, args []string, out io.Writer) error
  ```
  Subcommands:
  - `add NAME [--from PATH]`: `ValidAccountName`; dir must not exist; `MkdirAll 0700`; `LinkShared`; if `--from`: copy file (0600) then `readCodexAuth` to validate; else `execLogin(dir)` (default impl: `exec.CommandContext(ctx, cfg.CodexBin, "login")` with `Env = append(os.Environ(), "CODEX_HOME="+dir)`, stdio inherited); afterwards print `added NAME email=<e> account_id=<id>`. On any failure after mkdir, remove the dir only if `auth.json` was never written.
  - `list`: table `NAME  EMAIL  REMAINING%  RESET  STATE` where quota comes from `statusFn` (default: HTTP `/_pool/status` like the `status` command; on error fall back to `h.Poll` + `h.Scheduler.Status()`); STATE is `disabled`, `error: <msg>`, or `ok`; also print `Errors()` entries as `NAME  -  -  -  error: <msg>`.
  - `enable NAME` / `disable NAME`: resolve via `List`, call `SetDisabled`.
  - `relink NAME|--all`: `LinkShared` again, print linked count.

- [ ] **Step 1: Write failing tests** (`account_test.go`, all with `t.TempDir()` as codex_home containing `config.toml`, `sessions/`, `auth.json`, `models_cache.json`, `log/`):

```go
func TestLinkSharedSkipsExcludedEntries(t *testing.T)     // links config.toml, sessions; not auth.json/models_cache.json/log
func TestAccountAddFromCopiesAuthAndLinks(t *testing.T)   // --from → dir/auth.json 0600, symlinks present, output has email
func TestAccountAddRejectsExistingName(t *testing.T)
func TestAccountAddRunsLoginInDir(t *testing.T)           // execLogin stub records dir, writes a valid auth.json → success
func TestAccountAddCleansUpWhenLoginFails(t *testing.T)   // stub returns error without writing → dir removed
func TestAccountListShowsQuotaAndErrors(t *testing.T)     // statusFn stub returns 2 accounts with quota; broken dir shows error row
func TestAccountDisableEnableByName(t *testing.T)
```

- [ ] **Step 2: Run** → FAIL.

- [ ] **Step 3: Implement** as specified. In `main.go` add `case "account":` before key reading (it needs `cfg`, `store` asserted to `*pool.CodexHomeStore`; error if legacy). Update usage string to `{init|account|launch|serve|status|probe|enable|disable|login|import}`.

- [ ] **Step 4: Run** `go test -race ./...` → PASS.

- [ ] **Step 5: Commit** `"Add account add/list/enable/disable/relink for Codex-home directories"`.

---

### Task 7: `codex-pool launch`

**Files:**
- Create: `cmd/codex-pool/launch.go`
- Create: `cmd/codex-pool/launch_test.go`
- Modify: `cmd/codex-pool/main.go` (dispatch `launch`; must run before `--config` flag parsing consumes `--`)
- Modify: `internal/pool/scheduler.go` (export a pure selection helper)

**Interfaces:**
- Produces:
  ```go
  // internal/pool/scheduler.go
  // PickFillFirst returns the usable account with the earliest weekly reset (ties by Name then ID).
  // ok=false when none usable; then best is the account with the lowest Weekly.Used that is not disabled (ties by Name), or "" if none.
  func PickFillFirst(accounts []AccountStatus, now time.Time, maxAge time.Duration, reserve float64) (chosen string, ok bool)

  // cmd/codex-pool/launch.go
  type launchDeps struct {
      status func(context.Context) ([]pool.AccountStatus, error) // /_pool/status then probe fallback
      exec   func(bin string, args []string, env []string) error // default syscall.Exec
      stderr io.Writer
  }
  func runLaunch(ctx context.Context, cfg pool.Config, store *pool.CodexHomeStore, deps launchDeps, args []string) error
  ```
  Behaviour:
  - Parse `launch` args: optional `--account NAME`, `--quiet`, then `--` and codex args. `CODEX_POOL_ACCOUNT` env is the same as `--account`.
  - If codex args contain only `--help`/`-h`/`--version`/`-V` (before any `--`), exec `cfg.CodexBin` immediately with unchanged env.
  - Fetch statuses; map `AccountStatus.Name` → dir via `store.Path`. If a status has empty `Name` (legacy pool), fall back to matching `ID` against `store.List()`.
  - Selection: pinned name if given (error if disabled/unknown). Otherwise `PickFillFirst`; if `!ok` and `best != ""` warn `codex-pool: no account has quota above reserve; using <name>`; if statuses empty or fetch failed, warn and choose the first enabled name alphabetically.
  - Env: `os.Environ()` with any existing `CODEX_HOME` removed, plus `CODEX_HOME=<dir>`. Unless `--quiet`, write `codex-pool: account=<name> remaining=<100-used>%` to stderr (`remaining=?` when unknown).
  - `exec(cfg.CodexBin, append([]string{cfg.CodexBin}, codexArgs...), env)`. `CodexBin` empty → error `codex_bin is not configured`.

- [ ] **Step 1: Write failing tests**

```go
func TestPickFillFirstPrefersEarliestResetAboveReserve(t *testing.T)
func TestPickFillFirstFallsBackToLeastUsed(t *testing.T)   // all below reserve → ok=false, best=least used
func TestLaunchSetsCodexHomeAndExecs(t *testing.T)        // status stub → exec stub records bin/args/env; CODEX_HOME=dir/alice; stderr line present
func TestLaunchPinnedAccount(t *testing.T)                // --account bob; disabled bob → error
func TestLaunchHelpBypassesSelection(t *testing.T)        // args ["--version"] → exec without CODEX_HOME change, status not called
func TestLaunchFallsBackWhenNoQuota(t *testing.T)         // status returns error → warning, first enabled name chosen, exec still called
func TestLaunchQuietSuppressesBanner(t *testing.T)
```

- [ ] **Step 2: Run** → FAIL.

- [ ] **Step 3: Implement.** In `main.go`, before `flags.Parse`, special-case `command == "launch"`: split `args[1:]` at the first `--`; parse `--config/--account/--quiet` from the left part with a `FlagSet`; the right part is codex args. Build `cfg`, `store` (must be `*pool.CodexHomeStore`), deps with real `status` (reuse the `status` command's HTTP code refactored into `fetchStatus(ctx, cfg, admin)`; on error build `pool.NewHandler` and `Poll` with a 10 s context) and `exec` = `syscall.Exec`.

- [ ] **Step 4: Run** `go test -race ./...` → PASS.

- [ ] **Step 5: Commit** `"Add launch command choosing CODEX_HOME by remaining quota"`.

---

### Task 8: Installer, service templates, wrapper, docs

**Files:**
- Create: `scripts/install-local.sh`
- Create: `deploy/com.local.codex-pool.plist.tmpl`
- Create: `deploy/codex-pool.service.tmpl`
- Create: `docs/local-host.md`
- Modify: `README.md` (short section pointing to `docs/local-host.md`; mark bridge as legacy)
- Test: `scripts/test_install_local.py` (unittest, runs the script with `HOME` pointed at a temp dir and `CODEX_POOL_NO_SERVICE=1`)

**Interfaces:**
- `scripts/install-local.sh [--codex-bin PATH] [--accounts-dir DIR] [--prefix ~/.local/bin] [--pool-home ~/.codex-pool]`
  1. `go build -o $PREFIX/codex-pool ./cmd/codex-pool` (skip when `CODEX_POOL_BIN` env points to a prebuilt binary; the test uses this).
  2. Resolve codex bin: `--codex-bin`, else first `codex` on PATH whose realpath is not `$PREFIX/codex` and whose content does not contain `codex-pool launch`; error if none.
  3. `mkdir -p $POOL_HOME`; if `$POOL_HOME/pool.json` missing: `codex-pool init --config $POOL_HOME/pool.json --accounts-dir $ACCOUNTS_DIR --codex-home $HOME/.codex --codex-bin $CODEX_BIN --listen 127.0.0.1:18473`.
  4. Write wrapper `$PREFIX/codex` (0755):
     ```sh
     #!/bin/sh
     # codex-pool launch: picks CODEX_HOME by remaining quota.
     exec "<PREFIX>/codex-pool" launch --config "<POOL_HOME>/pool.json" -- "$@"
     ```
     If `$PREFIX/codex` exists and is not already the wrapper, move it to `$PREFIX/codex.pre-pool-<date>`.
  5. Service unless `CODEX_POOL_NO_SERVICE=1`: macOS → render plist template (`ProgramArguments: codex-pool serve --config …`, `KeepAlive`, `RunAtLoad`, stdout/err `$POOL_HOME/state/serve.log`) to `~/Library/LaunchAgents/com.local.codex-pool.plist`, `launchctl bootout` if loaded, `launchctl bootstrap gui/$(id -u)`; Linux → render unit to `~/.config/systemd/user/codex-pool.service`, `systemctl --user daemon-reload && systemctl --user enable --now codex-pool`.
  6. Print next steps: `codex-pool account add main --from ~/.codex/auth.json`, `export KB_POOL_ORIGIN=http://127.0.0.1:18473`, `export KB_POOL_KEY_FILE=$POOL_HOME/state/client.key`, and a warning that `$PREFIX` must precede the old codex location in PATH (print `which -a codex`).
- `docs/local-host.md` (Japanese, same style as `docs/linux-host.md`): purpose, install, account add/list, how launch selects, kb-repomap env, migration from the bridge (`launchctl bootout gui/$(id -u)/com.local.codex-account-pool-bridge`), troubleshooting (`codex-pool account list`, `codex-pool status`, `serve.log`), what Codex App loses.

- [ ] **Step 1: Write failing test** `scripts/test_install_local.py`: build `codex-pool` once into a temp dir; run the script with `HOME=tmp`, `PATH` containing a fake `codex` script at `tmp/oldbin/codex`, `CODEX_POOL_BIN`, `CODEX_POOL_NO_SERVICE=1`, `--prefix tmp/bin`; assert `tmp/bin/codex` is the wrapper, `tmp/.codex-pool/pool.json` has `accounts_dir`, `codex_bin == tmp/oldbin/codex`, no `round_robin_endpoints`; run again → idempotent (no `.pre-pool-*` created for the wrapper itself).

- [ ] **Step 2: Run** `python3 -m unittest scripts.test_install_local -v` → FAIL.

- [ ] **Step 3: Implement** script (`set -eu`, POSIX sh, `uname` switch), templates, docs.

- [ ] **Step 4: Run** the unittest → PASS; `sh -n scripts/install-local.sh`.

- [ ] **Step 5: Commit** `"Add local installer, service templates and local-host docs"`.

---

### Task 9: Manual E2E migration on this Mac (performed by the coordinating session, not a subagent)

- [ ] Run `scripts/install-local.sh --codex-bin ~/.local/bin/convenient-codex --prefix ~/bin` (note `~/bin/codex` is currently a shell script exec'ing the launcher; the installer will move it to `codex.pre-pool-<date>`).
- [ ] `codex-pool account add main --from ~/.codex/auth.json`; `codex-pool account list` shows quota.
- [ ] `codex exec --skip-git-repo-check 'Reply with exactly: pool-ok'` prints the banner and `pool-ok`.
- [ ] `export KB_POOL_ORIGIN=http://127.0.0.1:18473 KB_POOL_KEY_FILE=~/.codex-pool/state/client.key`; `kb list`; `kb octane --rebuild never codex exec ... 'kb-ok'`; a compaction through `/_pool/rr/responses` succeeds (`kb create` on a small KB or `scripts/compact-jsonl.py`).
- [ ] `kb octane --remote codex exec ...` binds through `/_pool/kb/bind` on the local pool.
- [ ] `launchctl bootout gui/$(id -u)/com.local.codex-account-pool-bridge`; update `~/.zshrc` `KB_POOL_ORIGIN`; update `~/.config/kb/config.json` `build_args` `--pool-config` to `~/.codex-pool/kb-pool.json` (`{"origin":"http://127.0.0.1:18473","key_file":"…/state/client.key"}`).
- [ ] Update memory notes (`project_codex_account_pool_bridge.md`).
