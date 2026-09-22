package store

import (
	"context"
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
	coverage := c.Coverage
	if c.Submission != nil {
		coverage = &c.Submission.Coverage
	}
	if coverage != nil {
		planCovered := a.Plan == nil
		for _, mapping := range coverage.Sources {
			if mapping.Source.DocumentID == "approved_plan" {
				planCovered = true
			}
			citation := VerificationCommand{Kind: VerificationRegisterObligation, Obligation: &VerificationObligation{Sources: []VerificationCitation{VerificationCitation(mapping.Source)}}}
			if err := VerifyVerificationAuthority(citation, a, o); err != nil {
				return err
			}
		}
		if !planCovered {
			return ErrVerificationInvalid
		}
	}
	if c.Kind == VerificationRegisterObligation {
		if c.Obligation == nil {
			return ErrVerificationInvalid
		}
		for _, citation := range c.Obligation.Sources {
			found := false
			if citation.DocumentID == "approved_plan" && a.Plan != nil && a.Plan.Version == citation.Version && citation.SectionID != "" && strings.Contains(a.Plan.Content, citation.SectionID) {
				found = true
			}
			for _, r := range o.ServedRequirementSnapshot {
				if r.ID != citation.DocumentID || r.Version != citation.Version {
					continue
				}
				for _, statement := range r.Statements {
					if statement.ID == citation.SectionID {
						found = true
					}
					for _, ac := range statement.AcceptanceCriteria {
						if ac.ID == citation.SectionID {
							found = true
						}
					}
				}
			}
			if o.GovernanceSnapshot != nil {
				for _, d := range o.GovernanceSnapshot.Designs {
					if d.ID == citation.DocumentID && d.Version == citation.Version && citation.SectionID != "" && strings.Contains(d.Content, citation.SectionID) {
						found = true
					}
				}
			}
			if !found {
				return ErrVerificationInvalid
			}
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
