package main

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	workerservice "github.com/kidus-tiliksew/conveyor/internal/worker"
)

func testRunTUIGate() runTUIGate {
	return runTUIGate{
		task: core.Task{ID: "target", Title: "Ship target"},
		gate: workerservice.TaskRunGate{
			Kind:              "spec",
			Label:             "spec approval gate",
			Summary:           "execution plan v3",
			CanOperate:        true,
			CanRequestChanges: true,
		},
	}
}

func TestRunTUIGatePollUpdatesInPlace(t *testing.T) {
	actions := make(chan runTUIAction, 1)
	interrupt := make(chan struct{}, 1)
	gate := testRunTUIGate()
	model := newRunTUIModel(runTUIStage{started: time.Now()}, &gate, actions, interrupt)
	before := model.View()
	updated, _ := model.Update(runTUIGateMsg(gate))
	after := updated.(runTUIModel).View()
	if before != after {
		t.Fatalf("identical gate poll changed the view:\nbefore=%q\nafter=%q", before, after)
	}
	if strings.Count(after, "spec approval gate") != 1 || strings.Count(after, "Choose an action") != 1 {
		t.Fatalf("gate block or prompt was duplicated: %q", after)
	}

	gate.gate.Summary = "execution plan v4"
	updated, _ = updated.(runTUIModel).Update(runTUIGateMsg(gate))
	view := updated.(runTUIModel).View()
	if strings.Contains(view, "execution plan v3") || strings.Count(view, "execution plan v4") != 1 {
		t.Fatalf("changed gate did not replace the snapshot in place: %q", view)
	}
}

func TestRunTUIViewportResizeScrollAndFollow(t *testing.T) {
	model := newRunTUIModel(runTUIStage{
		task: core.Task{ID: "target", Title: "Ship target"}, stage: core.StageImplement,
		harness: "codex", model: "gpt-5.6-sol", started: time.Now(),
	}, nil, make(chan runTUIAction, 1), make(chan struct{}, 1))
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 48, Height: 9})
	model = updated.(runTUIModel)
	if model.viewport.Width != 48 || model.viewport.Height < 1 || model.viewport.Height >= 9 {
		t.Fatalf("viewport bounds = %dx%d", model.viewport.Width, model.viewport.Height)
	}
	updated, _ = model.Update(runTUIOutputMsg("one\ntwo\nthree\nfour\nfive\nsix\nseven\n"))
	model = updated.(runTUIModel)
	if !model.viewport.AtBottom() {
		t.Fatal("new output did not follow the newest line")
	}
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyPgUp})
	model = updated.(runTUIModel)
	if model.viewport.AtBottom() {
		t.Fatal("page-up did not expose viewport scrollback")
	}
	offset := model.viewport.YOffset
	updated, _ = model.Update(runTUIOutputMsg("eight\n"))
	model = updated.(runTUIModel)
	if model.viewport.YOffset != offset {
		t.Fatalf("new output stole manual scroll position: before=%d after=%d", offset, model.viewport.YOffset)
	}
	updated, _ = model.Update(tea.WindowSizeMsg{Width: 24, Height: 4})
	model = updated.(runTUIModel)
	if model.viewport.Width != 24 || model.viewport.Height != 1 {
		t.Fatalf("small resize was not bounded: %dx%d", model.viewport.Width, model.viewport.Height)
	}
	updated, _ = model.Update(tea.WindowSizeMsg{Width: 120, Height: 50})
	model = updated.(runTUIModel)
	if model.viewport.Width != 120 || model.viewport.Height != runTUIBoxMaxHeight {
		t.Fatalf("large terminal did not cap the box: %dx%d", model.viewport.Width, model.viewport.Height)
	}
}

func TestRunTUIGateWithoutOutputHidesBox(t *testing.T) {
	gate := testRunTUIGate()
	model := newRunTUIModel(runTUIStage{started: time.Now()}, &gate, make(chan runTUIAction, 1), make(chan struct{}, 1))
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	view := updated.(runTUIModel).View()
	// One rounded border belongs to the header tab; a second would be the
	// output box, which must stay hidden while a gate has no stream output.
	if strings.Count(view, "╭") != 0 || strings.Contains(view, "waiting for agent output") {
		t.Fatalf("gate frame without stream output rendered the box: %q", view)
	}
	if strings.Count(view, "\n") > 9 {
		t.Fatalf("gate frame is not compact: %d lines", strings.Count(view, "\n")+1)
	}
	updated, _ = updated.(runTUIModel).Update(runTUIOutputMsg("late line\n"))
	if strings.Count(updated.(runTUIModel).View(), "╭") != 2 {
		t.Fatal("gate frame with stream output did not render the box")
	}
}

func TestRunTUIGateListUsesWaitDefaultAndTypedApproval(t *testing.T) {
	actions := make(chan runTUIAction, 2)
	gate := testRunTUIGate()
	model := newRunTUIModel(runTUIStage{}, &gate, actions, make(chan struct{}, 1))

	if model.selectedActionKey() != "wait" {
		t.Fatalf("initial action = %q, want wait", model.selectedActionKey())
	}
	updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = updated.(runTUIModel)
	select {
	case action := <-actions:
		t.Fatalf("Wait produced mutation action: %+v", action)
	default:
	}
	for range 2 {
		updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyUp})
		model = updated.(runTUIModel)
	}
	if model.selectedActionKey() != model.gateActionKey("approve") {
		t.Fatalf("arrow selection = %q, want approve", model.selectedActionKey())
	}
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = updated.(runTUIModel)
	select {
	case action := <-actions:
		t.Fatalf("highlighted approval produced action: %+v", action)
	default:
	}
	for _, key := range []tea.KeyMsg{{Type: tea.KeyRunes, Runes: []rune("a")}, {Type: tea.KeyEnter}} {
		updated, _ := model.Update(key)
		model = updated.(runTUIModel)
	}
	select {
	case action := <-actions:
		t.Fatalf("single-key approval produced action: %+v", action)
	default:
	}
	if !strings.Contains(model.View(), "full word approve") {
		t.Fatalf("invalid action guidance missing: %q", model.View())
	}
	for _, key := range []tea.KeyMsg{{Type: tea.KeyRunes, Runes: []rune("approve")}, {Type: tea.KeyEnter}} {
		updated, _ = model.Update(key)
		model = updated.(runTUIModel)
	}
	if action := <-actions; action.decision != runGateApprove {
		t.Fatalf("approval action = %+v", action)
	}

	model = newRunTUIModel(runTUIStage{}, &gate, actions, make(chan struct{}, 1))
	for _, key := range []tea.KeyMsg{
		{Type: tea.KeyUp}, {Type: tea.KeyEnter}, {Type: tea.KeyEnter},
	} {
		updated, _ := model.Update(key)
		model = updated.(runTUIModel)
	}
	if !strings.Contains(model.View(), "Feedback is required") {
		t.Fatalf("blank feedback was not rejected: %q", model.View())
	}
	for _, key := range []tea.KeyMsg{{Type: tea.KeyRunes, Runes: []rune("  fix the race  ")}, {Type: tea.KeyEnter}} {
		updated, _ := model.Update(key)
		model = updated.(runTUIModel)
	}
	action := <-actions
	if action.decision != runGateRequestChanges || action.feedback != "  fix the race  " {
		t.Fatalf("request changes action = %+v", action)
	}
}

func TestRunTUIGateRefreshPreservesPartialActionsFeedbackAndStatus(t *testing.T) {
	actions := make(chan runTUIAction, 2)
	gate := testRunTUIGate()
	model := newRunTUIModel(runTUIStage{}, &gate, actions, make(chan struct{}, 1))
	var updated tea.Model

	for range 2 {
		updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyUp})
		model = updated.(runTUIModel)
	}
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = updated.(runTUIModel)
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("app")})
	model = updated.(runTUIModel)
	for range 2 {
		updated, _ = model.Update(runTUIProposalsMsg{})
		model = updated.(runTUIModel)
		updated, _ = model.Update(runTUIGateMsg(gate))
		model = updated.(runTUIModel)
	}
	if model.input != "app" || !strings.Contains(model.View(), "app") {
		t.Fatalf("partial gate action was lost across refresh: input=%q view=%q", model.input, model.View())
	}
	for _, key := range []tea.KeyMsg{{Type: tea.KeyRunes, Runes: []rune("rove")}, {Type: tea.KeyEnter}} {
		updated, _ = model.Update(key)
		model = updated.(runTUIModel)
	}
	select {
	case action := <-actions:
		if action.decision != runGateApprove {
			t.Fatalf("completed approval action = %+v", action)
		}
	default:
		t.Fatal("completed approval did not produce an action")
	}
	select {
	case action := <-actions:
		t.Fatalf("approval produced more than one action: %+v", action)
	default:
	}

	model = newRunTUIModel(runTUIStage{}, &gate, actions, make(chan struct{}, 1))
	for _, key := range []tea.KeyMsg{{Type: tea.KeyUp}, {Type: tea.KeyEnter}, {Type: tea.KeyRunes, Runes: []rune("fix the")}} {
		updated, _ = model.Update(key)
		model = updated.(runTUIModel)
	}
	updated, _ = model.Update(runTUIProposalsMsg{})
	model = updated.(runTUIModel)
	updated, _ = model.Update(runTUIGateMsg(gate))
	model = updated.(runTUIModel)
	if !model.feedback || model.input != "fix the" {
		t.Fatalf("feedback was lost across refresh: feedback=%t input=%q", model.feedback, model.input)
	}
	for _, key := range []tea.KeyMsg{{Type: tea.KeyRunes, Runes: []rune(" race")}, {Type: tea.KeyEnter}} {
		updated, _ = model.Update(key)
		model = updated.(runTUIModel)
	}
	if action := <-actions; action.decision != runGateRequestChanges || action.feedback != "fix the race" {
		t.Fatalf("request changes action = %+v", action)
	}

	model = newRunTUIModel(runTUIStage{}, &gate, actions, make(chan struct{}, 1))
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyUp})
	model = updated.(runTUIModel)
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = updated.(runTUIModel)
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = updated.(runTUIModel)
	status := model.status
	updated, _ = model.Update(runTUIProposalsMsg{})
	model = updated.(runTUIModel)
	updated, _ = model.Update(runTUIGateMsg(gate))
	model = updated.(runTUIModel)
	if model.status != status {
		t.Fatalf("validation status changed across refresh: before=%q after=%q", status, model.status)
	}
	updated, _ = model.Update(runTUIClearGateMsg{})
	model = updated.(runTUIModel)
	if model.gate != nil || model.input != "" || model.status != "" || model.feedback {
		t.Fatalf("completed gate retained stale prompt state: gate=%+v input=%q status=%q feedback=%t", model.gate, model.input, model.status, model.feedback)
	}
}

func TestRunTUIProposalPanelReplacesStateAndRequiresFullWord(t *testing.T) {
	actions := make(chan runTUIAction, 2)
	model := newRunTUIModel(runTUIStage{task: core.Task{ID: "target"}}, nil, actions, make(chan struct{}, 1))
	design := workerservice.TaskRunProposal{Kind: "design", DocumentID: "design-run", Title: "Attached run", Version: 4, CanConfirm: true, ActorHint: "an operator can confirm"}
	decision := workerservice.TaskRunProposal{Kind: "decision", DocumentID: "DEC-8", Title: "Keep authority server-side", Version: 1, CanConfirm: true, ActorHint: "an operator can confirm"}
	updated, _ := model.Update(runTUIProposalsMsg{design, decision})
	model = updated.(runTUIModel)
	view := model.View()
	for _, want := range []string{"Choose an action", "design-run", "Attached run", "v4", "Wait", "Enter choose"} {
		if !strings.Contains(view, want) {
			t.Fatalf("proposal view missing %q: %q", want, view)
		}
	}
	for _, key := range []tea.KeyMsg{{Type: tea.KeyUp}, {Type: tea.KeyEnter}, {Type: tea.KeyRunes, Runes: []rune("c")}, {Type: tea.KeyEnter}} {
		updated, _ = model.Update(key)
		model = updated.(runTUIModel)
	}
	select {
	case action := <-actions:
		t.Fatalf("partial confirmation produced action: %+v", action)
	default:
	}
	for _, key := range []tea.KeyMsg{{Type: tea.KeyRunes, Runes: []rune("confirm")}, {Type: tea.KeyEnter}} {
		updated, _ = model.Update(key)
		model = updated.(runTUIModel)
	}
	action := <-actions
	if action.decision != runConfirmProposal || action.proposal == nil || action.proposal.DocumentID != decision.DocumentID {
		t.Fatalf("proposal action=%+v", action)
	}
	updated, _ = model.Update(runTUIProposalsMsg{decision})
	model = updated.(runTUIModel)
	if strings.Contains(model.View(), design.DocumentID) || strings.Count(model.View(), decision.DocumentID) != 1 {
		t.Fatalf("proposal refresh did not replace in place: %q", model.View())
	}
	updated, _ = model.Update(runTUIProposalsMsg{})
	if strings.Contains(updated.(runTUIModel).View(), "Confirm design") {
		t.Fatalf("cleared proposal remained visible: %q", updated.(runTUIModel).View())
	}
}

func TestRunTUIActionListFiltersGatePermissions(t *testing.T) {
	tests := []struct {
		name            string
		canOperate      bool
		canRequest      bool
		wantKeys        []string
		unexpectedLabel string
	}{
		{name: "operator", canOperate: true, wantKeys: []string{"approve", "changes", "wait"}},
		{name: "request changes only", canRequest: true, wantKeys: []string{"changes", "wait"}, unexpectedLabel: "Approve"},
		{name: "wait only", wantKeys: []string{"wait"}, unexpectedLabel: "Request changes"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			gate := testRunTUIGate()
			gate.gate.CanOperate = test.canOperate
			gate.gate.CanRequestChanges = test.canRequest
			model := newRunTUIModel(runTUIStage{}, &gate, make(chan runTUIAction, 1), make(chan struct{}, 1))
			var got []string
			for _, raw := range model.actionList.Items() {
				item := raw.(runTUIActionItem)
				switch item.decision {
				case runGateApprove:
					got = append(got, "approve")
				case runGateRequestChanges:
					got = append(got, "changes")
				case runGateStop:
					got = append(got, "wait")
				}
			}
			if strings.Join(got, ",") != strings.Join(test.wantKeys, ",") {
				t.Fatalf("action keys = %v, want %v", got, test.wantKeys)
			}
			if model.selectedActionKey() != "wait" {
				t.Fatalf("initial action = %q, want wait", model.selectedActionKey())
			}
			if test.unexpectedLabel != "" && strings.Contains(model.View(), test.unexpectedLabel) {
				t.Fatalf("view exposed unavailable %q action: %q", test.unexpectedLabel, model.View())
			}
		})
	}
}

func TestRunTUIIdempotentProposalRefreshPreservesSelectionAndInput(t *testing.T) {
	model := newRunTUIModel(runTUIStage{task: core.Task{ID: "target"}}, nil, make(chan runTUIAction, 1), make(chan struct{}, 1))
	design := workerservice.TaskRunProposal{Kind: "design", DocumentID: "design-run", Version: 4, CanConfirm: true}
	decision := workerservice.TaskRunProposal{Kind: "decision", DocumentID: "DEC-8", Version: 1, CanConfirm: true}
	updated, _ := model.Update(runTUIProposalsMsg{design, decision})
	model = updated.(runTUIModel)
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyUp})
	model = updated.(runTUIModel)
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = updated.(runTUIModel)
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("con")})
	model = updated.(runTUIModel)
	updated, _ = model.Update(runTUIProposalsMsg{decision, design})
	model = updated.(runTUIModel)
	if model.input != "con" || model.inputAction != "proposal:"+taskRunProposalKey(decision) || model.selectedActionKey() != "proposal:"+taskRunProposalKey(decision) {
		t.Fatalf("idempotent proposal refresh lost state: input=%q action=%q selected=%q proposals=%+v", model.input, model.inputAction, model.selectedActionKey(), model.proposals)
	}
	decision.CanConfirm = false
	updated, _ = model.Update(runTUIProposalsMsg{decision, design})
	model = updated.(runTUIModel)
	if model.input != "" || model.confirmation != "" || model.inputAction != "" {
		t.Fatalf("capability removal retained confirmation input: input=%q confirmation=%q action=%q", model.input, model.confirmation, model.inputAction)
	}
}

func TestRunTUIProposalWithoutCapabilityShowsActorAndCannotConfirm(t *testing.T) {
	actions := make(chan runTUIAction, 1)
	model := newRunTUIModel(runTUIStage{}, nil, actions, make(chan struct{}, 1))
	proposal := workerservice.TaskRunProposal{Kind: "design", DocumentID: "design-run", Title: "Attached run", Version: 5, ActorHint: "an operator can confirm"}
	updated, _ := model.Update(runTUIProposalsMsg{proposal})
	model = updated.(runTUIModel)
	for _, key := range []tea.KeyMsg{{Type: tea.KeyUp}, {Type: tea.KeyEnter}} {
		updated, _ = model.Update(key)
		model = updated.(runTUIModel)
	}
	select {
	case action := <-actions:
		t.Fatalf("unauthorized proposal produced action: %+v", action)
	default:
	}
	if !strings.Contains(model.View(), "an operator can confirm") || !strings.Contains(model.View(), "unavailable for this credential") {
		t.Fatalf("actor guidance missing: %q", model.View())
	}
}

func TestRunTUICollapseClearsFinalFrame(t *testing.T) {
	model := newRunTUIModel(runTUIStage{task: core.Task{ID: "target"}}, nil, make(chan runTUIAction, 1), make(chan struct{}, 1))
	updated, command := model.Update(runTUICollapseMsg{})
	if command == nil || updated.(runTUIModel).View() != "" {
		t.Fatalf("collapse retained final frame: %q", updated.(runTUIModel).View())
	}
}

func TestRunTUICtrlCInterruptsAndCollapses(t *testing.T) {
	interrupt := make(chan struct{}, 1)
	model := newRunTUIModel(runTUIStage{task: core.Task{ID: "target"}}, nil, make(chan runTUIAction, 1), interrupt)
	updated, command := model.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if command == nil || updated.(runTUIModel).View() != "" {
		t.Fatalf("Ctrl+C retained the active frame: %q", updated.(runTUIModel).View())
	}
	select {
	case <-interrupt:
	default:
		t.Fatal("Ctrl+C did not notify the run controller")
	}
}
