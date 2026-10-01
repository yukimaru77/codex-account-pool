package pool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"codex-account-pool/internal/cpa"
)

// codexAuthFile is the auth.json written by the Codex CLI itself. Fields this
// program does not know about are kept in Extra so a rewrite never drops them.
type codexAuthFile struct {
	AuthMode    string                     `json:"auth_mode"`
	HasAuthMode bool                       `json:"-"` // auth_mode was present when read
	APIKey      json.RawMessage            `json:"OPENAI_API_KEY"`
	Tokens      *codexTokens               `json:"tokens"`
	LastRefresh string                     `json:"last_refresh,omitempty"`
	Extra       map[string]json.RawMessage `json:"-"`
}

type codexTokens struct {
	IDToken      string                     `json:"id_token"`
	AccessToken  string                     `json:"access_token"`
	RefreshToken string                     `json:"refresh_token"`
	AccountID    string                     `json:"account_id"`
	Extra        map[string]json.RawMessage `json:"-"`
}

var codexTokensKnownKeys = []string{"id_token", "access_token", "refresh_token", "account_id"}

func (t *codexTokens) UnmarshalJSON(b []byte) error {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	var out codexTokens
	for k, dst := range map[string]*string{
		"id_token":      &out.IDToken,
		"access_token":  &out.AccessToken,
		"refresh_token": &out.RefreshToken,
		"account_id":    &out.AccountID,
	} {
		raw, ok := m[k]
		if !ok || string(raw) == "null" {
			continue
		}
		if err := json.Unmarshal(raw, dst); err != nil {
			return fmt.Errorf("%s: %w", k, err)
		}
	}
	for _, k := range codexTokensKnownKeys {
		delete(m, k)
	}
	if len(m) > 0 {
		out.Extra = m
	}
	*t = out
	return nil
}

func (t codexTokens) MarshalJSON() ([]byte, error) {
	m := make(map[string]any, len(t.Extra)+len(codexTokensKnownKeys))
	for k, v := range t.Extra {
		m[k] = v
	}
	m["id_token"] = t.IDToken
	m["access_token"] = t.AccessToken
	m["refresh_token"] = t.RefreshToken
	m["account_id"] = t.AccountID
	return json.Marshal(m)
}

var codexAuthKnownKeys = []string{"auth_mode", "OPENAI_API_KEY", "tokens", "last_refresh"}

func (f *codexAuthFile) UnmarshalJSON(b []byte) error {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	var out codexAuthFile
	if raw, ok := m["auth_mode"]; ok {
		out.HasAuthMode = true
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
	// Keys absent from the original file stay absent, so a rewrite only ever
	// changes tokens.* and last_refresh.
	if f.HasAuthMode || f.AuthMode != "" {
		m["auth_mode"] = f.AuthMode
	}
	if len(f.APIKey) > 0 { // RawMessage keeps an explicit null as "null"
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
	names  map[string]string // Credential.ID() -> directory name
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

// errTornAuth marks an auth.json that is empty or not valid JSON, which is
// what a reader sees while Codex truncates and rewrites the file in place.
var errTornAuth = errors.New("parse auth.json")

// Codex overwrites auth.json in place (not atomically), so a read can catch
// it half written; such a read is retried a few times after a short wait.
const (
	tornAuthRetries = 3
	tornAuthWait    = 50 * time.Millisecond
)

// readCodexAuth parses a Codex auth.json into the raw file (for faithful
// rewrites) and the pool's Credential view. Name and Disabled are left unset.
func readCodexAuth(path string) (codexAuthFile, Credential, error) {
	for attempt := 0; ; attempt++ {
		f, c, err := readCodexAuthOnce(path)
		if !errors.Is(err, errTornAuth) || attempt >= tornAuthRetries {
			return f, c, err
		}
		time.Sleep(tornAuthWait)
	}
}

func readCodexAuthOnce(path string) (codexAuthFile, Credential, error) {
	var f codexAuthFile
	var c Credential
	b, err := os.ReadFile(path)
	if err != nil {
		return f, c, err
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return f, c, fmt.Errorf("%w: empty file", errTornAuth)
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return f, c, fmt.Errorf("%w: %w", errTornAuth, err)
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

// Validate reads Dir/<name>/auth.json the way List does and returns the
// credential it holds.
func (s *CodexHomeStore) Validate(name string) (Credential, error) {
	return s.read(name)
}

// ReadCodexAuthFile validates a Codex auth.json anywhere on disk and returns
// the credential it holds (Name and Disabled unset).
func ReadCodexAuthFile(path string) (Credential, error) {
	_, c, err := readCodexAuth(path)
	return c, err
}

// SharedExclude lists the Codex home entries that stay per account.
var SharedExclude = map[string]bool{"auth.json": true, "models_cache.json": true, "log": true, "tmp": true,
	"app-server-control": true, "app-server-daemon": true}

// perAccountDirs are SharedExclude entries Codex expects to be real
// directories: Codex 0.159 refuses to start its background server when they
// are symlinks. Directories created before they were excluded may still hold
// a symlink into codexHome; LinkShared replaces such a symlink with an empty
// private directory.
var perAccountDirs = map[string]bool{"app-server-control": true, "app-server-daemon": true}

// LinkShared symlinks every top-level entry of codexHome (except SharedExclude
// and names starting with ".write-") into dir, replacing existing symlinks,
// never touching regular files/dirs already in dir. Returns linked names.
func LinkShared(dir, codexHome string) ([]string, error) {
	home, err := filepath.Abs(codexHome)
	if err != nil {
		return nil, err
	}
	target, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(home)
	if err != nil {
		return nil, err
	}
	linked := []string{}
	for name := range perAccountDirs {
		dst := filepath.Join(target, name)
		if info, err := os.Lstat(dst); err == nil && info.Mode()&os.ModeSymlink != 0 {
			if err := os.Remove(dst); err != nil {
				return linked, err
			}
			if err := os.Mkdir(dst, 0o700); err != nil {
				return linked, err
			}
		}
	}
	for _, e := range entries {
		name := e.Name()
		if SharedExclude[name] || strings.HasPrefix(name, ".write-") {
			continue
		}
		src := filepath.Join(home, name)
		// Never link an entry that contains the account directory itself
		// (accounts_dir inside codex_home), which would make a loop.
		if rel, err := filepath.Rel(src, target); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		dst := filepath.Join(target, name)
		info, err := os.Lstat(dst)
		switch {
		case err == nil && info.Mode()&os.ModeSymlink == 0:
			continue // the account's own file or directory
		case err == nil:
			if err := os.Remove(dst); err != nil {
				return linked, err
			}
		case !errors.Is(err, os.ErrNotExist):
			return linked, err
		}
		if err := os.Symlink(src, dst); err != nil {
			return linked, err
		}
		linked = append(linked, name)
	}
	return linked, nil
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
	ids := make(map[string]string, len(out))
	for _, c := range out {
		ids[c.ID()] = c.Name
	}
	s.mu.Lock()
	s.errors = errs
	s.names = ids
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

func (s *CodexHomeStore) lockPath(name string) string {
	return filepath.Join(s.Path(name), ".pool.lock")
}

// cachedName returns the directory name remembered for id by the last List.
func (s *CodexHomeStore) cachedName(id string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	name, ok := s.names[id]
	return name, ok
}

// Token serializes read/refresh/persist across requests and processes
// (including the Codex CLI's own writes, which it detects by rereading
// auth.json under the lock) so a rotated refresh token is never reused.
func (s *CodexHomeStore) Token(ctx context.Context, id string, force bool) (Credential, error) {
	return s.token(ctx, id, force, "")
}

// RefreshRejected refreshes only the credential actually rejected upstream.
// Another request, process or the Codex CLI may already have rotated it.
func (s *CodexHomeStore) RefreshRejected(ctx context.Context, id, accessToken string) (Credential, error) {
	return s.token(ctx, id, true, accessToken)
}

// errAccountMoved means the directory no longer holds the account the name
// cache pointed at; the caller rescans and retries once.
var errAccountMoved = errors.New("account directory changed")

func (s *CodexHomeStore) token(ctx context.Context, id string, force bool, rejected string) (Credential, error) {
	if !validID(id) {
		return Credential{}, fmt.Errorf("invalid account identifier")
	}
	name, cached := s.cachedName(id)
	for attempt := 0; ; attempt++ {
		if !cached {
			var err error
			if name, err = s.nameOf(id); err != nil {
				return Credential{}, err
			}
		}
		c, err := s.tokenIn(ctx, name, id, force, rejected)
		if !errors.Is(err, errAccountMoved) {
			return c, err
		}
		if attempt > 0 {
			return Credential{}, fmt.Errorf("account not found")
		}
		cached = false // rescan the directories and retry once
	}
}

func (s *CodexHomeStore) tokenIn(ctx context.Context, name, id string, force bool, rejected string) (Credential, error) {
	var current Credential
	err := withContextLock(ctx, s.lockPath(name), func() error {
		file, c, err := readCodexAuth(s.authPath(name))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return errAccountMoved
			}
			return err
		}
		if c.ID() != id {
			return errAccountMoved
		}
		c.Name = name
		if c.Disabled, err = exists(s.markerPath(name)); err != nil {
			return err
		}
		current = c
		if current.Disabled {
			return fmt.Errorf("account is disabled")
		}
		if rejected != "" && current.AccessToken != rejected {
			force = false
		}
		// Expire comes from the access token; an unparseable token has none
		// and is treated as expired.
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
		next := current
		next.AccessToken = updated.AccessToken
		if updated.RefreshToken != "" {
			next.RefreshToken = updated.RefreshToken
		}
		if updated.IDToken != "" {
			claims, err := cpa.ParseJWTToken(updated.IDToken)
			if err != nil {
				// Codex parses id_token itself; never persist one it cannot read.
				return fmt.Errorf("refresh returned an unparseable id_token: %w", err)
			}
			if claimID := claims.GetAccountID(); claimID != "" && claimID != current.AccountID {
				return fmt.Errorf("refresh changed the account identity")
			}
			if email := claims.GetUserEmail(); email != "" {
				next.Email = email
			}
			next.IDToken = updated.IDToken
		}
		if updated.Email != "" {
			next.Email = updated.Email
		}
		next.Expire = updated.Expire
		if claims, err := cpa.ParseJWTToken(next.AccessToken); err == nil && claims.Exp > 0 {
			next.Expire = time.Unix(int64(claims.Exp), 0).UTC().Format(time.RFC3339)
		}
		next.LastRefresh = time.Now().UTC().Format(time.RFC3339Nano)

		tokens := *file.Tokens
		tokens.IDToken = next.IDToken
		tokens.AccessToken = next.AccessToken
		tokens.RefreshToken = next.RefreshToken
		tokens.AccountID = next.AccountID
		file.Tokens = &tokens
		file.LastRefresh = next.LastRefresh
		b, err := json.MarshalIndent(file, "", "  ")
		if err != nil {
			return err
		}
		if err := AtomicWrite(s.authPath(name), b); err != nil {
			return err
		}
		current = next
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		// The directory vanished (e.g. renamed) before the lock could be taken.
		return Credential{}, errAccountMoved
	}
	return current, err
}
