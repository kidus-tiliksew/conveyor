package planning

import (
	"reflect"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/corpus"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func TestPlanningDelegatesConfirmedCorpusReadsToSharedExecutor(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	requirement, version, err := st.CreateRequirement(ctx, core.Requirement{ID: "req-planning-corpus", Title: "Shared corpus"}, core.RequirementVersion{
		Content: "# Shared corpus\n\nThe full planning body is explicit-read only.", Origin: core.RequirementOriginOperator,
		Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Planning and triage share corpus reads."}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmRequirementVersion(ctx, requirement.ID, version.Version); err != nil {
		t.Fatal(err)
	}
	service := &Service{Store: st}

	planningList, err := service.executeTool(ctx, core.PlanningSession{}, toolCall{Name: corpus.ListRequirements, ArgumentsJSON: `{}`}, "")
	if err != nil {
		t.Fatal(err)
	}
	sharedList, err := (corpus.Executor{Store: st}).Execute(ctx, corpus.ListRequirements, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(planningList.Output, sharedList) {
		t.Fatalf("planning=%+v shared=%+v", planningList.Output, sharedList)
	}
	if strings.Contains(string(core.JSONPayload(planningList.Output)), "explicit-read only") {
		t.Fatal("planning list leaked body content")
	}

	planningRead, err := service.executeTool(ctx, core.PlanningSession{}, toolCall{Name: corpus.ReadRequirement, ArgumentsJSON: `{"requirement_id":"req-planning-corpus"}`}, "")
	if err != nil || !strings.Contains(string(core.JSONPayload(planningRead.Output)), "explicit-read only") {
		t.Fatalf("read=%+v err=%v", planningRead.Output, err)
	}
	if err = validatePlanningToolArguments(toolCall{Name: corpus.ReadRequirement, ArgumentsJSON: `{"requirement_id":"req-planning-corpus","version":1}`}, DefaultMaxToolBytes); err == nil {
		t.Fatal("planning accepted historical-version corpus arguments")
	}
}

func TestPlanningRejectsArchivedCorpusIDs(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	_, v, err := st.CreateRequirement(ctx, core.Requirement{ID: "req-archived", Title: "Archive"}, core.RequirementVersion{Content: "# Archived", Origin: core.RequirementOriginOperator, Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Exclude archived authority."}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmRequirementVersion(ctx, "req-archived", v.Version); err != nil {
		t.Fatal(err)
	}
	if err = st.ArchiveRequirement(ctx, "req-archived", "operator", nil); err != nil {
		t.Fatal(err)
	}
	_, dv, err := st.CreateSystemDesign(ctx, core.SystemDesign{ID: "design-archived", Title: "Archive", Category: "Architecture"}, core.SystemDesignVersion{Content: "# Archived\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/**\n```", Origin: core.SystemDesignOriginOperator})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmSystemDesignVersion(ctx, "design-archived", dv.Version); err != nil {
		t.Fatal(err)
	}
	if err = st.ArchiveSystemDesign(ctx, "design-archived", "operator", nil); err != nil {
		t.Fatal(err)
	}
	service := &Service{Store: st}
	for _, call := range []toolCall{{Name: corpus.ReadRequirement, ArgumentsJSON: `{"requirement_id":"req-archived"}`}, {Name: corpus.ReadSystemDesign, ArgumentsJSON: `{"document_id":"design-archived"}`}} {
		if _, err = service.executeTool(ctx, core.PlanningSession{}, call, ""); err == nil || !strings.Contains(err.Error(), "is archived") {
			t.Fatalf("planning read %s: %v", call.Name, err)
		}
	}
}
