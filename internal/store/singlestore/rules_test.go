package singlestore

import (
	"errors"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func TestEventsAppendOnly(t *testing.T)           { checkLedgerRule(t, "events") }
func TestDeploymentEventsAppendOnly(t *testing.T) { checkLedgerRule(t, "deployment_events") }
func TestInterventionsAppendOnly(t *testing.T)    { checkLedgerRule(t, "interventions") }
func checkLedgerRule(t *testing.T, table string) {
	t.Helper()
	for _, op := range []string{"UPDATE", "DELETE"} {
		if err := checkWrite(rowWrite{table: table, operation: op}); err == nil {
			t.Fatalf("%s %s accepted", op, table)
		}
	}
	if err := checkWrite(rowWrite{table: table, operation: "INSERT"}); err != nil {
		t.Fatal(err)
	}
}
func TestTaskStates(t *testing.T) {
	for _, state := range core.TaskStates() {
		if err := checkWrite(rowWrite{table: "tasks", values: map[string]any{"state": state}}); err != nil {
			t.Fatal(err)
		}
	}
	if checkWrite(rowWrite{table: "tasks", values: map[string]any{"state": "invented"}}) == nil {
		t.Fatal("invalid task state accepted")
	}
}
func TestWorkOrderStates(t *testing.T) {
	for _, state := range core.WorkOrderStates() {
		if err := checkWrite(rowWrite{table: "work_orders", values: map[string]any{"state": state}}); err != nil {
			t.Fatal(err)
		}
	}
	if checkWrite(rowWrite{table: "work_orders", values: map[string]any{"state": "invented"}}) == nil {
		t.Fatal("invalid work-order state accepted")
	}
}
func TestOneDeploymentCredential(t *testing.T) {
	if checkDeploymentCredential(true, 1) == nil {
		t.Fatal("duplicate accepted")
	}
	if checkDeploymentCredential(true, 0) != nil || checkDeploymentCredential(false, 1) != nil {
		t.Fatal("nonconflicting token refused")
	}
}
func TestLiveReferenceName(t *testing.T) {
	if !errors.Is(checkReferenceName(true, 1), store.ErrReferenceDocumentNameConflict) {
		t.Fatal("duplicate accepted")
	}
	if checkReferenceName(false, 1) != nil || checkReferenceName(true, 0) != nil {
		t.Fatal("nonconflicting name refused")
	}
}
func TestConflictTranslation(t *testing.T) {
	for _, tc := range []struct {
		code    uint16
		message string
		want    error
	}{{1062, "Duplicate key jobs_pkey", store.ErrDispatchJobConflict}, {1062, "Duplicate key reference_documents_live_name_idx", store.ErrReferenceDocumentNameConflict}, {1205, "lock wait timeout", store.ErrRetryable}, {1213, "deadlock", store.ErrRetryable}} {
		err := translateBackendConflict(&mysql.MySQLError{Number: tc.code, Message: tc.message})
		if !errors.Is(err, tc.want) {
			t.Fatalf("%d: %v", tc.code, err)
		}
		var driver *mysql.MySQLError
		if errors.As(err, &driver) {
			t.Fatal("driver error escaped")
		}
	}
}
func TestMigrationLedgerRefusals(t *testing.T) {
	file := migration{version: 1, name: "0001_schema.sql", checksum: "expected"}
	embedded := map[int]migration{1: file}
	for _, record := range []migration{{version: 2}, {version: 1, name: "different"}, {version: 1, name: file.name, checksum: "changed"}} {
		if checkLedger(record, embedded, 1) == nil {
			t.Fatal("invalid ledger accepted")
		}
	}
	if err := checkLedger(file, embedded, 1); err != nil {
		t.Fatal(err)
	}
}
func TestConnectionSettings(t *testing.T) {
	cfg, err := connectionConfig("singlestore://root:fixture@localhost/conveyor_test")
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.ParseTime || cfg.Loc.String() != "UTC" || cfg.Params["time_zone"] != "'+00:00'" || cfg.Params["sql_mode"] != "'STRICT_ALL_TABLES'" || cfg.MultiStatements {
		t.Fatal("unsafe connection configuration")
	}
}

func TestUniqueRuleInputsFailBeforeSQL(t *testing.T) {
	var nilTime *time.Time
	for _, w := range []rowWrite{
		{table: "user_tokens", operation: "INSERT", values: map[string]any{"deployment_credential": 1}},
		{table: "user_tokens", operation: "UPDATE", values: map[string]any{"deployment_credential": true}, where: map[string]any{"user_id": "multiple"}},
		{table: "reference_documents", operation: "INSERT", values: map[string]any{"workspace_id": "ws", "name": "name", "deleted_at": nilTime}},
		{table: "reference_documents", operation: "UPDATE", values: map[string]any{"workspace_id": "ws", "name": "name", "deleted_at": nil}, where: map[string]any{"workspace_id": "ws"}},
	} {
		if _, err := writeRow(t.Context(), nil, w); err == nil {
			t.Fatal("unsafe unique-rule input accepted")
		}
	}
}
