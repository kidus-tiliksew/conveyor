package singlestore

import (
	"database/sql/driver"
	"errors"
	"fmt"
	"log"
	"reflect"
	"strings"

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

// translateBackendConflict keeps driver messages within the backend. Named
// conflicts become domain errors. Every other driver condition is logged with
// its operation, error number, SQLSTATE, and redacted message, then replaced
// by the generic backend error, as PostgreSQL's translateDriverError does
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
	case 1062:
		for _, key := range []string{"jobs_pkey", "jobs_dispatch_unique"} {
			if strings.Contains(driverErr.Message, key) {
				return store.ErrDispatchJobConflict
			}
		}
		if strings.Contains(driverErr.Message, "reference_documents_live_name_idx") {
			return store.ErrReferenceDocumentNameConflict
		}
		if strings.Contains(driverErr.Message, "task_start_over_task") {
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
// condition. The message is redacted and bounded; no SQL text or argument is
// ever logged.
func logDriverError(number uint16, state, message string) {
	log.Printf("singlestore: database operation failed: operation=%s error=%d sqlstate=%s message=%q", backendOperation(), number, state, store.DriverLogMessage(message))
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
