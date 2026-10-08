package worker

import (
	"context"
	"fmt"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// CaptureAttempt records a parent launcher's observational capture of one
// already-ended attempt, for every stage and independently of Git
// preservation (req-260820-221be8 AC-2.1, AC-2.2; DEC-26;
// component-work-orders). The transcript is redacted and bounded to the
// newest AttemptTranscriptLimit bytes before the store sees it. When the
// redaction secret source fails the transcript is dropped rather than
// persisted unredacted; the ending is still recorded so the attempt's
// activity snapshot is superseded. No claim, lease, release, or retry state
// changes here.
func (s *Service) CaptureAttempt(ctx context.Context, claim core.WorkOrderClaimIdentity, id string, capture core.WorkOrderAttemptCapture) (core.WorkOrderAttemptCaptureResult, error) {
	claim.WorkerID = strings.TrimSpace(claim.WorkerID)
	claim.ClaimantID = strings.TrimSpace(claim.ClaimantID)
	claim.SessionID = strings.TrimSpace(claim.SessionID)
	capture.SessionID = strings.TrimSpace(capture.SessionID)
	capture.AttemptID = strings.TrimSpace(capture.AttemptID)
	capture.TerminationReason = strings.TrimSpace(capture.TerminationReason)
	if capture.SessionID == "" || capture.AttemptID == "" {
		return core.WorkOrderAttemptCaptureResult{}, fmt.Errorf("session_id and attempt_id are required")
	}
	if capture.Transcript != nil {
		content, truncated, redactErr := s.boundedObservabilityContent(ctx, capture.Transcript.Content, AttemptTranscriptLimit, capture.Transcript.Truncated)
		if redactErr != nil {
			capture.Transcript = nil
		} else {
			capture.Transcript = &core.WorkOrderAttemptTranscript{Content: content, Truncated: truncated}
		}
	}
	return s.Store.RecordWorkOrderAttemptCapture(ctx, id, claim, capture)
}

// CaptureWorkerAttempt is CaptureAttempt for the worker plane, where the
// enrolled worker is both the claim's worker and claimant.
func (s *Service) CaptureWorkerAttempt(ctx context.Context, worker core.Worker, id string, capture core.WorkOrderAttemptCapture) (core.WorkOrderAttemptCaptureResult, error) {
	return s.CaptureAttempt(ctx, core.WorkOrderClaimIdentity{
		WorkerID: worker.ID, ClaimantID: worker.ID, SessionID: strings.TrimSpace(capture.SessionID),
	}, id, capture)
}
