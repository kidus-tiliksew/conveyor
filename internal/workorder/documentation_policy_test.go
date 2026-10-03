package workorder

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/dispatch"
	"github.com/kidus-tiliksew/conveyor/internal/docsconfig"
	"github.com/kidus-tiliksew/conveyor/internal/pack"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	githubtrigger "github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

type docsPolicyFixture struct {
	ctx     context.Context
	st      store.Store
	service *Service
	cfg     *config.Config
	task    core.Task
	order   core.WorkOrder
}

func newDocsPolicyFixture(t *testing.T, githubSlug string, hooks func(*Service)) docsPolicyFixture {
	return newDocsPolicyFixtureAt(t, githubSlug, "main", hooks)
}

func newDocsPolicyFixtureAt(t *testing.T, githubSlug, baseBranch string, hooks func(*Service)) docsPolicyFixture {
	t.Helper()
	ctx := store.WithWorkspace(t.Context(), "test")
	st := store.NewMemory()
	task := core.Task{ID: "docs-task", Workspace: "test", Repo: "app", Title: "Docs", Branch: "conveyor/docs", BaseBranch: baseBranch, State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: time.Now().UTC()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: "docs-job", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	order := core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageImplement, State: core.WorkOrderQueued, QueueEnteredAt: now, QueueDeadline: now.Add(config.DefaultWorkOrderQueueTimeout)}
	if err := storetest.For(st).CreateWorkOrder(ctx, order); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Workspace: "test", WorkOrderQueueTimeout: config.DefaultWorkOrderQueueTimeout,
		Repos:   []config.Repo{{Name: "app", URL: "https://github.com/" + githubSlug, Base: "main", GitHub: githubSlug}},
		Routing: config.Routing{Stages: map[string]config.StageRoute{"implement": {Timeout: time.Hour}, "review": {Timeout: time.Hour}, "verify": {Timeout: time.Hour}}},
	}
	service := &Service{Store: st, ConfigProvider: func(context.Context) (*config.Config, error) { return cfg, nil }}
	if hooks != nil {
		hooks(service)
	}
	return docsPolicyFixture{ctx: ctx, st: st, service: service, cfg: cfg, task: task, order: order}
}

func (f docsPolicyFixture) claim(t *testing.T) core.WorkOrder {
	t.Helper()
	claimed, err := f.service.Claim(f.ctx, f.order.ID, core.WorkOrderClaim{
		SessionID: "docs-session", ClientToken: "docs-token", ClaimantID: core.TaskRunClaimantID("owner"), OwnerUserID: "owner", Lease: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	return claimed
}

func (f docsPolicyFixture) taskPolicy(t *testing.T) *core.DocumentationPolicy {
	t.Helper()
	task, err := f.st.GetTask(f.ctx, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	return task.DocumentationPolicy
}

func docsPolicyBaseSHA() string { return strings.Repeat("b", 40) }

func docsPolicyDiscovery(revision string) githubtrigger.DocumentationPolicyDiscovery {
	return githubtrigger.DocumentationPolicyDiscovery{Revision: revision, State: "present", Hash: "sha256:" + strings.Repeat("a", 64),
		Policy: &docsconfig.Config{
			SchemaVersion: 1,
			Docs:          []docsconfig.Document{{Path: "docs/knowledge-base/**", Description: "shipped-system knowledge base"}},
			Rule:          docsconfig.Rule{NoneStatement: "docs: none", ReasonRequired: true, Text: "Durable docs describe merged behavior only."},
		}}
}

func hasEventKind(events []core.Event, kind string) bool {
	for _, event := range events {
		if event.Kind == kind {
			return true
		}
	}
	return false
}

func TestClaimPinsDocumentationPolicyAtBaseHead(t *testing.T) {
	base := docsPolicyBaseSHA()
	var resolved, discovered string
	f := newDocsPolicyFixture(t, "acme/app", func(s *Service) {
		s.ResolveBranchHead = func(_ context.Context, workspace, slug, branch string) (string, string) {
			resolved = workspace + "|" + slug + "|" + branch
			return base, "present"
		}
		s.DiscoverDocumentationPolicy = func(_ context.Context, workspace, slug, revision string) githubtrigger.DocumentationPolicyDiscovery {
			discovered = workspace + "|" + slug + "|" + revision
			return docsPolicyDiscovery(revision)
		}
	})
	f.claim(t)
	if resolved != "test|acme/app|main" {
		t.Fatalf("resolve call = %q", resolved)
	}
	if discovered != "test|acme/app|"+base {
		t.Fatalf("discover call = %q", discovered)
	}
	policy := f.taskPolicy(t)
	if policy == nil || !policy.Enabled {
		t.Fatalf("policy = %+v", policy)
	}
	if policy.BaseSHA != base || policy.ContentHash != "sha256:"+strings.Repeat("a", 64) {
		t.Fatalf("policy identity = %+v", policy)
	}
	if len(policy.Paths) != 1 || policy.Paths[0] != "docs/knowledge-base/**" || policy.NoneStatement != "docs: none" || !policy.ReasonRequired || policy.RuleText == "" {
		t.Fatalf("policy content = %+v", policy)
	}
	events, err := f.st.ListEvents(f.ctx, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !hasEventKind(events, documentationPolicyPinnedEvent) {
		t.Fatalf("events = %+v", events)
	}
}

func TestClaimResolvesDocumentationPolicyAtTaskBaseBranch(t *testing.T) {
	base := docsPolicyBaseSHA()
	hooks := func(resolved *string) func(*Service) {
		return func(s *Service) {
			s.ResolveBranchHead = func(_ context.Context, _, _, branch string) (string, string) {
				*resolved = branch
				return base, "present"
			}
			s.DiscoverDocumentationPolicy = func(_ context.Context, _, _, revision string) githubtrigger.DocumentationPolicyDiscovery {
				return docsPolicyDiscovery(revision)
			}
		}
	}

	var resolved string
	f := newDocsPolicyFixtureAt(t, "acme/app", "release/2.x", hooks(&resolved))
	f.claim(t)
	if resolved != "release/2.x" {
		t.Fatalf("resolved branch = %q, want the task's non-default base branch", resolved)
	}

	var fallback string
	f = newDocsPolicyFixtureAt(t, "acme/app", "", hooks(&fallback))
	f.claim(t)
	if fallback != "main" {
		t.Fatalf("resolved branch = %q, want the configured repo base when the task base branch is empty", fallback)
	}
}

func TestClaimPinsDocumentationPolicyOffWhenDeclarationAbsent(t *testing.T) {
	f := newDocsPolicyFixture(t, "acme/app", func(s *Service) {
		s.ResolveBranchHead = func(context.Context, string, string, string) (string, string) {
			return docsPolicyBaseSHA(), "present"
		}
		s.DiscoverDocumentationPolicy = func(_ context.Context, _, _, revision string) githubtrigger.DocumentationPolicyDiscovery {
			return githubtrigger.DocumentationPolicyDiscovery{Revision: revision, State: "absent"}
		}
	})
	f.claim(t)
	policy := f.taskPolicy(t)
	if policy == nil || policy.Enabled || policy.OffReason != store.DocumentationOffAbsent {
		t.Fatalf("policy = %+v", policy)
	}
	events, _ := f.st.ListEvents(f.ctx, f.task.ID)
	if !hasEventKind(events, documentationPolicyOffEvent) {
		t.Fatalf("events = %+v", events)
	}
}

func TestClaimPinsDocumentationPolicyOffWithoutGitHubRepository(t *testing.T) {
	f := newDocsPolicyFixture(t, "", func(s *Service) {
		s.ResolveBranchHead = func(context.Context, string, string, string) (string, string) {
			t.Fatal("resolved a branch for a repository without GitHub")
			return "", ""
		}
		s.DiscoverDocumentationPolicy = func(context.Context, string, string, string) githubtrigger.DocumentationPolicyDiscovery {
			t.Fatal("discovered a policy for a repository without GitHub")
			return githubtrigger.DocumentationPolicyDiscovery{}
		}
	})
	f.claim(t)
	policy := f.taskPolicy(t)
	if policy == nil || policy.Enabled || policy.OffReason != store.DocumentationOffNoGitHub {
		t.Fatalf("policy = %+v", policy)
	}
}

func TestDocumentationPolicyPinIsSetOnceAcrossClaimsAndBaseMovement(t *testing.T) {
	first := docsPolicyBaseSHA()
	second := strings.Repeat("c", 40)
	calls := 0
	f := newDocsPolicyFixture(t, "acme/app", func(s *Service) {
		s.ResolveBranchHead = func(_ context.Context, _, _, _ string) (string, string) {
			calls++
			if calls == 1 {
				return first, "present"
			}
			return second, "present"
		}
		s.DiscoverDocumentationPolicy = func(_ context.Context, _, _, revision string) githubtrigger.DocumentationPolicyDiscovery {
			return docsPolicyDiscovery(revision)
		}
	})
	f.claim(t)
	// A later claim on the same task observes the moved base but must not
	// overwrite the set-once pin. The Claim path invokes exactly this hook.
	f.service.pinDocumentationPolicyForClaim(f.ctx, f.cfg, f.order)
	if calls != 1 {
		t.Fatalf("resolve calls = %d", calls)
	}
	policy := f.taskPolicy(t)
	if policy == nil || policy.BaseSHA != first {
		t.Fatalf("policy = %+v", policy)
	}
}

func TestDocumentationPolicyReadFailureLeavesUnsetAndRetries(t *testing.T) {
	base := docsPolicyBaseSHA()
	calls := 0
	f := newDocsPolicyFixture(t, "acme/app", func(s *Service) {
		s.ResolveBranchHead = func(_ context.Context, _, _, _ string) (string, string) {
			calls++
			if calls == 1 {
				return "", "transport"
			}
			return base, "present"
		}
		s.DiscoverDocumentationPolicy = func(_ context.Context, _, _, revision string) githubtrigger.DocumentationPolicyDiscovery {
			return docsPolicyDiscovery(revision)
		}
	})
	f.claim(t)
	if policy := f.taskPolicy(t); policy != nil {
		t.Fatalf("policy after transport failure = %+v", policy)
	}
	events, _ := f.st.ListEvents(f.ctx, f.task.ID)
	if !hasEventKind(events, documentationPolicyUnavailableEvent) {
		t.Fatalf("events = %+v", events)
	}
	// The next claim of any order on the task retries while the pin is unset.
	f.service.pinDocumentationPolicyForClaim(f.ctx, f.cfg, f.order)
	policy := f.taskPolicy(t)
	if policy == nil || !policy.Enabled || policy.BaseSHA != base {
		t.Fatalf("retry policy = %+v", policy)
	}
}

func TestWorkOrderContextRendersDocumentationPolicy(t *testing.T) {
	base := docsPolicyBaseSHA()
	f := newDocsPolicyFixture(t, "acme/app", func(s *Service) {
		s.ResolveBranchHead = func(context.Context, string, string, string) (string, string) { return base, "present" }
		s.DiscoverDocumentationPolicy = func(_ context.Context, _, _, revision string) githubtrigger.DocumentationPolicyDiscovery {
			return docsPolicyDiscovery(revision)
		}
	})
	f.claim(t)
	bundle, err := pack.Load("../../pack")
	if err != nil {
		t.Fatal(err)
	}
	f.service.Pack = bundle
	result, err := f.service.GetVisible(f.ctx, f.order.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.DocumentationPolicy == nil || !result.DocumentationPolicy.Enabled {
		t.Fatalf("context policy = %+v", result.DocumentationPolicy)
	}
	for _, want := range []string{"# Documentation policy", "docs/knowledge-base/**", "docs: none", base, "sha256:" + strings.Repeat("a", 64), "Durable docs describe merged behavior only."} {
		if !strings.Contains(result.RolePrompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, result.RolePrompt)
		}
	}
}

func TestWorkOrderContextRendersUnavailableDocumentationPolicy(t *testing.T) {
	f := newDocsPolicyFixture(t, "acme/app", func(s *Service) {
		s.ResolveBranchHead = func(context.Context, string, string, string) (string, string) { return "", "permission" }
		s.DiscoverDocumentationPolicy = func(context.Context, string, string, string) githubtrigger.DocumentationPolicyDiscovery {
			t.Fatal("discover called after resolve failure")
			return githubtrigger.DocumentationPolicyDiscovery{}
		}
	})
	f.claim(t)
	bundle, err := pack.Load("../../pack")
	if err != nil {
		t.Fatal(err)
	}
	f.service.Pack = bundle
	result, err := f.service.GetVisible(f.ctx, f.order.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.DocumentationPolicy != nil {
		t.Fatalf("context policy = %+v", result.DocumentationPolicy)
	}
	if !strings.Contains(result.RolePrompt, "documentation policy unavailable") {
		t.Fatalf("prompt = %s", result.RolePrompt)
	}
}

func TestSubmitForReviewRecordsDocumentationGateEvidence(t *testing.T) {
	base := docsPolicyBaseSHA()
	head := strings.Repeat("e", 40)
	f := newDocsPolicyFixture(t, "acme/app", func(s *Service) {
		s.ResolveBranchHead = func(context.Context, string, string, string) (string, string) { return base, "present" }
		s.DiscoverDocumentationPolicy = func(_ context.Context, _, _, revision string) githubtrigger.DocumentationPolicyDiscovery {
			return docsPolicyDiscovery(revision)
		}
	})
	f.claim(t)
	bundle, err := pack.Load("../../pack")
	if err != nil {
		t.Fatal(err)
	}
	dispatcher := dispatch.New(f.st, f.cfg, nil)
	dispatcher.DisableMemoryQueueForTest()
	f.service.Dispatcher = dispatcher
	f.service.Pack = bundle
	f.service.SubmissionChangedPaths = func(context.Context, *config.Config, core.Task) ([]string, error) {
		return []string{"docs/knowledge-base/page.md", "internal/x.go"}, nil
	}
	f.service.SubmissionPR = func(_ context.Context, _, branch string) (githubtrigger.SubmissionPullRequest, error) {
		pr := githubtrigger.SubmissionPullRequest{Number: 7, URL: "https://github.com/acme/app/pull/7",
			Body: "## Implementation notes\n\ndocs: none internal refactor\n\n<!-- conveyor:task-link -->\nConveyor task `x`\n<!-- conveyor:lifecycle-end -->\n"}
		pr.Head.Ref, pr.Head.SHA, pr.Base.Ref, pr.Base.SHA = branch, head, "main", strings.Repeat("d", 40)
		return pr, nil
	}
	f.service.ReconcileSubmissionPR = func(context.Context, string, githubtrigger.SubmissionPullRequest, string) error { return nil }
	f.service.ReviewDiffBetween = func(context.Context, string, string, string) (string, error) { return "diff", nil }
	if _, err := f.service.SubmitForReview(f.ctx, f.order.ID, "docs-session", head); err != nil {
		t.Fatal(err)
	}
	evidence, found, err := f.st.GetDocumentationGateEvidence(f.ctx, f.task.ID, head)
	if err != nil || !found {
		t.Fatalf("evidence found=%v err=%v", found, err)
	}
	if len(evidence.MatchedPaths) != 1 || evidence.MatchedPaths[0] != "docs/knowledge-base/page.md" {
		t.Fatalf("matched paths = %+v", evidence.MatchedPaths)
	}
	if evidence.NoneStatement != "docs: none" || evidence.NoneReason != "internal refactor" {
		t.Fatalf("none statement = %q reason = %q", evidence.NoneStatement, evidence.NoneReason)
	}
}

func TestDocumentationGateEvidenceIgnoresGeneratedLifecycleRegion(t *testing.T) {
	head := strings.Repeat("f", 40)
	f := newDocsPolicyFixture(t, "acme/app", nil)
	task := f.task
	task.DocumentationPolicy = &core.DocumentationPolicy{Enabled: true, BaseSHA: docsPolicyBaseSHA(), ContentHash: "sha256:" + strings.Repeat("a", 64), Paths: []string{"docs/**"}, NoneStatement: "docs: none", ReasonRequired: true}
	lifecycleOnly := "<!-- conveyor:task-link -->\nConveyor task `x`\n\ndocs: none — injected by Conveyor\n<!-- conveyor:lifecycle-end -->\n"
	if err := f.service.recordDocumentationGateEvidence(f.ctx, task, head, []string{"docs/page.md"}, lifecycleOnly); err != nil {
		t.Fatal(err)
	}
	evidence, found, err := f.st.GetDocumentationGateEvidence(f.ctx, task.ID, head)
	if err != nil || !found {
		t.Fatalf("evidence found=%v err=%v", found, err)
	}
	if evidence.NoneStatement != "" || evidence.NoneReason != "" {
		t.Fatalf("lifecycle-region statement leaked: %+v", evidence)
	}
}

func TestDocumentationGateEvidenceFreshPerHead(t *testing.T) {
	f := newDocsPolicyFixture(t, "acme/app", nil)
	task := f.task
	task.DocumentationPolicy = &core.DocumentationPolicy{Enabled: true, BaseSHA: docsPolicyBaseSHA(), ContentHash: "sha256:" + strings.Repeat("a", 64), Paths: []string{"docs/**"}, NoneStatement: "docs: none", ReasonRequired: true}
	first, second := strings.Repeat("1", 40), strings.Repeat("2", 40)
	if err := f.service.recordDocumentationGateEvidence(f.ctx, task, first, []string{"docs/a.md"}, ""); err != nil {
		t.Fatal(err)
	}
	if err := f.service.recordDocumentationGateEvidence(f.ctx, task, second, []string{"docs/b.md"}, ""); err != nil {
		t.Fatal(err)
	}
	firstEvidence, found, err := f.st.GetDocumentationGateEvidence(f.ctx, task.ID, first)
	if err != nil || !found || len(firstEvidence.MatchedPaths) != 1 || firstEvidence.MatchedPaths[0] != "docs/a.md" {
		t.Fatalf("first evidence = %+v found=%v err=%v", firstEvidence, found, err)
	}
	secondEvidence, found, err := f.st.GetDocumentationGateEvidence(f.ctx, task.ID, second)
	if err != nil || !found || len(secondEvidence.MatchedPaths) != 1 || secondEvidence.MatchedPaths[0] != "docs/b.md" {
		t.Fatalf("second evidence = %+v found=%v err=%v", secondEvidence, found, err)
	}
}
