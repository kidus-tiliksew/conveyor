package storetest

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/monitor"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

type SystemDesignDriftFactory func(t *testing.T) (store.Store, context.Context, string)

func RunSystemDesignDriftConformance(t *testing.T, factory SystemDesignDriftFactory) {
	t.Helper()
	for _, test := range []struct {
		name               string
		proposal           string
		wantDrift          bool
		wantCausalEvidence bool
	}{
		{name: "matching causal proposal suppresses", proposal: "matching", wantDrift: false},
		{name: "unrelated task proposal does not suppress", proposal: "unrelated", wantDrift: true, wantCausalEvidence: true},
		{name: "proposal after merge does not suppress", proposal: "after", wantDrift: true, wantCausalEvidence: true},
		{name: "different document proposal does not suppress", proposal: "different", wantDrift: true, wantCausalEvidence: true},
		{name: "non causal merge reference does not suppress", proposal: "invalid-causal", wantDrift: true, wantCausalEvidence: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			st, ctx, workspace := factory(t)
			now := time.Date(2026, 8, 5, 8, 0, 0, 0, time.UTC)
			service := &monitor.Service{Store: st.(monitor.Store), WorkspaceID: workspace, Enabled: true, Repositories: map[string]struct{}{"conveyor": {}}, Now: func() time.Time { return now }}
			service.Intake = func(ctx context.Context, request monitor.TaskRequest) (monitor.IntakeResult, error) {
				id := core.NewTaskID()
				task := core.Task{ID: id, Workspace: workspace, Repo: request.Repository, BaseBranch: "main", Branch: "conveyor/task-" + id, Source: request.Source, IntakeKey: request.IntakeKey, State: core.TaskQueued, NextStage: core.StageTriage, CreatedAt: now}
				return monitor.IntakeResult{Task: task, Created: true}, st.CreateTask(ctx, task)
			}

			delivery := createDriftTask(t, st, ctx, workspace, "delivery")
			other := createDriftTask(t, st, ctx, workspace, "other")
			document := createConfirmedDesign(t, st, ctx, "DESIGN-main", "internal/dispatch/**")
			if test.proposal == "matching" {
				proposeDesignRevision(t, st, ctx, document.ID, delivery.ID)
			}
			if test.proposal == "unrelated" {
				proposeDesignRevision(t, st, ctx, document.ID, other.ID)
			}
			if test.proposal == "different" {
				different := createConfirmedDesign(t, st, ctx, "DESIGN-other", "internal/httpapi/**")
				proposeDesignRevision(t, st, ctx, different.ID, delivery.ID)
			}

			if err := st.AppendEvent(ctx, core.Event{TaskID: delivery.ID, Kind: "merge.confirmed", Payload: core.JSONPayload(map[string]any{"repository": "kidus-tiliksew/conveyor", "base_sha": "base", "head_sha": "head"})}); err != nil {
				t.Fatal(err)
			}
			mergeEventID := latestEventID(t, st, ctx, delivery.ID, "merge.confirmed")
			if test.proposal == "after" {
				proposeDesignRevision(t, st, ctx, document.ID, delivery.ID)
			}
			causalEventID := mergeEventID
			if test.proposal == "invalid-causal" {
				causalEventID = latestEventID(t, st, ctx, other.ID, "task.created")
			}

			if _, err := service.Process(ctx, monitor.Observation{Repository: "conveyor", Kind: monitor.ExternalPRMerge, OccurrenceID: fmt.Sprintf("merge-%s", test.proposal), SourceURL: "https://example.test/merge/head", CommitSHA: "head", ChangedPaths: []string{"internal/dispatch/service.go"}, CausalEventID: causalEventID}); err != nil {
				t.Fatal(err)
			}
			status, err := service.Status(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var designDrift *monitor.Drift
			for i := range status.Drift {
				if status.Drift[i].SystemDesignID == document.ID {
					designDrift = &status.Drift[i]
				}
			}
			if (designDrift != nil) != test.wantDrift {
				t.Fatalf("design drift=%+v status=%+v", designDrift, status)
			}
			if designDrift != nil {
				if got := designDrift.CausalEventID != 0; got != test.wantCausalEvidence {
					t.Fatalf("causal evidence id=%d want_present=%t", designDrift.CausalEventID, test.wantCausalEvidence)
				}
				if test.wantCausalEvidence && designDrift.CausalEventID != mergeEventID {
					t.Fatalf("causal evidence id=%d want=%d", designDrift.CausalEventID, mergeEventID)
				}
			}
		})
	}
}

func createDriftTask(t *testing.T, st store.Store, ctx context.Context, workspace, suffix string) core.Task {
	t.Helper()
	id := core.NewTaskID()
	task := core.Task{ID: id, Workspace: workspace, Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-" + id, Title: suffix, State: core.TaskQueued, NextStage: core.StageTriage, CreatedAt: time.Now().UTC()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	return task
}

func createConfirmedDesign(t *testing.T, st store.Store, ctx context.Context, id, path string) core.SystemDesign {
	t.Helper()
	content := fmt.Sprintf("# Design\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - %s\n```", path)
	document, version, err := st.CreateSystemDesign(ctx, core.SystemDesign{ID: id, Title: strings.ReplaceAll(id, "-", " "), Category: "Architecture"}, core.SystemDesignVersion{Content: content, Origin: core.SystemDesignOriginOperator})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmSystemDesignVersion(ctx, document.ID, version.Version, 0); err != nil {
		t.Fatal(err)
	}
	return document
}

func proposeDesignRevision(t *testing.T, st store.Store, ctx context.Context, documentID, taskID string) {
	t.Helper()
	content := "# Revised design\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/dispatch/**\n```"
	if _, err := st.ProposeSystemDesignVersion(ctx, core.SystemDesignVersion{DocumentID: documentID, Content: content, Origin: core.SystemDesignOriginImplementation, OriginTaskID: taskID}); err != nil {
		t.Fatal(err)
	}
}

func latestEventID(t *testing.T, st store.Store, ctx context.Context, taskID, kind string) int64 {
	t.Helper()
	events, err := st.ListEvents(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Kind == kind {
			return events[i].ID
		}
	}
	t.Fatalf("task %s has no %s event", taskID, kind)
	return 0
}
