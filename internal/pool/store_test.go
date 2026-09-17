package pool

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"codex-account-pool/internal/cpa"
)

func credential(account string) Credential {
	return Credential{Type: "codex", CodexTokenData: cpa.CodexTokenData{AccountID: account, AccessToken: "access-" + account, RefreshToken: "refresh-" + account, Expire: time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}}
}

func TestStoreImportIsCopyOnlyAndCannotRollbackToken(t *testing.T) {
	s, err := OpenStore(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	c := credential("a")
	b, _ := json.Marshal(c)
	id, err := s.Import(b)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Import(b); err == nil {
		t.Fatal("duplicate import could roll token back")
	}
	if info, err := os.Stat(s.path(id)); err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("credential permissions")
	}
	if _, err = s.Token(context.Background(), "../../outside", false); err == nil {
		t.Fatal("path traversal")
	}
	if err = s.SetDisabled(id, true); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Token(context.Background(), id, false); err == nil {
		t.Fatal("disabled account used")
	}
}

func TestRefreshRotationIsSerializedAcrossStoreInstances(t *testing.T) {
	var calls atomic.Int64
	refresh := func(_ context.Context, token string) (*cpa.CodexTokenData, error) {
		calls.Add(1)
		if token != "refresh-a" {
			return nil, errors.New("reused or wrong refresh token")
		}
		return &cpa.CodexTokenData{AccountID: "a", AccessToken: "new-access", RefreshToken: "new-refresh", IDToken: "new-id-token", Email: "updated@example.invalid", Expire: time.Now().Add(time.Hour).Format(time.RFC3339)}, nil
	}
	s, _ := OpenStore(t.TempDir(), refresh)
	s2, _ := OpenStore(s.Dir, refresh)
	c := credential("a")
	c.IDToken = "old-id-token"
	c.Email = "old@example.invalid"
	c.Expire = time.Now().Add(-time.Hour).Format(time.RFC3339)
	id, err := s.Login(c)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			store := s
			if i%2 == 0 {
				store = s2
			}
			got, err := store.Token(context.Background(), id, false)
			if err != nil || got.AccessToken != "new-access" || got.RefreshToken != "new-refresh" || got.IDToken != "new-id-token" || got.Email != "updated@example.invalid" {
				t.Errorf("refresh %s %v", got.AccessToken, err)
			}
		}(i)
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("refresh calls=%d", calls.Load())
	}
	got, _ := s.read(id)
	if got.RefreshToken != "new-refresh" || got.IDToken != "new-id-token" || got.Email != "updated@example.invalid" {
		t.Fatal("rotation not persisted")
	}
	reopened, err := OpenStore(s.Dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	afterRestart, err := reopened.Token(context.Background(), id, false)
	if err != nil || afterRestart != got {
		t.Fatal("refreshed credentials changed after reopening the store")
	}
}

func TestRefreshFailureAndWrongIdentityDoNotOverwrite(t *testing.T) {
	for _, wrong := range []bool{false, true} {
		s, _ := OpenStore(t.TempDir(), func(context.Context, string) (*cpa.CodexTokenData, error) {
			if wrong {
				return &cpa.CodexTokenData{AccountID: "b", AccessToken: "wrong"}, nil
			}
			return nil, errors.New("failed")
		})
		c := credential("a")
		id, err := s.Login(c)
		if err != nil {
			t.Fatal(err)
		}
		before, _ := os.ReadFile(s.path(id))
		if _, err = s.Token(context.Background(), id, true); err == nil {
			t.Fatal("invalid refresh succeeded")
		}
		after, _ := os.ReadFile(s.path(id))
		if string(before) != string(after) {
			t.Fatal("failed refresh overwrote credential")
		}
	}
}

func TestRefreshRetainsOptionalCredentialsWhenOmitted(t *testing.T) {
	s, _ := OpenStore(t.TempDir(), func(context.Context, string) (*cpa.CodexTokenData, error) {
		return &cpa.CodexTokenData{AccessToken: "new", Expire: time.Now().Add(time.Hour).Format(time.RFC3339)}, nil
	})
	original := credential("a")
	original.IDToken = "original-id-token"
	original.Email = "original@example.invalid"
	id, err := s.Login(original)
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.Token(context.Background(), id, true)
	if err != nil || c.AccessToken != "new" || c.RefreshToken != original.RefreshToken || c.IDToken != original.IDToken || c.Email != original.Email || c.AccountID != original.AccountID {
		t.Fatalf("token retention failed %v", err)
	}
	persisted, err := s.read(id)
	if err != nil || persisted != c {
		t.Fatal("retained credentials were not persisted")
	}
}

func TestRefreshOnlyWhenWithinOneMinuteOrExpiryUnknown(t *testing.T) {
	for _, tc := range []struct {
		name   string
		expire string
		calls  int
	}{
		{"fresh", time.Now().Add(time.Hour).Format(time.RFC3339), 0},
		{"due", time.Now().Add(30 * time.Second).Format(time.RFC3339), 1},
		{"expired", time.Now().Add(-time.Minute).Format(time.RFC3339), 1},
		{"missing", "", 1},
		{"unrecognized", "unknown", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			refresh := func(context.Context, string) (*cpa.CodexTokenData, error) {
				calls++
				return &cpa.CodexTokenData{AccessToken: "new", RefreshToken: "rotated", Expire: time.Now().Add(time.Hour).Format(time.RFC3339)}, nil
			}
			s, _ := OpenStore(t.TempDir(), refresh)
			c := credential("a")
			c.Expire = tc.expire
			id, err := s.Login(c)
			if err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadFile(s.path(id))
			if _, err = s.Token(context.Background(), id, false); err != nil {
				t.Fatal(err)
			}
			// Simulate restart: reread the on-disk expiry and rotated token.
			reopened, _ := OpenStore(s.Dir, refresh)
			if _, err = reopened.Token(context.Background(), id, false); err != nil {
				t.Fatal(err)
			}
			if calls != tc.calls {
				t.Fatalf("refresh calls=%d want=%d", calls, tc.calls)
			}
			after, _ := os.ReadFile(s.path(id))
			if tc.calls == 0 && string(before) != string(after) {
				t.Fatal("fresh credential was rewritten")
			}
		})
	}
}
