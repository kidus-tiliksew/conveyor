package httpapi

// Crate briefing routes (feature-crate-insights CI-2; component-crates
// invariants 29 to 32, Public surfaces; req-crate-insights REQ-5, AC-5.2,
// AC-5.4, AC-5.6; DEC-142). GET reads the stored briefing and never calls
// the model. Only POST calls it, and it writes only the stored briefing row:
// no task, work order, gate, document, crate field, or event changes, and
// the briefing never enters work-order or agent context (DEC-142(2), (4)).

import (
	"context"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/crateinsight"
	"github.com/kidus-tiliksew/conveyor/internal/dispatch"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// crateBriefingFailureWithheld is the 502 detail when redaction is
// unavailable, so an unredacted error never leaves the server
// (component-requirement-insights invariant 17, through component-crates
// invariant 29).
const crateBriefingFailureWithheld = "crate briefing generation failed; detail withheld because redaction is unavailable"

// crateBriefingView is the briefing view of component-crates Public
// surfaces. Briefing is null when no briefing is stored. Fingerprint is the
// live fingerprint, and CanGenerate is the caller's create_tasks capability.
type crateBriefingView struct {
	Briefing    *crateBriefingStoredView `json:"briefing"`
	Fingerprint string                   `json:"fingerprint"`
	CanGenerate bool                     `json:"can_generate"`
}

// crateBriefingStoredView is the stored briefing as the page reads it.
// Current is true when the stored fingerprint equals the live one, computed
// on each read. The token counts are stored telemetry and are not served.
type crateBriefingStoredView struct {
	Headline    string                       `json:"headline"`
	Account     string                       `json:"account"`
	LookFirst   []crateinsight.BriefingEntry `json:"look_first"`
	Model       string                       `json:"model"`
	GeneratedAt time.Time                    `json:"generated_at"`
	GeneratedBy string                       `json:"generated_by"`
	Current     bool                         `json:"current"`
}

// crateBriefingState is one observation of a crate for its briefing: the
// insight state, the briefing document built from it, and the document's
// fingerprint (component-crates invariants 30 and 31).
type crateBriefingState struct {
	insight     crateInsightState
	document    []byte
	fingerprint string
}

func (s *Server) readCrateBriefingState(r *http.Request, backend crateBackend, crateID string) (crateBriefingState, error) {
	state, err := s.readCrateInsightState(r, backend, crateID)
	if err != nil {
		return crateBriefingState{}, err
	}
	document := crateinsight.BriefingDocument(state.inputs, state.actions, state.now)
	return crateBriefingState{insight: state, document: document, fingerprint: crateinsight.BriefingFingerprintOf(document)}, nil
}

// storedCrateBriefing returns the stored row, or nil when the crate has none.
func storedCrateBriefing(ctx context.Context, backend crateBackend, crateID string) (*store.CrateBriefing, error) {
	briefing, err := backend.GetCrateBriefing(ctx, crateID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &briefing, nil
}

// crateBriefingViewOf assembles the view from the live fingerprint and the
// stored row.
func crateBriefingViewOf(fingerprint string, stored *store.CrateBriefing, canGenerate bool) crateBriefingView {
	view := crateBriefingView{Fingerprint: fingerprint, CanGenerate: canGenerate}
	if stored != nil {
		view.Briefing = &crateBriefingStoredView{
			Headline: stored.Headline, Account: stored.Account, LookFirst: store.CloneCrateBriefingEntries(stored.LookFirst),
			Model: stored.Model, GeneratedAt: stored.GeneratedAt.UTC(), GeneratedBy: stored.GeneratedBy,
			Current: stored.Fingerprint == fingerprint,
		}
	}
	return view
}

// getCrateBriefing serves GET /v1/crates/{id}/briefing under view_workspace.
// It never calls the model (DEC-142(4)). current compares the stored
// fingerprint with the live one on each read, and can_generate reports the
// caller's create_tasks capability (DEC-142(6)). A missing or foreign crate
// answers 404 crate_not_found, and a failed read answers the generic 500.
func (s *Server) getCrateBriefing(w http.ResponseWriter, r *http.Request) {
	backend, ok := s.crateStore(w)
	if !ok {
		return
	}
	ctx := r.Context()
	crateID := chi.URLParam(r, "id")
	state, err := s.readCrateBriefingState(r, backend, crateID)
	if err != nil {
		writeCrateInsightFailure(w, r, err)
		return
	}
	stored, err := storedCrateBriefing(ctx, backend, crateID)
	if err != nil {
		writeStoreFailure(w, r, err)
		return
	}
	canGenerate, err := s.callerCapability(ctx, core.CapabilityCreateTasks)
	if err != nil {
		writeStoreFailure(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, crateBriefingViewOf(state.fingerprint, stored, canGenerate))
}

// generateCrateBriefing serves POST /v1/crates/{id}/briefing under
// create_tasks (component-crates invariant 29; req-crate-insights AC-5.2,
// AC-5.4; DEC-142(4), (6)). Under the workspace and crate mutex it reads the
// inputs and the stored row, so a request that waited sees its
// predecessor's result before it calls the model. A crate with no member
// answers 409 crate_empty and a stored row with the live fingerprint answers
// 409 crate_briefing_current, each before any model call. The row records
// the fingerprint of the document actually sent, so a change during
// generation reads as out of date on the next read. Every failure leaves the
// stored row unchanged and the reply is never echoed.
func (s *Server) generateCrateBriefing(w http.ResponseWriter, r *http.Request) {
	backend, ok := s.crateStore(w)
	if !ok {
		return
	}
	ctx := r.Context()
	crateID := chi.URLParam(r, "id")
	workspace, _ := store.WorkspaceFromContext(ctx)
	unlock := s.crateBriefingLocks.lock(requirementInsightKey{workspace: workspace, requirement: crateID})
	defer unlock()

	state, err := s.readCrateBriefingState(r, backend, crateID)
	if err != nil {
		writeCrateInsightFailure(w, r, err)
		return
	}
	if len(state.insight.inputs.Members) == 0 {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "crate_empty", "crate_id": crateID, "message": "The crate has no task. Add a task to the crate before you generate a briefing."})
		return
	}
	stored, err := storedCrateBriefing(ctx, backend, crateID)
	if err != nil {
		writeStoreFailure(w, r, err)
		return
	}
	if stored != nil && stored.Fingerprint == state.fingerprint {
		writeCrateBriefingCurrent(w, crateID)
		return
	}
	canGenerate, err := s.callerCapability(ctx, core.CapabilityCreateTasks)
	if err != nil {
		writeStoreFailure(w, r, err)
		return
	}
	if s.WorkOrders == nil || s.WorkOrders.Dispatcher == nil {
		writeCrateBriefingUnconfigured(w)
		return
	}
	memberIDs := make([]string, len(state.insight.inputs.Members))
	for i, member := range state.insight.inputs.Members {
		memberIDs[i] = member.Task.ID
	}
	generated, err := s.WorkOrders.Dispatcher.GenerateCrateBriefing(ctx, dispatch.CrateBriefingInput{Document: state.document, MemberIDs: memberIDs})
	if errors.Is(err, dispatch.ErrCrateBriefingUnconfigured) {
		writeCrateBriefingUnconfigured(w)
		return
	}
	if err != nil {
		detail := s.redactedFailureDetail(ctx, err, crateBriefingFailureWithheld)
		log.Printf("crate briefing generation for %s: %s", crateID, detail)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "crate_briefing_failed", "detail": detail, "message": "The briefing could not be generated. The stored briefing did not change."})
		return
	}
	recorded, err := backend.RecordCrateBriefing(ctx, store.CrateBriefing{
		CrateID:     crateID,
		Fingerprint: state.fingerprint,
		Headline:    generated.Headline,
		Account:     generated.Account,
		LookFirst:   generated.LookFirst,
		Model:       generated.Model,
		TokensIn:    generated.TokensIn,
		TokensOut:   generated.TokensOut,
		GeneratedBy: store.ActorFromContext(ctx).ID,
	})
	if errors.Is(err, store.ErrCrateBriefingCurrent) {
		writeCrateBriefingCurrent(w, crateID)
		return
	}
	if err != nil {
		writeStoreFailure(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, crateBriefingViewOf(state.fingerprint, &recorded, canGenerate))
}

func writeCrateBriefingCurrent(w http.ResponseWriter, crateID string) {
	writeJSON(w, http.StatusConflict, map[string]string{"error": "crate_briefing_current", "crate_id": crateID, "message": "The stored briefing is current. Nothing changed."})
}

func writeCrateBriefingUnconfigured(w http.ResponseWriter) {
	writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "crate_briefing_unconfigured", "message": "This server has no model for briefings. An administrator must set the triage route model."})
}
