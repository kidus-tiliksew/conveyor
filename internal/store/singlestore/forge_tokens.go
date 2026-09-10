package singlestore

// DEC-38: this aggregate follows component-identity-membership and component-persistence.

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"errors"
	"strings"
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
func (s *Store) StoreForgeToken(ctx context.Context, userID, token, login string) (core.ForgeTokenStatus, error) {
	return s.storeForgeToken(ctx, userID, token, login, false)
}
func (s *Store) storeForgeToken(ctx context.Context, owner, token, login string, ws bool) (core.ForgeTokenStatus, error) {
	login = strings.TrimSpace(login)
	if owner == "" || token == "" || login == "" {
		return core.ForgeTokenStatus{}, store.ErrNotFound
	}
	table, col, aad, kind := "user_forge_tokens", "user_id", owner, "identity.forge_token_"
	if ws {
		table, col, aad, kind = "workspace_forge_tokens", "workspace_id", "workspace:"+owner, "workspace.forge_token_"
	}
	nonce, ciphertext, err := s.encryptForgeToken(aad, token)
	if err != nil {
		return core.ForgeTokenStatus{}, translateBackendConflict(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		if ws {
			var id string
			if err := tx.QueryRowContext(ctx, "SELECT id FROM workspaces WHERE id=? FOR UPDATE", owner).Scan(&id); err != nil {
				return identityNotFound(err)
			}
		} else {
			var status string
			if err := tx.QueryRowContext(ctx, "SELECT status FROM users WHERE id=? FOR UPDATE", owner).Scan(&status); err != nil {
				return identityNotFound(err)
			}
			if status != "active" {
				return store.ErrForgeTokenOwnerInactive
			}
		}
		var count int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table+" WHERE "+col+"=?", owner).Scan(&count); err != nil {
			return translateBackendConflict(err)
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO "+table+" ("+col+",cipher_nonce,ciphertext,forge_login,stored_at) VALUES(?,?,?,?,?) ON DUPLICATE KEY UPDATE cipher_nonce=VALUES(cipher_nonce),ciphertext=VALUES(ciphertext),forge_login=VALUES(forge_login),stored_at=VALUES(stored_at)", owner, nonce, ciphertext, login, now); err != nil {
			return translateBackendConflict(err)
		}
		if count > 0 {
			kind += "replaced"
		} else {
			kind += "stored"
		}
		payload := map[string]any{col: owner, "forge_login": login}
		if ws {
			_, err := appendWorkspaceEvent(ctx, tx, owner, kind, payload)
			return translateBackendConflict(err)
		}
		return appendDeploymentEvent(ctx, tx, kind, payload)
	})
	if err != nil {
		return core.ForgeTokenStatus{}, translateBackendConflict(err)
	}
	return core.ForgeTokenStatus{Configured: true, ForgeLogin: login, StoredAt: now}, nil
}
func (s *Store) DeleteForgeToken(ctx context.Context, userID string) error {
	return s.deleteForgeToken(ctx, userID, false)
}
func (s *Store) deleteForgeToken(ctx context.Context, owner string, ws bool) error {
	table, col, kind := "user_forge_tokens", "user_id", "identity.forge_token_deleted"
	if ws {
		table, col, kind = "workspace_forge_tokens", "workspace_id", "workspace.forge_token_deleted"
		if owner == "" {
			return store.ErrNotFound
		}
	}
	return s.withTx(ctx, func(tx *sql.Tx) error {
		r, err := tx.ExecContext(ctx, "DELETE FROM "+table+" WHERE "+col+"=?", owner)
		if err != nil {
			return translateBackendConflict(err)
		}
		n, err := r.RowsAffected()
		if err != nil || n == 0 {
			return translateBackendConflict(err)
		}
		payload := map[string]any{col: owner}
		if ws {
			_, err := appendWorkspaceEvent(ctx, tx, owner, kind, payload)
			return translateBackendConflict(err)
		}
		return appendDeploymentEvent(ctx, tx, kind, payload)
	})
}
func (s *Store) GetForgeTokenStatus(ctx context.Context, userID string) (core.ForgeTokenStatus, error) {
	return s.forgeTokenStatus(ctx, userID, false)
}
func (s *Store) forgeTokenStatus(ctx context.Context, owner string, ws bool) (core.ForgeTokenStatus, error) {
	query := "SELECT f.forge_login,f.stored_at FROM users u LEFT JOIN user_forge_tokens f ON f.user_id=u.id WHERE u.id=?"
	if ws {
		query = "SELECT f.forge_login,f.stored_at FROM workspaces w LEFT JOIN workspace_forge_tokens f ON f.workspace_id=w.id WHERE w.id=?"
	}
	var login sql.NullString
	var at sql.NullTime
	if err := s.db.QueryRowContext(ctx, query, owner).Scan(&login, &at); err != nil {
		return core.ForgeTokenStatus{}, identityNotFound(err)
	}
	return core.ForgeTokenStatus{Configured: at.Valid, ForgeLogin: login.String, StoredAt: at.Time}, nil
}
func (s *Store) GetForgeTokenForUse(ctx context.Context, userID string) (core.ForgeTokenCredential, error) {
	var nonce, ciphertext []byte
	var status string
	var r core.ForgeTokenCredential
	r.UserID = userID
	err := s.db.QueryRowContext(ctx, `SELECT u.status,f.cipher_nonce,f.ciphertext,f.forge_login,f.stored_at FROM users u JOIN user_forge_tokens f ON f.user_id=u.id WHERE u.id=?`, userID).Scan(&status, &nonce, &ciphertext, &r.ForgeLogin, &r.StoredAt)
	if errors.Is(err, sql.ErrNoRows) {
		var state string
		if e := s.db.QueryRowContext(ctx, "SELECT status FROM users WHERE id=?", userID).Scan(&state); e == nil && state != "active" {
			return r, store.ErrForgeTokenOwnerInactive
		}
	}
	if err != nil {
		return r, identityNotFound(err)
	}
	if status != "active" {
		return core.ForgeTokenCredential{}, store.ErrForgeTokenOwnerInactive
	}
	r.Token, err = s.decryptForgeToken(userID, nonce, ciphertext)
	r.Configured = err == nil
	return r, translateBackendConflict(err)
}
func (s *Store) ListForgeTokensForRedaction(ctx context.Context) ([]string, error) {
	values := []string{}
	for _, q := range []string{"SELECT user_id,cipher_nonce,ciphertext FROM user_forge_tokens ORDER BY user_id", "SELECT CONCAT('workspace:',workspace_id),cipher_nonce,ciphertext FROM workspace_forge_tokens ORDER BY workspace_id", "SELECT CONCAT('workspace-app:',workspace_id),private_key_nonce,private_key_ciphertext FROM workspace_github_apps ORDER BY workspace_id"} {
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
