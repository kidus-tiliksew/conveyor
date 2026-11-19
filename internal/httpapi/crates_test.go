package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/crateinsight"
	"github.com/kidus-tiliksew/conveyor/internal/nextaction"
	"github.com/kidus-tiliksew/conveyor/internal/pack"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"github.com/kidus-tiliksew/conveyor/internal/workorder"
)

// crateFixture drives the crate routes through the full REST handler over the
// memory store (component-crates Verification: routes, refusals,
// capabilities, intake, and the task filter).
type crateFixture struct {
	*sessionIntakeFixture
	ctx      context.Context
	betaCtx  context.Context
	editor   string
	viewer   string
	nonMaker string
	foreign  string
}

func newCrateFixture(t *testing.T) *crateFixture {
	t.Helper()
	base := newSessionIntakeFixture(t)
	return &crateFixture{
		sessionIntakeFixture: base,
		ctx:                  store.WithActor(store.WithWorkspace(t.Context(), "alpha"), store.SystemActor()),
		betaCtx:              store.WithActor(store.WithWorkspace(t.Context(), "beta"), store.SystemActor()),
		editor:               humanIntakeToken(core.WorkspaceRoleMaintainer),
		viewer:               humanIntakeToken(core.WorkspaceRoleViewer),
		nonMaker:             humanIntakeToken(core.WorkspaceRoleContributor),
		foreign:              "pat-foreign",
	}
}

func (f *crateFixture) do(t *testing.T, token, method, path string, body any) (int, []byte) {
	t.Helper()
	var reader *bytes.Reader
	switch value := body.(type) {
	case nil:
		reader = bytes.NewReader(nil)
	case string:
		reader = bytes.NewReader([]byte(value))
	default:
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}
	separator := "?"
	if strings.Contains(path, "?") {
		separator = "&"
	}
	workspace := "alpha"
	if token == f.foreign {
		workspace = "beta"
	}
	request := httptest.NewRequest(method, path+separator+"workspace_id="+workspace, reader)
	request.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response := httptest.NewRecorder()
	f.handler.ServeHTTP(response, request)
	return response.Code, response.Body.Bytes()
}

func (f *crateFixture) createCrate(t *testing.T, token string, body map[string]any) store.Crate {
	t.Helper()
	status, raw := f.do(t, token, http.MethodPost, "/v1/crates", body)
	if status != http.StatusCreated {
		t.Fatalf("create crate status=%d body=%s", status, raw)
	}
	var crate store.Crate
	if err := json.Unmarshal(raw, &crate); err != nil {
		t.Fatal(err)
	}
	return crate
}

func (f *crateFixture) requirement(t *testing.T, ctx context.Context, confirm bool) string {
	t.Helper()
	id := "req-crate-" + core.NewTaskID()
	version := core.RequirementVersion{RequirementID: id, Content: "# " + id + "\n\n```conveyor:requirements\n- id: REQ-1\n  statement: Track crate work.\n```", Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Track crate work."}}, Origin: core.RequirementOriginOperator}
	if _, _, err := f.store.CreateRequirement(ctx, core.Requirement{ID: id, Title: "Title of " + id}, version); err != nil {
		t.Fatal(err)
	}
	if confirm {
		if _, _, err := f.store.ConfirmRequirementVersion(ctx, id, 1); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

func (f *crateFixture) task(t *testing.T, crateID string) core.Task {
	t.Helper()
	task := core.Task{ID: core.NewTaskID(), Workspace: "alpha", Repo: "api", Title: "Crate task", Body: "Crate task.", State: core.TaskQueued, NextStage: core.StageImplement, CreatedAt: time.Now().UTC(), PlanState: core.PlanNotRequired}
	task.Branch = "conveyor/task-" + task.ID
	if crateID != "" {
		task.Crate = &core.TaskCrate{ID: crateID}
	}
	if err := f.store.CreateTask(f.ctx, task); err != nil {
		t.Fatal(err)
	}
	return task
}

// recorded is the durable state a refused request must leave unchanged: the
// task list, every crate with its declared requirements, and every event.
func (f *crateFixture) recorded(t *testing.T) string {
	t.Helper()
	tasks, err := f.store.ListTasks(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, len(tasks))
	for i, task := range tasks {
		ids[i] = task.ID + ":" + task.TaskCrateID()
	}
	slices.Sort(ids)
	crates, err := f.store.(store.CrateStore).ListCrates(f.ctx, store.CrateListQuery{})
	if err != nil {
		t.Fatal(err)
	}
	events, err := f.store.ListEvents(f.ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	taskEvents := 0
	for _, task := range tasks {
		listed, err := f.store.ListEvents(f.ctx, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		taskEvents += len(listed)
	}
	declared := make(map[string][]string, len(crates))
	for _, crate := range crates {
		declared[crate.ID] = crate.RequirementIDs
	}
	raw, _ := json.Marshal(map[string]any{"tasks": ids, "crates": crates, "declared": declared, "workspace_events": len(events), "task_events": taskEvents})
	return string(raw)
}

func crateErrorCode(t *testing.T, raw []byte) map[string]string {
	t.Helper()
	var payload map[string]string
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("refusal body %s: %v", raw, err)
	}
	return payload
}

func TestCrateRoutesCreateReadEditAndList(t *testing.T) {
	f := newCrateFixture(t)
	requirement := f.requirement(t, f.ctx, true)
	crate := f.createCrate(t, f.editor, map[string]any{"name": " Launch ", "target_date": "2027-04-01", "requirement_ids": []string{requirement}})
	if crate.Name != "Launch" || crate.State != store.CrateOpen || crate.TargetDate == nil || *crate.TargetDate != "2027-04-01" || crate.CreatedBy != "user:"+intakeOwner(core.WorkspaceRoleMaintainer) {
		t.Fatalf("created crate = %+v", crate)
	}
	status, raw := f.do(t, f.viewer, http.MethodGet, "/v1/crates/"+crate.ID, nil)
	if status != http.StatusOK {
		t.Fatalf("viewer read status=%d body=%s", status, raw)
	}
	var detail map[string]json.RawMessage
	if err := json.Unmarshal(raw, &detail); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"id", "name", "target_date", "state", "shipped", "created_by", "created_at", "closed_at", "open_task_count", "merged_task_count", "total_task_count", "requirements", "undeclared_requirements", "tasks"} {
		if _, ok := detail[field]; !ok {
			t.Fatalf("crate read lacks %s: %s", field, raw)
		}
	}
	var requirements []store.CrateRequirementCoverage
	_ = json.Unmarshal(detail["requirements"], &requirements)
	if len(requirements) != 1 || requirements[0].ID != requirement || requirements[0].Title == "" || requirements[0].TaskCount != 0 {
		t.Fatalf("declared requirements = %+v", requirements)
	}

	status, raw = f.do(t, f.editor, http.MethodPatch, "/v1/crates/"+crate.ID, `{"name":"Launch two","target_date":null}`)
	if status != http.StatusOK {
		t.Fatalf("patch status=%d body=%s", status, raw)
	}
	var edited store.Crate
	_ = json.Unmarshal(raw, &edited)
	if edited.Name != "Launch two" || edited.TargetDate != nil {
		t.Fatalf("edited crate = %+v", edited)
	}
	// An omitted target_date leaves the date as it is.
	if status, raw = f.do(t, f.editor, http.MethodPatch, "/v1/crates/"+crate.ID, `{"target_date":"2027-05-01"}`); status != http.StatusOK {
		t.Fatalf("set date status=%d body=%s", status, raw)
	}
	if status, raw = f.do(t, f.editor, http.MethodPatch, "/v1/crates/"+crate.ID, `{"name":"Launch three"}`); status != http.StatusOK || !strings.Contains(string(raw), `"target_date":"2027-05-01"`) {
		t.Fatalf("omitted date status=%d body=%s", status, raw)
	}
	other := f.createCrate(t, f.editor, map[string]any{"name": "Archive"})
	if status, raw = f.do(t, f.editor, http.MethodPatch, "/v1/crates/"+other.ID, `{"state":"closed"}`); status != http.StatusOK || !strings.Contains(string(raw), `"state":"closed"`) {
		t.Fatalf("close status=%d body=%s", status, raw)
	}
	list := func(query string) []string {
		t.Helper()
		status, raw := f.do(t, f.viewer, http.MethodGet, "/v1/crates"+query, nil)
		if status != http.StatusOK {
			t.Fatalf("list %s status=%d body=%s", query, status, raw)
		}
		var rows []store.Crate
		_ = json.Unmarshal(raw, &rows)
		ids := make([]string, len(rows))
		for i, row := range rows {
			ids[i] = row.ID
		}
		return ids
	}
	if got := list(""); !slices.Equal(got, []string{crate.ID}) {
		t.Fatalf("default list = %v, want only the open crate", got)
	}
	if got := list("?state=closed"); !slices.Equal(got, []string{other.ID}) {
		t.Fatalf("closed list = %v", got)
	}
	if got := list("?state=all"); len(got) != 2 {
		t.Fatalf("all list = %v", got)
	}
	if status, raw = f.do(t, f.editor, http.MethodPatch, "/v1/crates/"+other.ID, `{"state":"open"}`); status != http.StatusOK || !strings.Contains(string(raw), `"state":"open"`) {
		t.Fatalf("reopen status=%d body=%s", status, raw)
	}
}

func TestCrateRouteRefusalsRecordNothing(t *testing.T) {
	f := newCrateFixture(t)
	unconfirmed := f.requirement(t, f.ctx, false)
	archived := f.requirement(t, f.ctx, true)
	if err := f.store.ArchiveRequirement(f.ctx, archived, "operator", nil); err != nil {
		t.Fatal(err)
	}
	foreignRequirement := f.requirement(t, f.betaCtx, true)
	crate := f.createCrate(t, f.editor, map[string]any{"name": "Open"})
	closed := f.createCrate(t, f.editor, map[string]any{"name": "Closed"})
	if status, raw := f.do(t, f.editor, http.MethodPatch, "/v1/crates/"+closed.ID, `{"state":"closed"}`); status != http.StatusOK {
		t.Fatalf("close status=%d body=%s", status, raw)
	}
	foreignCrate := f.createCrate(t, f.foreign, map[string]any{"name": "Beta crate"})
	member := f.task(t, crate.ID)
	before := f.recorded(t)
	cases := []struct {
		name, method, path string
		body               any
		status             int
		code, key, value   string
	}{
		{"empty name", http.MethodPost, "/v1/crates", `{"name":"  "}`, 400, "invalid_crate", "field", "name"},
		{"missing name", http.MethodPost, "/v1/crates", `{}`, 400, "invalid_crate", "field", "name"},
		{"long name", http.MethodPost, "/v1/crates", map[string]any{"name": strings.Repeat("x", 121)}, 400, "invalid_crate", "field", "name"},
		{"name type", http.MethodPost, "/v1/crates", `{"name":7}`, 400, "invalid_crate", "field", "name"},
		{"bad date", http.MethodPost, "/v1/crates", `{"name":"Dated","target_date":"2027-13-01"}`, 400, "invalid_crate", "field", "target_date"},
		{"date type", http.MethodPost, "/v1/crates", `{"name":"Dated","target_date":20270101}`, 400, "invalid_crate", "field", "target_date"},
		{"requirement list type", http.MethodPost, "/v1/crates", `{"name":"Needs","requirement_ids":"req-x"}`, 400, "invalid_crate", "field", "requirement_ids"},
		{"missing requirement", http.MethodPost, "/v1/crates", map[string]any{"name": "Needs", "requirement_ids": []string{"req-missing"}}, 400, "invalid_context_reference", "requirement", "req-missing"},
		{"unconfirmed requirement", http.MethodPost, "/v1/crates", map[string]any{"name": "Needs", "requirement_ids": []string{unconfirmed}}, 400, "invalid_context_reference", "requirement", unconfirmed},
		{"archived requirement", http.MethodPost, "/v1/crates", map[string]any{"name": "Needs", "requirement_ids": []string{archived}}, 400, "invalid_context_reference", "requirement", archived},
		{"other workspace requirement", http.MethodPost, "/v1/crates", map[string]any{"name": "Needs", "requirement_ids": []string{foreignRequirement}}, 400, "invalid_context_reference", "requirement", foreignRequirement},
		{"edit archived requirement", http.MethodPatch, "/v1/crates/" + crate.ID, map[string]any{"requirement_ids": []string{archived}}, 400, "invalid_context_reference", "requirement", archived},
		{"edit bad state", http.MethodPatch, "/v1/crates/" + crate.ID, `{"state":"shipped"}`, 400, "invalid_crate", "field", "state"},
		{"edit null name", http.MethodPatch, "/v1/crates/" + crate.ID, `{"name":null}`, 400, "invalid_crate", "field", "name"},
		{"list bad state", http.MethodGet, "/v1/crates?state=done", nil, 400, "invalid_crate", "field", "state"},
		{"activity bad limit", http.MethodGet, "/v1/crates/" + crate.ID + "/activity?limit=0", nil, 400, "invalid_crate", "field", "limit"},
		{"missing crate read", http.MethodGet, "/v1/crates/crate-000000-000000", nil, 404, "crate_not_found", "crate_id", "crate-000000-000000"},
		{"other workspace read", http.MethodGet, "/v1/crates/" + foreignCrate.ID, nil, 404, "crate_not_found", "crate_id", foreignCrate.ID},
		{"other workspace edit", http.MethodPatch, "/v1/crates/" + foreignCrate.ID, `{"name":"Taken"}`, 404, "crate_not_found", "crate_id", foreignCrate.ID},
		{"other workspace activity", http.MethodGet, "/v1/crates/" + foreignCrate.ID + "/activity", nil, 404, "crate_not_found", "crate_id", foreignCrate.ID},
		{"join other workspace crate", http.MethodPut, "/v1/tasks/" + member.ID + "/crate", map[string]any{"crate_id": foreignCrate.ID}, 404, "crate_not_found", "crate_id", foreignCrate.ID},
		{"join missing crate", http.MethodPut, "/v1/tasks/" + member.ID + "/crate", map[string]any{"crate_id": "crate-000000-000000"}, 404, "crate_not_found", "crate_id", "crate-000000-000000"},
		{"join closed crate", http.MethodPut, "/v1/tasks/" + member.ID + "/crate", map[string]any{"crate_id": closed.ID}, 409, "crate_closed", "crate_id", closed.ID},
		{"join without crate", http.MethodPut, "/v1/tasks/" + member.ID + "/crate", `{}`, 400, "invalid_crate", "field", "crate_id"},
		{"join missing task", http.MethodPut, "/v1/tasks/missing-task/crate", map[string]any{"crate_id": crate.ID}, 404, "task_not_found", "", ""},
	}
	for _, tc := range cases {
		status, raw := f.do(t, f.editor, tc.method, tc.path, tc.body)
		if status != tc.status {
			t.Fatalf("%s: status=%d body=%s, want %d", tc.name, status, raw, tc.status)
		}
		payload := crateErrorCode(t, raw)
		if payload["error"] != tc.code || (tc.key != "" && payload[tc.key] != tc.value) || payload["message"] == "" {
			t.Fatalf("%s: refusal = %+v, want %s naming %s %s", tc.name, payload, tc.code, tc.key, tc.value)
		}
	}
	if after := f.recorded(t); after != before {
		t.Fatalf("refusals recorded state:\nbefore %s\nafter  %s", before, after)
	}
}

func TestCrateRoutesRequireCreateTasks(t *testing.T) {
	f := newCrateFixture(t)
	crate := f.createCrate(t, f.editor, map[string]any{"name": "Guarded"})
	task := f.task(t, "")
	before := f.recorded(t)
	for _, token := range []string{f.viewer, f.nonMaker} {
		for _, request := range []struct {
			method, path string
			body         any
		}{
			{http.MethodPost, "/v1/crates", map[string]any{"name": "Refused"}},
			{http.MethodPatch, "/v1/crates/" + crate.ID, map[string]any{"name": "Refused"}},
			{http.MethodPut, "/v1/tasks/" + task.ID + "/crate", map[string]any{"crate_id": crate.ID}},
			{http.MethodDelete, "/v1/tasks/" + task.ID + "/crate", nil},
		} {
			if status, raw := f.do(t, token, request.method, request.path, request.body); status != http.StatusNotFound {
				t.Fatalf("%s %s %s status=%d body=%s, want the workspace not-found refusal", token, request.method, request.path, status, raw)
			}
		}
		for _, path := range []string{"/v1/crates", "/v1/crates/" + crate.ID, "/v1/crates/" + crate.ID + "/activity"} {
			if status, raw := f.do(t, token, http.MethodGet, path, nil); status != http.StatusOK {
				t.Fatalf("%s GET %s status=%d body=%s", token, path, status, raw)
			}
		}
	}
	if status, raw := f.do(t, "pat-unbound", http.MethodGet, "/v1/crates", nil); status != http.StatusNotFound {
		t.Fatalf("unbound read status=%d body=%s", status, raw)
	}
	if after := f.recorded(t); after != before {
		t.Fatalf("refused mutations recorded state:\nbefore %s\nafter  %s", before, after)
	}
}

func TestCrateMembershipRoutes(t *testing.T) {
	f := newCrateFixture(t)
	first := f.createCrate(t, f.editor, map[string]any{"name": "First"})
	second := f.createCrate(t, f.editor, map[string]any{"name": "Second"})
	task := f.task(t, "")
	status, raw := f.do(t, f.editor, http.MethodPut, "/v1/tasks/"+task.ID+"/crate", map[string]any{"crate_id": first.ID})
	if status != http.StatusOK || !strings.Contains(string(raw), `"crate":{"id":"`+first.ID+`","name":"First"}`) {
		t.Fatalf("join status=%d body=%s", status, raw)
	}
	if status, raw = f.do(t, f.editor, http.MethodPut, "/v1/tasks/"+task.ID+"/crate", map[string]any{"crate_id": second.ID}); status != http.StatusOK || !strings.Contains(string(raw), `"name":"Second"`) {
		t.Fatalf("move status=%d body=%s", status, raw)
	}
	if status, raw = f.do(t, f.editor, http.MethodPatch, "/v1/crates/"+second.ID, `{"state":"closed"}`); status != http.StatusOK {
		t.Fatalf("close status=%d body=%s", status, raw)
	}
	// A task may leave a closed crate.
	if status, raw = f.do(t, f.editor, http.MethodDelete, "/v1/tasks/"+task.ID+"/crate", nil); status != http.StatusOK || strings.Contains(string(raw), `"crate":`) {
		t.Fatalf("leave status=%d body=%s", status, raw)
	}
	events, err := f.store.ListEvents(f.ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	var moves []store.TaskCrateMovePayload
	for _, event := range events {
		if event.Kind == store.TaskCrateSetEvent || event.Kind == store.TaskCrateClearedEvent {
			if event.ActorID != "user:"+intakeOwner(core.WorkspaceRoleMaintainer) {
				t.Fatalf("membership actor = %q", event.ActorID)
			}
			var payload store.TaskCrateMovePayload
			_ = json.Unmarshal(event.Payload, &payload)
			moves = append(moves, payload)
		}
	}
	want := []store.TaskCrateMovePayload{{TaskID: task.ID, CrateID: first.ID}, {TaskID: task.ID, CrateID: second.ID, LeftCrateID: first.ID}, {TaskID: task.ID, LeftCrateID: second.ID}}
	if !slices.Equal(moves, want) {
		t.Fatalf("membership events = %+v, want %+v", moves, want)
	}
}

func TestCrateIntake(t *testing.T) {
	f := newCrateFixture(t)
	declared := f.requirement(t, f.ctx, true)
	named := f.requirement(t, f.ctx, true)
	crate := f.createCrate(t, f.editor, map[string]any{"name": "Intake", "requirement_ids": []string{declared}})
	other := f.createCrate(t, f.editor, map[string]any{"name": "Elsewhere"})
	closed := f.createCrate(t, f.editor, map[string]any{"name": "Shut"})
	if status, raw := f.do(t, f.editor, http.MethodPatch, "/v1/crates/"+closed.ID, `{"state":"closed"}`); status != http.StatusOK {
		t.Fatalf("close status=%d body=%s", status, raw)
	}
	foreign := f.createCrate(t, f.foreign, map[string]any{"name": "Beta"})

	created := f.postTaskJSON(t, f.editor, map[string]any{"body": "Crate intake.", "repo": "api", "crate_id": crate.ID, "requirement_ids": []string{named}})
	if created.status != http.StatusCreated || created.task.Crate == nil || created.task.Crate.ID != crate.ID || created.task.Crate.Name != "Intake" {
		t.Fatalf("intake status=%d body=%s", created.status, created.body)
	}
	// The task takes exactly the requirements its request names (req-crates AC-3.1).
	active, err := store.TaskContextForTask(f.ctx, f.store, created.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(active.Requirements) != 1 || active.Requirements[0].ID != named {
		t.Fatalf("intake context = %+v, want only %s", active.Requirements, named)
	}
	plain := f.postTaskJSON(t, f.editor, map[string]any{"body": "No crate.", "repo": "api"})
	if plain.status != http.StatusCreated || plain.task.Crate != nil || strings.Contains(plain.body, `"crate"`) {
		t.Fatalf("intake without crate status=%d body=%s", plain.status, plain.body)
	}

	before := f.recorded(t)
	for _, tc := range []struct {
		name    string
		crateID string
		status  int
		code    string
	}{
		{"missing", "crate-000000-000000", http.StatusNotFound, "crate_not_found"},
		{"closed", closed.ID, http.StatusConflict, "crate_closed"},
		{"other workspace", foreign.ID, http.StatusNotFound, "crate_not_found"},
	} {
		refused := f.postTaskJSON(t, f.editor, map[string]any{"body": "Refused intake " + tc.name + ".", "repo": "api", "crate_id": tc.crateID})
		if refused.status != tc.status || refused.code() != tc.code || !strings.Contains(refused.body, tc.crateID) {
			t.Fatalf("%s intake status=%d body=%s", tc.name, refused.status, refused.body)
		}
		multipart := f.postTaskMultipart(t, f.editor, map[string]any{"body": "Refused upload " + tc.name + ".", "repo": "api", "crate_id": tc.crateID}, "refused-"+tc.name)
		if multipart.status != tc.status || multipart.code() != tc.code {
			t.Fatalf("%s multipart status=%d body=%s", tc.name, multipart.status, multipart.body)
		}
	}
	if after := f.recorded(t); after != before {
		t.Fatalf("refused intake recorded state:\nbefore %s\nafter  %s", before, after)
	}

	fields := map[string]any{"body": "Keyed crate intake.", "repo": "api", "crate_id": crate.ID}
	first := f.postTaskMultipart(t, f.editor, fields, "crate-retry")
	if first.status != http.StatusCreated || first.task.TaskCrateID() != crate.ID {
		t.Fatalf("multipart intake status=%d body=%s", first.status, first.body)
	}
	if again := f.postTaskMultipart(t, f.editor, fields, "crate-retry"); again.status != http.StatusOK || again.task.ID != first.task.ID {
		t.Fatalf("equal retry status=%d body=%s", again.status, again.body)
	}
	different := map[string]any{"body": "Keyed crate intake.", "repo": "api", "crate_id": other.ID}
	if again := f.postTaskMultipart(t, f.editor, different, "crate-retry"); again.status != http.StatusConflict {
		t.Fatalf("retry naming another crate status=%d body=%s", again.status, again.body)
	}
	withoutCrate := map[string]any{"body": "Keyed crate intake.", "repo": "api"}
	if again := f.postTaskMultipart(t, f.editor, withoutCrate, "crate-retry"); again.status != http.StatusConflict {
		t.Fatalf("retry naming no crate status=%d body=%s", again.status, again.body)
	}
	// A later move does not change what the retry compares: the recorded
	// request named the first crate (component-crates invariant 7).
	if status, raw := f.do(t, f.editor, http.MethodPut, "/v1/tasks/"+first.task.ID+"/crate", map[string]any{"crate_id": other.ID}); status != http.StatusOK {
		t.Fatalf("move status=%d body=%s", status, raw)
	}
	if again := f.postTaskMultipart(t, f.editor, fields, "crate-retry"); again.status != http.StatusOK || again.task.ID != first.task.ID {
		t.Fatalf("retry after a move status=%d body=%s", again.status, again.body)
	}
	if again := f.postTaskMultipart(t, f.editor, different, "crate-retry"); again.status != http.StatusConflict {
		t.Fatalf("retry naming the current crate status=%d body=%s", again.status, again.body)
	}
}

func TestCrateTaskFilter(t *testing.T) {
	f := newCrateFixture(t)
	crate := f.createCrate(t, f.editor, map[string]any{"name": "Filtered"})
	other := f.createCrate(t, f.editor, map[string]any{"name": "Other"})
	members := []string{f.task(t, crate.ID).ID, f.task(t, crate.ID).ID}
	f.task(t, other.ID)
	f.task(t, "")
	slices.Sort(members)
	for _, path := range []string{"/v1/tasks?crate=" + crate.ID, "/v1/activity?crate=" + crate.ID} {
		status, raw := f.do(t, f.viewer, http.MethodGet, path, nil)
		if status != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", path, status, raw)
		}
		got := map[string]*core.TaskCrate{}
		for id, rowCrate := range crateRows(t, raw) {
			got[id] = rowCrate
			if rowCrate == nil || rowCrate.ID != crate.ID || rowCrate.Name != "Filtered" {
				t.Fatalf("%s row %s crate = %+v, want {%s Filtered}", path, id, rowCrate, crate.ID)
			}
		}
		ids := slices.Sorted(maps.Keys(got))
		if !slices.Equal(ids, members) {
			t.Fatalf("%s rows = %v, want %v", path, ids, members)
		}
	}
	status, raw := f.do(t, f.viewer, http.MethodGet, "/v1/tasks?crate="+crate.ID+"&limit=1", nil)
	if status != http.StatusOK || strings.Count(string(raw), `"next_action"`) != 1 {
		t.Fatalf("paged filter status=%d body=%s", status, raw)
	}
	status, raw = f.do(t, f.viewer, http.MethodGet, "/v1/tasks/"+members[0], nil)
	if status != http.StatusOK || !strings.Contains(string(raw), `"crate":{"id":"`+crate.ID+`","name":"Filtered"}`) {
		t.Fatalf("task read status=%d body=%s", status, raw)
	}
	status, raw = f.do(t, f.viewer, http.MethodGet, "/v1/crates/"+crate.ID, nil)
	var detail struct {
		Tasks []struct {
			ID          string          `json:"id"`
			Branch      string          `json:"branch"`
			NextAction  json.RawMessage `json:"next_action"`
			PullRequest json.RawMessage `json:"pull_request"`
		} `json:"tasks"`
		Total int `json:"total_task_count"`
		Open  int `json:"open_task_count"`
	}
	if err := json.Unmarshal(raw, &detail); err != nil || status != http.StatusOK || len(detail.Tasks) != 2 || detail.Total != 2 || detail.Open != 2 {
		t.Fatalf("crate read status=%d body=%s", status, raw)
	}
	// Each member row's next action is the one GET /v1/tasks states
	// (component-crates invariant 12).
	_, listRaw := f.do(t, f.viewer, http.MethodGet, "/v1/tasks?crate="+crate.ID, nil)
	var listed []struct {
		ID         string          `json:"id"`
		NextAction json.RawMessage `json:"next_action"`
	}
	_ = json.Unmarshal(listRaw, &listed)
	byID := map[string]string{}
	for _, row := range listed {
		byID[row.ID] = string(row.NextAction)
	}
	for _, row := range detail.Tasks {
		if row.Branch == "" || string(row.PullRequest) != "null" || byID[row.ID] == "" || string(row.NextAction) != byID[row.ID] {
			t.Fatalf("crate member row %+v, want the task list next action %s", row, byID[row.ID])
		}
	}
	// An activity row carries the current name after a rename, and no crate
	// after its task leaves the crate (component-crates, Public surfaces).
	if status, raw := f.do(t, f.editor, http.MethodPatch, "/v1/crates/"+crate.ID, `{"name":"Renamed"}`); status != http.StatusOK {
		t.Fatalf("rename status=%d body=%s", status, raw)
	}
	if status, raw := f.do(t, f.editor, http.MethodDelete, "/v1/tasks/"+members[0]+"/crate", nil); status != http.StatusOK {
		t.Fatalf("leave status=%d body=%s", status, raw)
	}
	status, raw = f.do(t, f.viewer, http.MethodGet, "/v1/activity", nil)
	if status != http.StatusOK {
		t.Fatalf("activity status=%d body=%s", status, raw)
	}
	rows := crateRows(t, raw)
	if rowCrate, ok := rows[members[0]]; !ok || rowCrate != nil {
		t.Fatalf("activity row of the task that left = %+v (present %v), want no crate", rowCrate, ok)
	}
	if rowCrate := rows[members[1]]; rowCrate == nil || rowCrate.ID != crate.ID || rowCrate.Name != "Renamed" {
		t.Fatalf("activity row after the rename = %+v, want {%s Renamed}", rowCrate, crate.ID)
	}
	if status, raw := f.do(t, f.viewer, http.MethodGet, "/v1/activity?crate="+crate.ID, nil); status != http.StatusOK || len(crateRows(t, raw)) != 1 {
		t.Fatalf("crate filter after the task left status=%d body=%s", status, raw)
	}
}

func TestCrateActivityRoute(t *testing.T) {
	f := newCrateFixture(t)
	crate := f.createCrate(t, f.editor, map[string]any{"name": "Busy"})
	member := f.task(t, crate.ID)
	if err := f.store.AppendEvent(f.ctx, core.Event{TaskID: member.ID, Kind: "pull_request.opened", Payload: core.JSONPayload(map[string]any{"number": 12, "url": "https://github.com/acme/api/pull/12", "repository": "acme/api"})}); err != nil {
		t.Fatal(err)
	}
	if err := f.store.AppendEvent(f.ctx, core.Event{TaskID: member.ID, Kind: "review.round_completed", Payload: core.JSONPayload(map[string]any{"verdict": "changes_requested"})}); err != nil {
		t.Fatal(err)
	}
	status, raw := f.do(t, f.viewer, http.MethodGet, "/v1/crates/"+crate.ID+"/activity?limit=2", nil)
	if status != http.StatusOK {
		t.Fatalf("activity status=%d body=%s", status, raw)
	}
	var page struct {
		Events []struct {
			Kind        string `json:"kind"`
			TaskID      string `json:"task_id"`
			TaskTitle   string `json:"task_title"`
			Verdict     string `json:"verdict"`
			PullRequest *struct {
				URL string `json:"url"`
			} `json:"pull_request"`
		} `json:"events"`
		NextCursor string `json:"next_cursor"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 2 || page.Events[0].Kind != "review.round_completed" || page.Events[0].Verdict != "changes_requested" ||
		page.Events[1].Kind != "pull_request.opened" || page.Events[1].PullRequest == nil || page.Events[1].PullRequest.URL != "https://github.com/acme/api/pull/12" ||
		page.Events[0].TaskID != member.ID || page.Events[0].TaskTitle != member.Title || page.NextCursor == "" {
		t.Fatalf("activity page = %s", raw)
	}
	status, raw = f.do(t, f.viewer, http.MethodGet, "/v1/crates/"+crate.ID+"/activity?limit=2&cursor="+page.NextCursor, nil)
	if status != http.StatusOK || !strings.Contains(string(raw), `"kind":"task.created"`) || strings.Contains(string(raw), "next_cursor") {
		t.Fatalf("second page status=%d body=%s", status, raw)
	}
}

// TestCrateMembershipLeavesDeliveryUnchanged holds invariant 1: a membership
// act changes no claimable work order, no claim eligibility, and no lineage
// relationship (req-crates AC-4.1, AC-4.2, AC-4.4).
func TestCrateMembershipLeavesDeliveryUnchanged(t *testing.T) {
	f := newCrateFixture(t)
	crate := f.createCrate(t, f.editor, map[string]any{"name": "Delivery"})
	tasks := []core.Task{f.task(t, ""), f.task(t, ""), f.task(t, "")}
	for i, task := range tasks {
		order := core.WorkOrder{ID: task.ID + "-implement-1", JobID: task.ID + "-implement-1", TaskID: task.ID, Stage: core.StageImplement, State: core.WorkOrderQueued, CreatedAt: time.Now().UTC().Add(time.Duration(i) * time.Second)}
		if err := f.store.CreateJob(f.ctx, core.Job{ID: order.JobID, TaskID: task.ID, Stage: order.Stage, State: core.JobPending}); err != nil {
			t.Fatal(err)
		}
		if err := storetest.For(f.store).CreateWorkOrder(f.ctx, order); err != nil {
			t.Fatal(err)
		}
	}
	type orderView struct {
		ID        string `json:"id"`
		Claimable bool   `json:"claimable"`
		State     string `json:"state"`
	}
	orders := func() []orderView {
		t.Helper()
		status, raw := f.do(t, f.viewer, http.MethodGet, "/v1/work-orders", nil)
		if status != http.StatusOK {
			t.Fatalf("work orders status=%d body=%s", status, raw)
		}
		var listed []orderView
		if err := json.Unmarshal(raw, &listed); err != nil {
			t.Fatal(err)
		}
		return listed
	}
	lineage := func(id string) string {
		t.Helper()
		status, raw := f.do(t, f.viewer, http.MethodGet, "/v1/lineage/task/"+id, nil)
		if status != http.StatusOK {
			t.Fatalf("lineage status=%d body=%s", status, raw)
		}
		return string(raw)
	}
	snapshot := func(id string) string {
		t.Helper()
		task, err := f.store.GetTask(f.ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		context, err := store.TaskContextForTask(f.ctx, f.store, id)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal([]any{task.State, task.GateKind, task.PlanState, task.MergeApproval, task.SetupContract, task.Assignee, task.Dependencies, task.NextStage, context})
		return string(raw)
	}
	ordersBefore := orders()
	lineageBefore := lineage(tasks[1].ID)
	stateBefore := snapshot(tasks[1].ID)
	if status, raw := f.do(t, f.editor, http.MethodPut, "/v1/tasks/"+tasks[1].ID+"/crate", map[string]any{"crate_id": crate.ID}); status != http.StatusOK {
		t.Fatalf("join status=%d body=%s", status, raw)
	}
	if got := orders(); !slices.Equal(got, ordersBefore) {
		t.Fatalf("work orders with a member = %+v, without %+v", got, ordersBefore)
	}
	if got := lineage(tasks[1].ID); got != lineageBefore || strings.Contains(got, crate.ID) || strings.Contains(got, "crate") {
		t.Fatalf("lineage changed with membership:\nbefore %s\nafter  %s", lineageBefore, got)
	}
	if got := snapshot(tasks[1].ID); got != stateBefore {
		t.Fatalf("membership changed delivery fields:\nbefore %s\nafter  %s", stateBefore, got)
	}
	if status, raw := f.do(t, f.editor, http.MethodDelete, "/v1/tasks/"+tasks[1].ID+"/crate", nil); status != http.StatusOK {
		t.Fatalf("leave status=%d body=%s", status, raw)
	}
	if got := orders(); !slices.Equal(got, ordersBefore) {
		t.Fatalf("work orders after leaving = %+v, want %+v", got, ordersBefore)
	}
	if got := snapshot(tasks[1].ID); got != stateBefore {
		t.Fatalf("leaving changed delivery fields:\nbefore %s\nafter  %s", stateBefore, got)
	}
}

// TestCrateWorkOrderTaskCarriesNoCrate holds the boundary with the sibling
// work-order task: a member task's order carries no crate through its task
// row, so crate content reaches an order only through the labeled context
// field of component-crates invariant 20 (req-crates AC-4.3, AC-4.5).
func TestCrateWorkOrderTaskCarriesNoCrate(t *testing.T) {
	f := newCrateFixture(t)
	crate := f.createCrate(t, f.editor, map[string]any{"name": "Hidden from orders"})
	member := f.task(t, crate.ID)
	job := core.Job{ID: member.ID + "-implement-1", TaskID: member.ID, Stage: core.StageImplement, State: core.JobPending}
	if err := f.store.CreateJob(f.ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := storetest.For(f.store).CreateWorkOrder(f.ctx, core.WorkOrder{ID: job.ID, TaskID: member.ID, JobID: job.ID, Stage: job.Stage, State: core.WorkOrderQueued}); err != nil {
		t.Fatal(err)
	}
	order, err := storetest.For(f.store).ClaimWorkOrder(f.ctx, job.ID, core.WorkOrderClaim{ClaimantID: core.TaskRunClaimantID("owner"), SessionID: "crate-session", ClientToken: "crate-secret", Lease: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := pack.Load("")
	if err != nil {
		t.Fatal(err)
	}
	service := &workorder.Service{Store: f.store, Pack: loaded}
	result, err := service.Get(f.ctx, order.ID, order.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(result.Task)
	if err != nil {
		t.Fatal(err)
	}
	if result.Task.Crate != nil || strings.Contains(string(raw), crate.ID) || strings.Contains(string(raw), `"crate"`) {
		t.Fatalf("member order task = %s, want no crate", raw)
	}
	if stored, err := f.store.GetTask(f.ctx, member.ID); err != nil || stored.TaskCrateID() != crate.ID {
		t.Fatalf("the order read changed membership: %+v %v", stored.Crate, err)
	}
}

// crateRows reads a task list or an activity page and returns each row's
// crate by task ID. An activity row nests its task under "task".
func crateRows(t *testing.T, raw []byte) map[string]*core.TaskCrate {
	t.Helper()
	var rows []struct {
		ID    string          `json:"id"`
		Crate *core.TaskCrate `json:"crate"`
		Task  *struct {
			ID    string          `json:"id"`
			Crate *core.TaskCrate `json:"crate"`
		} `json:"task"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatalf("rows %s: %v", raw, err)
	}
	out := map[string]*core.TaskCrate{}
	for _, row := range rows {
		if row.Task != nil {
			out[row.Task.ID] = row.Task.Crate
			continue
		}
		out[row.ID] = row.Crate
	}
	return out
}

// insightMember stores one member of crateID with the given lifecycle
// fields, base branch, and creation time.
func (f *crateFixture) insightMember(t *testing.T, crateID string, state core.TaskState, stage core.Stage, gate core.GateKind, plan core.PlanState, base string, created time.Time) core.Task {
	t.Helper()
	task := core.Task{ID: core.NewTaskID(), Workspace: "alpha", Repo: "api", Title: "Insight " + string(state), Body: "Insight member.", State: state, NextStage: stage,
		GateKind: gate, PlanState: plan, BaseBranch: base, CreatedAt: created, Crate: &core.TaskCrate{ID: crateID}}
	task.Branch = "conveyor/task-" + task.ID
	if err := f.store.CreateTask(f.ctx, task); err != nil {
		t.Fatal(err)
	}
	return task
}

func (f *crateFixture) appendAt(t *testing.T, taskID, kind string, at time.Time, payload any) {
	t.Helper()
	if err := f.store.AppendEvent(f.ctx, core.Event{TaskID: taskID, Kind: kind, At: at.UTC(), Payload: core.JSONPayload(payload)}); err != nil {
		t.Fatal(err)
	}
}

// crateReadTaskIDs returns the task IDs of GET /v1/crates/{id} in order,
// with each row's next action.
func (f *crateFixture) crateReadTasks(t *testing.T, crateID string) ([]string, map[string]nextaction.Compact) {
	t.Helper()
	status, raw := f.do(t, f.viewer, http.MethodGet, "/v1/crates/"+crateID, nil)
	if status != http.StatusOK {
		t.Fatalf("crate read status=%d body=%s", status, raw)
	}
	var detail struct {
		Tasks []struct {
			ID         string             `json:"id"`
			NextAction nextaction.Compact `json:"next_action"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(raw, &detail); err != nil {
		t.Fatal(err)
	}
	ids := make([]string, len(detail.Tasks))
	actions := map[string]nextaction.Compact{}
	for i, row := range detail.Tasks {
		ids[i] = row.ID
		actions[row.ID] = row.NextAction
	}
	return ids, actions
}

func crateJSONKeys(t *testing.T, raw json.RawMessage) []string {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	keys := slices.Collect(maps.Keys(fields))
	slices.Sort(keys)
	return keys
}

func crateSortedStrings(values ...string) []string {
	slices.Sort(values)
	return values
}

// TestCrateInsightRoute serves the insight view of component-crates to a
// viewer: exactly the documented fields, the values crateinsight.Compute
// gives for the store's inputs, the projection's next actions, and the
// read instant, and the member order the crate read shares (invariants 21,
// 22, and 28; req-crate-insights AC-1.1, AC-1.2, AC-1.4, AC-2.1, AC-2.3,
// AC-2.4, AC-4.1 to AC-4.4; req-crates AC-5.7). MCP get_crate keeps its
// oldest-first member IDs (invariant 14).
func TestCrateInsightRoute(t *testing.T) {
	f := newCrateFixture(t)
	readAt := time.Now().UTC().Truncate(time.Second)
	f.server.nextActionClock = func() time.Time { return readAt }
	crate := f.createCrate(t, f.editor, map[string]any{"name": "Insight", "target_date": "2027-03-01"})
	base := readAt.Add(-24 * time.Hour)
	day := 24 * time.Hour

	merged := f.insightMember(t, crate.ID, core.TaskMerged, "", "", core.PlanNotRequired, "develop", base.Add(-10*day))
	f.appendAt(t, merged.ID, "pull_request.opened", base.Add(-9*day), map[string]any{"number": 11})
	f.appendAt(t, merged.ID, "review.round_completed", base.Add(-8*day), map[string]any{"verdict": "approve"})
	f.appendAt(t, merged.ID, "task.state_changed", base.Add(-5*day), map[string]any{"from": "approved", "to": "merged"})
	closed := f.insightMember(t, crate.ID, core.TaskClosed, "", "", core.PlanNotRequired, "main", base.Add(-9*day))
	f.appendAt(t, closed.ID, "task.cancelled", base.Add(-4*day), map[string]any{"reason": "dropped"})
	f.appendAt(t, closed.ID, "task.state_changed", base.Add(-4*day), map[string]any{"from": "queued", "to": "closed"})
	building := f.insightMember(t, crate.ID, core.TaskRunning, core.StageImplement, "", core.PlanNotRequired, "main", base)
	f.appendAt(t, building.ID, "pull_request.opened", base.Add(2*time.Hour), map[string]any{"number": 12})
	f.appendAt(t, building.ID, "review.round_completed", base.Add(3*time.Hour), map[string]any{"verdict": "changes_requested"})
	f.appendAt(t, building.ID, "pull_request.opened", base.Add(4*time.Hour), map[string]any{"number": 13})
	mergeGate := f.insightMember(t, crate.ID, core.TaskAwaiting, core.StageGate, core.GateKindMergeApproval, core.PlanNotRequired, "main", base.Add(30*time.Minute))
	planGate := f.insightMember(t, crate.ID, core.TaskAwaiting, core.StageImplement, core.GateKindPlanApproval, core.PlanAwaitingApproval, "main", base.Add(time.Hour))
	tieA := f.insightMember(t, crate.ID, core.TaskQueued, core.StageImplement, "", core.PlanNotRequired, "main", base.Add(5*time.Hour))
	tieB := f.insightMember(t, crate.ID, core.TaskQueued, core.StageImplement, "", core.PlanNotRequired, "main", base.Add(5*time.Hour))
	ties := crateSortedStrings(tieA.ID, tieB.ID)

	status, raw := f.do(t, f.viewer, http.MethodGet, "/v1/crates/"+crate.ID+"/insight", nil)
	if status != http.StatusOK {
		t.Fatalf("insight status=%d body=%s", status, raw)
	}
	if got, want := crateJSONKeys(t, raw), crateSortedStrings("tasks", "branches", "whereabouts", "progress", "first_review", "forecast_days_before_target", "replay"); !slices.Equal(got, want) {
		t.Fatalf("view keys %v, want %v", got, want)
	}
	var view crateinsight.View
	if err := json.Unmarshal(raw, &view); err != nil {
		t.Fatal(err)
	}
	var shape struct {
		Tasks       []json.RawMessage `json:"tasks"`
		Whereabouts []json.RawMessage `json:"whereabouts"`
		Replay      struct {
			Steps []json.RawMessage `json:"steps"`
		} `json:"replay"`
		ReplayRaw   json.RawMessage `json:"-"`
		FirstReview json.RawMessage `json:"first_review"`
		Progress    json.RawMessage `json:"progress"`
	}
	if err := json.Unmarshal(raw, &shape); err != nil {
		t.Fatal(err)
	}
	for _, check := range []struct {
		name string
		raw  json.RawMessage
		want []string
	}{
		{"task", shape.Tasks[0], crateSortedStrings("id", "phase", "station", "base_branch", "created_at", "pull_request_opened_at", "changes_requested_at", "review_rounds", "merged_at", "closed_at")},
		{"whereabouts", shape.Whereabouts[0], crateSortedStrings("key", "branch", "task_ids")},
		{"step", shape.Replay.Steps[0], crateSortedStrings("at", "task_id", "phase", "station")},
		{"first_review", shape.FirstReview, crateSortedStrings("approved", "reviewed")},
		{"progress", shape.Progress, crateSortedStrings("daily", "forecast", "weekly", "delivered", "total", "open", "throughput_per_week", "median_cycle_seconds", "status_mix")},
	} {
		if got := crateJSONKeys(t, check.raw); !slices.Equal(got, check.want) {
			t.Fatalf("%s keys %v, want %v", check.name, got, check.want)
		}
	}

	// One order for both REST reads: judgments, then the other open
	// members, then merged, then closed; creation time and then ID inside
	// each group.
	wantOrder := []string{mergeGate.ID, planGate.ID, building.ID, ties[0], ties[1], merged.ID, closed.ID}
	readOrder, actions := f.crateReadTasks(t, crate.ID)
	insightOrder := make([]string, len(view.Tasks))
	for i, entry := range view.Tasks {
		insightOrder[i] = entry.ID
	}
	if !slices.Equal(readOrder, wantOrder) || !slices.Equal(insightOrder, wantOrder) {
		t.Fatalf("crate read order %v, insight order %v, want %v", readOrder, insightOrder, wantOrder)
	}

	// The served view is the pure computation over the store's inputs,
	// the projection's statements, and the read instant.
	inputs, err := f.store.(store.CrateStore).ReadCrateInsightInputs(f.ctx, crate.ID)
	if err != nil {
		t.Fatal(err)
	}
	want, err := json.Marshal(crateinsight.Compute(inputs, actions, readAt))
	if err != nil {
		t.Fatal(err)
	}
	if !crateJSONEqual(t, raw, want) {
		t.Fatalf("served view differs from the computation:\n got %s\nwant %s", raw, want)
	}

	entries := map[string]crateinsight.Task{}
	for _, entry := range view.Tasks {
		entries[entry.ID] = entry
	}
	if got := entries[merged.ID]; got.Phase != crateinsight.PhaseMerged || got.Station != crateinsight.StationCrate || got.MergedAt == nil || !got.MergedAt.Equal(base.Add(-5*day)) || got.ReviewRounds != 1 || got.BaseBranch != "develop" {
		t.Fatalf("merged entry %+v", got)
	}
	if got := entries[closed.ID]; got.Phase != crateinsight.PhaseClosed || got.Station != crateinsight.StationAside || got.ClosedAt == nil || !got.ClosedAt.Equal(base.Add(-4*day)) {
		t.Fatalf("closed entry %+v", got)
	}
	if got := entries[building.ID]; got.Station != crateinsight.StationBuild || len(got.PullRequestOpenedAt) != 2 || len(got.ChangesRequestedAt) != 1 || !got.ChangesRequestedAt[0].Equal(base.Add(3*time.Hour)) || got.ReviewRounds != 1 {
		t.Fatalf("building entry %+v", got)
	}
	if got := entries[mergeGate.ID]; got.Phase != crateinsight.PhaseNeedsYou || got.Station != crateinsight.StationGate {
		t.Fatalf("merge gate entry %+v", got)
	}
	if got := entries[planGate.ID]; got.Phase != crateinsight.PhaseNeedsYou || got.Station != crateinsight.StationPlan {
		t.Fatalf("plan gate entry %+v", got)
	}
	if !slices.Equal(view.Branches, []string{"main", "develop"}) {
		t.Fatalf("branches %v", view.Branches)
	}
	last := view.Whereabouts[len(view.Whereabouts)-1]
	if last.Key != crateinsight.WhereaboutsMerged || last.Branch == nil || *last.Branch != "main" || len(last.TaskIDs) != 0 {
		t.Fatalf("last whereabouts %+v, want an empty main entry", last)
	}
	if !view.Replay.End.Equal(readAt) || !view.Replay.Start.Equal(merged.CreatedAt) {
		t.Fatalf("replay span %s to %s", view.Replay.Start, view.Replay.End)
	}
	if view.FirstReview != (crateinsight.FirstReview{Approved: 1, Reviewed: 2}) {
		t.Fatalf("first review %+v", view.FirstReview)
	}
	if view.Progress.Delivered != 1 || view.Progress.Open != 5 || view.Progress.Total != 6 || view.Progress.ThroughputPerWeek != 0.5 || view.Progress.Forecast == nil || view.ForecastDaysBeforeTarget == nil {
		t.Fatalf("delivery numbers %+v, difference %v", view.Progress, view.ForecastDaysBeforeTarget)
	}

	// MCP get_crate keeps the oldest-first member IDs of invariant 14.
	oldest := []core.Task{merged, closed, building, mergeGate, planGate, tieA, tieB}
	slices.SortFunc(oldest, func(a, b core.Task) int {
		if n := a.CreatedAt.Compare(b.CreatedAt); n != 0 {
			return n
		}
		return strings.Compare(a.ID, b.ID)
	})
	oldestIDs := make([]string, len(oldest))
	for i, task := range oldest {
		oldestIDs[i] = task.ID
	}
	if got := crateMCPRows(t, readAs(t, f.handler, f.viewer, "get_crate", map[string]any{"workspace_id": "alpha", "crate_id": crate.ID})); len(got) != 1 || !slices.Equal(got[0].TaskIDs, oldestIDs) {
		t.Fatalf("MCP get_crate task IDs %+v, want oldest first %v", got, oldestIDs)
	}
}

func crateJSONEqual(t *testing.T, a, b []byte) bool {
	t.Helper()
	var left, right any
	if err := json.Unmarshal(a, &left); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &right); err != nil {
		t.Fatal(err)
	}
	return reflect.DeepEqual(left, right)
}

// TestCrateInsightRouteEmptyCrate serves an empty crate's view with empty
// lists and a replay span at the read instant.
func TestCrateInsightRouteEmptyCrate(t *testing.T) {
	f := newCrateFixture(t)
	readAt := time.Now().UTC().Truncate(time.Second)
	f.server.nextActionClock = func() time.Time { return readAt }
	crate := f.createCrate(t, f.editor, map[string]any{"name": "Empty insight"})
	status, raw := f.do(t, f.viewer, http.MethodGet, "/v1/crates/"+crate.ID+"/insight", nil)
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, raw)
	}
	var view crateinsight.View
	if err := json.Unmarshal(raw, &view); err != nil {
		t.Fatal(err)
	}
	if len(view.Tasks) != 0 || view.Tasks == nil || len(view.Replay.Steps) != 0 || !view.Replay.Start.Equal(readAt) || !view.Replay.End.Equal(readAt) || view.Progress.Forecast != nil {
		t.Fatalf("empty crate view %s", raw)
	}
}

// TestCrateInsightRouteRefusalsAndCapability: a missing crate and another
// workspace's crate answer 404 crate_not_found, every role that holds
// view_workspace reads the view, and a caller without view_workspace gets
// the canonical workspace 404 and no view (component-crates invariant 3).
func TestCrateInsightRouteRefusalsAndCapability(t *testing.T) {
	f := newCrateFixture(t)
	crate := f.createCrate(t, f.editor, map[string]any{"name": "Guarded insight"})
	f.task(t, crate.ID)
	foreign := f.createCrate(t, f.foreign, map[string]any{"name": "Beta insight"})
	before := f.recorded(t)
	for _, id := range []string{"crate-000000-000000", foreign.ID} {
		status, raw := f.do(t, f.viewer, http.MethodGet, "/v1/crates/"+id+"/insight", nil)
		if status != http.StatusNotFound || crateErrorCode(t, raw)["error"] != "crate_not_found" || crateErrorCode(t, raw)["crate_id"] != id {
			t.Fatalf("insight of %s status=%d body=%s", id, status, raw)
		}
	}
	if status, raw := f.do(t, f.foreign, http.MethodGet, "/v1/crates/"+crate.ID+"/insight", nil); status != http.StatusNotFound || crateErrorCode(t, raw)["error"] != "crate_not_found" {
		t.Fatalf("beta member read of an alpha crate status=%d body=%s", status, raw)
	}
	for _, token := range []string{f.viewer, f.nonMaker, f.editor} {
		if status, raw := f.do(t, token, http.MethodGet, "/v1/crates/"+crate.ID+"/insight", nil); status != http.StatusOK || !strings.Contains(string(raw), `"replay"`) {
			t.Fatalf("%s insight status=%d body=%s", token, status, raw)
		}
	}
	status, raw := f.do(t, "pat-unbound", http.MethodGet, "/v1/crates/"+crate.ID+"/insight", nil)
	if status != http.StatusNotFound || strings.Contains(string(raw), `"replay"`) || strings.Contains(string(raw), crate.ID) {
		t.Fatalf("unbound insight status=%d body=%s", status, raw)
	}
	if after := f.recorded(t); after != before {
		t.Fatalf("insight reads recorded state:\nbefore %s\nafter  %s", before, after)
	}
}

// crateInsightFailingStore fails the insight input read or the next-action
// projection's work-order read with an error that carries a secret.
type crateInsightFailingStore struct {
	crateBackend
	failInputs     error
	failProjection error
}

func (s *crateInsightFailingStore) ReadCrateInsightInputs(ctx context.Context, id string) (store.CrateInsightInputs, error) {
	if s.failInputs != nil {
		return store.CrateInsightInputs{}, s.failInputs
	}
	return s.crateBackend.ReadCrateInsightInputs(ctx, id)
}

// ListWorkOrdersForTasks is the projection's first store read.
func (s *crateInsightFailingStore) ListWorkOrdersForTasks(ctx context.Context, ids []string) ([]core.WorkOrder, error) {
	if s.failProjection != nil {
		return nil, s.failProjection
	}
	return s.crateBackend.ListWorkOrdersForTasks(ctx, ids)
}

// crateCapabilityFailingMemberships fails the caller capability lookup that
// the next-action options read for operate_gates. Route admission reads
// view_workspace, so the request still reaches the handler.
type crateCapabilityFailingMemberships struct {
	store.MembershipStore
	err error
}

func (m crateCapabilityFailingMemberships) AuthorizeWorkspace(ctx context.Context, userID, workspaceID string, capability core.Capability) (bool, error) {
	if capability == core.CapabilityOperateGates {
		return false, m.err
	}
	return m.MembershipStore.AuthorizeWorkspace(ctx, userID, workspaceID, capability)
}

// TestCrateInsightRouteFailsWhole: a failed input read, a failed caller
// capability read, and a failed next-action projection each fail the
// insight read whole with the generic 500. No part of the view, no member
// ID, and no store text reaches the response, and the read records nothing
// (component-crates, State and failure; DEC-105(1)). The crate read fails
// whole on a projection failure the same way.
func TestCrateInsightRouteFailsWhole(t *testing.T) {
	const secret = "hunter2hunter2hunter2hunter2"
	for _, tc := range []struct {
		name        string
		store       func(crateBackend) *crateInsightFailingStore
		memberships bool
		paths       func(crateID string) []string
	}{
		{"input read", func(inner crateBackend) *crateInsightFailingStore {
			return &crateInsightFailingStore{crateBackend: inner, failInputs: errors.New("driver refused the insight read password=" + secret)}
		}, false, func(id string) []string { return []string{"/v1/crates/" + id + "/insight"} }},
		{"projection", func(inner crateBackend) *crateInsightFailingStore {
			return &crateInsightFailingStore{crateBackend: inner, failProjection: errors.New("driver refused the work-order read password=" + secret)}
		}, false, func(id string) []string { return []string{"/v1/crates/" + id + "/insight", "/v1/crates/" + id} }},
		{"capability read", func(inner crateBackend) *crateInsightFailingStore {
			return &crateInsightFailingStore{crateBackend: inner}
		}, true, func(id string) []string { return []string{"/v1/crates/" + id + "/insight"} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCrateFixture(t)
			crate := f.createCrate(t, f.editor, map[string]any{"name": "Failing insight"})
			member := f.task(t, crate.ID)
			before := f.recorded(t)
			f.server.Store = tc.store(f.store.(crateBackend))
			if tc.memberships {
				f.server.Memberships = crateCapabilityFailingMemberships{MembershipStore: f.members, err: errors.New("driver refused the capability read password=" + secret)}
			}
			var captured bytes.Buffer
			previous := log.Writer()
			log.SetOutput(&captured)
			t.Cleanup(func() { log.SetOutput(previous) })
			for _, path := range tc.paths(crate.ID) {
				status, raw := f.do(t, f.viewer, http.MethodGet, path, nil)
				if status != http.StatusInternalServerError || string(raw) != "internal server error\n" {
					t.Fatalf("%s status=%d body=%q, want the generic 500", path, status, raw)
				}
				if strings.Contains(string(raw), member.ID) || strings.Contains(string(raw), secret) {
					t.Fatalf("%s 500 body carries the view or the store text: %q", path, raw)
				}
			}
			log.SetOutput(previous)
			if strings.Contains(captured.String(), secret) || !strings.Contains(captured.String(), "store failure") || !strings.Contains(captured.String(), "driver refused") {
				t.Fatalf("log %q, want a redacted store failure that names its cause", captured.String())
			}
			f.server.Store, f.server.Memberships = f.store, f.members
			if after := f.recorded(t); after != before {
				t.Fatalf("failed reads recorded state:\nbefore %s\nafter  %s", before, after)
			}
		})
	}
}
