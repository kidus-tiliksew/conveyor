package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/crateinsight"
	"github.com/kidus-tiliksew/conveyor/internal/nextaction"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// crateBackend is the store surface the crate routes read and write
// (component-crates).
type crateBackend interface {
	store.Store
	store.CrateStore
}

// defaultCrateActivityLimit is the page size of a crate activity read that
// sends no limit.
const defaultCrateActivityLimit = 50

func (s *Server) crateStore(w http.ResponseWriter) (crateBackend, bool) {
	backend, ok := s.Store.(crateBackend)
	if !ok {
		http.Error(w, "crates are not available on this store", http.StatusNotImplemented)
		return nil, false
	}
	return backend, true
}

// crateDetailResponse is the crate read of component-crates Public surfaces:
// the crate row plus requirements, undeclared_requirements, and tasks.
type crateDetailResponse struct {
	store.Crate
	Requirements           []store.CrateRequirementCoverage `json:"requirements"`
	UndeclaredRequirements []store.CrateRequirementCoverage `json:"undeclared_requirements"`
	Tasks                  []crateTaskRow                   `json:"tasks"`
}

// crateTaskRow is one member task on the crate read (req-crates AC-5.3).
type crateTaskRow struct {
	ID          string             `json:"id"`
	Title       string             `json:"title"`
	State       core.TaskState     `json:"state"`
	Branch      string             `json:"branch"`
	PullRequest *cratePullRequest  `json:"pull_request"`
	NextAction  nextaction.Compact `json:"next_action"`
}

// cratePullRequest names a member task's latest pull request.
type cratePullRequest struct {
	Number     int    `json:"number,omitempty"`
	URL        string `json:"url,omitempty"`
	Repository string `json:"repository,omitempty"`
}

// crateActivityItem is one member task event on the crate activity read
// (component-crates invariant 13; req-crates AC-5.5).
type crateActivityItem struct {
	ID          int64             `json:"id"`
	Kind        string            `json:"kind"`
	At          time.Time         `json:"at"`
	ActorID     string            `json:"actor_id"`
	ActorRole   core.ActorRole    `json:"actor_role"`
	TaskID      string            `json:"task_id"`
	TaskTitle   string            `json:"task_title"`
	PullRequest *cratePullRequest `json:"pull_request,omitempty"`
	Verdict     string            `json:"verdict,omitempty"`
}

type crateActivityResponse struct {
	Events     []crateActivityItem `json:"events"`
	NextCursor string              `json:"next_cursor,omitempty"`
}

// listCrates serves GET /v1/crates. state is open (the default), closed, or
// all (component-crates, Public surfaces; req-crates AC-5.1).
func (s *Server) listCrates(w http.ResponseWriter, r *http.Request) {
	backend, ok := s.crateStore(w)
	if !ok {
		return
	}
	query := store.CrateListQuery{State: store.CrateOpen}
	switch state := strings.TrimSpace(r.URL.Query().Get("state")); state {
	case "", string(store.CrateOpen):
	case string(store.CrateClosed):
		query.State = store.CrateClosed
	case "all":
		query.State = ""
	default:
		writeCrateError(w, r, &store.CrateFieldError{Field: "state", Message: "state must be open, closed, or all"})
		return
	}
	crates, err := backend.ListCrates(r.Context(), query)
	if err != nil {
		writeCrateError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, crates)
}

// createCrate serves POST /v1/crates. It answers 201 with the crate row
// (req-crates AC-1.1).
func (s *Server) createCrate(w http.ResponseWriter, r *http.Request) {
	backend, ok := s.crateStore(w)
	if !ok {
		return
	}
	fields, ok := decodeCrateFields(w, r)
	if !ok {
		return
	}
	var input store.CrateInput
	name, present, err := crateStringField(fields, "name", false)
	if err == nil && !present {
		err = &store.CrateFieldError{Field: "name", Message: "name is required"}
	}
	if err != nil {
		writeCrateError(w, r, err)
		return
	}
	input.Name = *name
	if input.TargetDate, _, err = crateStringField(fields, "target_date", true); err != nil {
		writeCrateError(w, r, err)
		return
	}
	if ids, present, err := crateStringListField(fields, "requirement_ids"); err != nil {
		writeCrateError(w, r, err)
		return
	} else if present {
		input.RequirementIDs = ids
	}
	crate, err := backend.CreateCrate(r.Context(), input)
	if err != nil {
		writeCrateError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, crate)
}

// patchCrate serves PATCH /v1/crates/{id}. An omitted field stays unchanged,
// and an explicit null target_date clears the date (req-crates AC-1.2,
// AC-1.4).
func (s *Server) patchCrate(w http.ResponseWriter, r *http.Request) {
	backend, ok := s.crateStore(w)
	if !ok {
		return
	}
	fields, ok := decodeCrateFields(w, r)
	if !ok {
		return
	}
	var patch store.CratePatch
	var err error
	if patch.Name, _, err = crateStringField(fields, "name", false); err != nil {
		writeCrateError(w, r, err)
		return
	}
	if patch.TargetDate, patch.TargetDateSet, err = crateStringField(fields, "target_date", true); err != nil {
		writeCrateError(w, r, err)
		return
	}
	if ids, present, err := crateStringListField(fields, "requirement_ids"); err != nil {
		writeCrateError(w, r, err)
		return
	} else if present {
		patch.RequirementIDs = &ids
	}
	state, _, err := crateStringField(fields, "state", false)
	if err != nil {
		writeCrateError(w, r, err)
		return
	}
	if state != nil {
		value := store.CrateState(*state)
		patch.State = &value
	}
	crate, err := backend.UpdateCrate(r.Context(), chi.URLParam(r, "id"), patch)
	if err != nil {
		writeCrateError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, crate)
}

// getCrate serves GET /v1/crates/{id}. Each member row's next_action comes
// from the projection GET /v1/tasks uses, and a failed projection fails the
// whole read (component-crates invariant 12, State and failure; DEC-61).
// tasks follows the order of invariant 28, the order the insight view uses
// (req-crates AC-5.7). MCP get_crate keeps the order of invariant 14.
func (s *Server) getCrate(w http.ResponseWriter, r *http.Request) {
	backend, ok := s.crateStore(w)
	if !ok {
		return
	}
	detail, err := store.ReadCrateDetail(r.Context(), backend, chi.URLParam(r, "id"))
	if err != nil {
		writeCrateError(w, r, err)
		return
	}
	opts, err := s.nextActionOptions(r)
	if err != nil {
		writeStoreFailure(w, r, err)
		return
	}
	opts.CompactOnly = true
	projected, err := store.ReadNextActions(r.Context(), s.Store, detail.Tasks, opts)
	if err != nil {
		writeStoreFailure(w, r, err)
		return
	}
	ids := make([]string, len(detail.Tasks))
	for i, task := range detail.Tasks {
		ids[i] = task.ID
	}
	pulls, err := s.Store.ListLatestPullRequestEventsForTasks(r.Context(), ids)
	if err != nil {
		writeStoreFailure(w, r, err)
		return
	}
	actions := make(map[string]nextaction.Compact, len(detail.Tasks))
	for _, task := range detail.Tasks {
		actions[task.ID] = nextaction.CompactFrom(projected[task.ID])
	}
	crateinsight.SortTasks(detail.Tasks, actions)
	response := crateDetailResponse{Crate: detail.Crate, Requirements: detail.Requirements, UndeclaredRequirements: detail.UndeclaredRequirements, Tasks: make([]crateTaskRow, 0, len(detail.Tasks))}
	for _, task := range detail.Tasks {
		row := crateTaskRow{
			ID: task.ID, Title: task.Title, State: task.State, Branch: task.Branch,
			NextAction: actions[task.ID],
		}
		if event, found := pulls[task.ID]; found {
			row.PullRequest = cratePullRequestFrom(event.Payload)
		}
		response.Tasks = append(response.Tasks, row)
	}
	writeJSON(w, http.StatusOK, response)
}

// getCrateInsight serves GET /v1/crates/{id}/insight under view_workspace:
// the insight view of component-crates, computed by crateinsight.Compute
// from the store's insight inputs, each member's compact next action from
// one batch call to the projection GET /v1/tasks uses, and the projection's
// read instant (invariants 21 and 22; DEC-61). A missing or foreign crate
// answers 404 crate_not_found. A failure of the input read or of the
// projection fails the whole read with the generic 500 before any part of
// the view is written (State and failure; DEC-105(1)).
func (s *Server) getCrateInsight(w http.ResponseWriter, r *http.Request) {
	backend, ok := s.crateStore(w)
	if !ok {
		return
	}
	state, err := s.readCrateInsightState(r, backend, chi.URLParam(r, "id"))
	if err != nil {
		writeCrateInsightFailure(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, crateinsight.Compute(state.inputs, state.actions, state.now))
}

// crateInsightState is one consistent observation of a crate's insight
// inputs: the store read, each member's compact next action, and the
// projection's read instant (component-crates invariants 21 and 22). The
// insight view and the briefing document are both computed from it.
type crateInsightState struct {
	inputs  crateinsight.Inputs
	actions map[string]nextaction.Compact
	now     time.Time
}

// crateInsightInputsError marks a failure of the insight input read, which
// writeCrateError answers, apart from a projection failure.
type crateInsightInputsError struct{ err error }

func (e *crateInsightInputsError) Error() string { return e.err.Error() }
func (e *crateInsightInputsError) Unwrap() error { return e.err }

// readCrateInsightState reads the store's insight inputs and projects each
// member's compact next action once through the projection GET /v1/tasks
// uses (DEC-61).
func (s *Server) readCrateInsightState(r *http.Request, backend crateBackend, crateID string) (crateInsightState, error) {
	inputs, err := backend.ReadCrateInsightInputs(r.Context(), crateID)
	if err != nil {
		return crateInsightState{}, &crateInsightInputsError{err: err}
	}
	opts, err := s.nextActionOptions(r)
	if err != nil {
		return crateInsightState{}, err
	}
	opts.CompactOnly = true
	actions := make(map[string]nextaction.Compact, len(inputs.Members))
	if len(inputs.Members) > 0 {
		tasks := make([]core.Task, len(inputs.Members))
		for i, member := range inputs.Members {
			tasks[i] = member.Task
		}
		projected, err := store.ReadNextActions(r.Context(), s.Store, tasks, opts)
		if err != nil {
			return crateInsightState{}, err
		}
		for _, task := range tasks {
			actions[task.ID] = nextaction.CompactFrom(projected[task.ID])
		}
	}
	return crateInsightState{inputs: inputs, actions: actions, now: opts.Now}, nil
}

// writeCrateInsightFailure answers a failed readCrateInsightState: an input
// read failure through writeCrateError, so a missing or foreign crate answers
// 404 crate_not_found, and any other failure with the generic 500
// (component-crates, State and failure; DEC-105(1)).
func writeCrateInsightFailure(w http.ResponseWriter, r *http.Request, err error) {
	var inputs *crateInsightInputsError
	if errors.As(err, &inputs) {
		writeCrateError(w, r, inputs.err)
		return
	}
	writeStoreFailure(w, r, err)
}

// getCrateActivity serves GET /v1/crates/{id}/activity: one page of the
// listed events of current members, newest first, each with its task
// (component-crates invariant 13; req-crates AC-5.5).
func (s *Server) getCrateActivity(w http.ResponseWriter, r *http.Request) {
	backend, ok := s.crateStore(w)
	if !ok {
		return
	}
	query := store.CrateActivityQuery{Limit: defaultCrateActivityLimit, Cursor: strings.TrimSpace(r.URL.Query().Get("cursor"))}
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil {
			writeCrateError(w, r, &store.CrateFieldError{Field: "limit", Message: "limit must be a whole number between 1 and 200"})
			return
		}
		query.Limit = limit
	}
	page, err := backend.ListCrateActivity(r.Context(), chi.URLParam(r, "id"), query)
	if err != nil {
		writeCrateError(w, r, err)
		return
	}
	response := crateActivityResponse{Events: make([]crateActivityItem, 0, len(page.Entries)), NextCursor: page.NextCursor}
	for _, entry := range page.Entries {
		item := crateActivityItem{
			ID: entry.Event.ID, Kind: entry.Event.Kind, At: entry.Event.At, ActorID: entry.Event.ActorID, ActorRole: entry.Event.ActorRole,
			TaskID: entry.Event.TaskID, TaskTitle: entry.TaskTitle,
		}
		switch entry.Event.Kind {
		case "pull_request.opened":
			item.PullRequest = cratePullRequestFrom(entry.Event.Payload)
		case "review.round_completed":
			var payload struct {
				Verdict string `json:"verdict"`
			}
			if json.Unmarshal(entry.Event.Payload, &payload) == nil {
				item.Verdict = payload.Verdict
			}
		}
		response.Events = append(response.Events, item)
	}
	writeJSON(w, http.StatusOK, response)
}

// setTaskCrate serves PUT /v1/tasks/{id}/crate. The task joins the named open
// crate and leaves the crate it held (req-crates AC-2.2, AC-2.4).
func (s *Server) setTaskCrate(w http.ResponseWriter, r *http.Request) {
	backend, ok := s.crateStore(w)
	if !ok {
		return
	}
	fields, ok := decodeCrateFields(w, r)
	if !ok {
		return
	}
	crateID, present, err := crateStringField(fields, "crate_id", false)
	if err == nil && (!present || strings.TrimSpace(*crateID) == "") {
		err = &store.CrateFieldError{Field: "crate_id", Message: "crate_id is required"}
	}
	if err != nil {
		writeCrateError(w, r, err)
		return
	}
	task, err := backend.SetTaskCrate(r.Context(), chi.URLParam(r, "id"), *crateID)
	if err != nil {
		writeCrateError(w, r, err)
		return
	}
	s.writeCrateTask(w, r, task)
}

// clearTaskCrate serves DELETE /v1/tasks/{id}/crate. A task may leave a
// closed crate (req-crates AC-2.3; component-crates invariant 6).
func (s *Server) clearTaskCrate(w http.ResponseWriter, r *http.Request) {
	backend, ok := s.crateStore(w)
	if !ok {
		return
	}
	task, err := backend.ClearTaskCrate(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeCrateError(w, r, err)
		return
	}
	s.writeCrateTask(w, r, task)
}

func (s *Server) writeCrateTask(w http.ResponseWriter, r *http.Request, task core.Task) {
	s.writeTaskJSON(w, r, http.StatusOK, task)
}

func cratePullRequestFrom(payload json.RawMessage) *cratePullRequest {
	var decoded struct {
		Number     int    `json:"number"`
		URL        string `json:"url"`
		Repository string `json:"repository"`
	}
	if json.Unmarshal(payload, &decoded) != nil || (decoded.Number == 0 && decoded.URL == "") {
		return nil
	}
	return &cratePullRequest{Number: decoded.Number, URL: decoded.URL, Repository: decoded.Repository}
}

// decodeCrateFields reads a JSON object body field by field, so an edit can
// tell an omitted field from an explicit null.
func decodeCrateFields(w http.ResponseWriter, r *http.Request) (map[string]json.RawMessage, bool) {
	var fields map[string]json.RawMessage
	if err := decodeBoundedJSON(w, r, taskRouteBodyLimit, &fields); err != nil {
		writeBodyDecodeError(w, err)
		return nil, false
	}
	if fields == nil {
		fields = map[string]json.RawMessage{}
	}
	return fields, true
}

// crateStringField reads one string field. A null is accepted only when
// nullable is true and reads as a nil value with present true.
func crateStringField(fields map[string]json.RawMessage, name string, nullable bool) (*string, bool, error) {
	raw, present := fields[name]
	if !present {
		return nil, false, nil
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		if nullable {
			return nil, true, nil
		}
		return nil, true, &store.CrateFieldError{Field: name, Message: name + " must be a string"}
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, true, &store.CrateFieldError{Field: name, Message: name + " must be a string"}
	}
	return &value, true, nil
}

func crateStringListField(fields map[string]json.RawMessage, name string) ([]string, bool, error) {
	raw, present := fields[name]
	if !present {
		return nil, false, nil
	}
	var values []string
	if err := json.Unmarshal(raw, &values); err != nil || values == nil {
		return nil, true, &store.CrateFieldError{Field: name, Message: name + " must be a list of strings"}
	}
	return values, true, nil
}

// writeCrateError maps the crate refusals to the codes component-crates
// lists. Any other failure answers 500 through writeStoreFailure (DEC-105).
func writeCrateError(w http.ResponseWriter, r *http.Request, err error) {
	var field *store.CrateFieldError
	var notFound *store.CrateNotFoundError
	var closed *store.CrateClosedError
	var reference *store.TaskContextReferenceError
	var archived *store.RequirementArchivedError
	switch {
	case errors.As(err, &field):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_crate", "field": field.Field, "message": field.Message})
	case errors.As(err, &reference):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_context_reference", "requirement": reference.ID, "message": reference.Error()})
	case errors.As(err, &archived):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_context_reference", "requirement": archived.RequirementID, "message": archived.Error()})
	case errors.As(err, &notFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "crate_not_found", "crate_id": notFound.ID, "message": notFound.Error()})
	case errors.As(err, &closed):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "crate_closed", "crate_id": closed.ID, "message": closed.Error()})
	case errors.Is(err, store.ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "task_not_found", "message": "The task was not found."})
	default:
		writeStoreFailure(w, r, err)
	}
}
