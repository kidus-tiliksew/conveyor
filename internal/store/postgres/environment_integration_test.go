package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

// ageEnvironmentKey moves one environment key's stored last use back by by.
func ageEnvironmentKey(t *testing.T, ctx context.Context, st *Store, keyID string, by time.Duration) {
	t.Helper()
	tag, err := st.pool.Exec(ctx, `UPDATE environment_keys SET last_used_at=last_used_at-$3::bigint*interval '1 microsecond' WHERE workspace_id=$1 AND id=$2 AND last_used_at IS NOT NULL`, workspace(ctx), keyID, by.Microseconds())
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("age environment key: rows=%d err=%v", tag.RowsAffected(), err)
	}
}

// storedEnvironmentKey reads one key row: its hash apart and every other
// column as text.
func storedEnvironmentKey(t *testing.T, ctx context.Context, st *Store, keyID string) storetest.EnvironmentStoredKey {
	t.Helper()
	var stored storetest.EnvironmentStoredKey
	if err := st.pool.QueryRow(ctx, `SELECT k.key_hash, (to_jsonb(k)-'key_hash')::text FROM environment_keys k WHERE k.workspace_id=$1 AND k.id=$2`, workspace(ctx), keyID).Scan(&stored.Digest, &stored.Text); err != nil {
		t.Fatal(err)
	}
	return stored
}

// TestEnvironmentMigrationIntegration proves migration 177: the taskless
// event CHECK admits the seven environment kinds and refuses an unknown one,
// and the tables hold the active-name and hash-size rules that PostgreSQL can
// hold (component-environments invariants 3, 9, and 11; component-persistence
// invariant 22). A second run changes nothing.
func TestEnvironmentMigrationIntegration(t *testing.T) {
	st := newIdentityIntegrationStore(t, 124)
	ctx := store.WithActor(store.WithWorkspace(t.Context(), "environments"), store.SystemActor())
	for range 2 {
		if err := Migrate(ctx, st.pool); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.BootstrapWorkspaceConfig(ctx, &config.Config{Workspace: "environments"}); err != nil {
		t.Fatal(err)
	}
	insertEvent := func(kind string) error {
		_, err := st.pool.Exec(ctx, `INSERT INTO events(workspace_id,kind,actor_id,actor_role,payload_json) VALUES('environments',$1,'system','system','{}')`, kind)
		return err
	}
	for _, kind := range []string{store.EnvironmentCreatedEvent, store.EnvironmentUpdatedEvent, store.EnvironmentArchivedEvent, store.EnvironmentRestoredEvent, store.EnvironmentKeyIssuedEvent, store.EnvironmentKeyRevokedEvent, store.EnvironmentSuggestionIgnoredEvent, store.CrateCreatedEvent, "config.updated"} {
		if err := insertEvent(kind); err != nil {
			t.Fatalf("kind %s refused: %v", kind, err)
		}
	}
	requireCode := func(err error, code, what string) {
		t.Helper()
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != code {
			t.Fatalf("%s err=%v, want SQLSTATE %s", what, err, code)
		}
	}
	requireCode(insertEvent("environment.deleted"), "23514", "an unknown environment kind")

	record, err := st.CreateEnvironment(ctx, store.EnvironmentInput{Name: "Production"})
	if err != nil {
		t.Fatal(err)
	}
	insert := func(id string, archived bool) error {
		_, err := st.pool.Exec(ctx, `INSERT INTO environments(workspace_id,id,name,name_fold,tracked_branches,github_environments,timing_window_minutes,archived,created_by,created_at) VALUES('environments',$1,'production',$2,'{}','[]',60,$3,'system',now())`, id, store.EnvironmentFold("production"), archived)
		return err
	}
	requireCode(insert("env-duplicate", false), "23505", "a second active name")
	if err := insert("env-archived", true); err != nil {
		t.Fatalf("an archived name: %v", err)
	}
	_, err = st.pool.Exec(ctx, `INSERT INTO environment_keys(workspace_id,id,environment_id,label,key_hash,created_by,created_at) VALUES('environments','envkey_short',$1,'short','\x00','system',now())`, record.ID)
	requireCode(err, "23514", "a hash that is not 32 bytes")
	_, err = st.pool.Exec(ctx, `INSERT INTO environment_keys(workspace_id,id,environment_id,label,key_hash,created_by,created_at) VALUES('environments','envkey_orphan','env-missing','orphan',sha256('x'),'system',now())`)
	requireCode(err, "23503", "a key of a missing environment")
}

// TestEnvironmentAuditFailureRollsBackIntegration proves that a mutation
// whose audit event fails records nothing (component-environments invariants
// 8 and 9; component-persistence invariant 5).
func TestEnvironmentAuditFailureRollsBackIntegration(t *testing.T) {
	st := newIdentityIntegrationStore(t, 124)
	ctx := store.WithActor(store.WithWorkspace(t.Context(), "environments"), store.SystemActor())
	if err := Migrate(ctx, st.pool); err != nil {
		t.Fatal(err)
	}
	if _, err := st.BootstrapWorkspaceConfig(ctx, &config.Config{Workspace: "environments"}); err != nil {
		t.Fatal(err)
	}
	record, err := st.CreateEnvironment(ctx, store.EnvironmentInput{Name: "Production"})
	if err != nil {
		t.Fatal(err)
	}
	key, err := st.IssueEnvironmentKey(ctx, record.ID, "deploy")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `ALTER TABLE events ADD CONSTRAINT environment_audit_refused CHECK (left(kind, 12) <> 'environment.') NOT VALID`); err != nil {
		t.Fatal(err)
	}
	archived := true
	for name, act := range map[string]func() error{
		"create": func() error {
			_, err := st.CreateEnvironment(ctx, store.EnvironmentInput{Name: "Staging"})
			return err
		},
		"archive": func() error {
			_, err := st.UpdateEnvironment(ctx, record.ID, store.EnvironmentPatch{Archived: &archived})
			return err
		},
		"issue": func() error {
			_, err := st.IssueEnvironmentKey(ctx, record.ID, "second")
			return err
		},
		"revoke": func() error { return st.RevokeEnvironmentKey(ctx, record.ID, key.ID) },
		"ignore": func() error { return st.IgnoreEnvironmentSuggestion(ctx, "preview") },
	} {
		if err := act(); !errors.Is(err, store.ErrBackendOperation) {
			t.Fatalf("%s err=%v, want the generic store failure", name, err)
		}
	}
	records, err := st.ListEnvironmentRecords(ctx, store.EnvironmentListQuery{})
	if err != nil || len(records) != 1 || records[0].Archived || len(records[0].Keys) != 1 {
		t.Fatalf("records=%+v err=%v, want the one unarchived environment with one key", records, err)
	}
	if _, err := st.CheckEnvironmentKey(ctx, key.Value); err != nil {
		t.Fatalf("the key after a failed revocation: %v", err)
	}
	var ignored int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM environment_suggestions`).Scan(&ignored); err != nil || ignored != 0 {
		t.Fatalf("suggestion rows=%d err=%v, want none", ignored, err)
	}
}
