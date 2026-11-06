package singlestore

import (
	"database/sql/driver"
	"errors"
	"fmt"
	"log"
	"reflect"
	"regexp"
	"slices"

	"github.com/go-sql-driver/mysql"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// errBackendOperation is the generic SingleStore failure. It wraps
// store.ErrBackendOperation and carries nothing from the driver.
var errBackendOperation = fmt.Errorf("SingleStore %w", store.ErrBackendOperation)

// driverSentinels are the go-sql-driver and database/sql connection failures
// that carry no *mysql.MySQLError. They are driver conditions like any other
// and must not reach a caller as driver text (component-persistence; DEC-38).
var driverSentinels = []error{
	mysql.ErrInvalidConn,
	mysql.ErrMalformPkt,
	mysql.ErrPktSync,
	mysql.ErrPktSyncMul,
	mysql.ErrPktTooLarge,
	mysql.ErrBusyBuffer,
	driver.ErrBadConn,
}

// driverLogMessageLimit bounds the driver message written to the server log.
const driverLogMessageLimit = store.DriverLogMessageLimit

// duplicateEntryError is the server's error number for a unique-key violation.
const duplicateEntryError uint16 = 1062

// omittedDriverMessage replaces every server message the log does not admit.
const omittedDriverMessage = "server message omitted"

// fixedDriverMessages lists, per error number, the only messages the log admits
// verbatim, each compared whole. Number 0 holds the driver's own sentinel text,
// which never comes from the server. Client-side interpolation puts bound values
// inside the SQL text, so any other server message can quote them
// (component-persistence invariants 10 and 13).
var fixedDriverMessages = map[uint16][]string{
	0:    driverSentinelMessages(),
	1706: {"Feature 'COM_STMT_SEND_LONG_DATA' is not supported by SingleStore"},
	1857: {"The open transaction was aborted due to being idle during a concurrent DDL operation."},
}

func driverSentinelMessages() []string {
	messages := make([]string, 0, len(driverSentinels))
	for _, sentinel := range driverSentinels {
		messages = append(messages, sentinel.Error())
	}
	return messages
}

// duplicateKeyMessage recognizes a whole duplicate-key message. The key name is
// the last token and holds only identifier characters, so a row value that
// quotes another key's name cannot stand in for it.
var duplicateKeyMessage = regexp.MustCompile(`(?s)^(?:Leaf Error \([^)]*\): )?Duplicate (?:entry(?: '.*')? for key '([A-Za-z0-9_]+)'|key ([A-Za-z0-9_]+))$`)

// duplicateKeyName takes the key name from the end of a duplicate-key message.
// It reports false for a message whose name cannot be taken safely.
func duplicateKeyName(message string) (string, bool) {
	match := duplicateKeyMessage.FindStringSubmatch(message)
	if match == nil {
		return "", false
	}
	return match[1] + match[2], true
}

// isDuplicateKey reports whether err is a duplicate-key violation of one of the
// named keys. It is the one place a conflict name is matched against a driver
// message, and it never looks inside the quoted row value.
func isDuplicateKey(err error, names ...string) bool {
	var driverErr *mysql.MySQLError
	if !errors.As(err, &driverErr) || driverErr.Number != duplicateEntryError {
		return false
	}
	name, ok := duplicateKeyName(driverErr.Message)
	return ok && slices.Contains(names, name)
}

// translateBackendConflict keeps driver messages within the backend. Named
// conflicts become domain errors. Every other driver condition is logged with
// its operation, error number, SQLSTATE, and only the message text
// admittedDriverMessage allows, then replaced by the generic backend error
// (component-persistence; req-security-boundaries REQ-2).
func translateBackendConflict(err error) error {
	if err == nil {
		return nil
	}
	var driverErr *mysql.MySQLError
	if !errors.As(err, &driverErr) {
		for _, sentinel := range driverSentinels {
			if errors.Is(err, sentinel) {
				logDriverError(0, "", sentinel.Error())
				return errBackendOperation
			}
		}
		return err
	}
	switch driverErr.Number {
	case duplicateEntryError:
		switch {
		case isDuplicateKey(driverErr, "jobs_pkey", "jobs_dispatch_unique"):
			return store.ErrDispatchJobConflict
		case isDuplicateKey(driverErr, "reference_documents_live_name_idx"):
			return store.ErrReferenceDocumentNameConflict
		case isDuplicateKey(driverErr, "task_start_over_task"):
			return store.ErrTaskAlreadyStartedOver
		}
	case 1205, 1213:
		return store.ErrRetryable
	}
	state := ""
	if driverErr.SQLState != [5]byte{} {
		state = string(driverErr.SQLState[:])
	}
	logDriverError(driverErr.Number, state, driverErr.Message)
	return errBackendOperation
}

// logDriverError writes the one server log line for an untranslated driver
// condition. Server text is denied by default: the line holds the operation,
// number, SQLSTATE, and only what admittedDriverMessage allows, redacted and
// bounded, so no SQL text, argument, row value, or leaf address is ever logged.
func logDriverError(number uint16, state, message string) {
	log.Printf("singlestore: database operation failed: operation=%s error=%d sqlstate=%s message=%q", backendOperation(), number, state, store.DriverLogMessage(admittedDriverMessage(number, message)))
}

// admittedDriverMessage rebuilds a duplicate-key message around its key name,
// keeps a message on its number's fixed list, and omits every other message.
func admittedDriverMessage(number uint16, message string) string {
	if number == duplicateEntryError {
		if name, ok := duplicateKeyName(message); ok {
			return "Duplicate entry [value omitted] for key '" + name + "'"
		}
		return omittedDriverMessage
	}
	if slices.Contains(fixedDriverMessages[number], message) {
		return message
	}
	return omittedDriverMessage
}

// storePackage is this package's import path, as runtime function names
// spell it.
var storePackage = reflect.TypeOf(Store{}).PkgPath()

// backendOperation names the nearest exported Store method or exported package
// function on the caller's stack.
func backendOperation() string {
	return store.BackendOperation(storePackage)
}

// exportedOperation reports the exported operation a runtime function name
// belongs to, when it is declared in this package.
func exportedOperation(function string) (string, bool) {
	return store.ExportedBackendOperation(storePackage, function)
}
