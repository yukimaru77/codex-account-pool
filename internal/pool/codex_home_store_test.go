package pool

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"codex-account-pool/internal/cpa"
)

func testJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	enc := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(b)
	}
	return enc(map[string]string{"alg": "none", "typ": "JWT"}) + "." + enc(claims) + "." + base64.RawURLEncoding.EncodeToString([]byte("sig"))
}

// writeAuth writes dir/name/auth.json in genuine Codex format, including an
// unknown top-level field so round-tripping can be checked.
func writeAuth(t *testing.T, dir, name, accountID, email string, exp time.Time, refresh string) string {
	t.Helper()
	claims := map[string]any{
		"exp":   exp.Unix(),
		"email": email,
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": accountID,
		},
	}
	auth := map[string]any{
		"auth_mode":      "chatgpt",
		"OPENAI_API_KEY": nil,
		"tokens": map[string]any{
			"id_token":      testJWT(t, claims),
			"access_token":  testJWT(t, claims),
			"refresh_token": refresh,
			"account_id":    accountID,
		},
		"last_refresh": "2026-09-01T00:00:00Z",
		"custom":       map[string]any{"x": 1},
	}
	b, err := json.MarshalIndent(auth, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(p, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p, "auth.json"), b, 0600); err != nil {
		t.Fatal(err)
	}
	return p
}

func openTestCodexHome(t *testing.T) (*CodexHomeStore, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := OpenCodexHomeStore(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	return s, dir
}

func TestCodexHomeListReadsCodexAuth(t *testing.T) {
	s, dir := openTestCodexHome(t)
	exp := time.Now().Add(time.Hour).Truncate(time.Second)
	writeAuth(t, dir, "alice", "acct-alice", "alice@example.com", exp, "rt-alice")

	list, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("got %d credentials, want 1: %+v", len(list), list)
	}
	c := list[0]
	if c.Name != "alice" || c.Email != "alice@example.com" || c.AccountID != "acct-alice" {
		t.Fatalf("unexpected credential identity: %+v", c)
	}
	if want := exp.UTC().Format(time.RFC3339); c.Expire != want {
		t.Fatalf("Expire = %q, want %q", c.Expire, want)
	}
	if c.Type != "codex" || c.Disabled || c.RefreshToken != "rt-alice" || c.LastRefresh != "2026-09-01T00:00:00Z" {
		t.Fatalf("unexpected credential fields: %+v", c)
	}
	if want := (Credential{CodexTokenData: c.CodexTokenData}).ID(); c.ID() != want || len(c.ID()) != 32 {
		t.Fatalf("unstable ID %q", c.ID())
	}
	again, err := s.List()
	if err != nil || len(again) != 1 || again[0].ID() != c.ID() {
		t.Fatalf("ID not stable across List calls: %v %+v", err, again)
	}
	if got := s.Path("alice"); got != filepath.Join(dir, "alice") {
		t.Fatalf("Path = %q", got)
	}
	names, err := s.Names()
	if err != nil || len(names) != 1 || names[0] != "alice" {
		t.Fatalf("Names = %v, %v", names, err)
	}
}

func TestCodexHomeListHonoursDisabledMarker(t *testing.T) {
	s, dir := openTestCodexHome(t)
	exp := time.Now().Add(time.Hour)
	writeAuth(t, dir, "alice", "acct-alice", "alice@example.com", exp, "rt-a")
	bob := writeAuth(t, dir, "bob", "acct-bob", "bob@example.com", exp, "rt-b")
	if err := os.WriteFile(filepath.Join(bob, "disabled"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	list, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("got %d credentials, want 2", len(list))
	}
	for _, c := range list {
		if want := c.Name == "bob"; c.Disabled != want {
			t.Fatalf("%s Disabled = %v, want %v", c.Name, c.Disabled, want)
		}
	}
}

func TestCodexHomeListSkipsBrokenAuth(t *testing.T) {
	s, dir := openTestCodexHome(t)
	writeAuth(t, dir, "alice", "acct-alice", "alice@example.com", time.Now().Add(time.Hour), "rt-a")
	for name, body := range map[string]string{
		"broken": "{",
		"apikey": `{"auth_mode":"apikey","OPENAI_API_KEY":"sk"}`,
	} {
		if err := os.MkdirAll(filepath.Join(dir, name), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name, "auth.json"), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	list, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Name != "alice" {
		t.Fatalf("want only alice, got %+v", list)
	}
	errs := s.Errors()
	for _, name := range []string{"broken", "apikey"} {
		if errs[name] == "" {
			t.Fatalf("Errors()[%q] empty; errors = %v", name, errs)
		}
	}
	if _, ok := errs["alice"]; ok {
		t.Fatalf("alice should have no error: %v", errs)
	}
}

func TestCodexHomeListRejectsDuplicateAccount(t *testing.T) {
	s, dir := openTestCodexHome(t)
	exp := time.Now().Add(time.Hour)
	writeAuth(t, dir, "first", "acct-same", "same@example.com", exp, "rt-1")
	writeAuth(t, dir, "second", "acct-same", "same@example.com", exp, "rt-2")
	list, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Name != "first" {
		t.Fatalf("want only first, got %+v", list)
	}
	if got := s.Errors()["second"]; !strings.Contains(got, ErrDuplicateAccount.Error()) {
		t.Fatalf("Errors()[second] = %q, want it to contain %q", got, ErrDuplicateAccount.Error())
	}
}

func TestCodexHomeListIgnoresDirsWithoutAuth(t *testing.T) {
	s, dir := openTestCodexHome(t)
	if err := os.MkdirAll(filepath.Join(dir, "empty"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "stray.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	list, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("want no credentials, got %+v", list)
	}
	if errs := s.Errors(); len(errs) != 0 {
		t.Fatalf("want no errors, got %v", errs)
	}
	names, err := s.Names()
	if err != nil || len(names) != 0 {
		t.Fatalf("Names = %v, %v", names, err)
	}
}

func TestValidAccountName(t *testing.T) {
	for _, name := range []string{"alice", "a-b_1", strings.Repeat("a", 64)} {
		if !ValidAccountName(name) {
			t.Errorf("ValidAccountName(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"", ".x", "a/b", "-a", "_a", "..", "a b", strings.Repeat("a", 65)} {
		if ValidAccountName(name) {
			t.Errorf("ValidAccountName(%q) = true, want false", name)
		}
	}
}

func TestCodexHomeSetDisabledCreatesAndRemovesMarker(t *testing.T) {
	s, dir := openTestCodexHome(t)
	p := writeAuth(t, dir, "alice", "acct-alice", "alice@example.com", time.Now().Add(time.Hour), "rt-a")
	before, err := os.ReadFile(filepath.Join(p, "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	list, err := s.List()
	if err != nil || len(list) != 1 {
		t.Fatalf("List = %+v, %v", list, err)
	}
	id := list[0].ID()
	marker := filepath.Join(p, "disabled")

	if err := s.SetDisabled(id, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("marker missing after disable: %v", err)
	}
	if list, _ := s.List(); len(list) != 1 || !list[0].Disabled {
		t.Fatalf("want disabled credential, got %+v", list)
	}
	if err := s.SetDisabled(id, true); err != nil {
		t.Fatalf("disable twice: %v", err)
	}
	if err := s.SetDisabled(id, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("marker still present after enable: %v", err)
	}
	if err := s.SetDisabled(id, false); err != nil {
		t.Fatalf("enable twice: %v", err)
	}
	after, err := os.ReadFile(filepath.Join(p, "auth.json"))
	if err != nil || string(after) != string(before) {
		t.Fatalf("auth.json must not be modified by SetDisabled")
	}
	if err := s.SetDisabled(strings.Repeat("0", 32), true); err == nil {
		t.Fatal("want error for unknown id")
	}
}

func TestCodexAuthFileRoundTripsUnknownFields(t *testing.T) {
	_, dir := openTestCodexHome(t)
	p := writeAuth(t, dir, "alice", "acct-alice", "alice@example.com", time.Now().Add(time.Hour), "rt-a")
	f, _, err := readCodexAuth(filepath.Join(p, "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if string(m["custom"]) != `{"x":1}` {
		t.Fatalf("custom = %s, want {\"x\":1}; full = %s", m["custom"], b)
	}
	if string(m["OPENAI_API_KEY"]) != "null" || string(m["auth_mode"]) != `"chatgpt"` {
		t.Fatalf("known fields not preserved: %s", b)
	}
	var back codexAuthFile
	if err := json.Unmarshal(b, &back); err != nil || back.Tokens == nil || back.Tokens.RefreshToken != "rt-a" {
		t.Fatalf("reparse failed: %v %+v", err, back)
	}
}

func codexClaims(accountID, email string, exp time.Time) map[string]any {
	return map[string]any{
		"exp":   exp.Unix(),
		"email": email,
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": accountID,
		},
	}
}

// refreshedTokens builds a refresh result whose tokens carry the given identity
// and expiry, tagged so tests can tell rotations apart.
func refreshedTokens(t *testing.T, accountID, tag string, exp time.Time) *cpa.CodexTokenData {
	t.Helper()
	claims := codexClaims(accountID, accountID+"@example.com", exp)
	claims["tag"] = tag
	return &cpa.CodexTokenData{
		IDToken:      testJWT(t, claims),
		AccessToken:  testJWT(t, claims),
		RefreshToken: "rt-" + tag,
		Expire:       exp.UTC().Format(time.RFC3339),
	}
}

func codexHomeID(t *testing.T, s *CodexHomeStore, name string) string {
	t.Helper()
	list, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range list {
		if c.Name == name {
			return c.ID()
		}
	}
	t.Fatalf("account %s not listed; errors = %v", name, s.Errors())
	return ""
}

func readAuthMap(t *testing.T, path string) (map[string]json.RawMessage, map[string]json.RawMessage) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var top, tokens map[string]json.RawMessage
	if err := json.Unmarshal(b, &top); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(top["tokens"], &tokens); err != nil {
		t.Fatal(err)
	}
	return top, tokens
}

// compactJSON strips insignificant whitespace; write-back reindents raw values.
func compactJSON(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var b bytes.Buffer
	if err := json.Compact(&b, raw); err != nil {
		t.Fatalf("invalid JSON %q: %v", raw, err)
	}
	return b.String()
}

func jsonString(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("not a JSON string: %s", raw)
	}
	return s
}

func TestCodexHomeTokenNoRefreshWhenValid(t *testing.T) {
	s, dir := openTestCodexHome(t)
	s.Refresh = func(context.Context, string) (*cpa.CodexTokenData, error) {
		t.Error("refresh called for a valid token")
		return nil, errors.New("unexpected")
	}
	p := writeAuth(t, dir, "alice", "acct-alice", "alice@example.com", time.Now().Add(time.Hour), "rt-a")
	before, err := os.ReadFile(filepath.Join(p, "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	id := codexHomeID(t, s, "alice")
	got, err := s.Token(context.Background(), id, false)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "alice" || got.RefreshToken != "rt-a" || got.ID() != id {
		t.Fatalf("unexpected credential: %+v", got)
	}
	after, err := os.ReadFile(filepath.Join(p, "auth.json"))
	if err != nil || string(after) != string(before) {
		t.Fatalf("auth.json modified without refresh: %v", err)
	}
}

func TestCodexHomeTokenRefreshesNearExpiryAndWritesBack(t *testing.T) {
	s, dir := openTestCodexHome(t)
	newExp := time.Now().Add(time.Hour).Truncate(time.Second)
	fresh := refreshedTokens(t, "acct-alice", "new", newExp)
	var gotRefresh []string
	s.Refresh = func(_ context.Context, rt string) (*cpa.CodexTokenData, error) {
		gotRefresh = append(gotRefresh, rt)
		return fresh, nil
	}
	p := writeAuth(t, dir, "alice", "acct-alice", "alice@example.com", time.Now().Add(30*time.Second), "rt-a")
	id := codexHomeID(t, s, "alice")

	got, err := s.Token(context.Background(), id, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(gotRefresh) != 1 || gotRefresh[0] != "rt-a" {
		t.Fatalf("refresh calls = %v, want [rt-a]", gotRefresh)
	}
	if got.AccessToken != fresh.AccessToken || got.RefreshToken != "rt-new" || got.IDToken != fresh.IDToken || got.Name != "alice" {
		t.Fatalf("returned credential not refreshed: %+v", got)
	}
	if want := newExp.UTC().Format(time.RFC3339); got.Expire != want {
		t.Fatalf("Expire = %q, want %q", got.Expire, want)
	}

	top, tokens := readAuthMap(t, filepath.Join(p, "auth.json"))
	if jsonString(t, tokens["access_token"]) != fresh.AccessToken ||
		jsonString(t, tokens["id_token"]) != fresh.IDToken ||
		jsonString(t, tokens["refresh_token"]) != "rt-new" ||
		jsonString(t, tokens["account_id"]) != "acct-alice" {
		t.Fatalf("tokens not written back: %v", tokens)
	}
	last := jsonString(t, top["last_refresh"])
	if last == "2026-09-01T00:00:00Z" {
		t.Fatal("last_refresh not updated")
	}
	if ts, err := time.Parse(time.RFC3339Nano, last); err != nil || time.Since(ts) > time.Minute || ts.Location() != time.UTC {
		t.Fatalf("last_refresh = %q (%v)", last, err)
	}
	if compactJSON(t, top["custom"]) != `{"x":1}` {
		t.Fatalf("custom field lost: %s", top["custom"])
	}
	if jsonString(t, top["auth_mode"]) != "chatgpt" || string(top["OPENAI_API_KEY"]) != "null" {
		t.Fatalf("auth_mode/OPENAI_API_KEY not preserved: %v", top)
	}
	for _, k := range []string{"disabled", "name", "expired", "type", "email"} {
		if _, ok := top[k]; ok {
			t.Fatalf("pool-owned field %q written into auth.json", k)
		}
	}
	// The written file is still readable as a genuine Codex auth.json.
	if again, err := s.Token(context.Background(), id, false); err != nil || again.AccessToken != fresh.AccessToken {
		t.Fatalf("reread after write-back: %v %+v", err, again)
	}
	if len(gotRefresh) != 1 {
		t.Fatalf("refreshed again: %v", gotRefresh)
	}
}

func TestCodexHomeTokenRereadsBeforeRefresh(t *testing.T) {
	s, dir := openTestCodexHome(t)
	var calls atomic.Int64
	var rotated *cpa.CodexTokenData
	var p string
	s.Refresh = func(context.Context, string) (*cpa.CodexTokenData, error) {
		calls.Add(1)
		// Simulate the Codex CLI rotating the credential on disk while the
		// pool's refresh is in flight.
		rotated = refreshedTokens(t, "acct-alice", "rotated", time.Now().Add(time.Hour))
		writeCodexTokens(t, p, rotated)
		return rotated, nil
	}
	p = writeAuth(t, dir, "alice", "acct-alice", "alice@example.com", time.Now().Add(30*time.Second), "rt-a")
	id := codexHomeID(t, s, "alice")

	first, err := s.Token(context.Background(), id, false)
	if err != nil {
		t.Fatal(err)
	}
	if first.AccessToken != rotated.AccessToken {
		t.Fatalf("first Token did not return rotated token")
	}
	second, err := s.Token(context.Background(), id, false)
	if err != nil {
		t.Fatal(err)
	}
	if second.AccessToken != rotated.AccessToken || second.RefreshToken != "rt-rotated" {
		t.Fatalf("second Token = %+v, want rotated", second)
	}
	if calls.Load() != 1 {
		t.Fatalf("refresh called %d times, want 1", calls.Load())
	}
	// An out-of-band rotation (Codex CLI) after the last read is picked up
	// without refreshing, because Token rereads auth.json every time.
	external := refreshedTokens(t, "acct-alice", "external", time.Now().Add(time.Hour))
	writeCodexTokens(t, p, external)
	third, err := s.Token(context.Background(), id, false)
	if err != nil {
		t.Fatal(err)
	}
	if third.AccessToken != external.AccessToken || third.RefreshToken != "rt-external" || calls.Load() != 1 {
		t.Fatalf("third Token = %+v (calls %d), want external rotation", third, calls.Load())
	}
}

// writeCodexTokens rewrites dir/auth.json in Codex format with the given tokens,
// keeping the helper's other fields.
func writeCodexTokens(t *testing.T, dir string, tok *cpa.CodexTokenData) {
	t.Helper()
	path := filepath.Join(dir, "auth.json")
	top, tokens := readAuthMap(t, path)
	set := func(k, v string) {
		b, _ := json.Marshal(v)
		tokens[k] = b
	}
	set("id_token", tok.IDToken)
	set("access_token", tok.AccessToken)
	set("refresh_token", tok.RefreshToken)
	tb, _ := json.Marshal(tokens)
	top["tokens"] = tb
	b, _ := json.MarshalIndent(top, "", "  ")
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestCodexHomeRefreshRejectedSkipsWhenTokenChanged(t *testing.T) {
	s, dir := openTestCodexHome(t)
	s.Refresh = func(context.Context, string) (*cpa.CodexTokenData, error) {
		t.Error("refresh called although the rejected token was already rotated")
		return nil, errors.New("unexpected")
	}
	p := writeAuth(t, dir, "alice", "acct-alice", "alice@example.com", time.Now().Add(time.Hour), "rt-a")
	id := codexHomeID(t, s, "alice")
	newer := refreshedTokens(t, "acct-alice", "newer", time.Now().Add(time.Hour))
	writeCodexTokens(t, p, newer)

	got, err := s.RefreshRejected(context.Background(), id, "old-access-token")
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessToken != newer.AccessToken || got.RefreshToken != "rt-newer" {
		t.Fatalf("got %+v, want newer token", got)
	}
}

func TestCodexHomeRefreshRejectsIdentityChange(t *testing.T) {
	for name, result := range map[string]func(t *testing.T) *cpa.CodexTokenData{
		"id_token claim": func(t *testing.T) *cpa.CodexTokenData {
			return refreshedTokens(t, "acct-mallory", "other", time.Now().Add(time.Hour))
		},
		"AccountID field": func(t *testing.T) *cpa.CodexTokenData {
			r := refreshedTokens(t, "acct-alice", "other", time.Now().Add(time.Hour))
			r.AccountID = "acct-mallory"
			return r
		},
		"unparseable id_token": func(t *testing.T) *cpa.CodexTokenData {
			r := refreshedTokens(t, "acct-alice", "other", time.Now().Add(time.Hour))
			r.IDToken = "not-a-jwt"
			return r
		},
	} {
		t.Run(name, func(t *testing.T) {
			s, dir := openTestCodexHome(t)
			s.Refresh = func(context.Context, string) (*cpa.CodexTokenData, error) { return result(t), nil }
			p := writeAuth(t, dir, "alice", "acct-alice", "alice@example.com", time.Now().Add(time.Hour), "rt-a")
			before, err := os.ReadFile(filepath.Join(p, "auth.json"))
			if err != nil {
				t.Fatal(err)
			}
			id := codexHomeID(t, s, "alice")
			if _, err := s.Token(context.Background(), id, true); err == nil {
				t.Fatal("want identity-change error")
			}
			after, err := os.ReadFile(filepath.Join(p, "auth.json"))
			if err != nil || string(after) != string(before) {
				t.Fatalf("auth.json modified on identity change: %v", err)
			}
		})
	}
}

func TestCodexHomeTokenConcurrentRefreshOnce(t *testing.T) {
	var calls atomic.Int64
	fresh := refreshedTokens(t, "acct-alice", "new", time.Now().Add(time.Hour))
	refresh := func(context.Context, string) (*cpa.CodexTokenData, error) {
		calls.Add(1)
		return fresh, nil
	}
	s, dir := openTestCodexHome(t)
	s.Refresh = refresh
	s2, err := OpenCodexHomeStore(dir, refresh)
	if err != nil {
		t.Fatal(err)
	}
	writeAuth(t, dir, "alice", "acct-alice", "alice@example.com", time.Now().Add(time.Hour), "rt-a")
	old, err := s.List()
	if err != nil || len(old) != 1 {
		t.Fatalf("List = %+v, %v", old, err)
	}
	id, oldAccess := old[0].ID(), old[0].AccessToken

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			store := s
			if i%2 == 0 {
				store = s2
			}
			got, err := store.RefreshRejected(context.Background(), id, oldAccess)
			if err != nil || got.AccessToken != fresh.AccessToken {
				t.Errorf("refresh result: %v", err)
			}
		}(i)
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("rotated %d times for the same rejected token", calls.Load())
	}
	if _, err := s2.RefreshRejected(context.Background(), id, oldAccess); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatal("delayed rejection refreshed the already-rotated token")
	}
}

func TestCodexHomeTokenPreservesUnknownTokenFields(t *testing.T) {
	s, dir := openTestCodexHome(t)
	fresh := refreshedTokens(t, "acct-alice", "new", time.Now().Add(time.Hour))
	s.Refresh = func(context.Context, string) (*cpa.CodexTokenData, error) { return fresh, nil }
	p := writeAuth(t, dir, "alice", "acct-alice", "alice@example.com", time.Now().Add(30*time.Second), "rt-a")
	path := filepath.Join(p, "auth.json")
	top, tokens := readAuthMap(t, path)
	tokens["future_field"] = json.RawMessage(`{"nested":[1,2]}`)
	tb, _ := json.Marshal(tokens)
	top["tokens"] = tb
	b, _ := json.Marshal(top)
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	id := codexHomeID(t, s, "alice")
	if _, err := s.Token(context.Background(), id, false); err != nil {
		t.Fatal(err)
	}
	_, tokens = readAuthMap(t, path)
	if compactJSON(t, tokens["future_field"]) != `{"nested":[1,2]}` {
		t.Fatalf("unknown tokens field lost: %s", tokens["future_field"])
	}
	if jsonString(t, tokens["access_token"]) != fresh.AccessToken {
		t.Fatal("access_token not written back")
	}
}

func TestCodexHomeListRejectsAccountIDMismatch(t *testing.T) {
	s, dir := openTestCodexHome(t)
	writeAuth(t, dir, "alice", "acct-alice", "alice@example.com", time.Now().Add(time.Hour), "rt-a")
	p := writeAuth(t, dir, "bob", "acct-bob", "bob@example.com", time.Now().Add(time.Hour), "rt-b")
	path := filepath.Join(p, "auth.json")
	top, tokens := readAuthMap(t, path)
	tokens["account_id"] = json.RawMessage(`"acct-somebody-else"`)
	tb, _ := json.Marshal(tokens)
	top["tokens"] = tb
	b, _ := json.Marshal(top)
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	list, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Name != "alice" {
		t.Fatalf("want only alice, got %+v", list)
	}
	if got := s.Errors()["bob"]; !strings.Contains(got, "account_id") {
		t.Fatalf("Errors()[bob] = %q, want account_id mismatch", got)
	}
}

// rewriteAuth applies edit to the decoded top-level and tokens maps of
// dir/auth.json and writes it back compactly.
func rewriteAuth(t *testing.T, dir string, edit func(top, tokens map[string]json.RawMessage)) {
	t.Helper()
	path := filepath.Join(dir, "auth.json")
	top, tokens := readAuthMap(t, path)
	edit(top, tokens)
	tb, err := json.Marshal(tokens)
	if err != nil {
		t.Fatal(err)
	}
	top["tokens"] = tb
	b, err := json.Marshal(top)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestCodexHomeTokenRewriteKeepsAbsentKeysAbsent(t *testing.T) {
	s, dir := openTestCodexHome(t)
	fresh := refreshedTokens(t, "acct-alice", "new", time.Now().Add(time.Hour))
	s.Refresh = func(context.Context, string) (*cpa.CodexTokenData, error) { return fresh, nil }
	p := writeAuth(t, dir, "alice", "acct-alice", "alice@example.com", time.Now().Add(30*time.Second), "rt-a")
	// Legacy shape: no auth_mode and no OPENAI_API_KEY.
	rewriteAuth(t, p, func(top, _ map[string]json.RawMessage) {
		delete(top, "auth_mode")
		delete(top, "OPENAI_API_KEY")
	})
	id := codexHomeID(t, s, "alice")
	got, err := s.Token(context.Background(), id, false)
	if err != nil || got.AccessToken != fresh.AccessToken {
		t.Fatalf("Token = %+v, %v", got, err)
	}
	top, _ := readAuthMap(t, filepath.Join(p, "auth.json"))
	for _, k := range []string{"auth_mode", "OPENAI_API_KEY"} {
		if raw, ok := top[k]; ok {
			t.Fatalf("rewrite added absent key %q = %s", k, raw)
		}
	}
	if compactJSON(t, top["custom"]) != `{"x":1}` {
		t.Fatalf("custom field lost: %s", top["custom"])
	}
}

func TestCodexHomeTokenRefreshesOpaqueAccessToken(t *testing.T) {
	s, dir := openTestCodexHome(t)
	var calls atomic.Int64
	fresh := refreshedTokens(t, "acct-alice", "new", time.Now().Add(time.Hour))
	s.Refresh = func(context.Context, string) (*cpa.CodexTokenData, error) {
		calls.Add(1)
		return fresh, nil
	}
	p := writeAuth(t, dir, "alice", "acct-alice", "alice@example.com", time.Now().Add(time.Hour), "rt-a")
	rewriteAuth(t, p, func(_, tokens map[string]json.RawMessage) {
		tokens["access_token"] = json.RawMessage(`"opaque-access-token"`)
	})
	id := codexHomeID(t, s, "alice")
	got, err := s.Token(context.Background(), id, false)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || got.AccessToken != fresh.AccessToken {
		t.Fatalf("unparseable access token must be refreshed: calls %d, got %+v", calls.Load(), got)
	}
}

func TestCodexHomeTokenRescansWhenAccountMoves(t *testing.T) {
	s, dir := openTestCodexHome(t)
	s.Refresh = func(context.Context, string) (*cpa.CodexTokenData, error) {
		t.Error("refresh called for a valid token")
		return nil, errors.New("unexpected")
	}
	writeAuth(t, dir, "alice", "acct-alice", "alice@example.com", time.Now().Add(time.Hour), "rt-a")
	id := codexHomeID(t, s, "alice") // caches id -> alice
	if err := os.Rename(filepath.Join(dir, "alice"), filepath.Join(dir, "alice2")); err != nil {
		t.Fatal(err)
	}
	got, err := s.Token(context.Background(), id, false)
	if err != nil || got.Name != "alice2" {
		t.Fatalf("Token after move = %+v, %v", got, err)
	}
	if err := os.RemoveAll(filepath.Join(dir, "alice2")); err != nil {
		t.Fatal(err)
	}
	_, err = s.Token(context.Background(), id, false)
	if err == nil || errors.Is(err, errAccountMoved) || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("Token after removal err = %v, want account not found", err)
	}
}

func TestReadCodexAuthRetriesTornWrite(t *testing.T) {
	_, dir := openTestCodexHome(t)
	p := writeAuth(t, dir, "torn", "acct-torn", "torn@example.com", time.Now().Add(time.Hour), "refresh-torn")
	path := filepath.Join(p, "auth.json")
	full, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, partial := range [][]byte{nil, full[:len(full)/2]} {
		// Codex truncates and rewrites in place; the reader may catch it mid-write.
		if err := os.WriteFile(path, partial, 0600); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() {
			time.Sleep(60 * time.Millisecond)
			done <- os.WriteFile(path, full, 0600)
		}()
		_, c, err := readCodexAuth(path)
		if werr := <-done; werr != nil {
			t.Fatal(werr)
		}
		if err != nil {
			t.Fatalf("partial %d bytes: %v", len(partial), err)
		}
		if c.AccountID != "acct-torn" {
			t.Fatalf("account = %q", c.AccountID)
		}
	}
	// A file that stays broken still fails.
	if err := os.WriteFile(path, []byte("{not json"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readCodexAuth(path); err == nil || !strings.Contains(err.Error(), "parse auth.json") {
		t.Fatalf("persistently broken file: %v", err)
	}
}
