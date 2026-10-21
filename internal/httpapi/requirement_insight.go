package httpapi

// Requirement insight routes (feature-requirement-insights RI-6, RI-5;
// req-requirement-insights REQ-2, REQ-3, REQ-4; DEC-87; DEC-61). Every read
// composes the store's insight inputs, the shared compact next-action
// projection, and the pure internal/insight sections; no response carries a
// task body. Only the summary POST calls the model, and it writes only the
// stored headline row: no task, work order, gate, document, or event changes,
// and the headline never enters work-order or agent context (DEC-87 (2)).

import (
	"context"
	"errors"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/dispatch"
	"github.com/kidus-tiliksew/conveyor/internal/inprocess"
	"github.com/kidus-tiliksew/conveyor/internal/insight"
	"github.com/kidus-tiliksew/conveyor/internal/nextaction"
	"github.com/kidus-tiliksew/conveyor/internal/redact"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// requirementInsightSummaryView is the Summary tab's data (RI-6; AC-2.1 to
// AC-2.6). Headline is null when no headline is stored.
type requirementInsightSummaryView struct {
	Headline     *requirementInsightHeadlineView `json:"headline"`
	Fingerprint  string                          `json:"fingerprint"`
	CanGenerate  bool                            `json:"can_generate"`
	Groups       []insight.Group                 `json:"groups"`
	CriticalPath insight.CriticalPathResult      `json:"critical_path"`
	Forecast     *insight.Forecast               `json:"forecast"`
}

// requirementInsightHeadlineView is the stored headline as the panel shows
// it. Current is true when the stored fingerprint equals the current one
// (AC-2.6). Token counts are stored telemetry and are not served.
type requirementInsightHeadlineView struct {
	Text        string    `json:"text"`
	Model       string    `json:"model"`
	GeneratedAt time.Time `json:"generated_at"`
	GeneratedBy string    `json:"generated_by"`
	Current     bool      `json:"current"`
}

// requirementInsightState is one consistent observation of a requirement's
// insight inputs: the RI-2 read, its compact next actions, the one instant
// every section is computed at, and the RI-3 fingerprint of that input.
type requirementInsightState struct {
	inputs      insight.Inputs
	actions     map[string]nextaction.Compact
	now         time.Time
	fingerprint string
	document    []byte
}

// requirementInsightKey scopes generation serialization to one requirement
// in one workspace.
type requirementInsightKey struct {
	workspace   string
	requirement string
}

// requirementInsightLocks serializes headline generation per workspace and
// requirement within one conveyord process (RI-5). Its zero value is ready
// to use. Entries are reference counted and removed once no request holds
// or waits on them, so the registry does not grow with requirement count.
// Across processes the store's ErrInsightHeadlineCurrent refusal is the
// guard: a duplicate generation never overwrites a current row.
type requirementInsightLocks struct {
	mu      sync.Mutex
	entries map[requirementInsightKey]*requirementInsightLock
}

type requirementInsightLock struct {
	mu   sync.Mutex
	refs int
}

// lock acquires the key's mutex and returns its release.
func (l *requirementInsightLocks) lock(key requirementInsightKey) func() {
	l.mu.Lock()
	if l.entries == nil {
		l.entries = map[requirementInsightKey]*requirementInsightLock{}
	}
	entry := l.entries[key]
	if entry == nil {
		entry = &requirementInsightLock{}
		l.entries[key] = entry
	}
	entry.refs++
	l.mu.Unlock()
	entry.mu.Lock()
	return func() {
		entry.mu.Unlock()
		l.mu.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(l.entries, key)
		}
		l.mu.Unlock()
	}
}

// writeRequirementInsightNotFound answers an unknown, archived, or
// unconfirmed requirement (RI-6).
func writeRequirementInsightNotFound(w http.ResponseWriter) {
	writeJSON(w, http.StatusNotFound, map[string]string{"error": "requirement_not_found"})
}

func writeRequirementInsightReadError(w http.ResponseWriter, what string, err error) {
	if errors.Is(err, store.ErrNotFound) {
		writeRequirementInsightNotFound(w)
		return
	}
	log.Printf("requirement insight %s: %v", what, err)
	http.Error(w, "internal server error", http.StatusInternalServerError)
}

// readRequirementInsightState reads RI-2's inputs and projects every serving
// task's next action once through store.ReadNextActions with CompactOnly,
// as getTaskMap does, then nextaction.CompactFrom (DEC-61). The projection
// loads the workspace's real dependency view itself. The returned instant is
// the projection's own, so every section and the fingerprint describe one
// moment.
func (s *Server) readRequirementInsightState(ctx context.Context, requirementID string) (requirementInsightState, error) {
	inputs, err := s.Store.ReadRequirementInsightInputs(ctx, requirementID)
	if err != nil {
		return requirementInsightState{}, err
	}
	opts, err := s.nextActionOptionsFor(ctx)
	if err != nil {
		return requirementInsightState{}, err
	}
	opts.CompactOnly = true
	actions := make(map[string]nextaction.Compact, len(inputs.Tasks))
	if len(inputs.Tasks) > 0 {
		tasks := make([]core.Task, len(inputs.Tasks))
		for i, serving := range inputs.Tasks {
			tasks[i] = serving.Task
		}
		projected, err := store.ReadNextActions(ctx, s.Store, tasks, opts)
		if err != nil {
			return requirementInsightState{}, err
		}
		for _, task := range tasks {
			actions[task.ID] = nextaction.CompactFrom(projected[task.ID])
		}
	}
	state := requirementInsightState{inputs: inputs, actions: actions, now: opts.Now.UTC()}
	state.document = insight.FingerprintDocument(inputs, actions, state.now)
	state.fingerprint = insight.Fingerprint(inputs, actions, state.now)
	return state, nil
}

// storedRequirementInsightHeadline returns the stored row, or nil when the
// requirement has none.
func (s *Server) storedRequirementInsightHeadline(ctx context.Context, requirementID string) (*store.RequirementInsightHeadline, error) {
	headline, err := s.Store.GetRequirementInsightHeadline(ctx, requirementID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &headline, nil
}

// requirementInsightSummary assembles the summary view from one observation
// and the stored row.
func requirementInsightSummary(state requirementInsightState, stored *store.RequirementInsightHeadline, canGenerate bool) requirementInsightSummaryView {
	view := requirementInsightSummaryView{
		Fingerprint:  state.fingerprint,
		CanGenerate:  canGenerate,
		Groups:       insight.Groups(state.inputs, state.actions, state.now),
		CriticalPath: insight.CriticalPath(state.inputs, state.actions, state.now),
		Forecast:     insight.Progress(state.inputs, state.actions, state.now).Forecast,
	}
	if stored != nil {
		view.Headline = &requirementInsightHeadlineView{
			Text: stored.Headline, Model: stored.Model, GeneratedAt: stored.GeneratedAt.UTC(),
			GeneratedBy: stored.GeneratedBy, Current: stored.Fingerprint == state.fingerprint,
		}
	}
	return view
}

// getRequirementInsightSummary serves GET /v1/requirements/{id}/insights/summary
// under view_workspace. It never calls the model (DEC-87 (4)); can_generate
// reports the caller's create_tasks capability (DEC-87 (6); AC-2.5).
func (s *Server) getRequirementInsightSummary(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	requirementID := chi.URLParam(r, "id")
	state, err := s.readRequirementInsightState(ctx, requirementID)
	if err != nil {
		writeRequirementInsightReadError(w, "summary read", err)
		return
	}
	stored, err := s.storedRequirementInsightHeadline(ctx, requirementID)
	if err != nil {
		writeRequirementInsightReadError(w, "headline read", err)
		return
	}
	canGenerate, err := s.taskRunCapability(ctx, core.CapabilityCreateTasks)
	if err != nil {
		writeRequirementInsightReadError(w, "generate capability", err)
		return
	}
	writeJSON(w, http.StatusOK, requirementInsightSummary(state, stored, canGenerate))
}

// generateRequirementInsightSummary serves POST
// /v1/requirements/{id}/insights/summary under create_tasks (RI-6; AC-2.5 to
// AC-2.7; DEC-87 (4), (6)). Under the key's mutex it re-reads the inputs and
// the stored row, so a request that waited sees its predecessor's result. A
// row with the current fingerprint answers 409 without a model call. Every
// failure leaves the stored row unchanged.
func (s *Server) generateRequirementInsightSummary(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	requirementID := chi.URLParam(r, "id")
	workspace, _ := store.WorkspaceFromContext(ctx)
	unlock := s.requirementInsightLocks.lock(requirementInsightKey{workspace: workspace, requirement: requirementID})
	defer unlock()

	state, err := s.readRequirementInsightState(ctx, requirementID)
	if err != nil {
		writeRequirementInsightReadError(w, "summary read", err)
		return
	}
	stored, err := s.storedRequirementInsightHeadline(ctx, requirementID)
	if err != nil {
		writeRequirementInsightReadError(w, "headline read", err)
		return
	}
	if stored != nil && stored.Fingerprint == state.fingerprint {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "insight_headline_current"})
		return
	}
	canGenerate, err := s.taskRunCapability(ctx, core.CapabilityCreateTasks)
	if err != nil {
		writeRequirementInsightReadError(w, "generate capability", err)
		return
	}
	if s.WorkOrders == nil || s.WorkOrders.Dispatcher == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "insight_model_unconfigured"})
		return
	}
	generated, err := s.WorkOrders.Dispatcher.GenerateRequirementHeadline(ctx, dispatch.RequirementHeadlineInput{
		FingerprintDocument: state.document,
		Title:               state.inputs.Requirement.Title,
		Statements:          state.inputs.Requirement.Statements,
	})
	if errors.Is(err, dispatch.ErrHeadlineUnconfigured) {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "insight_model_unconfigured"})
		return
	}
	if err != nil {
		detail := s.requirementInsightFailureDetail(ctx, err)
		log.Printf("requirement insight generation for %s: %s", requirementID, detail)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "insight_generation_failed", "detail": detail})
		return
	}
	// The row records the fingerprint of the input actually sent, so a
	// change during generation is reported as out of date on the next read.
	recorded, err := s.Store.RecordRequirementInsightHeadline(ctx, store.RequirementInsightHeadline{
		RequirementID: requirementID,
		Fingerprint:   state.fingerprint,
		Headline:      generated.Text,
		Model:         generated.Model,
		TokensIn:      generated.TokensIn,
		TokensOut:     generated.TokensOut,
		GeneratedBy:   store.ActorFromContext(ctx).ID,
	})
	if errors.Is(err, store.ErrInsightHeadlineCurrent) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "insight_headline_current"})
		return
	}
	if err != nil {
		log.Printf("requirement insight headline write for %s: %v", requirementID, err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, requirementInsightSummary(state, &recorded, canGenerate))
}

// requirementInsightFailureDetail redacts a generation failure as the
// in-process client redacts its transcripts: the stored workspace secrets,
// the live registered secrets and credential patterns, and the provider key
// the in-process client holds. A redaction failure yields a fixed detail so
// an unredacted error never leaves the server.
func (s *Server) requirementInsightFailureDetail(ctx context.Context, err error) string {
	var secrets redact.SecretSource
	var additional []string
	if s.WorkOrders != nil {
		secrets = s.WorkOrders.RedactionSecrets
		if s.WorkOrders.Dispatcher != nil {
			if client, ok := s.WorkOrders.Dispatcher.Agent.(*inprocess.OpenAI); ok && client.APIKey != "" {
				additional = append(additional, client.APIKey)
			}
		}
	}
	redactor, redactErr := redact.WithSecrets(ctx, secrets, additional)
	if redactErr != nil {
		return "headline generation failed; detail withheld because redaction is unavailable"
	}
	detail, _ := redactor.Redact(err.Error())
	return detail
}

// getRequirementInsightProgress serves GET
// /v1/requirements/{id}/insights/progress under view_workspace: RI-3's
// daily series, forecast, weekly counts, headline numbers, and status mix
// (AC-3.1 to AC-3.5, AC-3.7).
func (s *Server) getRequirementInsightProgress(w http.ResponseWriter, r *http.Request) {
	state, err := s.readRequirementInsightState(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeRequirementInsightReadError(w, "progress read", err)
		return
	}
	writeJSON(w, http.StatusOK, insight.Progress(state.inputs, state.actions, state.now))
}

// getRequirementInsightDocuments serves GET
// /v1/requirements/{id}/insights/documents under view_workspace: RI-3's
// associations over every workspace System Design, archived ones included,
// with its current confirmed content and pending-version count, and every
// decision (AC-4.1 to AC-4.3). Designs carry attached_task_ids beside
// attached_task_count (feature-requirement-insights v2).
func (s *Server) getRequirementInsightDocuments(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	state, err := s.readRequirementInsightState(ctx, chi.URLParam(r, "id"))
	if err != nil {
		writeRequirementInsightReadError(w, "documents read", err)
		return
	}
	designs, err := s.Store.ListSystemDesigns(ctx, true)
	if err != nil {
		writeRequirementInsightReadError(w, "design read", err)
		return
	}
	versions, err := s.Store.ListSystemDesignVersionsByDocument(ctx)
	if err != nil {
		writeRequirementInsightReadError(w, "design version read", err)
		return
	}
	decisions, err := s.Store.ListDecisions(ctx)
	if err != nil {
		writeRequirementInsightReadError(w, "decision read", err)
		return
	}
	candidates := make([]insight.DesignInput, 0, len(designs))
	for _, design := range designs {
		candidate := insight.DesignInput{ID: design.ID, Title: design.Title, CurrentVersion: design.CurrentVersion}
		for _, version := range versions[design.ID] {
			if version.Version == design.CurrentVersion && version.Confirmed {
				candidate.Content = version.Content
			}
			if !version.Confirmed && !version.Dismissed {
				candidate.PendingVersionCount++
			}
		}
		candidates = append(candidates, candidate)
	}
	writeJSON(w, http.StatusOK, insight.Associations(state.inputs, state.actions, state.now, candidates, decisions))
}
