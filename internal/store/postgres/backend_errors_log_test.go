package postgres

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
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

// uniqueViolation is the driver error for a unique constraint that has no
// store meaning. The sanitizing stage admits its message, so what the constraint
// name holds reaches the redacting and bounding stages.
func uniqueViolation(constraint string) *pgconn.PgError {
	return &pgconn.PgError{
		Severity:       "ERROR",
		Code:           "23505",
		ConstraintName: constraint,
		Message:        uniqueViolationPrefix + constraint + `"`,
	}
}

const uniqueViolationPrefix = `duplicate key value violates unique constraint "`

// TestDriverErrorLogLineIsRedactedBoundedAndNamesOperation is the task's exit
// proof. A driver error holding a registered secret and an oversized message,
// reached through a real exported store method, writes one redacted, bounded
// line naming that method and the SQLSTATE, with no SQL text or argument, and
// the caller receives only the generic error. The message is one the sanitizing
// stage admits, and the control shows the secret reaching the line before it is
// registered, so the redactor and the bound are what this test exercises.
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
	driverErr := uniqueViolation(secret + ": " + strings.Repeat("é", 2*store.DriverLogMessageLimit))
	driverErr.Detail, driverErr.Hint, driverErr.InternalQuery, driverErr.Where = detail, hint, internal, where
	if len(driverErr.Message) <= store.DriverLogMessageLimit {
		t.Fatal("precondition failed: the driver message is not oversized")
	}
	querier := &recordingQuerier{err: fmt.Errorf("query: %w", driverErr)}
	s := &Store{boundary: boundaryDB{q: querier}}
	ctx := logTestContext(t)

	_, _, err := s.GetArtifact(ctx, argument)
	requireGenericBackendError(t, err, secret)
	if !strings.Contains(onlyDriverLogLine(t, captured), secret) {
		t.Fatal("control failed: an unregistered value in an admitted message did not reach the line")
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
	if logged.state != "23505" {
		t.Fatalf("sqlstate = %q, want 23505", logged.state)
	}
	if !strings.HasPrefix(logged.message, uniqueViolationPrefix+"[REDACTED:exact]: é") {
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
	// The message quotes the constraint name, and each such quote takes one
	// escape byte in the line.
	overhead := len("postgres: database operation failed: operation=GetArtifact sqlstate=23505 message=\"\"") + strings.Count(logged.message, `"`)
	if len(line) > overhead+store.DriverLogMessageLimit {
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

// TestDriverLogStagesEachDoTheirOwnWork isolates sanitizing, redacting, and
// bounding, so that no stage's result depends on another stage having hidden the
// value first.
func TestDriverLogStagesEachDoTheirOwnWork(t *testing.T) {
	t.Run("sanitizing removes a value the redactor does not know", func(t *testing.T) {
		captured := captureDriverLog(t)
		const ordinary = "fixture-ordinary-value"
		if clean := store.DriverLogMessage(ordinary); clean != ordinary {
			t.Fatal("precondition failed: the redactor changes the ordinary value")
		}
		requireGenericBackendError(t, translateDriverError(&pgconn.PgError{Code: "22P02", Message: `invalid input syntax for type integer: "` + ordinary + `"`}), ordinary)
		logged := parseDriverLogLine(t, onlyDriverLogLine(t, captured))
		if logged.state != "22P02" || logged.message != serverMessageOmitted {
			t.Fatalf("logged %+v", logged)
		}
	})
	t.Run("redacting removes a registered value the sanitizer admits", func(t *testing.T) {
		captured := captureDriverLog(t)
		secret := "fixture-stage-secret-" + core.NewTaskID()
		requireGenericBackendError(t, translateDriverError(uniqueViolation(secret)), secret)
		if logged := parseDriverLogLine(t, onlyDriverLogLine(t, captured)); logged.message != uniqueViolationPrefix+secret+`"` {
			t.Fatal("control failed: the sanitizer changed an admitted message")
		}
		captured.reset()
		redact.RegisterSecret(secret, time.Time{})
		requireGenericBackendError(t, translateDriverError(uniqueViolation(secret)), secret)
		if logged := parseDriverLogLine(t, onlyDriverLogLine(t, captured)); logged.message != uniqueViolationPrefix+`[REDACTED:exact]"` {
			t.Fatal("the registered value was not replaced by the placeholder")
		}
	})
	t.Run("bounding cuts a clean admitted message", func(t *testing.T) {
		captured := captureDriverLog(t)
		name := strings.Repeat("x", 2*store.DriverLogMessageLimit)
		requireGenericBackendError(t, translateDriverError(uniqueViolation(name)))
		logged := parseDriverLogLine(t, onlyDriverLogLine(t, captured))
		if want := (uniqueViolationPrefix + name)[:store.DriverLogMessageLimit]; logged.message != want {
			t.Fatalf("logged message is %d bytes, want the first %d", len(logged.message), store.DriverLogMessageLimit)
		}
	})
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
			padding := strings.Repeat("x", store.DriverLogMessageLimit-len(uniqueViolationPrefix)-len(tc.prefix)-visible-1) + " "
			constraint := padding + tc.prefix + tc.secret + " trailing text"
			requireGenericBackendError(t, translateDriverError(uniqueViolation(constraint)), tc.secret)
			line := onlyDriverLogLine(t, captured)
			if strings.Contains(line, tc.secret[:visible]) {
				t.Fatal("the cut left the start of the secret in the line")
			}
			logged := parseDriverLogLine(t, line)
			if len(logged.message) > store.DriverLogMessageLimit {
				t.Fatalf("logged message is %d bytes", len(logged.message))
			}
			if !strings.HasPrefix(logged.message, uniqueViolationPrefix+padding+tc.prefix+"[") {
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
	requireGenericBackendError(t, translateDriverError(uniqueViolation("first line\npostgres: database operation failed: operation=Forged sqlstate=00000 message=x")))
	logged := parseDriverLogLine(t, onlyDriverLogLine(t, captured))
	if logged.operation != "TestDriverLogIsOneLineWithOnlyItsOwnFields" || !strings.HasPrefix(logged.message, uniqueViolationPrefix+"first line\n") {
		t.Fatalf("operation = %q, message of %d bytes", logged.operation, len(logged.message))
	}
	captured.reset()
	secret := "fixture-sqlstate-secret-" + core.NewTaskID()
	requireGenericBackendError(t, translateDriverError(&pgconn.PgError{Code: secret, Message: "division by zero"}), secret)
	line := onlyDriverLogLine(t, captured)
	if strings.Contains(line, secret) {
		t.Fatal("log line holds the value from the SQLSTATE field")
	}
	if logged = parseDriverLogLine(t, line); logged.state != "" || logged.message != serverMessageOmitted {
		t.Fatalf("logged sqlstate %q and a message of %d bytes for an implausible code", logged.state, len(logged.message))
	}
}

// TestServerMessagesAreDeniedByDefault holds every server message outside the
// admitted shapes to the fixed diagnostic, including messages nobody listed, a
// value holding its own quotes, text appended to an admitted message, and a
// function raising an admitted message's words without the server's identifier
// fields. The value is unregistered, so only the sanitizer can remove it.
func TestServerMessagesAreDeniedByDefault(t *testing.T) {
	const value = "fixture-denied-value"
	for name, driverErr := range map[string]*pgconn.PgError{
		"quoted value":         {Code: "22P02", Message: `invalid input syntax for type integer: "` + value + `"`},
		"value breaks a quote": {Code: "22P02", Message: `invalid input syntax for type integer: "a" ` + value + ` "b"`},
		"out of range":         {Code: "22003", Message: `value "` + value + `" is out of range for type integer`},
		"enum":                 {Code: "22P02", Message: `invalid input value for enum fixture: "` + value + `"`},
		"syntax":               {Code: "42601", Message: `syntax error at or near "` + value + `"`},
		"unquoted raise":       {Code: "P0001", Message: "dependency cycle: task " + value + " cannot depend on another"},
		"unquoted identifier":  {Code: "42883", Message: "function " + value + "(integer) does not exist"},
		"never seen":           {Code: "XX000", Message: value},
		"no fields":            {Code: "23505", Message: uniqueViolationPrefix + value + `"`},
		"other constraint":     {Code: "23505", ConstraintName: "fixture_key", Message: uniqueViolationPrefix + value + `"`},
		"appended":             {Code: "23505", ConstraintName: "fixture_key", Message: uniqueViolationPrefix + `fixture_key" ` + value},
		"appended fixed":       {Code: "22012", Message: "division by zero: " + value},
		"fixed under other":    {Code: "22012", Message: "integer out of range " + value},
	} {
		t.Run(name, func(t *testing.T) {
			captured := captureDriverLog(t)
			requireGenericBackendError(t, translateDriverError(driverErr), value)
			line := onlyDriverLogLine(t, captured)
			if strings.Contains(line, value) {
				t.Fatal("log line holds the value")
			}
			logged := parseDriverLogLine(t, line)
			if logged.state != driverErr.Code || logged.message != serverMessageOmitted {
				t.Fatalf("logged %+v", logged)
			}
		})
	}
}

// TestAdmittedServerMessagesKeepTheirText keeps the diagnosis the rule can
// afford: each constraint shape rebuilt from its fields, and a fixed message.
func TestAdmittedServerMessagesKeepTheirText(t *testing.T) {
	for _, driverErr := range []*pgconn.PgError{
		{Code: "23505", ConstraintName: "fixture_key", Message: `duplicate key value violates unique constraint "fixture_key"`},
		{Code: "23P01", ConstraintName: "fixture_excl", Message: `conflicting key value violates exclusion constraint "fixture_excl"`},
		{Code: "23503", TableName: "fixture_child", ConstraintName: "fixture_fkey", Message: `insert or update on table "fixture_child" violates foreign key constraint "fixture_fkey"`},
		{Code: "23514", TableName: "fixture_table", ConstraintName: "fixture_check", Message: `new row for relation "fixture_table" violates check constraint "fixture_check"`},
		{Code: "23502", TableName: "fixture_table", ColumnName: "fixture_column", Message: `null value in column "fixture_column" of relation "fixture_table" violates not-null constraint`},
		{Code: "25P02", Message: "current transaction is aborted, commands ignored until end of transaction block"},
	} {
		captured := captureDriverLog(t)
		requireGenericBackendError(t, translateDriverError(driverErr))
		if logged := parseDriverLogLine(t, onlyDriverLogLine(t, captured)); logged.state != driverErr.Code || logged.message != driverErr.Message {
			t.Fatalf("logged %+v, want %q", logged, driverErr.Message)
		}
	}
}

// TestDriverLogOmitsArgumentsAndSQLThatPgxQuotes uses pgx's own encoder, whose
// error repeats the rejected argument, wrapped as pgx wraps it for a query and
// for a batched statement, which also repeats the statement's SQL. A failed scan
// repeats a row value the same way.
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
		"wrapped":     {fmt.Errorf("list tasks: %w", query), "failed to encode query argument args[1]: value omitted"},
		"unencodable": {errors.New("unable to encode 1099511627776 into binary format for int4"), "failed to encode query argument: value omitted"},
		"scan":        {pgx.ScanArgError{ColumnIndex: 2, FieldName: "fixture_column", Err: errors.New("parsing " + argument)}, "failed to scan result column dest[2]: value omitted"},
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

// TestDriverLogAdmitsTransportFailureText proves the second path that reaches
// the redactor and the bound: a connection failure. Its text is kept, without
// the caller's wrapping or anything quoted, and any other driver text is denied.
func TestDriverLogAdmitsTransportFailureText(t *testing.T) {
	t.Run("fixed failures drop the wrapping", func(t *testing.T) {
		captured := captureDriverLog(t)
		for _, tc := range []struct {
			err  error
			want string
		}{
			{fmt.Errorf("commit fixture-wrapper-value: %w", pgx.ErrTxClosed), "tx is closed"},
			{fmt.Errorf("fixture-wrapper-value: %w", pgx.ErrTxCommitRollback), "commit unexpectedly resulted in rollback"},
			{fmt.Errorf("receive fixture-wrapper-value: %w", io.ErrUnexpectedEOF), "unexpected EOF"},
			{errors.New("fixture-wrapper-value: conn closed"), "conn closed"},
		} {
			captured.reset()
			requireGenericBackendError(t, translateDriverError(tc.err))
			if logged := parseDriverLogLine(t, onlyDriverLogLine(t, captured)); logged.state != "" || logged.message != tc.want {
				t.Fatalf("sqlstate %q, message %q; want %q", logged.state, logged.message, tc.want)
			}
		}
	})
	t.Run("a network failure is redacted and bounded", func(t *testing.T) {
		captured := captureDriverLog(t)
		secret := "fixture-host-secret-" + core.NewTaskID()
		failure := func() error {
			return fmt.Errorf("fixture-wrapper-value: %w", &net.DNSError{Err: "no such host", Name: secret + "." + strings.Repeat("x", 2*store.DriverLogMessageLimit)})
		}
		requireGenericBackendError(t, translateDriverError(failure()), secret)
		if !strings.Contains(onlyDriverLogLine(t, captured), secret) {
			t.Fatal("control failed: the unregistered host did not reach the line")
		}
		captured.reset()
		redact.RegisterSecret(secret, time.Time{})
		requireGenericBackendError(t, translateDriverError(failure()), secret)
		line := onlyDriverLogLine(t, captured)
		if strings.Contains(line, secret) || strings.Contains(line, "fixture-wrapper-value") {
			t.Fatal("log line holds the registered secret or the caller's wrapping")
		}
		logged := parseDriverLogLine(t, line)
		if len(logged.message) != store.DriverLogMessageLimit || !strings.HasPrefix(logged.message, "lookup [REDACTED:exact].x") {
			t.Fatalf("logged message of %d bytes is not the redacted, cut host failure", len(logged.message))
		}
	})
	t.Run("quoted text is removed even from a network failure", func(t *testing.T) {
		for name, tc := range map[string]struct{ host, want string }{
			"pair":    {`"fixture-quoted-value"`, "lookup [omitted]: no such host"},
			"inside":  {`a"b" fixture-quoted-value "c"d`, "lookup a[omitted]d: no such host"},
			"single":  {`a'fixture-quoted-value`, "lookup a[omitted]"},
			"tick":    {"`fixture-quoted-value`", "lookup [omitted]: no such host"},
			"no text": {"fixture-host", "lookup fixture-host: no such host"},
		} {
			t.Run(name, func(t *testing.T) {
				captured := captureDriverLog(t)
				requireGenericBackendError(t, translateDriverError(&net.DNSError{Err: "no such host", Name: tc.host}))
				if logged := parseDriverLogLine(t, onlyDriverLogLine(t, captured)); logged.message != tc.want {
					t.Fatalf("message = %q, want %q", logged.message, tc.want)
				}
			})
		}
	})
	t.Run("any other driver text is denied", func(t *testing.T) {
		captured := captureDriverLog(t)
		const value = "fixture-unknown-driver-value"
		requireGenericBackendError(t, translateDriverError(errors.New("fixture failure quoting "+value)), value)
		line := onlyDriverLogLine(t, captured)
		if strings.Contains(line, value) {
			t.Fatal("log line holds the value")
		}
		if logged := parseDriverLogLine(t, line); logged.message != driverMessageOmitted {
			t.Fatalf("message of %d bytes is not the fixed diagnostic", len(logged.message))
		}
	})
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
			if logged.operation != "TestEveryBoundaryPathLogsOneLine" || logged.state != "23503" || logged.message != serverMessageOmitted {
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
