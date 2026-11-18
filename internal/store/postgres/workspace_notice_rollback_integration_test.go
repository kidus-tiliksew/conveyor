package postgres

import (
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/kidus-tiliksew/conveyor/internal/changes"
	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
)

// TestWorkspaceEventNoticeFollowsCommitIntegration proves the workspace
// notice a document proposal or membership event raises through the shared
// workspace event writer joins its transaction: a rolled-back transaction
// publishes nothing, and the same write committed publishes one notice
// (component-persistence invariant 23; DEC-140(3)).
func TestWorkspaceEventNoticeFollowsCommitIntegration(t *testing.T) {
	// A private schema and store: peers are never enabled on it, so no
	// listener can republish an earlier notice into the quiet checks.
	st := newIdentityIntegrationStore(t, 0)
	workspace := "notice-" + core.NewTaskID()
	ctx := store.WithActor(store.WithWorkspace(t.Context(), workspace), store.SystemActor())
	if _, err := st.CreateWorkspace(ctx, workspace, workspace, &config.Config{Workspace: workspace}); err != nil {
		t.Fatal(err)
	}
	notices := st.Changes().Subscribe(workspace, changes.Workspace)
	defer notices.Close()
	drain := func() {
		for {
			select {
			case <-notices.C():
			case <-time.After(300 * time.Millisecond):
				return
			}
		}
	}
	drain()
	aborted := errors.New("abort after the event")
	for _, kind := range []string{"decision.proposed", "workspace.membership_granted"} {
		err := st.inTx(ctx, func(_ pgx.Tx, q *db.Queries) error {
			if err := insertWorkspaceEvent(ctx, q, core.Event{Kind: kind, Payload: core.JSONPayload(map[string]any{"workspace_id": workspace})}); err != nil {
				return err
			}
			return aborted
		})
		if !errors.Is(err, aborted) {
			t.Fatalf("%s: rolled-back transaction err=%v", kind, err)
		}
		select {
		case n := <-notices.C():
			t.Fatalf("%s: a rolled-back transaction published %+v", kind, n)
		case <-time.After(100 * time.Millisecond):
		}
		if err := st.inTx(ctx, func(_ pgx.Tx, q *db.Queries) error {
			return insertWorkspaceEvent(ctx, q, core.Event{Kind: kind, Payload: core.JSONPayload(map[string]any{"workspace_id": workspace})})
		}); err != nil {
			t.Fatal(err)
		}
		select {
		case n := <-notices.C():
			if n.Kind != changes.Workspace || n.Workspace != workspace {
				t.Fatalf("%s: notice=%+v", kind, n)
			}
		default:
			t.Fatalf("%s: the committed transaction published no notice before it returned", kind)
		}
		drain()
	}
}
