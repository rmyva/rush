package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAPITokenRevokeWriteFailurePreservesCachedToken(t *testing.T) {
	isolateAllGlobalConfigPaths(t)
	t.Setenv(GlobalDirEnv, "")
	restoreKtav := SetKtavAutoMigrateForTest(false)
	t.Cleanup(restoreKtav)
	path := filepath.Join(t.TempDir(), "tokens.json")
	store := &APITokenStore{Path: path}
	plain, record, err := store.CreateAPIToken("cached", []string{"read"}, "ask", nil)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	writeErr := errors.New("injected write failure")
	previousWriteFile := apiTokenWriteFile
	apiTokenWriteFile = func(string, []byte, os.FileMode) error { return writeErr }
	t.Cleanup(func() { apiTokenWriteFile = previousWriteFile })
	if err := store.RevokeAPIToken(record.ID); !errors.Is(err, writeErr) {
		t.Fatalf("revoke error = %v, want %v", err, writeErr)
	}

	got, ok, err := store.VerifyAPIToken(plain, time.Now())
	if err != nil || !ok || got.ID != record.ID {
		t.Fatalf("token after failed revoke: record=%+v ok=%v err=%v", got, ok, err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatal("failed revoke changed the token file")
	}
}

func TestAPITokenConcurrentReadersAndMutations(t *testing.T) {
	isolateAllGlobalConfigPaths(t)
	t.Setenv(GlobalDirEnv, "")
	restoreKtav := SetKtavAutoMigrateForTest(false)
	t.Cleanup(restoreKtav)
	path := filepath.Join(t.TempDir(), "tokens.json")
	store := &APITokenStore{Path: path}
	stablePlain, stableRecord, err := store.CreateAPIToken("stable", []string{"read"}, "ask", nil)
	if err != nil {
		t.Fatal(err)
	}

	const iterations = 500
	start := make(chan struct{})
	readerDone := make(chan error, 1)
	go func() {
		<-start
		for i := 0; i < iterations; i++ {
			if _, ok, err := store.VerifyAPIToken(stablePlain, time.Now()); err != nil || !ok {
				readerDone <- errors.New("stable token verification failed")
				return
			}
			tokens, err := store.ListAPITokens()
			if err != nil {
				readerDone <- err
				return
			}
			found := false
			for _, token := range tokens {
				if token.ID == stableRecord.ID {
					found = true
					break
				}
			}
			if !found {
				readerDone <- errors.New("stable token missing from list")
				return
			}
			if _, err := store.ShowAPIToken(stableRecord.ID); err != nil {
				readerDone <- err
				return
			}
		}
		readerDone <- nil
	}()

	close(start)
	revoked := make([]struct {
		plain  string
		record APIToken
	}, 5)
	for i := range revoked {
		plain, record, err := store.CreateAPIToken("concurrent", []string{"read"}, "ask", nil)
		if err != nil {
			t.Fatal(err)
		}
		revoked[i] = struct {
			plain  string
			record APIToken
		}{plain, record}
		if err := store.RevokeAPIToken(record.ID); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case err := <-readerDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for readerDone")
	}

	for _, token := range revoked {
		if _, ok, err := store.VerifyAPIToken(token.plain, time.Now()); ok || err != ErrInvalidAPIToken {
			t.Fatalf("revoked token verification: ok=%v err=%v", ok, err)
		}
	}
	items, err := store.ListAPITokens()
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 6 {
		t.Fatalf("token count = %d, want 6", len(items))
	}
	for _, revokedToken := range revoked {
		found := false
		for _, item := range items {
			if item.ID == revokedToken.record.ID {
				found = item.RevokedAt != nil
				break
			}
		}
		if !found {
			t.Errorf("revoked token %s missing or not revoked on disk", revokedToken.record.ID)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var disk apiTokenDocument
	if err := json.Unmarshal(data, &disk); err != nil {
		t.Fatal(err)
	}
	if len(disk.Tokens) != 6 {
		t.Fatalf("on-disk token count = %d, want 6", len(disk.Tokens))
	}
	for _, revokedToken := range revoked {
		found := false
		for _, item := range disk.Tokens {
			if item.ID == revokedToken.record.ID {
				found = item.RevokedAt != nil
				break
			}
		}
		if !found {
			t.Errorf("revoked token %s missing or not revoked in file", revokedToken.record.ID)
		}
	}
}
