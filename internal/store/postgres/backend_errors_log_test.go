package postgres

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/redact"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
)

// These tests prove the one server log line for a replaced driver error
// (component-persistence, Errors; req-security-boundaries REQ-2). Their failure
// messages never print a captured line, because a failing line can hold the
// fixture secret.

// driverLogCapture holds the standard logger's output for one test. Tests that
// use it do not run in parallel.
type driverLogCapture struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *driverLogCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

// lines returns this package's driver log lines. A store left over from
// another test logs from a background goroutine as operation=unknown; a
// translation on a test's own stack always names the test or a Store method.
func (c *driverLogCapture) lines() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var lines []string
	for _, line := range strings.Split(c.buf.String(), "\n") {
		if strings.HasPrefix(line, "postgres: database operation failed:") && !strings.Contains(line, "operation=unknown ") {
			lines = append(lines, line)
		}
	}
	return lines
}

func (c *driverLogCapture) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.buf.Reset()
}

func captureDriverLog(t *testing.T) *driverLogCapture {
	t.Helper()
	captured := &driverLogCapture{}
	previousWriter, previousFlags := log.Writer(), log.Flags()
	log.SetOutput(captured)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(previousWriter)
		log.SetFlags(previousFlags)
	})
	return captured
}

// driverLogForm is the whole line: these three fields and nothing else.
var driverLogForm = regexp.MustCompile(`^postgres: database operation failed: operation=(\S+) sqlstate=([0-9A-Z]*) message=(".*")$`)

type driverLogLine struct{ operation, state, message string }

func parseDriverLogLine(t *testing.T, line string) driverLogLine {
	t.Helper()
	match := driverLogForm.FindStringSubmatch(line)
	if match == nil {
		t.Fatalf("log line of %d bytes does not have the expected form", len(line))
	}
	message, err := strconv.Unquote(match[3])
	if err != nil {
		t.Fatalf("message field is not quoted: %v", err)
	}
	return driverLogLine{operation: match[1], state: match[2], message: message}
}

func onlyDriverLogLine(t *testing.T, captured *driverLogCapture) string {
	t.Helper()
	lines := captured.lines()
	if len(lines) != 1 {
		t.Fatalf("captured %d driver log lines, want exactly one", len(lines))
	}
	return lines[0]
}

// recordingQuerier fails every query with one driver error and keeps the SQL
// text and arguments the store sent.
type recordingQuerier struct {
	err  error
	sql  []string
	args []any
}

func (q *recordingQuerier) record(sql string, args []any) {
	q.sql = append(q.sql, sql)
	q.args = append(q.args, args...)
}

func (q *recordingQuerier) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	q.record(sql, args)
	return pgconn.CommandTag{}, q.err
}

func (q *recordingQuerier) Query(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
	q.record(sql, args)
	return fakeRows{err: q.err}, nil
}

func (q *recordingQuerier) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	q.record(sql, args)
	return fakeRow{q.err}
}

func logTestContext(t *testing.T) context.Context {
	t.Helper()
	return store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "driver-log-workspace")
}

func requireGenericBackendError(t *testing.T, err error, secrets ...string) {
	t.Helper()
	if !errors.Is(err, store.ErrBackendOperation) || err.Error() != "PostgreSQL database operation failed" {
		t.Fatal("caller did not receive the generic backend error")
	}
	requireNoDriverType(t, err)
	for _, secret := range secrets {
		if strings.Contains(err.Error(), secret) {
			t.Fatal("caller error holds the fixture secret")
		}
	}
}

// TestDriverErrorLogLineIsRedactedBoundedAndNamesOperation is the task's exit
// proof. A driver error holding a registered secret and an oversized message,
// reached through a real exported store method, writes one redacted, bounded
// line naming that method and the SQLSTATE, with no SQL text or argument, and
// the caller receives only the generic error.
func TestDriverErrorLogLineIsRedactedBoundedAndNamesOperation(t *testing.T) {
	captured := captureDriverLog(t)
	secret := "fixture-registered-secret-" + core.NewTaskID()
	const (
		argument = "fixture-artifact-argument"
		detail   = "fixture-detail-row-value"
		hint     = "fixture-hint"
		internal = "fixture-internal-query"
		where    = "fixture-where-context"
	)
	driverErr := &pgconn.PgError{
		Severity:      "ERROR",
		Code:          "XX000",
		Message:       "cannot store value " + secret + ": " + strings.Repeat("é", 2*store.DriverLogMessageLimit),
		Detail:        detail,
		Hint:          hint,
		InternalQuery: internal,
		Where:         where,
	}
	querier := &recordingQuerier{err: fmt.Errorf("query: %w", driverErr)}
	s := &Store{boundary: boundaryDB{q: querier}}
	ctx := logTestContext(t)

	// Control: the redactor, once the secret is registered, is what removes
	// it. The same call shows it in the line before registration.
	_, _, err := s.GetArtifact(ctx, argument)
	requireGenericBackendError(t, err, secret)
	if !strings.Contains(onlyDriverLogLine(t, captured), secret) {
		t.Fatal("control failed: an unregistered value did not reach the line")
	}
	captured.reset()
	redact.RegisterSecret(secret, time.Time{})

	_, _, err = s.GetArtifact(ctx, argument)
	requireGenericBackendError(t, err, secret)
	line := onlyDriverLogLine(t, captured)

	if strings.Contains(line, secret) {
		t.Fatal("log line holds the registered secret")
	}
	logged := parseDriverLogLine(t, line)
	if logged.operation != "GetArtifact" {
		t.Fatalf("operation = %q, want GetArtifact", logged.operation)
	}
	if logged.state != "XX000" {
		t.Fatalf("sqlstate = %q, want XX000", logged.state)
	}
	if !strings.HasPrefix(logged.message, "cannot store value [REDACTED:exact]: é") {
		t.Fatal("logged message does not start with the redacted driver message")
	}
	if store.DriverLogMessageLimit != 1024 {
		t.Fatalf("message bound = %d, want SingleStore's 1024", store.DriverLogMessageLimit)
	}
	if len(logged.message) > store.DriverLogMessageLimit || len(logged.message) < store.DriverLogMessageLimit-1 {
		t.Fatalf("logged message is %d bytes, want the %d-byte bound", len(logged.message), store.DriverLogMessageLimit)
	}
	if !utf8.ValidString(logged.message) || strings.ContainsRune(logged.message, utf8.RuneError) {
		t.Fatal("the cut split a rune")
	}
	if overhead := len("postgres: database operation failed: operation=GetArtifact sqlstate=XX000 message=\"\""); len(line) > overhead+store.DriverLogMessageLimit {
		t.Fatalf("log line is %d bytes, want at most %d", len(line), overhead+store.DriverLogMessageLimit)
	}

	if len(querier.sql) != 2 || len(querier.args) == 0 {
		t.Fatalf("the store sent %d statements and %d arguments, want 2 and some", len(querier.sql), len(querier.args))
	}
	for _, statement := range querier.sql {
		if strings.Contains(line, statement) {
			t.Fatal("log line holds the SQL text")
		}
	}
	for _, fragment := range []string{"SELECT", "FROM artifacts", "workspace_id"} {
		if strings.Contains(line, fragment) {
			t.Fatalf("log line holds SQL fragment %q", fragment)
		}
	}
	for _, sent := range querier.args {
		if text := fmt.Sprint(sent); strings.Contains(line, text) {
			t.Fatalf("log line holds bound argument %q", text)
		}
	}
	for _, field := range []string{detail, hint, internal, where} {
		if strings.Contains(line, field) {
			t.Fatalf("log line holds server error field %q", field)
		}
	}
}

// TestDriverLogRedactsBeforeItCuts places each secret across the bound. A cut
// before redaction would leave the secret's start in the line, past matchers
// that no longer see all of it.
func TestDriverLogRedactsBeforeItCuts(t *testing.T) {
	registered := "fixture-straddling-secret-" + core.NewTaskID()
	redact.RegisterSecret(registered, time.Time{})
	for name, tc := range map[string]struct{ prefix, secret string }{
		"registered": {"", registered},
		"token":      {"", "ghp_" + strings.Repeat("F", 36)},
		"assigned":   {"password=", "F1xtureOnlyAssignedValue00000"},
	} {
		t.Run(name, func(t *testing.T) {
			captured := captureDriverLog(t)
			const visible = 6
			// The padding ends in a space because the token pattern needs a
			// word boundary before the token.
			padding := strings.Repeat("x", store.DriverLogMessageLimit-len(tc.prefix)-visible-1) + " "
			message := padding + tc.prefix + tc.secret + " trailing text"
			requireGenericBackendError(t, translateDriverError(&pgconn.PgError{Code: "XX000", Message: message}), tc.secret)
			line := onlyDriverLogLine(t, captured)
			if strings.Contains(line, tc.secret[:visible]) {
				t.Fatal("the cut left the start of the secret in the line")
			}
			logged := parseDriverLogLine(t, line)
			if len(logged.message) > store.DriverLogMessageLimit {
				t.Fatalf("logged message is %d bytes", len(logged.message))
			}
			if !strings.HasPrefix(logged.message, padding+tc.prefix+"[") {
				t.Fatal("the secret's position does not hold the redaction placeholder")
			}
		})
	}
}

// TestDriverLogIsOneLineWithOnlyItsOwnFields keeps driver text inside the
// quoted message field. A newline in a message cannot start a forged line, and
// an implausible SQLSTATE stays out of the field that is not redacted.
func TestDriverLogIsOneLineWithOnlyItsOwnFields(t *testing.T) {
	captured := captureDriverLog(t)
	secret := "fixture-sqlstate-secret-" + core.NewTaskID()
	redact.RegisterSecret(secret, time.Time{})
	requireGenericBackendError(t, translateDriverError(&pgconn.PgError{
		Code:    secret,
		Message: "first line\npostgres: database operation failed: operation=Forged sqlstate=00000 message=\"x\"",
	}), secret)
	line := onlyDriverLogLine(t, captured)
	if strings.Contains(line, secret) {
		t.Fatal("log line holds the value from the SQLSTATE field")
	}
	logged := parseDriverLogLine(t, line)
	if logged.state != "" {
		t.Fatalf("sqlstate = %q, want empty for an implausible code", logged.state)
	}
	if logged.operation != "TestDriverLogIsOneLineWithOnlyItsOwnFields" || !strings.HasPrefix(logged.message, "first line\n") {
		t.Fatalf("operation = %q, message of %d bytes", logged.operation, len(logged.message))
	}
}

// TestDriverLogOmitsArgumentsAndSQLThatPgxQuotes uses pgx's own encoder, whose
// error repeats the rejected argument, wrapped as pgx wraps it for a query and
// for a batched statement, which also repeats the statement's SQL.
func TestDriverLogOmitsArgumentsAndSQLThatPgxQuotes(t *testing.T) {
	const (
		argument  = "fixture-bound-argument"
		statement = "SELECT fixture_column FROM fixture_table WHERE id = $1"
	)
	_, encodeErr := pgtype.NewMap().Encode(pgtype.Int4OID, pgtype.BinaryFormatCode, argument, nil)
	if encodeErr == nil || !strings.Contains(encodeErr.Error(), argument) {
		t.Fatal("precondition failed: pgx's encoder no longer quotes the argument")
	}
	query := fmt.Errorf("failed to encode args[1]: %w", encodeErr)
	for name, tc := range map[string]struct {
		err  error
		want string
	}{
		"encoder":     {encodeErr, "failed to encode query argument: value omitted"},
		"query":       {query, "failed to encode query argument args[1]: value omitted"},
		"batch":       {fmt.Errorf("error building query %s: %w", statement, query), "failed to encode query argument args[1]: value omitted"},
		"wrapped":     {fmt.Errorf("list tasks: %w", query), "list tasks: failed to encode query argument args[1]: value omitted"},
		"unencodable": {errors.New("unable to encode 1099511627776 into binary format for int4"), "failed to encode query argument: value omitted"},
	} {
		t.Run(name, func(t *testing.T) {
			captured := captureDriverLog(t)
			requireGenericBackendError(t, translateDriverError(tc.err))
			line := onlyDriverLogLine(t, captured)
			logged := parseDriverLogLine(t, line)
			if logged.message != tc.want {
				t.Fatalf("message = %q, want %q", logged.message, tc.want)
			}
			if logged.state != "" {
				t.Fatalf("sqlstate = %q for a client-side failure", logged.state)
			}
		})
	}
}

// TestDriverLogKeepsOtherClientFailureText keeps a failure the driver raises
// itself diagnosable, with no SQLSTATE, and still redacted and bounded.
func TestDriverLogKeepsOtherClientFailureText(t *testing.T) {
	captured := captureDriverLog(t)
	translated := translateBackendConflict(fmt.Errorf("commit: %w", pgx.ErrTxClosed))
	requireGenericBackendError(t, translated)
	logged := parseDriverLogLine(t, onlyDriverLogLine(t, captured))
	if logged.state != "" || logged.message != "commit: tx is closed" {
		t.Fatalf("sqlstate = %q, message = %q", logged.state, logged.message)
	}
	captured.reset()
	token := "ghp_" + strings.Repeat("F", 36)
	requireGenericBackendError(t, translateDriverError(errors.New("connect "+token+" "+strings.Repeat("x", 2*store.DriverLogMessageLimit))), token)
	line := onlyDriverLogLine(t, captured)
	if strings.Contains(line, token) {
		t.Fatal("log line holds the token-shaped value")
	}
	if logged = parseDriverLogLine(t, line); len(logged.message) != store.DriverLogMessageLimit {
		t.Fatalf("logged message is %d bytes", len(logged.message))
	}
}

// TestMappedDriverConditionsReturnWithoutLogLine holds every condition with a
// store meaning silent on both translators. The final unmapped error proves
// the capture would have seen a line.
func TestMappedDriverConditionsReturnWithoutLogLine(t *testing.T) {
	captured := captureDriverLog(t)
	const text = "fixture driver text"
	type condition struct {
		name  string
		input error
		want  error
	}
	conditions := []condition{
		{"deadlock", &pgconn.PgError{Code: "40P01", Message: text}, store.ErrRetryable},
		{"lock wait", &pgconn.PgError{Code: "55P03", Message: text}, store.ErrRetryable},
		{"no rows", pgx.ErrNoRows, store.ErrNotFound},
		{"wrapped no rows", fmt.Errorf("scan: %w", pgx.ErrNoRows), store.ErrNotFound},
		{"translated no rows", errNoRows, store.ErrNotFound},
		{"canceled", fmt.Errorf("query: %w", context.Canceled), context.Canceled},
		{"deadline", fmt.Errorf("query: %w", context.DeadlineExceeded), context.DeadlineExceeded},
		{"canceled statement", errors.Join(&pgconn.PgError{Code: "57014", Message: text}, context.Canceled), context.Canceled},
	}
	for constraint, sentinel := range uniqueConstraintErrors {
		conditions = append(conditions, condition{"conflict " + constraint, &pgconn.PgError{Code: "23505", ConstraintName: constraint, Message: text}, sentinel})
	}
	for _, tc := range conditions {
		if got := translateDriverError(tc.input); !errors.Is(got, tc.want) {
			t.Fatalf("%s: driver translation = %v, want %v", tc.name, got, tc.want)
		}
		if lines := captured.lines(); len(lines) != 0 {
			t.Fatalf("%s: driver translation wrote %d log lines", tc.name, len(lines))
		}
		if got := translateBackendConflict(tc.input); !errors.Is(got, tc.want) {
			t.Fatalf("%s: mixed translation = %v, want %v", tc.name, got, tc.want)
		}
		if lines := captured.lines(); len(lines) != 0 {
			t.Fatalf("%s: mixed translation wrote %d log lines", tc.name, len(lines))
		}
	}
	application := errors.New("fixture application error")
	if got := translateBackendConflict(application); got != application {
		t.Fatalf("application error replaced: %v", got)
	}
	if mapped := notFound(translateDriverError(pgx.ErrNoRows), "task %s", "task-1"); !errors.Is(mapped, store.ErrNotFound) {
		t.Fatalf("not-found mapping = %v", mapped)
	}
	if translateDriverError(nil) != nil || translateBackendConflict(nil) != nil {
		t.Fatal("nil error changed")
	}
	if lines := captured.lines(); len(lines) != 0 {
		t.Fatalf("mapped conditions wrote %d log lines", len(lines))
	}
	requireGenericBackendError(t, translateDriverError(&pgconn.PgError{Code: "23505", ConstraintName: "tasks_pkey", Message: text}))
	onlyDriverLogLine(t, captured)
}

// TestEveryBoundaryPathLogsOneLine covers each outward path the boundary
// translates: one driver failure, one line, and no second line when an already
// translated error passes through the mixed translator.
func TestEveryBoundaryPathLogsOneLine(t *testing.T) {
	driverErr := &pgconn.PgError{Code: "23503", Message: "fixture foreign key violation"}
	ctx := store.WithActor(t.Context(), store.SystemActor())
	boundary := boundaryDB{q: fakeQuerier{err: driverErr}}
	for name, call := range map[string]func() error{
		"exec":      func() error { _, err := boundary.Exec(ctx, "UPDATE"); return err },
		"row.Scan":  func() error { return boundary.QueryRow(ctx, "SELECT").Scan() },
		"rows.Scan": func() error { rows, _ := boundary.Query(ctx, "SELECT"); return rows.Scan() },
		"rows.Err":  func() error { rows, _ := boundary.Query(ctx, "SELECT"); return rows.Err() },
		"commit":    func() error { return newBoundaryTx(fakeTx{err: driverErr}).Commit(ctx) },
	} {
		t.Run(name, func(t *testing.T) {
			captured := captureDriverLog(t)
			err := call()
			requireGenericBackendError(t, err)
			if again := translateBackendConflict(fmt.Errorf("transaction: %w", err)); !errors.Is(again, store.ErrBackendOperation) {
				t.Fatal("a translated error changed in the mixed translator")
			}
			logged := parseDriverLogLine(t, onlyDriverLogLine(t, captured))
			if logged.operation != "TestEveryBoundaryPathLogsOneLine" || logged.state != "23503" || logged.message != "fixture foreign key violation" {
				t.Fatalf("logged %+v", logged)
			}
		})
	}
}

// TestDriverLogNamesOperationThroughGeneratedQueries walks past the boundary
// and the generated query package to the exported store method that called
// them.
func TestDriverLogNamesOperationThroughGeneratedQueries(t *testing.T) {
	captured := captureDriverLog(t)
	boundary := boundaryDB{q: &recordingQuerier{err: &pgconn.PgError{Code: "42P01", Message: "fixture missing relation"}}}
	s := &Store{boundary: boundary, queries: db.New(boundary)}
	_, err := s.ListEvents(logTestContext(t), "fixture-task")
	requireGenericBackendError(t, err)
	logged := parseDriverLogLine(t, onlyDriverLogLine(t, captured))
	if logged.operation != "ListEvents" || logged.state != "42P01" {
		t.Fatalf("logged %+v", logged)
	}
}

func TestPostgresOperationNames(t *testing.T) {
	if want := reflect.TypeOf(boundaryDB{}).PkgPath(); storePackage != want {
		t.Fatalf("storePackage = %q, want %q", storePackage, want)
	}
	for function, want := range map[string]string{
		storePackage + ".(*Store).ClaimWorkOrderCommand":       "ClaimWorkOrderCommand",
		storePackage + ".(*Store).ClaimWorkOrderCommand.func1": "ClaimWorkOrderCommand",
		storePackage + ".Migrate":                              "Migrate",
	} {
		if got, ok := store.ExportedBackendOperation(storePackage, function); !ok || got != want {
			t.Fatalf("operation of %q = %q, %v; want %q", function, got, ok, want)
		}
	}
	for _, function := range []string{
		storePackage + ".(*Store).inTx",
		storePackage + ".translateDriverError",
		storePackage + ".boundaryDB.Exec",
		storePackage + ".boundaryRow.Scan",
		storePackage + ".(*txLog).Append",
		storePackage + "/db.(*Queries).GetJob",
		"github.com/jackc/pgx/v5.(*Conn).Exec",
	} {
		if got, ok := store.ExportedBackendOperation(storePackage, function); ok {
			t.Fatalf("operation of %q = %q, want none", function, got)
		}
	}
}
