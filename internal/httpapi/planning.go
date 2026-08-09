package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/planning"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

const maxPlanningRequestBytes = 1 << 20

func (s *Server) listPlanningSessions(w http.ResponseWriter, r *http.Request) {
	sessions, err := s.Store.ListPlanningSessions(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if sessions == nil {
		sessions = []core.PlanningSession{}
	}
	writeJSON(w, http.StatusOK, sessions)
}

func (s *Server) createPlanningSession(w http.ResponseWriter, r *http.Request) {
	// No title is accepted: the session is named by its goal and then by the
	// artifact it produces.
	var request struct {
		RequirementContextID  string `json:"requirement_context_id"`
		SystemDesignContextID string `json:"system_design_context_id"`
		Model                 string `json:"model"`
		// Goal is accepted once at creation and never updated.
		Goal      string                      `json:"goal"`
		Promotion *core.RequirementDerivation `json:"promotion"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxPlanningRequestBytes)
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	request.RequirementContextID = strings.TrimSpace(request.RequirementContextID)
	request.SystemDesignContextID = strings.TrimSpace(request.SystemDesignContextID)
	request.Model = strings.TrimSpace(request.Model)
	goal, err := core.NormalizePlanningSessionGoal(
		core.PlanningSessionGoal(strings.TrimSpace(request.Goal)))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if goal == core.PlanningGoalBlueprint {
		http.Error(w, "blueprint planning is historical; use a bundle goal", http.StatusBadRequest)
		return
	}
	var session core.PlanningSession
	if s.Planning != nil && s.Planning.ConfigProvider != nil {
		session, err = s.Planning.CreateSession(r.Context(), planning.CreateSessionInput{
			RequirementContextID:  request.RequirementContextID,
			SystemDesignContextID: request.SystemDesignContextID,
			ModelOverride:         request.Model,
			Goal:                  goal,
			Promotion:             request.Promotion,
		})
	} else {
		if request.Promotion != nil {
			if goal != core.PlanningGoalRequirement {
				err = fmt.Errorf("promotion requires a requirement goal")
			} else {
				err = (&planning.Service{Store: s.Store}).ValidatePromotionSource(r.Context(), request.Promotion)
			}
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
		}
		session, err = s.Store.CreatePlanningSession(r.Context(), core.PlanningSession{
			ID: "session-" + core.NewTaskID(), Title: goal.ProvisionalTitle(), Goal: goal,
			RequirementContextID:  request.RequirementContextID,
			SystemDesignContextID: request.SystemDesignContextID,
			Promotion:             request.Promotion,
		})
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusCreated, session)
}

func (s *Server) getPlanningSession(w http.ResponseWriter, r *http.Request) {
	session, err := s.Store.GetPlanningSession(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, session)
}

func (s *Server) listPlanningBundles(w http.ResponseWriter, r *http.Request) {
	bundles, err := s.Store.ListPlanningBundles(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if bundles == nil {
		bundles = []core.PlanningBundle{}
	}
	writeJSON(w, http.StatusOK, bundles)
}

func (s *Server) getPlanningBundle(w http.ResponseWriter, r *http.Request) {
	bundle, err := s.Store.GetPlanningBundle(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, bundle)
}

func (s *Server) approvePlanningBundle(w http.ResponseWriter, r *http.Request) {
	prior, err := s.Store.GetPlanningBundle(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writePlanningBundleError(w, err)
		return
	}
	bundle, err := s.Store.ApprovePlanningBundle(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writePlanningBundleError(w, err)
		return
	}
	notify := false
	if prior.Status == core.PlanningBundlePending && bundle.Status == core.PlanningBundleApproved && s.OnCreate != nil {
		key := bundle.Workspace + "\x00" + bundle.ID
		s.planningBundleMu.Lock()
		if s.planningBundleDispatched == nil {
			s.planningBundleDispatched = map[string]struct{}{}
		}
		_, alreadyDispatched := s.planningBundleDispatched[key]
		if !alreadyDispatched {
			s.planningBundleDispatched[key] = struct{}{}
			notify = true
		}
		s.planningBundleMu.Unlock()
	}
	if notify {
		for _, member := range bundle.Tasks {
			s.OnCreate(r.Context(), member.CreatedTaskID)
		}
	}
	writeJSON(w, http.StatusOK, bundle)
}

func (s *Server) rejectPlanningBundle(w http.ResponseWriter, r *http.Request) {
	bundle, err := s.Store.RejectPlanningBundle(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writePlanningBundleError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, bundle)
}

func writePlanningBundleError(w http.ResponseWriter, err error) {
	var referenceErr *store.TaskContextReferenceError
	var conflictErr *store.PlanningBundleConflictError
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "planning_bundle_not_found", "message": "planning bundle was not found"})
	case errors.As(err, &referenceErr):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_bundle_context", "message": referenceErr.Error()})
	case errors.As(err, &conflictErr):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "planning_bundle_conflict", "message": conflictErr.Error()})
	default:
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "planning_bundle_failed", "message": "planning bundle operation failed"})
	}
}

func (s *Server) listPlanningMessages(w http.ResponseWriter, r *http.Request) {
	if _, err := s.Store.GetPlanningSession(r.Context(), chi.URLParam(r, "id")); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	messages, err := s.Store.ListPlanningMessages(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if messages == nil {
		messages = []core.PlanningMessage{}
	}
	writeJSON(w, http.StatusOK, messages)
}

func (s *Server) abandonPlanningSession(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Reason string `json:"reason"`
	}
	if r.Body != nil && r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	session, err := s.Store.AbandonPlanningSession(r.Context(), chi.URLParam(r, "id"), request.Reason)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, http.StatusOK, session)
}

func (s *Server) streamPlanningMessage(w http.ResponseWriter, r *http.Request) {
	if s.Planning == nil {
		http.Error(w, "planning service unavailable", http.StatusServiceUnavailable)
		return
	}
	sessionID := chi.URLParam(r, "id")
	if _, err := s.Store.GetPlanningSession(r.Context(), sessionID); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxPlanningRequestBytes)
	message, err := decodePlanningUserMessage(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err = s.validatePlanningAttachments(r.Context(), sessionID, message.Parts); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("X-Vercel-AI-UI-Message-Stream", "v1")
	started := false
	emit := func(part map[string]any) error {
		data, marshalErr := json.Marshal(part)
		if marshalErr != nil {
			return marshalErr
		}
		if _, writeErr := fmt.Fprintf(w, "data: %s\n\n", data); writeErr != nil {
			return writeErr
		}
		started = true
		flusher.Flush()
		return nil
	}
	if err = s.Planning.Run(r.Context(), sessionID, message, emit); err != nil {
		if !started && errors.Is(err, store.ErrPlanningSessionRunConflict) {
			clearPlanningStreamHeaders(w.Header())
			http.Error(w, "planning session already has a message in progress", http.StatusConflict)
			return
		}
		log.Printf("planning session %s run failed: %v", sessionID, err)
		if !started {
			clearPlanningStreamHeaders(w.Header())
			http.Error(w, "planning request failed", http.StatusInternalServerError)
			return
		}
		_ = emit(map[string]any{"type": "error", "errorText": "Planning request failed. Please retry."})
	}
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func (s *Server) validatePlanningAttachments(ctx context.Context, sessionID string, raw json.RawMessage) error {
	if len(raw) == 0 {
		return nil
	}
	var parts []struct {
		Type       string `json:"type"`
		ArtifactID string `json:"artifactId"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return fmt.Errorf("message parts: %w", err)
	}
	for _, part := range parts {
		if part.Type != "file" && part.Type != "attachment" {
			continue
		}
		if strings.TrimSpace(part.ArtifactID) == "" {
			return fmt.Errorf("planning file part requires an artifactId")
		}
		if _, _, err := s.Store.GetArtifactForPlanningSession(ctx, part.ArtifactID, sessionID); err != nil {
			return fmt.Errorf("artifact %s is not owned by planning session %s", part.ArtifactID, sessionID)
		}
	}
	return nil
}

func clearPlanningStreamHeaders(header http.Header) {
	for _, name := range []string{
		"Content-Type", "Cache-Control", "Connection", "X-Accel-Buffering", "X-Vercel-AI-UI-Message-Stream",
	} {
		header.Del(name)
	}
}

type planningUIMessage struct {
	Role    string          `json:"role"`
	Content string          `json:"content"`
	Parts   json.RawMessage `json:"parts"`
}

func decodePlanningUserMessage(reader io.Reader) (planning.UserMessage, error) {
	var request struct {
		Content  string              `json:"content"`
		Message  planningUIMessage   `json:"message"`
		Messages []planningUIMessage `json:"messages"`
	}
	decoder := json.NewDecoder(reader)
	if err := decoder.Decode(&request); err != nil {
		return planning.UserMessage{}, err
	}
	candidate := request.Message
	if request.Content != "" {
		candidate = planningUIMessage{Role: "user", Content: request.Content}
	}
	if candidate.Content == "" && len(candidate.Parts) == 0 {
		for index := len(request.Messages) - 1; index >= 0; index-- {
			if request.Messages[index].Role == "user" {
				candidate = request.Messages[index]
				break
			}
		}
	}
	if candidate.Role != "" && candidate.Role != "user" {
		return planning.UserMessage{}, fmt.Errorf("planning chat requires a user message")
	}
	content := strings.TrimSpace(candidate.Content)
	if content == "" && len(candidate.Parts) != 0 {
		var parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(candidate.Parts, &parts); err != nil {
			return planning.UserMessage{}, fmt.Errorf("message parts: %w", err)
		}
		var text []string
		for _, part := range parts {
			if part.Type == "text" && strings.TrimSpace(part.Text) != "" {
				text = append(text, part.Text)
			}
		}
		content = strings.TrimSpace(strings.Join(text, "\n"))
	}
	if content == "" {
		return planning.UserMessage{}, fmt.Errorf("planning chat requires non-empty text content")
	}
	return planning.UserMessage{Content: content, Parts: candidate.Parts}, nil
}
