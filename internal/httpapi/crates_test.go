package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
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
// task list, every crate, and every event.
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
	raw, _ := json.Marshal(map[string]any{"tasks": ids, "crates": crates, "workspace_events": len(events), "task_events": taskEvents})
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
		var rows []struct {
			ID    string          `json:"id"`
			Crate *core.TaskCrate `json:"crate"`
			Task  *struct {
				ID string `json:"id"`
			} `json:"task"`
		}
		if err := json.Unmarshal(raw, &rows); err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, row := range rows {
			if row.Task != nil {
				got = append(got, row.Task.ID)
				continue
			}
			got = append(got, row.ID)
			if row.Crate == nil || row.Crate.ID != crate.ID || row.Crate.Name != "Filtered" {
				t.Fatalf("%s row crate = %+v", path, row.Crate)
			}
		}
		slices.Sort(got)
		if !slices.Equal(got, members) {
			t.Fatalf("%s rows = %v, want %v", path, got, members)
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
