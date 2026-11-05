package postgres

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/redact"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func TestBackendConflictSentinelsIntegration(t *testing.T) {
	st, ctx, workspace := newPhase61IntegrationStore(t)
	t.Cleanup(st.Close)
	task := phase61Task(workspace, core.NewTaskID(), core.TaskRunning, "")
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: task.ID + "-implement", TaskID: task.ID, Stage: core.StageImplement, State: core.JobRunning, StartedAt: time.Now().UTC()}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateJob(ctx, job); !errors.Is(err, store.ErrDispatchJobConflict) {
		t.Fatalf("duplicate job: %v", err)
	}
	doc := core.ReferenceDocument{ID: "ref-" + core.NewTaskID(), Name: "Backend contract"}
	version := core.ReferenceDocumentVersion{Filename: "contract.md", ContentType: "text/markdown", Content: "# Contract"}
	if _, _, err := st.CreateReferenceDocument(ctx, doc, version); err != nil {
		t.Fatal(err)
	}
	doc.ID = "ref-" + core.NewTaskID()
	doc.Name = "BACKEND CONTRACT"
	if _, _, err := st.CreateReferenceDocument(ctx, doc, version); !errors.Is(err, store.ErrReferenceDocumentNameConflict) {
		t.Fatalf("duplicate live name: %v", err)
	}
}

// TestUnmappedDriverFailuresLeaveAsStoreErrorsIntegration drives real
// unmapped PostgreSQL failures through public store methods: a foreign-key
// violation inside a transaction and a parameter the driver cannot encode on
// a direct read. Neither may expose a pgx or pgconn type, SQLSTATE, or driver
// message (component-persistence; DEC-38).
func TestUnmappedDriverFailuresLeaveAsStoreErrorsIntegration(t *testing.T) {
	st, ctx, workspace := newPhase61IntegrationStore(t)
	t.Cleanup(st.Close)
	parent := phase61Task(workspace, "unmapped-parent-"+core.NewTaskID(), core.TaskClosed, "")
	if err := st.CreateTask(ctx, parent); err != nil {
		t.Fatal(err)
	}
	other := workspace + "-other"
	otherCtx := store.WithWorkspace(store.WithActor(context.Background(), store.SystemActor()), other)
	if _, err := st.BootstrapWorkspaceConfig(otherCtx, &config.Config{Workspace: other, Repos: []config.Repo{{Name: "conveyor", URL: "https://example.test/conveyor", Base: "main"}}}); err != nil {
		t.Fatal(err)
	}
	_, directErr := st.GetSystemDesignVersion(ctx, "design-"+core.NewTaskID(), 1<<40)
	for name, err := range map[string]error{
		"transaction foreign key": st.CreateTask(otherCtx, phase61Task(other, "unmapped-child-"+core.NewTaskID(), core.TaskClosed, parent.ID)),
		"direct read encoding":    directErr,
	} {
		if !errors.Is(err, store.ErrBackendOperation) {
			t.Fatalf("%s err=%v, want store.ErrBackendOperation", name, err)
		}
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) || carriesDriverError(err) {
			t.Fatalf("%s exposed a driver error: %#v", name, err)
		}
		for _, leaked := range []string{"SQLSTATE", "23503", "violates", "int4", "encode"} {
			if strings.Contains(err.Error(), leaked) {
				t.Fatalf("%s message %q leaks %q", name, err, leaked)
			}
		}
	}
	missing, err := st.GetRequirementVersion(ctx, "req-"+core.NewTaskID(), 1)
	if !errors.Is(err, store.ErrNotFound) || missing.Version != 0 {
		t.Fatalf("missing version err=%v", err)
	}
}

// TestDriverFailureLogLinesIntegration reads the log line for real failures: a
// server message quoting a registered secret, a server error whose detail holds
// row values, and a driver error quoting a bound argument (component-persistence,
// Errors; req-security-boundaries REQ-2).
func TestDriverFailureLogLinesIntegration(t *testing.T) {
	st, ctx, workspace := newPhase61IntegrationStore(t)
	t.Cleanup(st.Close)
	parent := phase61Task(workspace, "log-parent-"+core.NewTaskID(), core.TaskClosed, "")
	if err := st.CreateTask(ctx, parent); err != nil {
		t.Fatal(err)
	}
	other := workspace + "-other"
	otherCtx := store.WithWorkspace(store.WithActor(context.Background(), store.SystemActor()), other)
	if _, err := st.BootstrapWorkspaceConfig(otherCtx, &config.Config{Workspace: other, Repos: []config.Repo{{Name: "conveyor", URL: "https://example.test/conveyor", Base: "main"}}}); err != nil {
		t.Fatal(err)
	}
	captured := captureDriverLog(t)

	secret := "fixture-integration-secret-" + core.NewTaskID()
	redact.RegisterSecret(secret, time.Time{})
	_, err := st.boundary.Exec(ctx, `SELECT $1::int`, secret)
	requireGenericBackendError(t, err, secret)
	line := onlyDriverLogLine(t, captured)
	if strings.Contains(line, secret) {
		t.Fatal("log line holds the registered secret the server quoted")
	}
	logged := parseDriverLogLine(t, line)
	if logged.operation != "TestDriverFailureLogLinesIntegration" || logged.state != "22P02" || !strings.Contains(logged.message, "[REDACTED:exact]") {
		t.Fatalf("server failure logged operation %q, sqlstate %q, message of %d bytes", logged.operation, logged.state, len(logged.message))
	}
	captured.reset()

	child := "log-child-" + core.NewTaskID()
	requireGenericBackendError(t, st.CreateTask(otherCtx, phase61Task(other, child, core.TaskClosed, parent.ID)))
	line = onlyDriverLogLine(t, captured)
	logged = parseDriverLogLine(t, line)
	if logged.operation != "CreateTaskWithDependenciesAndContext" || logged.state != "23503" {
		t.Fatalf("foreign key failure logged operation %q, sqlstate %q", logged.operation, logged.state)
	}
	for _, leaked := range []string{"Key (", "INSERT", child, parent.ID} {
		if strings.Contains(line, leaked) {
			t.Fatalf("foreign key line holds %q", leaked)
		}
	}
	captured.reset()

	design := "log-design-" + core.NewTaskID()
	_, err = st.GetSystemDesignVersion(ctx, design, 1<<40)
	requireGenericBackendError(t, err)
	line = onlyDriverLogLine(t, captured)
	logged = parseDriverLogLine(t, line)
	if logged.operation != "GetSystemDesignVersion" || logged.state != "" || !strings.Contains(logged.message, "value omitted") {
		t.Fatalf("encoding failure logged operation %q, sqlstate %q, message %q", logged.operation, logged.state, logged.message)
	}
	for _, leaked := range []string{"1099511627776", design, "SELECT"} {
		if strings.Contains(line, leaked) {
			t.Fatalf("encoding line holds %q", leaked)
		}
	}
}
