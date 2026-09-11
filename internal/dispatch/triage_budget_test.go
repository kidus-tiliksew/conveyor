package dispatch

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/inprocess"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func TestTriageBudgetRetainsIntentAndProvenanceWhileOmittingLargeAdjacentLog(t *testing.T) {
	input := inprocess.Input{Prompt: "exact task intent\nREQ-1 confirmed governing instruction\nsource event 17; sibling path\n", Attachments: []inprocess.Attachment{
		{ID: "log", Name: "ci.log", Kind: inprocess.AttachmentDocument, ContentType: "text/plain", Content: bytes.Repeat([]byte("x"), 4864716)},
		{ID: "local", Name: "intent.md", Kind: inprocess.AttachmentDocument, Content: []byte("local instructions")},
		{ID: "small", Name: "outcome.md", Kind: inprocess.AttachmentDocument, Content: []byte("relevant outcome")},
	}}
	input.Prompt += "Context artifact supplied as document input: ci.log (text/plain, 4864716 bytes, id log)\n"
	artifacts := []core.Artifact{{ID: "log", TaskID: "sibling"}, {ID: "local", TaskID: "task"}, {ID: "small", TaskID: "sibling"}}
	got, err := boundTriageInput(input, artifacts, "task")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Attachments) != 2 || got.Attachments[0].ID != "local" || got.Attachments[1].ID != "small" {
		t.Fatalf("attachments=%v", got.Attachments)
	}
	for _, want := range []string{"exact task intent", "REQ-1 confirmed governing instruction", "source event 17; sibling path", "body omitted by triage byte budget: ci.log", "id log"} {
		if !strings.Contains(got.Prompt, want) {
			t.Fatalf("missing %q", want)
		}
	}
	if triageInputBytes(got) > maxTriageInitialBytes {
		t.Fatal("oversized input")
	}
	again, err := boundTriageInput(input, artifacts, "task")
	if err != nil || again.Prompt != got.Prompt {
		t.Fatal("selection not deterministic")
	}
}

func TestTriageBudgetRefusesMandatoryInputWithoutTruncation(t *testing.T) {
	for _, attachment := range []bool{false, true} {
		input := inprocess.Input{Prompt: "mandatory task"}
		if attachment {
			input.Attachments = []inprocess.Attachment{{ID: "local", Content: bytes.Repeat([]byte("x"), maxTriageInitialBytes)}}
		} else {
			input.Prompt = strings.Repeat("authority", maxTriageInitialBytes)
		}
		_, err := boundTriageInput(input, []core.Artifact{{ID: "local", TaskID: "task"}}, "task")
		if err == nil || !strings.Contains(err.Error(), "were not truncated") {
			t.Fatalf("err=%v", err)
		}
	}
}

func TestTriageOversizedCorpusBodyIsExplicitlyUnread(t *testing.T) {
	content := strings.Repeat("authority must not be partially cited", maxTriageToolResultBytes)
	for _, got := range []any{boundedTriageToolOutput(map[string]any{"content": content}), boundedTriagePromptJSON([]byte(content))} {
		raw, _ := json.Marshal(got)
		if !strings.Contains(string(raw), "body not supplied or read") || strings.Contains(string(raw), "preview") || len(raw) > 1024 {
			t.Fatalf("invalid refusal %s", raw)
		}
	}
}

func TestTriageContinuationBudgetReturnsVerdictWithoutSendingOversizedHistory(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "demo")
	first := nativeCallResult("list", "list_requirements", "{}", "")
	item, _ := json.Marshal(map[string]any{"type": "reasoning", "content": strings.Repeat("r", maxTriageInputBytes)})
	first.ResponseItems = append(first.ResponseItems, item)
	agent := &sequenceAgent{results: []inprocess.Result{first}}
	result, err := New(store.NewMemory(), nil, agent).runTriageLoop(ctx, "model", inprocess.Input{Prompt: "intent"})
	if err != nil || len(agent.inputs) != 1 || !strings.Contains(result.Output, `"route":"proceed"`) || !strings.Contains(result.Output, "input byte allowance was exhausted") || !strings.Contains(string(result.Transcript), "provider_call_skipped") {
		t.Fatalf("calls=%d err=%v output=%s", len(agent.inputs), err, result.Output)
	}
}

func TestTriageCorpusReadsRespectCumulativeAllowance(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	req, version, err := st.CreateRequirement(ctx, core.Requirement{ID: "req-large", Title: "Large confirmed body"}, core.RequirementVersion{Content: "# Large confirmed body\n\n" + strings.Repeat("governing text ", 3200), Origin: core.RequirementOriginOperator, Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Keep authority whole."}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmRequirementVersion(ctx, req.ID, version.Version); err != nil {
		t.Fatal(err)
	}
	first := inprocess.Result{}
	for _, id := range []string{"one", "two", "three", "four"} {
		call := nativeCallResult(id, "read_requirement", `{"requirement_id":"req-large"}`, "")
		first.FunctionCalls = append(first.FunctionCalls, call.FunctionCalls...)
		first.ResponseItems = append(first.ResponseItems, call.ResponseItems...)
	}
	agent := &sequenceAgent{results: []inprocess.Result{first, nativeMessageResult("final")}}
	_, err = New(st, nil, agent).runTriageLoop(ctx, "model", inprocess.Input{Prompt: strings.Repeat("p", maxTriageInitialBytes-4096)})
	if err != nil || len(agent.inputs) != 2 {
		t.Fatalf("err=%v calls=%d", err, len(agent.inputs))
	}
	next := agent.inputs[1]
	if triageInputBytes(next) > maxTriageInputBytes {
		t.Fatal("oversized continuation sent")
	}
	outputs := next.Continuation.FunctionCallOutputs
	if len(outputs) != 4 || !strings.Contains(outputs[0].Output, "governing text") || !strings.Contains(outputs[3].Output, "body not supplied or read") || strings.Contains(outputs[3].Output, "governing text") {
		t.Fatalf("tool results did not retain whole bodies and explicit refusals")
	}
}
