package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// GET /v1/tasks/{id}/history serves the task's stored events a page at a
// time under a captured boundary, and the bounded detail view presents its
// newest human events the same way (task 261015-3721c6; component-http-api
// "Task history pages", "Bounded detail view"; req-operator-next-action
// AC-6.10, AC-6.11).
const (
	boundedDetailEventLimit = 100
	taskHistoryErrorRestart = "history_restart"
	taskHistoryErrorCursor  = "history_cursor_invalid"
	taskHistoryErrorRequest = "invalid_history_request"
)

type taskHistoryBoundary struct {
	MaxID string `json:"max_id"`
	Count int    `json:"count"`
}

type taskHistoryPage struct {
	Selection   string              `json:"selection"`
	Limit       int                 `json:"limit"`
	Events      []json.RawMessage   `json:"events"`
	Boundary    taskHistoryBoundary `json:"boundary"`
	HasOlder    bool                `json:"has_older"`
	HasNewer    bool                `json:"has_newer"`
	OlderCursor string              `json:"older_cursor,omitempty"`
	NewerCursor string              `json:"newer_cursor,omitempty"`
}

// taskEventPageView describes the bounded view's presentation page.
type taskEventPageView struct {
	Limit       int    `json:"limit"`
	Returned    int    `json:"returned"`
	Kinds       string `json:"kinds"`
	HasOlder    bool   `json:"has_older"`
	OlderCursor string `json:"older_cursor,omitempty"`
}

func writeTaskHistoryError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]string{"error": code, "message": message})
}

// writeTaskEventReadError maps a window read failure. A changed history is a
// restart, never a page that silently skipped or repeated an event.
func writeTaskEventReadError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrTaskEventHistoryChanged):
		writeTaskHistoryError(w, http.StatusConflict, taskHistoryErrorRestart, "task history changed: restart from the newest page")
	case errors.Is(err, store.ErrNotFound):
		http.NotFound(w, r)
	default:
		log.Printf("handle API request: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}
}

func (s *Server) getTaskHistory(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	ctx := r.Context()
	taskID := chi.URLParam(r, "id")
	workspace, scoped := store.WorkspaceFromContext(ctx)
	task, err := s.Store.GetTask(ctx, taskID)
	if err != nil || !scoped || task.Workspace != workspace {
		http.NotFound(w, r)
		return
	}
	query := r.URL.Query()
	for _, name := range []string{"selection", "limit", "cursor"} {
		if len(query[name]) > 1 {
			writeTaskHistoryError(w, http.StatusBadRequest, taskHistoryErrorRequest, name+" may appear once")
			return
		}
	}
	selection := query.Get("selection")
	if query.Has("selection") && selection != taskHistoryTechnical && selection != taskHistoryHuman {
		writeTaskHistoryError(w, http.StatusBadRequest, taskHistoryErrorRequest, "selection must be technical or human")
		return
	}
	limit := taskHistoryMaxLimit
	if query.Has("limit") {
		limit, err = strconv.Atoi(query.Get("limit"))
		if err != nil || limit < 1 || limit > taskHistoryMaxLimit {
			writeTaskHistoryError(w, http.StatusBadRequest, taskHistoryErrorRequest, "limit must be between 1 and 50")
			return
		}
	}
	codec, err := s.historyCursorCodec()
	if err != nil {
		log.Printf("handle API request: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	credential, _ := store.CredentialFromContext(ctx)
	binding := taskHistoryBinding{workspace: workspace, taskID: taskID, credential: credential.ID}
	read := store.TaskEventWindowQuery{TaskID: taskID, Descending: true, MaxBytes: store.TaskEventWindowMaxBytes, PayloadLimit: taskEventPayloadLimit}
	direction, expires := taskHistoryOlder, codec.now().Add(taskHistoryCursorTTL)
	fromCursor := query.Has("cursor")
	if fromCursor {
		cursor, boundary, seek, err := codec.decode(query.Get("cursor"), binding)
		switch {
		case errors.Is(err, errTaskHistoryRestart):
			writeTaskHistoryError(w, http.StatusConflict, taskHistoryErrorRestart, err.Error())
			return
		case err != nil:
			writeTaskHistoryError(w, http.StatusBadRequest, taskHistoryErrorCursor, err.Error())
			return
		}
		if selection != "" && selection != cursor.Selection {
			writeTaskHistoryError(w, http.StatusBadRequest, taskHistoryErrorCursor, "history cursor belongs to another selection")
			return
		}
		if query.Has("limit") && limit != cursor.Limit {
			writeTaskHistoryError(w, http.StatusBadRequest, taskHistoryErrorRequest, "limit differs from the cursor's limit")
			return
		}
		selection, limit, direction = cursor.Selection, cursor.Limit, cursor.Direction
		expires = timeFromMicros(cursor.Expires)
		read.Boundary = &boundary
		if direction == taskHistoryOlder {
			read.Before = &seek
		} else {
			read.Descending, read.After = false, &seek
		}
	}
	if selection == "" {
		selection = taskHistoryTechnical
	}
	if selection == taskHistoryHuman {
		read.Kinds = humanTimelineEventKinds
	}
	read.Limit = limit
	window, err := s.Store.ReadTaskEventWindow(ctx, read)
	if err != nil {
		writeTaskEventReadError(w, r, err)
		return
	}
	rendered, err := s.renderTaskEventWindow(ctx, read, window, nil)
	if err != nil {
		writeTaskEventReadError(w, r, err)
		return
	}
	issue := func(seek core.Event, toward string) (string, error) {
		return codec.issue(binding, selection, window.Boundary, store.TaskEventPosition{At: seek.At, ID: seek.ID}, toward, limit, expires)
	}
	// A descending read continues from its newest end, so the page drops its
	// oldest events to fit; an ascending read drops its newest.
	newestFirst := direction == taskHistoryOlder
	build := func(n int) ([]byte, error) {
		page := taskHistoryPage{Selection: selection, Limit: limit, Events: []json.RawMessage{},
			Boundary: taskHistoryBoundary{MaxID: formatTaskEventID(window.Boundary.MaxID), Count: window.Boundary.Count}}
		dropped := len(rendered) - n
		var included []core.Event
		if newestFirst {
			included = window.Events[len(rendered)-n:]
			page.Events = append(page.Events, rendered[len(rendered)-n:]...)
			page.HasOlder, page.HasNewer = window.More || dropped > 0, fromCursor
		} else {
			included = window.Events[:n]
			page.Events = append(page.Events, rendered[:n]...)
			page.HasOlder, page.HasNewer = true, window.More || dropped > 0
		}
		if n == 0 {
			page.HasOlder, page.HasNewer = false, false
			return json.Marshal(page)
		}
		var err error
		if page.HasOlder {
			if page.OlderCursor, err = issue(included[0], taskHistoryOlder); err != nil {
				return nil, err
			}
		}
		if page.HasNewer {
			if page.NewerCursor, err = issue(included[n-1], taskHistoryNewer); err != nil {
				return nil, err
			}
		}
		return json.Marshal(page)
	}
	body, err := fitTaskEventSection(rendered, newestFirst, build)
	if err != nil {
		log.Printf("handle API request: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(append(body, '\n'))
}

// fitTaskEventSection finds the most events, counted from the end nearest the
// continuation, whose built section is at most taskEventSectionMaxBytes.
// Every presented event is at most taskEventViewMaxBytes, so one event always
// fits and traversal always progresses.
func fitTaskEventSection(rendered []json.RawMessage, newestFirst bool, build func(n int) ([]byte, error)) ([]byte, error) {
	// Estimate from event sizes and a cursor-sized envelope, then confirm.
	const envelopeReserve = 2*taskHistoryCursorMaxBytes + 512
	n, size := 0, 2
	for n < len(rendered) {
		next := rendered[n]
		if newestFirst {
			next = rendered[len(rendered)-1-n]
		}
		if size+len(next)+1 > taskEventSectionMaxBytes-envelopeReserve {
			break
		}
		size += len(next) + 1
		n++
	}
	if n == 0 && len(rendered) > 0 {
		n = 1
	}
	body, err := build(n)
	for err == nil && len(body) > taskEventSectionMaxBytes && n > 1 {
		n--
		body, err = build(n)
	}
	// Grow past the estimate while the next event still fits. build keeps
	// caller state, so the accepted size is always the last one built.
	for err == nil && len(body) <= taskEventSectionMaxBytes && n < len(rendered) {
		next, nextErr := build(n + 1)
		if nextErr != nil {
			return nil, nextErr
		}
		if len(next) > taskEventSectionMaxBytes {
			body, err = build(n)
			break
		}
		n, body = n+1, next
	}
	return body, err
}

// renderTaskEventWindow presents a window's events in ascending order. known
// maps event IDs to complete stored payloads already loaded by the caller.
func (s *Server) renderTaskEventWindow(ctx context.Context, read store.TaskEventWindowQuery, window store.TaskEventWindow, known map[int64]json.RawMessage) ([]json.RawMessage, error) {
	full, err := s.omittedTaskEventPayloads(ctx, read, window, known)
	if err != nil {
		return nil, err
	}
	rendered := make([]json.RawMessage, 0, len(window.Events))
	for _, event := range window.Events {
		view, err := renderTaskEventView(event, window.OmittedPayloadBytes[event.ID], full[event.ID])
		if err != nil {
			return nil, err
		}
		rendered = append(rendered, view)
	}
	return rendered, nil
}

// omittedTaskEventPayloads loads the stored payload of each event the window's
// PayloadLimit omitted, only to build its payload_index. It rereads the same
// selection under the same boundary from just before each omitted event, so
// the recount still refuses a changed history, and one reread usually covers
// the whole page. An event whose own source bytes exceed the window budget
// keeps no index.
func (s *Server) omittedTaskEventPayloads(ctx context.Context, read store.TaskEventWindowQuery, window store.TaskEventWindow, known map[int64]json.RawMessage) (map[int64]json.RawMessage, error) {
	if len(window.OmittedPayloadBytes) == 0 {
		return nil, nil
	}
	full := map[int64]json.RawMessage{}
	events := window.Events
	for i := 0; i < len(events); {
		event := events[i]
		if _, omitted := window.OmittedPayloadBytes[event.ID]; !omitted {
			i++
			continue
		}
		if payload, ok := known[event.ID]; ok {
			full[event.ID] = payload
			i++
			continue
		}
		reread, err := s.Store.ReadTaskEventWindow(ctx, store.TaskEventWindowQuery{
			TaskID: read.TaskID, Kind: read.Kind, Kinds: read.Kinds, Boundary: &window.Boundary,
			// IDs are integers, so (at, id-1) is immediately before the event.
			After: &store.TaskEventPosition{At: event.At, ID: event.ID - 1},
			Limit: min(len(events)-i, store.TaskEventWindowMaxEvents), MaxBytes: store.TaskEventWindowMaxBytes,
		})
		var tooLarge *store.TaskEventTooLargeError
		if errors.As(err, &tooLarge) {
			i++
			continue
		}
		if errors.Is(err, store.ErrTaskEventHistoryChanged) && known != nil {
			// The bounded view's page is already captured and its derivations
			// ran on the complete ledger; a commit after capture only costs
			// the remaining indexes, never the detail read.
			return full, nil
		}
		if err != nil {
			return nil, err
		}
		matched := 0
		for j, candidate := range reread.Events {
			if i+j >= len(events) || events[i+j].ID != candidate.ID {
				break
			}
			if _, omitted := window.OmittedPayloadBytes[candidate.ID]; omitted {
				full[candidate.ID] = candidate.Payload
			}
			matched++
		}
		i += max(matched, 1)
	}
	return full, nil
}

// boundedTaskEventSection reads the bounded view's presentation page: the
// newest human-timeline events under one captured boundary, fitted newest
// first so events plus event_page stay within 65,536 bytes.
func (s *Server) boundedTaskEventSection(r *http.Request, taskID string, complete []core.Event) ([]json.RawMessage, taskEventPageView, error) {
	ctx := r.Context()
	read := store.TaskEventWindowQuery{
		TaskID: taskID, Kinds: humanTimelineEventKinds, Descending: true, Limit: boundedDetailEventLimit,
		MaxBytes: store.TaskEventWindowMaxBytes, PayloadLimit: taskEventPayloadLimit,
	}
	window, err := s.Store.ReadTaskEventWindow(ctx, read)
	if err != nil {
		return nil, taskEventPageView{}, err
	}
	known := map[int64]json.RawMessage{}
	for _, event := range complete {
		if _, omitted := window.OmittedPayloadBytes[event.ID]; omitted {
			known[event.ID] = event.Payload
		}
	}
	rendered, err := s.renderTaskEventWindow(ctx, read, window, known)
	if err != nil {
		return nil, taskEventPageView{}, err
	}
	codec, err := s.historyCursorCodec()
	if err != nil {
		return nil, taskEventPageView{}, err
	}
	workspace, _ := store.WorkspaceFromContext(ctx)
	credential, _ := store.CredentialFromContext(ctx)
	binding := taskHistoryBinding{workspace: workspace, taskID: taskID, credential: credential.ID}
	expires := codec.now().Add(taskHistoryCursorTTL)
	var events []json.RawMessage
	var page taskEventPageView
	build := func(n int) ([]byte, error) {
		events = append([]json.RawMessage{}, rendered[len(rendered)-n:]...)
		page = taskEventPageView{Limit: boundedDetailEventLimit, Returned: n, Kinds: taskHistoryHuman, HasOlder: window.More || n < len(rendered)}
		if page.HasOlder && n > 0 {
			oldest := window.Events[len(rendered)-n]
			cursor, err := codec.issue(binding, taskHistoryHuman, window.Boundary, store.TaskEventPosition{At: oldest.At, ID: oldest.ID}, taskHistoryOlder, taskHistoryMaxLimit, expires)
			if err != nil {
				return nil, err
			}
			page.OlderCursor = cursor
		}
		eventsJSON, err := json.Marshal(events)
		if err != nil {
			return nil, err
		}
		pageJSON, err := json.Marshal(page)
		if err != nil {
			return nil, err
		}
		return append(eventsJSON, pageJSON...), nil
	}
	if _, err := fitTaskEventSection(rendered, true, build); err != nil {
		return nil, taskEventPageView{}, err
	}
	return events, page, nil
}

// boundedTaskDetailProjection is the bounded detail view: the complete view's
// fields, with events replaced by the presentation page, event_page, and
// summaries, and each transcript capture replaced by its metadata.
type boundedTaskDetailProjection struct {
	reviewItem
	events    []json.RawMessage
	eventPage taskEventPageView
	summaries taskDetailSummaries
}

func (detail boundedTaskDetailProjection) MarshalJSON() ([]byte, error) {
	orders, err := projectWorkOrderViews(detail.WorkOrders)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(struct {
		reviewItem
		Events     []json.RawMessage   `json:"events"`
		EventPage  taskEventPageView   `json:"event_page"`
		Summaries  taskDetailSummaries `json:"summaries"`
		WorkOrders []json.RawMessage   `json:"work_orders"`
	}{detail.reviewItem, detail.events, detail.eventPage, detail.summaries, orders})
	if err != nil {
		return nil, err
	}
	return projectBoundedDetailCaptures(body)
}
