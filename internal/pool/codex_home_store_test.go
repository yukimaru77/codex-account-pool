package pool

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
