package singlestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
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
	if err = st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM events_columnstore WHERE workspace_id=? AND id=? AND kind='config.updated'`, "atomic", receipt.EventID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("missing configuration audit: %d %v", count, err)
	}
	if _, err = st.UpdateWorkspaceConfig(ctx, 1, &next); !errors.Is(err, config.ErrVersionConflict) {
		t.Fatal(err)
	}
	if err = st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM events_columnstore WHERE workspace_id=?`, "atomic").Scan(&count); err != nil || count != 1 {
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
	// The claim and the committed-row count compare the same LOWER(name)
	// bytes, without PAD SPACE, as Postgres lower(name) does (DEC-70).
	t.Run("reference padded", func(t *testing.T) {
		if err := write(reference("ref-spaced", "Spaced ")); err != nil {
			t.Fatal(err)
		}
		hide(`DELETE FROM reference_documents WHERE id=?`, "ref-spaced")
		if err := write(reference("ref-spaced-late", "spaced ")); !errors.Is(err, store.ErrReferenceDocumentNameConflict) {
			t.Fatalf("padded live name against an unseen padded winner: %v", err)
		}
		if err := write(reference("ref-unpadded", "spaced")); err != nil {
			t.Fatalf("unpadded name is distinct from an unseen padded winner: %v", err)
		}
	})
	t.Run("reference legacy row", func(t *testing.T) {
		// A row written before claims existed has no lock row; the count is
		// its only guard.
		if _, err := st.db.ExecContext(ctx, `INSERT INTO reference_documents(workspace_id,id,name,current_version,created_at,updated_at) VALUES('claims','ref-legacy','Legacy',1,?,?)`, now, now); err != nil {
			t.Fatal(err)
		}
		if err := write(reference("ref-legacy-case", "LEGACY")); !errors.Is(err, store.ErrReferenceDocumentNameConflict) {
			t.Fatalf("case variant of an unclaimed legacy name: %v", err)
		}
		if err := write(reference("ref-legacy-padded", "legacy ")); err != nil {
			t.Fatalf("padded name is distinct from an unclaimed legacy name: %v", err)
		}
	})
	// Open-task branch claims (req-task-branch-assignment AC-4.1, AC-4.2;
	// DEC-70, component-persistence Lock and transaction contract).
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
	// The claim and the occupant checks compare the same exact repo and branch
	// bytes, without PAD SPACE, as the Postgres open-branch index does (DEC-70).
	t.Run("task branch padded", func(t *testing.T) {
		if err := st.CreateTask(tasks, task("padded-winner", "spaced ")); err != nil {
			t.Fatal(err)
		}
		hide(`DELETE FROM tasks WHERE workspace_id='branch-claims' AND id=?`, "padded-winner")
		refusedBy(t, st.CreateTask(tasks, task("padded-late", "spaced ")), "padded-winner")
		if err := st.CreateTask(tasks, task("unpadded", "spaced")); err != nil {
			t.Fatalf("unpadded branch is distinct from an unseen padded winner: %v", err)
		}
	})
	paddedRepo := func(id, branch string) core.Task {
		padded := task(id, branch)
		padded.Repo = "repo "
		return padded
	}
	t.Run("task branch padded repo", func(t *testing.T) {
		if err := st.CreateTask(tasks, paddedRepo("repo-padded", "repo-pad")); err != nil {
			t.Fatal(err)
		}
		if err := st.CreateTask(tasks, task("repo-unpadded", "repo-pad")); err != nil {
			t.Fatalf("same branch in an unpadded repo is distinct: %v", err)
		}
	})
	t.Run("task branch padded repo attach", func(t *testing.T) {
		if err := st.CreateTask(tasks, paddedRepo("repo-occupant", "repo-attach")); err != nil {
			t.Fatal(err)
		}
		if err := st.CreateTask(tasks, task("repo-attacher", "repo-attacher-own")); err != nil {
			t.Fatal(err)
		}
		if _, err := st.AttachTaskBranch(tasks, "repo-attacher", "repo-attach"); err != nil {
			t.Fatalf("attach beside a padded-repo occupant: %v", err)
		}
	})
	intakeWorkspace := "intake-claims"
	intakeCtx := store.WithWorkspace(ctx, intakeWorkspace)
	if _, err := st.CreateWorkspace(intakeCtx, intakeWorkspace, "Intake claims", &config.Config{Workspace: intakeWorkspace}); err != nil {
		t.Fatal(err)
	}
	intakeTask := func(id, key, branch string) core.Task {
		return core.Task{ID: id, Workspace: intakeWorkspace, Repo: "repo", Branch: branch, IntakeKey: key, Title: id, State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: now}
	}
	t.Run("intake key", func(t *testing.T) {
		if err := st.CreateTask(intakeCtx, intakeTask("intake-winner", "duplicate ", "intake-a")); err != nil {
			t.Fatal(err)
		}
		hide(`DELETE FROM tasks WHERE workspace_id='intake-claims' AND id=?`, "intake-winner")
		err := st.CreateTask(intakeCtx, intakeTask("intake-late", "duplicate ", "intake-b"))
		if err == nil || !strings.Contains(err.Error(), "intake key") {
			t.Fatalf("second intake key against an unseen winner: %v", err)
		}
	})
	t.Run("intake key planning bundle", func(t *testing.T) {
		// A bundle task reaches its writer with its plan state set, as
		// ApprovePlanningBundle sets it (component-planning invariant 32).
		first := intakeTask("planning-winner", "planning-duplicate ", "planning-a")
		first.PlanState = core.PlanNeeded
		if err := st.withTx(intakeCtx, func(tx *sql.Tx) error {
			_, err := insertPlanningBundleTask(intakeCtx, tx, first)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		hide(`DELETE FROM tasks WHERE workspace_id='intake-claims' AND id=?`, first.ID)
		second := intakeTask("planning-late", "planning-duplicate ", "planning-b")
		second.PlanState = core.PlanNeeded
		if err := st.withTx(intakeCtx, func(tx *sql.Tx) error {
			_, err := insertPlanningBundleTask(intakeCtx, tx, second)
			return err
		}); err == nil || !strings.Contains(err.Error(), "intake key") {
			t.Fatalf("planning bundle intake key against an unseen winner: %v", err)
		}
	})
	t.Run("intake key predating claims", func(t *testing.T) {
		first := intakeTask("predating-winner", "predating", "predating-a")
		if err := st.CreateTask(intakeCtx, first); err != nil {
			t.Fatal(err)
		}
		if err := st.withTx(intakeCtx, func(tx *sql.Tx) error {
			return setClaim(intakeCtx, tx, taskIntakeClaimKey(intakeWorkspace, first.IntakeKey), "")
		}); err != nil {
			t.Fatal(err)
		}
		err := st.CreateTask(intakeCtx, intakeTask("predating-late", "predating", "predating-b"))
		if err == nil || !strings.Contains(err.Error(), "intake key") {
			t.Fatalf("intake key row without claim was accepted: %v", err)
		}
	})
	t.Run("intake key retry resolves", func(t *testing.T) {
		first := intakeTask("retry-winner", "retry", "retry-a")
		if err := st.CreateTask(intakeCtx, first); err != nil {
			t.Fatal(err)
		}
		if err := st.CreateTask(intakeCtx, intakeTask("retry-late", "retry", "retry-b")); err == nil {
			t.Fatal("duplicate intake key was accepted")
		}
		got, ok, err := st.GetTaskByIntakeKey(intakeCtx, first.IntakeKey)
		if err != nil {
			t.Fatal(err)
		}
		if !ok || got.ID != first.ID {
			t.Fatalf("retry lookup got ok=%v id=%q, want %q", ok, got.ID, first.ID)
		}
	})
	t.Run("intake key padded spelling is distinct", func(t *testing.T) {
		padded := intakeTask("padded-intake-winner", "padded ", "padded-a")
		if err := st.CreateTask(intakeCtx, padded); err != nil {
			t.Fatal(err)
		}
		unpadded := intakeTask("padded-distinct", "padded", "padded-b")
		if err := st.CreateTask(intakeCtx, unpadded); err != nil {
			t.Fatal(err)
		}
		for _, want := range []core.Task{padded, unpadded} {
			got, ok, err := st.GetTaskByIntakeKey(intakeCtx, want.IntakeKey)
			if err != nil {
				t.Fatal(err)
			}
			if !ok || got.ID != want.ID {
				t.Fatalf("padded intake key %q got ok=%v id=%q, want %q", want.IntakeKey, ok, got.ID, want.ID)
			}
		}
	})

	t.Run("workspace name", func(t *testing.T) {
		if _, err := st.CreateWorkspace(ctx, "wn-a", "Alpha", &config.Config{Workspace: "wn-a"}); err != nil {
			t.Fatal(err)
		}
		hide(`DELETE FROM workspaces WHERE id=?`, "wn-a")
		_, err := st.CreateWorkspace(ctx, "wn-b", " alpha ", &config.Config{Workspace: "wn-b"})
		if !errors.Is(err, store.ErrWorkspaceConflict) {
			t.Fatalf("second workspace name against an unseen winner: %v", err)
		}
	})
	t.Run("workspace name bootstrap", func(t *testing.T) {
		if _, err := st.CreateWorkspace(ctx, "wn-c", "Gamma", &config.Config{Workspace: "wn-c"}); err != nil {
			t.Fatal(err)
		}
		hide(`DELETE FROM workspaces WHERE id=?`, "wn-c")
		seeded, err := st.BootstrapWorkspaceConfig(ctx, &config.Config{Workspace: "gamma"})
		if !errors.Is(err, store.ErrWorkspaceConflict) {
			t.Fatalf("bootstrap workspace name against an unseen winner: seeded=%v err=%v", seeded, err)
		}
	})
	t.Run("workspace name predating claims", func(t *testing.T) {
		if _, err := st.CreateWorkspace(ctx, "wn-d", "Delta", &config.Config{Workspace: "wn-d"}); err != nil {
			t.Fatal(err)
		}
		if err := st.withTx(ctx, func(tx *sql.Tx) error {
			return setClaim(ctx, tx, "workspace-name:delta", "")
		}); err != nil {
			t.Fatal(err)
		}
		_, err := st.CreateWorkspace(ctx, "wn-e", " delta ", &config.Config{Workspace: "wn-e"})
		if !errors.Is(err, store.ErrWorkspaceConflict) {
			t.Fatalf("workspace name row without claim was accepted: %v", err)
		}
	})
}

func TestAttachTaskBranchKeepsCloseLockUntilCommitIntegration(t *testing.T) {
	st := integrationStore(t)
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "attach-order")
	if _, err := st.CreateWorkspace(ctx, "attach-order", "Attach ordering", &config.Config{Workspace: "attach-order"}); err != nil {
		t.Fatal(err)
	}
	taskID := "attach-order-task"
	if err := st.CreateTask(ctx, core.Task{
		ID: taskID, Workspace: "attach-order", Repo: "repo", Branch: "old",
		Title: taskID, State: core.TaskRunning, NextStage: core.StageImplement,
	}); err != nil {
		t.Fatal(err)
	}
	var probeAcquired bool
	var probeErr error
	probeKey := store.BranchCloseLockKey("repo", "new")
	baseCtx := ctx
	ctx, obs := observeTaskTxCommit(baseCtx, func() {
		probeCtx, cancel := context.WithTimeout(baseCtx, 3*time.Second)
		defer cancel()
		probeErr = st.WithTaskSideEffectLock(probeCtx, probeKey, func(context.Context) error {
			probeAcquired = true
			return nil
		})
	})
	result, err := st.AttachTaskBranch(ctx, taskID, "new")
	if err != nil {
		t.Fatal(err)
	}
	if obs.calls.Load() != 1 {
		t.Fatalf("task transaction commit hook called %d times, want 1", obs.calls.Load())
	}
	if probeAcquired {
		t.Fatalf("close-lock probe acquired before AttachTaskBranch returned: %v", probeErr)
	}
	if probeErr == nil {
		t.Fatal("close-lock probe unexpectedly completed without waiting for the held lock")
	}
	if result.Branch != "new" {
		t.Fatalf("attach result branch = %q, want new", result.Branch)
	}
}

// Task 261007-9d50e0 retires the features entity. The version-18 hook detaches
// residual feature-owned attachment links into workspace-unattached links and
// drops both feature columns, and file 0018 drops the table
// (component-persistence, component-artifacts). Recorded history stays
// unchanged (component-lineage).

const featureDropVersion = 18

// restorePreFeatureDrop models a database recorded below 0018 on the shared
// test database: the version-one feature table and columns exist again and
// the ledger lacks version 18. Tests end by migrating, which drops them.
func restorePreFeatureDrop(t *testing.T, st *Store) {
	t.Helper()
	ctx := store.WithActor(t.Context(), store.SystemActor())
	for _, table := range []string{"artifact_links", "tasks"} {
		exists, err := st.columnExists(ctx, table, "feature_id")
		if err != nil {
			t.Fatal(err)
		}
		if !exists {
			if _, err = st.db.ExecContext(ctx, "ALTER TABLE "+table+" ADD COLUMN feature_id LONGTEXT"); err != nil {
				t.Fatal(err)
			}
		}
	}
	raw, err := migrationFiles.ReadFile("migrations/0001_schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	restored := false
	for _, statement := range strings.Split(string(raw), ";") {
		if strings.Contains(statement, "CREATE ROWSTORE TABLE IF NOT EXISTS `features`") {
			if _, err = st.db.ExecContext(ctx, statement); err != nil {
				t.Fatal(err)
			}
			restored = true
		}
	}
	if !restored {
		t.Fatal("version-one schema no longer defines the features table")
	}
	if _, err = st.db.ExecContext(ctx, "DELETE FROM conveyor_singlestore_migrations WHERE version=?", featureDropVersion); err != nil {
		t.Fatal(err)
	}
}

func assertSingleStoreFeatureSchemaRetired(t *testing.T, st *Store) {
	t.Helper()
	ctx := store.WithActor(t.Context(), store.SystemActor())
	// SingleStore refuses one distributed query that joins information_schema
	// with a user table (error 1749), so each count is a separate query.
	var tables, columns, ledger int
	for _, count := range []struct {
		query string
		into  *int
	}{
		{`SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name='features'`, &tables},
		{`SELECT COUNT(*) FROM information_schema.columns WHERE table_schema=DATABASE() AND column_name='feature_id' AND table_name IN ('tasks','artifact_links')`, &columns},
		{`SELECT COUNT(*) FROM conveyor_singlestore_migrations WHERE version=18 AND name='0018_drop_features.sql'`, &ledger},
	} {
		if err := st.db.QueryRowContext(ctx, count.query).Scan(count.into); err != nil {
			t.Fatal(err)
		}
	}
	if tables != 0 || columns != 0 || ledger != 1 {
		t.Fatalf("feature schema tables=%d columns=%d ledger rows=%d", tables, columns, ledger)
	}
}

// featureFixture seeds one workspace with residual feature rows written
// through the retired store methods: shared, duplicate, already-detached,
// mixed, and untouched attachment links, a task assignment, and recorded
// feature history.
type featureFixture struct {
	ctx                                context.Context
	workspace                          string
	task                               core.Task
	shared, existing, mixed, untouched core.Artifact
	suffix                             string
}

func seedFeatureFixture(t *testing.T, st *Store) featureFixture {
	t.Helper()
	f := featureFixture{workspace: "feature-retired-" + core.NewTaskID(), suffix: core.NewTaskID()}
	f.ctx = store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), f.workspace)
	if _, err := st.BootstrapWorkspaceConfig(f.ctx, &config.Config{Workspace: f.workspace, Repos: []config.Repo{{Name: "repo", URL: "https://example.test/repo", Base: "main"}}}); err != nil {
		t.Fatal(err)
	}
	f.task = core.Task{ID: core.NewTaskID(), Workspace: f.workspace, Repo: "repo", Title: "Retired", BaseBranch: "main", State: core.TaskQueued, NextStage: core.StageImplement, CreatedAt: time.Now().UTC()}
	f.task.Branch = "conveyor/task-" + f.task.ID
	if err := st.CreateTask(f.ctx, f.task); err != nil {
		t.Fatal(err)
	}
	session, err := st.CreatePlanningSession(f.ctx, core.PlanningSession{ID: "session-" + core.NewTaskID(), Title: "Retired"})
	if err != nil {
		t.Fatal(err)
	}
	requirement, _, err := st.CreateRequirement(f.ctx, core.Requirement{ID: "req-" + core.NewTaskID(), Title: "Retired"}, core.RequirementVersion{
		Content: "# Retired", Origin: core.RequirementOriginChat, OriginSessionID: session.ID,
		Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Keep attachments."}},
	})
	if err != nil {
		t.Fatal(err)
	}
	featureOne, featureTwo := "feat-one-"+f.suffix, "feat-two-"+f.suffix
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := st.db.ExecContext(f.ctx, query, args...); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	for _, feature := range []string{featureOne, featureTwo} {
		exec(`INSERT INTO features(id,workspace_id,name) VALUES(?,?,?)`, feature, f.workspace, feature)
	}
	exec(`UPDATE tasks SET feature_id=? WHERE workspace_id=? AND id=?`, featureOne, f.workspace, f.task.ID)
	upload := func(name string) core.Artifact {
		t.Helper()
		artifact, err := st.CreateArtifact(f.ctx, core.Artifact{Name: name, ContentType: "text/plain"}, []byte("bytes of "+name+" "+f.suffix))
		if err != nil {
			t.Fatal(err)
		}
		return artifact
	}
	link := func(artifactID, column, owner, role string) {
		t.Helper()
		if column == "" {
			exec(`INSERT INTO artifact_links(workspace_id,artifact_id,role) VALUES(?,?,?)`, f.workspace, artifactID, role)
			return
		}
		exec(`INSERT INTO artifact_links(workspace_id,artifact_id,`+column+`,role) VALUES(?,?,?,?)`, f.workspace, artifactID, owner, role)
	}
	f.shared = upload("shared.txt")
	link(f.shared.ID, "feature_id", featureOne, "task_context")
	link(f.shared.ID, "feature_id", featureTwo, "task_context")
	link(f.shared.ID, "feature_id", featureOne, "generated_output")
	f.existing = upload("existing.txt")
	link(f.existing.ID, "", "", "task_context")
	link(f.existing.ID, "feature_id", featureTwo, "task_context")
	f.mixed = upload("mixed.txt")
	link(f.mixed.ID, "task_id", f.task.ID, "task_context")
	link(f.mixed.ID, "feature_id", featureOne, "task_context")
	f.untouched = upload("untouched.txt")
	link(f.untouched.ID, "task_id", f.task.ID, "task_context")
	link(f.untouched.ID, "requirement_id", requirement.ID, "task_context")
	link(f.untouched.ID, "planning_session_id", session.ID, "task_context")
	if err = st.AppendEvent(f.ctx, core.Event{TaskID: f.task.ID, Kind: "task.feature_assigned", Payload: core.JSONPayload(map[string]string{"feature_id": featureOne})}); err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO links(workspace_id,src_type,src_id,dst_type,dst_id,kind,legacy_created_by_event) VALUES(?,'requirement',?,'task',?,'historical_feature_assignment','feature.migrated')`, f.workspace, "req-"+featureOne, f.task.ID)
	return f
}

func (f featureFixture) snapshot(t *testing.T, st *Store) map[string][]string {
	t.Helper()
	return map[string][]string{
		"artifacts":          queryRows(t, st, f.ctx, `SELECT id,name,content_type,size_bytes,content,created_at FROM artifacts WHERE workspace_id=?`, f.workspace),
		"owned":              queryRows(t, st, f.ctx, `SELECT artifact_id,task_id,requirement_id,planning_session_id,role FROM artifact_links WHERE workspace_id=? AND (task_id IS NOT NULL OR requirement_id IS NOT NULL OR planning_session_id IS NOT NULL)`, f.workspace),
		"events_columnstore": queryRows(t, st, f.ctx, `SELECT id,task_id,kind,payload_json FROM events_columnstore WHERE workspace_id=?`, f.workspace),
		"links":              queryRows(t, st, f.ctx, `SELECT src_type,src_id,dst_type,dst_id,kind,legacy_created_by_event,created_by_event_id FROM links WHERE workspace_id=?`, f.workspace),
	}
}

// assertConverged checks the lossless detach: one unattached link per
// distinct workspace, artifact, and role, no duplicate, and every owned link,
// artifact row, recorded event, and historical link unchanged.
func (f featureFixture) assertConverged(t *testing.T, st *Store, before map[string][]string) {
	t.Helper()
	assertSingleStoreFeatureSchemaRetired(t, st)
	after := f.snapshot(t, st)
	for key := range before {
		if !reflect.DeepEqual(before[key], after[key]) {
			t.Fatalf("%s changed across the drop:\nbefore=%v\nafter=%v", key, before[key], after[key])
		}
	}
	detached := map[string]string{}
	for _, row := range queryRows(t, st, f.ctx, `SELECT artifact_id,role,COUNT(*) FROM artifact_links WHERE workspace_id=? AND task_id IS NULL AND requirement_id IS NULL AND planning_session_id IS NULL GROUP BY artifact_id,role`, f.workspace) {
		detached[row] = row
	}
	want := []string{
		fmt.Sprint([]sql.NullString{{String: f.shared.ID, Valid: true}, {String: "task_context", Valid: true}, {String: "1", Valid: true}}),
		fmt.Sprint([]sql.NullString{{String: f.shared.ID, Valid: true}, {String: "generated_output", Valid: true}, {String: "1", Valid: true}}),
		fmt.Sprint([]sql.NullString{{String: f.existing.ID, Valid: true}, {String: "task_context", Valid: true}, {String: "1", Valid: true}}),
		fmt.Sprint([]sql.NullString{{String: f.mixed.ID, Valid: true}, {String: "task_context", Valid: true}, {String: "1", Valid: true}}),
	}
	if len(detached) != len(want) {
		t.Fatalf("detached links=%v want %v", detached, want)
	}
	for _, row := range want {
		if _, ok := detached[row]; !ok {
			t.Fatalf("detached links=%v missing %s", detached, row)
		}
	}
	selected, err := st.ListArtifactsForLineage(f.ctx, []core.LineageNode{{Type: core.LineageTask, ID: f.task.ID}})
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, artifact := range selected {
		if artifact.TaskID != f.task.ID {
			t.Fatalf("task lineage selected a detached link: %+v", artifact)
		}
		ids[artifact.ID] = true
	}
	if !ids[f.mixed.ID] || !ids[f.untouched.ID] || ids[f.shared.ID] || ids[f.existing.ID] {
		t.Fatalf("task lineage selection=%v", ids)
	}
	for _, artifact := range []core.Artifact{f.shared, f.existing, f.mixed, f.untouched} {
		read, content, err := st.GetArtifact(f.ctx, artifact.ID)
		if err != nil || string(content) != "bytes of "+artifact.Name+" "+f.suffix || (read.TaskID == "" && read.EligibleVerificationEvidence()) {
			t.Fatalf("artifact %s read=%+v content=%q err=%v", artifact.Name, read, content, err)
		}
	}
	if read, err := st.GetTask(f.ctx, f.task.ID); err != nil || read.ID != f.task.ID {
		t.Fatalf("task after the drop=%+v err=%v", read, err)
	}
}
