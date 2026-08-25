package corpus

import (
	"reflect"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func TestFunctionToolsStaySynchronizedAndStrict(t *testing.T) {
	definitions := Tools()
	functions := FunctionTools()
	if len(functions) != len(definitions) {
		t.Fatalf("function tools=%d definitions=%d", len(functions), len(definitions))
	}
	for index, function := range functions {
		definition := definitions[index]
		if function.Type != "function" || !function.Strict || function.Name != definition.Name || function.Description != definition.Description || !reflect.DeepEqual(function.Parameters, definition.Parameters) {
			t.Fatalf("function tool %d=%+v definition=%+v", index, function, definition)
		}
		if !IsTool(function.Name) {
			t.Fatalf("unregistered function tool %q", function.Name)
		}
	}
}

func TestToolsAreReadOnlyAndListsAreConfirmedSummaries(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	confirmed, version, err := st.CreateRequirement(ctx, core.Requirement{ID: "req-confirmed", Title: "Confirmed", Slug: "confirmed"}, core.RequirementVersion{
		Content: "# Confirmed body\n\nSecret detail that belongs only in an explicit read.", Origin: core.RequirementOriginOperator,
		Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Expose summaries first."}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmRequirementVersion(ctx, confirmed.ID, version.Version); err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.CreateRequirement(ctx, core.Requirement{ID: "req-pending", Title: "Pending"}, core.RequirementVersion{
		Content: "Pending body", Origin: core.RequirementOriginOperator, Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Remain pending."}},
	}); err != nil {
		t.Fatal(err)
	}

	for _, tool := range Tools() {
		if strings.Contains(tool.Name, "draft") || strings.Contains(tool.Name, "write") || strings.Contains(tool.Name, "repo") {
			t.Fatalf("write or repository tool leaked: %+v", tool)
		}
	}
	output, err := (Executor{Store: st}).Execute(ctx, ListRequirements, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	items := output.([]RequirementSummary)
	if len(items) != 1 || items[0].ID != confirmed.ID || strings.Contains(items[0].Summary, "Secret detail") {
		t.Fatalf("summaries=%+v", items)
	}
	encoded := string(core.JSONPayload(items))
	if strings.Contains(encoded, "content") || strings.Contains(encoded, "statements") || strings.Contains(encoded, "Pending body") {
		t.Fatalf("list leaked body/version fields: %s", encoded)
	}
	read, err := (Executor{Store: st}).Execute(ctx, ReadRequirement, `{"requirement_id":"req-confirmed"}`)
	if err != nil || !strings.Contains(string(core.JSONPayload(read)), "Secret detail") {
		t.Fatalf("read=%+v err=%v", read, err)
	}
	if _, err = (Executor{Store: st}).Execute(ctx, ReadRequirement, `{"requirement_id":"req-pending"}`); err == nil {
		t.Fatal("pending requirement body was readable")
	}
}

func TestSystemDesignAndDecisionListsExcludeUnconfirmedAuthority(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	design, version, err := st.CreateSystemDesign(ctx, core.SystemDesign{ID: "design-confirmed", Title: "Confirmed design", Category: "Architecture", Slug: "confirmed-design"}, core.SystemDesignVersion{
		Content: "# Confirmed design\n\nBody only on read.\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/**\n```", Origin: core.SystemDesignOriginOperator,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmSystemDesignVersion(ctx, design.ID, version.Version); err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.CreateSystemDesign(ctx, core.SystemDesign{ID: "design-pending", Title: "Pending", Category: "Architecture"}, core.SystemDesignVersion{
		Content: "# Pending\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - cmd/**\n```", Origin: core.SystemDesignOriginOperator,
	}); err != nil {
		t.Fatal(err)
	}
	confirmedDecision, err := st.ProposeDecision(ctx, core.Decision{Statement: "Use confirmed corpus tools.", Context: "Triage grounding.", AlternativesRejected: "Blind title matching.", Origin: core.DecisionOriginOperator})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.ConfirmDecision(ctx, confirmedDecision.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = st.ProposeDecision(ctx, core.Decision{Statement: "Pending decision is hidden.", Context: "No authority.", AlternativesRejected: "None.", Origin: core.DecisionOriginOperator}); err != nil {
		t.Fatal(err)
	}

	designs, err := (Executor{Store: st}).Execute(ctx, ListSystemDesigns, `{}`)
	if err != nil || len(designs.([]SystemDesignSummary)) != 1 {
		t.Fatalf("designs=%+v err=%v", designs, err)
	}
	decisions, err := (Executor{Store: st}).Execute(ctx, ListDecisions, `{}`)
	if err != nil || len(decisions.([]DecisionSummary)) != 1 || decisions.([]DecisionSummary)[0].ID != confirmedDecision.ID {
		t.Fatalf("decisions=%+v err=%v", decisions, err)
	}
	read, err := (Executor{Store: st}).Execute(ctx, ReadSystemDesign, `{"document_id":"design-confirmed"}`)
	if err != nil || !strings.Contains(string(core.JSONPayload(read)), "Body only on read") {
		t.Fatalf("read=%+v err=%v", read, err)
	}
}
