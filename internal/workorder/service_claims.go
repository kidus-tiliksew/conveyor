package workorder

// Claim lifecycle for MCP sessions: renewal with the session's chosen lease,
// release with checkpoint normalization, and plan-revision requests. Every
// work order is claimed by an agent session over MCP (DEC-97(1));
// component-work-orders and component-mcp-protocol describe this contract.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// Release retry defaults apply when the daemon configures no retry delays.
const (
	DefaultReleaseRetryDelay   = time.Second
	DefaultReleaseRetryMaximum = 4 * time.Second
	DefaultReleaseRetryLimit   = 3
)

// RenewClaimWithLease renews the exact claim for the given lease duration. The
// store caps the renewed expiry at the fixed execution deadline, so renewal
// never extends it (req-delegated-execution AC-1.1; req-agent-skills AC-2.2).
func (s *Service) RenewClaimWithLease(ctx context.Context, claim core.WorkOrderClaimIdentity, id string, lease time.Duration) (core.WorkOrder, error) {
	sessionID := claim.SessionID
	if strings.TrimSpace(sessionID) == "" {
		return core.WorkOrder{}, fmt.Errorf("session_id is required")
	}
	order, err := s.Store.GetWorkOrder(ctx, id)
	if err != nil {
		return core.WorkOrder{}, err
	}
	renewed, err := taskops.ExecuteWorkOrder(ctx, s.Store, order.TaskID, core.WorkOrderCmdRenew, func(taskLease taskops.TaskLease) (core.WorkOrder, error) {
		return s.Store.RenewWorkerClaimCommand(ctx, taskLease, id, claim, lease)
	})
	if err != nil {
		return core.WorkOrder{}, err
	}
	return renewed, nil
}

// ReleaseClaim releases the exact authenticated claim that MCP
// release_work_order resolved through claim-bound authorization
// (component-work-orders, Release and automatic retry).
func (s *Service) ReleaseClaim(ctx context.Context, claim core.WorkOrderClaimIdentity, id string, release core.WorkOrderRelease) (core.WorkOrder, error) {
	if strings.TrimSpace(release.SessionID) == "" {
		return core.WorkOrder{}, fmt.Errorf("session_id is required")
	}
	if claim.SessionID != release.SessionID {
		return core.WorkOrder{}, store.ErrWorkOrderClaimLost
	}
	release.Reason = strings.TrimSpace(release.Reason)
	checkpoint, err := s.normalizeCheckpoint(ctx, release.Reason, release.Checkpoint)
	if err != nil {
		return core.WorkOrder{}, err
	}
	release.Checkpoint = checkpoint
	release.Cause = strings.TrimSpace(release.Cause)
	if release.Cause == "" {
		release.Cause = core.WorkOrderReleaseCauseSessionExit
	}
	if !core.ValidWorkOrderReleaseCause(release.Cause) {
		return core.WorkOrder{}, fmt.Errorf("invalid work-order release cause %q", release.Cause)
	}
	release.FailureDetail = boundedFailureDetail(release.FailureDetail)
	if release.Outcome == "" {
		release.Outcome = core.WorkOrderOutcomeReleased
	}
	if release.Outcome != core.WorkOrderOutcomeChildFailure && release.Outcome != core.WorkOrderOutcomeStalled && release.Outcome != core.WorkOrderOutcomeReleased && release.Outcome != core.WorkOrderOutcomeCancelled {
		return core.WorkOrder{}, fmt.Errorf("invalid work-order release outcome %q", release.Outcome)
	}
	if release.Outcome == core.WorkOrderOutcomeChildFailure && release.FailureCategory == "" {
		switch {
		case providerUsageLimit(release.FailureDetail):
			release.FailureCategory = core.WorkOrderFailureProviderUsageLimit
		case transientConnectivityFailure(release.FailureDetail):
			release.FailureCategory = core.WorkOrderFailureTransientConnectivity
		}
	}
	release.InitialRetryDelay = s.RetryDelay
	if release.InitialRetryDelay <= 0 {
		release.InitialRetryDelay = DefaultReleaseRetryDelay
	}
	release.MaximumRetryDelay = s.RetryMaximum
	if release.MaximumRetryDelay <= 0 {
		release.MaximumRetryDelay = DefaultReleaseRetryMaximum
	}
	if release.MaximumRetryDelay < release.InitialRetryDelay {
		release.MaximumRetryDelay = release.InitialRetryDelay
	}
	release.AutomaticRetryLimit = s.RetryLimit
	if release.AutomaticRetryLimit <= 0 {
		release.AutomaticRetryLimit = DefaultReleaseRetryLimit
	}
	current, err := s.Store.GetWorkOrder(ctx, id)
	if err != nil {
		return core.WorkOrder{}, err
	}
	order, err := taskops.ExecuteWorkOrder(ctx, s.Store, current.TaskID, core.WorkOrderCmdRelease, func(taskLease taskops.TaskLease) (core.WorkOrder, error) {
		return s.Store.ReleaseWorkerClaimCommand(ctx, taskLease, id, claim, release)
	})
	if err != nil {
		return core.WorkOrder{}, err
	}
	return order, nil
}

func (s *Service) normalizeCheckpoint(ctx context.Context, reason string, checkpoint *core.WorkOrderCheckpoint) (*core.WorkOrderCheckpoint, error) {
	if checkpoint == nil {
		if reason == core.WorkOrderReleaseReasonOperatorCheckpointReached {
			return nil, fmt.Errorf("checkpoint.decision_request is required when reason is %q", core.WorkOrderReleaseReasonOperatorCheckpointReached)
		}
		return nil, nil
	}
	normalized := &core.WorkOrderCheckpoint{
		DecisionRequest: strings.TrimSpace(checkpoint.DecisionRequest),
		Class:           strings.TrimSpace(checkpoint.Class),
	}
	if utf8.RuneCountInString(normalized.DecisionRequest) > core.WorkOrderDecisionRequestLimit {
		return nil, fmt.Errorf("checkpoint.decision_request exceeds %d Unicode characters (runes)", core.WorkOrderDecisionRequestLimit)
	}
	if reason == core.WorkOrderReleaseReasonOperatorCheckpointReached && normalized.DecisionRequest == "" {
		return nil, fmt.Errorf("checkpoint.decision_request is required when reason is %q", core.WorkOrderReleaseReasonOperatorCheckpointReached)
	}
	if normalized.Class != "" && normalized.Class != core.WorkOrderCheckpointClassAuthorityConflict {
		return nil, fmt.Errorf("checkpoint.class %q is not recognized; allowed value is %q", normalized.Class, core.WorkOrderCheckpointClassAuthorityConflict)
	}
	if normalized.Class == "" && len(checkpoint.Citations) > 0 {
		return nil, fmt.Errorf("checkpoint.class must be %q when citations are supplied", core.WorkOrderCheckpointClassAuthorityConflict)
	}
	if normalized.Class == core.WorkOrderCheckpointClassAuthorityConflict && len(checkpoint.Citations) == 0 {
		return nil, fmt.Errorf("checkpoint.citations is required when checkpoint.class is %q", core.WorkOrderCheckpointClassAuthorityConflict)
	}
	for i, citation := range checkpoint.Citations {
		citation.DocumentID = strings.TrimSpace(citation.DocumentID)
		citation.StatementOrSectionID = strings.TrimSpace(citation.StatementOrSectionID)
		if citation.DocumentID == "" {
			return nil, fmt.Errorf("checkpoint.citations[%d].document_id is required", i)
		}
		if citation.CitedVersion <= 0 {
			return nil, fmt.Errorf("checkpoint.citations[%d].cited_version must be positive", i)
		}
		exists, clauseKnown, err := s.checkpointCitation(ctx, citation)
		if err != nil {
			return nil, err
		}
		if !exists {
			return nil, fmt.Errorf("checkpoint.citations[%d].document_id %q does not exist in this workspace", i, citation.DocumentID)
		}
		if !clauseKnown {
			citation.StatementOrSectionID = ""
		}
		normalized.Citations = append(normalized.Citations, citation)
	}
	if normalized.DecisionRequest == "" && normalized.Class == "" && len(normalized.Citations) == 0 {
		return nil, nil
	}
	return normalized, nil
}

func (s *Service) checkpointCitation(ctx context.Context, citation core.WorkOrderAuthorityConflictCitation) (bool, bool, error) {
	clause := citation.StatementOrSectionID
	if requirement, err := s.Store.GetRequirement(ctx, citation.DocumentID); err == nil {
		version := requirement.CurrentVersion
		if citation.CitedVersion > 0 {
			version = citation.CitedVersion
		}
		revision, getErr := s.Store.GetRequirementVersion(ctx, citation.DocumentID, version)
		if getErr != nil {
			return true, clause == "", nil
		}
		if clause == "" {
			return true, true, nil
		}
		for _, statement := range revision.Statements {
			if statement.ID == clause {
				return true, true, nil
			}
			for _, criterion := range statement.AcceptanceCriteria {
				if criterion.ID == clause {
					return true, true, nil
				}
			}
		}
		return true, false, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return false, false, err
	}
	if document, err := s.Store.GetSystemDesign(ctx, citation.DocumentID); err == nil {
		version := document.CurrentVersion
		if citation.CitedVersion > 0 {
			version = citation.CitedVersion
		}
		revision, getErr := s.Store.GetSystemDesignVersion(ctx, citation.DocumentID, version)
		return true, clause == "" || (getErr == nil && markdownClauseExists(revision.Content, clause)), nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return false, false, err
	}
	return false, false, nil
}

// markdownClauseExists reports whether a System Design clause names one of
// the content's headings, by text or by anchor. Inline code markup is removed
// from both sides first, so a clause naming a code-formatted heading such as
// `agentdesk context` is kept rather than dropped (component-mcp-protocol,
// Checkpoint decision requests on release; req-260820-394cac AC-2.1).
func markdownClauseExists(content, clause string) bool {
	if strings.TrimSpace(clause) == "" {
		return true
	}
	clause = strings.TrimSpace(stripInlineCode(clause))
	if clause == "" {
		return false
	}
	needle := headingAnchor(clause)
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#") {
			heading := strings.TrimSpace(stripInlineCode(strings.TrimLeft(line, "#")))
			if strings.EqualFold(heading, clause) || headingAnchor(heading) == needle {
				return true
			}
		}
	}
	return false
}

// stripInlineCode removes the backticks of Markdown inline code spans.
func stripInlineCode(text string) string {
	return strings.ReplaceAll(text, "`", "")
}

// headingAnchor is the heading's GitHub-style anchor: lower-case letters,
// digits, hyphens, and underscores, with spaces as hyphens.
func headingAnchor(heading string) string {
	return strings.Trim(strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + ('a' - 'A')
		}
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			return r
		}
		if r == ' ' {
			return '-'
		}
		return -1
	}, heading), "-")
}

// RequestPlanRevisionClaim atomically releases one exact implement attempt and
// moves its task to the distinct operator gate for the exact authenticated
// claim owner (REQ-1, AC-1.1-AC-1.3; component-work-orders).
func (s *Service) RequestPlanRevisionClaim(ctx context.Context, claim core.WorkOrderClaimIdentity, id, rationale string) (store.PlanRevisionRequestResult, error) {
	sessionID := claim.SessionID
	sessionID, rationale = strings.TrimSpace(sessionID), strings.TrimSpace(rationale)
	if sessionID == "" {
		return store.PlanRevisionRequestResult{}, fmt.Errorf("session_id is required")
	}
	if rationale == "" {
		return store.PlanRevisionRequestResult{}, fmt.Errorf("rationale is required")
	}
	current, err := s.Store.GetWorkOrder(ctx, id)
	if err != nil {
		return store.PlanRevisionRequestResult{}, err
	}
	return taskops.ExecuteWorkOrder(ctx, s.Store, current.TaskID, core.WorkOrderCmdRequestPlanRevision, func(taskLease taskops.TaskLease) (store.PlanRevisionRequestResult, error) {
		claim.SessionID = sessionID
		return s.Store.RequestPlanRevisionCommand(ctx, taskLease, id, claim, rationale)
	})
}

func transientConnectivityFailure(detail string) bool {
	detail = strings.ToLower(detail)
	for _, marker := range []string{
		"failed to connect to websocket",
		"nodename nor servname provided",
		"temporary failure in name resolution",
		"network is unreachable",
		"no such host",
		"connection refused",
		"connection reset by peer",
		"tls handshake timeout",
		"i/o timeout",
	} {
		if strings.Contains(detail, marker) {
			return true
		}
	}
	return false
}

// failureDetailLimit bounds a release's stored failure detail.
const failureDetailLimit = core.WorkOrderFailureDetailLimit

func boundedFailureDetail(detail string) string {
	detail = strings.ToValidUTF8(detail, "�")
	if len(detail) > failureDetailLimit {
		detail = detail[len(detail)-failureDetailLimit:]
		detail = strings.ToValidUTF8(detail, "�")
	}
	return strings.TrimSpace(detail)
}

// providerUsageLimit maps provider-specific stderr onto a stable category at
// the release boundary. The raw detail remains durable for audit and future
// classifiers; presentation code consumes only this provider-neutral value.
func providerUsageLimit(detail string) bool {
	detail = strings.ToLower(strings.TrimSpace(detail))
	for _, marker := range []string{
		"usage limit", "usage cap", "quota exceeded", "quota has been exceeded",
		"rate limit", "too many requests", "capacity limit", "capacity exhausted",
	} {
		if strings.Contains(detail, marker) {
			return true
		}
	}
	return false
}
