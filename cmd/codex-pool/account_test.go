package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codex-account-pool/internal/pool"
)

func accountJWT(t *testing.T, accountID, email string) string {
	t.Helper()
	enc := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(b)
	}
	claims := map[string]any{
		"exp":                         time.Now().Add(time.Hour).Unix(),
		"email":                       email,
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": accountID},
	}
	return enc(map[string]string{"alg": "none", "typ": "JWT"}) + "." + enc(claims) + "." + base64.RawURLEncoding.EncodeToString([]byte("sig"))
}

func codexAuthJSON(t *testing.T, accountID, email string) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"auth_mode":      "chatgpt",
		"OPENAI_API_KEY": nil,
		"tokens": map[string]any{
			"id_token":      accountJWT(t, accountID, email),
			"access_token":  accountJWT(t, accountID, email),
			"refresh_token": "refresh-" + accountID,
			"account_id":    accountID,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// newCodexHome builds a fake ~/.codex with shared and excluded entries.
func newCodexHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	for _, d := range []string{"sessions", "log"} {
		if err := os.MkdirAll(filepath.Join(home, d), 0700); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string][]byte{
		"config.toml":       []byte("model = \"x\"\n"),
		"auth.json":         codexAuthJSON(t, "acct-own", "own@example.com"),
		"models_cache.json": []byte("{}"),
	}
	for name, b := range files {
		if err := os.WriteFile(filepath.Join(home, name), b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	return home
}

type accountEnv struct {
	cfg   pool.Config
	store *pool.CodexHomeStore
	home  string
}

func newAccountEnv(t *testing.T) accountEnv {
	t.Helper()
	home := newCodexHome(t)
	accounts := filepath.Join(t.TempDir(), "accounts")
	store, err := pool.OpenCodexHomeStore(accounts, nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg := pool.DefaultConfig()
	cfg.AccountsDir, cfg.CodexHome, cfg.CodexBin = accounts, home, "codex"
	return accountEnv{cfg: cfg, store: store, home: home}
}

func noStatus(context.Context) ([]pool.AccountStatus, error) { return nil, errors.New("no status") }

func noLogin(t *testing.T) func(string) error {
	return func(string) error { t.Fatal("login must not run"); return nil }
}

func assertLinked(t *testing.T, dir, home string) {
	t.Helper()
	for _, name := range []string{"config.toml", "sessions"} {
		target, err := os.Readlink(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("%s not linked: %v", name, err)
		}
		if target != filepath.Join(home, name) {
			t.Fatalf("%s -> %s", name, target)
		}
	}
	for _, name := range []string{"models_cache.json", "log"} {
		if _, err := os.Lstat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("%s must not be linked: %v", name, err)
		}
	}
}

func TestLinkSharedSkipsExcludedEntries(t *testing.T) {
	home := newCodexHome(t)
	if err := os.WriteFile(filepath.Join(home, ".write-tmp"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	// A stale symlink is replaced; a regular file already in dir is kept.
	if err := os.Symlink("/nonexistent", filepath.Join(dir, "sessions")); err != nil {
		t.Fatal(err)
	}
	linked, err := pool.LinkShared(dir, home)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(linked, ",") != "config.toml,sessions" {
		t.Fatalf("linked = %v", linked)
	}
	assertLinked(t, dir, home)
	for _, name := range []string{"auth.json", ".write-tmp"} {
		if _, err := os.Lstat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("%s must not be linked: %v", name, err)
		}
	}
	if err := os.Remove(filepath.Join(dir, "config.toml")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte("own"), 0600); err != nil {
		t.Fatal(err)
	}
	if linked, err = pool.LinkShared(dir, home); err != nil {
		t.Fatal(err)
	}
	if strings.Join(linked, ",") != "sessions" {
		t.Fatalf("relinked = %v", linked)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "config.toml")); string(b) != "own" {
		t.Fatal("regular file in account dir was replaced")
	}
}

func TestAccountAddFromCopiesAuthAndLinks(t *testing.T) {
	env := newAccountEnv(t)
	src := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(src, codexAuthJSON(t, "acct-work", "work@example.com"), 0644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runAccount(context.Background(), env.cfg, env.store, noStatus, noLogin(t), []string{"add", "work", "--from", src}, &out); err != nil {
		t.Fatal(err)
	}
	dir := env.store.Path("work")
	info, err := os.Stat(filepath.Join(dir, "auth.json"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("auth.json not private: %v %v", info, err)
	}
	assertLinked(t, dir, env.home)
	if !strings.Contains(out.String(), "added work email=work@example.com account_id=acct-work") {
		t.Fatalf("output = %q", out.String())
	}
}

func TestAccountAddRejectsExistingName(t *testing.T) {
	env := newAccountEnv(t)
	dir := env.store.Path("work")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(dir, "keep")
	if err := os.WriteFile(keep, nil, 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runAccount(context.Background(), env.cfg, env.store, noStatus, noLogin(t), []string{"add", "work"}, &out); err == nil {
		t.Fatal("existing name accepted")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatal("existing directory was touched", err)
	}
	if err := runAccount(context.Background(), env.cfg, env.store, noStatus, noLogin(t), []string{"add", "../evil"}, &out); err == nil {
		t.Fatal("invalid name accepted")
	}
}

func TestAccountAddRunsLoginInDir(t *testing.T) {
	env := newAccountEnv(t)
	var loginDir string
	login := func(dir string) error {
		loginDir = dir
		// Codex links config.toml before login runs.
		if _, err := os.Readlink(filepath.Join(dir, "config.toml")); err != nil {
			t.Error("shared entries not linked before login", err)
		}
		return os.WriteFile(filepath.Join(dir, "auth.json"), codexAuthJSON(t, "acct-new", "new@example.com"), 0600)
	}
	var out bytes.Buffer
	if err := runAccount(context.Background(), env.cfg, env.store, noStatus, login, []string{"add", "new"}, &out); err != nil {
		t.Fatal(err)
	}
	if loginDir != env.store.Path("new") {
		t.Fatalf("login ran in %q", loginDir)
	}
	if !strings.Contains(out.String(), "added new email=new@example.com account_id=acct-new") {
		t.Fatalf("output = %q", out.String())
	}
}

func TestAccountAddCleansUpWhenLoginFails(t *testing.T) {
	env := newAccountEnv(t)
	login := func(string) error { return errors.New("login aborted") }
	var out bytes.Buffer
	if err := runAccount(context.Background(), env.cfg, env.store, noStatus, login, []string{"add", "gone"}, &out); err == nil {
		t.Fatal("failed login reported success")
	}
	if _, err := os.Lstat(env.store.Path("gone")); !os.IsNotExist(err) {
		t.Fatal("directory left after failed login", err)
	}
	// A login that wrote auth.json and then failed keeps the credential.
	login = func(dir string) error {
		if err := os.WriteFile(filepath.Join(dir, "auth.json"), codexAuthJSON(t, "acct-half", "half@example.com"), 0600); err != nil {
			t.Fatal(err)
		}
		return errors.New("exit status 1")
	}
	if err := runAccount(context.Background(), env.cfg, env.store, noStatus, login, []string{"add", "half"}, &out); err == nil {
		t.Fatal("failed login reported success")
	}
	if _, err := os.Stat(filepath.Join(env.store.Path("half"), "auth.json")); err != nil {
		t.Fatal("written auth.json removed", err)
	}
}

func TestAccountListShowsQuotaAndErrors(t *testing.T) {
	env := newAccountEnv(t)
	for name, acct := range map[string]string{"alpha": "acct-a", "beta": "acct-b"} {
		dir := env.store.Path(name)
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "auth.json"), codexAuthJSON(t, acct, name+"@example.com"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(env.store.Path("beta"), "disabled"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	broken := env.store.Path("broken")
	if err := os.MkdirAll(broken, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(broken, "auth.json"), []byte("{not json"), 0600); err != nil {
		t.Fatal(err)
	}
	list, err := env.store.List()
	if err != nil || len(list) != 2 {
		t.Fatalf("list = %v %v", list, err)
	}
	ids := map[string]string{}
	for _, c := range list {
		ids[c.Name] = c.ID()
	}
	reset := time.Date(2026, 10, 3, 12, 0, 0, 0, time.Local)
	statusFn := func(context.Context) ([]pool.AccountStatus, error) {
		return []pool.AccountStatus{
			{ID: ids["alpha"], Name: "alpha", Quota: pool.Quota{Weekly: pool.Window{Used: 25, Seconds: 604800, Reset: reset}, Observed: time.Now(), Allowed: true}},
			{ID: ids["beta"], Name: "beta", Disabled: true, Quota: pool.Quota{Weekly: pool.Window{Used: 90, Seconds: 604800, Reset: reset}, Observed: time.Now(), Allowed: true}},
		}, nil
	}
	var out bytes.Buffer
	if err := runAccount(context.Background(), env.cfg, env.store, statusFn, noLogin(t), []string{"list"}, &out); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 4 {
		t.Fatalf("output = %q", out.String())
	}
	if f := strings.Fields(lines[0]); strings.Join(f, " ") != "NAME AUTH_INDEX EMAIL REMAINING% RESET STATE" {
		t.Fatalf("header = %q", lines[0])
	}
	want := map[string][]string{
		"alpha":  {ids["alpha"], "alpha@example.com", "75%", "ok"},
		"beta":   {ids["beta"], "beta@example.com", "10%", "disabled"},
		"broken": {"-", "-", "-", "error:"},
	}
	for _, id := range []string{ids["alpha"], ids["beta"]} {
		if len(id) != 32 {
			t.Fatalf("auth_index %q is not the full id", id)
		}
	}
	for _, line := range lines[1:] {
		f := strings.Fields(line)
		w, ok := want[f[0]]
		if !ok {
			t.Fatalf("unexpected row %q", line)
		}
		if f[1] != w[0] || f[2] != w[1] || f[3] != w[2] || !strings.Contains(line, w[3]) {
			t.Fatalf("row %q, want %v", line, w)
		}
		delete(want, f[0])
	}
	if !strings.Contains(out.String(), "2026-10-03") {
		t.Fatalf("reset missing: %q", out.String())
	}
	if !strings.Contains(out.String(), "error: parse auth.json") {
		t.Fatalf("broken dir error missing: %q", out.String())
	}
}

func TestAccountDisableEnableByName(t *testing.T) {
	env := newAccountEnv(t)
	dir := env.store.Path("work")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), codexAuthJSON(t, "acct-work", "work@example.com"), 0600); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(dir, "auth.json"))
	marker := filepath.Join(dir, "disabled")
	var out bytes.Buffer
	if err := runAccount(context.Background(), env.cfg, env.store, noStatus, noLogin(t), []string{"disable", "work"}, &out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("disable did not create marker", err)
	}
	if err := runAccount(context.Background(), env.cfg, env.store, noStatus, noLogin(t), []string{"enable", "work"}, &out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("enable did not remove marker", err)
	}
	if after, _ := os.ReadFile(filepath.Join(dir, "auth.json")); !bytes.Equal(before, after) {
		t.Fatal("auth.json changed")
	}
	if err := runAccount(context.Background(), env.cfg, env.store, noStatus, noLogin(t), []string{"disable", "missing"}, &out); err == nil {
		t.Fatal("unknown name accepted")
	}
}

func TestLinkSharedSkipsAccountsDirInsideCodexHome(t *testing.T) {
	home := newCodexHome(t)
	dir := filepath.Join(home, "accounts", "work")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	linked, err := pool.LinkShared(dir, home)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(linked, ",") != "config.toml,sessions" {
		t.Fatalf("linked = %v", linked)
	}
}

func TestAccountListWithoutQuotaStillListsDirectories(t *testing.T) {
	env := newAccountEnv(t)
	dir := env.store.Path("work")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), codexAuthJSON(t, "acct-work", "work@example.com"), 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runAccount(context.Background(), env.cfg, env.store, noStatus, noLogin(t), []string{"list"}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "work@example.com") || !strings.Contains(out.String(), "quota unavailable: no status") {
		t.Fatalf("output = %q", out.String())
	}
}

func TestAccountRelinkAll(t *testing.T) {
	env := newAccountEnv(t)
	for _, name := range []string{"a", "b"} {
		dir := env.store.Path(name)
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "auth.json"), codexAuthJSON(t, "acct-"+name, name+"@example.com"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	if err := runAccount(context.Background(), env.cfg, env.store, noStatus, noLogin(t), []string{"relink", "--all"}, &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != "relinked a: 2 entries\nrelinked b: 2 entries\n" {
		t.Fatalf("output = %q", out.String())
	}
	assertLinked(t, env.store.Path("b"), env.home)
}

func TestAccountCommandThroughRun(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "pool.json")
	var out bytes.Buffer
	if err := run([]string{"init", "--config", config}, &out); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"account", "--config", config, "list"}, &out); err == nil || !strings.Contains(err.Error(), "accounts_dir") {
		t.Fatalf("legacy store error = %v", err)
	}
	config2 := filepath.Join(dir, "home.json")
	if err := run([]string{"init", "--config", config2, "--accounts-dir", "accounts", "--codex-home", newCodexHome(t)}, &out); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, "auth.json")
	if err := os.WriteFile(src, codexAuthJSON(t, "acct-x", "x@example.com"), 0600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := run([]string{"account", "add", "x", "--from", src, "--config", config2}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "added x email=x@example.com") {
		t.Fatalf("output = %q", out.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "accounts", "x", "auth.json")); err != nil {
		t.Fatal(err)
	}
}

func TestAccountAddFromRejectsInvalidSource(t *testing.T) {
	env := newAccountEnv(t)
	src := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(src, []byte(`{"auth_mode":"apikey","OPENAI_API_KEY":"sk"}`), 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runAccount(context.Background(), env.cfg, env.store, noStatus, noLogin(t), []string{"add", "bad", "--from", src}, &out); err == nil {
		t.Fatal("invalid source accepted")
	}
	if _, err := os.Lstat(env.store.Path("bad")); !os.IsNotExist(err) {
		t.Fatal("directory created for invalid source", err)
	}
}

func TestAccountAddFromCodexHomeReplacesSourceWithSymlink(t *testing.T) {
	env := newAccountEnv(t)
	src := filepath.Join(env.home, "auth.json")
	want, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runAccount(context.Background(), env.cfg, env.store, noStatus, noLogin(t), []string{"add", "main", "--from", src}, &out); err != nil {
		t.Fatal(err)
	}
	accountAuth := filepath.Join(env.store.Path("main"), "auth.json")
	target, err := os.Readlink(src)
	if err != nil {
		t.Fatalf("source not replaced by a symlink: %v", err)
	}
	if target != accountAuth {
		t.Fatalf("source -> %s, want %s", target, accountAuth)
	}
	if info, err := os.Lstat(accountAuth); err != nil || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("account auth.json must be a regular file: %v %v", info, err)
	}
	if got, err := os.ReadFile(src); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("read through symlink = %q, %v", got, err)
	}
	if strings.Contains(out.String(), "warning") {
		t.Fatalf("unexpected warning: %q", out.String())
	}
	entries, _ := os.ReadDir(env.home)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "auth.json") && e.Name() != "auth.json" {
			t.Fatalf("backup copy left behind: %s", e.Name())
		}
	}
}

func TestAccountAddFromOtherPathWarns(t *testing.T) {
	env := newAccountEnv(t)
	src := filepath.Join(t.TempDir(), "auth.json")
	b := codexAuthJSON(t, "acct-other", "other@example.com")
	if err := os.WriteFile(src, b, 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runAccount(context.Background(), env.cfg, env.store, noStatus, noLogin(t), []string{"add", "other", "--from", src}, &out); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(src); err != nil || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("other source must be left alone: %v %v", info, err)
	}
	if got, _ := os.ReadFile(src); !bytes.Equal(got, b) {
		t.Fatal("other source modified")
	}
	want := "warning: " + src + " still holds the same refresh token; stop using it or it will exhaust this account"
	if !strings.Contains(out.String(), want) {
		t.Fatalf("output = %q, want warning %q", out.String(), want)
	}
}

func TestAccountAddRefusesDuplicateAccount(t *testing.T) {
	env := newAccountEnv(t)
	existing := env.store.Path("mid")
	if err := os.MkdirAll(existing, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(existing, "auth.json"), codexAuthJSON(t, "acct-dup", "dup@example.com"), 0600); err != nil {
		t.Fatal(err)
	}
	// Both a name sorting after and one sorting before the existing
	// directory are refused, through --from and through login.
	src := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(src, codexAuthJSON(t, "acct-dup", "dup@example.com"), 0600); err != nil {
		t.Fatal(err)
	}
	login := func(dir string) error {
		return os.WriteFile(filepath.Join(dir, "auth.json"), codexAuthJSON(t, "acct-dup", "dup@example.com"), 0600)
	}
	for _, c := range []struct {
		name  string
		args  []string
		login func(string) error
	}{
		{"zed", []string{"add", "zed", "--from", src}, noLogin(t)},
		{"aaa", []string{"add", "aaa"}, login},
	} {
		var out bytes.Buffer
		err := runAccount(context.Background(), env.cfg, env.store, noStatus, c.login, c.args, &out)
		if err == nil || err.Error() != "account already registered as mid" {
			t.Fatalf("%s: err = %v", c.name, err)
		}
		if _, err := os.Lstat(env.store.Path(c.name)); !os.IsNotExist(err) {
			t.Fatalf("%s: new directory left behind: %v", c.name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(existing, "auth.json")); err != nil {
		t.Fatal("existing account touched", err)
	}
}

func TestAccountLoginRunsInExistingDir(t *testing.T) {
	env := newAccountEnv(t)
	dir := env.store.Path("work")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), codexAuthJSON(t, "acct-work", "old@example.com"), 0600); err != nil {
		t.Fatal(err)
	}
	var loginDir string
	account := "acct-work"
	login := func(d string) error {
		loginDir = d
		return os.WriteFile(filepath.Join(d, "auth.json"), codexAuthJSON(t, account, "new@example.com"), 0600)
	}
	var out bytes.Buffer
	if err := runAccount(context.Background(), env.cfg, env.store, noStatus, login, []string{"login", "work"}, &out); err != nil {
		t.Fatal(err)
	}
	if loginDir != dir {
		t.Fatalf("login ran in %q", loginDir)
	}
	if out.String() != "logged in work email=new@example.com account_id=acct-work\n" {
		t.Fatalf("output = %q", out.String())
	}

	// A login as a different account is reported and fails, but the new
	// auth.json (the user's choice) is kept.
	account = "acct-other"
	out.Reset()
	err := runAccount(context.Background(), env.cfg, env.store, noStatus, login, []string{"login", "work"}, &out)
	if err == nil || !strings.Contains(err.Error(), "acct-other") || !strings.Contains(err.Error(), "acct-work") {
		t.Fatalf("mismatch err = %v", err)
	}
	c, err := env.store.Validate("work")
	if err != nil || c.AccountID != "acct-other" {
		t.Fatalf("new auth.json not kept: %v %v", c.AccountID, err)
	}

	// A directory without a readable auth.json can be logged into.
	fresh := env.store.Path("fresh")
	if err := os.MkdirAll(fresh, 0700); err != nil {
		t.Fatal(err)
	}
	account = "acct-fresh"
	out.Reset()
	if err := runAccount(context.Background(), env.cfg, env.store, noStatus, login, []string{"login", "fresh"}, &out); err != nil {
		t.Fatal(err)
	}
}

func TestAccountLoginRejectsUnknownName(t *testing.T) {
	env := newAccountEnv(t)
	var out bytes.Buffer
	for _, args := range [][]string{{"login", "missing"}, {"login", "../evil"}, {"login"}, {"login", "a", "b"}} {
		if err := runAccount(context.Background(), env.cfg, env.store, noStatus, noLogin(t), args, &out); err == nil {
			t.Fatalf("%q accepted", args)
		}
	}
	if _, err := os.Lstat(env.store.Path("missing")); !os.IsNotExist(err) {
		t.Fatal("login created a directory", err)
	}
}
