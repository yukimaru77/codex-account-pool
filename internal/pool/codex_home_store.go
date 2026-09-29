package pool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"

	"codex-account-pool/internal/cpa"
)

// codexAuthFile is the auth.json written by the Codex CLI itself. Fields this
// program does not know about are kept in Extra so a rewrite never drops them.
type codexAuthFile struct {
	AuthMode    string                     `json:"auth_mode"`
	APIKey      json.RawMessage            `json:"OPENAI_API_KEY"`
	Tokens      *codexTokens               `json:"tokens"`
	LastRefresh string                     `json:"last_refresh,omitempty"`
	Extra       map[string]json.RawMessage `json:"-"`
}

type codexTokens struct {
	IDToken      string `json:"id_token"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	AccountID    string `json:"account_id"`
}

var codexAuthKnownKeys = []string{"auth_mode", "OPENAI_API_KEY", "tokens", "last_refresh"}

func (f *codexAuthFile) UnmarshalJSON(b []byte) error {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	var out codexAuthFile
	if raw, ok := m["auth_mode"]; ok {
		if err := json.Unmarshal(raw, &out.AuthMode); err != nil {
			return fmt.Errorf("auth_mode: %w", err)
		}
	}
	if raw, ok := m["OPENAI_API_KEY"]; ok {
		out.APIKey = append(json.RawMessage(nil), raw...)
	}
	if raw, ok := m["tokens"]; ok {
		if err := json.Unmarshal(raw, &out.Tokens); err != nil {
			return fmt.Errorf("tokens: %w", err)
		}
	}
	if raw, ok := m["last_refresh"]; ok {
		if err := json.Unmarshal(raw, &out.LastRefresh); err != nil {
			return fmt.Errorf("last_refresh: %w", err)
		}
	}
	for _, k := range codexAuthKnownKeys {
		delete(m, k)
	}
	if len(m) > 0 {
		out.Extra = m
	}
	*f = out
	return nil
}

func (f codexAuthFile) MarshalJSON() ([]byte, error) {
	m := make(map[string]any, len(f.Extra)+len(codexAuthKnownKeys))
	for k, v := range f.Extra {
		m[k] = v
	}
	m["auth_mode"] = f.AuthMode
	if len(f.APIKey) == 0 {
		m["OPENAI_API_KEY"] = nil
	} else {
		m["OPENAI_API_KEY"] = f.APIKey
	}
	m["tokens"] = f.Tokens
	if f.LastRefresh != "" {
		m["last_refresh"] = f.LastRefresh
	}
	return json.Marshal(m)
}

// CodexHomeStore reads accounts from per-account Codex home directories
// (Dir/<name>/auth.json), so the Codex CLI can run with CODEX_HOME=Dir/<name>
// and share the credential with the pool. Pool-owned state never goes into
// auth.json: disabling is a Dir/<name>/disabled marker file.
type CodexHomeStore struct {
	Dir     string
	Refresh func(context.Context, string) (*cpa.CodexTokenData, error)
	Logf    func(string, ...any)

	mu     sync.Mutex
	errors map[string]string
}

var _ AccountStore = (*CodexHomeStore)(nil)

var ErrDuplicateAccount = errors.New("account already present in another directory")

var accountNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

func ValidAccountName(name string) bool { return accountNameRE.MatchString(name) }

func OpenCodexHomeStore(dir string, refresh func(context.Context, string) (*cpa.CodexTokenData, error)) (*CodexHomeStore, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	return &CodexHomeStore{Dir: dir, Refresh: refresh}, nil
}

func (s *CodexHomeStore) Path(name string) string { return filepath.Join(s.Dir, name) }

func (s *CodexHomeStore) authPath(name string) string {
	return filepath.Join(s.Path(name), "auth.json")
}

func (s *CodexHomeStore) markerPath(name string) string {
	return filepath.Join(s.Path(name), "disabled")
}

func exists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

// candidates returns the sorted directory names under Dir that contain an
// auth.json, regardless of whether the name is valid.
func (s *CodexHomeStore) candidates() ([]string, error) {
	entries, err := os.ReadDir(s.Dir) // sorted by name
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if ok, _ := exists(s.authPath(e.Name())); ok {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

// Names returns the sorted, valid account directory names that hold an auth.json.
func (s *CodexHomeStore) Names() ([]string, error) {
	all, err := s.candidates()
	if err != nil {
		return nil, err
	}
	out := []string{}
	for _, name := range all {
		if ValidAccountName(name) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out, nil
}

// readCodexAuth parses a Codex auth.json into the raw file (for faithful
// rewrites) and the pool's Credential view. Name and Disabled are left unset.
func readCodexAuth(path string) (codexAuthFile, Credential, error) {
	var f codexAuthFile
	var c Credential
	b, err := os.ReadFile(path)
	if err != nil {
		return f, c, err
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return f, c, fmt.Errorf("parse auth.json: %w", err)
	}
	if f.Tokens == nil || f.Tokens.AccessToken == "" || f.Tokens.RefreshToken == "" {
		return f, c, fmt.Errorf("auth.json has no ChatGPT tokens (auth_mode %q)", f.AuthMode)
	}
	c.Type = "codex"
	c.IDToken = f.Tokens.IDToken
	c.AccessToken = f.Tokens.AccessToken
	c.RefreshToken = f.Tokens.RefreshToken
	c.AccountID = f.Tokens.AccountID
	c.LastRefresh = f.LastRefresh
	if claims, err := cpa.ParseJWTToken(f.Tokens.IDToken); err == nil {
		c.Email = claims.GetUserEmail()
		if claimID := claims.GetAccountID(); claimID != "" {
			if c.AccountID == "" {
				c.AccountID = claimID
			} else if c.AccountID != claimID {
				return f, c, fmt.Errorf("tokens.account_id does not match the ID token")
			}
		}
	}
	if c.AccountID == "" {
		return f, c, fmt.Errorf("auth.json has no account_id")
	}
	if claims, err := cpa.ParseJWTToken(f.Tokens.AccessToken); err == nil && claims.Exp > 0 {
		c.Expire = time.Unix(int64(claims.Exp), 0).UTC().Format(time.RFC3339)
	}
	return f, c, nil
}

func (s *CodexHomeStore) read(name string) (Credential, error) {
	if !ValidAccountName(name) {
		return Credential{}, fmt.Errorf("invalid account directory name")
	}
	_, c, err := readCodexAuth(s.authPath(name))
	if err != nil {
		return c, err
	}
	c.Name = name
	c.Disabled, err = exists(s.markerPath(name))
	return c, err
}

// List reads every account directory. A broken directory is skipped and
// reported through Errors so it never stops the other accounts.
func (s *CodexHomeStore) List() ([]Credential, error) {
	names, err := s.candidates()
	if err != nil {
		return nil, err
	}
	errs := map[string]string{}
	seen := map[string]string{} // account_id -> directory name
	var out []Credential
	for _, name := range names {
		c, err := s.read(name)
		if err == nil {
			if first, dup := seen[c.AccountID]; dup {
				err = fmt.Errorf("%w: %s", ErrDuplicateAccount, first)
			}
		}
		if err != nil {
			errs[name] = err.Error()
			if s.Logf != nil {
				s.Logf("account %s skipped: %v", name, err)
			}
			continue
		}
		seen[c.AccountID] = name
		out = append(out, c)
	}
	s.mu.Lock()
	s.errors = errs
	s.mu.Unlock()
	return out, nil
}

// Errors returns directory name -> read error from the most recent List.
func (s *CodexHomeStore) Errors() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]string, len(s.errors))
	for k, v := range s.errors {
		out[k] = v
	}
	return out
}

func (s *CodexHomeStore) nameOf(id string) (string, error) {
	if !validID(id) {
		return "", fmt.Errorf("invalid account identifier")
	}
	list, err := s.List()
	if err != nil {
		return "", err
	}
	for _, c := range list {
		if c.ID() == id {
			return c.Name, nil
		}
	}
	return "", fmt.Errorf("account not found")
}

// SetDisabled creates or removes the disabled marker; auth.json is untouched.
func (s *CodexHomeStore) SetDisabled(id string, disabled bool) error {
	name, err := s.nameOf(id)
	if err != nil {
		return err
	}
	marker := s.markerPath(name)
	if disabled {
		return AtomicWrite(marker, nil)
	}
	if err := os.Remove(marker); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Token is implemented in a follow-up change.
func (s *CodexHomeStore) Token(ctx context.Context, id string, force bool) (Credential, error) {
	return Credential{}, errors.New("not implemented")
}

// RefreshRejected is implemented in a follow-up change.
func (s *CodexHomeStore) RefreshRejected(ctx context.Context, id, accessToken string) (Credential, error) {
	return Credential{}, errors.New("not implemented")
}
