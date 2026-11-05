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

// TestDriverFailureLogLinesIntegration reads the log line for failures reached
// through public store methods: a foreign-key failure whose detail holds row
// values and a pgx failure quoting a bound argument (component-persistence,
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

	child := "log-child-" + core.NewTaskID()
	requireGenericBackendError(t, st.CreateTask(otherCtx, phase61Task(other, child, core.TaskClosed, parent.ID)))
	line := onlyDriverLogLine(t, captured)
	logged := parseDriverLogLine(t, line)
	if logged.operation != "CreateTaskWithDependenciesAndContext" || logged.state != "23503" || !strings.HasPrefix(logged.message, "insert or update on table ") {
		t.Fatalf("foreign key failure logged %+v", logged)
	}
	for _, leaked := range []string{"Key (", "INSERT", child, parent.ID} {
		if strings.Contains(line, leaked) {
			t.Fatalf("foreign key line holds %q", leaked)
		}
	}
	captured.reset()

	design := "log-design-" + core.NewTaskID()
	_, err := st.GetSystemDesignVersion(ctx, design, 1<<40)
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

// TestDriverLogHoldsNoValueFromRealServerMessagesIntegration sends real
// statements to the server. Each value is unregistered, so the redactor cannot
// be what removes it. Each case first runs on the raw pool and checks that the
// server really returns the value, then runs through the boundary and checks
// that the value does not reach the captured line. The constraint cases keep
// their message, whose failing value the server puts only in the detail field.
func TestDriverLogHoldsNoValueFromRealServerMessagesIntegration(t *testing.T) {
	st, ctx, _ := newPhase61IntegrationStore(t)
	t.Cleanup(st.Close)
	suffix := strings.ReplaceAll(strings.ToLower(core.NewTaskID()), "-", "_")
	var (
		enum       = "fixture_log_enum_" + suffix
		parent     = "fixture_log_parent_" + suffix
		child      = "fixture_log_child_" + suffix
		parentKey  = "fixture_log_key_" + suffix
		foreignKey = "fixture_log_fkey_" + suffix
		check      = "fixture_log_check_" + suffix
	)
	const existing = "fixture-unique-row-value"
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		for _, statement := range []string{"DROP TABLE IF EXISTS " + child, "DROP TABLE IF EXISTS " + parent, "DROP TYPE IF EXISTS " + enum} {
			if _, err := st.pool.Exec(cleanup, statement); err != nil {
				t.Errorf("clean up fixture: %v", err)
			}
		}
	})
	for _, statement := range []string{
		"CREATE TYPE " + enum + " AS ENUM ('fixture')",
		"CREATE TABLE " + parent + " (id text, CONSTRAINT " + parentKey + " PRIMARY KEY (id))",
		"CREATE TABLE " + child + " (id text PRIMARY KEY, parent text, note text NOT NULL DEFAULT '', CONSTRAINT " + foreignKey + " FOREIGN KEY (parent) REFERENCES " + parent + " (id), CONSTRAINT " + check + " CHECK (length(note) < 30))",
		"INSERT INTO " + parent + " (id) VALUES ('" + existing + "')",
	} {
		if _, err := st.pool.Exec(ctx, statement); err != nil {
			t.Fatalf("prepare fixture: %v", err)
		}
	}
	for _, tc := range []struct {
		name  string
		sql   string
		args  []any
		state string
		value string
		want  string
	}{
		{"unregistered bound argument", "SELECT $1::int", []any{"fixture-ordinary-bound-value"}, "22P02", "fixture-ordinary-bound-value", serverMessageOmitted},
		{"out of range input", "SELECT $1::int", []any{"98765432109"}, "22003", "98765432109", serverMessageOmitted},
		{"invalid date input", "SELECT $1::date", []any{"fixture-date-value"}, "22007", "fixture-date-value", serverMessageOmitted},
		{"invalid enum input", "SELECT $1::" + enum, []any{"fixture-enum-value"}, "22P02", "fixture-enum-value", serverMessageOmitted},
		{"echoed syntax error", "fixture_echoed_sql_token SELECT 1", nil, "42601", "fixture_echoed_sql_token", serverMessageOmitted},
		{"echoed unterminated string", "SELECT 'fixture_unterminated_sql", nil, "42601", "fixture_unterminated_sql", serverMessageOmitted},
		{"echoed column", "SELECT fixture_echoed_column FROM " + parent, nil, "42703", "fixture_echoed_column", serverMessageOmitted},
		{"echoed relation", "SELECT 1 FROM fixture_echoed_relation", nil, "42P01", "fixture_echoed_relation", serverMessageOmitted},
		{"echoed function without quotes", "SELECT fixture_echoed_function(1)", nil, "42883", "fixture_echoed_function", serverMessageOmitted},
		{"raised value without quotes", "DO $$ BEGIN RAISE EXCEPTION 'raised %', 'fixture-raised-value'; END $$", nil, "P0001", "fixture-raised-value", serverMessageOmitted},
		{"raised imitation of an admitted message", `DO $$ BEGIN RAISE EXCEPTION 'duplicate key value violates unique constraint "%"', 'fixture-imitation-value' USING ERRCODE = '23505'; END $$`, nil, "23505", "fixture-imitation-value", serverMessageOmitted},
		{"unmapped unique failure", "INSERT INTO " + parent + " (id) VALUES ($1)", []any{existing}, "23505", existing, `duplicate key value violates unique constraint "` + parentKey + `"`},
		{"unmapped foreign key failure", "INSERT INTO " + child + " (id, parent) VALUES ('fixture-row', $1)", []any{"fixture-fk-value"}, "23503", "fixture-fk-value", `insert or update on table "` + child + `" violates foreign key constraint "` + foreignKey + `"`},
		{"unmapped check failure", "INSERT INTO " + child + " (id, note) VALUES ($1, repeat('x', 40))", []any{"fixture-check-value"}, "23514", "fixture-check-value", `new row for relation "` + child + `" violates check constraint "` + check + `"`},
		{"not-null failure", "INSERT INTO " + child + " (id, note) VALUES ($1, NULL)", []any{"fixture-null-row-value"}, "23502", "fixture-null-row-value", `null value in column "note" of relation "` + child + `" violates not-null constraint`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, rawErr := st.pool.Exec(ctx, tc.sql, tc.args...)
			var pgErr *pgconn.PgError
			if !errors.As(rawErr, &pgErr) {
				t.Fatalf("precondition failed: raw statement returned %v, want a server error", rawErr)
			}
			if pgErr.Code != tc.state || !strings.Contains(pgErr.Message+" "+pgErr.Detail, tc.value) {
				t.Fatalf("precondition failed: raw statement returned SQLSTATE %s without the value, message %q", pgErr.Code, pgErr.Message)
			}
			if tc.want != serverMessageOmitted && pgErr.Message != tc.want {
				t.Fatalf("precondition failed: server message %q, want %q", pgErr.Message, tc.want)
			}

			captured := captureDriverLog(t)
			_, err := st.boundary.Exec(ctx, tc.sql, tc.args...)
			requireGenericBackendError(t, err, tc.value)
			line := onlyDriverLogLine(t, captured)
			if strings.Contains(line, tc.value) {
				t.Fatal("log line holds the value")
			}
			if strings.Contains(line, tc.sql) {
				t.Fatal("log line holds the SQL text")
			}
			logged := parseDriverLogLine(t, line)
			if logged.operation != "TestDriverLogHoldsNoValueFromRealServerMessagesIntegration" || logged.state != tc.state || logged.message != tc.want {
				t.Fatalf("logged %+v, want sqlstate %s and message %q", logged, tc.state, tc.want)
			}
		})
	}
	t.Run("fixed server message", func(t *testing.T) {
		captured := captureDriverLog(t)
		_, err := st.boundary.Exec(ctx, "SELECT 1/0")
		requireGenericBackendError(t, err)
		if logged := parseDriverLogLine(t, onlyDriverLogLine(t, captured)); logged.state != "22012" || logged.message != "division by zero" {
			t.Fatalf("logged %+v", logged)
		}
	})
}

// TestDriverLogRedactsRealServerMessageIntegration gives a real server message
// the secret through a constraint name, which the sanitizer admits, so the
// redactor is what removes it. The first failure shows it reaching the line
// before it is registered.
func TestDriverLogRedactsRealServerMessageIntegration(t *testing.T) {
	st, ctx, _ := newPhase61IntegrationStore(t)
	t.Cleanup(st.Close)
	suffix := strings.ReplaceAll(strings.ToLower(core.NewTaskID()), "-", "_")
	table, secret := "fixture_log_secret_"+suffix, "fixture_integration_secret_"+suffix
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := st.pool.Exec(cleanup, "DROP TABLE IF EXISTS "+table); err != nil {
			t.Errorf("clean up fixture: %v", err)
		}
	})
	for _, statement := range []string{
		"CREATE TABLE " + table + " (id text, CONSTRAINT " + secret + " PRIMARY KEY (id))",
		"INSERT INTO " + table + " (id) VALUES ('fixture-row')",
	} {
		if _, err := st.pool.Exec(ctx, statement); err != nil {
			t.Fatalf("prepare fixture: %v", err)
		}
	}
	insert := "INSERT INTO " + table + " (id) VALUES ('fixture-row')"
	captured := captureDriverLog(t)
	_, err := st.boundary.Exec(ctx, insert)
	requireGenericBackendError(t, err, secret)
	if !strings.Contains(onlyDriverLogLine(t, captured), secret) {
		t.Fatal("control failed: an unregistered constraint name did not reach the line")
	}
	captured.reset()
	redact.RegisterSecret(secret, time.Time{})
	_, err = st.boundary.Exec(ctx, insert)
	requireGenericBackendError(t, err, secret)
	line := onlyDriverLogLine(t, captured)
	if strings.Contains(line, secret) {
		t.Fatal("log line holds the registered secret")
	}
	if logged := parseDriverLogLine(t, line); logged.state != "23505" || logged.message != uniqueViolationPrefix+`[REDACTED:exact]"` {
		t.Fatalf("server failure logged sqlstate %q and a message of %d bytes", logged.state, len(logged.message))
	}
}
