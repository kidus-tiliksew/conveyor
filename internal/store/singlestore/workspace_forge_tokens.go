package singlestore

// DEC-38: this aggregate follows component-identity-membership and component-persistence.

import (
	"context"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

func (s *Store) StoreWorkspaceForgeToken(ctx context.Context, workspaceID, token, login string) (core.ForgeTokenStatus, error) {
	return s.storeForgeToken(ctx, strings.TrimSpace(workspaceID), token, login, true)
}
func (s *Store) DeleteWorkspaceForgeToken(ctx context.Context, workspaceID string) error {
	return s.deleteForgeToken(ctx, strings.TrimSpace(workspaceID), true)
}
func (s *Store) GetWorkspaceForgeTokenStatus(ctx context.Context, workspaceID string) (core.ForgeTokenStatus, error) {
	return s.forgeTokenStatus(ctx, workspaceID, true)
}
func (s *Store) GetWorkspaceForgeTokenForUse(ctx context.Context, workspaceID string) (core.WorkspaceForgeTokenCredential, error) {
	var r core.WorkspaceForgeTokenCredential
	r.WorkspaceID = workspaceID
	var nonce, ciphertext []byte
	err := s.db.QueryRowContext(ctx, "SELECT f.cipher_nonce,f.ciphertext,f.forge_login,f.stored_at FROM workspace_forge_tokens f JOIN workspaces w ON w.id=f.workspace_id WHERE w.id=?", workspaceID).Scan(&nonce, &ciphertext, &r.ForgeLogin, &r.StoredAt)
	if err != nil {
		return r, identityNotFound(err)
	}
	r.Token, err = s.decryptForgeToken("workspace:"+workspaceID, nonce, ciphertext)
	r.Configured = err == nil
	return r, translateBackendConflict(err)
}
