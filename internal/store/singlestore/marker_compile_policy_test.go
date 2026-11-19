package singlestore

import (
	"context"
	"database/sql/driver"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
)

// TestMarkerTrialDecide pins the compile guard's rule for one trial. A
// historical baseline records a timeout as evidence and never sets the
// guard result. Error 2408 needs no cleanup, because the server already
// ended the statement, and a lost connection stops the server statement
// and reconnects. A fixed shape fails the guard on every timeout and
// execution error, and any other baseline error fails too.
func TestMarkerTrialDecide(t *testing.T) {
	compileTimeout := &mysql.MySQLError{Number: 2408, Message: "Compilation of the query timed out"}
	syntax := &mysql.MySQLError{Number: 1064, Message: "syntax error"}
	for _, tc := range []struct {
		name      string
		fixed     bool
		err       error
		record    string
		reconnect bool
		fatal     bool
	}{
		{name: "baseline success", fixed: false, err: nil},
		{name: "fixed success", fixed: true, err: nil},
		{name: "baseline server 2408", fixed: false, err: compileTimeout, record: "error 2408 after 12.346s"},
		{name: "baseline wrapped server 2408", fixed: false, err: fmt.Errorf("query: %w", compileTimeout), record: "error 2408 after 12.346s"},
		{name: "baseline read timeout", fixed: false, err: mysql.ErrInvalidConn, record: "read timeout after 12.346s", reconnect: true},
		{name: "baseline bad connection", fixed: false, err: driver.ErrBadConn, record: "read timeout after 12.346s", reconnect: true},
		{name: "baseline deadline", fixed: false, err: context.DeadlineExceeded, record: "read timeout after 12.346s", reconnect: true},
		{name: "baseline execution error", fixed: false, err: syntax, fatal: true},
		{name: "fixed server 2408", fixed: true, err: compileTimeout, fatal: true},
		{name: "fixed read timeout", fixed: true, err: mysql.ErrInvalidConn, fatal: true},
		{name: "fixed execution error", fixed: true, err: syntax, fatal: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			action := markerTrialDecide("shape", tc.fixed, 12345678*time.Microsecond, tc.err)
			if got := action.fatal != ""; got != tc.fatal {
				t.Fatalf("fatal = %q, want fatal %t", action.fatal, tc.fatal)
			}
			if action.record != tc.record {
				t.Fatalf("record = %q, want %q", action.record, tc.record)
			}
			if action.reconnect != tc.reconnect {
				t.Fatalf("reconnect = %t, want %t", action.reconnect, tc.reconnect)
			}
			if action.stopShape != (tc.record != "") {
				t.Fatalf("stopShape = %t, want it only with a recorded timeout", action.stopShape)
			}
			if tc.fatal && tc.fixed && tc.err != syntax && !strings.Contains(action.fatal, "timed out") {
				t.Fatalf("fixed timeout message %q does not name the timeout", action.fatal)
			}
		})
	}
}
