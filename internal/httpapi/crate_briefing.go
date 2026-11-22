package httpapi

// Crate briefing routes (feature-crate-insights CI-2; component-crates
// invariants 29 to 32 and 36, Public surfaces; req-crate-insights REQ-5,
// AC-5.2, AC-5.4, AC-5.6; DEC-144). GET reads the stored briefing and never
// calls the model. Only POST and the start after a join call it, and each
// writes only the stored briefing row:
// no task, work order, gate, document, crate field, or event changes, and
// the briefing never enters work-order or agent context (DEC-142(2), (4)).

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
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

func (s *Server) readCrateBriefingState(ctx context.Context, backend crateBackend, crateID string) (crateBriefingState, error) {
	state, err := s.readCrateInsightState(ctx, backend, crateID)
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
	state, err := s.readCrateBriefingState(ctx, backend, crateID)
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
// AC-5.4; DEC-144(4), (6)). runCrateBriefing does the work under the
// workspace and crate mutex. A crate with no member answers 409 crate_empty
// and a stored row with the live fingerprint answers 409
// crate_briefing_current, each before any model call. Every failure leaves
// the stored row unchanged and the reply is never echoed.
func (s *Server) generateCrateBriefing(w http.ResponseWriter, r *http.Request) {
	backend, ok := s.crateStore(w)
	if !ok {
		return
	}
	ctx := r.Context()
	crateID := chi.URLParam(r, "id")
	var canGenerate bool
	result, err := s.runCrateBriefing(ctx, backend, crateID, false, func() (err error) {
		canGenerate, err = s.callerCapability(ctx, core.CapabilityCreateTasks)
		return err
	})
	if err != nil {
		writeCrateInsightFailure(w, r, err)
		return
	}
	switch result.outcome {
	case crateBriefingEmpty:
		writeJSON(w, http.StatusConflict, map[string]string{"error": "crate_empty", "crate_id": crateID, "message": "The crate has no task. Add a task to the crate before you generate a briefing."})
	case crateBriefingCurrent:
		writeCrateBriefingCurrent(w, crateID)
	case crateBriefingUnconfigured:
		writeCrateBriefingUnconfigured(w)
	case crateBriefingFailed:
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "crate_briefing_failed", "detail": result.detail, "message": "The briefing could not be generated. The stored briefing did not change."})
	case crateBriefingStored:
		writeJSON(w, http.StatusOK, crateBriefingViewOf(result.fingerprint, &result.recorded, canGenerate))
	default:
		// Only the missing-only mode of a start after a join skips.
		writeStoreFailure(w, r, fmt.Errorf("crate briefing outcome %d is not served", result.outcome))
	}
}

// crateBriefingOutcome is how one generation attempt ended.
type crateBriefingOutcome int

const (
	// crateBriefingStored: the model answered and the row was written.
	crateBriefingStored crateBriefingOutcome = iota
	// crateBriefingEmpty: the crate has no member. No model call.
	crateBriefingEmpty
	// crateBriefingCurrent: the stored row has the live fingerprint, or a
	// concurrent writer stored that fingerprint first.
	crateBriefingCurrent
	// crateBriefingUnconfigured: no model is configured. No model call.
	crateBriefingUnconfigured
	// crateBriefingFailed: the model call or its reply failed. Nothing
	// was stored, and detail holds the redacted cause.
	crateBriefingFailed
	// crateBriefingExists: missing-only mode found a stored row. No model
	// call.
	crateBriefingExists
	// crateBriefingClosed: missing-only mode found the crate closed. No
	// model call.
	crateBriefingClosed
)

// crateBriefingResult is one generation attempt's outcome, the live
// fingerprint it read, the row it stored, and a failure's redacted detail.
type crateBriefingResult struct {
	outcome     crateBriefingOutcome
	fingerprint string
	recorded    store.CrateBriefing
	detail      string
}

// runCrateBriefing is the generation core that POST and the start after a
// join share (component-crates invariants 29 and 36). Under the workspace and
// crate mutex it reads the inputs and the stored row, so a caller that waited
// sees its predecessor's result before it calls the model. The row records
// the fingerprint of the document actually sent, so a change during
// generation reads as out of date on the next read, and the compare-and-write
// leaves one winner across processes. generated_by is the context's actor.
//
// missingOnly is the mode of the start after a join: it first checks for a
// stored row and skips when one exists, even when it is out of date, and it
// skips a closed crate. Both checks run under the mutex before any input read
// or model call. beforeModel, when set, runs after the empty and current
// checks and before the configuration check; POST reads the caller's
// capability there. A returned error is an input, store, projection, or
// beforeModel failure. A model failure is the crateBriefingFailed outcome,
// logged here with its redacted detail.
func (s *Server) runCrateBriefing(ctx context.Context, backend crateBackend, crateID string, missingOnly bool, beforeModel func() error) (crateBriefingResult, error) {
	workspace, _ := store.WorkspaceFromContext(ctx)
	unlock := s.crateBriefingLocks.lock(requirementInsightKey{workspace: workspace, requirement: crateID})
	defer unlock()

	if missingOnly {
		stored, err := storedCrateBriefing(ctx, backend, crateID)
		if err != nil {
			return crateBriefingResult{}, err
		}
		if stored != nil {
			return crateBriefingResult{outcome: crateBriefingExists}, nil
		}
	}
	state, err := s.readCrateBriefingState(ctx, backend, crateID)
	if err != nil {
		return crateBriefingResult{}, err
	}
	result := crateBriefingResult{fingerprint: state.fingerprint}
	if missingOnly && state.insight.inputs.Crate.State != string(store.CrateOpen) {
		result.outcome = crateBriefingClosed
		return result, nil
	}
	if len(state.insight.inputs.Members) == 0 {
		result.outcome = crateBriefingEmpty
		return result, nil
	}
	if !missingOnly {
		stored, err := storedCrateBriefing(ctx, backend, crateID)
		if err != nil {
			return crateBriefingResult{}, err
		}
		if stored != nil && stored.Fingerprint == state.fingerprint {
			result.outcome = crateBriefingCurrent
			return result, nil
		}
	}
	if beforeModel != nil {
		if err := beforeModel(); err != nil {
			return crateBriefingResult{}, err
		}
	}
	if s.WorkOrders == nil || s.WorkOrders.Dispatcher == nil {
		result.outcome = crateBriefingUnconfigured
		return result, nil
	}
	memberIDs := make([]string, len(state.insight.inputs.Members))
	for i, member := range state.insight.inputs.Members {
		memberIDs[i] = member.Task.ID
	}
	generated, err := s.WorkOrders.Dispatcher.GenerateCrateBriefing(ctx, dispatch.CrateBriefingInput{Document: state.document, MemberIDs: memberIDs})
	if errors.Is(err, dispatch.ErrCrateBriefingUnconfigured) {
		result.outcome = crateBriefingUnconfigured
		return result, nil
	}
	if err != nil {
		result.outcome = crateBriefingFailed
		result.detail = s.redactedFailureDetail(ctx, err, crateBriefingFailureWithheld)
		log.Printf("crate briefing generation for %s: %s", crateID, result.detail)
		return result, nil
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
		result.outcome = crateBriefingCurrent
		return result, nil
	}
	if err != nil {
		return crateBriefingResult{}, err
	}
	result.outcome, result.recorded = crateBriefingStored, recorded
	return result, nil
}

// crateBriefingJoinTimeout bounds one background generation after a join:
// its checks before the start, its reads, its model call, and its write. The model call keeps the triage
// route's own timeout inside this bound.
const crateBriefingJoinTimeout = 30 * time.Minute

// startCrateBriefingAfterJoin starts one background generation after the
// transaction that put a task in crateID committed (component-crates
// invariant 36; req-crate-insights AC-5.2; DEC-144(4)(a), (6)). Intake calls
// it only for a task it created, never for a replayed create, and the
// membership route calls it only when its write changed the task's crate, so
// a same-crate repeat, a removal, and a refused join start nothing.
//
// The checks here run before the response and start nothing when the crate
// ID is empty, the store keeps no crates, no model is configured, the acting
// member does not hold create_tasks, or the crate has a stored briefing. A
// failed check is logged and starts nothing. Only the authenticated
// workspace, actor, and credential are read from the request context. They
// are copied first into a new context.Background() child bounded by
// crateBriefingJoinTimeout, and the checks and the background work all run
// on that context. A request cancelled after the commit therefore cannot
// fail the checks, and the request context is never read after the
// response. The background work runs runCrateBriefing in missing-only mode
// with the acting member as generated_by. A failure stores nothing, is
// logged with the redaction of invariant 29, and schedules no retry.
func (s *Server) startCrateBriefingAfterJoin(ctx context.Context, crateID string) {
	crateID = strings.TrimSpace(crateID)
	if crateID == "" {
		return
	}
	backend, ok := s.Store.(crateBackend)
	if !ok || s.WorkOrders == nil || s.WorkOrders.Dispatcher == nil {
		return
	}
	detached := store.WithActor(context.Background(), store.ActorFromContext(ctx))
	if workspace, scoped := store.WorkspaceFromContext(ctx); scoped {
		detached = store.WithWorkspace(detached, workspace)
	}
	if credential, ok := store.CredentialFromContext(ctx); ok {
		detached = store.WithCredential(detached, credential)
	}
	jobCtx, cancel := context.WithTimeout(detached, crateBriefingJoinTimeout)
	started := false
	defer func() {
		if !started {
			cancel()
		}
	}()
	allowed, err := s.callerCapability(jobCtx, core.CapabilityCreateTasks)
	if err != nil {
		log.Printf("crate briefing after a join for %s: capability read failed: %s", crateID, s.redactedFailureDetail(jobCtx, err, crateBriefingFailureWithheld))
		return
	}
	if !allowed {
		return
	}
	if err := s.WorkOrders.Dispatcher.CrateBriefingConfigured(jobCtx); err != nil {
		if !errors.Is(err, dispatch.ErrCrateBriefingUnconfigured) {
			log.Printf("crate briefing after a join for %s: configuration read failed: %s", crateID, s.redactedFailureDetail(jobCtx, err, crateBriefingFailureWithheld))
		}
		return
	}
	// A crate with a stored briefing starts nothing. The background work
	// checks again under the mutex, because another request can store one
	// between this read and the start.
	if stored, err := storedCrateBriefing(jobCtx, backend, crateID); err != nil {
		log.Printf("crate briefing after a join for %s: briefing read failed: %s", crateID, s.redactedFailureDetail(jobCtx, err, crateBriefingFailureWithheld))
		return
	} else if stored != nil {
		return
	}
	started = true
	s.crateBriefingJobs.Add(1)
	s.crateBriefingJobsStarted.Add(1)
	go func() {
		defer s.crateBriefingJobs.Done()
		defer cancel()
		if _, err := s.runCrateBriefing(jobCtx, backend, crateID, true, nil); err != nil {
			log.Printf("crate briefing after a join for %s: %s", crateID, s.redactedFailureDetail(jobCtx, err, crateBriefingFailureWithheld))
		}
	}()
}

func writeCrateBriefingCurrent(w http.ResponseWriter, crateID string) {
	writeJSON(w, http.StatusConflict, map[string]string{"error": "crate_briefing_current", "crate_id": crateID, "message": "The stored briefing is current. Nothing changed."})
}

func writeCrateBriefingUnconfigured(w http.ResponseWriter) {
	writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "crate_briefing_unconfigured", "message": "This server has no model for briefings. An administrator must set the triage route model."})
}
