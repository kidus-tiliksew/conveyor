package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/adapter"
	"github.com/kidus-tiliksew/conveyor/internal/snapshot"
)

type adapterScript struct {
	events []adapter.Event
	err    error
}

type scriptedAdapter struct {
	capabilities  adapter.Capabilities
	runScripts    []adapterScript
	resumeScripts []adapterScript
	runSpecs      []adapter.RunSpec
	resumeRefs    []string
}

func (a *scriptedAdapter) Name() string { return "scripted" }

func (a *scriptedAdapter) Capabilities() adapter.Capabilities { return a.capabilities }

func (a *scriptedAdapter) Run(_ context.Context, spec adapter.RunSpec) (<-chan adapter.Event, error) {
	a.runSpecs = append(a.runSpecs, spec)
	if len(a.runScripts) == 0 {
		return nil, errors.New("unexpected Run call")
	}
	script := a.runScripts[0]
	a.runScripts = a.runScripts[1:]
	return scriptedEvents(script)
}

func (a *scriptedAdapter) Resume(_ context.Context, sessionRef, _ string) (<-chan adapter.Event, error) {
	a.resumeRefs = append(a.resumeRefs, sessionRef)
	if len(a.resumeScripts) == 0 {
		return nil, errors.New("unexpected Resume call")
	}
	script := a.resumeScripts[0]
	a.resumeScripts = a.resumeScripts[1:]
	return scriptedEvents(script)
}

func scriptedEvents(script adapterScript) (<-chan adapter.Event, error) {
	if script.err != nil {
		return nil, script.err
	}
	ch := make(chan adapter.Event, len(script.events))
	for _, ev := range script.events {
		ch <- ev
	}
	close(ch)
	return ch, nil
}

func TestSuperviseFallsBackWithoutResumeAndEmitsOneTerminalDone(t *testing.T) {
	a := &scriptedAdapter{
		capabilities: adapter.Capabilities{Resume: false},
		runScripts: []adapterScript{
			{events: []adapter.Event{
				{Kind: adapter.EventAssistantText, Text: "implementation complete"},
				{Kind: adapter.EventDone},
			}},
			{events: []adapter.Event{
				{Kind: adapter.EventSessionStart, SessionRef: "fallback-session"},
				{Kind: adapter.EventAssistantText, Text: `{"state":"done","todos":["await review"]}`},
				{Kind: adapter.EventTokenUsage, Usage: &adapter.TokenUsage{In: 1200, Out: 80}},
				{Kind: adapter.EventDone},
			}},
		},
	}
	var forwarded []adapter.Event
	control := t.TempDir()
	if err := supervise(a, "/work", control, "scripted", "task-1", "task-1-implement-1", "original task", func(ev adapter.Event) {
		forwarded = append(forwarded, ev)
	}); err != nil {
		t.Fatal(err)
	}
	if len(a.runSpecs) != 2 {
		t.Fatalf("Run calls = %d, want 2", len(a.runSpecs))
	}
	if len(a.resumeRefs) != 0 {
		t.Fatalf("Resume calls = %d, want 0", len(a.resumeRefs))
	}
	if !strings.Contains(a.runSpecs[1].Prompt, "read-only handoff writer") || !strings.Contains(a.runSpecs[1].Prompt, "original task") {
		t.Fatalf("fallback prompt missing briefing:\n%s", a.runSpecs[1].Prompt)
	}

	doneCount := 0
	for _, ev := range forwarded {
		if ev.Kind == adapter.EventDone {
			doneCount++
		}
	}
	if doneCount != 1 || forwarded[len(forwarded)-1].Kind != adapter.EventDone {
		t.Fatalf("forwarded events have %d done events or non-terminal final event: %+v", doneCount, forwarded)
	}
	if forwarded[len(forwarded)-1].Phase != adapter.PhaseJob {
		t.Fatalf("terminal phase = %q, want %q", forwarded[len(forwarded)-1].Phase, adapter.PhaseJob)
	}
	foundFallbackUsage := false
	for _, ev := range forwarded {
		if ev.Kind == adapter.EventTokenUsage && ev.Phase == adapter.PhaseHandoffFallback {
			foundFallbackUsage = true
		}
	}
	if !foundFallbackUsage {
		t.Fatalf("fallback token usage was not phase-tagged: %+v", forwarded)
	}
	handoffPath, err := snapshot.Path(control, "task-1-implement-1")
	if err != nil {
		t.Fatal(err)
	}
	h, err := snapshot.Load(handoffPath)
	if err != nil {
		t.Fatal(err)
	}
	if h.TaskID != "task-1" || h.JobID != "task-1-implement-1" || h.State != "done" {
		t.Fatalf("handoff = %+v", h)
	}
}

func TestElicitHandoffFallsBackAfterResumeError(t *testing.T) {
	a := &scriptedAdapter{
		capabilities: adapter.Capabilities{Resume: true},
		resumeScripts: []adapterScript{{events: []adapter.Event{
			{Kind: adapter.EventError, Err: "session corrupt"},
			{Kind: adapter.EventDone},
		}}},
		runScripts: []adapterScript{{events: []adapter.Event{
			{Kind: adapter.EventAssistantText, Text: `{"state":"reconstructed","todos":[]}`},
			{Kind: adapter.EventTokenUsage, Usage: &adapter.TokenUsage{In: 900, Out: 60}},
			{Kind: adapter.EventDone},
		}}},
	}
	control := t.TempDir()
	var forwarded []adapter.Event
	if err := elicitHandoff(a, "session-1", "/work", control, "scripted", "task-1", "job-1", "task prompt", func(ev adapter.Event) {
		forwarded = append(forwarded, ev)
	}); err != nil {
		t.Fatal(err)
	}
	if len(a.resumeRefs) != 1 || len(a.runSpecs) != 1 {
		t.Fatalf("resume calls = %d, fallback calls = %d", len(a.resumeRefs), len(a.runSpecs))
	}
	path, err := snapshot.Path(control, "job-1")
	if err != nil {
		t.Fatal(err)
	}
	h, err := snapshot.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if h.State != "reconstructed" {
		t.Fatalf("state = %q", h.State)
	}
	foundResumeWarning := false
	foundFallbackUsage := false
	for _, ev := range forwarded {
		if ev.Kind == adapter.EventWarning && ev.Phase == adapter.PhaseHandoffResume && strings.Contains(ev.Err, "session corrupt") {
			foundResumeWarning = true
		}
		if ev.Kind == adapter.EventTokenUsage && ev.Phase == adapter.PhaseHandoffFallback {
			foundFallbackUsage = true
		}
	}
	if !foundResumeWarning || !foundFallbackUsage {
		t.Fatalf("handoff transcript missing warning or fallback usage: %+v", forwarded)
	}
}

func TestSuperviseSnapshotsFailedRunAndEmitsTerminalErrorLast(t *testing.T) {
	a := &scriptedAdapter{
		capabilities: adapter.Capabilities{Resume: false},
		runScripts: []adapterScript{
			{events: []adapter.Event{
				{Kind: adapter.EventAssistantText, Text: "partial work"},
				{Kind: adapter.EventError, Err: "tests failed"},
				{Kind: adapter.EventDone},
			}},
			{events: []adapter.Event{
				{Kind: adapter.EventAssistantText, Text: `{"state":"failed after partial work","todos":["fix tests"]}`},
				{Kind: adapter.EventDone},
			}},
		},
	}
	var forwarded []adapter.Event
	control := t.TempDir()
	err := supervise(a, "/work", control, "scripted", "task-1", "job-1", "original task", func(ev adapter.Event) {
		forwarded = append(forwarded, ev)
	})
	if err == nil {
		t.Fatal("expected failed main run")
	}
	if len(forwarded) == 0 || forwarded[len(forwarded)-1].Kind != adapter.EventError {
		t.Fatalf("terminal event is not the final error: %+v", forwarded)
	}
	for i, ev := range forwarded[:len(forwarded)-1] {
		if ev.Kind == adapter.EventDone || ev.Kind == adapter.EventError {
			t.Fatalf("premature terminal event at %d: %+v", i, ev)
		}
	}
	path, pathErr := snapshot.Path(control, "job-1")
	if pathErr != nil {
		t.Fatal(pathErr)
	}
	h, loadErr := snapshot.Load(path)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if h.State != "failed after partial work" {
		t.Fatalf("state = %q", h.State)
	}
}
