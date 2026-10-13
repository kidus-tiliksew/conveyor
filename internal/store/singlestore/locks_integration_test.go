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
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
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
					ctx, cancel := context.WithTimeout(store.WithActor(t.Context(), store.SystemActor()), 5*time.Second)
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
	ctx, cancel := context.WithCancel(store.WithActor(t.Context(), store.SystemActor()))
	release, err := st.sessionLock(ctx, "cancelled-owner")
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if err = release(); err != nil {
		t.Fatal(err)
	}
	next, err := st.sessionLock(store.WithActor(t.Context(), store.SystemActor()), "cancelled-owner")
	if err != nil {
		t.Fatal(err)
	}
	if err = next(); err != nil {
		t.Fatal(err)
	}
	held, err := st.sessionLock(store.WithActor(t.Context(), store.SystemActor()), "cancelled-waiter")
	if err != nil {
		t.Fatal(err)
	}
	defer held()
	waitCtx, stop := context.WithTimeout(store.WithActor(t.Context(), store.SystemActor()), 100*time.Millisecond)
	defer stop()
	if _, err = st.sessionLock(waitCtx, "cancelled-waiter"); err == nil {
		t.Fatal("cancelled waiter acquired held lock")
	}
	held()
	callbackError := errors.New("callback")
	scoped := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "locks")
	for _, result := range []error{callbackError, nil, callbackError} {
		err = st.WithTaskSideEffectLock(scoped, "task", func(context.Context) error { return result })
		if !errors.Is(err, result) {
			t.Fatal(err)
		}
	}
	requireNoConnectionsInUse(t, st.db)
}
func TestTransactionRollbackAndWorkspaceAtomicityIntegration(t *testing.T) {
	st := integrationStore(t)
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "atomic")
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
			if _, err := st.db.ExecContext(store.WithActor(t.Context(), store.SystemActor()), tc.query, tc.args...); err != nil {
				t.Fatal(err)
			}
			if err := st.migrate(store.WithActor(t.Context(), store.SystemActor())); err == nil {
				t.Fatal("mismatched ledger accepted")
			}
			if _, err := st.db.ExecContext(store.WithActor(t.Context(), store.SystemActor()), `DELETE FROM conveyor_singlestore_migrations WHERE version=99999`); err != nil {
				t.Fatal(err)
			}
			if _, err := st.db.ExecContext(store.WithActor(t.Context(), store.SystemActor()), `UPDATE conveyor_singlestore_migrations SET name=?,checksum=? WHERE version=?`, file.name, file.checksum, file.version); err != nil {
				t.Fatal(err)
			}
		})
	}
	if err := st.migrate(store.WithActor(t.Context(), store.SystemActor())); err != nil {
		t.Fatal(err)
	}
}
func TestGoUniqueWriteRulesSerializeIntegration(t *testing.T) {
	st := integrationStore(t)
	// Pin nonzero nanoseconds so macOS clocks cannot conceal Linux write failures.
	now := time.Date(2026, time.September, 6, 5, 0, 0, 123456789, time.UTC)
	for _, kind := range []string{"reference", "deployment"} {
		t.Run(kind, func(t *testing.T) {
			start := make(chan struct{})
			results := make(chan error, 2)
			type attempt struct {
				id       string
				lockAt   time.Time
				commitAt time.Time
			}
			attempts := make([]attempt, 2)
			for i := range 2 {
				go func() {
					<-start
					id := fmt.Sprintf("token-%d", i)
					writeCtx, observed := observeLockReturn(store.WithActor(t.Context(), store.SystemActor()))
					err := st.withTx(store.WithActor(t.Context(), store.SystemActor()), func(tx *sql.Tx) error {
						var w rowWrite
						if kind == "reference" {
							name := "Same"
							if i == 1 {
								name = "same"
							}
							w = rowWrite{table: "reference_documents", operation: "INSERT", values: map[string]any{"workspace_id": "rules", "id": fmt.Sprintf("ref-%d", i), "name": name, "deleted_at": nil, "current_version": 1, "created_at": now, "updated_at": now}}
						} else {
							w = rowWrite{table: "user_tokens", operation: "INSERT", values: map[string]any{"id": id, "user_id": "fixture", "token_hash": []byte(fmt.Sprintf("synthetic-hash-%d", i)), "deployment_credential": true}}
						}
						_, err := writeRow(writeCtx, tx, w)
						if err != nil {
							t.Logf("fixture %s write: %v", kind, err)
						}
						return err
					})
					attempts[i] = attempt{id: id, lockAt: observed.at, commitAt: time.Now()}
					results <- err
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
				if kind == "deployment" {
					t.Logf("deployment tokens %s lock %s commit %s; %s lock %s commit %s", attempts[0].id, attempts[0].lockAt.Format(time.RFC3339Nano), attempts[0].commitAt.Format(time.RFC3339Nano), attempts[1].id, attempts[1].lockAt.Format(time.RFC3339Nano), attempts[1].commitAt.Format(time.RFC3339Nano))
					rows, err := st.db.QueryContext(store.WithActor(t.Context(), store.SystemActor()), `SELECT id, user_id, deployment_credential FROM user_tokens WHERE deployment_credential=TRUE`)
					if err != nil {
						t.Logf("deployment_credential rows: %v", err)
					} else {
						defer rows.Close()
						for rows.Next() {
							var id, userID string
							var credential bool
							if err = rows.Scan(&id, &userID, &credential); err != nil {
								t.Logf("deployment_credential row: %v", err)
								break
							}
							t.Logf("deployment_credential=TRUE row id=%s user_id=%s", id, userID)
						}
						if err = rows.Err(); err != nil {
							t.Logf("deployment_credential rows: %v", err)
						}
					}
				}
				t.Fatalf("%s winners=%d", kind, successes)
			}
			if kind == "reference" {
				var stored time.Time
				if err := st.db.QueryRowContext(store.WithActor(t.Context(), store.SystemActor()), `SELECT created_at FROM reference_documents WHERE workspace_id='rules'`).Scan(&stored); err != nil {
					t.Fatal(err)
				}
				if !stored.Equal(now.Truncate(time.Microsecond)) {
					t.Fatalf("stored timestamp %s, want microsecond truncation of %s", stored, now)
				}
			}
		})
	}
}
func TestConcurrentStartupAndConnectionSettingsIntegration(t *testing.T) {
	st := openOwnedStore(t)
	// Both starts begin below the feature drop (task 261007-9d50e0) with
	// residual feature links; they converge on one applied 0018.
	restorePreFeatureDrop(t, st)
	f := seedFeatureFixture(t, st)
	before := f.snapshot(t, st)
	start := make(chan struct{})
	results := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			results <- st.migrate(store.WithActor(t.Context(), store.SystemActor()))
		}()
	}
	close(start)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	f.assertConverged(t, st, before)
	var zone, mode string
	if err := st.db.QueryRowContext(store.WithActor(t.Context(), store.SystemActor()), `SELECT @@system_time_zone,@@sql_mode`).Scan(&zone, &mode); err != nil {
		t.Fatal(err)
	}
	if zone != "UTC" || !strings.Contains(mode, "STRICT_ALL_TABLES") {
		t.Fatalf("settings: %s %s", zone, mode)
	}
	if st.db.Stats().MaxOpenConnections != 16 {
		t.Fatal("unbounded pool")
	}
	st.Close()
	if err := st.db.PingContext(store.WithActor(t.Context(), store.SystemActor())); err == nil {
		t.Fatal("closed pool remained usable")
	}
}

func TestNamedJobConflictCrossesTransactionBoundaryIntegration(t *testing.T) {
	st := integrationStore(t)
	query := `INSERT INTO jobs(workspace_id,id,task_id,stage,harness,runner,confinement_tier,state) VALUES('conflict','job','task','implement','fixture','fixture','fixture','pending')`
	if _, err := st.db.ExecContext(store.WithActor(t.Context(), store.SystemActor()), query); err != nil {
		t.Fatal(err)
	}
	err := st.withTx(store.WithActor(t.Context(), store.SystemActor()), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(store.WithActor(t.Context(), store.SystemActor()), query)
		return err
	})
	if !errors.Is(err, store.ErrDispatchJobConflict) {
		t.Fatalf("named job conflict escaped: %v", err)
	}
}

// component-persistence: workspace sharding must not make the workspace itself
// unique in a collection. Aggregate writers own artifact ownership uniqueness.
func TestArtifactLinkSchemaAllowsMultipleLinksIntegration(t *testing.T) {
	st := integrationStore(t)
	for _, link := range []struct{ artifact, task string }{
		{"artifact-a", "task-a"},
		{"artifact-b", "task-a"},
		{"artifact-a", "task-b"},
	} {
		if _, err := st.db.ExecContext(store.WithActor(t.Context(), store.SystemActor()), `INSERT INTO artifact_links(workspace_id,artifact_id,task_id) VALUES('links',?,?)`, link.artifact, link.task); err != nil {
			t.Fatalf("insert distinct artifact link: %v", err)
		}
	}
	var count int
	if err := st.db.QueryRowContext(store.WithActor(t.Context(), store.SystemActor()), `SELECT COUNT(*) FROM artifact_links WHERE workspace_id='links'`).Scan(&count); err != nil || count != 3 {
		t.Fatalf("artifact links: count=%d err=%v", count, err)
	}
}

// A commit spanning partitions can release its lock row before its guarded row
// is visible on another partition (task 261012-832d9c). Deleting the committed
// winner's row leaves exactly that view: the lock row committed, the guarded
// row unseen. The next writer must still lose to the claim on the lock row.
func TestUniqueClaimsOutliveUnseenRowsIntegration(t *testing.T) {
	st := integrationStore(t)
	ctx := store.WithActor(t.Context(), store.SystemActor())
	now := time.Date(2026, time.October, 12, 0, 0, 0, 0, time.UTC)
	token := func(id string, deployment bool) rowWrite {
		return rowWrite{table: "user_tokens", operation: "INSERT", values: map[string]any{"id": id, "user_id": "fixture", "token_hash": []byte("hash-" + id), "deployment_credential": deployment}}
	}
	reference := func(id, name string) rowWrite {
		return rowWrite{table: "reference_documents", operation: "INSERT", values: map[string]any{"workspace_id": "claims", "id": id, "name": name, "deleted_at": nil, "current_version": 1, "created_at": now, "updated_at": now}}
	}
	write := func(w rowWrite) error {
		return st.withTx(ctx, func(tx *sql.Tx) error { _, err := writeRow(ctx, tx, w); return err })
	}
	hide := func(query, id string) {
		if _, err := st.db.ExecContext(ctx, query, id); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("deployment", func(t *testing.T) {
		if err := write(token("winner", true)); err != nil {
			t.Fatal(err)
		}
		hide(`DELETE FROM user_tokens WHERE id=?`, "winner")
		if err := write(token("late", true)); err == nil || !strings.Contains(err.Error(), "deployment credential already exists") {
			t.Fatalf("second deployment credential against an unseen winner: %v", err)
		}
	})
	t.Run("deployment release", func(t *testing.T) {
		if err := write(token("retired", true)); err == nil {
			t.Fatal("claim held by winner was not enforced")
		}
		if err := write(rowWrite{table: "user_tokens", operation: "UPDATE", values: map[string]any{"deployment_credential": false}, where: map[string]any{"id": "winner"}}); err != nil {
			t.Fatal(err)
		}
		if err := write(token("reissued", true)); err != nil {
			t.Fatalf("retiring the holder must release its claim: %v", err)
		}
	})
	t.Run("reference", func(t *testing.T) {
		if err := write(reference("ref-winner", "Claimed")); err != nil {
			t.Fatal(err)
		}
		hide(`DELETE FROM reference_documents WHERE id=?`, "ref-winner")
		if err := write(reference("ref-late", "claimed")); !errors.Is(err, store.ErrReferenceDocumentNameConflict) {
			t.Fatalf("second live name against an unseen winner: %v", err)
		}
	})
	t.Run("reference release", func(t *testing.T) {
		if err := write(reference("ref-held", "Released")); err != nil {
			t.Fatal(err)
		}
		if err := write(rowWrite{table: "reference_documents", operation: "UPDATE", values: map[string]any{"name": "Released", "deleted_at": now, "updated_at": now}, where: map[string]any{"workspace_id": "claims", "id": "ref-held"}}); err != nil {
			t.Fatal(err)
		}
		if err := write(reference("ref-reused", "released")); err != nil {
			t.Fatalf("soft delete must release the name claim: %v", err)
		}
	})
	// Open-task branch claims (req-task-branch-assignment AC-4.1, AC-4.2;
	// DEC-68, component-persistence Lock and transaction contract).
	tasks := store.WithWorkspace(ctx, "branch-claims")
	if _, err := st.CreateWorkspace(tasks, "branch-claims", "Branch claims", &config.Config{Workspace: "branch-claims"}); err != nil {
		t.Fatal(err)
	}
	task := func(id, branch string) core.Task {
		return core.Task{ID: id, Workspace: "branch-claims", Repo: "repo", Branch: branch, Title: id, State: core.TaskRunning, NextStage: core.StageImplement}
	}
	refusedBy := func(t *testing.T, err error, winner string) {
		t.Helper()
		var inUse *store.BranchInUseError
		if !errors.Is(err, store.ErrTaskBranchConflict) || !errors.As(err, &inUse) || inUse.OtherTaskID != winner {
			t.Fatalf("want branch conflict naming %s, got %v", winner, err)
		}
	}
	t.Run("task branch", func(t *testing.T) {
		if err := st.CreateTask(tasks, task("branch-winner", "claimed")); err != nil {
			t.Fatal(err)
		}
		hide(`DELETE FROM tasks WHERE workspace_id='branch-claims' AND id=?`, "branch-winner")
		refusedBy(t, st.CreateTask(tasks, task("branch-late", "claimed")), "branch-winner")
	})
	t.Run("task branch attach", func(t *testing.T) {
		if err := st.CreateTask(tasks, task("attach-late", "attach-own")); err != nil {
			t.Fatal(err)
		}
		_, err := st.AttachTaskBranch(tasks, "attach-late", "claimed")
		refusedBy(t, err, "branch-winner")
	})
	t.Run("task branch terminal release", func(t *testing.T) {
		if err := st.CreateTask(tasks, task("cancelled", "reused")); err != nil {
			t.Fatal(err)
		}
		refusedBy(t, st.CreateTask(tasks, task("early", "reused")), "cancelled")
		if _, err := taskops.New(st).Cancel(tasks, core.Intervention{TaskID: "cancelled", Action: core.InterventionCancel, ReasonCode: "obsolete"}); err != nil {
			t.Fatal(err)
		}
		if err := st.CreateTask(tasks, task("successor", "reused")); err != nil {
			t.Fatalf("a terminal task must release its branch claim: %v", err)
		}
	})
	t.Run("task branch attach release", func(t *testing.T) {
		if err := st.CreateTask(tasks, task("mover", "b1")); err != nil {
			t.Fatal(err)
		}
		if _, err := st.AttachTaskBranch(tasks, "mover", "b2"); err != nil {
			t.Fatal(err)
		}
		if err := st.CreateTask(tasks, task("b1-taker", "b1")); err != nil {
			t.Fatalf("attach must release the previous branch claim: %v", err)
		}
		hide(`DELETE FROM tasks WHERE workspace_id='branch-claims' AND id=?`, "mover")
		refusedBy(t, st.CreateTask(tasks, task("b2-taker", "b2")), "mover")
	})
}
