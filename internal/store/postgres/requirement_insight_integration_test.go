package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// requirementInsightStore opens a migrated store whose every query is
// recorded, with one fresh workspace.
func requirementInsightStore(t *testing.T) (*Store, *queryRecorder, context.Context) {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(integrationDatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	recorder := &queryRecorder{}
	cfg.ConnConfig.Tracer = recorder
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err = Migrate(store.WithActor(t.Context(), store.SystemActor()), pool); err != nil {
		t.Fatal(err)
	}
	st := newStore(pool)
	workspace := "insight-" + strings.ToLower(core.NewTaskID())
	if _, err = st.BootstrapWorkspaceConfig(store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), workspace), &config.Config{Workspace: workspace, Repos: []config.Repo{{Name: "conveyor", URL: "https://example.test/conveyor", Base: "main"}}}); err != nil {
		t.Fatal(err)
	}
	return st, recorder, store.WithWorkspace(store.WithActor(t.Context(), store.Actor{ID: "operator", Role: core.ActorHuman}), workspace)
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

// Callers racing an absent row with one fingerprint produce one row and
// refuse the rest; the stored row is the winner's (RI-5; AC-2.6).
func TestRequirementInsightHeadlineConcurrentInsertIntegration(t *testing.T) {
	st, _, ctx := requirementInsightStore(t)
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
}

// Callers replacing a stored row serialize on the row: distinct new
// fingerprints all commit in some order and leave one of them, and a shared
// new fingerprint admits exactly one (RI-5).
func TestRequirementInsightHeadlineConcurrentReplaceIntegration(t *testing.T) {
	st, _, ctx := requirementInsightStore(t)
	requirement := "req-insight-replace-" + strings.ToLower(core.NewTaskID())
	if _, err := st.RecordRequirementInsightHeadline(ctx, store.RequirementInsightHeadline{RequirementID: requirement, Fingerprint: "fp-initial", Headline: "Initial.", Model: "model", GeneratedBy: "user:initial"}); err != nil {
		t.Fatal(err)
	}
	headlines := insightRaceHeadlines(requirement, func(i int) string { return fmt.Sprintf("fp-distinct-%d", i) })
	_, errs := recordInsightHeadlinesConcurrently(st, ctx, headlines)
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
