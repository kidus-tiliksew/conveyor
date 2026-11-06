package singlestore

import (
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// TestDriverLogLineFormatIsUnchanged pins SingleStore's whole line, and its
// bound to the one PostgreSQL shares, now that both build it from the shared
// store helpers (component-persistence; DEC-38).
func TestDriverLogLineFormatIsUnchanged(t *testing.T) {
	for _, tc := range []struct {
		input error
		want  string
	}{
		{
			&mysql.MySQLError{Number: 1706, SQLState: [5]byte{'H', 'Y', '0', '0', '0'}, Message: "Feature 'COM_STMT_SEND_LONG_DATA' is not supported by SingleStore"},
			`singlestore: database operation failed: operation=TestDriverLogLineFormatIsUnchanged error=1706 sqlstate=HY000 message="Feature 'COM_STMT_SEND_LONG_DATA' is not supported by SingleStore"` + "\n",
		},
		{
			&mysql.MySQLError{Number: 1706, SQLState: [5]byte{'H', 'Y', '0', '0', '0'}, Message: "Feature 'X' is not supported"},
			`singlestore: database operation failed: operation=TestDriverLogLineFormatIsUnchanged error=1706 sqlstate=HY000 message="server message omitted"` + "\n",
		},
		{
			mysql.ErrInvalidConn,
			`singlestore: database operation failed: operation=TestDriverLogLineFormatIsUnchanged error=0 sqlstate= message="invalid connection"` + "\n",
		},
	} {
		captured := captureLog(t)
		translateBackendConflict(tc.input)
		if got := captured.String(); got != tc.want {
			t.Fatalf("log line = %q, want %q", got, tc.want)
		}
	}
	captured := captureLog(t)
	translateBackendConflict(&mysql.MySQLError{Number: 1062, Message: "Duplicate key " + strings.Repeat("x", 2*store.DriverLogMessageLimit)})
	const admitted = "Duplicate entry [value omitted] for key '"
	want := `singlestore: database operation failed: operation=TestDriverLogLineFormatIsUnchanged error=1062 sqlstate= message="` + admitted + strings.Repeat("x", 1024-len(admitted)) + "\"\n"
	if got := captured.String(); got != want {
		t.Fatalf("bounded log line has %d bytes, want %d", len(got), len(want))
	}
}
