package postgres

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
)

func TestMigration095RenamesWorkspaceRolesAndEnforcesFiveRoleVocabularyIntegration(t *testing.T) {
	databaseURL := integrationDatabaseURL(t)
	admin, err := pgxpool.New(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := "workspace_role_migration_" + strings.ReplaceAll(core.NewTaskID(), "-", "_")
	if _, err = admin.Exec(t.Context(), "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	poolConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	poolConfig.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(t.Context(), poolConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err = migrateControlPlaneToVersion(t.Context(), pool, 94); err != nil {
		t.Fatal(err)
	}

	st := newStore(pool)
	workspace := "role-migration-" + core.NewTaskID()
	ctx := store.WithWorkspace(t.Context(), workspace)
	if _, err = st.BootstrapWorkspaceConfig(ctx, isolationConfig(workspace)); err != nil {
		t.Fatal(err)
	}
	owner, err := st.queries.InsertIdentityUser(t.Context(), db.InsertIdentityUserParams{ID: "usr_owner_" + core.NewTaskID(), Email: "owner-" + core.NewTaskID() + "@example.test", DisplayName: "Owner"})
	if err != nil {
		t.Fatal(err)
	}
	member, err := st.queries.InsertIdentityUser(t.Context(), db.InsertIdentityUserParams{ID: "usr_member_" + core.NewTaskID(), Email: "member-" + core.NewTaskID() + "@example.test", DisplayName: "Member"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO workspace_role_bindings(workspace_id,user_id,role) VALUES($1,$2,'user')`, workspace, member.ID); err != nil {
		t.Fatal(err)
	}
	invitedEmail := "invited-" + core.NewTaskID() + "@example.test"
	if _, err = pool.Exec(ctx, `INSERT INTO workspace_membership_invitations(workspace_id,email,role,invited_by) VALUES($1,$2,'user',$3)`, workspace, invitedEmail, owner.ID); err != nil {
		t.Fatal(err)
	}
	if err = migrateControlPlaneToVersion(t.Context(), pool, 0); err != nil {
		t.Fatal(err)
	}

	for _, query := range []struct {
		name string
		sql  string
		arg  any
	}{
		{name: "binding", sql: `SELECT role FROM workspace_role_bindings WHERE workspace_id=$1 AND user_id=$2`, arg: member.ID},
		{name: "invitation", sql: `SELECT role FROM workspace_membership_invitations WHERE workspace_id=$1 AND email=$2`, arg: invitedEmail},
	} {
		var role string
		if err = pool.QueryRow(ctx, query.sql, workspace, query.arg).Scan(&role); err != nil || role != "contributor" {
			t.Fatalf("%s role=%q err=%v", query.name, role, err)
		}
	}
	for _, role := range []string{"viewer", "executor", "contributor", "maintainer", "operator"} {
		user, insertErr := st.queries.InsertIdentityUser(t.Context(), db.InsertIdentityUserParams{ID: "usr_" + role + "_" + core.NewTaskID(), Email: role + "-" + core.NewTaskID() + "@example.test", DisplayName: role})
		if insertErr != nil {
			t.Fatal(insertErr)
		}
		if _, insertErr = pool.Exec(ctx, `INSERT INTO workspace_role_bindings(workspace_id,user_id,role) VALUES($1,$2,$3)`, workspace, user.ID, role); insertErr != nil {
			t.Fatalf("role %q rejected: %v", role, insertErr)
		}
	}
	for _, role := range []string{"user", "owner", ""} {
		if _, insertErr := pool.Exec(ctx, `INSERT INTO workspace_role_bindings(workspace_id,user_id,role) VALUES($1,$2,$3)`, workspace, owner.ID, role); insertErr == nil {
			t.Fatalf("legacy or unknown role %q accepted", role)
		}
	}
}
