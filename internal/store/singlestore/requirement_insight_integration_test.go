package singlestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog/s2log"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

func requirementInsightIntegrationStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	st := integrationStore(t)
	workspace := "insight-" + strings.ToLower(core.NewTaskID())
	if _, err := st.BootstrapWorkspaceConfig(store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), workspace), &config.Config{Workspace: workspace, Repos: []config.Repo{{Name: "conveyor", URL: "https://example.test/conveyor", Base: "main"}}}); err != nil {
		t.Fatal(err)
	}
	return st, store.WithWorkspace(store.WithActor(t.Context(), store.Actor{ID: "operator", Role: core.ActorHuman}), workspace)
}

// seedRequirementInsight creates a confirmed requirement served by count
// tasks in a dependency chain, each attached to a design, with every fifth
// task merged and every seventh closed.
func seedRequirementInsight(t *testing.T, st *Store, ctx context.Context, count int) string {
	t.Helper()
	suffix := strings.ToLower(core.NewTaskID())
	requirement := "req-insight-scale-" + suffix
	_, version, err := st.CreateRequirement(ctx, core.Requirement{ID: requirement, Title: "Scale"}, core.RequirementVersion{Content: "# Scale", Origin: core.RequirementOriginOperator, Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Insights scale."}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmRequirementVersion(ctx, requirement, version.Version); err != nil {
		t.Fatal(err)
	}
	design := "design-insight-scale-" + suffix
	_, designVersion, err := st.CreateSystemDesign(ctx, core.SystemDesign{ID: design, Title: "Scale design", Category: "Component design"}, core.SystemDesignVersion{Content: "# Scale design\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/insight/**\n```", Origin: core.SystemDesignOriginOperator})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmSystemDesignVersion(ctx, design, designVersion.Version); err != nil {
		t.Fatal(err)
	}
	plane := taskops.New(st)
	workspace, _ := store.WorkspaceFromContext(ctx)
	created := time.Now().UTC().Add(-24 * time.Hour)
	previous := ""
	for i := 0; i < count; i++ {
		id := fmt.Sprintf("insight-%04d-%s", i, suffix)
		var dependencies []string
		if previous != "" {
			dependencies = []string{previous}
		}
		task := core.Task{ID: id, Workspace: workspace, Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-" + id, Title: "Insight " + id, Body: "Body " + id, State: core.TaskQueued, NextStage: core.StageImplement, CreatedAt: created.Add(time.Duration(i) * time.Second)}
		if err = st.CreateTaskWithDependencies(ctx, task, dependencies); err != nil {
			t.Fatal(err)
		}
		if _, err = st.UpdateTaskContext(ctx, id, store.TaskContextChange{Add: store.TaskContextInput{RequirementIDs: []string{requirement}, DesignIDs: []string{design}}}); err != nil {
			t.Fatal(err)
		}
		switch {
		case i%5 == 0:
			_, err = plane.Perform(ctx, id, taskops.Command{Kind: core.TaskMergeRecover})
		case i%7 == 0:
			_, err = plane.Perform(ctx, id, taskops.Command{Kind: core.TaskCancel})
		}
		if err != nil {
			t.Fatal(err)
		}
		previous = id
	}
	return requirement
}

// The input read issues the same fixed number of queries for 3 and 200
// serving tasks, covering task rows with bodies, dependencies, designs, and
// terminal times (feature-requirement-insights RI-2; req-requirement-insights
// AC-3.7).
func TestRequirementInsightInputsBoundedQueriesIntegration(t *testing.T) {
	st, ctx := requirementInsightIntegrationStore(t)
	recorder := &selectedBytesRecorder{}
	connector, err := mysql.NewConnector(sharedDatabase.cfg)
	if err != nil {
		t.Fatal(err)
	}
	db := sql.OpenDB(recordingConnector{Connector: connector, recorder: recorder})
	t.Cleanup(func() { _ = db.Close() })
	recorded := &Store{db: db, log: s2log.New(db)}
	counts := map[int]int{}
	for _, size := range []int{3, 200} {
		requirement := seedRequirementInsight(t, st, ctx, size)
		recorder.start()
		inputs, err := recorded.ReadRequirementInsightInputs(ctx, requirement)
		queries, _ := recorder.stop()
		if err != nil {
			t.Fatal(err)
		}
		if len(inputs.Tasks) != size {
			t.Fatalf("size %d: %d serving tasks", size, len(inputs.Tasks))
		}
		merged, closed := 0, 0
		for i, task := range inputs.Tasks {
			if task.Task.Body == "" || len(task.Designs) != 1 || (i > 0 && len(task.DependsOn) != 1) {
				t.Fatalf("size %d: task %+v lacks its body, design, or dependency", size, task)
			}
			if !task.MergedAt.IsZero() {
				merged++
			}
			if !task.ClosedAt.IsZero() {
				closed++
			}
		}
		if merged == 0 || (closed == 0 && size > 7) {
			t.Fatalf("size %d: merged=%d closed=%d", size, merged, closed)
		}
		counts[size] = len(queries)
		t.Logf("SingleStore requirement insight inputs: %d tasks, %d queries", size, len(queries))
	}
	if counts[3] != counts[200] || counts[200] == 0 || counts[200] > 8 {
		t.Fatalf("input read queries %v, want an equal fixed count of at most 8", counts)
	}
}

func recordInsightHeadlinesConcurrently(st *Store, ctx context.Context, headlines []store.RequirementInsightHeadline) ([]store.RequirementInsightHeadline, []error) {
	start := make(chan struct{})
	results := make([]store.RequirementInsightHeadline, len(headlines))
	errs := make([]error, len(headlines))
	var ready, done sync.WaitGroup
	for i := range headlines {
		ready.Add(1)
		done.Add(1)
		go func() {
			defer done.Done()
			ready.Done()
			<-start
			results[i], errs[i] = st.RecordRequirementInsightHeadline(ctx, headlines[i])
		}()
	}
	ready.Wait()
	close(start)
	done.Wait()
	return results, errs
}

func insightRaceHeadlines(requirement string, fingerprint func(int) string) []store.RequirementInsightHeadline {
	var out []store.RequirementInsightHeadline
	for i := 0; i < 8; i++ {
		out = append(out, store.RequirementInsightHeadline{RequirementID: requirement, Fingerprint: fingerprint(i), Headline: fmt.Sprintf("Caller %d.", i), Model: "model", TokensIn: int64(i), TokensOut: int64(i), GeneratedBy: fmt.Sprintf("user:%d", i)})
	}
	return out
}

// Independent callers racing an absent row with one fingerprint produce one
// row: the primary key decides the insert, and every loser reads the
// committed row under its lock and refuses (RI-5; AC-2.6).
func TestRequirementInsightHeadlineConcurrentInsertIntegration(t *testing.T) {
	st, ctx := requirementInsightIntegrationStore(t)
	requirement := "req-insight-race-" + strings.ToLower(core.NewTaskID())
	results, errs := recordInsightHeadlinesConcurrently(st, ctx, insightRaceHeadlines(requirement, func(int) string { return "fp-race" }))
	winner := -1
	for i, err := range errs {
		switch {
		case err == nil:
			if winner >= 0 {
				t.Fatalf("callers %d and %d both stored the headline", winner, i)
			}
			winner = i
		case !errors.Is(err, store.ErrInsightHeadlineCurrent):
			t.Fatalf("caller %d: %v", i, err)
		}
	}
	if winner < 0 {
		t.Fatal("no caller stored the headline")
	}
	stored, err := st.GetRequirementInsightHeadline(ctx, requirement)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Headline != results[winner].Headline || stored.GeneratedBy != results[winner].GeneratedBy || !stored.GeneratedAt.Equal(results[winner].GeneratedAt) {
		t.Fatalf("stored %+v, want the winner %+v", stored, results[winner])
	}
	var rows int
	if err = st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM requirement_insight_headlines WHERE workspace_id=? AND requirement_id=?`, documentWorkspace(ctx), requirement).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("headline rows=%d err=%v, want one", rows, err)
	}
}

// Callers replacing a stored row serialize on the row lock: distinct new
// fingerprints all commit and leave one of them, and a shared new
// fingerprint admits exactly one (RI-5).
func TestRequirementInsightHeadlineConcurrentReplaceIntegration(t *testing.T) {
	st, ctx := requirementInsightIntegrationStore(t)
	requirement := "req-insight-replace-" + strings.ToLower(core.NewTaskID())
	if _, err := st.RecordRequirementInsightHeadline(ctx, store.RequirementInsightHeadline{RequirementID: requirement, Fingerprint: "fp-initial", Headline: "Initial.", Model: "model", GeneratedBy: "user:initial"}); err != nil {
		t.Fatal(err)
	}
	_, errs := recordInsightHeadlinesConcurrently(st, ctx, insightRaceHeadlines(requirement, func(i int) string { return fmt.Sprintf("fp-distinct-%d", i) }))
	for i, err := range errs {
		if err != nil {
			t.Fatalf("distinct caller %d: %v", i, err)
		}
	}
	stored, err := st.GetRequirementInsightHeadline(ctx, requirement)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(stored.Fingerprint, "fp-distinct-") {
		t.Fatalf("stored %+v, want one of the replacements", stored)
	}
	results, errs := recordInsightHeadlinesConcurrently(st, ctx, insightRaceHeadlines(requirement, func(int) string { return "fp-shared" }))
	winners := 0
	for i, err := range errs {
		switch {
		case err == nil:
			winners++
			stored = results[i]
		case !errors.Is(err, store.ErrInsightHeadlineCurrent):
			t.Fatalf("shared caller %d: %v", i, err)
		}
	}
	if winners != 1 {
		t.Fatalf("shared fingerprint winners=%d, want 1", winners)
	}
	got, err := st.GetRequirementInsightHeadline(ctx, requirement)
	if err != nil {
		t.Fatal(err)
	}
	if got.Fingerprint != "fp-shared" || got.Headline != stored.Headline || !got.GeneratedAt.Equal(stored.GeneratedAt) {
		t.Fatalf("stored %+v, want the shared winner %+v", got, stored)
	}
}
