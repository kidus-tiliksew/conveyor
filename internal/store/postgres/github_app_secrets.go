package postgres

import (
	"context"
	"crypto/aes"
	"crypto/cipher"

	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func (s *Store) gitHubAppKeyAEAD() (cipher.AEAD, error) {
	if len(s.gitHubAppEncryptionKey) != 32 {
		return nil, store.ErrGitHubAppKey
	}
	block, err := aes.NewCipher(s.gitHubAppEncryptionKey)
	if err != nil {
		return nil, store.ErrGitHubAppKey
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, store.ErrGitHubAppKey
	}
	return aead, nil
}

func (s *Store) ListGitHubAppKeysForRedaction(ctx context.Context) ([]string, error) {
	var values []string
	appRows, err := s.boundary.Query(ctx, `SELECT workspace_id,private_key_nonce,private_key_ciphertext FROM workspace_github_apps`)
	if err != nil {
		return nil, err
	}
	defer appRows.Close()
	for appRows.Next() {
		var id string
		var nonce, ciphertext []byte
		if err = appRows.Scan(&id, &nonce, &ciphertext); err != nil {
			return nil, err
		}
		a, err := s.gitHubAppKeyAEAD()
		if err != nil {
			return nil, err
		}
		if len(nonce) != a.NonceSize() {
			return nil, store.ErrGitHubAppKeyDecrypt
		}
		key, err := a.Open(nil, nonce, ciphertext, []byte("workspace-app:"+id))
		if err != nil {
			return nil, store.ErrGitHubAppKeyDecrypt
		}
		values = append(values, string(key))
	}
	if err := appRows.Err(); err != nil {
		return nil, err
	}

	return values, nil
}
