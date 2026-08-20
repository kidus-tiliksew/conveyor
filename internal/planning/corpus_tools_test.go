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
