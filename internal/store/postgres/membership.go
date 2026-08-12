package postgres

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
)

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

func (s *Store) GrantWorkspaceRole(ctx context.Context, email, workspaceID string, role core.WorkspaceRole) (core.MembershipGrant, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	address, err := mail.ParseAddress(email)
	if err != nil || address.Address != email || !strings.Contains(email, "@") {
		return core.MembershipGrant{}, errors.New("email must be a valid normalized address")
	}
	if role != core.WorkspaceRoleUser && role != core.WorkspaceRoleOperator {
		return core.MembershipGrant{}, errors.New("role must be user or operator")
	}
	credential, ok := store.CredentialFromContext(ctx)
	if !ok {
		return core.MembershipGrant{}, errors.New("authenticated user credential is required")
	}
	var result core.MembershipGrant
	err = s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
		var userID, displayName string
		lookupErr := tx.QueryRow(ctx, `SELECT id,display_name FROM users WHERE email=$1 AND status='active'`, email).Scan(&userID, &displayName)
		if errors.Is(lookupErr, pgx.ErrNoRows) {
			_, insertErr := tx.Exec(ctx, `INSERT INTO workspace_membership_invitations(workspace_id,email,role,invited_by)
				VALUES($1,$2,$3,$4) ON CONFLICT(workspace_id,email) DO UPDATE SET role=excluded.role,invited_by=excluded.invited_by,created_at=now()`, workspaceID, email, role, credential.OwnerUserID)
			if insertErr != nil {
				return insertErr
			}
			result.InvitationEmail = email
		} else if lookupErr != nil {
			return lookupErr
		} else {
			var createdAt time.Time
			if err := tx.QueryRow(ctx, `INSERT INTO workspace_role_bindings(workspace_id,user_id,role)
				VALUES($1,$2,$3) ON CONFLICT(workspace_id,user_id) DO UPDATE SET role=excluded.role,updated_at=now()
				RETURNING created_at`, workspaceID, userID, role).Scan(&createdAt); err != nil {
				return err
			}
			_, _ = tx.Exec(ctx, `DELETE FROM workspace_membership_invitations WHERE workspace_id=$1 AND email=$2`, workspaceID, email)
			result.Membership = &core.WorkspaceMembership{WorkspaceID: workspaceID, UserID: userID, Email: email, DisplayName: displayName, Role: role, CreatedAt: createdAt}
		}
		return insertWorkspaceEvent(ctx, q, core.Event{Kind: "workspace.membership_granted", Payload: core.JSONPayload(map[string]any{
			"workspace_id": workspaceID, "email": email, "role": role, "invitation": result.Membership == nil,
		})})
	})
	return result, err
}

func (s *Store) RevokeWorkspaceRole(ctx context.Context, userID, workspaceID string) error {
	return s.inTx(ctx, func(tx pgx.Tx, q *db.Queries) error {
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
