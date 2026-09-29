package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestInitAndImportWriteOnlyDedicatedState(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "pool.json")
	var output bytes.Buffer
	if err := run([]string{"init", "--config", config}, &output); err != nil {
		t.Fatal(err)
	}
	files := []string{"pool.json", "state/client.key", "state/admin.key"}
	before := map[string][]byte{}
	for _, path := range files {
		full := filepath.Join(dir, path)
		b, err := os.ReadFile(full)
		if err != nil {
			t.Fatal(err)
		}
		before[path] = b
		info, err := os.Stat(full)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("non-private init file", path)
		}
	}
	if bytes.Equal(before["state/client.key"], before["state/admin.key"]) {
		t.Fatal("client and admin keys are identical")
	}
	if err := run([]string{"init", "--config", config}, &output); err == nil {
		t.Fatal("init overwrote config")
	}
	for path, b := range before {
		after, _ := os.ReadFile(filepath.Join(dir, path))
		if !bytes.Equal(b, after) {
			t.Fatal("init overwrote existing file", path)
		}
	}
	source := filepath.Join(dir, "explicit-cpa-source.json")
	credential, _ := json.Marshal(map[string]string{"type": "codex", "account_id": "test-account", "access_token": "test-access", "refresh_token": "test-refresh", "expired": time.Now().Add(time.Hour).Format(time.RFC3339)})
	if err := os.WriteFile(source, credential, 0600); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if err := run([]string{"import", "--config", config, source}, &output); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"test-access", "test-refresh"} {
		if strings.Contains(output.String(), secret) {
			t.Fatal("import printed a token")
		}
	}
	after, _ := os.ReadFile(source)
	if !bytes.Equal(credential, after) {
		t.Fatal("import modified source credential")
	}
	if err := run([]string{"import", "--config", config, source}, &output); err == nil {
		t.Fatal("duplicate import accepted")
	}
}

func TestInitWritesAccountsDirAndOmitsRoutes(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "pool.json")
	var output bytes.Buffer
	args := []string{"init", "--config", config, "--accounts-dir", "d", "--codex-home", "/opt/codex-home", "--codex-bin", "codex", "--listen", "127.0.0.1:19999"}
	if err := run(args, &output); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["round_robin_endpoints"]; ok {
		t.Fatal("init wrote round_robin_endpoints")
	}
	for key, want := range map[string]string{"accounts_dir": "d", "codex_home": "/opt/codex-home", "codex_bin": "codex", "listen": "127.0.0.1:19999"} {
		if raw[key] != want {
			t.Fatalf("%s = %v, want %q", key, raw[key], want)
		}
	}
	if info, err := os.Stat(filepath.Join(dir, "d")); err != nil || !info.IsDir() {
		t.Fatal("init did not create accounts_dir", err)
	}
	for _, name := range []string{"client.key", "admin.key"} {
		if _, err := os.Stat(filepath.Join(dir, "state", name)); err != nil {
			t.Fatal(err)
		}
	}

	plain := filepath.Join(t.TempDir(), "pool.json")
	if err := run([]string{"init", "--config", plain}, &output); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(plain)
	raw = nil
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"round_robin_endpoints", "accounts_dir", "codex_home", "codex_bin"} {
		if _, ok := raw[key]; ok {
			t.Fatalf("default init wrote %s", key)
		}
	}
}

func TestLoginRefusedWithAccountsDir(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "pool.json")
	var output bytes.Buffer
	if err := run([]string{"init", "--config", config, "--accounts-dir", "accounts"}, &output); err != nil {
		t.Fatal(err)
	}
	want := "this pool reads Codex account directories; use: codex-pool account add NAME"
	for _, command := range []string{"login", "import"} {
		err := run([]string{command, "--config", config, filepath.Join(dir, "x.json")}, &output)
		if err == nil || err.Error() != want {
			t.Fatalf("%s error = %v", command, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "state", "accounts")); err == nil {
		t.Fatal("legacy accounts directory created under Codex-home store")
	}
}

func TestEnableDisableAcceptsDirectoryName(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "pool.json")
	var output bytes.Buffer
	if err := run([]string{"init", "--config", config, "--accounts-dir", "accounts"}, &output); err != nil {
		t.Fatal(err)
	}
	account := filepath.Join(dir, "accounts", "work")
	if err := os.MkdirAll(account, 0700); err != nil {
		t.Fatal(err)
	}
	auth := `{"auth_mode":"chatgpt","tokens":{"id_token":"x","access_token":"opaque-access","refresh_token":"opaque-refresh","account_id":"acct-work"}}`
	if err := os.WriteFile(filepath.Join(account, "auth.json"), []byte(auth), 0600); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(account, "disabled")
	if err := run([]string{"disable", "--config", config, "work"}, &output); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("disable by name did not create marker", err)
	}
	h := sha256.Sum256([]byte("acct-work"))
	id := hex.EncodeToString(h[:16])
	if err := run([]string{"enable", "--config", config, id}, &output); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("enable by id did not remove marker", err)
	}
	if err := run([]string{"disable", "--config", config, "missing"}, &output); err == nil {
		t.Fatal("unknown directory name accepted")
	}
}
