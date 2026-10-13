package postgres

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
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
)

// TestInsertEventRequiresActor proves every PostgreSQL event writer refuses a
// context without a complete actor before any SQL: each call gets a nil
// query handle, so reaching SQL would panic. An actor named on the event
// envelope does not bypass the check (component-persistence, Actor context).
func TestInsertEventRequiresActor(t *testing.T) {
	envelope := core.Event{TaskID: "task-1", Kind: "test.event", ActorID: "user:usr-1", ActorRole: core.ActorUser}
	bare := store.WithWorkspace(context.Background(), "demo")
	for name, ctx := range map[string]context.Context{
		"absent":  bare,
		"empty":   store.WithActor(bare, store.Actor{}),
		"partial": store.WithActor(bare, store.Actor{ID: "user:usr-1"}),
	} {
		writers := map[string]func() error{
			"insertEvent":       func() error { return insertEvent(ctx, nil, envelope) },
			"insertEventWithID": func() error { _, err := insertEventWithID(ctx, nil, envelope); return err },
			"insertWorkspaceEvent": func() error {
				workspaceEvent := envelope
				workspaceEvent.TaskID = ""
				return insertWorkspaceEvent(ctx, nil, workspaceEvent)
			},
			"insertWorkspaceEventRow": func() error {
				_, err := insertWorkspaceEventRow(ctx, nil, db.InsertWorkspaceEventParams{WorkspaceID: "demo", Kind: "test.event", ActorID: "user:usr-1", ActorRole: string(core.ActorUser)})
				return err
			},
			"insertDeploymentEvent": func() error {
				return insertDeploymentEvent(ctx, nil, db.InsertDeploymentEventParams{Kind: "test.event", ActorID: "system", ActorRole: string(core.ActorSystem)})
			},
		}
		for writer, call := range writers {
			if err := call(); !errors.Is(err, store.ErrMissingActor) {
				t.Fatalf("%s context, %s: error=%v", name, writer, err)
			}
		}
	}
}

// TestEventWritersUseGuardedHelpers scans the package source: the generated
// event inserts are called only from the guarded helpers in store.go, and the
// only raw task-event INSERT outside migrations checks RequireActor first.
func TestEventWritersUseGuardedHelpers(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	generated := regexp.MustCompile(`\.(InsertEvent|InsertWorkspaceEvent|InsertDeploymentEvent)\(ctx`)
	raw := regexp.MustCompile("INSERT INTO (events|deployment_events)")
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		body, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		source := string(body)
		for number, line := range strings.Split(source, "\n") {
			if generated.MatchString(line) && file != "store.go" {
				t.Errorf("%s:%d calls a generated event insert outside the guarded helpers: %s", file, number+1, strings.TrimSpace(line))
			}
			if raw.MatchString(line) && file != "migrate.go" && file != "lifecycle_store.go" {
				t.Errorf("%s:%d writes a ledger row without a guarded helper: %s", file, number+1, strings.TrimSpace(line))
			}
		}
		if file == "lifecycle_store.go" {
			guard, insert := strings.Index(source, "store.RequireActor(ctx)"), strings.Index(source, "INSERT INTO events")
			if guard < 0 || insert < 0 || guard > insert {
				t.Errorf("lifecycle_store.go raw event insert is not preceded by RequireActor")
			}
		}
	}
}
