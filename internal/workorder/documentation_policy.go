package workorder

import (
	"context"
	"fmt"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/docsconfig"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

// Task event kinds recorded while pinning a task's documentation-closure
// policy. The pin is set once at the first implement claim and retried on later
// claims while unset; every attempt outcome is observable on the task ledger.
const (
	documentationPolicyPinnedEvent      = "documentation.policy_pinned"
	documentationPolicyOffEvent         = "documentation.policy_off"
	documentationPolicyUnavailableEvent = "documentation.policy_unavailable"
)

// documentationPolicyReadConfigured reports whether the service can read the
// base-branch pin through a forge. Unit-test fixtures that never wire a forge
// leave this false so a claim performs no network read; production sets
// WorkspaceGitHubApps on the service.
func (s *Service) documentationPolicyReadConfigured() bool {
	return s.ResolveBranchHead != nil || s.DiscoverDocumentationPolicy != nil || s.GitHubApps != nil || s.WorkspaceGitHubApps != nil
}

func (s *Service) resolveBranchHead(ctx context.Context, workspace, slug, branch string) (string, string) {
	if s.ResolveBranchHead != nil {
		return s.ResolveBranchHead(ctx, workspace, slug, branch)
	}
	apps := s.GitHubApps
	if apps == nil {
		apps = github.DefaultAppClient
	}
	return apps.ResolveBranchHead(ctx, s.WorkspaceGitHubApps, workspace, slug, branch)
}

func (s *Service) discoverDocumentationPolicy(ctx context.Context, workspace, slug, revision string) github.DocumentationPolicyDiscovery {
	if s.DiscoverDocumentationPolicy != nil {
		return s.DiscoverDocumentationPolicy(ctx, workspace, slug, revision)
	}
	apps := s.GitHubApps
	if apps == nil {
		apps = github.DefaultAppClient
	}
	return apps.DiscoverDocumentationPolicy(ctx, s.WorkspaceGitHubApps, workspace, slug, revision)
}

// pinDocumentationPolicyForClaim attempts the set-once documentation-closure
// policy pin for a freshly claimed order. It applies to implement, review, and
// verify orders; a spec or triage claim precedes the gate and does not attempt.
// It never blocks or fails the claim: a resolution failure leaves the field
// unset, records a task event naming the cause, and is retried at the next
// claim while unset.
func (s *Service) pinDocumentationPolicyForClaim(ctx context.Context, cfg *config.Config, order core.WorkOrder) {
	if order.Stage != core.StageImplement && order.Stage != core.StageReview && order.Stage != core.StageVerify {
		return
	}
	task, err := s.Store.GetTask(ctx, order.TaskID)
	if err != nil || task.DocumentationPolicy != nil {
		return
	}
	repo, ok := cfg.Repo(task.Repo)
	if !ok {
		return
	}
	if repo.GitHub == "" {
		s.pinDocumentationPolicyOff(ctx, task, order, store.DocumentationOffNoGitHub)
		return
	}
	if !s.documentationPolicyReadConfigured() {
		return
	}
	base := strings.TrimSpace(task.BaseBranch)
	if base == "" {
		base = strings.TrimSpace(repo.Base)
	}
	if base == "" {
		s.recordDocumentationPolicyUnavailable(ctx, task, order, "base_branch_unresolved", "")
		return
	}
	sha, state := s.resolveBranchHead(ctx, task.Workspace, repo.GitHub, base)
	if state != "present" || strings.TrimSpace(sha) == "" {
		s.recordDocumentationPolicyUnavailable(ctx, task, order, "base_branch_unresolved", state)
		return
	}
	discovery := s.discoverDocumentationPolicy(ctx, task.Workspace, repo.GitHub, sha)
	switch discovery.State {
	case "present":
		if discovery.Policy == nil {
			s.recordDocumentationPolicyUnavailable(ctx, task, order, "malformed", "")
			return
		}
		s.storeDocumentationPolicyPin(ctx, task, order, core.DocumentationPolicy{
			Enabled:        true,
			BaseSHA:        sha,
			ContentHash:    discovery.Hash,
			Paths:          discovery.Policy.Paths(),
			NoneStatement:  discovery.Policy.Rule.NoneStatement,
			ReasonRequired: discovery.Policy.Rule.ReasonRequired,
			RuleText:       discovery.Policy.Rule.Text,
		}, documentationPolicyPinnedEvent)
	case "absent":
		s.pinDocumentationPolicyOff(ctx, task, order, store.DocumentationOffAbsent)
	default:
		s.recordDocumentationPolicyUnavailable(ctx, task, order, discovery.State, "")
	}
}

func (s *Service) pinDocumentationPolicyOff(ctx context.Context, task core.Task, order core.WorkOrder, reason string) {
	s.storeDocumentationPolicyPin(ctx, task, order, core.DocumentationPolicy{Enabled: false, OffReason: reason}, documentationPolicyOffEvent)
}

func (s *Service) storeDocumentationPolicyPin(ctx context.Context, task core.Task, order core.WorkOrder, policy core.DocumentationPolicy, kind string) {
	pinned, err := s.Store.PinTaskDocumentationPolicy(ctx, task.ID, policy)
	if err != nil {
		s.logf("pin documentation policy for task %s: %v", task.ID, err)
		return
	}
	if !pinned {
		return
	}
	_ = s.Store.AppendEvent(ctx, core.Event{TaskID: task.ID, JobID: order.JobID, Kind: kind, Payload: core.JSONPayload(map[string]any{
		"work_order_id": order.ID,
		"stage":         string(order.Stage),
		"base_sha":      policy.BaseSHA,
		"off_reason":    policy.OffReason,
		"path_count":    len(policy.Paths),
	})})
}

func (s *Service) recordDocumentationPolicyUnavailable(ctx context.Context, task core.Task, order core.WorkOrder, cause, detail string) {
	payload := map[string]any{"work_order_id": order.ID, "stage": string(order.Stage), "cause": cause}
	if detail != "" {
		payload["detail"] = detail
	}
	_ = s.Store.AppendEvent(ctx, core.Event{TaskID: task.ID, JobID: order.JobID, Kind: documentationPolicyUnavailableEvent, Payload: core.JSONPayload(payload)})
}

// recordDocumentationGateEvidence records the per-head docs-gate evidence read
// by the verdict validator. It is a no-op unless the task's pin is applicable.
// Cross-head resubmissions record fresh rows keyed by the reviewed head.
func (s *Service) recordDocumentationGateEvidence(ctx context.Context, task core.Task, headSHA string, changedPaths []string, pullRequestBody string) error {
	policy := task.DocumentationPolicy
	if !policy.Applicable() {
		return nil
	}
	matched := make([]string, 0, len(changedPaths))
	for _, path := range changedPaths {
		for _, glob := range policy.Paths {
			if docsconfig.MatchGlob(glob, path) {
				matched = append(matched, path)
				break
			}
		}
	}
	reason, found := docsconfig.MatchNoneStatement(github.AgentAuthoredPullRequestBody(pullRequestBody), policy.NoneStatement, policy.ReasonRequired)
	noneStatement := ""
	if found {
		noneStatement = policy.NoneStatement
	}
	if err := s.Store.RecordDocumentationGateEvidence(ctx, core.DocumentationGateEvidence{
		TaskID:        task.ID,
		HeadSHA:       headSHA,
		MatchedPaths:  matched,
		NoneStatement: noneStatement,
		NoneReason:    reason,
	}); err != nil {
		return fmt.Errorf("record documentation gate evidence for head %s: %w", headSHA, err)
	}
	return nil
}

// documentationPolicyUnavailableRecorded reports whether any claim attempt
// failed to resolve the policy, so the renderer can surface the gate-off line.
func documentationPolicyUnavailableRecorded(events []core.Event) bool {
	for _, event := range events {
		if event.Kind == documentationPolicyUnavailableEvent {
			return true
		}
	}
	return false
}

// documentationPolicyContract renders the pinned gate for a work-order context.
// An enabled pin renders the declared paths, the docs-none literal, and the
// pinned base SHA and content hash. An explicit off pin renders nothing (the
// gate is simply absent). An unset pin renders the unavailable line once a
// claim already attempted the read.
func documentationPolicyContract(policy *core.DocumentationPolicy, unavailable bool) string {
	if policy != nil {
		if !policy.Enabled {
			return ""
		}
		var b strings.Builder
		b.WriteString("\n\n# Documentation policy\n\n")
		if text := strings.TrimSpace(policy.RuleText); text != "" {
			b.WriteString(text)
			b.WriteString("\n\n")
		}
		b.WriteString("Pinned from base commit " + policy.BaseSHA + " (.conveyor/docs.yaml " + policy.ContentHash + ").\n")
		b.WriteString("Declared durable-docs paths:\n")
		for _, path := range policy.Paths {
			b.WriteString("- " + path + "\n")
		}
		b.WriteString("Docs-none statement: `" + policy.NoneStatement + "`")
		if policy.ReasonRequired {
			b.WriteString(" (a non-empty reason is required on the same line)")
		}
		b.WriteString("\n")
		return b.String()
	}
	if unavailable {
		return "\n\ndocumentation policy unavailable\n"
	}
	return ""
}
