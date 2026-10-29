package postgres

import (
	"errors"
	"testing"
	"time"
)

// failingDetailRows yields its rows, then fails: with scanErr on the scan
// of the row at failScan, or with iterErr after the last row.
type failingDetailRows struct {
	rows     [][]any
	next     int
	failScan int
	scanErr  error
	iterErr  error
}

func (r *failingDetailRows) Next() bool {
	if r.next >= len(r.rows) {
		return false
	}
	r.next++
	return true
}

func (r *failingDetailRows) Scan(dest ...any) error {
	if r.next == r.failScan {
		return r.scanErr
	}
	for i, value := range r.rows[r.next-1] {
		switch target := dest[i].(type) {
		case *string:
			*target = value.(string)
		case *bool:
			*target = value.(bool)
		case *int:
			*target = value.(int)
		case *time.Time:
			*target = value.(time.Time)
		case *[]string:
			*target = value.([]string)
		}
	}
	return nil
}

func (r *failingDetailRows) Err() error { return r.iterErr }

// TestTaskDetailCollectorsReturnNoPartialResult proves every selection's
// collector returns no result when iteration fails after one row was read,
// or when a later row fails to scan (component-persistence, Task-detail
// batch reads).
func TestTaskDetailCollectorsReturnNoPartialResult(t *testing.T) {
	at := time.Date(2026, 10, 29, 2, 0, 0, 0, time.UTC)
	capture := []any{"order-1", "attempt-1", "content", "", true, at}
	snapshot := []any{"order-1", "attempt-1", "tail", at}
	metadata := []any{"req-1", "requirement", "Title", 2, false, []string{}}
	iterErr := errors.New("connection reset after one row")
	scanErr := errors.New("scan failed on the second row")
	for name, fail := range map[string]func(rows []any) *failingDetailRows{
		"iterator error after one row": func(row []any) *failingDetailRows {
			return &failingDetailRows{rows: [][]any{row}, iterErr: iterErr}
		},
		"scan error on the second row": func(row []any) *failingDetailRows {
			return &failingDetailRows{rows: [][]any{row, row}, failScan: 2, scanErr: scanErr}
		},
	} {
		if got, err := collectTaskCaptures(fail(capture)); err == nil || got != nil {
			t.Fatalf("%s: captures=%v err=%v, want no result and the error", name, got, err)
		}
		if got, err := collectTaskSnapshots(fail(snapshot)); err == nil || got != nil {
			t.Fatalf("%s: snapshots=%v err=%v, want no result and the error", name, got, err)
		}
		if got, err := collectDocumentMetadata(fail(metadata)); err == nil || got.Requirements != nil || got.Designs != nil {
			t.Fatalf("%s: metadata=%+v err=%v, want no result and the error", name, got, err)
		}
	}
	// The same rows without a failure are collected in full.
	captures, err := collectTaskCaptures(&failingDetailRows{rows: [][]any{capture}})
	if err != nil || len(captures["order-1"]) != 1 || captures["order-1"][0].AttemptID != "attempt-1" {
		t.Fatalf("captures=%v err=%v", captures, err)
	}
	metadataSet, err := collectDocumentMetadata(&failingDetailRows{rows: [][]any{metadata}})
	if err != nil || metadataSet.Requirements["req-1"].CurrentVersion != 2 {
		t.Fatalf("metadata=%+v err=%v", metadataSet, err)
	}
}
