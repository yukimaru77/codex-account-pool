package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"codex-account-pool/internal/pool"
)

type execCall struct {
	called bool
	bin    string
	args   []string
	env    []string
}

// launchEnv creates account directories (name -> disabled) and returns the
// config, store and the directory name -> account ID map.
func launchEnv(t *testing.T, accounts map[string]bool) (pool.Config, *pool.CodexHomeStore, map[string]string) {
	t.Helper()
	// The test itself may run under a pooled Codex session.
	t.Setenv("CODEX_HOME", "")
	env := newAccountEnv(t)
	env.cfg.CodexBin = "/opt/codex/bin/codex"
	for name, disabled := range accounts {
		dir := env.store.Path(name)
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "auth.json"), codexAuthJSON(t, "acct-"+name, name+"@example.com"), 0600); err != nil {
			t.Fatal(err)
		}
		if disabled {
			if err := os.WriteFile(filepath.Join(dir, "disabled"), nil, 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	list, err := env.store.List()
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]string{}
	for _, c := range list {
		ids[c.Name] = c.ID()
	}
	return env.cfg, env.store, ids
}

func weekly(remaining float64, reset time.Duration) pool.Quota {
	return pool.Quota{Weekly: pool.Window{Used: 100 - remaining, Seconds: 604800, Reset: time.Now().Add(reset)}, Observed: time.Now(), Allowed: true}
}

func recordingDeps(status func(context.Context) ([]pool.AccountStatus, error), call *execCall, stderr *bytes.Buffer) launchDeps {
	return launchDeps{
		status: status,
		exec: func(bin string, args []string, env []string) error {
			call.called, call.bin, call.args, call.env = true, bin, args, env
			return nil
		},
		stderr: stderr,
	}
}

func envValues(env []string, key string) []string {
	var out []string
	for _, kv := range env {
		if strings.HasPrefix(kv, key+"=") {
			out = append(out, strings.TrimPrefix(kv, key+"="))
		}
	}
	return out
}

func TestLaunchSetsCodexHomeAndExecs(t *testing.T) {
	t.Setenv("CODEX_POOL_ACCOUNT", "")
	t.Setenv("LAUNCH_TEST_KEEP", "1")
	cfg, store, ids := launchEnv(t, map[string]bool{"alice": false, "bob": false})
	status := func(context.Context) ([]pool.AccountStatus, error) {
		return []pool.AccountStatus{
			{ID: ids["alice"], Name: "alice", Quota: weekly(60, 24*time.Hour)},
			{ID: ids["bob"], Name: "bob", Quota: weekly(90, 72*time.Hour)},
		}, nil
	}
	var call execCall
	var stderr bytes.Buffer
	if err := runLaunch(context.Background(), cfg, store, recordingDeps(status, &call, &stderr), []string{"--", "exec", "--json", "hi"}); err != nil {
		t.Fatal(err)
	}
	if !call.called || call.bin != cfg.CodexBin {
		t.Fatalf("exec = %+v", call)
	}
	if want := []string{cfg.CodexBin, "exec", "--json", "hi"}; !slices.Equal(call.args, want) {
		t.Fatalf("args = %q, want %q", call.args, want)
	}
	if got := envValues(call.env, "CODEX_HOME"); !slices.Equal(got, []string{store.Path("alice")}) {
		t.Fatalf("CODEX_HOME = %q", got)
	}
	if got := envValues(call.env, "LAUNCH_TEST_KEEP"); !slices.Equal(got, []string{"1"}) {
		t.Fatalf("environment not inherited: %q", got)
	}
	if got := stderr.String(); got != "codex-pool: account=alice remaining=60%\n" {
		t.Fatalf("stderr = %q", got)
	}

	// A legacy status without names is matched to directories by ID.
	legacy := func(context.Context) ([]pool.AccountStatus, error) {
		return []pool.AccountStatus{
			{ID: ids["alice"], Quota: weekly(60, 72*time.Hour)},
			{ID: ids["bob"], Quota: weekly(90, 24*time.Hour)},
		}, nil
	}
	call, stderr = execCall{}, bytes.Buffer{}
	if err := runLaunch(context.Background(), cfg, store, recordingDeps(legacy, &call, &stderr), []string{"--"}); err != nil {
		t.Fatal(err)
	}
	if got := envValues(call.env, "CODEX_HOME"); !slices.Equal(got, []string{store.Path("bob")}) {
		t.Fatalf("legacy CODEX_HOME = %q", got)
	}

	cfg.CodexBin = ""
	if err := runLaunch(context.Background(), cfg, store, recordingDeps(status, &call, &stderr), nil); err == nil || !strings.Contains(err.Error(), "codex_bin is not configured") {
		t.Fatalf("empty codex_bin: %v", err)
	}
}

func TestLaunchPinnedAccount(t *testing.T) {
	t.Setenv("CODEX_POOL_ACCOUNT", "")
	cfg, store, ids := launchEnv(t, map[string]bool{"alice": false, "bob": false, "carol": true})
	status := func(context.Context) ([]pool.AccountStatus, error) {
		return []pool.AccountStatus{
			{ID: ids["alice"], Name: "alice", Quota: weekly(60, 24*time.Hour)},
			{ID: ids["bob"], Name: "bob", Quota: weekly(5, 72*time.Hour)},
			{ID: ids["carol"], Name: "carol", Disabled: true, Quota: weekly(90, time.Hour)},
		}, nil
	}
	var call execCall
	var stderr bytes.Buffer
	if err := runLaunch(context.Background(), cfg, store, recordingDeps(status, &call, &stderr), []string{"--account", "bob", "--", "resume"}); err != nil {
		t.Fatal(err)
	}
	if got := envValues(call.env, "CODEX_HOME"); !slices.Equal(got, []string{store.Path("bob")}) {
		t.Fatalf("pinned CODEX_HOME = %q", got)
	}
	if !slices.Equal(call.args, []string{cfg.CodexBin, "resume"}) {
		t.Fatalf("args = %q", call.args)
	}
	if got := stderr.String(); got != "codex-pool: account=bob remaining=5%\n" {
		t.Fatalf("stderr = %q", got)
	}

	// CODEX_POOL_ACCOUNT is the same as --account.
	t.Setenv("CODEX_POOL_ACCOUNT", "bob")
	call = execCall{}
	if err := runLaunch(context.Background(), cfg, store, recordingDeps(status, &call, &stderr), []string{"--quiet"}); err != nil {
		t.Fatal(err)
	}
	if got := envValues(call.env, "CODEX_HOME"); !slices.Equal(got, []string{store.Path("bob")}) {
		t.Fatalf("env-pinned CODEX_HOME = %q", got)
	}
	t.Setenv("CODEX_POOL_ACCOUNT", "")

	for _, name := range []string{"carol", "nobody"} {
		call = execCall{}
		err := runLaunch(context.Background(), cfg, store, recordingDeps(status, &call, &stderr), []string{"--account", name, "--"})
		if err == nil || !strings.Contains(err.Error(), name) {
			t.Fatalf("pinned %s: err = %v", name, err)
		}
		if call.called {
			t.Fatalf("pinned %s: exec ran", name)
		}
	}
}

func TestLaunchHelpBypassesSelection(t *testing.T) {
	cfg, store, _ := launchEnv(t, map[string]bool{"alice": false})
	t.Setenv("CODEX_HOME", "/my/own/codex")
	status := func(context.Context) ([]pool.AccountStatus, error) {
		t.Fatal("status must not be called")
		return nil, nil
	}
	for _, args := range [][]string{{"--", "--version"}, {"--", "-h"}, {"--", "--help", "-V"}} {
		var call execCall
		var stderr bytes.Buffer
		if err := runLaunch(context.Background(), cfg, store, recordingDeps(status, &call, &stderr), args); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(call.args, append([]string{cfg.CodexBin}, args[1:]...)) {
			t.Fatalf("args = %q", call.args)
		}
		if !slices.Equal(call.env, os.Environ()) || !slices.Equal(envValues(call.env, "CODEX_HOME"), []string{"/my/own/codex"}) {
			t.Fatalf("env changed for %q", args)
		}
		if stderr.Len() != 0 {
			t.Fatalf("banner printed: %q", stderr.String())
		}
	}
}

func TestLaunchFallsBackWhenNoQuota(t *testing.T) {
	t.Setenv("CODEX_POOL_ACCOUNT", "")
	cfg, store, ids := launchEnv(t, map[string]bool{"alice": true, "bob": false, "carol": false})
	var call execCall
	var stderr bytes.Buffer
	failed := func(context.Context) ([]pool.AccountStatus, error) { return nil, errors.New("server down") }
	if err := runLaunch(context.Background(), cfg, store, recordingDeps(failed, &call, &stderr), []string{"--", "exec"}); err != nil {
		t.Fatal(err)
	}
	if got := envValues(call.env, "CODEX_HOME"); !slices.Equal(got, []string{store.Path("bob")}) {
		t.Fatalf("fallback CODEX_HOME = %q", got)
	}
	if got := stderr.String(); !strings.Contains(got, "server down") || !strings.Contains(got, "codex-pool: account=bob remaining=?%\n") {
		t.Fatalf("stderr = %q", got)
	}

	// Quota known but nothing above the reserve: least-used account with a warning.
	low := func(context.Context) ([]pool.AccountStatus, error) {
		return []pool.AccountStatus{
			{ID: ids["bob"], Name: "bob", Quota: weekly(2, time.Hour)},
			{ID: ids["carol"], Name: "carol", Quota: weekly(7, time.Hour)},
		}, nil
	}
	call, stderr = execCall{}, bytes.Buffer{}
	if err := runLaunch(context.Background(), cfg, store, recordingDeps(low, &call, &stderr), nil); err != nil {
		t.Fatal(err)
	}
	if got := envValues(call.env, "CODEX_HOME"); !slices.Equal(got, []string{store.Path("carol")}) {
		t.Fatalf("best-effort CODEX_HOME = %q", got)
	}
	if want := "codex-pool: no account has quota above reserve; using carol\ncodex-pool: account=carol remaining=7%\n"; stderr.String() != want {
		t.Fatalf("stderr = %q, want %q", stderr.String(), want)
	}

	// Empty status also falls back.
	empty := func(context.Context) ([]pool.AccountStatus, error) { return nil, nil }
	call, stderr = execCall{}, bytes.Buffer{}
	if err := runLaunch(context.Background(), cfg, store, recordingDeps(empty, &call, &stderr), nil); err != nil {
		t.Fatal(err)
	}
	if got := envValues(call.env, "CODEX_HOME"); !slices.Equal(got, []string{store.Path("bob")}) || !call.called {
		t.Fatalf("empty-status CODEX_HOME = %q", got)
	}
}

func TestLaunchQuietSuppressesBanner(t *testing.T) {
	t.Setenv("CODEX_POOL_ACCOUNT", "")
	cfg, store, ids := launchEnv(t, map[string]bool{"alice": false})
	status := func(context.Context) ([]pool.AccountStatus, error) {
		return []pool.AccountStatus{{ID: ids["alice"], Name: "alice", Quota: weekly(60, time.Hour)}}, nil
	}
	var call execCall
	var stderr bytes.Buffer
	if err := runLaunch(context.Background(), cfg, store, recordingDeps(status, &call, &stderr), []string{"--quiet", "--", "exec"}); err != nil {
		t.Fatal(err)
	}
	if !call.called || stderr.Len() != 0 {
		t.Fatalf("called=%v stderr=%q", call.called, stderr.String())
	}
}

func TestLaunchArgsParsing(t *testing.T) {
	cases := []struct {
		args    []string
		account string
		quiet   bool
		codex   []string
		config  string
	}{
		{[]string{"--config", "p.json", "--quiet", "--", "exec", "--config", "x=y", "--", "z"}, "", true, []string{"exec", "--config", "x=y", "--", "z"}, "p.json"},
		{[]string{"--account=bob", "exec", "-c", "k=v"}, "bob", false, []string{"exec", "-c", "k=v"}, ""},
		{nil, "", false, nil, ""},
	}
	for _, c := range cases {
		o, err := parseLaunchArgs(c.args)
		if err != nil {
			t.Fatalf("%q: %v", c.args, err)
		}
		if o.account != c.account || o.quiet != c.quiet || !slices.Equal(o.codexArgs, c.codex) || o.config != c.config {
			t.Fatalf("%q: got %+v", c.args, o)
		}
	}
	if _, err := parseLaunchArgs([]string{"exec", "--", "x"}); err == nil {
		t.Fatal("positional argument before -- accepted")
	}
}

func TestLaunchRefusesLoginLogout(t *testing.T) {
	t.Setenv("CODEX_POOL_ACCOUNT", "")
	cfg, store, _ := launchEnv(t, map[string]bool{"alice": false})
	status := func(context.Context) ([]pool.AccountStatus, error) {
		t.Fatal("status must not be called")
		return nil, nil
	}
	want := "run login/logout per account: codex-pool account login NAME  (or CODEX_HOME=<dir> <codex_bin> login)"
	for _, args := range [][]string{{"--", "login"}, {"--", "logout"}, {"--", "-c", "k=v", "login", "--with-api-key"}, {"login"}} {
		var call execCall
		var stderr bytes.Buffer
		err := runLaunch(context.Background(), cfg, store, recordingDeps(status, &call, &stderr), args)
		if err == nil || err.Error() != want {
			t.Fatalf("%q: err = %v", args, err)
		}
		if call.called {
			t.Fatalf("%q: exec ran", args)
		}
	}
	// "login" as a later argument (e.g. a prompt) is not the subcommand.
	var call execCall
	var stderr bytes.Buffer
	if err := runLaunch(context.Background(), cfg, store, recordingDeps(noStatus, &call, &stderr), []string{"--", "exec", "login"}); err != nil || !call.called {
		t.Fatalf("exec login prompt: err=%v called=%v", err, call.called)
	}
}

func TestLaunchInheritedCodexHomeUnderAccountsDirPins(t *testing.T) {
	t.Setenv("CODEX_POOL_ACCOUNT", "")
	cfg, store, ids := launchEnv(t, map[string]bool{"alice": false, "bob": false})
	status := func(context.Context) ([]pool.AccountStatus, error) {
		return []pool.AccountStatus{
			{ID: ids["alice"], Name: "alice", Quota: weekly(90, 24*time.Hour)},
			{ID: ids["bob"], Name: "bob", Quota: weekly(20, 72*time.Hour)},
		}, nil
	}
	// A nested codex started from a pooled session keeps its account, even
	// when CODEX_HOME is spelled with a trailing slash or "..".
	for _, home := range []string{store.Path("bob"), store.Path("bob") + "/", filepath.Join(store.Path("alice"), "..", "bob")} {
		t.Setenv("CODEX_HOME", home)
		var call execCall
		var stderr bytes.Buffer
		if err := runLaunch(context.Background(), cfg, store, recordingDeps(status, &call, &stderr), []string{"--", "exec"}); err != nil {
			t.Fatal(err)
		}
		if got := envValues(call.env, "CODEX_HOME"); !slices.Equal(got, []string{store.Path("bob")}) {
			t.Fatalf("%s: CODEX_HOME = %q", home, got)
		}
		if got := stderr.String(); got != "codex-pool: account=bob remaining=20%\n" {
			t.Fatalf("stderr = %q", got)
		}
	}
	// A disabled or unknown directory under accounts_dir is refused like --account.
	if err := os.WriteFile(filepath.Join(store.Path("bob"), "disabled"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"bob", "nobody"} {
		t.Setenv("CODEX_HOME", store.Path(name))
		var call execCall
		var stderr bytes.Buffer
		if err := runLaunch(context.Background(), cfg, store, recordingDeps(status, &call, &stderr), nil); err == nil || call.called {
			t.Fatalf("%s: err=%v called=%v", name, err, call.called)
		}
	}
}

func TestLaunchInheritedCodexHomeElsewherePassesThrough(t *testing.T) {
	t.Setenv("CODEX_POOL_ACCOUNT", "")
	cfg, store, _ := launchEnv(t, map[string]bool{"alice": false})
	own := t.TempDir()
	t.Setenv("CODEX_HOME", own)
	status := func(context.Context) ([]pool.AccountStatus, error) {
		t.Fatal("status must not be called")
		return nil, nil
	}
	var call execCall
	var stderr bytes.Buffer
	if err := runLaunch(context.Background(), cfg, store, recordingDeps(status, &call, &stderr), []string{"--", "exec", "hi"}); err != nil {
		t.Fatal(err)
	}
	if !call.called || !slices.Equal(call.args, []string{cfg.CodexBin, "exec", "hi"}) {
		t.Fatalf("exec = %+v", call)
	}
	if got := envValues(call.env, "CODEX_HOME"); !slices.Equal(got, []string{own}) {
		t.Fatalf("CODEX_HOME = %q", got)
	}
	if stderr.Len() != 0 {
		t.Fatalf("banner printed: %q", stderr.String())
	}
}
