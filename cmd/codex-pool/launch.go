package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"codex-account-pool/internal/pool"
)

const launchUsage = "usage: codex-pool launch [--config pool.json] [--account NAME] [--quiet] -- [codex arguments]"

// launchDeps are the side effects of launch, replaceable in tests.
type launchDeps struct {
	status func(context.Context) ([]pool.AccountStatus, error) // /_pool/status then probe fallback
	exec   func(bin string, args []string, env []string) error // default syscall.Exec
	stderr io.Writer
}

type launchOptions struct {
	config    string
	account   string
	quiet     bool
	codexArgs []string
}

// parseLaunchArgs splits args at the first "--": the left part holds the
// launch flags and the right part is passed to Codex untouched. Without "--",
// Codex arguments start at the first non-flag argument.
func parseLaunchArgs(args []string) (launchOptions, error) {
	var o launchOptions
	left, right, sep := args, []string(nil), false
	if i := slices.Index(args, "--"); i >= 0 {
		left, right, sep = args[:i], args[i+1:], true
	}
	fs := flag.NewFlagSet("launch", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&o.config, "config", "", "application config file")
	fs.StringVar(&o.account, "account", "", "account directory name to use")
	fs.BoolVar(&o.quiet, "quiet", false, "do not print the chosen account")
	if err := fs.Parse(left); err != nil {
		return o, fmt.Errorf("%v; %s", err, launchUsage)
	}
	if sep {
		if fs.NArg() > 0 {
			return o, fmt.Errorf("unexpected argument %q before --; %s", fs.Arg(0), launchUsage)
		}
		o.codexArgs = right
	} else {
		o.codexArgs = fs.Args()
	}
	return o, nil
}

// launchedEnv marks an environment prepared by launch. Its value is the PID
// that ran launch; exec keeps the PID, so seeing our own PID means codex_bin
// execed back into launch, while a nested codex (another process) is fine.
const launchedEnv = "CODEX_POOL_LAUNCHED"

func launchedMarker() string { return launchedEnv + "=" + strconv.Itoa(os.Getpid()) }

// withLaunchedMarker replaces any inherited marker with this process's.
func withLaunchedMarker(env []string) []string {
	env = slices.DeleteFunc(env, func(kv string) bool { return strings.HasPrefix(kv, launchedEnv+"=") })
	return append(env, launchedMarker())
}

// codexValueFlags are Codex's top-level flags that take a separate value,
// so "codex -c k=v login" is still recognised as the login subcommand.
var codexValueFlags = map[string]bool{
	"-c": true, "--config": true, "-m": true, "--model": true, "-p": true, "--profile": true,
	"-i": true, "--image": true, "-s": true, "--sandbox": true, "-a": true, "--ask-for-approval": true,
	"-C": true, "--cd": true, "--add-dir": true, "--local-provider": true, "--enable": true, "--disable": true,
}

// codexSubcommand returns the first non-flag Codex argument (before any
// "--"), which is the subcommand when there is one.
func codexSubcommand(args []string) string {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			return ""
		case codexValueFlags[a]:
			i++ // skip the flag's value
		case !strings.HasPrefix(a, "-"):
			return a
		}
	}
	return ""
}

// runLaunch is a compatibility pass-through for older wrappers. Account
// selection is deliberately not done here: normal Codex launches must use the
// user's existing CODEX_HOME, while the pool server is reserved for explicit
// round-robin requests.
func runLaunch(ctx context.Context, cfg pool.Config, store *pool.CodexHomeStore, deps launchDeps, args []string) error {
	if os.Getenv(launchedEnv) == strconv.Itoa(os.Getpid()) {
		return errors.New("launch re-entered itself; codex_bin points at the wrapper")
	}
	o, err := parseLaunchArgs(args)
	if err != nil {
		return err
	}
	if cfg.CodexBin == "" {
		return errors.New("codex_bin is not configured; set it in the config")
	}
	argv := append([]string{cfg.CodexBin}, o.codexArgs...)
	if sub := codexSubcommand(o.codexArgs); sub == "login" || sub == "logout" {
		return errors.New("run login/logout per account: codex-pool account login NAME  (or CODEX_HOME=<dir> <codex_bin> login)")
	}
	if o.account != "" || os.Getenv("CODEX_POOL_ACCOUNT") != "" {
		return errors.New("account selection was removed from codex-pool; set CODEX_HOME explicitly")
	}
	return deps.exec(cfg.CodexBin, argv, withLaunchedMarker(os.Environ()))
}

// execCodex replaces this process with the Codex binary.
func execCodex(bin string, args []string, env []string) error {
	path, err := exec.LookPath(bin)
	if err != nil {
		return err
	}
	return syscall.Exec(path, args, env)
}
