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
type forgeEncryptionState struct {
	mu  sync.RWMutex
	key []byte
}

func (s *Store) ConfigureForgeTokenEncryptionKey(key []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.key = append([]byte(nil), key...)
}
func (s *Store) forgeTokenAEAD() (cipher.AEAD, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.key) != 32 {
		return nil, store.ErrForgeTokenKey
	}
	block, err := aes.NewCipher(s.key)
	if err != nil {
		return nil, store.ErrForgeTokenKey
	}
	return cipher.NewGCM(block)
}
func (s *Store) encryptForgeToken(owner, token string) ([]byte, []byte, error) {
	a, err := s.forgeTokenAEAD()
	if err != nil {
		return nil, nil, translateBackendConflict(err)
	}
	nonce := make([]byte, a.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return nil, nil, translateBackendConflict(err)
	}
	return nonce, a.Seal(nil, nonce, []byte(token), []byte(owner)), nil
}
func (s *Store) decryptForgeToken(owner string, nonce, ciphertext []byte) (string, error) {
	a, err := s.forgeTokenAEAD()
	if err != nil {
		return "", translateBackendConflict(err)
	}
	if len(nonce) != a.NonceSize() {
		return "", store.ErrForgeTokenDecrypt
	}
	b, err := a.Open(nil, nonce, ciphertext, []byte(owner))
	if err != nil {
		return "", store.ErrForgeTokenDecrypt
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
			v, e := s.decryptForgeToken(owner, nonce, ciphertext)
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
