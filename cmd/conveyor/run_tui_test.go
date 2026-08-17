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
	if strings.Count(after, "spec approval gate") != 1 || strings.Count(after, "Gate action [approve/changes/wait]") != 1 {
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
	if model.viewport.Width != 46 || model.viewport.Height < 1 || model.viewport.Height >= 9 {
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
	if model.viewport.Width != 22 || model.viewport.Height != 1 {
		t.Fatalf("small resize was not bounded: %dx%d", model.viewport.Width, model.viewport.Height)
	}
	updated, _ = model.Update(tea.WindowSizeMsg{Width: 120, Height: 50})
	model = updated.(runTUIModel)
	if model.viewport.Width != 98 || model.viewport.Height != runTUIBoxMaxHeight {
		t.Fatalf("large terminal did not cap the box: %dx%d", model.viewport.Width, model.viewport.Height)
	}
}

func TestRunTUIGateWithoutOutputHidesBox(t *testing.T) {
	gate := testRunTUIGate()
	model := newRunTUIModel(runTUIStage{started: time.Now()}, &gate, make(chan runTUIAction, 1), make(chan struct{}, 1))
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	view := updated.(runTUIModel).View()
	if strings.Contains(view, "╭") || strings.Contains(view, "waiting for agent output") {
		t.Fatalf("gate frame without stream output rendered the box: %q", view)
	}
	if strings.Count(view, "\n") > 8 {
		t.Fatalf("gate frame is not compact: %d lines", strings.Count(view, "\n")+1)
	}
	updated, _ = updated.(runTUIModel).Update(runTUIOutputMsg("late line\n"))
	if !strings.Contains(updated.(runTUIModel).View(), "╭") {
		t.Fatal("gate frame with stream output did not render the box")
	}
}

func TestRunTUIGateInputRequiresTypedActionsAndFeedback(t *testing.T) {
	actions := make(chan runTUIAction, 2)
	gate := testRunTUIGate()
	model := newRunTUIModel(runTUIStage{}, &gate, actions, make(chan struct{}, 1))

	for _, key := range []tea.KeyMsg{{Type: tea.KeyRunes, Runes: []rune("a")}, {Type: tea.KeyEnter}} {
		updated, _ := model.Update(key)
		model = updated.(runTUIModel)
	}
	select {
	case action := <-actions:
		t.Fatalf("single-key approval produced action: %+v", action)
	default:
	}
	if !strings.Contains(model.View(), "Approval requires the full word approve") {
		t.Fatalf("invalid action guidance missing: %q", model.View())
	}

	for _, key := range []tea.KeyMsg{
		{Type: tea.KeyRunes, Runes: []rune("changes")}, {Type: tea.KeyEnter},
		{Type: tea.KeyRunes, Runes: []rune("fix the race")}, {Type: tea.KeyEnter},
	} {
		updated, _ := model.Update(key)
		model = updated.(runTUIModel)
	}
	action := <-actions
	if action.decision != runGateRequestChanges || action.feedback != "fix the race" {
		t.Fatalf("request changes action = %+v", action)
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
