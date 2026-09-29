package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"codex-account-pool/internal/cpa"
	"codex-account-pool/internal/pool"
	"golang.org/x/sys/unix"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "codex-pool:", err)
		os.Exit(1)
	}
}

func run(args []string, out io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: codex-pool {init|account|launch|serve|status|probe|enable|disable|login|import} --config pool.json [files or account ID]")
	}
	command := args[0]
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	configPath := flags.String("config", "pool.json", "application config file")
	var opts initOptions
	if command == "init" {
		flags.StringVar(&opts.AccountsDir, "accounts-dir", "", "directory of Codex account homes (selects the Codex-home store)")
		flags.StringVar(&opts.CodexHome, "codex-home", "", "your own Codex home (default ~/.codex)")
		flags.StringVar(&opts.CodexBin, "codex-bin", "", "real codex executable")
		flags.StringVar(&opts.Listen, "listen", "", "listen address")
	}
	var accountArgs []string
	if command == "launch" {
		// Parsed before the generic flags so "--" and the Codex arguments
		// after it are never consumed here; runLaunch parses the rest.
		o, err := parseLaunchArgs(args[1:])
		if err != nil {
			return err
		}
		if o.config != "" {
			*configPath = o.config
		}
	} else if command == "account" {
		// --config may appear anywhere; the rest belongs to the subcommand.
		var err error
		if accountArgs, err = extractConfigFlag(args[1:], configPath); err != nil {
			return err
		}
	} else if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if command == "init" {
		return initialize(*configPath, out, opts)
	}
	cfg, err := pool.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	if cfg.UsesCodexHome() && (command == "login" || command == "import") {
		return fmt.Errorf("this pool reads Codex account directories; use: codex-pool account add NAME")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DisableCompression = true
	if cfg.ProxyURL != "" {
		u, err := url.Parse(cfg.ProxyURL)
		if err != nil {
			return err
		}
		if u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "socks5" && u.Scheme != "socks5h" {
			return fmt.Errorf("unsupported proxy scheme")
		}
		transport.Proxy = http.ProxyURL(u)
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	auth := cpa.NewCodexAuth(client)
	refresh := func(ctx context.Context, token string) (*cpa.CodexTokenData, error) {
		return auth.RefreshTokensWithRetry(ctx, token, 3)
	}
	var store pool.AccountStore
	var legacy *pool.Store
	if cfg.UsesCodexHome() {
		if store, err = pool.OpenCodexHomeStore(cfg.AccountsDir, refresh); err != nil {
			return err
		}
	} else {
		if legacy, err = pool.OpenStore(cfg.StateDir, refresh); err != nil {
			return err
		}
		store = legacy
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if command == "account" {
		home, ok := store.(*pool.CodexHomeStore)
		if !ok {
			return fmt.Errorf("account manages Codex account directories; set accounts_dir in the config (codex-pool init --accounts-dir DIR)")
		}
		statusFn := statusOrProbe(cfg, store, transport, 0)
		execLogin := func(dir string) error {
			if cfg.CodexBin == "" {
				return fmt.Errorf("codex_bin is not configured; set it or use --from PATH")
			}
			cmd := exec.CommandContext(ctx, cfg.CodexBin, "login")
			env := slices.DeleteFunc(os.Environ(), func(kv string) bool { return strings.HasPrefix(kv, "CODEX_HOME=") })
			cmd.Env = withLaunchedMarker(append(env, "CODEX_HOME="+dir))
			cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
			return cmd.Run()
		}
		return runAccount(ctx, cfg, home, statusFn, execLogin, accountArgs, out)
	}
	if command == "launch" {
		home, ok := store.(*pool.CodexHomeStore)
		if !ok {
			return fmt.Errorf("launch needs Codex account directories; set accounts_dir in the config (codex-pool init --accounts-dir DIR)")
		}
		deps := launchDeps{status: statusOrProbe(cfg, store, transport, 10*time.Second), exec: execCodex, stderr: os.Stderr}
		return runLaunch(ctx, cfg, home, deps, args[1:])
	}
	switch command {
	case "import":
		if len(flags.Args()) == 0 {
			return fmt.Errorf("provide CPA OAuth JSON files to import")
		}
		for _, path := range flags.Args() {
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			id, err := legacy.Import(b)
			if err != nil {
				return err
			}
			fmt.Fprintln(out, "imported", id)
		}
		return nil
	case "login":
		bundle, err := cpa.DeviceLogin(ctx, client, func(uri, code string) error {
			_, err := fmt.Fprintf(out, "Open %s\nDevice code: %s\n", uri, code)
			return err
		})
		if err != nil {
			return err
		}
		id, err := legacy.Login(pool.Credential{CodexTokenData: bundle.TokenData, Type: "codex", LastRefresh: bundle.LastRefresh})
		if err != nil {
			return err
		}
		fmt.Fprintln(out, "logged in", id)
		return nil
	case "enable", "disable":
		if len(flags.Args()) != 1 {
			return fmt.Errorf("provide one account ID from status (or a directory name with accounts_dir)")
		}
		id, err := accountID(store, cfg, flags.Arg(0))
		if err != nil {
			return err
		}
		if err := store.SetDisabled(id, command == "disable"); err != nil {
			return err
		}
		fmt.Fprintln(out, command, flags.Arg(0))
		return nil
	}
	key, err := readKey(cfg.StateDir, "client.key")
	if err != nil {
		return err
	}
	admin, err := readKey(cfg.StateDir, "admin.key")
	if err != nil {
		return err
	}
	h := pool.NewHandler(cfg, store, transport, key, admin)
	switch command {
	case "probe":
		if err = h.Poll(ctx); err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(h.Scheduler.Status())
	case "status":
		st, err := fetchStatus(ctx, cfg, admin)
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(st)
	case "serve":
		lock, err := os.OpenFile(filepath.Join(cfg.StateDir, "server.lock"), os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			return err
		}
		defer func() { _ = lock.Close() }()
		if err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
			return fmt.Errorf("another pool server is using this state directory")
		}
		defer func() { _ = unix.Flock(int(lock.Fd()), unix.LOCK_UN) }()
		if err = h.Poll(ctx); err != nil {
			return err
		}
		go h.PollLoop(ctx)
		server := &http.Server{Addr: cfg.Listen, Handler: h, BaseContext: func(net.Listener) context.Context { return ctx }}
		go func() {
			<-ctx.Done()
			shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = server.Shutdown(shutdown)
		}()
		fmt.Fprintf(out, "Codex account pool listening on %s; reserve %.2f%% of weekly quota\n", cfg.Listen, cfg.ReservePercent)
		if cfg.TLSCert != "" || cfg.TLSKey != "" {
			err = server.ListenAndServeTLS(cfg.TLSCert, cfg.TLSKey)
		} else {
			err = server.ListenAndServe()
		}
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	default:
		return fmt.Errorf("unknown command %q", command)
	}
}

// statusOrProbe returns a status source that asks the running pool server
// and, when none answers, probes the accounts directly (bounded by
// probeTimeout when it is positive).
func statusOrProbe(cfg pool.Config, store pool.AccountStore, transport http.RoundTripper, probeTimeout time.Duration) func(context.Context) ([]pool.AccountStatus, error) {
	return func(ctx context.Context) ([]pool.AccountStatus, error) {
		if admin, err := readKey(cfg.StateDir, "admin.key"); err == nil {
			if st, err := fetchStatus(ctx, cfg, admin); err == nil {
				return st, nil
			}
		}
		// No running server: probe the accounts directly.
		if probeTimeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, probeTimeout)
			defer cancel()
		}
		h := pool.NewHandler(cfg, store, transport, "", "")
		if err := h.Poll(ctx); err != nil {
			return nil, err
		}
		return h.Scheduler.Status(), nil
	}
}

// fetchStatus asks the running pool server for its account status.
func fetchStatus(ctx context.Context, cfg pool.Config, admin string) ([]pool.AccountStatus, error) {
	scheme := "http"
	if cfg.TLSCert != "" {
		scheme = "https"
	}
	host := cfg.Listen
	if address, port, err := net.SplitHostPort(host); err == nil && (address == "" || address == "0.0.0.0" || address == "::") {
		host = net.JoinHostPort("127.0.0.1", port)
	}
	r, err := http.NewRequestWithContext(ctx, "GET", scheme+"://"+host+"/_pool/status", nil)
	if err != nil {
		return nil, err
	}
	r.Header.Set("Authorization", "Bearer "+admin)
	statusClient := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := statusClient.Do(r)
	if err != nil {
		return nil, fmt.Errorf("server unavailable; use probe to fetch quota directly")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("status HTTP %d", resp.StatusCode)
	}
	var st []pool.AccountStatus
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return nil, fmt.Errorf("decode status: %w", err)
	}
	return st, nil
}

// extractConfigFlag removes --config PATH (or -config, --config=PATH) from
// args wherever it appears and returns the remaining arguments.
func extractConfigFlag(args []string, configPath *string) ([]string, error) {
	var rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--config" || a == "-config":
			if i+1 >= len(args) {
				return nil, fmt.Errorf("flag needs an argument: %s", a)
			}
			*configPath = args[i+1]
			i++
		case strings.HasPrefix(a, "--config=") || strings.HasPrefix(a, "-config="):
			*configPath = a[strings.IndexByte(a, '=')+1:]
		default:
			rest = append(rest, a)
		}
	}
	return rest, nil
}

// accountID resolves a Codex-home directory name to its account ID; any other
// argument (and every argument for the legacy store) is taken as an ID.
func accountID(store pool.AccountStore, cfg pool.Config, arg string) (string, error) {
	if !cfg.UsesCodexHome() || !pool.ValidAccountName(arg) {
		return arg, nil
	}
	list, err := store.List()
	if err != nil {
		return "", err
	}
	for _, c := range list {
		if c.Name == arg {
			return c.ID(), nil
		}
	}
	return arg, nil
}

func readKey(dir, name string) (string, error) {
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return "", err
	}
	key := strings.TrimSpace(string(b))
	if key == "" {
		return "", fmt.Errorf("empty %s", name)
	}
	return key, nil
}

type initOptions struct {
	AccountsDir, CodexHome, CodexBin, Listen string
}

func initialize(path string, out io.Writer, opts initOptions) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("config already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	cfg := pool.DefaultConfig()
	cfg.RoundRobin = nil // omitted so LoadConfig applies the built-in routes
	cfg.AccountsDir, cfg.CodexHome, cfg.CodexBin = opts.AccountsDir, opts.CodexHome, opts.CodexBin
	if opts.Listen != "" {
		cfg.Listen = opts.Listen
	}
	b, _ := json.MarshalIndent(cfg, "", "  ")
	// Validate the flag values before anything is written, so a bad flag
	// leaves no file behind.
	if _, err := pool.ParseConfig(b, filepath.Dir(path)); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	loaded, err := pool.LoadConfig(path)
	if err != nil {
		return err
	}
	if loaded.UsesCodexHome() {
		if err = os.MkdirAll(loaded.StateDir, 0700); err != nil {
			return err
		}
		if _, err = pool.OpenCodexHomeStore(loaded.AccountsDir, nil); err != nil {
			return err
		}
	} else if _, err = pool.OpenStore(loaded.StateDir, nil); err != nil {
		return err
	}
	for _, name := range []string{"client.key", "admin.key"} {
		p := filepath.Join(loaded.StateDir, name)
		if _, err = os.Stat(p); err == nil {
			continue
		}
		secret := make([]byte, 32)
		if _, err = rand.Read(secret); err != nil {
			return err
		}
		if err = pool.AtomicWrite(p, []byte(hex.EncodeToString(secret)+"\n")); err != nil {
			return err
		}
	}
	fmt.Fprintf(out, "Created %s and private application state at %s\n", path, loaded.StateDir)
	return nil
}
