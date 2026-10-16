package storetest

import (
	"errors"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// runWorkspaceNameUniqueness keeps case- and space-folded workspace names
// consistent across every backend (DEC-75(7), DEC-70, component-persistence).
func runWorkspaceNameUniqueness(t *testing.T, x Fixture) {
	t.Helper()
	control, ok := x.Backend.(store.WorkspaceControlStore)
	if !ok {
		t.Fatal("backend must implement WorkspaceControlStore")
	}
	id := x.Workspace + "-workspace-name-alpha"
	cfg := &config.Config{Workspace: id, Repos: x.Config.Repos}
	if _, err := control.ListWorkspaces(x.Context); err != nil {
		t.Fatal(err)
	}
	if _, err := control.CreateWorkspace(x.Context, id, "Workspace Alpha", cfg); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		id, name string
	}{
		{id: x.Workspace + "-workspace-name-case", name: "workspace alpha"},
		{id: x.Workspace + "-workspace-name-space", name: " workspace alpha "},
	} {
		if _, err := control.CreateWorkspace(x.Context, tc.id, tc.name, &config.Config{Workspace: tc.id, Repos: x.Config.Repos}); !errors.Is(err, store.ErrWorkspaceConflict) {
			t.Fatalf("workspace name %q: got %v, want ErrWorkspaceConflict", tc.name, err)
		}
	}
	tabID := x.Workspace + "-workspace-name-tab"
	if _, err := control.CreateWorkspace(x.Context, tabID, "\tWorkspace Alpha\t", &config.Config{Workspace: tabID, Repos: x.Config.Repos}); err != nil {
		t.Fatalf("tab-prefixed workspace name refused: %v", err)
	}
	otherID := x.Workspace + "-workspace-name-alphabet"
	if _, err := control.CreateWorkspace(x.Context, otherID, "Workspace Alphabet", &config.Config{Workspace: otherID, Repos: x.Config.Repos}); err != nil {
		t.Fatalf("distinct workspace name refused: %v", err)
	}
}
