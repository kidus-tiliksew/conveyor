package store

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

type VerificationAuthoritySnapshot struct {
	Task core.Task
	Plan *core.SpecVersion
}

func LoadVerificationAuthority(ctx context.Context, b Store, c VerificationCommand) (VerificationAuthoritySnapshot, error) {
	task, err := b.GetTask(ctx, c.Access.TaskID)
	if err != nil {
		return VerificationAuthoritySnapshot{}, ErrVerificationAccess
	}
	ws, ok := WorkspaceFromContext(ctx)
	if !ok || task.Workspace != ws {
		return VerificationAuthoritySnapshot{}, ErrVerificationAccess
	}
	plan, found, err := ApprovedExecutionDocument(ctx, b, task)
	if err != nil {
		return VerificationAuthoritySnapshot{}, err
	}
	a := VerificationAuthoritySnapshot{Task: task}
	if found {
		a.Plan = &plan
	}
	return a, nil
}
func VerifyVerificationAuthority(c VerificationCommand, a VerificationAuthoritySnapshot, o core.WorkOrder) error {
	if c.Kind == VerificationCreateContext {
		if c.Context == nil || c.Context.ReviewScope != o.ReviewScope || c.Context.BaselineSHA != o.BaselineSHA {
			return ErrVerificationInvalid
		}
		pins := []core.VerificationPin{}
		for _, r := range o.ServedRequirementSnapshot {
			pins = append(pins, core.VerificationPin{Kind: "requirement", DocumentID: r.ID, Version: r.Version})
		}
		if o.GovernanceSnapshot != nil {
			for _, d := range o.GovernanceSnapshot.Designs {
				pins = append(pins, core.VerificationPin{Kind: "system_design", DocumentID: d.ID, Version: d.Version})
			}
		}
		supplied := append([]core.VerificationPin{}, c.Context.GoverningPins...)
		sortPins := func(v []core.VerificationPin) {
			sort.Slice(v, func(i, j int) bool {
				if v[i].Kind != v[j].Kind {
					return v[i].Kind < v[j].Kind
				}
				return v[i].DocumentID < v[j].DocumentID
			})
		}
		sortPins(pins)
		sortPins(supplied)
		if !verificationEqual(pins, supplied) {
			return ErrVerificationAccess
		}
		// The submitted head is the primary repository's authoritative revision.
		// Additional repository resolution remains the trusted discovery caller's
		// job; it cannot substitute another head for the task's own repository.
		found := false
		seen := map[string]bool{}
		for _, r := range c.Context.Revisions {
			if seen[r.Repository] {
				return ErrVerificationInvalid
			}
			seen[r.Repository] = true
			if r.Repository == a.Task.Repo {
				if r.SHA != o.HeadSHA {
					return ErrVerificationAccess
				}
				found = true
			}
		}
		if !found {
			return ErrVerificationAccess
		}
	}
	return nil
}

// VerificationBoundAuthority is the authority snapshot and locked order a
// backend binds after claim authentication.
type VerificationBoundAuthority struct {
	Snapshot VerificationAuthoritySnapshot
	Order    core.WorkOrder
}

// BindVerificationAuthority attaches the transaction-bound authority so the
// mutation can check citations after it authenticates the context.
func BindVerificationAuthority(c *VerificationCommand, a VerificationAuthoritySnapshot, o core.WorkOrder) {
	c.BoundAuthority = &VerificationBoundAuthority{Snapshot: a, Order: o}
}

// verifyVerificationCitations checks coverage, finding, and obligation
// citations. PrepareVerificationMutation calls it only after the context is
// authenticated as the claim's own, so its detail never reaches an unknown,
// foreign, or stale scope. Rules name field indices and authority values, never
// caller text (component-verification-service "Correctable refusals").
func verifyVerificationCitations(c VerificationCommand) error {
	if c.BoundAuthority == nil {
		return ErrVerificationInvalid
	}
	a, o := c.BoundAuthority.Snapshot, c.BoundAuthority.Order
	coverage := c.Coverage
	if c.Submission != nil {
		coverage = &c.Submission.Coverage
	}
	if coverage != nil && coverage.Sources == nil {
		return verificationRefusal(ErrVerificationInvalid, VerificationCodeFieldRequired, "coverage.sources", verificationRemedyList, "coverage.sources is required; send [] for an empty list.")
	}
	if coverage != nil {
		planCovered := a.Plan == nil
		for i, mapping := range coverage.Sources {
			if mapping.Source.DocumentID == "approved_plan" {
				planCovered = true
			}
			if !verificationCitationResolves(VerificationCitation(mapping.Source), a, o, true) {
				return verificationRefusal(ErrVerificationInvalid, VerificationCodeCitationUnresolved, fmt.Sprintf("coverage.sources[%d].source", i), verificationRemedyCitation, "the citation resolves to no approved-plan section, served requirement statement or acceptance criterion, or served design section.")
			}
		}
		if !planCovered {
			return verificationRefusal(ErrVerificationInvalid, VerificationCodePlanMissing, "coverage.sources", verificationRemedyCitation, "the task has approved plan v%d; coverage.sources must include an approved_plan source at that version.", a.Plan.Version)
		}
	}
	if c.Submission != nil {
		if err := verificationValidateFindings(*c.Submission, a, o); err != nil {
			return err
		}
	}
	if c.Kind == VerificationRegisterObligation {
		if c.Obligation == nil {
			return ErrVerificationInvalid
		}
		for i, citation := range c.Obligation.Sources {
			if !verificationCitationResolves(citation, a, o, true) {
				return verificationRefusal(ErrVerificationInvalid, VerificationCodeCitationUnresolved, fmt.Sprintf("sources[%d]", i), verificationRemedyCitation, "the citation resolves to no approved-plan section, served requirement statement or acceptance criterion, or served design section.")
			}
		}
	}
	return nil
}

// verificationCitationResolves applies the citation rule of
// component-verification-service: the approved plan at its version with a
// section ID its content holds, a statement or acceptance criterion of a
// served requirement version, or (when designs are allowed) a section of a
// served design version.
func verificationCitationResolves(citation VerificationCitation, a VerificationAuthoritySnapshot, o core.WorkOrder, designs bool) bool {
	if citation.DocumentID == "approved_plan" && a.Plan != nil && a.Plan.Version == citation.Version && citation.SectionID != "" && strings.Contains(a.Plan.Content, citation.SectionID) {
		return true
	}
	for _, r := range o.ServedRequirementSnapshot {
		if r.ID != citation.DocumentID || r.Version != citation.Version {
			continue
		}
		for _, statement := range r.Statements {
			if statement.ID == citation.SectionID {
				return true
			}
			for _, ac := range statement.AcceptanceCriteria {
				if ac.ID == citation.SectionID {
					return true
				}
			}
		}
	}
	if designs && o.GovernanceSnapshot != nil {
		for _, d := range o.GovernanceSnapshot.Designs {
			if d.ID == citation.DocumentID && d.Version == citation.Version && citation.SectionID != "" && strings.Contains(d.Content, citation.SectionID) {
				return true
			}
		}
	}
	return false
}

// VerificationFindingLimit and VerificationFindingTextLimit bound the sourced
// findings of a feedback submission (component-verification-service).
const (
	VerificationFindingLimit     = 20
	VerificationFindingTextLimit = 1024
)

func verificationValidateFindings(s VerificationSubmission, a VerificationAuthoritySnapshot, o core.WorkOrder) error {
	if len(s.Findings) == 0 {
		return nil
	}
	remedy := "Send 1 to 20 findings with outcome feedback; each cites the approved plan or a served requirement once, states the finding in 1 to 1024 bytes, and its source also appears in coverage.sources."
	if s.Outcome != "feedback" {
		return verificationRefusal(ErrVerificationInvalid, VerificationCodeFindingInvalid, "findings", remedy, "only outcome feedback accepts findings.")
	}
	if len(s.Findings) > VerificationFindingLimit {
		return verificationRefusal(ErrVerificationInvalid, VerificationCodeFindingInvalid, "findings", remedy, "a submission holds at most %d findings.", VerificationFindingLimit)
	}
	seen := map[VerificationCoverageReference]bool{}
	for i, f := range s.Findings {
		field := fmt.Sprintf("findings[%d]", i)
		if text := strings.TrimSpace(f.Statement); text == "" || len(f.Statement) > VerificationFindingTextLimit {
			return verificationRefusal(ErrVerificationInvalid, VerificationCodeFindingInvalid, field+".statement", remedy, "statement must be 1 to %d bytes.", VerificationFindingTextLimit)
		}
		if seen[f.Source] {
			return verificationRefusal(ErrVerificationInvalid, VerificationCodeFindingInvalid, field+".source", remedy, "each finding source appears once.")
		}
		seen[f.Source] = true
		if !verificationCitationResolves(VerificationCitation(f.Source), a, o, false) {
			return verificationRefusal(ErrVerificationInvalid, VerificationCodeCitationUnresolved, field+".source", verificationRemedyCitation, "the finding source resolves to no approved-plan section or served requirement statement or acceptance criterion.")
		}
		mapped := false
		for _, m := range s.Coverage.Sources {
			mapped = mapped || m.Source == f.Source
		}
		if !mapped {
			return verificationRefusal(ErrVerificationInvalid, VerificationCodeFindingInvalid, field+".source", remedy, "the finding source must also appear in coverage.sources.")
		}
	}
	return nil
}

// BindVerificationEvidenceAuthority derives operator capability from durable
// membership. Submitted booleans cannot turn a worker into an operator.
func BindVerificationEvidenceAuthority(ctx context.Context, b MembershipStore, c *VerificationCommand) error {
	actor := ActorFromContext(ctx)
	c.Authority.SubmittedBy = actor.ID
	c.Authority.OperateGates = false
	c.Authority.TrustedRunner = c.Authority.TrustedRunner && actor.Role == core.ActorSystem && actor.ID == "verification-runner"
	if c.Access.UserID == "" {
		return nil
	}
	if actor.Role != core.ActorUser || actor.ID != UserActorID(c.Access.UserID) || (c.Kind != VerificationWriteEvidence && c.Kind != VerificationStageChunk && c.Kind != VerificationObserveOperation && c.Kind != VerificationAuthorizeRetry && !VerificationPermissionCommand(*c)) {
		return ErrVerificationAccess
	}
	ws, ok := WorkspaceFromContext(ctx)
	if !ok {
		return ErrVerificationAccess
	}
	allowed, err := b.AuthorizeWorkspace(ctx, c.Access.UserID, ws, core.CapabilityOperateGates)
	if err != nil {
		return err
	}
	if !allowed {
		return ErrVerificationAccess
	}
	c.Authority.OperateGates = true
	return nil
}
