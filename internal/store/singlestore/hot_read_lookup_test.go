package singlestore

import (
	"reflect"
	"strings"
	"testing"
)

func TestHotReadLookupNamesDigestOnlyWhenKeyIsReady(t *testing.T) {
	const documentOriginal = "BINARY JSON_EXTRACT_STRING(payload_json,'document_id')=BINARY ?"
	const intakeOriginal = "BINARY intake_key=BINARY ?"
	tests := []struct {
		name     string
		store    *Store
		index    string
		column   string
		original string
		value    string
		wantSQL  string
		wantArgs []any
	}{
		{
			name:     "ready document key names the digest and keeps the original guard",
			store:    &Store{hotReadLayout: map[string]bool{"events_document_key_idx": true}},
			index:    "events_document_key_idx",
			column:   "event_document_key",
			original: documentOriginal,
			value:    "doc-1",
			wantSQL:  "event_document_key=SHA2(?,256) AND " + documentOriginal,
			wantArgs: []any{"doc-1", "doc-1"},
		},
		{
			name:     "other ready keys do not enable the document digest",
			store:    &Store{hotReadLayout: map[string]bool{"events_session_key_idx": true}},
			index:    "events_document_key_idx",
			column:   "event_document_key",
			original: documentOriginal,
			value:    "doc-1",
			wantSQL:  documentOriginal,
			wantArgs: []any{"doc-1"},
		},
		{
			name:     "unset layout keeps only the original guard",
			store:    &Store{},
			index:    "events_document_key_idx",
			column:   "event_document_key",
			original: documentOriginal,
			value:    "doc-1",
			wantSQL:  documentOriginal,
			wantArgs: []any{"doc-1"},
		},
		{
			name:     "ready intake key names the digest with the raw intake guard",
			store:    &Store{hotReadLayout: map[string]bool{"tasks_workspace_intake_lookup_idx": true}},
			index:    "tasks_workspace_intake_lookup_idx",
			column:   "task_intake_key",
			original: intakeOriginal,
			value:    "REQ-1",
			wantSQL:  "task_intake_key=SHA2(?,256) AND " + intakeOriginal,
			wantArgs: []any{"REQ-1", "REQ-1"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sql, args := tc.store.hotReadLookup(tc.index, tc.column, tc.original, tc.value)
			if sql != tc.wantSQL {
				t.Fatalf("predicate = %q, want %q", sql, tc.wantSQL)
			}
			if !reflect.DeepEqual(args, tc.wantArgs) {
				t.Fatalf("args = %#v, want %#v", args, tc.wantArgs)
			}
			if got := strings.Count(sql, "?"); got != len(args) {
				t.Fatalf("%d placeholders for %d args in %q", got, len(args), sql)
			}
		})
	}
}
