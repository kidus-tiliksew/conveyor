package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/dispatch"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// Human approvals of an enforcing task (req-risk-based-review REQ-8, REQ-9,
// AC-8.3 to AC-8.8, AC-11.2; feature-risk-based-review RR-7;
// component-http-api). Both routes run under operate_gates and the request
// credential's person; no new capability exists. An approval names the head
// it approves, and a head other than the task's reviewed head is refused with
// nothing recorded.

// approvalBodyLimit bounds the whole body of the approval routes, trailing
// bytes included. A waiver reason holds at most
// core.MaxApprovalWaiverReasonRunes runes, so 16 KiB leaves room for any
// valid request; a larger body answers 413 before any read or write
// (component-http-api invariant 17).
const approvalBodyLimit = 16 << 10

type approvalRequest struct {
	HeadSHA string
	Reason  string
}

// decodeApprovalRequest strictly decodes one JSON object holding head_sha
// and, when withReason, reason. Unknown fields, trailing JSON, and a
// non-string value are refused.
func decodeApprovalRequest(body []byte, withReason bool) (approvalRequest, error) {
	shape := "{head_sha}"
	if withReason {
		shape = "{head_sha, reason}"
	}
	var raw map[string]json.RawMessage
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(&raw); err != nil || raw == nil {
		return approvalRequest{}, fmt.Errorf("body must be one %s JSON object", shape)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return approvalRequest{}, fmt.Errorf("body must be one %s JSON object", shape)
	}
	var request approvalRequest
	for key, value := range raw {
		var target *string
		switch {
		case key == "head_sha":
			target = &request.HeadSHA
		case key == "reason" && withReason:
			target = &request.Reason
		default:
			return approvalRequest{}, fmt.Errorf("unknown field %q; the body holds only %s", key, strings.Trim(shape, "{}"))
		}
		if len(value) == 0 || value[0] != '"' || json.Unmarshal(value, target) != nil {
			return approvalRequest{}, fmt.Errorf("%s must be a string", key)
		}
	}
	if strings.TrimSpace(request.HeadSHA) == "" || strings.ContainsAny(request.HeadSHA, " \t\r\n") {
		return approvalRequest{}, errors.New("head_sha is required and names the head you approve")
	}
	if withReason {
		if err := core.ValidateApprovalWaiverReason(request.Reason); err != nil {
			return approvalRequest{}, err
		}
	}
	return request, nil
}

// readApprovalRequest reads and decodes an approval route body, answering
// 413 or 400 itself.
func readApprovalRequest(w http.ResponseWriter, r *http.Request, withReason bool) (approvalRequest, bool) {
	body, err := readBoundedBody(w, r, approvalBodyLimit)
	if errors.Is(err, errBodyTooLarge) {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "body_too_large", "message": fmt.Sprintf("the request body exceeds %d bytes", approvalBodyLimit)})
		return approvalRequest{}, false
	}
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_approval", "message": err.Error()})
		return approvalRequest{}, false
	}
	request, err := decodeApprovalRequest(body, withReason)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_approval", "message": err.Error()})
		return approvalRequest{}, false
	}
	return request, true
}

// approvalTask reads the route's task inside the request workspace.
func (s *Server) approvalTask(w http.ResponseWriter, r *http.Request) (core.Task, core.Actor, bool) {
	actor, err := store.RequireActor(r.Context())
	if err != nil {
		http.Error(w, "an authenticated person is required", http.StatusUnauthorized)
		return core.Task{}, core.Actor{}, false
	}
	task, err := s.Store.GetTask(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeTaskReadFailure(w, r, err)
		return core.Task{}, core.Actor{}, false
	}
	if workspace, scoped := store.WorkspaceFromContext(r.Context()); scoped && task.Workspace != workspace {
		http.Error(w, "task not found", http.StatusNotFound)
		return core.Task{}, core.Actor{}, false
	}
	return task, actor, true
}

// recordTaskApproval serves POST /v1/tasks/{id}/approvals: the person records
// one approval of the reviewed head. It never merges. 200 answers a count
// that is now met, and 202 a recorded approval that leaves the task short;
// both carry the task's risk read (req-risk-based-review AC-8.3, AC-8.6,
// AC-11.2; feature-risk-based-review RR-7).
func (s *Server) recordTaskApproval(w http.ResponseWriter, r *http.Request) {
	request, ok := readApprovalRequest(w, r, false)
	if !ok {
		return
	}
	task, actor, ok := s.approvalTask(w, r)
	if !ok {
		return
	}
	if s.OnRecordApproval == nil {
		http.Error(w, "approval recording is not configured", http.StatusNotImplemented)
		return
	}
	_, err := s.OnRecordApproval(r.Context(), task, actor, request.HeadSHA)
	s.writeApprovalOutcome(w, r, task.ID, err)
}

// waiveTaskAuthorApproval serves POST /v1/tasks/{id}/approvals/waive-author:
// a person who is not an author waives the trusted author's approval with a
// reason, recorded as an audited event. The waiver leaves the required
// number unchanged (req-risk-based-review AC-8.5).
func (s *Server) waiveTaskAuthorApproval(w http.ResponseWriter, r *http.Request) {
	request, ok := readApprovalRequest(w, r, true)
	if !ok {
		return
	}
	task, actor, ok := s.approvalTask(w, r)
	if !ok {
		return
	}
	if s.OnWaiveAuthorApproval == nil {
		http.Error(w, "approval recording is not configured", http.StatusNotImplemented)
		return
	}
	_, _, err := s.OnWaiveAuthorApproval(r.Context(), task, actor, request.HeadSHA, request.Reason)
	s.writeApprovalOutcome(w, r, task.ID, err)
}

// writeApprovalOutcome answers an approval act: the task's risk read with 200
// when every requirement is met, 202 with the shortfall when the task is
// still short, 409 for a refusal, and the generic 500 for a store failure.
func (s *Server) writeApprovalOutcome(w http.ResponseWriter, r *http.Request, taskID string, err error) {
	var pending *dispatch.RiskApprovalPending
	var notReady *dispatch.ErrGateMergeNotReady
	switch {
	case errors.As(err, &pending):
		s.writeRiskApprovalPending(w, r, taskID, pending)
		return
	case errors.As(err, &notReady):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "approval_not_recorded", "message": "the head is not ready to merge, so no approval was recorded: readiness " + notReady.State, "readiness": notReady.State})
		return
	case err != nil && isApprovalRefusal(err):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "approval_refused", "message": err.Error()})
		return
	case err != nil:
		writeStoreFailure(w, r, err)
		return
	}
	body, readErr := s.approvalResponse(r.Context(), taskID)
	if readErr != nil {
		writeStoreFailure(w, r, readErr)
		return
	}
	writeJSON(w, http.StatusOK, body)
}

func isApprovalRefusal(err error) bool {
	return errors.Is(err, store.ErrRefusal) || dispatch.MergeRouteRefusal(err)
}

// writeRiskApprovalPending answers an act that left an enforcing task short
// of its required approvals: 202 with the shortfall, whether the act recorded
// the person's approval, and the task's risk read. Nothing was merged
// (req-risk-based-review AC-8.7).
func (s *Server) writeRiskApprovalPending(w http.ResponseWriter, r *http.Request, taskID string, pending *dispatch.RiskApprovalPending) {
	body, err := s.approvalResponse(r.Context(), taskID)
	if err != nil {
		writeStoreFailure(w, r, err)
		return
	}
	body["error"] = "approvals_required"
	body["message"] = pending.Error()
	body["recorded"] = pending.Recorded
	body["merged"] = false
	writeJSON(w, http.StatusAccepted, body)
}

func (s *Server) approvalResponse(ctx context.Context, taskID string) (map[string]any, error) {
	task, err := s.Store.GetTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	risk, err := s.taskRisk(ctx, task)
	if err != nil {
		return nil, err
	}
	return map[string]any{"task": task, "risk": risk}, nil
}

// trustedAuthors reads the task repository's live trusted_authors list.
func (s *Server) trustedAuthors(ctx context.Context, task core.Task) ([]string, error) {
	if s.ConfigProvider == nil {
		return nil, nil
	}
	cfg, err := s.ConfigProvider(store.WithWorkspace(ctx, task.Workspace))
	if err != nil {
		return nil, fmt.Errorf("resolve trusted authors: %w", err)
	}
	if cfg == nil {
		return nil, nil
	}
	repo, _ := cfg.Repo(task.Repo)
	return repo.TrustedAuthors, nil
}

// taskApprovalEvaluation evaluates an enforcing task's approvals with the
// repository's live trusted_authors list for the task read and next action.
func (s *Server) taskApprovalEvaluation(ctx context.Context, task core.Task) (store.ApprovalEvaluation, error) {
	if !task.SetupContract.Risk.Enforcing() {
		return store.ApprovalEvaluation{}, nil
	}
	trusted, err := s.trustedAuthors(ctx, task)
	if err != nil {
		return store.ApprovalEvaluation{}, err
	}
	var st store.Store = s.Store
	if s.Memberships != nil {
		st = store.WithCapabilityAuthorizer(st, s.Memberships)
	}
	return store.EvaluateTaskApprovals(ctx, st, task, trusted, "")
}
