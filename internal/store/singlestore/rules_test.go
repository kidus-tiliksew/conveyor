package singlestore

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func TestEventsAppendOnly(t *testing.T)           { checkLedgerRule(t, "events_columnstore") }
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
	}{{1062, "Duplicate key jobs_pkey", store.ErrDispatchJobConflict}, {1062, "Duplicate key reference_documents_live_name_idx", store.ErrReferenceDocumentNameConflict}, {1062, "Duplicate entry for key 'task_start_over_task'", store.ErrTaskAlreadyStartedOver}, {1205, "lock wait timeout", store.ErrRetryable}, {1213, "deadlock", store.ErrRetryable}} {
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

func TestPublishedMigrationChecksums(t *testing.T) {
	files, err := migrations()
	if err != nil {
		t.Fatal(err)
	}
	embedded := make(map[int]migration, len(files))
	for _, file := range files {
		embedded[file.version] = file
	}
	for version, checksum := range publishedChecksums {
		file := embedded[version]
		data, err := os.ReadFile(filepath.Join("testdata", "published_migrations", file.name))
		if err != nil {
			t.Fatal(err)
		}
		if got := fmt.Sprintf("%x", sha256.Sum256(data)); got != checksum {
			t.Errorf("%s: published checksum = %s, want %s", file.name, got, checksum)
		}
		for _, accepted := range []string{checksum, file.checksum} {
			record := file
			record.checksum = accepted
			if err := checkLedger(record, embedded, files[len(files)-1].version); err != nil {
				t.Errorf("version %d checksum %s: %v", version, accepted, err)
			}
		}
		for otherVersion, otherChecksum := range publishedChecksums {
			if otherVersion == version {
				continue
			}
			for _, foreign := range []string{otherChecksum, embedded[otherVersion].checksum} {
				record := file
				record.checksum = foreign
				if checkLedger(record, embedded, files[len(files)-1].version) == nil {
					t.Errorf("version %d accepted version %d checksum", version, otherVersion)
				}
			}
		}
		record := file
		record.name = "wrong"
		record.checksum = checksum
		if checkLedger(record, embedded, files[len(files)-1].version) == nil {
			t.Errorf("version %d accepted wrong name", version)
		}
	}
	for _, version := range []int{1, 2} {
		for _, checksum := range []string{"", "arbitrary"} {
			record := embedded[version]
			record.checksum = checksum
			if checkLedger(record, embedded, files[len(files)-1].version) == nil {
				t.Errorf("version %d accepted checksum %q", version, checksum)
			}
		}
	}
	if checkLedger(migration{version: 0}, embedded, files[len(files)-1].version) == nil ||
		checkLedger(migration{version: files[len(files)-1].version + 1}, embedded, files[len(files)-1].version) == nil {
		t.Fatal("unknown or newer migration accepted")
	}
}
func TestConnectionSettings(t *testing.T) {
	for i, raw := range []string{"singlestore://root:fixture@localhost/conveyor_test", "root:fixture@tcp(localhost:3306)/conveyor_test?interpolateParams=false"} {
		cfg, err := connectionConfig(raw)
		if err != nil {
			t.Fatal(err)
		}
		if !cfg.ParseTime || cfg.Loc.String() != "UTC" || cfg.Params["time_zone"] != "'+00:00'" || cfg.Params["sql_mode"] != "'STRICT_ALL_TABLES'" || cfg.MultiStatements {
			t.Fatal("unsafe connection configuration")
		}
		// SingleStore refuses the driver's long-data command, so arguments
		// must be interpolated whatever the DSN asks (component-persistence).
		if !cfg.InterpolateParams {
			t.Fatalf("connection form %d does not interpolate parameters", i)
		}
	}
}

func TestConnectionCompression(t *testing.T) {
	const raw = "singlestore://root:fixture@localhost/conveyor_test"
	for _, tc := range []struct {
		name   string
		option []bool
		want   bool
	}{
		{"default", nil, true},
		{"enabled", []bool{true}, true},
		{"disabled", []bool{false}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := connectionConfig(raw, tc.option...)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Contains(cfg.FormatDSN(), "compress=true"); got != tc.want {
				t.Fatalf("compression requested = %t, want %t", got, tc.want)
			}
		})
	}
	for _, raw := range []string{
		"root:fixture@tcp(localhost:3306)/conveyor_test?compress=false",
		"singlestore://root:fixture@localhost/conveyor_test?compress=false",
	} {
		t.Run(raw, func(t *testing.T) {
			cfg, err := connectionConfig(raw)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(cfg.FormatDSN(), "compress=true") {
				t.Fatal("explicit DSN compression opt-out was ignored")
			}
			cfg, err = connectionConfig(raw, true)
			if err != nil || !strings.Contains(cfg.FormatDSN(), "compress=true") {
				t.Fatalf("explicit option did not override DSN: %v", err)
			}
		})
	}
}

func TestUniqueRuleInputsFailBeforeSQL(t *testing.T) {
	var nilTime *time.Time
	for _, w := range []rowWrite{
		{table: "user_tokens", operation: "INSERT", values: map[string]any{"deployment_credential": 1}},
		{table: "user_tokens", operation: "UPDATE", values: map[string]any{"deployment_credential": true}, where: map[string]any{"user_id": "multiple"}},
		{table: "reference_documents", operation: "INSERT", values: map[string]any{"workspace_id": "ws", "name": "name", "deleted_at": nilTime}},
		{table: "reference_documents", operation: "UPDATE", values: map[string]any{"workspace_id": "ws", "name": "name", "deleted_at": nil}, where: map[string]any{"workspace_id": "ws"}},
		{table: "tasks", operation: "DELETE", where: map[string]any{"workspace_id": "ws", "id": "task"}},
		{table: "tasks", operation: "INSERT", values: map[string]any{"workspace_id": "ws", "id": "task", "state": "running"}},
		{table: "tasks", operation: "UPDATE", values: map[string]any{"state": "closed"}, where: map[string]any{"workspace_id": "ws"}},
		{table: "tasks", operation: "UPDATE", values: map[string]any{"intake_key": "key"}, where: map[string]any{"workspace_id": "ws", "id": "task"}},
		{table: "workspaces", operation: "DELETE", where: map[string]any{"id": "ws"}},
		{table: "workspaces", operation: "UPDATE", values: map[string]any{"name": "new"}, where: map[string]any{"id": "ws"}},
		{table: "workspaces", operation: "INSERT", values: map[string]any{"id": "ws"}},
		{table: "workspaces", operation: "INSERT", values: map[string]any{"id": "ws", "name": ""}},
	} {
		if _, err := writeRow(store.WithActor(t.Context(), store.SystemActor()), nil, w); err == nil {
			t.Fatal("unsafe unique-rule input accepted")
		}
	}
}

func TestSingletonOrganization(t *testing.T) {
	for _, ids := range [][]string{nil, {"deployment"}} {
		if err := checkSingletonOrganization(ids); err != nil {
			t.Fatal(err)
		}
	}
	for _, ids := range [][]string{{"other"}, {"deployment", "other"}, {"deployment", "deployment"}} {
		if checkSingletonOrganization(ids) == nil {
			t.Fatal("multiple or foreign organization accepted")
		}
	}
}
