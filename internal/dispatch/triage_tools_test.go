package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/inprocess"
	"github.com/kidus-tiliksew/conveyor/internal/pipeline"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func confirmedTriageRequirement(t *testing.T, st store.Store) core.Requirement {
	t.Helper()
	ctx := store.WithWorkspace(t.Context(), "demo")
	requirement, version, err := st.CreateRequirement(ctx, core.Requirement{ID: "req-grounding", Title: "Grounding"}, core.RequirementVersion{
		Content: "# Grounding\n\nTriage reads confirmed bodies.", Origin: core.RequirementOriginOperator,
		Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Triage reads confirmed bodies."}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmRequirementVersion(ctx, requirement.ID, version.Version); err != nil {
		t.Fatal(err)
	}
	return requirement
}

func TestTriageToolLoopReadsCorpusThenReturnsVerdict(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	requirement := confirmedTriageRequirement(t, st)
	agent := &sequenceAgent{results: []inprocess.Result{
		nativeCallResult("list", "list_requirements", "{}", ""),
		nativeCallResult("read", "read_requirement", `{"requirement_id":"req-grounding"}`, ""),
		nativeMessageResult("```conveyor:triage\n{\"class\":\"feature\",\"route\":\"proceed\",\"summary\":\"Grounded.\",\"brief\":{\"questions\":[],\"affected_areas\":[],\"risks\":[]},\"requirement_proposals\":[{\"id\":\"req-grounding\",\"justification\":\"REQ-1 requires confirmed-body reads.\"}],\"system_design_proposals\":[]}\n```"),
	}}
	d := New(st, nil, agent)
	result, err := d.runTriageLoop(ctx, "test-model", inprocessInput("triage"))
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := pipeline.ParseTriage(result.Output)
	if err != nil || len(parsed.RequirementProposals) != 1 || parsed.RequirementProposals[0].ID != requirement.ID {
		t.Fatalf("parsed=%+v err=%v", parsed, err)
	}
	if len(agent.inputs) != 3 || len(agent.inputs[1].Continuation.FunctionCallOutputs) != 1 || !strings.Contains(agent.inputs[1].Continuation.FunctionCallOutputs[0].Output, requirement.Title) || strings.Contains(agent.inputs[1].Continuation.FunctionCallOutputs[0].Output, "Triage reads confirmed bodies.") {
		t.Fatalf("list turn did not expose summary-only data: continuation=%+v", agent.inputs[1].Continuation)
	}
	if !strings.Contains(agent.inputs[2].Continuation.FunctionCallOutputs[0].Output, "Triage reads confirmed bodies.") {
		t.Fatal("explicit read body was not fed back")
	}
}

func TestTriageMixedFunctionCallAndVerdictExecutesBeforeFinalizing(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := &countingCorpusStore{Store: store.NewMemory()}
	premature := "```conveyor:triage\n{\"class\":\"bug\",\"route\":\"proceed\",\"summary\":\"Premature.\",\"brief\":{\"questions\":[],\"affected_areas\":[],\"risks\":[]},\"requirement_proposals\":[],\"system_design_proposals\":[]}\n```"
	final := strings.Replace(premature, "Premature.", "Final after grounding.", 1)
	agent := &sequenceAgent{results: []inprocess.Result{
		nativeCallResult("list", "list_requirements", "{}", premature),
		nativeMessageResult(final),
	}}
	result, err := New(st, nil, agent).runTriageLoop(ctx, "test-model", inprocessInput("triage"))
	if err != nil || result.Output != final || result.ToolCallsExecuted != 1 || st.listCalls != 1 || len(agent.inputs) != 2 {
		t.Fatalf("output=%q executed=%d store_calls=%d inputs=%d err=%v", result.Output, result.ToolCallsExecuted, st.listCalls, len(agent.inputs), err)
	}
}

type failingCorpusStore struct{ store.Store }

func (s failingCorpusStore) ListRequirements(context.Context, bool) ([]core.Requirement, error) {
	return nil, errors.New("corpus unavailable")
}

type countingCorpusStore struct {
	store.Store
	listCalls int
}

func (s *countingCorpusStore) ListRequirements(ctx context.Context, includeArchived bool) ([]core.Requirement, error) {
	s.listCalls++
	return s.Store.ListRequirements(ctx, includeArchived)
}

func TestTriageCorpusFailureIsInBandAndFailOpen(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := failingCorpusStore{Store: store.NewMemory()}
	agent := &sequenceAgent{results: []inprocess.Result{
		nativeCallResult("list", "list_requirements", "{}", ""),
		nativeMessageResult("```conveyor:triage\n{\"class\":\"chore\",\"route\":\"proceed\",\"summary\":\"Proceed without corpus grounding.\",\"brief\":{\"questions\":[],\"affected_areas\":[],\"risks\":[\"Corpus unavailable.\"]},\"requirement_proposals\":[],\"system_design_proposals\":[]}\n```"),
	}}
	result, err := New(st, nil, agent).runTriageLoop(ctx, "test-model", inprocessInput("triage"))
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := pipeline.ParseTriage(result.Output)
	if err != nil || parsed.Route != "proceed" || !strings.Contains(agent.inputs[1].Continuation.FunctionCallOutputs[0].Output, "corpus unavailable") {
		t.Fatalf("parsed=%+v err=%v", parsed, err)
	}
}

func TestTriageToolBudgetExhaustionStillProducesCompleteVerdict(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	results := make([]inprocess.Result, maxTriageIterations)
	for i := range results {
		results[i] = nativeCallResult(fmt.Sprintf("again-%d", i), "list_requirements", "{}", "")
	}
	agent := &sequenceAgent{results: results}
	result, err := New(st, nil, agent).runTriageLoop(ctx, "test-model", inprocessInput("triage"))
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := pipeline.ParseTriage(result.Output)
	if err != nil || parsed.Route != "proceed" || len(agent.inputs) != maxTriageIterations {
		t.Fatalf("parsed=%+v calls=%d err=%v", parsed, len(agent.inputs), err)
	}
	if !strings.Contains(agent.inputs[len(agent.inputs)-1].Prompt, "Tool loop closed") {
		t.Fatal("final iteration did not close tools")
	}
}

func TestTriageToolCallBudgetDefersExcessCalls(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := &countingCorpusStore{Store: store.NewMemory()}
	calls := make([]inprocess.FunctionCall, 10)
	for i := range calls {
		calls[i] = inprocess.FunctionCall{CallID: fmt.Sprintf("call-%d", i), Name: "list_requirements", ArgumentsJSON: "{}"}
	}
	agent := &sequenceAgent{results: []inprocess.Result{
		{FunctionCalls: calls, ResponseItems: []json.RawMessage{json.RawMessage(`{"type":"reasoning","encrypted_content":"opaque"}`)}},
		nativeMessageResult("```conveyor:triage\n{\"class\":\"chore\",\"route\":\"proceed\",\"summary\":\"Bounded.\",\"brief\":{\"questions\":[],\"affected_areas\":[],\"risks\":[]},\"requirement_proposals\":[],\"system_design_proposals\":[]}\n```"),
	}}
	if _, err := New(st, nil, agent).runTriageLoop(ctx, "test-model", inprocessInput("triage")); err != nil {
		t.Fatal(err)
	}
	if st.listCalls != maxTriageToolCalls {
		t.Fatalf("executed calls=%d want=%d", st.listCalls, maxTriageToolCalls)
	}
	encoded, _ := json.Marshal(agent.inputs[1].Continuation.FunctionCallOutputs)
	if got := strings.Count(string(encoded), "tool call budget exhausted"); got != 2 {
		t.Fatalf("budget errors=%d outputs=%s", got, encoded)
	}
}

func nativeCallResult(id, name, arguments, output string) inprocess.Result {
	items := []json.RawMessage{json.RawMessage(`{"type":"reasoning","encrypted_content":"opaque-reasoning"}`)}
	call, _ := json.Marshal(map[string]any{"type": "function_call", "call_id": id, "name": name, "arguments": arguments})
	items = append(items, call)
	if output != "" {
		message, _ := json.Marshal(map[string]any{"type": "message", "content": []map[string]any{{"type": "output_text", "text": output}}})
		items = append(items, message)
	}
	return inprocess.Result{Output: output, FunctionCalls: []inprocess.FunctionCall{{CallID: id, Name: name, ArgumentsJSON: arguments}}, ResponseItems: items, TokensIn: 20, TokensOut: 10}
}

func nativeMessageResult(output string) inprocess.Result {
	message, _ := json.Marshal(map[string]any{"type": "message", "content": []map[string]any{{"type": "output_text", "text": output}}})
	return inprocess.Result{Output: output, ResponseItems: []json.RawMessage{message}, TokensIn: 20, TokensOut: 10}
}

func inprocessInput(prompt string) inprocess.Input { return inprocess.Input{Prompt: prompt} }
