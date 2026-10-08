package singlestore

import (
	"bytes"
	"errors"
	"sync"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

func TestGitHubAppKeyEncryptionOwnership(t *testing.T) {
	var first, second Store
	key := bytes.Repeat([]byte{1}, 32)
	first.ConfigureGitHubAppKeyEncryptionKey(key)
	key[0] = 9
	second.ConfigureGitHubAppKeyEncryptionKey(bytes.Repeat([]byte{2}, 32))
	nonce, ciphertext, err := first.encryptGitHubAppKey("owner", "fixture-app-key")
	if err != nil {
		t.Fatal(err)
	}
	if len(nonce) != 12 || bytes.Contains(ciphertext, []byte("fixture-app-key")) {
		t.Fatal("unsafe ciphertext or nonce")
	}
	first.ConfigureGitHubAppKeyEncryptionKey(bytes.Repeat([]byte{1}, 32))
	value, err := first.decryptGitHubAppKey("owner", nonce, ciphertext)
	if err != nil || value != "fixture-app-key" {
		t.Fatal("caller key mutation affected stored key")
	}
	if _, err = second.decryptGitHubAppKey("owner", nonce, ciphertext); !errors.Is(err, store.ErrGitHubAppKeyDecrypt) {
		t.Fatal("instances share keys")
	}
	for _, owner := range []string{"other", "workspace:owner"} {
		if _, err = first.decryptGitHubAppKey(owner, nonce, ciphertext); !errors.Is(err, store.ErrGitHubAppKeyDecrypt) {
			t.Fatal("owner binding was not authenticated")
		}
	}
	if _, err = first.decryptGitHubAppKey("owner", nonce[:11], ciphertext); !errors.Is(err, store.ErrGitHubAppKeyDecrypt) {
		t.Fatal("invalid nonce accepted")
	}
	ciphertext[0] ^= 1
	if _, err = first.decryptGitHubAppKey("owner", nonce, ciphertext); !errors.Is(err, store.ErrGitHubAppKeyDecrypt) {
		t.Fatal("corrupt ciphertext accepted")
	}
}
func TestGitHubAppKeyEncryptionInvalidKeys(t *testing.T) {
	var s Store
	for _, n := range []int{0, 1, 16, 24, 31, 33} {
		s.ConfigureGitHubAppKeyEncryptionKey(make([]byte, n))
		if _, _, err := s.encryptGitHubAppKey("owner", "fixture-app-key"); !errors.Is(err, store.ErrGitHubAppKey) {
			t.Fatalf("key length %d: %v", n, err)
		}
	}
}
func TestGitHubAppKeyEncryptionConcurrentConfiguration(t *testing.T) {
	var s Store
	s.ConfigureGitHubAppKeyEncryptionKey(bytes.Repeat([]byte{1}, 32))
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				if i%2 == 0 {
					s.ConfigureGitHubAppKeyEncryptionKey(bytes.Repeat([]byte{1}, 32))
				} else {
					nonce, ciphertext, err := s.encryptGitHubAppKey("owner", "fixture-app-key")
					if err != nil {
						t.Error(err)
						return
					}
					v, err := s.decryptGitHubAppKey("owner", nonce, ciphertext)
					if err != nil || v != "fixture-app-key" {
						t.Error("concurrent key use failed")
						return
					}
				}
			}
		}()
	}
	wg.Wait()
}

// DEC-59 clause 2; req-delivery-and-forge AC-1.11: the renamed helper opens
// ciphertext that the pre-rename SingleStore encryption helper produced.
func TestGitHubAppKeyDecryptsPreRenameCiphertext(t *testing.T) {
	fixture := storetest.LegacyGitHubAppKeyFixture
	var s Store
	s.ConfigureGitHubAppKeyEncryptionKey(fixture.Key)
	owner := "workspace-app:" + fixture.Workspace
	value, err := s.decryptGitHubAppKey(owner, fixture.Nonce, fixture.Ciphertext)
	if err != nil || value != fixture.Plaintext {
		t.Fatalf("pre-rename ciphertext: value=%q err=%v", value, err)
	}
	if _, err = s.decryptGitHubAppKey("workspace-app:"+fixture.Workspace+"-other", fixture.Nonce, fixture.Ciphertext); !errors.Is(err, store.ErrGitHubAppKeyDecrypt) {
		t.Fatalf("other workspace additional data: %v", err)
	}
	s.ConfigureGitHubAppKeyEncryptionKey(bytes.Repeat([]byte{2}, 32))
	if _, err = s.decryptGitHubAppKey(owner, fixture.Nonce, fixture.Ciphertext); !errors.Is(err, store.ErrGitHubAppKeyDecrypt) {
		t.Fatalf("wrong key: %v", err)
	}
}
