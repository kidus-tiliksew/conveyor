package singlestore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// TestInsertEventRequiresActor proves the SingleStore event writers refuse a
// context without a complete actor before any SQL: each call gets a nil
// transaction, so reaching SQL would panic. An actor named on the event
// envelope or the row values does not bypass the check (component-persistence,
// Actor context).
func TestInsertEventRequiresActor(t *testing.T) {
	envelope := core.Event{TaskID: "task-1", Kind: "test.event", ActorID: "user:usr-1", ActorRole: core.ActorUser}
	bare := store.WithWorkspace(context.Background(), "demo")
	for name, ctx := range map[string]context.Context{
		"absent":  bare,
		"empty":   store.WithActor(bare, store.Actor{}),
		"partial": store.WithActor(bare, store.Actor{Role: core.ActorUser}),
	} {
		writers := map[string]func() error{
			"insertEvent":       func() error { return insertEvent(ctx, nil, envelope) },
			"insertEventWithID": func() error { _, err := insertEventWithID(ctx, nil, envelope); return err },
			"insertWorkspaceEvent": func() error {
				workspaceEvent := envelope
				workspaceEvent.TaskID = ""
				return insertWorkspaceEvent(ctx, nil, workspaceEvent)
			},
			"writeRow events": func() error {
				_, err := writeRow(ctx, nil, rowWrite{table: "events", operation: "INSERT", values: map[string]any{"workspace_id": "demo", "kind": "test.event", "actor_id": "user:usr-1", "actor_role": "human"}})
				return err
			},
			"appendDeploymentEvent": func() error { return appendDeploymentEvent(ctx, nil, "test.event", map[string]any{}) },
		}
		for writer, call := range writers {
			if err := call(); !errors.Is(err, store.ErrMissingActor) {
				t.Fatalf("%s context, %s: error=%v", name, writer, err)
			}
		}
	}
}

// TestLedgerRowsUseGuardedWriter scans the package source: ledger rows are
// written only through writeRow, whose ledger guard requires the context
// actor, so no raw SQL appends an event without that check.
func TestLedgerRowsUseGuardedWriter(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	raw := regexp.MustCompile("(?i)INSERT\\s+INTO\\s+`?(events|deployment_events)`?\\b")
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		body, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for number, line := range strings.Split(string(body), "\n") {
			if raw.MatchString(line) {
				t.Errorf("%s:%d writes a ledger row outside writeRow: %s", file, number+1, strings.TrimSpace(line))
			}
		}
	}
}
