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
	"sort"
	"strings"
	"syscall"
	"time"

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

// helpOnly reports whether the Codex arguments (up to any "--") are only help
// or version flags, which need no account.
func helpOnly(args []string) bool {
	if i := slices.Index(args, "--"); i >= 0 {
		args = args[:i]
	}
	if len(args) == 0 {
		return false
	}
	for _, a := range args {
		switch a {
		case "--help", "-h", "--version", "-V":
		default:
			return false
		}
	}
	return true
}

// runLaunch picks the account directory with the most usable weekly quota
// (fill-first), sets CODEX_HOME to it and execs the real Codex binary.
func runLaunch(ctx context.Context, cfg pool.Config, store *pool.CodexHomeStore, deps launchDeps, args []string) error {
	o, err := parseLaunchArgs(args)
	if err != nil {
		return err
	}
	if cfg.CodexBin == "" {
		return errors.New("codex_bin is not configured; set it in the config")
	}
	argv := append([]string{cfg.CodexBin}, o.codexArgs...)
	if helpOnly(o.codexArgs) {
		return deps.exec(cfg.CodexBin, argv, os.Environ())
	}
	stderr := deps.stderr
	if stderr == nil {
		stderr = io.Discard
	}
	list, err := store.List()
	if err != nil {
		return err
	}
	creds := make(map[string]pool.Credential, len(list))
	names := make(map[string]string, len(list)) // account ID -> directory name
	for _, c := range list {
		creds[c.Name] = c
		names[c.ID()] = c.Name
	}

	pinned := o.account
	if pinned == "" {
		pinned = os.Getenv("CODEX_POOL_ACCOUNT")
	}
	var chosen string
	var statuses []pool.AccountStatus
	if pinned != "" {
		c, ok := creds[pinned]
		if !ok {
			if msg, broken := store.Errors()[pinned]; broken {
				return fmt.Errorf("account %q is unusable: %s", pinned, msg)
			}
			return fmt.Errorf("account %q not found in %s", pinned, store.Dir)
		}
		if c.Disabled {
			return fmt.Errorf("account %q is disabled; enable it with: codex-pool account enable %s", pinned, pinned)
		}
		chosen = pinned
		if !o.quiet {
			statuses, _ = mapStatuses(ctx, deps.status, creds, names)
		}
	} else {
		var statusErr error
		statuses, statusErr = mapStatuses(ctx, deps.status, creds, names)
		maxAge := time.Duration(cfg.QuotaMaxAgeSeconds) * time.Second
		id, ok := pool.PickFillFirst(statuses, time.Now(), maxAge, cfg.ReservePercent)
		for _, s := range statuses {
			if s.ID == id {
				chosen = s.Name
			}
		}
		switch {
		case chosen != "" && !ok:
			fmt.Fprintf(stderr, "codex-pool: no account has quota above reserve; using %s\n", chosen)
		case chosen == "":
			if chosen = firstEnabled(creds); chosen == "" {
				return fmt.Errorf("no enabled account in %s; add one with: codex-pool account add NAME", store.Dir)
			}
			reason := "no quota status for any enabled account"
			if statusErr != nil {
				reason = "quota status unavailable: " + statusErr.Error()
			}
			fmt.Fprintf(stderr, "codex-pool: %s; using %s\n", reason, chosen)
		}
	}

	dir := store.Path(chosen)
	env := slices.DeleteFunc(os.Environ(), func(kv string) bool { return strings.HasPrefix(kv, "CODEX_HOME=") })
	env = append(env, "CODEX_HOME="+dir)
	if !o.quiet {
		fmt.Fprintf(stderr, "codex-pool: account=%s remaining=%s%%\n", chosen, remainingOf(statuses, chosen))
	}
	return deps.exec(cfg.CodexBin, argv, env)
}

// mapStatuses fetches the status and keeps the entries that belong to an
// account directory, filling Name from the ID for legacy (unnamed) statuses.
// The directory's disabled marker is authoritative.
func mapStatuses(ctx context.Context, status func(context.Context) ([]pool.AccountStatus, error), creds map[string]pool.Credential, names map[string]string) ([]pool.AccountStatus, error) {
	if status == nil {
		return nil, errors.New("no status source")
	}
	st, err := status(ctx)
	if err != nil {
		return nil, err
	}
	var out []pool.AccountStatus
	for _, s := range st {
		if s.Name == "" {
			s.Name = names[s.ID]
		}
		c, ok := creds[s.Name]
		if !ok {
			continue
		}
		s.Disabled = s.Disabled || c.Disabled
		out = append(out, s)
	}
	return out, nil
}

func firstEnabled(creds map[string]pool.Credential) string {
	var names []string
	for name, c := range creds {
		if !c.Disabled {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return ""
	}
	return names[0]
}

// remainingOf formats the weekly quota left for name, or "?" when unknown.
func remainingOf(statuses []pool.AccountStatus, name string) string {
	for _, s := range statuses {
		if s.Name == name && s.Error == "" && !s.Quota.Observed.IsZero() && s.Quota.Weekly.Seconds == 604800 {
			return fmt.Sprintf("%.0f", 100-s.Quota.Weekly.Used)
		}
	}
	return "?"
}

// execCodex replaces this process with the Codex binary.
func execCodex(bin string, args []string, env []string) error {
	path, err := exec.LookPath(bin)
	if err != nil {
		return err
	}
	return syscall.Exec(path, args, env)
}
