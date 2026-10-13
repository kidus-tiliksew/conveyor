package main

import (
	"bufio"
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	workerservice "github.com/kidus-tiliksew/conveyor/internal/worker"
)

// Plain and TUI run clients agree at the merge_approval gate: merge is
// offered at MERGEABLE, the confirm names the reviewed head and pull request,
// and --yes skips it (DEC-64(1,3)).

func mergeApprovalGate(state string) workerservice.TaskRunGate {
	return workerservice.TaskRunGate{
		Kind: workerservice.TaskRunGateMergeApproval, Label: "merge approval gate", CanOperate: true, CanRequestChanges: true,
		MergeReadiness: &workerservice.TaskRunMergeReadiness{State: state, HeadSHA: "reviewed-head-sha", Number: 7, URL: "https://github.test/o/r/pull/7"},
	}
}

func plainGateDecision(t *testing.T, ctx context.Context, input string) (runGateDecision, string) {
	t.Helper()
	gate := mergeApprovalGate("MERGEABLE")
	var out bytes.Buffer
	answers := newRunInputSource(bufio.NewReader(strings.NewReader(input)))
	decision, _, err := waitAtTaskRunGate(ctx, answers, &out, workerservice.DispatchOrder{Task: core.Task{ID: "t"}, Gate: &gate}, false, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return decision, out.String()
}

func TestPlainRunMergeAtGateConfirmsHeadAndPullRequest(t *testing.T) {
	const named = "Merge reviewed head reviewed-head-sha, pull request #7 https://github.test/o/r/pull/7."
	if decision, out := plainGateDecision(t, context.Background(), "merge\nno\n\n"); decision == runGateMerge || !strings.Contains(out, named) || !strings.Contains(out, "Not merged.") {
		t.Fatalf("declined decision=%v out=%q", decision, out)
	}
	if decision, out := plainGateDecision(t, context.Background(), "merge\nyes\n"); decision != runGateMerge || !strings.Contains(out, "Type yes") {
		t.Fatalf("accepted decision=%v out=%q", decision, out)
	}
	if decision, out := plainGateDecision(t, withRunMergeYes(context.Background(), true), "merge\n"); decision != runGateMerge || strings.Contains(out, "Type yes") || !strings.Contains(out, named) {
		t.Fatalf("--yes decision=%v out=%q", decision, out)
	}
}

func tuiAt(state string, yes bool) (runTUIModel, chan runTUIAction) {
	actions := make(chan runTUIAction, 2)
	gate := runTUIGate{task: core.Task{ID: "t"}, gate: mergeApprovalGate(state)}
	model := newRunTUIModel(runTUIStage{}, &gate, actions, make(chan struct{}, 1))
	model.mergeYes = yes
	return model, actions
}

func TestTUIMergeAtGateOfferedOnlyAtMergeable(t *testing.T) {
	for state, want := range map[string]bool{"MERGEABLE": true, "STALE": false, "CONFLICTING": false} {
		model, _ := tuiAt(state, false)
		_, offered := model.actionItem(model.gateActionKey("merge"))
		_, approve := model.actionItem(model.gateActionKey("approve"))
		if offered != want || !approve {
			t.Fatalf("%s merge offered=%v approve=%v", state, offered, approve)
		}
	}
}

func TestTUIMergeAtGateConfirmNamesHeadAndYesSkips(t *testing.T) {
	model, actions := tuiAt("MERGEABLE", false)
	item, _ := model.actionItem(model.gateActionKey("merge"))
	model.inputAction = item.key
	selectItem(t, &model, item.key)
	model.selectInteractiveAction()
	if !strings.Contains(model.confirmation, "reviewed head reviewed-head-sha, pull request #7") || model.confirmYes {
		t.Fatalf("confirmation=%q yes=%v", model.confirmation, model.confirmYes)
	}
	select {
	case action := <-actions:
		t.Fatalf("merge sent before confirm: %+v", action)
	default:
	}
	model, actions = tuiAt("MERGEABLE", true)
	selectItem(t, &model, model.gateActionKey("merge"))
	model.selectInteractiveAction()
	if action := <-actions; action.decision != runGateMerge || model.confirmation != "" {
		t.Fatalf("--yes action=%+v confirmation=%q", action, model.confirmation)
	}
}

func selectItem(t *testing.T, model *runTUIModel, key string) {
	t.Helper()
	for range 10 {
		if model.selectedActionKey() == key {
			return
		}
		updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyDown})
		*model = updated.(runTUIModel)
	}
	t.Fatalf("action %q not selectable", key)
}
