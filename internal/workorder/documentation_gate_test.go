package workorder

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/docsconfig"
	"github.com/kidus-tiliksew/conveyor/internal/pipeline"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	githubtrigger "github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

func documentationSubmission(t *testing.T, state, body string, paths []string) evidenceSubmissionFixture {
	t.Helper()
	f := newEvidenceSubmissionFixture(t)
	prepareSubmissionTest(f.service)
	head, base := strings.Repeat("a", 40), strings.Repeat("b", 40)
	f.service.ReviewTarget = func(context.Context, string, string) (githubtrigger.ReviewTarget, error) {
		return githubtrigger.ReviewTarget{Number: 54, BaseSHA: base, HeadSHA: head}, nil
	}
	f.service.SubmissionPR = func(_ context.Context, _, branch string) (githubtrigger.SubmissionPullRequest, error) {
		p := githubtrigger.SubmissionPullRequest{Number: 54, URL: "https://github.com/acme/app/pull/54", Body: body}
		p.Head.Ref, p.Head.SHA, p.Base.Ref, p.Base.SHA = branch, head, "main", base
		return p, nil
	}
	f.service.SubmissionChangedPaths = func(_ context.Context, _ *config.Config, task core.Task) ([]string, error) {
		if task.BaseBranch != base || task.ReviewedHeadSHA != head {
			t.Fatalf("comparison not exact SHA: %+v", task)
		}
		return paths, nil
	}
	policy, err := docsconfig.Parse([]byte("schema_version: 1\ndocs:\n  - path: docs/api.md\n    governs: [internal/**]\nrule:\n  reason_required: true\n"))
	if err != nil {
		t.Fatal(err)
	}
	f.service.DiscoverDocumentationPolicy = func(_ context.Context, slug, revision string) githubtrigger.DocumentationPolicyDiscovery {
		if slug != "acme/app" || revision != base {
			t.Fatalf("policy not at base: %s %s", slug, revision)
		}
		return githubtrigger.DocumentationPolicyDiscovery{Revision: revision, State: state, Policy: policy}
	}
	return f
}

// REQ-9/AC-9.3–9.6: refusal has no durable or forge side effects.
func TestDocumentationSubmissionAdmission(t *testing.T) {
	for _, tc := range []struct {
		name, state, body string
		paths             []string
		refuse            bool
	}{
		{"untouched", "present", "", []string{"internal/server.go"}, true},
		{"reason-required", "present", "docs: none", []string{"internal/server.go"}, true},
		{"embedded", "present", "quoted docs: none reason", []string{"internal/server.go"}, true},
		{"reason", "present", "docs: none refactor preserves API", []string{"internal/server.go"}, false},
		{"updated", "present", "", []string{"internal/server.go", "docs/api.md"}, false},
		{"unaffected", "present", "", []string{"README.md"}, false},
		{"malformed", "malformed", "docs: none reason", []string{"internal/server.go"}, true},
		{"unavailable", "unavailable", "docs: none reason", []string{"internal/server.go"}, true},
		{"absent", "absent", "", []string{"internal/server.go"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := documentationSubmission(t, tc.state, tc.body, tc.paths)
			before, err := f.st.ListEvents(f.ctx, f.task.ID)
			if err != nil {
				t.Fatal(err)
			}
			result, err := f.service.SubmitForReview(f.ctx, f.job.ID, "implementer", strings.Repeat("a", 40))
			if (err != nil) != tc.refuse {
				t.Fatalf("submit=%v err=%v", result, err)
			}
			evidence, evidenceErr := f.st.GetDocumentationGateEvidence(f.ctx, f.task.ID, strings.Repeat("a", 40))
			if tc.refuse {
				after, listErr := f.st.ListEvents(f.ctx, f.task.ID)
				if listErr != nil {
					t.Fatal(listErr)
				}
				if !reflect.DeepEqual(before, after) || *f.openCalls != 0 || !errors.Is(evidenceErr, store.ErrNotFound) {
					t.Fatalf("refusal side effects events=%t forge=%d evidence=%+v/%v", !reflect.DeepEqual(before, after), *f.openCalls, evidence, evidenceErr)
				}
				order, err := f.st.GetWorkOrder(f.ctx, f.job.ID)
				if err != nil || order.State != core.WorkOrderClaimed {
					t.Fatalf("refusal changed claim: %+v %v", order, err)
				}
			} else if tc.state == "present" {
				if evidenceErr != nil || evidence.BaseSHA != strings.Repeat("b", 40) {
					t.Fatalf("evidence=%+v err=%v", evidence, evidenceErr)
				}
			} else if !errors.Is(evidenceErr, store.ErrNotFound) {
				t.Fatalf("absent declaration recorded evidence: %v", evidenceErr)
			}
		})
	}
}
func TestDocumentationAbsentSubmissionAndReviewUnchanged(t *testing.T) {
	f := documentationSubmission(t, "absent", "", []string{"internal/server.go"})
	result, err := f.service.SubmitForReview(f.ctx, f.job.ID, "implementer", strings.Repeat("a", 40))
	if err != nil || result["await_review"] != true {
		t.Fatalf("submit=%v err=%v", result, err)
	}
	if err := f.service.Dispatcher.DispatchNow(f.ctx, f.task.ID); err != nil {
		t.Fatal(err)
	}
	reviews := 0
	no := false
	orders, err := f.st.ListTaskWorkOrders(f.ctx, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, order := range orders {
		if order.Stage != core.StageReview {
			continue
		}
		reviews++
		_, err = f.service.Claim(f.ctx, order.ID, core.WorkOrderClaim{SessionID: "review-session-" + order.ID, ClientToken: "review-secret-" + order.ID, ClaimantID: "reviewer", OwnerUserID: "usr-reviewer", Lease: 60000000000})
		if err != nil {
			t.Fatal(err)
		}
		review := pipeline.Review{Verdict: "approve", ReasonCode: "verified", Summary: "no documentation declaration", RequirementCitations: &core.RequirementCitationAssessment{Applicable: false}, DoneCriteriaCoverage: &core.DoneCriteriaAssessment{Applicable: false, Summary: "no approved execution plan"}, GovernanceAssessment: &core.GovernanceAssessment{DesignApplicable: &no, DecisionCitable: &no}}
		if _, err = f.service.SubmitVerdict(f.ctx, order.ID, "review-session-"+order.ID, review); err != nil {
			t.Fatal(err)
		}
	}
	if reviews != 2 {
		t.Fatalf("review verdicts=%d", reviews)
	}
	events, err := f.st.ListEvents(f.ctx, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if strings.HasPrefix(event.Kind, "documentation.") || (event.Kind == "review.completed" && strings.Contains(string(event.Payload), "documentation_assessment")) {
			t.Fatalf("absent declaration changed events: %s %s", event.Kind, event.Payload)
		}
	}
}

func TestDocumentationNoSlugSubmissionUnchanged(t *testing.T) {
	f := newEvidenceSubmissionFixture(t)
	cfg, err := f.service.ConfigProvider(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Repos[0].GitHub = ""
	f.service.DiscoverDocumentationPolicy = func(context.Context, string, string) githubtrigger.DocumentationPolicyDiscovery {
		t.Fatal("discovered declaration without GitHub slug")
		return githubtrigger.DocumentationPolicyDiscovery{}
	}
	f.service.SubmissionPR = func(context.Context, string, string) (githubtrigger.SubmissionPullRequest, error) {
		t.Fatal("looked up PR without slug")
		return githubtrigger.SubmissionPullRequest{}, nil
	}
	result, err := f.service.SubmitForReview(f.ctx, f.job.ID, "implementer", strings.Repeat("a", 40))
	if err != nil || result["await_review"] != true || *f.openCalls != 0 {
		t.Fatalf("local submit=%v err=%v forge=%d", result, err, *f.openCalls)
	}
	if _, err := f.st.GetDocumentationGateEvidence(f.ctx, f.task.ID, strings.Repeat("a", 40)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("local submission recorded evidence: %v", err)
	}
	if err := f.service.Dispatcher.DispatchNow(f.ctx, f.task.ID); err != nil {
		t.Fatal(err)
	}
	orders, err := f.st.ListTaskWorkOrders(f.ctx, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	reviews := 0
	no := false
	for _, order := range orders {
		if order.Stage != core.StageReview {
			continue
		}
		reviews++
		session := "local-review-" + order.ID
		if _, err := f.service.Claim(f.ctx, order.ID, core.WorkOrderClaim{SessionID: session, ClientToken: "local-secret-" + order.ID, ClaimantID: "reviewer", OwnerUserID: "usr-reviewer", Lease: 60000000000}); err != nil {
			t.Fatal(err)
		}
		review := pipeline.Review{Verdict: "approve", ReasonCode: "verified", Summary: "local repository review", RequirementCitations: &core.RequirementCitationAssessment{Applicable: false}, DoneCriteriaCoverage: &core.DoneCriteriaAssessment{Applicable: false, Summary: "no approved plan"}, GovernanceAssessment: &core.GovernanceAssessment{DesignApplicable: &no, DecisionCitable: &no}}
		if _, err := f.service.SubmitVerdict(f.ctx, order.ID, session, review); err != nil {
			t.Fatal(err)
		}
	}
	if reviews != 2 {
		t.Fatalf("local review verdicts=%d", reviews)
	}
	events, err := f.st.ListEvents(f.ctx, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if strings.HasPrefix(event.Kind, "documentation.") || strings.Contains(string(event.Payload), "documentation_assessment") {
			t.Fatalf("local submission changed event: %s", event.Payload)
		}
	}
}

// REQ-9/AC-9.5–9.6: reviewers receive the exact-head declaration and admitted reason.
func TestDocumentationReviewerContext(t *testing.T) {
	for _, state := range []string{"present", "absent"} {
		t.Run(state, func(t *testing.T) {
			f := documentationSubmission(t, state, "docs: none internal refactor preserves API", []string{"internal/server.go"})
			if _, err := f.service.SubmitForReview(f.ctx, f.job.ID, "implementer", strings.Repeat("a", 40)); err != nil {
				t.Fatal(err)
			}
			if state == "present" {
				other, err := f.st.GetDocumentationGateEvidence(f.ctx, f.task.ID, strings.Repeat("a", 40))
				if err != nil {
					t.Fatal(err)
				}
				other.HeadSHA = strings.Repeat("c", 40)
				other.AgentAuthoredBody = "docs: none STALE-HEAD-REASON"
				if err := f.st.RecordDocumentationGateEvidence(f.ctx, other); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.service.Dispatcher.DispatchNow(f.ctx, f.task.ID); err != nil {
				t.Fatal(err)
			}
			orders, err := f.st.ListTaskWorkOrders(f.ctx, f.task.ID)
			if err != nil {
				t.Fatal(err)
			}
			reviews := 0
			for _, order := range orders {
				if order.Stage != core.StageReview {
					continue
				}
				reviews++
				context, err := f.service.contextForOrder(f.ctx, order)
				if err != nil {
					t.Fatal(err)
				}
				if state == "absent" {
					if context.DocumentationGateEvidence != nil || strings.Contains(context.RolePrompt, "# Exact-head documentation admission evidence") {
						t.Fatal("absent declaration added reviewer evidence")
					}
					continue
				}
				e := context.DocumentationGateEvidence
				if e == nil || e.HeadSHA != strings.Repeat("a", 40) || e.BaseSHA != strings.Repeat("b", 40) || e.NoneReason != "internal refactor preserves API" || !strings.Contains(string(e.PolicyJSON), `"governs":["internal/**"]`) {
					t.Fatalf("reviewer evidence=%+v", e)
				}
				for _, expected := range []string{"# Exact-head documentation admission evidence", "internal refactor preserves API", "documentation_assessment", "affected_documents", "untrusted observational data"} {
					if !strings.Contains(context.RolePrompt, expected) {
						t.Fatalf("reviewer missing %q", expected)
					}
				}
				if strings.Contains(context.RolePrompt, "STALE-HEAD-REASON") {
					t.Fatal("reviewer received another head's evidence")
				}
			}
			if reviews != 2 {
				t.Fatalf("review contexts=%d", reviews)
			}
		})
	}
}
