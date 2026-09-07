package singlestore

import (
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"testing"
)

func TestGitHubPublicationConstraintsIntegration(t *testing.T) {
	st, ctx := taskAggregateFixture(t)
	first, second := aggregateTask(ctx, core.NewTaskID()), aggregateTask(ctx, core.NewTaskID())
	taskTestOK(t, st.CreateTask(ctx, first))
	taskTestOK(t, st.CreateTask(ctx, second))
	for _, id := range []string{first.ID, second.ID} {
		taskTestOK(t, st.QueueGitHubLifecycle(ctx, core.GitHubLifecycle{TaskID: id, Repository: "acme/app", SpecVersion: 1}))
	}
	a, _, err := st.GetGitHubLifecycle(ctx, first.ID)
	taskTestOK(t, err)
	a.State = core.GitHubPublicationPublished
	a.IssueNumber = 42
	a.Outcome = "created"
	a.CreateState = core.GitHubCreateConfirmed
	taskTestOK(t, st.UpdateGitHubLifecycle(ctx, a))
	// An identical update remains valid even when MySQL reports zero changed rows.
	taskTestOK(t, st.UpdateGitHubLifecycle(ctx, a))
	b, _, err := st.GetGitHubLifecycle(ctx, second.ID)
	taskTestOK(t, err)
	before, err := st.ListEvents(ctx, second.ID)
	taskTestOK(t, err)
	b.State = core.GitHubPublicationPublished
	b.IssueNumber = 42
	b.CreateState = core.GitHubCreateConfirmed
	if err = st.UpdateGitHubLifecycle(ctx, b); err == nil {
		t.Fatal("duplicate positive issue number accepted")
	}
	after, err := st.ListEvents(ctx, second.ID)
	taskTestOK(t, err)
	unchanged, _, err := st.GetGitHubLifecycle(ctx, second.ID)
	taskTestOK(t, err)
	if len(after) != len(before) || unchanged.State != core.GitHubPublicationQueued || unchanged.IssueNumber != 0 {
		t.Fatal("rejected publication partially committed")
	}
	for _, invalid := range []core.GitHubLifecycle{
		{TaskID: second.ID, State: core.GitHubPublicationPublished, CreateState: core.GitHubCreateConfirmed, IssueNumber: -1},
		{TaskID: second.ID, State: core.GitHubPublicationPublished, CreateState: "invalid"},
		{TaskID: second.ID, State: core.GitHubPublicationPublished, CreateState: core.GitHubCreateConfirmed, Outcome: "invalid"},
	} {
		if err = st.UpdateGitHubLifecycle(ctx, invalid); err == nil {
			t.Fatal("invalid publication projection accepted")
		}
	}
}
