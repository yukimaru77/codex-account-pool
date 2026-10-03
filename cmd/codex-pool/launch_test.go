package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
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
	t.Setenv("CODEX_POOL_LAUNCHED", "")
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

func TestLaunchPassesThroughWithoutSelectingAnAccount(t *testing.T) {
	t.Setenv("CODEX_POOL_ACCOUNT", "")
	t.Setenv("LAUNCH_TEST_KEEP", "1")
	cfg, store, _ := launchEnv(t, map[string]bool{"alice": false, "bob": false})
	var call execCall
	var stderr bytes.Buffer
	if err := runLaunch(context.Background(), cfg, store, recordingDeps(nil, &call, &stderr), []string{"--", "exec", "--json", "hi"}); err != nil {
		t.Fatal(err)
	}
	if !call.called || call.bin != cfg.CodexBin {
		t.Fatalf("exec = %+v", call)
	}
	if got := envValues(call.env, "CODEX_HOME"); len(got) != 1 || got[0] != "" {
		t.Fatalf("CODEX_HOME = %q", got)
	}
	if got := envValues(call.env, "CODEX_POOL_LAUNCHED"); !slices.Equal(got, []string{strconv.Itoa(os.Getpid())}) {
		t.Fatalf("marker = %q", got)
	}
	if got := envValues(call.env, "LAUNCH_TEST_KEEP"); !slices.Equal(got, []string{"1"}) {
		t.Fatalf("environment not inherited: %q", got)
	}
	if stderr.Len() != 0 {
		t.Fatalf("unexpected stderr: %q", stderr.String())
	}
}

func TestLaunchAccountSelectionWasRemoved(t *testing.T) {
	t.Setenv("CODEX_POOL_ACCOUNT", "")
	cfg, store, _ := launchEnv(t, map[string]bool{"alice": false})
	var call execCall
	if err := runLaunch(context.Background(), cfg, store, recordingDeps(nil, &call, nil), []string{"--account", "alice", "--"}); err == nil {
		t.Fatal("--account unexpectedly accepted")
	}
	t.Setenv("CODEX_POOL_ACCOUNT", "alice")
	if err := runLaunch(context.Background(), cfg, store, recordingDeps(nil, &call, nil), nil); err == nil {
		t.Fatal("CODEX_POOL_ACCOUNT unexpectedly accepted")
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
		if !slices.Equal(call.env, withLaunchedMarker(os.Environ())) || !slices.Equal(envValues(call.env, "CODEX_HOME"), []string{"/my/own/codex"}) {
			t.Fatalf("env changed for %q", args)
		}
		if stderr.Len() != 0 {
			t.Fatalf("banner printed: %q", stderr.String())
		}
	}
}

func TestLaunchDoesNotProbeQuota(t *testing.T) {
	t.Setenv("CODEX_POOL_ACCOUNT", "")
	cfg, store, _ := launchEnv(t, map[string]bool{"alice": false, "bob": false})
	var call execCall
	var stderr bytes.Buffer
	statusCalled := false
	status := func(context.Context) ([]pool.AccountStatus, error) {
		statusCalled = true
		return nil, errors.New("must not be called")
	}
	if err := runLaunch(context.Background(), cfg, store, recordingDeps(status, &call, &stderr), []string{"--", "exec"}); err != nil {
		t.Fatal(err)
	}
	if statusCalled || !call.called || stderr.Len() != 0 {
		t.Fatalf("statusCalled=%v called=%v stderr=%q", statusCalled, call.called, stderr.String())
	}
	if got := envValues(call.env, "CODEX_HOME"); len(got) != 1 || got[0] != "" {
		t.Fatalf("CODEX_HOME = %q", got)
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

func TestLaunchPreservesInheritedCodexHome(t *testing.T) {
	t.Setenv("CODEX_POOL_ACCOUNT", "")
	cfg, store, _ := launchEnv(t, map[string]bool{"alice": false, "bob": false})
	for _, home := range []string{store.Path("bob"), store.Path("bob") + "/", filepath.Join(store.Path("alice"), "..", "bob")} {
		t.Setenv("CODEX_HOME", home)
		var call execCall
		var stderr bytes.Buffer
		if err := runLaunch(context.Background(), cfg, store, recordingDeps(nil, &call, &stderr), []string{"--", "exec"}); err != nil {
			t.Fatal(err)
		}
		if got := envValues(call.env, "CODEX_HOME"); !slices.Equal(got, []string{home}) || stderr.Len() != 0 {
			t.Fatalf("home=%q got=%q stderr=%q", home, got, stderr.String())
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

func TestLaunchRefusesReentry(t *testing.T) {
	t.Setenv("CODEX_POOL_ACCOUNT", "")
	cfg, store, _ := launchEnv(t, map[string]bool{"alice": false})
	// codex_bin reaching the wrapper execs launch again in the same process.
	t.Setenv("CODEX_POOL_LAUNCHED", strconv.Itoa(os.Getpid()))
	want := "launch re-entered itself; codex_bin points at the wrapper"
	for _, args := range [][]string{{"--", "exec"}, {"--", "--version"}} {
		var call execCall
		var stderr bytes.Buffer
		err := runLaunch(context.Background(), cfg, store, recordingDeps(noStatus, &call, &stderr), args)
		if err == nil || err.Error() != want {
			t.Fatalf("%q: err = %v", args, err)
		}
		if call.called {
			t.Fatalf("%q: exec ran", args)
		}
	}
	// A nested codex started by a pooled session is another process.
	t.Setenv("CODEX_POOL_LAUNCHED", strconv.Itoa(os.Getpid()+1))
	var call execCall
	var stderr bytes.Buffer
	if err := runLaunch(context.Background(), cfg, store, recordingDeps(noStatus, &call, &stderr), []string{"--", "exec"}); err != nil || !call.called {
		t.Fatalf("nested launch: err=%v called=%v", err, call.called)
	}
	if got := envValues(call.env, "CODEX_POOL_LAUNCHED"); !slices.Equal(got, []string{strconv.Itoa(os.Getpid())}) {
		t.Fatalf("CODEX_POOL_LAUNCHED = %q", got)
	}
}

func TestLaunchPassThroughSetsLaunchedMarker(t *testing.T) {
	cfg, store, _ := launchEnv(t, map[string]bool{"alice": false})
	t.Setenv("CODEX_HOME", t.TempDir())
	var call execCall
	var stderr bytes.Buffer
	if err := runLaunch(context.Background(), cfg, store, recordingDeps(noStatus, &call, &stderr), []string{"--", "exec"}); err != nil {
		t.Fatal(err)
	}
	if got := envValues(call.env, "CODEX_POOL_LAUNCHED"); !slices.Equal(got, []string{strconv.Itoa(os.Getpid())}) {
		t.Fatalf("CODEX_POOL_LAUNCHED = %q", got)
	}
}
