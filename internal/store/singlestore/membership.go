package singlestore

// DEC-38: this aggregate follows component-identity-membership and component-persistence.

import (
	"context"
	"database/sql"
	"errors"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// AuthorizeDeployment derives deployment authority from the active account's
// current operator bindings. Persisted credential scope is only a protocol
// boundary and never sufficient authority on its own.
func (s *Store) AuthorizeDeployment(ctx context.Context, userID string, capability core.Capability) (bool, error) {
	// Deployment-scoped calls are restricted to live workspace operators, but
	// they may exercise any capability in the operator bundle. This preserves
	// legacy/non-workspace-scoped factory operations while keeping capabilities
	// absent from lower roles behind the operator boundary.
	if !core.RoleAllows(core.WorkspaceRoleOperator, capability) {
		return false, nil
	}
	var allowed bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1
		FROM users u
		JOIN workspace_role_bindings b ON b.user_id=u.id
		WHERE u.id=? AND u.status='active' AND b.role='operator'
	)`, userID).Scan(&allowed)
	return allowed, translateBackendConflict(err)
}

func (s *Store) AuthorizeWorkspace(ctx context.Context, userID, workspaceID string, capability core.Capability) (bool, error) {
	var role core.WorkspaceRole
	err := s.db.QueryRowContext(ctx, `SELECT role FROM workspace_role_bindings WHERE workspace_id=? AND user_id=?`, workspaceID, userID).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, translateBackendConflict(err)
	}
	return core.RoleAllows(role, capability), nil
}

func (s *Store) ListWorkspacesForUser(ctx context.Context, userID string) ([]core.Workspace, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT w.id,w.name,w.config_version,w.created_at
		FROM workspaces w JOIN workspace_role_bindings b ON b.workspace_id=w.id
		WHERE b.user_id=? ORDER BY lower(w.name),w.id`, userID)
	if err != nil {
		return nil, translateBackendConflict(err)
	}
	defer rows.Close()
	var result []core.Workspace
	for rows.Next() {
		var item core.Workspace
		if err := rows.Scan(&item.ID, &item.Name, &item.ConfigVersion, &item.CreatedAt); err != nil {
			return nil, translateBackendConflict(err)
		}
		result = append(result, item)
	}
	return result, translateBackendConflict(rows.Err())
}

func (s *Store) ListWorkspaceMembers(ctx context.Context, requesterUserID, workspaceID string) ([]core.WorkspaceMembership, error) {
	allowed, err := s.AuthorizeWorkspace(ctx, requesterUserID, workspaceID, core.CapabilityViewWorkspace)
	if err != nil || !allowed {
		return nil, translateBackendConflict(err)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT b.workspace_id,b.user_id,u.email,u.display_name,b.role,b.created_at
		FROM workspace_role_bindings b JOIN users u ON u.id=b.user_id
		WHERE b.workspace_id=? ORDER BY lower(u.display_name),u.id`, workspaceID)
	if err != nil {
		return nil, translateBackendConflict(err)
	}
	defer rows.Close()
	var result []core.WorkspaceMembership
	for rows.Next() {
		var item core.WorkspaceMembership
		if err := rows.Scan(&item.WorkspaceID, &item.UserID, &item.Email, &item.DisplayName, &item.Role, &item.CreatedAt); err != nil {
			return nil, translateBackendConflict(err)
		}
		result = append(result, item)
	}
	return result, translateBackendConflict(rows.Err())
}

// ListWorkspaceInvitations returns the workspace's unredeemed invitations. Rows
// are deleted on redemption and on revocation, so the table holds pending
// invitations only and the read needs no status predicate. The caller is
// authorized at the HTTP capability boundary, like RevokeWorkspaceInvitation.
func (s *Store) ListWorkspaceInvitations(ctx context.Context, workspaceID string) ([]core.WorkspaceInvitation, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT i.workspace_id,i.email,i.role,i.invited_by,COALESCE(u.display_name,''),i.created_at
		FROM workspace_membership_invitations i LEFT JOIN users u ON u.id=i.invited_by
		WHERE i.workspace_id=? ORDER BY i.created_at DESC,i.email`, workspaceID)
	if err != nil {
		return nil, translateBackendConflict(err)
	}
	defer rows.Close()
	var result []core.WorkspaceInvitation
	for rows.Next() {
		var item core.WorkspaceInvitation
		if err := rows.Scan(&item.WorkspaceID, &item.Email, &item.Role, &item.InvitedBy, &item.InvitedByDisplayName, &item.CreatedAt); err != nil {
			return nil, translateBackendConflict(err)
		}
		result = append(result, item)
	}
	return result, translateBackendConflict(rows.Err())
}

func (s *Store) GrantWorkspaceRole(ctx context.Context, email, workspaceID string, role core.WorkspaceRole) (core.MembershipGrant, error) {
	email, err := normalizeIdentityEmail(email)
	if err != nil {
		return core.MembershipGrant{}, translateBackendConflict(err)
	}
	if !core.RoleAllows(role, core.CapabilityViewWorkspace) {
		return core.MembershipGrant{}, errors.New("role must be viewer, executor, contributor, maintainer, or operator")
	}
	c, ok := store.CredentialFromContext(ctx)
	if !ok {
		return core.MembershipGrant{}, errors.New("authenticated user credential is required")
	}
	r := core.MembershipGrant{Email: email, Role: role}
	err = s.identityTx(ctx, func(tx *sql.Tx) error {
		if err := identityWorkspace(ctx, tx, workspaceID); err != nil {
			return translateBackendConflict(err)
		}
		var actorID string
		if err := tx.QueryRowContext(ctx, "SELECT id FROM users WHERE id=?", c.OwnerUserID).Scan(&actorID); err != nil {
			return identityNotFound(err)
		}
		var uid string
		lookup := tx.QueryRowContext(ctx, "SELECT id FROM users WHERE email=? AND status='active'", email).Scan(&uid)
		invitation := errors.Is(lookup, sql.ErrNoRows)
		if lookup != nil && !invitation {
			return translateBackendConflict(lookup)
		}
		if invitation {
			if _, err := tx.ExecContext(ctx, "INSERT INTO workspace_membership_invitations(workspace_id,email,role,invited_by) VALUES(?,?,?,?) ON DUPLICATE KEY UPDATE role=VALUES(role),invited_by=VALUES(invited_by),created_at=CURRENT_TIMESTAMP(6)", workspaceID, email, role, c.OwnerUserID); err != nil {
				return translateBackendConflict(err)
			}
		} else {
			var previous core.WorkspaceRole
			e := tx.QueryRowContext(ctx, "SELECT role FROM workspace_role_bindings WHERE workspace_id=? AND user_id=?", workspaceID, uid).Scan(&previous)
			if e != nil && !errors.Is(e, sql.ErrNoRows) {
				return translateBackendConflict(e)
			}
			if previous == core.WorkspaceRoleOperator && role != previous {
				if err := guardLastOperator(ctx, tx, workspaceID); err != nil {
					return translateBackendConflict(err)
				}
			}
			if _, err := tx.ExecContext(ctx, "INSERT INTO workspace_role_bindings(workspace_id,user_id,role) VALUES(?,?,?) ON DUPLICATE KEY UPDATE role=VALUES(role),updated_at=CURRENT_TIMESTAMP(6)", workspaceID, uid, role); err != nil {
				return translateBackendConflict(err)
			}
			if core.RoleAllows(previous, core.CapabilityClaimWork) && !core.RoleAllows(role, core.CapabilityClaimWork) {
				if err := clearMemberAssignments(ctx, tx, workspaceID, uid); err != nil {
					return translateBackendConflict(err)
				}
				if err := revokeOwnedWorkers(ctx, tx, uid, workspaceID, "workspace_membership_demoted"); err != nil {
					return translateBackendConflict(err)
				}
			}
			if _, err := tx.ExecContext(ctx, "DELETE FROM workspace_membership_invitations WHERE workspace_id=? AND email=?", workspaceID, email); err != nil {
				return translateBackendConflict(err)
			}
		}
		_, err := appendWorkspaceEvent(ctx, tx, workspaceID, "workspace.membership_granted", map[string]any{"workspace_id": workspaceID, "email": email, "role": role, "invitation": invitation, "granted_by": c.OwnerUserID})
		return translateBackendConflict(err)
	})
	return r, translateBackendConflict(err)
}
func guardLastOperator(ctx context.Context, tx *sql.Tx, ws string) error {
	var count int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM workspace_role_bindings WHERE workspace_id=? AND role='operator'", ws).Scan(&count); err != nil {
		return translateBackendConflict(err)
	}
	if count <= 1 {
		return store.ErrLastWorkspaceOperator
	}
	return nil
}
func (s *Store) RevokeWorkspaceInvitation(ctx context.Context, email, workspaceID string) error {
	email, err := normalizeIdentityEmail(email)
	if err != nil {
		return translateBackendConflict(err)
	}
	c, ok := store.CredentialFromContext(ctx)
	if !ok {
		return errors.New("authenticated user credential is required")
	}
	return s.identityTx(ctx, func(tx *sql.Tx) error {
		r, err := tx.ExecContext(ctx, "DELETE FROM workspace_membership_invitations WHERE workspace_id=? AND email=?", workspaceID, email)
		if err != nil {
			return translateBackendConflict(err)
		}
		n, err := r.RowsAffected()
		if err != nil {
			return translateBackendConflict(err)
		}
		if n == 0 {
			return store.ErrNotFound
		}
		_, err = appendWorkspaceEvent(ctx, tx, workspaceID, "workspace.membership_revoked", map[string]any{"workspace_id": workspaceID, "email": email, "invitation": true, "revoked_by": c.OwnerUserID})
		return translateBackendConflict(err)
	})
}
func (s *Store) RevokeWorkspaceRole(ctx context.Context, userID, workspaceID string) error {
	return s.identityTx(ctx, func(tx *sql.Tx) error {
		if err := identityWorkspace(ctx, tx, workspaceID); err != nil {
			return translateBackendConflict(err)
		}
		var role core.WorkspaceRole
		if err := tx.QueryRowContext(ctx, "SELECT role FROM workspace_role_bindings WHERE workspace_id=? AND user_id=?", workspaceID, userID).Scan(&role); err != nil {
			return identityNotFound(err)
		}
		if role == core.WorkspaceRoleOperator {
			if err := guardLastOperator(ctx, tx, workspaceID); err != nil {
				return translateBackendConflict(err)
			}
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM workspace_role_bindings WHERE workspace_id=? AND user_id=?", workspaceID, userID); err != nil {
			return translateBackendConflict(err)
		}
		if err := clearMemberAssignments(ctx, tx, workspaceID, userID); err != nil {
			return translateBackendConflict(err)
		}
		if err := revokeOwnedWorkers(ctx, tx, userID, workspaceID, "workspace_membership_revoked"); err != nil {
			return translateBackendConflict(err)
		}
		_, err := appendWorkspaceEvent(ctx, tx, workspaceID, "workspace.membership_revoked", map[string]any{"workspace_id": workspaceID, "user_id": userID})
		return translateBackendConflict(err)
	})
}

// These cascades belong to the membership command; no sibling task APIs are implemented.
func clearMemberAssignments(ctx context.Context, tx *sql.Tx, ws, uid string) error {
	rows, err := tx.QueryContext(ctx, "SELECT id FROM tasks WHERE workspace_id=? AND assignee_user_id=? FOR UPDATE", ws, uid)
	if err != nil {
		return translateBackendConflict(err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return translateBackendConflict(err)
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return translateBackendConflict(err)
	}
	for _, id := range ids {
		if _, err = tx.ExecContext(ctx, "UPDATE tasks SET assignee_user_id=NULL WHERE workspace_id=? AND id=?", ws, id); err != nil {
			return translateBackendConflict(err)
		}
		actor := store.ActorFromContext(ctx)
		if _, err = writeRow(ctx, tx, rowWrite{table: "events", operation: "INSERT", values: map[string]any{"workspace_id": ws, "task_id": id, "kind": "task.assignee.cleared", "actor_id": actor.ID, "actor_role": string(actor.Role), "payload_json": []byte(core.JSONPayload(map[string]any{"assignee_user_id": "", "revoked_user_id": uid}))}}); err != nil {
			return translateBackendConflict(err)
		}
	}
	return nil
}
func revokeOwnedWorkers(ctx context.Context, tx *sql.Tx, uid, ws, reason string) error {
	query := "SELECT id,workspace_id FROM workers WHERE owner_user_id=? AND revoked_at IS NULL"
	args := []any{uid}
	if ws != "" {
		query += " AND workspace_id=?"
		args = append(args, ws)
	}
	query += " FOR UPDATE"
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return translateBackendConflict(err)
	}
	type worker struct{ id, ws string }
	var items []worker
	for rows.Next() {
		var w worker
		if err = rows.Scan(&w.id, &w.ws); err != nil {
			rows.Close()
			return translateBackendConflict(err)
		}
		items = append(items, w)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return translateBackendConflict(err)
	}
	for _, w := range items {
		if _, err = tx.ExecContext(ctx, "UPDATE workers SET revoked_at=CURRENT_TIMESTAMP(6),lease_expires_at=NULL WHERE workspace_id=? AND id=?", w.ws, w.id); err != nil {
			return translateBackendConflict(err)
		}
		if _, err = appendWorkspaceEvent(ctx, tx, w.ws, "worker.revoked", map[string]any{"worker_id": w.id, "owner_user_id": uid, "reason": reason}); err != nil {
			return translateBackendConflict(err)
		}
	}
	return nil
}
