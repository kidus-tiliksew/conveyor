package httpapi

import (
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
	var request struct {
		Title                string `json:"title"`
		RequirementContextID string `json:"requirement_context_id"`
		Model                string `json:"model"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxPlanningRequestBytes)
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	request.Title = strings.TrimSpace(request.Title)
	request.RequirementContextID = strings.TrimSpace(request.RequirementContextID)
	request.Model = strings.TrimSpace(request.Model)
	if len(request.Title) > 200 {
		http.Error(w, "planning session title must be at most 200 characters", http.StatusBadRequest)
		return
	}
	var session core.PlanningSession
	var err error
	if s.Planning != nil && s.Planning.ConfigProvider != nil {
		session, err = s.Planning.CreateSession(
			r.Context(), request.Title, request.RequirementContextID, request.Model,
		)
	} else {
		session, err = s.Store.CreatePlanningSession(r.Context(), core.PlanningSession{
			ID: "session-" + core.NewTaskID(), Title: request.Title,
			RequirementContextID: request.RequirementContextID,
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
	session, err := s.Store.AbandonPlanningSession(r.Context(), chi.URLParam(r, "id"))
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
