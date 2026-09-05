package singlestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func TestKeyLocksIntegration(t *testing.T) {
	st := integrationStore(t)
	for _, mode := range []string{"transaction", "session"} {
		t.Run(mode, func(t *testing.T) {
			acquire := func(ctx context.Context, key string) (func() error, error) {
				if mode == "session" {
					return st.sessionLock(ctx, key)
				}
				tx, err := st.db.BeginTx(ctx, nil)
				if err != nil {
					return nil, err
				}
				if err = lockKey(ctx, tx, key); err != nil {
					tx.Rollback()
					return nil, err
				}
				return tx.Commit, nil
			}
			for _, same := range []bool{true, false} {
				t.Run(map[bool]string{true: "same-key-serializes", false: "different-keys-interleave"}[same], func(t *testing.T) {
					ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
					defer cancel()
					firstHeld := make(chan struct{})
					releaseFirst := make(chan struct{})
					firstDone := make(chan error, 1)
					key := t.Name()
					go func() {
						release, err := acquire(ctx, key)
						if err != nil {
							firstDone <- err
							return
						}
						close(firstHeld)
						select {
						case <-releaseFirst:
						case <-ctx.Done():
						}
						firstDone <- release()
					}()
					select {
					case <-firstHeld:
					case err := <-firstDone:
						t.Fatal(err)
					case <-ctx.Done():
						t.Fatal(ctx.Err())
					}
					secondKey := key
					if !same {
						secondKey += "-other"
					}
					secondHeld := make(chan struct{})
					secondDone := make(chan error, 1)
					go func() {
						release, err := acquire(ctx, secondKey)
						if err != nil {
							secondDone <- err
							return
						}
						close(secondHeld)
						secondDone <- release()
					}()
					if same {
						select {
						case <-secondHeld:
							t.Fatal("same-key critical sections overlapped")
						case err := <-secondDone:
							t.Fatal(err)
						case <-time.After(150 * time.Millisecond):
						}
					} else {
						select {
						case <-secondHeld:
						case err := <-secondDone:
							t.Fatal(err)
						case <-ctx.Done():
							t.Fatal("different keys did not interleave")
						}
					}
					close(releaseFirst)
					if err := <-firstDone; err != nil {
						t.Fatal(err)
					}
					if err := <-secondDone; err != nil {
						t.Fatal(err)
					}
				})
			}
		})
	}
}
func TestSessionLockCancellationAndCallbackCleanupIntegration(t *testing.T) {
	st := integrationStore(t)
	ctx, cancel := context.WithCancel(t.Context())
	release, err := st.sessionLock(ctx, "cancelled-owner")
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if err = release(); err != nil {
		t.Fatal(err)
	}
	next, err := st.sessionLock(t.Context(), "cancelled-owner")
	if err != nil {
		t.Fatal(err)
	}
	if err = next(); err != nil {
		t.Fatal(err)
	}
	held, err := st.sessionLock(t.Context(), "cancelled-waiter")
	if err != nil {
		t.Fatal(err)
	}
	defer held()
	waitCtx, stop := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer stop()
	if _, err = st.sessionLock(waitCtx, "cancelled-waiter"); err == nil {
		t.Fatal("cancelled waiter acquired held lock")
	}
	held()
	callbackError := errors.New("callback")
	scoped := store.WithWorkspace(t.Context(), "locks")
	for _, result := range []error{callbackError, nil, callbackError} {
		err = st.WithTaskSideEffectLock(scoped, "task", func(context.Context) error { return result })
		if !errors.Is(err, result) {
			t.Fatal(err)
		}
	}
	if count := st.db.Stats().InUse; count != 0 {
		t.Fatalf("leaked %d connections", count)
	}
}
func TestTransactionRollbackAndWorkspaceAtomicityIntegration(t *testing.T) {
	st := integrationStore(t)
	ctx := store.WithWorkspace(t.Context(), "atomic")
	cfg := &config.Config{Workspace: "atomic"}
	if _, err := st.BootstrapWorkspaceConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	callbackError := errors.New("rollback")
	err := st.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE workspaces SET name='uncommitted' WHERE id='atomic'`); err != nil {
			return err
		}
		return callbackError
	})
	if !errors.Is(err, callbackError) {
		t.Fatal(err)
	}
	w, err := st.GetWorkspace(ctx, "atomic")
	if err != nil || w.Name != "atomic" {
		t.Fatalf("rollback failed: %#v %v", w, err)
	}
	next := *cfg
	next.MaxBounces = 7
	receipt, err := st.UpdateWorkspaceConfig(ctx, 1, &next)
	if err != nil {
		t.Fatal(err)
	}
	var count int
	if err = st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE workspace_id=? AND id=? AND kind='config.updated'`, "atomic", receipt.EventID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("missing configuration audit: %d %v", count, err)
	}
	if _, err = st.UpdateWorkspaceConfig(ctx, 1, &next); !errors.Is(err, config.ErrVersionConflict) {
		t.Fatal(err)
	}
	if err = st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE workspace_id=?`, "atomic").Scan(&count); err != nil || count != 1 {
		t.Fatalf("stale update appended audit: %d %v", count, err)
	}
}

func TestMigrationRefusalsIntegration(t *testing.T) {
	st := integrationStore(t)
	files, err := migrations()
	if err != nil {
		t.Fatal(err)
	}
	file := files[0]
	for _, tc := range []struct {
		name, query string
		args        []any
	}{
		{"newer", `INSERT INTO conveyor_singlestore_migrations(version,name,checksum) VALUES(99999,'future','future')`, nil},
		{"name", `UPDATE conveyor_singlestore_migrations SET name='changed' WHERE version=?`, []any{file.version}},
		{"checksum", `UPDATE conveyor_singlestore_migrations SET checksum='changed' WHERE version=?`, []any{file.version}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := st.db.ExecContext(t.Context(), tc.query, tc.args...); err != nil {
				t.Fatal(err)
			}
			if err := st.migrate(t.Context()); err == nil {
				t.Fatal("mismatched ledger accepted")
			}
			if _, err := st.db.ExecContext(t.Context(), `DELETE FROM conveyor_singlestore_migrations WHERE version=99999`); err != nil {
				t.Fatal(err)
			}
			if _, err := st.db.ExecContext(t.Context(), `UPDATE conveyor_singlestore_migrations SET name=?,checksum=? WHERE version=?`, file.name, file.checksum, file.version); err != nil {
				t.Fatal(err)
			}
		})
	}
	if err := st.migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
}
func TestPartialBackendRefusesPopulatedProjectionIntegration(t *testing.T) {
	st := integrationStore(t)
	ctx := store.WithWorkspace(t.Context(), "partial")
	if _, err := st.db.ExecContext(ctx, `INSERT INTO features(workspace_id,id,name) VALUES('partial','feature','existing')`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ListFeatures(ctx); !errors.Is(err, store.ErrNotImplemented) {
		t.Fatalf("populated projection returned false success: %v", err)
	}
}

func TestGoUniqueWriteRulesSerializeIntegration(t *testing.T) {
	st := integrationStore(t)
	for _, kind := range []string{"reference", "deployment"} {
		t.Run(kind, func(t *testing.T) {
			start := make(chan struct{})
			results := make(chan error, 2)
			for i := range 2 {
				go func() {
					<-start
					results <- st.withTx(t.Context(), func(tx *sql.Tx) error {
						now := time.Now().UTC()
						var w rowWrite
						if kind == "reference" {
							name := "Same"
							if i == 1 {
								name = "same"
							}
							w = rowWrite{table: "reference_documents", operation: "INSERT", values: map[string]any{"workspace_id": "rules", "id": fmt.Sprintf("ref-%d", i), "name": name, "deleted_at": nil, "current_version": 1, "created_at": now, "updated_at": now}}
						} else {
							w = rowWrite{table: "user_tokens", operation: "INSERT", values: map[string]any{"id": fmt.Sprintf("token-%d", i), "user_id": "fixture", "token_hash": []byte(fmt.Sprintf("synthetic-hash-%d", i)), "deployment_credential": true}}
						}
						_, err := writeRow(t.Context(), tx, w)
						return err
					})
				}()
			}
			close(start)
			successes := 0
			for range 2 {
				err := <-results
				if err == nil {
					successes++
				} else if kind == "reference" && !errors.Is(err, store.ErrReferenceDocumentNameConflict) {
					t.Fatal(err)
				}
			}
			if successes != 1 {
				t.Fatalf("%s winners=%d", kind, successes)
			}
		})
	}
}
func TestConcurrentStartupAndConnectionSettingsIntegration(t *testing.T) {
	st := integrationStore(t)
	results := make(chan error, 2)
	for range 2 {
		go func() { results <- st.migrate(t.Context()) }()
	}
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	var zone, mode string
	if err := st.db.QueryRowContext(t.Context(), `SELECT @@system_time_zone,@@sql_mode`).Scan(&zone, &mode); err != nil {
		t.Fatal(err)
	}
	if zone != "UTC" || !strings.Contains(mode, "STRICT_ALL_TABLES") {
		t.Fatalf("settings: %s %s", zone, mode)
	}
	if st.db.Stats().MaxOpenConnections != 16 {
		t.Fatal("unbounded pool")
	}
	st.Close()
	if err := st.db.PingContext(t.Context()); err == nil {
		t.Fatal("closed pool remained usable")
	}
}

func TestNamedJobConflictCrossesTransactionBoundaryIntegration(t *testing.T) {
	st := integrationStore(t)
	query := `INSERT INTO jobs(workspace_id,id,task_id,stage,harness,runner,confinement_tier,state) VALUES('conflict','job','task','implement','fixture','fixture','fixture','pending')`
	if _, err := st.db.ExecContext(t.Context(), query); err != nil {
		t.Fatal(err)
	}
	err := st.withTx(t.Context(), func(tx *sql.Tx) error { _, err := tx.ExecContext(t.Context(), query); return err })
	if !errors.Is(err, store.ErrDispatchJobConflict) {
		t.Fatalf("named job conflict escaped: %v", err)
	}
}
