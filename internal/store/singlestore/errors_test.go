package singlestore

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"testing"

	"github.com/go-sql-driver/mysql"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

type lockedLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *lockedLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// captureLog redirects the standard logger for one test. Tests that call it
// do not run in parallel.
func captureLog(t *testing.T) *lockedLog {
	t.Helper()
	captured := &lockedLog{}
	previousWriter, previousFlags := log.Writer(), log.Flags()
	log.SetOutput(captured)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(previousWriter)
		log.SetFlags(previousFlags)
	})
	return captured
}

func TestUnmappedDriverErrorIsLoggedAndHidden(t *testing.T) {
	captured := captureLog(t)
	original := &mysql.MySQLError{Number: 1706, SQLState: [5]byte{'H', 'Y', '0', '0', '0'}, Message: "Feature 'COM_STMT_SEND_LONG_DATA' is not supported by SingleStore"}
	for name, input := range map[string]error{"unwrapped": original, "wrapped": fmt.Errorf("claim order: %w", original)} {
		got := translateBackendConflict(input)
		if !errors.Is(got, store.ErrBackendOperation) || got.Error() != "SingleStore database operation failed" {
			t.Fatalf("%s: translated error = %v", name, got)
		}
		var driverErr *mysql.MySQLError
		if errors.As(got, &driverErr) {
			t.Fatalf("%s: driver error escaped: %#v", name, driverErr)
		}
	}
	lines := strings.Split(strings.TrimSpace(captured.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("log lines = %q, want one per translation", lines)
	}
	for _, line := range lines {
		for _, want := range []string{"singlestore: database operation failed:", "operation=TestUnmappedDriverErrorIsLoggedAndHidden", "error=1706", "sqlstate=HY000", `message="Feature 'COM_STMT_SEND_LONG_DATA' is not supported by SingleStore"`} {
			if !strings.Contains(line, want) {
				t.Fatalf("log line %q lacks %q", line, want)
			}
		}
	}
}

func TestDriverSentinelsAreTranslatedAndLogged(t *testing.T) {
	for _, sentinel := range []error{mysql.ErrInvalidConn, mysql.ErrMalformPkt, mysql.ErrPktSync, mysql.ErrPktSyncMul, mysql.ErrPktTooLarge, mysql.ErrBusyBuffer, driver.ErrBadConn} {
		captured := captureLog(t)
		got := translateBackendConflict(fmt.Errorf("query: %w", sentinel))
		if !errors.Is(got, store.ErrBackendOperation) || errors.Is(got, sentinel) || strings.Contains(got.Error(), sentinel.Error()) {
			t.Fatalf("%v translated to %v", sentinel, got)
		}
		if line := captured.String(); !strings.Contains(line, "error=0 sqlstate= ") || !strings.Contains(line, fmt.Sprintf("message=%q", sentinel.Error())) {
			t.Fatalf("%v log = %q", sentinel, line)
		}
	}
}

func TestDriverLogRedactsAndBoundsMessage(t *testing.T) {
	captured := captureLog(t)
	secret := "password=" + strings.Repeat("s3cr3tV4lue", 4)
	message := "Duplicate entry '" + secret + "' for key 'PRIMARY' " + strings.Repeat("é", 2000)
	translateBackendConflict(&mysql.MySQLError{Number: 1062, Message: message})
	line := captured.String()
	if strings.Contains(line, "s3cr3tV4lue") || !strings.Contains(line, "[REDACTED:entropy]") {
		t.Fatalf("secret not redacted: %q", line)
	}
	start := strings.Index(line, "message=")
	if start < 0 {
		t.Fatalf("no message field: %q", line)
	}
	var logged string
	if _, err := fmt.Sscanf(line[start+len("message="):], "%q", &logged); err != nil {
		t.Fatalf("message field is not quoted: %v", err)
	}
	if len(logged) > driverLogMessageLimit || len(logged) < driverLogMessageLimit-1 || !strings.HasPrefix(logged, "Duplicate entry") || strings.ContainsRune(logged, '�') {
		t.Fatalf("logged message length %d, prefix %.20q", len(logged), logged)
	}
}

func TestTranslatedConditionsAreNotLogged(t *testing.T) {
	captured := captureLog(t)
	application := errors.New("work order is not claimable")
	for _, tc := range []struct {
		input error
		want  error
	}{
		{&mysql.MySQLError{Number: 1062, Message: "Duplicate entry for key 'jobs_pkey'"}, store.ErrDispatchJobConflict},
		{&mysql.MySQLError{Number: 1205, Message: "lock wait timeout"}, store.ErrRetryable},
		{&mysql.MySQLError{Number: 1213, Message: "deadlock"}, store.ErrRetryable},
		{sql.ErrNoRows, sql.ErrNoRows},
		{context.Canceled, context.Canceled},
		{context.DeadlineExceeded, context.DeadlineExceeded},
		{application, application},
		{store.ErrNotFound, store.ErrNotFound},
	} {
		if got := translateBackendConflict(tc.input); !errors.Is(got, tc.want) {
			t.Fatalf("%v translated to %v, want %v", tc.input, got, tc.want)
		}
	}
	if got := notFound(sql.ErrNoRows, "work order %s", "wo-1"); !errors.Is(got, store.ErrNotFound) || !strings.Contains(got.Error(), "work order wo-1") {
		t.Fatalf("notFound(no rows) = %v", got)
	}
	if logged := captured.String(); logged != "" {
		t.Fatalf("translated conditions logged %q", logged)
	}
}

// refusingConnector fails every connection with one driver error, as a
// SingleStore server refusing a command does.
type refusingConnector struct{ err error }

func (c refusingConnector) Connect(context.Context) (driver.Conn, error) { return nil, c.err }
func (c refusingConnector) Driver() driver.Driver                        { return refusingDriver{c.err} }

type refusingDriver struct{ err error }

func (d refusingDriver) Open(string) (driver.Conn, error) { return nil, d.err }

func TestClaimDriverFailureIsLoggedWithOperation(t *testing.T) {
	captured := captureLog(t)
	refusal := &mysql.MySQLError{Number: 1706, SQLState: [5]byte{'H', 'Y', '0', '0', '0'}, Message: "Feature 'COM_STMT_SEND_LONG_DATA' is not supported by SingleStore"}
	db := sql.OpenDB(refusingConnector{refusal})
	t.Cleanup(func() { db.Close() })
	st := &Store{db: db}
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	_, err := st.ClaimWorkOrderCommand(ctx, taskops.TaskLease{}, "wo-claim", core.WorkOrderClaim{SessionID: "session", ClientToken: "token"})
	if !errors.Is(err, store.ErrBackendOperation) || strings.Contains(err.Error(), "COM_STMT_SEND_LONG_DATA") {
		t.Fatalf("claim error = %v, want the generic backend error", err)
	}
	var driverErr *mysql.MySQLError
	if errors.As(err, &driverErr) {
		t.Fatal("driver error escaped the claim")
	}
	line := captured.String()
	for _, want := range []string{"operation=ClaimWorkOrderCommand", "error=1706", "sqlstate=HY000", "COM_STMT_SEND_LONG_DATA"} {
		if !strings.Contains(line, want) {
			t.Fatalf("claim log %q lacks %q", line, want)
		}
	}
}

func TestExportedOperationNames(t *testing.T) {
	for function, want := range map[string]string{
		storePackage + ".(*Store).ClaimWorkOrderCommand":       "ClaimWorkOrderCommand",
		storePackage + ".(*Store).ClaimWorkOrderCommand.func1": "ClaimWorkOrderCommand",
		storePackage + ".Open":                                 "Open",
		storePackage + ".Open.func2":                           "Open",
	} {
		if got, ok := exportedOperation(function); !ok || got != want {
			t.Fatalf("exportedOperation(%q) = %q, %v; want %q", function, got, ok, want)
		}
	}
	for _, function := range []string{
		storePackage + ".(*Store).withTx",
		storePackage + ".(*Store).orderTx.func1",
		storePackage + ".translateBackendConflict",
		storePackage + ".(*meteredConn).QueryContext",
		"database/sql.(*DB).QueryRowContext",
		"github.com/kidus-tiliksew/conveyor/internal/store/postgres.(*Store).ClaimWorkOrderCommand",
	} {
		if got, ok := exportedOperation(function); ok {
			t.Fatalf("exportedOperation(%q) = %q, want no operation", function, got)
		}
	}
}
