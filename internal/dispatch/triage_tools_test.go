package dispatch

import (
	"context"
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
	agent := &sequenceAgent{outputs: []string{
		`{"tool_calls":[{"id":"list","name":"list_requirements","arguments_json":"{}"}]}`,
		`{"tool_calls":[{"id":"read","name":"read_requirement","arguments_json":"{\"requirement_id\":\"req-grounding\"}"}]}`,
		"```conveyor:triage\n{\"class\":\"feature\",\"route\":\"proceed\",\"summary\":\"Grounded.\",\"brief\":{\"questions\":[],\"affected_areas\":[],\"risks\":[]},\"requirement_proposals\":[{\"id\":\"req-grounding\",\"justification\":\"REQ-1 requires confirmed-body reads.\"}],\"system_design_proposals\":[]}\n```",
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
	if len(agent.inputs) != 3 || !strings.Contains(agent.inputs[1].Prompt, requirement.Title) || strings.Contains(agent.inputs[1].Prompt, "Triage reads confirmed bodies.") {
		t.Fatalf("list turn did not expose summary-only data: inputs=%+v", agent.inputs)
	}
	if !strings.Contains(agent.inputs[2].Prompt, "Triage reads confirmed bodies.") {
		t.Fatal("explicit read body was not fed back")
	}
}

type failingCorpusStore struct{ store.Store }

func (s failingCorpusStore) ListRequirements(context.Context) ([]core.Requirement, error) {
	return nil, errors.New("corpus unavailable")
}

type countingCorpusStore struct {
	store.Store
	listCalls int
}

func (s *countingCorpusStore) ListRequirements(ctx context.Context) ([]core.Requirement, error) {
	s.listCalls++
	return s.Store.ListRequirements(ctx)
}

func TestTriageCorpusFailureIsInBandAndFailOpen(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := failingCorpusStore{Store: store.NewMemory()}
	agent := &sequenceAgent{outputs: []string{
		`{"tool_calls":[{"id":"list","name":"list_requirements","arguments_json":"{}"}]}`,
		"```conveyor:triage\n{\"class\":\"chore\",\"route\":\"proceed\",\"summary\":\"Proceed without corpus grounding.\",\"brief\":{\"questions\":[],\"affected_areas\":[],\"risks\":[\"Corpus unavailable.\"]},\"requirement_proposals\":[],\"system_design_proposals\":[]}\n```",
	}}
	result, err := New(st, nil, agent).runTriageLoop(ctx, "test-model", inprocessInput("triage"))
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := pipeline.ParseTriage(result.Output)
	if err != nil || parsed.Route != "proceed" || !strings.Contains(agent.inputs[1].Prompt, "corpus unavailable") {
		t.Fatalf("parsed=%+v err=%v", parsed, err)
	}
}

func TestTriageToolBudgetExhaustionStillProducesCompleteVerdict(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	outputs := make([]string, maxTriageIterations)
	for i := range outputs {
		outputs[i] = `{"tool_calls":[{"id":"again","name":"list_requirements","arguments_json":"{}"}]}`
	}
	agent := &sequenceAgent{outputs: outputs}
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
	calls := make([]string, 10)
	for i := range calls {
		calls[i] = fmt.Sprintf(`{"id":"call-%d","name":"list_requirements","arguments_json":"{}"}`, i)
	}
	agent := &sequenceAgent{outputs: []string{
		`{"tool_calls":[` + strings.Join(calls, ",") + `]}`,
		"```conveyor:triage\n{\"class\":\"chore\",\"route\":\"proceed\",\"summary\":\"Bounded.\",\"brief\":{\"questions\":[],\"affected_areas\":[],\"risks\":[]},\"requirement_proposals\":[],\"system_design_proposals\":[]}\n```",
	}}
	if _, err := New(st, nil, agent).runTriageLoop(ctx, "test-model", inprocessInput("triage")); err != nil {
		t.Fatal(err)
	}
	if st.listCalls != maxTriageToolCalls {
		t.Fatalf("executed calls=%d want=%d", st.listCalls, maxTriageToolCalls)
	}
	if got := strings.Count(agent.inputs[1].Prompt, "tool call budget exhausted"); got != 2 {
		t.Fatalf("budget errors=%d prompt=%s", got, agent.inputs[1].Prompt)
	}
}

func inprocessInput(prompt string) inprocess.Input { return inprocess.Input{Prompt: prompt} }
