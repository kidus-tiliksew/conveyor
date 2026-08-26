package postgres

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
)

func workspaceForgeTokenAAD(workspaceID string) []byte {
	return []byte("workspace:" + workspaceID)
}

func (s *Store) encryptWorkspaceForgeToken(workspaceID, token string) ([]byte, []byte, error) {
	aead, err := s.forgeTokenAEAD()
	if err != nil {
		return nil, nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err = io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, nil, fmt.Errorf("generate workspace forge token nonce: %w", err)
	}
	return nonce, aead.Seal(nil, nonce, []byte(token), workspaceForgeTokenAAD(workspaceID)), nil
}

func (s *Store) decryptWorkspaceForgeToken(row db.WorkspaceForgeToken) (string, error) {
	aead, err := s.forgeTokenAEAD()
	if err != nil {
		return "", err
	}
	if len(row.CipherNonce) != aead.NonceSize() {
		return "", store.ErrForgeTokenDecrypt
	}
	plaintext, err := aead.Open(nil, row.CipherNonce, row.Ciphertext, workspaceForgeTokenAAD(row.WorkspaceID))
	if err != nil {
		return "", store.ErrForgeTokenDecrypt
	}
	return string(plaintext), nil
}

// StoreWorkspaceForgeToken atomically replaces a workspace's one recoverable
// credential after the HTTP boundary has validated the candidate identity.
func (s *Store) StoreWorkspaceForgeToken(ctx context.Context, workspaceID, token, login string) (core.ForgeTokenStatus, error) {
	workspaceID, login = strings.TrimSpace(workspaceID), strings.TrimSpace(login)
	if workspaceID == "" || token == "" || login == "" {
		return core.ForgeTokenStatus{}, store.ErrNotFound
	}
	nonce, ciphertext, err := s.encryptWorkspaceForgeToken(workspaceID, token)
	if err != nil {
		return core.ForgeTokenStatus{}, err
	}
	var saved db.WorkspaceForgeToken
	err = s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		var lockedID string
		if err := tx.QueryRow(ctx, `SELECT id FROM workspaces WHERE id=$1 FOR UPDATE`, workspaceID).Scan(&lockedID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return store.ErrNotFound
			}
			return err
		}
		var existed bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM workspace_forge_tokens WHERE workspace_id=$1)`, workspaceID).Scan(&existed); err != nil {
			return err
		}
		saved, err = q.UpsertWorkspaceForgeToken(ctx, db.UpsertWorkspaceForgeTokenParams{
			WorkspaceID: workspaceID,
			CipherNonce: nonce,
			Ciphertext:  ciphertext,
			ForgeLogin:  login,
		})
		if err != nil {
			return err
		}
		kind := "workspace.forge_token_stored"
		if existed {
			kind = "workspace.forge_token_replaced"
		}
		actor := store.ActorFromContext(ctx)
		_, err = q.InsertWorkspaceEvent(ctx, db.InsertWorkspaceEventParams{
			WorkspaceID: workspaceID,
			Kind:        kind,
			ActorID:     actor.ID,
			ActorRole:   string(actor.Role),
			PayloadJson: core.JSONPayload(map[string]any{"workspace_id": workspaceID, "forge_login": login}),
			At:          timestamp(time.Now().UTC()),
		})
		return err
	})
	if err != nil {
		return core.ForgeTokenStatus{}, err
	}
	return core.ForgeTokenStatus{Configured: true, ForgeLogin: saved.ForgeLogin, StoredAt: saved.StoredAt.Time}, nil
}

func (s *Store) DeleteWorkspaceForgeToken(ctx context.Context, workspaceID string) error {
	workspaceID = strings.TrimSpace(workspaceID)
	if workspaceID == "" {
		return store.ErrNotFound
	}
	return s.inTx(ctx, func(_ pgx.Tx, q *db.Queries) error {
		count, err := q.DeleteWorkspaceForgeToken(ctx, workspaceID)
		if err != nil || count == 0 {
			return err
		}
		actor := store.ActorFromContext(ctx)
		_, err = q.InsertWorkspaceEvent(ctx, db.InsertWorkspaceEventParams{
			WorkspaceID: workspaceID,
			Kind:        "workspace.forge_token_deleted",
			ActorID:     actor.ID,
			ActorRole:   string(actor.Role),
			PayloadJson: core.JSONPayload(map[string]any{"workspace_id": workspaceID}),
			At:          timestamp(time.Now().UTC()),
		})
		return err
	})
}

func (s *Store) GetWorkspaceForgeTokenStatus(ctx context.Context, workspaceID string) (core.ForgeTokenStatus, error) {
	row, err := s.queries.GetWorkspaceForgeTokenStatus(ctx, workspaceID)
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

func (s *Store) GetWorkspaceForgeTokenForUse(ctx context.Context, workspaceID string) (core.WorkspaceForgeTokenCredential, error) {
	row, err := s.queries.GetWorkspaceForgeTokenForUse(ctx, workspaceID)
	if errors.Is(err, pgx.ErrNoRows) {
		return core.WorkspaceForgeTokenCredential{}, store.ErrNotFound
	}
	if err != nil {
		return core.WorkspaceForgeTokenCredential{}, err
	}
	token, err := s.decryptWorkspaceForgeToken(row)
	if err != nil {
		return core.WorkspaceForgeTokenCredential{}, err
	}
	return core.WorkspaceForgeTokenCredential{
		WorkspaceID: workspaceID,
		Token:       token,
		ForgeTokenStatus: core.ForgeTokenStatus{
			Configured: true,
			ForgeLogin: row.ForgeLogin,
			StoredAt:   row.StoredAt.Time,
		},
	}, nil
}
