package pool

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"codex-account-pool/internal/cpa"
	"golang.org/x/sys/unix"
)

// Credential is CPA-compatible, but is stored only in this application's own
// directory. No Codex/CPA credential file is written after an explicit import.
type Credential struct {
	cpa.CodexTokenData
	Type        string `json:"type"`
	Disabled    bool   `json:"disabled,omitempty"`
	LastRefresh string `json:"last_refresh,omitempty"`
}

func (c Credential) ID() string {
	h := sha256.Sum256([]byte(c.AccountID))
	return hex.EncodeToString(h[:16])
}

type Store struct {
	Dir     string
	Refresh func(context.Context, string) (*cpa.CodexTokenData, error)
}

func OpenStore(dir string, refresh func(context.Context, string) (*cpa.CodexTokenData, error)) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(dir, "accounts"), 0700); err != nil {
		return nil, err
	}
	return &Store{Dir: dir, Refresh: refresh}, nil
}

func AtomicWrite(path string, b []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".write-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}

func withLock(path string, fn func() error) error {
	return withContextLock(context.Background(), path, fn)
}

func withContextLock(ctx context.Context, path string, fn func() error) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if err != unix.EWOULDBLOCK && err != unix.EAGAIN {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	defer func() { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN) }()
	return fn()
}

func (s *Store) path(id string) string { return filepath.Join(s.Dir, "accounts", id+".json") }
func validID(id string) bool {
	if len(id) != 32 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}
func (s *Store) read(id string) (Credential, error) {
	var c Credential
	if !validID(id) {
		return c, fmt.Errorf("invalid account identifier")
	}
	b, err := os.ReadFile(s.path(id))
	if err != nil {
		return c, err
	}
	err = json.Unmarshal(b, &c)
	if err == nil && (c.AccountID == "" || c.ID() != id || c.Type != "codex") {
		err = fmt.Errorf("invalid stored account identity")
	}
	return c, err
}
func (s *Store) save(c Credential) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return AtomicWrite(s.path(c.ID()), append(b, '\n'))
}

func (s *Store) Import(b []byte) (string, error) {
	var c Credential
	if err := json.Unmarshal(b, &c); err != nil {
		return "", err
	}
	if c.Type != "codex" || c.AccountID == "" || c.AccessToken == "" || c.RefreshToken == "" {
		return "", fmt.Errorf("expected CPA Codex OAuth JSON with account_id, access_token and refresh_token")
	}
	if claims, err := cpa.ParseJWTToken(c.IDToken); err == nil && claims.GetAccountID() != "" && claims.GetAccountID() != c.AccountID {
		return "", fmt.Errorf("account_id does not match the ID token")
	}
	id := c.ID()
	err := withLock(s.path(id)+".lock", func() error {
		// Import is add-only: never roll a rotated refresh token back to an old copy.
		if _, err := os.Stat(s.path(id)); err == nil {
			return fmt.Errorf("account already exists; use login to replace credentials")
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return s.save(c)
	})
	return id, err
}

func (s *Store) Login(c Credential) (string, error) {
	if c.AccountID == "" || c.AccessToken == "" || c.RefreshToken == "" {
		return "", fmt.Errorf("incomplete OAuth result")
	}
	c.Type = "codex"
	id := c.ID()
	return id, withLock(s.path(id)+".lock", func() error { return s.save(c) })
}

func (s *Store) List() ([]Credential, error) {
	entries, err := os.ReadDir(filepath.Join(s.Dir, "accounts"))
	if err != nil {
		return nil, err
	}
	var out []Credential
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		c, err := s.read(strings.TrimSuffix(entry.Name(), ".json"))
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

func (s *Store) SetDisabled(id string, disabled bool) error {
	if !validID(id) {
		return fmt.Errorf("invalid account identifier")
	}
	return withLock(s.path(id)+".lock", func() error {
		c, err := s.read(id)
		if err != nil {
			return err
		}
		c.Disabled = disabled
		return s.save(c)
	})
}

// Token serializes read/refresh/persist across requests and processes. It rereads
// the credential under the lock so a rotated refresh token is never reused.
func (s *Store) Token(ctx context.Context, id string, force bool) (Credential, error) {
	return s.token(ctx, id, force, "")
}

// RefreshRejected refreshes only the credential actually rejected by the quota
// probe. Another request/process may already have rotated it while we waited.
func (s *Store) RefreshRejected(ctx context.Context, id, accessToken string) (Credential, error) {
	return s.token(ctx, id, true, accessToken)
}

func (s *Store) token(ctx context.Context, id string, force bool, rejected string) (Credential, error) {
	var current Credential
	if !validID(id) {
		return current, fmt.Errorf("invalid account identifier")
	}
	err := withContextLock(ctx, s.path(id)+".lock", func() error {
		var err error
		current, err = s.read(id)
		if err != nil {
			return err
		}
		if current.Disabled {
			return fmt.Errorf("account is disabled")
		}
		if rejected != "" && current.AccessToken != rejected {
			force = false
		}
		exp, err := time.Parse(time.RFC3339, current.Expire)
		if !force && err == nil && exp.After(time.Now().Add(time.Minute)) {
			return nil
		}
		if s.Refresh == nil {
			return fmt.Errorf("refresh unavailable")
		}
		updated, err := s.Refresh(ctx, current.RefreshToken)
		if err != nil {
			return err
		}
		if updated == nil || updated.AccessToken == "" {
			return fmt.Errorf("refresh returned no token")
		}
		if updated.AccountID != "" && updated.AccountID != current.AccountID {
			return fmt.Errorf("refresh changed the account identity")
		}
		current.AccessToken = updated.AccessToken
		current.Expire = updated.Expire
		if updated.RefreshToken != "" {
			current.RefreshToken = updated.RefreshToken
		}
		if updated.IDToken != "" {
			current.IDToken = updated.IDToken
		}
		if updated.Email != "" {
			current.Email = updated.Email
		}
		current.LastRefresh = time.Now().UTC().Format(time.RFC3339)
		return s.save(current)
	})
	return current, err
}
