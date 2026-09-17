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
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if command == "init" {
		return initialize(*configPath, out)
	}
	cfg, err := pool.LoadConfig(*configPath)
	if err != nil {
		return err
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
	store, err := pool.OpenStore(cfg.StateDir, func(ctx context.Context, token string) (*cpa.CodexTokenData, error) {
		return auth.RefreshTokensWithRetry(ctx, token, 3)
	})
	if err != nil {
		return err
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
			id, err := store.Import(b)
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
		id, err := store.Login(pool.Credential{CodexTokenData: bundle.TokenData, Type: "codex", LastRefresh: bundle.LastRefresh})
		if err != nil {
			return err
		}
		fmt.Fprintln(out, "logged in", id)
		return nil
	case "enable", "disable":
		if len(flags.Args()) != 1 {
			return fmt.Errorf("provide one account ID from status")
		}
		if err := store.SetDisabled(flags.Arg(0), command == "disable"); err != nil {
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

func initialize(path string, out io.Writer) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("config already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	cfg := pool.DefaultConfig()
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
	if _, err = pool.OpenStore(loaded.StateDir, nil); err != nil {
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
