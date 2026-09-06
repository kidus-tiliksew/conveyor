package singlestore

import (
	"bytes"
	"errors"
	"sync"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func TestForgeEncryptionOwnership(t *testing.T) {
	var first, second Store
	key := bytes.Repeat([]byte{1}, 32)
	first.ConfigureForgeTokenEncryptionKey(key)
	key[0] = 9
	second.ConfigureForgeTokenEncryptionKey(bytes.Repeat([]byte{2}, 32))
	nonce, ciphertext, err := first.encryptForgeToken("owner", "fixture-token")
	if err != nil {
		t.Fatal(err)
	}
	if len(nonce) != 12 || bytes.Contains(ciphertext, []byte("fixture-token")) {
		t.Fatal("unsafe ciphertext or nonce")
	}
	first.ConfigureForgeTokenEncryptionKey(bytes.Repeat([]byte{1}, 32))
	value, err := first.decryptForgeToken("owner", nonce, ciphertext)
	if err != nil || value != "fixture-token" {
		t.Fatal("caller key mutation affected stored key")
	}
	if _, err = second.decryptForgeToken("owner", nonce, ciphertext); !errors.Is(err, store.ErrForgeTokenDecrypt) {
		t.Fatal("instances share keys")
	}
	for _, owner := range []string{"other", "workspace:owner"} {
		if _, err = first.decryptForgeToken(owner, nonce, ciphertext); !errors.Is(err, store.ErrForgeTokenDecrypt) {
			t.Fatal("owner binding was not authenticated")
		}
	}
	if _, err = first.decryptForgeToken("owner", nonce[:11], ciphertext); !errors.Is(err, store.ErrForgeTokenDecrypt) {
		t.Fatal("invalid nonce accepted")
	}
	ciphertext[0] ^= 1
	if _, err = first.decryptForgeToken("owner", nonce, ciphertext); !errors.Is(err, store.ErrForgeTokenDecrypt) {
		t.Fatal("corrupt ciphertext accepted")
	}
}
func TestForgeEncryptionInvalidKeys(t *testing.T) {
	var s Store
	for _, n := range []int{0, 1, 16, 24, 31, 33} {
		s.ConfigureForgeTokenEncryptionKey(make([]byte, n))
		if _, _, err := s.encryptForgeToken("owner", "fixture-token"); !errors.Is(err, store.ErrForgeTokenKey) {
			t.Fatalf("key length %d: %v", n, err)
		}
	}
}
func TestForgeEncryptionConcurrentConfiguration(t *testing.T) {
	var s Store
	s.ConfigureForgeTokenEncryptionKey(bytes.Repeat([]byte{1}, 32))
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				if i%2 == 0 {
					s.ConfigureForgeTokenEncryptionKey(bytes.Repeat([]byte{1}, 32))
				} else {
					nonce, ciphertext, err := s.encryptForgeToken("owner", "fixture-token")
					if err != nil {
						t.Error(err)
						return
					}
					v, err := s.decryptForgeToken("owner", nonce, ciphertext)
					if err != nil || v != "fixture-token" {
						t.Error("concurrent key use failed")
						return
					}
				}
			}
		}()
	}
	wg.Wait()
}
