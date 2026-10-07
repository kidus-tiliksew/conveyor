package singlestore

// DEC-38: this aggregate follows component-identity-membership and component-persistence.

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"errors"
	"sync"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// DEC-38, component-identity-membership: each Store owns a defensive key copy.
// The mutex protects configuration and cipher construction, including runtime rotation.
type gitHubAppKeyEncryptionState struct {
	mu  sync.RWMutex
	key []byte
}

// ConfigureGitHubAppKeyEncryptionKey installs the process-only AES-256 key
// that seals workspace GitHub App private keys (DEC-59 clause 2).
func (s *Store) ConfigureGitHubAppKeyEncryptionKey(key []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.key = append([]byte(nil), key...)
}
func (s *Store) gitHubAppKeyAEAD() (cipher.AEAD, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.key) != 32 {
		return nil, store.ErrGitHubAppKey
	}
	block, err := aes.NewCipher(s.key)
	if err != nil {
		return nil, store.ErrGitHubAppKey
	}
	return cipher.NewGCM(block)
}
func (s *Store) encryptGitHubAppKey(owner, privateKey string) ([]byte, []byte, error) {
	a, err := s.gitHubAppKeyAEAD()
	if err != nil {
		return nil, nil, translateBackendConflict(err)
	}
	nonce := make([]byte, a.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return nil, nil, translateBackendConflict(err)
	}
	return nonce, a.Seal(nil, nonce, []byte(privateKey), []byte(owner)), nil
}
func (s *Store) decryptGitHubAppKey(owner string, nonce, ciphertext []byte) (string, error) {
	a, err := s.gitHubAppKeyAEAD()
	if err != nil {
		return "", translateBackendConflict(err)
	}
	if len(nonce) != a.NonceSize() {
		return "", store.ErrGitHubAppKeyDecrypt
	}
	b, err := a.Open(nil, nonce, ciphertext, []byte(owner))
	if err != nil {
		return "", store.ErrGitHubAppKeyDecrypt
	}
	return string(b), nil
}
func identityNotFound(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return store.ErrNotFound
	}
	return translateBackendConflict(err)
}
func appendDeploymentEvent(ctx context.Context, tx *sql.Tx, kind string, payload map[string]any) error {
	actor := store.ActorFromContext(ctx)
	_, err := writeRow(ctx, tx, rowWrite{table: "deployment_events", operation: "INSERT", values: map[string]any{"kind": kind, "actor_id": actor.ID, "actor_role": string(actor.Role), "payload_json": []byte(core.JSONPayload(payload)), "at": time.Now().UTC()}})
	return translateBackendConflict(err)
}
func (s *Store) ListGitHubAppKeysForRedaction(ctx context.Context) ([]string, error) {
	values := []string{}
	for _, q := range []string{"SELECT CONCAT('workspace-app:',workspace_id),private_key_nonce,private_key_ciphertext FROM workspace_github_apps ORDER BY workspace_id"} {
		rows, err := s.db.QueryContext(ctx, q)
		if err != nil {
			return nil, translateBackendConflict(err)
		}
		for rows.Next() {
			var owner string
			var nonce, ciphertext []byte
			if err = rows.Scan(&owner, &nonce, &ciphertext); err != nil {
				rows.Close()
				return nil, translateBackendConflict(err)
			}
			v, e := s.decryptGitHubAppKey(owner, nonce, ciphertext)
			if e != nil {
				rows.Close()
				return nil, translateBackendConflict(e)
			}
			values = append(values, v)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, translateBackendConflict(err)
		}
	}
	return values, nil
}
