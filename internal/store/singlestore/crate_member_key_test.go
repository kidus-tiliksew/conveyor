package singlestore

import (
	"strings"
	"testing"
)

// TestCrateMemberKeyCheck: migration 0047 accepts an existing
// tasks_workspace_crate_created_idx only when it is a non-unique ordered key
// on exactly (workspace_id, crate_id, created_at, id) in that order. Any
// other definition, or a missing key, refuses the start
// (component-persistence invariant 36).
func TestCrateMemberKeyCheck(t *testing.T) {
	parts := func(kind string, nonUnique int, columns ...string) []hotReadKeyPart {
		out := make([]hotReadKeyPart, len(columns))
		for i, column := range columns {
			out[i] = hotReadKeyPart{column: column, kind: kind, nonUnique: nonUnique}
		}
		return out
	}
	for _, tc := range []struct {
		name  string
		parts []hotReadKeyPart
		ok    bool
	}{
		{"skiplist key", parts("SKIPLIST", 1, "workspace_id", "crate_id", "created_at", "id"), true},
		{"btree key", parts("BTREE", 1, "workspace_id", "crate_id", "created_at", "id"), true},
		{"upper-case column names", parts("BTREE", 1, "WORKSPACE_ID", "CRATE_ID", "CREATED_AT", "ID"), true},
		{"missing key", nil, false},
		{"unique key", parts("BTREE", 0, "workspace_id", "crate_id", "created_at", "id"), false},
		{"hash key", parts("HASH", 1, "workspace_id", "crate_id", "created_at", "id"), false},
		{"columns out of order", parts("BTREE", 1, "workspace_id", "created_at", "crate_id", "id"), false},
		{"too few columns", parts("BTREE", 1, "workspace_id", "crate_id"), false},
		{"extra column", parts("BTREE", 1, "workspace_id", "crate_id", "created_at", "id", "state"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkCrateMemberKey(tc.parts)
			if tc.ok && err != nil {
				t.Fatalf("checkCrateMemberKey = %v, want accepted", err)
			}
			if !tc.ok && (err == nil || !strings.Contains(err.Error(), "incompatible crate member key")) {
				t.Fatalf("checkCrateMemberKey = %v, want an incompatible key refusal", err)
			}
		})
	}
}
