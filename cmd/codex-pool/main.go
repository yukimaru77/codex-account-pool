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
	"os/signal"
	"path/filepath"
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
		return fmt.Errorf("usage: codex-pool {init|login|import|serve|status|probe|enable|disable} --config pool.json [files or account ID]")
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
	if err := flags.Parse(args[1:]); err != nil {
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
			return err
		}
		r.Header.Set("Authorization", "Bearer "+admin)
		statusClient := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		resp, err := statusClient.Do(r)
		if err != nil {
			return fmt.Errorf("server unavailable; use probe to fetch quota directly")
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != 200 {
			return fmt.Errorf("status HTTP %d", resp.StatusCode)
		}
		_, err = io.Copy(out, resp.Body)
		return err
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
