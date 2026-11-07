package singlestore

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog/s2log"
	"github.com/kidus-tiliksew/conveyor/internal/monitor"
	"github.com/kidus-tiliksew/conveyor/internal/queue"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// driverFailureText appears only in the injected driver error, so finding it
// in a returned error proves the driver message crossed the boundary.
const driverFailureText = "driver-private failure text"

var injectedDriverError = &mysql.MySQLError{Number: 1706, SQLState: [5]byte{'H', 'Y', '0', '0', '0'}, Message: driverFailureText}

// scriptedConnector answers database/sql like a SingleStore server would, with
// no server: statement decides each statement's failure and iteration each
// result's later failure.
type scriptedConnector struct {
	mu        sync.Mutex
	statement func(query string) error
	iteration error
	// deliveryRow is the body a delivery read returns; empty means no row.
	deliveryRow string
}

func (c *scriptedConnector) script(statement func(string) error, iteration error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.statement, c.iteration = statement, iteration
}

func (c *scriptedConnector) failure(query string) (error, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.statement == nil {
		return nil, c.iteration
	}
	return c.statement(query), c.iteration
}

func (c *scriptedConnector) Connect(context.Context) (driver.Conn, error) {
	return &scriptedConn{connector: c}, nil
}
func (c *scriptedConnector) Driver() driver.Driver { return scriptedDriver{} }

type scriptedDriver struct{}

func (scriptedDriver) Open(string) (driver.Conn, error) { return nil, errors.New("unused") }

type scriptedConn struct{ connector *scriptedConnector }

func (c *scriptedConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("unused") }
func (c *scriptedConn) Close() error                        { return nil }
func (c *scriptedConn) Begin() (driver.Tx, error)           { return scriptedTx{c.connector}, nil }
func (c *scriptedConn) CheckNamedValue(*driver.NamedValue) error {
	return nil
}

func (c *scriptedConn) QueryContext(ctx context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	failure, iteration := c.connector.failure(query)
	if failure != nil {
		return nil, failure
	}
	rows := &scriptedRows{failure: iteration, value: "value"}
	// A lock read returns its row, so a transaction reaches its next statement.
	if iteration == nil && strings.Contains(query, "FOR UPDATE") {
		rows.pending = 1
	}
	c.connector.mu.Lock()
	defer c.connector.mu.Unlock()
	if c.connector.deliveryRow != "" && strings.Contains(query, "FROM verification_publication_deliveries") {
		rows.pending, rows.value = 1, c.connector.deliveryRow
	}
	return rows, nil
}

func (c *scriptedConn) ExecContext(ctx context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	failure, iteration := c.connector.failure(query)
	if failure != nil {
		return nil, failure
	}
	return scriptedResult{iteration}, nil
}

type scriptedTx struct{ connector *scriptedConnector }

func (t scriptedTx) Commit() error {
	failure, _ := t.connector.failure("COMMIT")
	return failure
}
func (scriptedTx) Rollback() error { return nil }

type scriptedResult struct{ failure error }

func (r scriptedResult) LastInsertId() (int64, error) { return 1, r.failure }
func (r scriptedResult) RowsAffected() (int64, error) { return 1, r.failure }

type scriptedRows struct {
	pending int
	value   string
	failure error
}

func (*scriptedRows) Columns() []string { return []string{"value"} }
func (*scriptedRows) Close() error      { return nil }
func (r *scriptedRows) Next(dest []driver.Value) error {
	if r.pending > 0 {
		r.pending--
		dest[0] = r.value
		return nil
	}
	if r.failure != nil {
		return r.failure
	}
	return io.EOF
}

func scriptedStore(t *testing.T) (*Store, *scriptedConnector, context.Context) {
	t.Helper()
	connector := &scriptedConnector{}
	db := sql.OpenDB(connector)
	t.Cleanup(func() { db.Close() })
	return &Store{db: db}, connector, store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
}

func alwaysFail(err error) func(string) error { return func(string) error { return err } }

func requireGenericBackendError(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, store.ErrBackendOperation) {
		t.Fatalf("error = %v, want the generic backend error", err)
	}
	if !strings.Contains(err.Error(), "SingleStore database operation failed") || strings.Contains(err.Error(), driverFailureText) {
		t.Fatalf("error text = %q", err.Error())
	}
	var driverErr *mysql.MySQLError
	if errors.As(err, &driverErr) {
		t.Fatal("a driver error crossed the store boundary")
	}
}

// TestNamedMethodsReturnGenericBackendError covers the two methods the audit
// started from, on each path where a driver error can arise.
func TestNamedMethodsReturnGenericBackendError(t *testing.T) {
	methods := map[string]func(*Store, context.Context) error{
		"ListRepositoryInstallTasks": func(s *Store, ctx context.Context) error {
			_, err := s.ListRepositoryInstallTasks(ctx, "repo")
			return err
		},
		"ClaimBaseConflictCheck": func(s *Store, ctx context.Context) error {
			_, err := s.ClaimBaseConflictCheck(ctx, "order", time.Now())
			return err
		},
	}
	for name, call := range methods {
		t.Run(name, func(t *testing.T) {
			for _, path := range []string{"statement", "iteration"} {
				captured := captureLog(t)
				st, connector, ctx := scriptedStore(t)
				if path == "statement" {
					connector.script(alwaysFail(injectedDriverError), nil)
				} else {
					connector.script(nil, injectedDriverError)
				}
				err := call(st, ctx)
				requireGenericBackendError(t, err)
				if err.Error() != "SingleStore database operation failed" {
					t.Fatalf("%s: error = %q", path, err)
				}
				line := captured.String()
				for _, want := range []string{"operation=" + name, "error=1706", "sqlstate=HY000"} {
					if !strings.Contains(line, want) {
						t.Fatalf("%s: log %q lacks %q", path, line, want)
					}
				}
				if count := strings.Count(line, "database operation failed"); count != 1 {
					t.Fatalf("%s: log wrote %d lines: %q", path, count, line)
				}
			}
		})
	}
}

// TestNamedMethodsKeepMappedErrors holds the behavior translation must not
// change: mapped conflicts, retryable conditions, and cancellation.
func TestNamedMethodsKeepMappedErrors(t *testing.T) {
	duplicate := &mysql.MySQLError{Number: 1062, Message: "Duplicate entry for key 'reference_documents_live_name_idx'"}
	cases := map[string]struct {
		driver error
		want   error
	}{
		"lock wait timeout": {&mysql.MySQLError{Number: 1205, Message: driverFailureText}, store.ErrRetryable},
		"deadlock":          {&mysql.MySQLError{Number: 1213, Message: driverFailureText}, store.ErrRetryable},
		"declared conflict": {duplicate, store.ErrReferenceDocumentNameConflict},
		"cancellation":      {context.Canceled, context.Canceled},
		"deadline":          {context.DeadlineExceeded, context.DeadlineExceeded},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			st, connector, ctx := scriptedStore(t)
			connector.script(alwaysFail(test.driver), nil)
			_, listErr := st.ListRepositoryInstallTasks(ctx, "repo")
			_, claimErr := st.ClaimBaseConflictCheck(ctx, "order", time.Now())
			for _, err := range []error{listErr, claimErr} {
				if !errors.Is(err, test.want) || errors.Is(err, store.ErrBackendOperation) {
					t.Fatalf("error = %v, want %v", err, test.want)
				}
			}
		})
	}
	t.Run("no rows", func(t *testing.T) {
		st, _, ctx := scriptedStore(t)
		tasks, err := st.ListRepositoryInstallTasks(ctx, "repo")
		if err != nil || len(tasks) != 0 {
			t.Fatalf("empty list = %v, %v", tasks, err)
		}
		if _, err := st.GetWorkspace(ctx, "demo"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("missing workspace = %v, want ErrNotFound", err)
		}
	})
}

// TestAuditedDirectStatementsReturnGenericBackendError drives every exported
// method the audit found issuing a statement outside a transaction, on both the
// statement and the row-iteration path.
func TestAuditedDirectStatementsReturnGenericBackendError(t *testing.T) {
	now := time.Now()
	discard := func(_ any, err error) error { return err }
	methods := map[string]func(*Store, context.Context) error{
		"PeerMarks":                     func(s *Store, c context.Context) error { return discard(s.PeerMarks(c)) },
		"ListGitHubAppKeysForRedaction": func(s *Store, c context.Context) error { return discard(s.ListGitHubAppKeysForRedaction(c)) },
		"GetCallerIdentity":             func(s *Store, c context.Context) error { return discard(s.GetCallerIdentity(c, "user", "")) },
		"GetCallerIdentity workspace":   func(s *Store, c context.Context) error { return discard(s.GetCallerIdentity(c, "user", "demo")) },
		"AuthorizeDeployment": func(s *Store, c context.Context) error {
			return discard(s.AuthorizeDeployment(c, "user", core.Capability("claim_work")))
		},
		"AuthorizeInstanceAdministration": func(s *Store, c context.Context) error {
			return discard(s.AuthorizeInstanceAdministration(c, "user"))
		},
		"AuthorizeWorkspace": func(s *Store, c context.Context) error {
			return discard(s.AuthorizeWorkspace(c, "user", "demo", core.Capability("claim_work")))
		},
		"AuthorizeWorkspaceCapabilities": func(s *Store, c context.Context) error {
			return discard(s.AuthorizeWorkspaceCapabilities(c, "user", "demo", []core.Capability{core.Capability("claim_work")}))
		},
		"ListWorkspacesForUser":    func(s *Store, c context.Context) error { return discard(s.ListWorkspacesForUser(c, "user")) },
		"ListWorkspaceMembers":     func(s *Store, c context.Context) error { return discard(s.ListWorkspaceMembers(c, "user", "demo")) },
		"ListWorkspaceInvitations": func(s *Store, c context.Context) error { return discard(s.ListWorkspaceInvitations(c, "demo")) },
		"FindOpenMonitorTask": func(s *Store, c context.Context) error {
			_, _, err := s.FindOpenMonitorTask(c, "repo", monitor.SignalKind("signal"))
			return err
		},
		"MonitorMergeCommitState": func(s *Store, c context.Context) error {
			_, _, err := s.MonitorMergeCommitState(c, "repo", "sha", now, now, time.Hour)
			return err
		},
		"MonitorStatus":       func(s *Store, c context.Context) error { return discard(s.MonitorStatus(c, true, now)) },
		"ListUnresolvedDrift": func(s *Store, c context.Context) error { return discard(s.ListUnresolvedDrift(c)) },
		"MonitorOriginsForTasks": func(s *Store, c context.Context) error {
			return discard(s.MonitorOriginsForTasks(c, []string{"task"}))
		},
		"SignInWithPassword": func(s *Store, c context.Context) error {
			_, _, err := s.SignInWithPassword(c, "user@example.com", "password")
			return err
		},
		"VerifyDashboardSession": func(s *Store, c context.Context) error {
			return discard(s.VerifyDashboardSession(c, "cv_session_candidate"))
		},
		"ReadTaskEventWindow": func(s *Store, c context.Context) error {
			return discard(s.ReadTaskEventWindow(c, store.TaskEventWindowQuery{TaskID: "task", Limit: 10, MaxBytes: 1024}))
		},
		"VerifyCredential": func(s *Store, c context.Context) error {
			return discard(s.VerifyCredential(c, "cv_pat_candidate"))
		},
		"VerifyPersonalAccessToken": func(s *Store, c context.Context) error {
			return discard(s.VerifyPersonalAccessToken(c, "cv_pat_candidate"))
		},
		"ListOwnPersonalAccessTokens": func(s *Store, c context.Context) error {
			return discard(s.ListOwnPersonalAccessTokens(c, "user"))
		},
		"GetWorkspaceGitHubAppStatus": func(s *Store, c context.Context) error {
			return discard(s.GetWorkspaceGitHubAppStatus(c, "demo"))
		},
		"GetWorkspaceGitHubAppForUse": func(s *Store, c context.Context) error {
			return discard(s.GetWorkspaceGitHubAppForUse(c, "demo"))
		},
		"GetWorkspaceGitHubWebhookSecretForUse": func(s *Store, c context.Context) error {
			return discard(s.GetWorkspaceGitHubWebhookSecretForUse(c, "demo"))
		},
		"RecordGitHubWebhookDelivery": func(s *Store, c context.Context) error {
			return discard(s.RecordGitHubWebhookDelivery(c, "demo", "delivery", "event", now))
		},
		"ForgetGitHubWebhookDelivery": func(s *Store, c context.Context) error {
			return s.ForgetGitHubWebhookDelivery(c, "demo", "delivery")
		},
		"GitHubWebhookStatus": func(s *Store, c context.Context) error { return discard(s.GitHubWebhookStatus(c, "demo")) },
		"ListTasksForRecovery": func(s *Store, c context.Context) error {
			return discard(s.ListTasksForRecovery(c, []string{"task"}))
		},
		"ListTasksFilteredWithoutBody": func(s *Store, c context.Context) error {
			return discard(s.ListTasksFilteredWithoutBody(c, store.TaskFilter{States: []core.TaskState{core.TaskQueued}}))
		},
		"ListWorkspaces":   func(s *Store, c context.Context) error { return discard(s.ListWorkspaces(c)) },
		"GetWorkspace":     func(s *Store, c context.Context) error { return discard(s.GetWorkspace(c, "demo")) },
		"WorkspaceConfig":  func(s *Store, c context.Context) error { return discard(s.WorkspaceConfig(c)) },
		"ListLineageLinks": func(s *Store, c context.Context) error { return discard(s.ListLineageLinks(c)) },
	}
	for name, call := range methods {
		t.Run(name, func(t *testing.T) {
			captureLog(t)
			st, connector, ctx := scriptedStore(t)
			connector.script(alwaysFail(injectedDriverError), nil)
			requireGenericBackendError(t, call(st, ctx))
			// A write whose count the method ignores has no later failure.
			if name == "ForgetGitHubWebhookDelivery" {
				return
			}
			connector.script(nil, injectedDriverError)
			requireGenericBackendError(t, call(st, ctx))
		})
	}
}

// TestDocumentResultSetTranslatesIteration covers the row-iteration path every
// workspace-scoped document read shares.
func TestDocumentResultSetTranslatesIteration(t *testing.T) {
	captureLog(t)
	st, connector, ctx := scriptedStore(t)
	connector.script(nil, injectedDriverError)
	rows, err := documentRows(ctx, st.db, "SELECT 1")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Fatal("the scripted rows produced a row")
	}
	requireGenericBackendError(t, rows.Err())
}

func TestBackendExecTranslatesResultCounts(t *testing.T) {
	captureLog(t)
	st, connector, ctx := scriptedStore(t)
	connector.script(nil, injectedDriverError)
	for name, exec := range map[string]func() (sql.Result, error){
		"backendExec":  func() (sql.Result, error) { return backendExec(ctx, st.db, "UPDATE t SET a=1") },
		"documentExec": func() (sql.Result, error) { return documentExec(ctx, st.db, "UPDATE t SET a=1") },
	} {
		result, err := exec()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		_, err = result.RowsAffected()
		requireGenericBackendError(t, err)
		_, err = result.LastInsertId()
		requireGenericBackendError(t, err)
	}
}

// TestLockedCallbacksTranslateDriverErrors covers the session-lock paths that
// hold their own transaction, including a lock taken inside the callback and a
// callback that returns a driver error.
func TestLockedCallbacksTranslateDriverErrors(t *testing.T) {
	raw := func(context.Context) error { return injectedDriverError }
	paths := map[string]func(*Store, context.Context, func(context.Context) error) error{
		"WithTaskSideEffectLock": func(s *Store, c context.Context, fn func(context.Context) error) error {
			return s.WithTaskSideEffectLock(c, "task", fn)
		},
		"WithPlanningSessionRun": func(s *Store, c context.Context, fn func(context.Context) error) error {
			return s.WithPlanningSessionRun(c, "session", fn)
		},
		"WithMonitorSignalClassLock": func(s *Store, c context.Context, fn func(context.Context) error) error {
			return s.WithMonitorSignalClassLock(c, "repo", monitor.SignalKind("signal"), fn)
		},
		"withPlanningSessionLock": func(s *Store, c context.Context, fn func(context.Context) error) error {
			return s.withPlanningSessionLock(c, "session", fn)
		},
	}
	for name, path := range paths {
		t.Run(name, func(t *testing.T) {
			captureLog(t)
			st, _, ctx := scriptedStore(t)
			requireGenericBackendError(t, path(st, ctx, raw))
		})
	}
	t.Run("lock inside a task session", func(t *testing.T) {
		captureLog(t)
		st, connector, ctx := scriptedStore(t)
		err := st.WithTaskSideEffectLock(ctx, "task", func(ctx context.Context) error {
			connector.script(alwaysFail(injectedDriverError), nil)
			lockErr := st.lockOnTaskSession(ctx, "demo", "task", "other-key")
			connector.script(nil, nil)
			requireGenericBackendError(t, lockErr)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})
	t.Run("session lock release", func(t *testing.T) {
		st, _, ctx := scriptedStore(t)
		release, err := st.sessionLock(ctx, "key")
		if err != nil {
			t.Fatal(err)
		}
		if err := release(); err != nil {
			t.Fatal(err)
		}
	})
}

// TestVerificationDeliveryTranslatesDriverErrors covers the one store method
// that begins and commits its own transaction on each of its statements.
func TestVerificationDeliveryTranslatesDriverErrors(t *testing.T) {
	args := queue.VerificationPublicationArgs{WorkspaceID: "demo", Repository: "owner/repo", PullRequestNumber: 7}
	noDelivery := func(*core.VerificationDelivery, func(string, core.VerificationDelivery) error) error {
		t.Fatal("the callback ran although no delivery exists")
		return nil
	}
	for name, failing := range map[string]string{
		"lock":   "conveyor_locks",
		"load":   "verification_publication_deliveries",
		"commit": "COMMIT",
	} {
		t.Run(name, func(t *testing.T) {
			captureLog(t)
			st, connector, ctx := scriptedStore(t)
			connector.script(func(query string) error {
				if strings.Contains(query, failing) {
					return injectedDriverError
				}
				return nil
			}, nil)
			requireGenericBackendError(t, st.RunVerificationDelivery(ctx, args, noDelivery))
		})
	}

	body, err := json.Marshal(core.VerificationDelivery{WorkspaceID: "demo", Repository: "owner/repo", ID: "delivery", Generation: 1, PullRequestNumber: 7, State: "pending"})
	if err != nil {
		t.Fatal(err)
	}
	failOn := func(connector *scriptedConnector, fragment string) {
		connector.script(func(query string) error {
			if strings.Contains(query, fragment) {
				return injectedDriverError
			}
			return nil
		}, nil)
	}
	callbacks := map[string]func(*scriptedConnector, *core.VerificationDelivery, func(string, core.VerificationDelivery) error) error{
		"callback driver error": func(*scriptedConnector, *core.VerificationDelivery, func(string, core.VerificationDelivery) error) error {
			return injectedDriverError
		},
		"check read": func(connector *scriptedConnector, view *core.VerificationDelivery, save func(string, core.VerificationDelivery) error) error {
			failOn(connector, "FROM verification_publication_deliveries")
			return save("check", *view)
		},
		"attempt write": func(connector *scriptedConnector, view *core.VerificationDelivery, save func(string, core.VerificationDelivery) error) error {
			failOn(connector, "INSERT INTO verification_publication_deliveries")
			return save("attempt", *view)
		},
		"attempt commit": func(connector *scriptedConnector, view *core.VerificationDelivery, save func(string, core.VerificationDelivery) error) error {
			failOn(connector, "COMMIT")
			return save("attempt", *view)
		},
		"attempt relock": func(connector *scriptedConnector, view *core.VerificationDelivery, save func(string, core.VerificationDelivery) error) error {
			failOn(connector, "conveyor_locks")
			return save("attempt", *view)
		},
	}
	for name, callback := range callbacks {
		t.Run(name, func(t *testing.T) {
			captureLog(t)
			st, connector, ctx := scriptedStore(t)
			connector.deliveryRow = string(body)
			var saveErr error
			err := st.RunVerificationDelivery(ctx, args, func(view *core.VerificationDelivery, save func(string, core.VerificationDelivery) error) error {
				saveErr = callback(connector, view, save)
				connector.script(nil, nil)
				return saveErr
			})
			if name != "callback driver error" {
				requireGenericBackendError(t, saveErr)
			}
			requireGenericBackendError(t, err)
		})
	}
}

func TestLoadJobTranslatesLogDriverErrors(t *testing.T) {
	captureLog(t)
	st, connector, ctx := scriptedStore(t)
	st.log = s2log.New(st.db)
	connector.script(alwaysFail(injectedDriverError), nil)
	_, err := st.loadJob(ctx, "demo", "stream")
	requireGenericBackendError(t, err)
}
