package main

import (
	"bytes"
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
