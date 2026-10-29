package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// transcriptCaptureMetadataView is one capture as the bounded detail presents
// it: metadata and the stored content's UTF-8 length, never the content
// (component-http-api, Bounded detail view; req-260820-221be8 AC-2.4).
type transcriptCaptureMetadataView struct {
	AttemptID         string    `json:"attempt_id"`
	TerminationReason string    `json:"termination_reason"`
	Truncated         bool      `json:"truncated"`
	CapturedAt        time.Time `json:"captured_at"`
	ContentBytes      int       `json:"content_bytes"`
}

// transcriptCaptureContentView is one capture as the content read returns it.
type transcriptCaptureContentView struct {
	AttemptID         string    `json:"attempt_id"`
	Content           string    `json:"content"`
	TerminationReason string    `json:"termination_reason"`
	Truncated         bool      `json:"truncated"`
	CapturedAt        time.Time `json:"captured_at"`
	ContentBytes      int       `json:"content_bytes"`
}

// boundedTranscriptCaptureViews builds fresh metadata values in the store's
// capture order. It never mutates the store's slice, and the core JSON tags
// stay unchanged, so the complete view and MCP keep their content.
func boundedTranscriptCaptureViews(captures []core.WorkOrderTranscriptCapture) []transcriptCaptureMetadataView {
	views := make([]transcriptCaptureMetadataView, 0, len(captures))
	for _, capture := range captures {
		views = append(views, transcriptCaptureMetadataView{
			AttemptID:         capture.AttemptID,
			TerminationReason: capture.TerminationReason,
			Truncated:         capture.Truncated,
			CapturedAt:        capture.CapturedAt,
			ContentBytes:      len(capture.Content),
		})
	}
	return views
}

// projectBoundedTranscriptCaptures replaces every transcript_captures array
// in one bounded-detail field with metadata views. Work orders appear in
// work_orders and again inside stalled and review-recovery states, so the walk
// covers each copy. Like omitAuditSnapshots it decodes only containers and
// leaves every other value's bytes unchanged.
func projectBoundedTranscriptCaptures(data json.RawMessage) (json.RawMessage, bool, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return data, false, nil
	}
	changed := false
	switch trimmed[0] {
	case '{':
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(data, &fields); err != nil {
			return nil, false, err
		}
		for key, value := range fields {
			if key == "transcript_captures" {
				var captures []core.WorkOrderTranscriptCapture
				if err := json.Unmarshal(value, &captures); err != nil {
					return nil, false, err
				}
				views, err := json.Marshal(boundedTranscriptCaptureViews(captures))
				if err != nil {
					return nil, false, err
				}
				fields[key], changed = views, true
				continue
			}
			child, childChanged, err := projectBoundedTranscriptCaptures(value)
			if err != nil {
				return nil, false, err
			}
			fields[key] = child
			changed = changed || childChanged
		}
		if changed {
			result, err := json.Marshal(fields)
			return result, true, err
		}
	case '[':
		var values []json.RawMessage
		if err := json.Unmarshal(data, &values); err != nil {
			return nil, false, err
		}
		for i, value := range values {
			child, childChanged, err := projectBoundedTranscriptCaptures(value)
			if err != nil {
				return nil, false, err
			}
			values[i] = child
			changed = changed || childChanged
		}
		if changed {
			result, err := json.Marshal(values)
			return result, true, err
		}
	}
	return data, false, nil
}

// boundedDetailEventFields hold the presentation page and its derived facts,
// never a work order, so the capture walk skips them.
var boundedDetailEventFields = map[string]bool{"events": true, "event_page": true, "summaries": true}

// projectBoundedDetailCaptures applies the capture projection to every
// top-level field of a serialized bounded detail except the event fields.
func projectBoundedDetailCaptures(body []byte) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, err
	}
	changed := false
	for key, value := range fields {
		if boundedDetailEventFields[key] {
			continue
		}
		projected, fieldChanged, err := projectBoundedTranscriptCaptures(value)
		if err != nil {
			return nil, err
		}
		fields[key] = projected
		changed = changed || fieldChanged
	}
	if !changed {
		return body, nil
	}
	return json.Marshal(fields)
}

// getTaskTranscriptCapture serves one capture's content. It shares the human
// workspace read boundary with task detail, confirms the task is in the
// resolved workspace and the order belongs to the task before any capture
// read, and reads only that order's captures. Every absent or foreign record
// answers the same 404 without content, and transcript text is never logged
// (component-http-api, Transcript capture content read; DEC-19).
func (s *Server) getTaskTranscriptCapture(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	ctx := r.Context()
	taskID, orderID, attemptID := chi.URLParam(r, "id"), chi.URLParam(r, "order_id"), chi.URLParam(r, "attempt_id")
	workspace, scoped := store.WorkspaceFromContext(ctx)
	task, err := s.Store.GetTask(ctx, taskID)
	if errors.Is(err, store.ErrNotFound) || err == nil && (!scoped || task.Workspace != workspace) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		log.Printf("transcript capture read: task %s: %v", taskID, err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	order, err := s.Store.GetWorkOrder(ctx, orderID)
	if errors.Is(err, store.ErrNotFound) || err == nil && order.TaskID != task.ID {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		log.Printf("transcript capture read: work order %s: %v", orderID, err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	captures, err := s.Store.ListWorkOrderTranscriptCaptures(ctx, order.ID)
	if err != nil {
		log.Printf("transcript capture read: captures of work order %s: %v", order.ID, err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	for _, capture := range captures {
		if capture.AttemptID == attemptID {
			writeJSON(w, http.StatusOK, transcriptCaptureContentView{
				AttemptID:         capture.AttemptID,
				Content:           capture.Content,
				TerminationReason: capture.TerminationReason,
				Truncated:         capture.Truncated,
				CapturedAt:        capture.CapturedAt,
				ContentBytes:      len(capture.Content),
			})
			return
		}
	}
	http.NotFound(w, r)
}
