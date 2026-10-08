package postgres

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// Inject database failures at successive writes, including after dispatch was
// enqueued, to prove the compound operation has one commit boundary.
func TestTaskStartOverRollbackAtWriteBoundariesIntegration(t *testing.T) {
	for _, phase := range []string{"cancel", "dismiss", "create", "artifact", "links", "final_event"} {
		t.Run(phase, func(t *testing.T) {
			st := newIdentityIntegrationStore(t, 0)
			ctx := store.WithWorkspace(t.Context(), "restart-rollback")
			ctx = store.WithActor(ctx, store.Actor{ID: "restart-operator", Role: core.ActorHuman})
			if _, err := st.BootstrapWorkspaceConfig(ctx, &config.Config{Workspace: "restart-rollback", Repos: []config.Repo{{Name: "conveyor", URL: "https://example.test/conveyor", Base: "main"}}}); err != nil {
				t.Fatal(err)
			}
			old := core.Task{ID: core.NewTaskID(), Workspace: "restart-rollback", Title: "Rollback", Body: "Restart rollback", Repo: "conveyor", BaseBranch: "main", State: core.TaskRunning}
			old.Branch = "conveyor/task-" + old.ID
			if err := st.CreateTask(ctx, old); err != nil {
				t.Fatal(err)
			}
			requirement, pending, err := st.CreateRequirement(ctx, core.Requirement{ID: "req-" + core.NewTaskID(), Title: "Pending"}, core.RequirementVersion{Content: "# Pending", Origin: core.RequirementOriginImplementation, OriginTaskID: old.ID, Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Pending requirement."}}})
			if err != nil {
				t.Fatal(err)
			}
			spec, err := st.CreateSpecVersion(ctx, core.SpecVersion{TaskID: old.ID, Content: "## Previous plan", Acceptance: core.JSONPayload([]any{}), Decomposition: core.JSONPayload([]any{})})
			if err != nil {
				t.Fatal(err)
			}
			if err = st.ApproveSpecVersion(ctx, old.ID, spec.Version); err != nil {
				t.Fatal(err)
			}
			table, operation, condition := "events", "INSERT", "NEW.kind='task.cancelled'"
			switch phase {
			case "dismiss":
				condition = "NEW.kind='requirement.version_dismissed'"
			case "create":
				condition = "NEW.kind='task.created'"
			case "artifact":
				table = "artifact_links"
				condition = "true"
			case "links":
				table = "tasks"
				operation = "UPDATE"
				condition = "NEW.superseded_by IS NOT NULL"
			case "final_event":
				condition = "NEW.kind='task.started_over'"
			}
			ddl := fmt.Sprintf(`CREATE FUNCTION restart_failure() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF %s THEN RAISE EXCEPTION 'restart injected failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER restart_failure BEFORE %s ON %s FOR EACH ROW EXECUTE FUNCTION restart_failure()`, condition, operation, table)
			if _, err = st.pool.Exec(ctx, ddl); err != nil {
				t.Fatal(err)
			}
			eventsBefore, _ := st.ListEvents(ctx, old.ID)
			// The trigger's message is driver text, so it stays in the server
			// log and the caller receives the store's backend-operation error
			// (component-persistence; DEC-38).
			driverLog := &lockedBuffer{}
			previousLog := log.Writer()
			log.SetOutput(driverLog)
			_, err = taskops.New(st).StartOver(ctx, core.TaskStartOverRequest{TaskID: old.ID, RequestID: "failure", Reason: "restart", Note: "dismiss note", CanConfirmDocuments: true})
			log.SetOutput(previousLog)
			if !errors.Is(err, store.ErrBackendOperation) || strings.Contains(err.Error(), "restart injected failure") || !strings.Contains(driverLog.String(), "restart injected failure") {
				t.Fatalf("injection did not run behind the store boundary: err=%v log=%q", err, driverLog.String())
			}
			task, err := st.GetTask(ctx, old.ID)
			if err != nil || task.State != old.State || task.SupersededBy != "" {
				t.Fatalf("partial task: %+v %v", task, err)
			}
			version, err := st.GetRequirementVersion(ctx, requirement.ID, pending.Version)
			if err != nil || version.Retired || version.DismissalNote != "" {
				t.Fatalf("partial dismissal: %+v %v", version, err)
			}
			eventsAfter, _ := st.ListEvents(ctx, old.ID)
			if len(eventsBefore) != len(eventsAfter) {
				t.Fatal("partial cancellation events")
			}
			for _, query := range []string{`SELECT count(*) FROM task_start_overs`, `SELECT count(*) FROM tasks WHERE supersedes IS NOT NULL`, `SELECT count(*) FROM artifacts`, `SELECT count(*) FROM event_log`} {
				var count int
				if err := st.pool.QueryRow(ctx, query).Scan(&count); err != nil {
					t.Fatal(err)
				}
				if count != 0 {
					t.Fatalf("partial write: %s = %d", query, count)
				}
			}
		})
	}
}

// lockedBuffer is a log destination safe for concurrent writers.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
