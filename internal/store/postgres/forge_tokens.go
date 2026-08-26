package postgres

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
)

func (s *Store) forgeTokenAEAD() (cipher.AEAD, error) {
	if len(s.forgeTokenKey) != 32 {
		return nil, store.ErrForgeTokenKey
	}
	block, err := aes.NewCipher(s.forgeTokenKey)
	if err != nil {
		return nil, store.ErrForgeTokenKey
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, store.ErrForgeTokenKey
	}
	return aead, nil
}

func (s *Store) encryptForgeToken(userID, token string) ([]byte, []byte, error) {
	aead, err := s.forgeTokenAEAD()
	if err != nil {
		return nil, nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err = io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, nil, fmt.Errorf("generate forge token nonce: %w", err)
	}
	return nonce, aead.Seal(nil, nonce, []byte(token), []byte(userID)), nil
}

func (s *Store) decryptForgeToken(row db.UserForgeToken) (string, error) {
	aead, err := s.forgeTokenAEAD()
	if err != nil {
		return "", err
	}
	if len(row.CipherNonce) != aead.NonceSize() {
		return "", store.ErrForgeTokenDecrypt
	}
	plaintext, err := aead.Open(nil, row.CipherNonce, row.Ciphertext, []byte(row.UserID))
	if err != nil {
		return "", store.ErrForgeTokenDecrypt
	}
	return string(plaintext), nil
}

// StoreForgeToken atomically replaces an active owner's one recoverable forge
// credential after the caller has completed authenticated identity validation.
func (s *Store) StoreForgeToken(ctx context.Context, userID, token, login string) (core.ForgeTokenStatus, error) {
	login = strings.TrimSpace(login)
	if userID == "" || token == "" || login == "" {
		return core.ForgeTokenStatus{}, store.ErrNotFound
	}
	nonce, ciphertext, err := s.encryptForgeToken(userID, token)
	if err != nil {
		return core.ForgeTokenStatus{}, err
	}
	var saved db.UserForgeToken
	err = s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		var status string
		if err := tx.QueryRow(ctx, `SELECT status FROM users WHERE id=$1 FOR UPDATE`, userID).Scan(&status); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return store.ErrNotFound
			}
			return err
		}
		if status != "active" {
			return store.ErrForgeTokenOwnerInactive
		}
		var existed bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM user_forge_tokens WHERE user_id=$1)`, userID).Scan(&existed); err != nil {
			return err
		}
		var err error
		saved, err = q.UpsertUserForgeToken(ctx, db.UpsertUserForgeTokenParams{UserID: userID, CipherNonce: nonce, Ciphertext: ciphertext, ForgeLogin: login})
		if err != nil {
			return err
		}
		kind := "identity.forge_token_stored"
		if existed {
			kind = "identity.forge_token_replaced"
		}
		actor := store.ActorFromContext(ctx)
		return q.InsertDeploymentEvent(ctx, db.InsertDeploymentEventParams{Kind: kind, ActorID: actor.ID, ActorRole: string(actor.Role), PayloadJson: core.JSONPayload(map[string]any{"user_id": userID, "forge_login": login}), At: pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true}})
	})
	if err != nil {
		return core.ForgeTokenStatus{}, err
	}
	return core.ForgeTokenStatus{Configured: true, ForgeLogin: saved.ForgeLogin, StoredAt: saved.StoredAt.Time}, nil
}

func (s *Store) DeleteForgeToken(ctx context.Context, userID string) error {
	return s.inTx(ctx, func(_ pgx.Tx, q *db.Queries) error {
		count, err := q.DeleteUserForgeToken(ctx, userID)
		if err != nil || count == 0 {
			return err
		}
		actor := store.ActorFromContext(ctx)
		return q.InsertDeploymentEvent(ctx, db.InsertDeploymentEventParams{Kind: "identity.forge_token_deleted", ActorID: actor.ID, ActorRole: string(actor.Role), PayloadJson: core.JSONPayload(map[string]any{"user_id": userID}), At: pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true}})
	})
}

func (s *Store) GetForgeTokenStatus(ctx context.Context, userID string) (core.ForgeTokenStatus, error) {
	row, err := s.queries.GetUserForgeTokenStatus(ctx, userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return core.ForgeTokenStatus{}, store.ErrNotFound
	}
	if err != nil {
		return core.ForgeTokenStatus{}, err
	}
	result := core.ForgeTokenStatus{Configured: row.Configured}
	if row.Configured {
		result.ForgeLogin, result.StoredAt = row.ForgeLogin.String, row.StoredAt.Time
	}
	return result, nil
}

func (s *Store) GetForgeTokenForUse(ctx context.Context, userID string) (core.ForgeTokenCredential, error) {
	row, err := s.queries.GetUserForgeTokenForUse(ctx, userID)
	if errors.Is(err, pgx.ErrNoRows) {
		status, statusErr := s.queries.GetUserForgeTokenStatus(ctx, userID)
		if statusErr == nil && status.Status != "active" {
			return core.ForgeTokenCredential{}, store.ErrForgeTokenOwnerInactive
		}
		return core.ForgeTokenCredential{}, store.ErrNotFound
	}
	if err != nil {
		return core.ForgeTokenCredential{}, err
	}
	token, err := s.decryptForgeToken(row)
	if err != nil {
		return core.ForgeTokenCredential{}, err
	}
	return core.ForgeTokenCredential{UserID: userID, Token: token, ForgeTokenStatus: core.ForgeTokenStatus{Configured: true, ForgeLogin: row.ForgeLogin, StoredAt: row.StoredAt.Time}}, nil
}

func (s *Store) ListForgeTokensForRedaction(ctx context.Context) ([]string, error) {
	rows, err := s.queries.ListUserForgeTokensForRedaction(ctx)
	if err != nil {
		return nil, err
	}
	values := make([]string, 0, len(rows))
	for _, row := range rows {
		value, err := s.decryptForgeToken(row)
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	workspaceRows, err := s.queries.ListWorkspaceForgeTokensForRedaction(ctx)
	if err != nil {
		return nil, err
	}
	for _, row := range workspaceRows {
		value, err := s.decryptWorkspaceForgeToken(row)
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, nil
}
