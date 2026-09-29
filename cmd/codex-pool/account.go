package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"text/tabwriter"

	"codex-account-pool/internal/pool"
)

const accountUsage = "usage: codex-pool account {add NAME [--from PATH]|list|enable NAME|disable NAME|relink NAME|--all}"

// runAccount manages the Codex-home account directories under accounts_dir.
// statusFn supplies quota for list; execLogin runs `codex login` with
// CODEX_HOME set to the given directory.
func runAccount(ctx context.Context, cfg pool.Config, store *pool.CodexHomeStore, statusFn func(context.Context) ([]pool.AccountStatus, error), execLogin func(dir string) error, args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New(accountUsage)
	}
	switch args[0] {
	case "add":
		name, from, err := parseAddArgs(args[1:])
		if err != nil {
			return err
		}
		return accountAdd(cfg, store, execLogin, name, from, out)
	case "list":
		if len(args) != 1 {
			return errors.New(accountUsage)
		}
		return accountList(ctx, store, statusFn, out)
	case "enable", "disable":
		if len(args) != 2 {
			return errors.New(accountUsage)
		}
		c, err := credentialByName(store, args[1])
		if err != nil {
			return err
		}
		if err := store.SetDisabled(c.ID(), args[0] == "disable"); err != nil {
			return err
		}
		fmt.Fprintf(out, "%sd %s\n", args[0], args[1])
		return nil
	case "relink":
		if len(args) != 2 {
			return errors.New(accountUsage)
		}
		return accountRelink(cfg, store, args[1], out)
	default:
		return fmt.Errorf("unknown account command %q; %s", args[0], accountUsage)
	}
}

// parseAddArgs accepts NAME and --from PATH in either order.
func parseAddArgs(args []string) (name, from string, err error) {
	fs := flag.NewFlagSet("account add", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&from, "from", "", "existing Codex auth.json to copy")
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return "", "", err
		}
		if fs.NArg() == 0 {
			break
		}
		positional = append(positional, fs.Arg(0))
		args = fs.Args()[1:]
	}
	if len(positional) != 1 {
		return "", "", errors.New("usage: codex-pool account add NAME [--from PATH]")
	}
	return positional[0], from, nil
}

func accountAdd(cfg pool.Config, store *pool.CodexHomeStore, execLogin func(string) error, name, from string, out io.Writer) (err error) {
	if !pool.ValidAccountName(name) {
		return fmt.Errorf("invalid account name %q (letters, digits, _ and -, up to 64)", name)
	}
	if from != "" {
		// Check the source first so a bad file is never copied in.
		if _, err := pool.ReadCodexAuthFile(from); err != nil {
			return fmt.Errorf("%s: %w", from, err)
		}
	}
	dir := store.Path(name)
	if _, err := os.Lstat(dir); err == nil {
		return fmt.Errorf("account directory %s already exists", dir)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	authPath := filepath.Join(dir, "auth.json")
	defer func() {
		if err == nil {
			return
		}
		// Keep the directory once a credential landed in it; an empty
		// attempt leaves nothing behind so the name can be retried.
		if _, statErr := os.Lstat(authPath); errors.Is(statErr, os.ErrNotExist) {
			_ = os.RemoveAll(dir)
		}
	}()
	if cfg.CodexHome != "" {
		if _, err := pool.LinkShared(dir, cfg.CodexHome); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("link shared Codex home entries: %w", err)
		}
	}
	if from != "" {
		b, err := os.ReadFile(from)
		if err != nil {
			return err
		}
		if err := pool.AtomicWrite(authPath, b); err != nil { // 0600
			return err
		}
	} else if err := execLogin(dir); err != nil {
		return fmt.Errorf("codex login: %w", err)
	}
	c, err := store.Validate(name)
	if err != nil {
		return fmt.Errorf("account %s: %w", name, err)
	}
	if other, err := registeredAs(store, name, c.ID()); err != nil {
		return err
	} else if other != "" {
		// The directory was created by this command; drop it with its copy.
		_ = os.RemoveAll(dir)
		return fmt.Errorf("account already registered as %s", other)
	}
	if from != "" {
		if err := retireSource(cfg, from, authPath, out); err != nil {
			return err
		}
	}
	fmt.Fprintf(out, "added %s email=%s account_id=%s\n", name, c.Email, c.AccountID)
	return nil
}

// retireSource keeps a copied credential from splitting its refresh-token
// chain: refresh tokens are single use, so two files holding the same one
// exhaust each other. When the source is the user's own Codex home auth.json
// it becomes a symlink to the account's file (no backup copy is kept, since
// a backup would be one more holder of the chain); any other source is left
// alone with a warning.
func retireSource(cfg pool.Config, from, authPath string, out io.Writer) error {
	if !sameCodexHomeAuth(cfg.CodexHome, from) {
		fmt.Fprintf(out, "warning: %s still holds the same refresh token; stop using it or it will exhaust this account\n", from)
		return nil
	}
	target, err := filepath.Abs(authPath)
	if err != nil {
		return err
	}
	// Swap in the symlink with a rename so Codex never sees auth.json missing.
	tmp := filepath.Join(filepath.Dir(from), ".auth.json.pool-link")
	_ = os.Remove(tmp)
	if err := os.Symlink(target, tmp); err != nil {
		return fmt.Errorf("link %s to the account: %w", from, err)
	}
	if err := os.Rename(tmp, from); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("link %s to the account: %w", from, err)
	}
	fmt.Fprintf(out, "linked %s -> %s\n", from, target)
	return nil
}

// sameCodexHomeAuth reports whether path names <codexHome>/auth.json,
// comparing the resolved parent directories.
func sameCodexHomeAuth(codexHome, path string) bool {
	if codexHome == "" || filepath.Base(path) != "auth.json" {
		return false
	}
	resolve := func(dir string) (string, bool) {
		abs, err := filepath.Abs(dir)
		if err != nil {
			return "", false
		}
		real, err := filepath.EvalSymlinks(abs)
		return real, err == nil
	}
	home, ok1 := resolve(codexHome)
	parent, ok2 := resolve(filepath.Dir(path))
	return ok1 && ok2 && home == parent
}

// registeredAs returns the name of another directory already holding the
// account id. Each directory is read directly, since List keeps only the
// first of two duplicates by name order.
func registeredAs(store *pool.CodexHomeStore, name, id string) (string, error) {
	names, err := store.Names()
	if err != nil {
		return "", err
	}
	for _, other := range names {
		if other == name {
			continue
		}
		if c, err := store.Validate(other); err == nil && c.ID() == id {
			return other, nil
		}
	}
	return "", nil
}

func credentialByName(store *pool.CodexHomeStore, name string) (pool.Credential, error) {
	list, err := store.List()
	if err != nil {
		return pool.Credential{}, err
	}
	for _, c := range list {
		if c.Name == name {
			return c, nil
		}
	}
	if msg, ok := store.Errors()[name]; ok {
		return pool.Credential{}, fmt.Errorf("account %s is unreadable: %s", name, msg)
	}
	return pool.Credential{}, fmt.Errorf("account %s not found", name)
}

func accountList(ctx context.Context, store *pool.CodexHomeStore, statusFn func(context.Context) ([]pool.AccountStatus, error), out io.Writer) error {
	list, err := store.List()
	if err != nil {
		return err
	}
	errs := store.Errors()
	statuses := map[string]pool.AccountStatus{}
	var quotaErr error
	if len(list) > 0 {
		st, err := statusFn(ctx)
		quotaErr = err // the directory listing is still useful without quota
		for _, s := range st {
			statuses[s.ID] = s
		}
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tEMAIL\tREMAINING%\tRESET\tSTATE")
	for _, c := range list {
		remaining, reset := "-", "-"
		state := "ok"
		s, ok := statuses[c.ID()]
		if left, known := s.Quota.WeeklyRemaining(); ok && known {
			remaining = fmt.Sprintf("%.0f%%", left)
			if !s.Quota.Weekly.Reset.IsZero() {
				reset = s.Quota.Weekly.Reset.Local().Format("2006-01-02 15:04")
			}
		}
		switch {
		case c.Disabled:
			state = "disabled"
		case ok && s.Error != "":
			state = "error: " + s.Error
		}
		email := c.Email
		if email == "" {
			email = "-"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", c.Name, email, remaining, reset, state)
	}
	names := make([]string, 0, len(errs))
	for name := range errs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(w, "%s\t-\t-\t-\terror: %s\n", name, errs[name])
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if quotaErr != nil {
		fmt.Fprintf(out, "quota unavailable: %v\n", quotaErr)
	}
	return nil
}

func accountRelink(cfg pool.Config, store *pool.CodexHomeStore, arg string, out io.Writer) error {
	if cfg.CodexHome == "" {
		return errors.New("codex_home is not configured")
	}
	var names []string
	if arg == "--all" {
		var err error
		if names, err = store.Names(); err != nil {
			return err
		}
	} else {
		if !pool.ValidAccountName(arg) {
			return fmt.Errorf("invalid account name %q", arg)
		}
		if info, err := os.Stat(store.Path(arg)); err != nil || !info.IsDir() {
			return fmt.Errorf("account %s not found", arg)
		}
		names = []string{arg}
	}
	for _, name := range names {
		linked, err := pool.LinkShared(store.Path(name), cfg.CodexHome)
		if err != nil {
			return fmt.Errorf("relink %s: %w", name, err)
		}
		fmt.Fprintf(out, "relinked %s: %d entries\n", name, len(linked))
	}
	return nil
}
