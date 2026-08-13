package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
)

// AuthorizeDeployment derives deployment authority from the active account's
// current operator bindings. Persisted credential scope is only a protocol
// boundary and never sufficient authority on its own.
func (s *Store) AuthorizeDeployment(ctx context.Context, userID string, capability core.Capability) (bool, error) {
	if capability != core.CapabilityOperateGates {
		return false, nil
	}
	var allowed bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1
		FROM users u
		JOIN workspace_role_bindings b ON b.user_id=u.id
		WHERE u.id=$1 AND u.status='active' AND b.role='operator'
	)`, userID).Scan(&allowed)
	return allowed, err
}

func (s *Store) AuthorizeWorkspace(ctx context.Context, userID, workspaceID string, capability core.Capability) (bool, error) {
	var role core.WorkspaceRole
	err := s.pool.QueryRow(ctx, `SELECT role FROM workspace_role_bindings WHERE workspace_id=$1 AND user_id=$2`, workspaceID, userID).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return core.RoleAllows(role, capability), nil
}

func (s *Store) ListWorkspacesForUser(ctx context.Context, userID string) ([]core.Workspace, error) {
	rows, err := s.pool.Query(ctx, `SELECT w.id,w.name,w.config_version,w.created_at
		FROM workspaces w JOIN workspace_role_bindings b ON b.workspace_id=w.id
		WHERE b.user_id=$1 ORDER BY lower(w.name),w.id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []core.Workspace
	for rows.Next() {
		var item core.Workspace
		if err := rows.Scan(&item.ID, &item.Name, &item.ConfigVersion, &item.CreatedAt); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) ListWorkspaceMembers(ctx context.Context, requesterUserID, workspaceID string) ([]core.WorkspaceMembership, error) {
	allowed, err := s.AuthorizeWorkspace(ctx, requesterUserID, workspaceID, core.CapabilityViewWorkspace)
	if err != nil || !allowed {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT b.workspace_id,b.user_id,u.email,u.display_name,b.role,b.created_at
		FROM workspace_role_bindings b JOIN users u ON u.id=b.user_id
		WHERE b.workspace_id=$1 ORDER BY lower(u.display_name),u.id`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []core.WorkspaceMembership
	for rows.Next() {
		var item core.WorkspaceMembership
		if err := rows.Scan(&item.WorkspaceID, &item.UserID, &item.Email, &item.DisplayName, &item.Role, &item.CreatedAt); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

// ListWorkspaceInvitations returns the workspace's unredeemed invitations. Rows
// are deleted on redemption and on revocation, so the table holds pending
// invitations only and the read needs no status predicate. The caller is
// authorized at the HTTP capability boundary, like RevokeWorkspaceInvitation.
func (s *Store) ListWorkspaceInvitations(ctx context.Context, workspaceID string) ([]core.WorkspaceInvitation, error) {
	rows, err := s.pool.Query(ctx, `SELECT i.workspace_id,i.email,i.role,i.invited_by,COALESCE(u.display_name,''),i.created_at
		FROM workspace_membership_invitations i LEFT JOIN users u ON u.id=i.invited_by
		WHERE i.workspace_id=$1 ORDER BY i.created_at DESC,i.email`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []core.WorkspaceInvitation
	for rows.Next() {
		var item core.WorkspaceInvitation
		if err := rows.Scan(&item.WorkspaceID, &item.Email, &item.Role, &item.InvitedBy, &item.InvitedByDisplayName, &item.CreatedAt); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) GrantWorkspaceRole(ctx context.Context, email, workspaceID string, role core.WorkspaceRole) (core.MembershipGrant, error) {
	email, err := normalizeIdentityEmail(email)
	if err != nil {
		return core.MembershipGrant{}, err
	}
	if role != core.WorkspaceRoleUser && role != core.WorkspaceRoleOperator {
		return core.MembershipGrant{}, errors.New("role must be user or operator")
	}
	credential, ok := store.CredentialFromContext(ctx)
	if !ok {
		return core.MembershipGrant{}, errors.New("authenticated user credential is required")
	}
	result := core.MembershipGrant{Email: email, Role: role}
	err = s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		if err := lockIdentityEmail(ctx, tx, email); err != nil {
			return err
		}
		var userID string
		lookupErr := tx.QueryRow(ctx, `SELECT id FROM users WHERE email=$1 AND status='active'`, email).Scan(&userID)
		if errors.Is(lookupErr, pgx.ErrNoRows) {
			_, insertErr := tx.Exec(ctx, `INSERT INTO workspace_membership_invitations(workspace_id,email,role,invited_by)
				VALUES($1,$2,$3,$4) ON CONFLICT(workspace_id,email) DO UPDATE SET role=excluded.role,invited_by=excluded.invited_by,created_at=now()`, workspaceID, email, role, credential.OwnerUserID)
			if insertErr != nil {
				return insertErr
			}
		} else if lookupErr != nil {
			return lookupErr
		} else {
			if _, err := tx.Exec(ctx, `INSERT INTO workspace_role_bindings(workspace_id,user_id,role)
				VALUES($1,$2,$3) ON CONFLICT(workspace_id,user_id) DO UPDATE SET role=excluded.role,updated_at=now()
			`, workspaceID, userID, role); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `DELETE FROM workspace_membership_invitations WHERE workspace_id=$1 AND email=$2`, workspaceID, email); err != nil {
				return err
			}
		}
		return insertWorkspaceEvent(ctx, q, core.Event{Kind: "workspace.membership_granted", Payload: core.JSONPayload(map[string]any{
			"workspace_id": workspaceID, "email": email, "role": role, "invitation": errors.Is(lookupErr, pgx.ErrNoRows),
			"granted_by": credential.OwnerUserID,
		})})
	})
	return result, err
}

// RevokeWorkspaceInvitation removes only an unredeemed invitation. The caller
// is authorized at the HTTP capability boundary and still must carry the
// authenticated operator credential used for the audit record.
func (s *Store) RevokeWorkspaceInvitation(ctx context.Context, email, workspaceID string) error {
	email, err := normalizeIdentityEmail(email)
	if err != nil {
		return err
	}
	credential, ok := store.CredentialFromContext(ctx)
	if !ok {
		return errors.New("authenticated user credential is required")
	}
	return s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		if err := lockIdentityEmail(ctx, tx, email); err != nil {
			return err
		}
		result, err := tx.Exec(ctx, `DELETE FROM workspace_membership_invitations WHERE workspace_id=$1 AND email=$2`, workspaceID, email)
		if err != nil {
			return err
		}
		if result.RowsAffected() == 0 {
			return fmt.Errorf("%w: workspace membership invitation", store.ErrNotFound)
		}
		return insertWorkspaceEvent(ctx, q, core.Event{Kind: "workspace.membership_revoked", Payload: core.JSONPayload(map[string]any{
			"workspace_id": workspaceID, "email": email, "invitation": true, "revoked_by": credential.OwnerUserID,
		})})
	})
}

func (s *Store) RevokeWorkspaceRole(ctx context.Context, userID, workspaceID string) error {
	return s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		// Serialize membership revocations per workspace so concurrent operator
		// removals cannot each observe another operator and leave none behind.
		var lockedWorkspace string
		if err := tx.QueryRow(ctx, `SELECT id FROM workspaces WHERE id=$1 FOR UPDATE`, workspaceID).Scan(&lockedWorkspace); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("%w: workspace membership", store.ErrNotFound)
			}
			return err
		}
		var role core.WorkspaceRole
		if err := tx.QueryRow(ctx, `SELECT role FROM workspace_role_bindings WHERE workspace_id=$1 AND user_id=$2`, workspaceID, userID).Scan(&role); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("%w: workspace membership", store.ErrNotFound)
			}
			return err
		}
		if role == core.WorkspaceRoleOperator {
			var operators int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM workspace_role_bindings WHERE workspace_id=$1 AND role=$2`, workspaceID, core.WorkspaceRoleOperator).Scan(&operators); err != nil {
				return err
			}
			if operators <= 1 {
				return store.ErrLastWorkspaceOperator
			}
		}
		result, err := tx.Exec(ctx, `DELETE FROM workspace_role_bindings WHERE workspace_id=$1 AND user_id=$2`, workspaceID, userID)
		if err != nil {
			return err
		}
		if result.RowsAffected() == 0 {
			return fmt.Errorf("%w: workspace membership", store.ErrNotFound)
		}
		rows, err := tx.Query(ctx, `UPDATE tasks SET assignee_user_id=NULL
			WHERE workspace_id=$1 AND assignee_user_id=$2
			RETURNING id`, workspaceID, userID)
		if err != nil {
			return err
		}
		var taskIDs []string
		for rows.Next() {
			var taskID string
			if err = rows.Scan(&taskID); err != nil {
				rows.Close()
				return err
			}
			taskIDs = append(taskIDs, taskID)
		}
		if err = rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		if err := revokeOwnedWorkersTx(ctx, tx, q, userID, workspaceID, "workspace_membership_revoked"); err != nil {
			return err
		}
		for _, taskID := range taskIDs {
			if err = insertEvent(ctx, q, core.Event{TaskID: taskID, Kind: "task.assignee.cleared", Payload: core.JSONPayload(map[string]any{
				"assignee_user_id": "", "revoked_user_id": userID,
			})}); err != nil {
				return err
			}
		}
		return insertWorkspaceEvent(ctx, q, core.Event{Kind: "workspace.membership_revoked", Payload: core.JSONPayload(map[string]any{
			"workspace_id": workspaceID, "user_id": userID,
		})})
	})
}
