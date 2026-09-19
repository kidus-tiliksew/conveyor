package postgres

import (
	"bytes"
	"fmt"
	"github.com/kidus-tiliksew/conveyor/internal/config"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/testimage"
)

func TestArtifactIntakeWriteFailuresRollbackIntegration(t *testing.T) {
	for _, table := range []string{"artifact_links", "events", "event_log"} {
		t.Run(table, func(t *testing.T) {
			st, ctx, ws := newPhase61IntegrationStore(t)
			t.Cleanup(st.Close)
			id := core.NewTaskID()
			task := phase61Task(ws, id, core.TaskQueued, "")
			// This trigger touches only the disposable fixture workspace, including
			// queue append failure after task, attachment and audit writes.
			function := "artifact_failure_" + table
			triggerSQL := fmt.Sprintf(`CREATE OR REPLACE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.workspace_id = '%s' THEN RAISE EXCEPTION 'injected artifact intake failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER %s BEFORE INSERT ON %s FOR EACH ROW EXECUTE FUNCTION %s()`, function, ws, function, table, function)
			if _, err := st.pool.Exec(ctx, triggerSQL); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				_, _ = st.pool.Exec(ctx, fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON %s; DROP FUNCTION IF EXISTS %s()`, function, table, function))
			})
			uploads := []store.ArtifactUpload{{Name: "one.jpg", ContentType: "image/jpeg", Content: testimage.JPEG("one")}, {Name: "two.jpg", ContentType: "image/jpeg", Content: testimage.JPEG("two")}}
			if err := st.CreateTaskWithAttachments(ctx, task, nil, store.TaskContextInput{}, uploads); err == nil {
				t.Fatal("injected failure succeeded")
			}
			for _, relation := range []string{"tasks", "artifacts", "artifact_links", "events", "event_log"} {
				var count int
				query := fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE workspace_id=$1`, relation)
				if relation == "events" {
					query += ` AND task_id=$2`
					if err := st.pool.QueryRow(ctx, query, ws, id).Scan(&count); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := st.pool.QueryRow(ctx, query, ws).Scan(&count); err != nil {
						t.Fatal(err)
					}
				}
				if count != 0 {
					t.Fatalf("%s retained %d rows after %s failure", relation, count, table)
				}
			}
			if _, err := st.pool.Exec(ctx, fmt.Sprintf(`DROP TRIGGER %s ON %s; DROP FUNCTION %s()`, function, table, function)); err != nil {
				t.Fatal(err)
			}
			if err := st.CreateTaskWithAttachments(ctx, task, nil, store.TaskContextInput{}, uploads); err != nil {
				t.Fatalf("retry: %v", err)
			}
		})
	}
}

func TestArtifactRepairMigrationPreservesHistoricalMetadataIntegration(t *testing.T) {
	st := newIdentityIntegrationStore(t, 129)
	ws := "artifact-migration-" + core.NewTaskID()
	ctx := store.WithWorkspace(t.Context(), ws)
	if _, err := st.BootstrapWorkspaceConfig(ctx, &config.Config{Workspace: ws}); err != nil {
		t.Fatal(err)
	}
	content := testimage.JPEG("historical")
	a, err := st.CreateArtifact(ctx, core.Artifact{Name: "historical.png", ContentType: "image/jpeg"}, content)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.pool.Exec(ctx, `UPDATE artifacts SET content_type='image/png' WHERE workspace_id=$1 AND id=$2`, ws, a.ID); err != nil {
		t.Fatal(err)
	}
	if err = migrateControlPlaneToVersion(ctx, st.pool, 0); err != nil {
		t.Fatal(err)
	}
	got, raw, err := st.GetArtifact(ctx, a.ID)
	if err != nil || got.ContentType != "image/png" || !bytes.Equal(raw, content) {
		t.Fatalf("migration changed historical artifact: %+v %v", got, err)
	}
	var receipts int
	if err = st.pool.QueryRow(ctx, `SELECT COUNT(*) FROM artifact_metadata_repairs WHERE workspace_id=$1`, ws).Scan(&receipts); err != nil || receipts != 0 {
		t.Fatalf("migration manufactured receipts: %d %v", receipts, err)
	}
}
