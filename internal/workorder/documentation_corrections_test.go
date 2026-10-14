package workorder

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/docsconfig"
	"github.com/kidus-tiliksew/conveyor/internal/pipeline"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres"
	"github.com/kidus-tiliksew/conveyor/internal/store/singlestore"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
	githubtrigger "github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

// Each SQL case owns an isolated schema/database, including cleanup after failure.
func documentationCorrectionStore(t *testing.T, backend string) store.Store {
	t.Helper()
	if backend == "memory" {
		return store.NewMemory()
	}
	var st store.Backend
	if backend == "postgres" {
		raw := os.Getenv("CONVEYOR_TEST_DATABASE_URL")
		if raw == "" {
			t.Skip("CONVEYOR_TEST_DATABASE_URL is not set")
		}
		admin, err := pgxpool.New(t.Context(), raw)
		if err != nil {
			t.Fatal(err)
		}
		name := "docgate_" + strings.ReplaceAll(core.NewTaskID(), "-", "_")
		if _, err = admin.Exec(t.Context(), "CREATE SCHEMA "+pgx.Identifier{name}.Sanitize()); err != nil {
			admin.Close()
			t.Fatal(err)
		}
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if _, err := admin.Exec(ctx, "DROP SCHEMA "+pgx.Identifier{name}.Sanitize()+" CASCADE"); err != nil {
				t.Error(err)
			}
			admin.Close()
		})
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		q := u.Query()
		q.Set("search_path", name)
		u.RawQuery = q.Encode()
		opened, err := postgres.Open(t.Context(), u.String())
		if err != nil {
			t.Fatal(err)
		}
		st = opened
		t.Cleanup(opened.Close)
	} else {
		raw := os.Getenv("CONVEYOR_TEST_SINGLESTORE_URL")
		if raw == "" {
			t.Skip("CONVEYOR_TEST_SINGLESTORE_URL is not set")
		}
		cfg, err := mysql.ParseDSN(raw)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasSuffix(cfg.DBName, "_test") {
			t.Fatal("SingleStore test database must end in _test")
		}
		connector, err := mysql.NewConnector(cfg)
		if err != nil {
			t.Fatal(err)
		}
		admin := sql.OpenDB(connector)
		name := fmt.Sprintf("docgate_%d_test", time.Now().UnixNano())
		if _, err = admin.ExecContext(t.Context(), "CREATE DATABASE `"+name+"` PARTITIONS 2"); err != nil {
			admin.Close()
			t.Fatal(err)
		}
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if _, err := admin.ExecContext(ctx, "DROP DATABASE `"+name+"`"); err != nil {
				t.Error(err)
			}
			admin.Close()
		})
		cfg.DBName = name
		opened, err := singlestore.Open(t.Context(), cfg.FormatDSN())
		if err != nil {
			t.Fatal(err)
		}
		st = opened
		t.Cleanup(opened.Close)
	}
	for _, workspace := range []string{"demo", "test"} {
		ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), workspace)
		if _, err := st.BootstrapWorkspaceConfig(ctx, &config.Config{Workspace: workspace, Repos: []config.Repo{{Name: "app", Base: "main", GitHub: "acme/app"}}}); err != nil {
			t.Fatal(err)
		}
	}
	return st
}

func approveDocumentationSeats(t *testing.T, f evidenceSubmissionFixture, assessment *core.DocumentationAssessment) {
	t.Helper()
	if err := f.service.Dispatcher.DispatchNow(f.ctx, f.task.ID); err != nil {
		t.Fatal(err)
	}
	orders, err := f.st.ListTaskWorkOrders(f.ctx, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	no := false
	seats := 0
	for _, order := range orders {
		if order.Stage != core.StageReview || order.State != core.WorkOrderQueued {
			continue
		}
		seats++
		session := "review-" + order.ID
		if _, err := f.service.Claim(f.ctx, order.ID, core.WorkOrderClaim{SessionID: session, ClientToken: session, ClaimantID: "reviewer", OwnerUserID: "usr-reviewer", Lease: time.Minute}); err != nil {
			t.Fatal(err)
		}
		result := pipeline.Review{Verdict: "approve", ReasonCode: "verified", Summary: "Review complete", RequirementCitations: &core.RequirementCitationAssessment{Applicable: false}, DoneCriteriaCoverage: &core.DoneCriteriaAssessment{Applicable: false, Summary: "no plan"}, GovernanceAssessment: &core.GovernanceAssessment{DesignApplicable: &no, DecisionCitable: &no}, DocumentationAssessment: assessment}
		if _, err := f.service.SubmitVerdict(f.ctx, order.ID, session, result); err != nil {
			t.Fatal(err)
		}
	}
	if seats != 2 {
		t.Fatalf("review seats=%d", seats)
	}
}

// Operator-requested B1–B4 regression cases, REQ-9/AC-9.3, AC-9.5, AC-9.6.
func TestDocumentationCorrectionRegressions(t *testing.T) {
	for _, backend := range []string{"memory", "postgres", "singlestore"} {
		t.Run(backend, func(t *testing.T) {
			t.Run("unknown-declaration-does-not-opt-in", func(t *testing.T) {
				st := documentationCorrectionStore(t, backend)
				f := documentationSubmission(t, "unknown", "", []string{"internal/server.go"}, func(store.Store) store.Store { return st })
				if _, err := f.service.SubmitForReview(f.ctx, f.job.ID, "implementer", strings.Repeat("a", 40)); err != nil {
					t.Fatalf("unknown declaration changed legacy submission: %v", err)
				}
				if _, err := st.GetDocumentationGateEvidence(f.ctx, f.task.ID, strings.Repeat("a", 40)); !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("unknown declaration recorded evidence: %v", err)
				}
			})
			t.Run("failed-submission-does-not-leave-evidence", func(t *testing.T) {
				st := documentationCorrectionStore(t, backend)
				f := documentationSubmission(t, "present", "docs: none unchanged API", []string{"internal/server.go"}, func(store.Store) store.Store { return st })
				fail := true
				f.service.ReconcileSubmissionPR = func(context.Context, string, githubtrigger.SubmissionPullRequest, string) error {
					if fail {
						return errors.New("injected reconcile failure")
					}
					return nil
				}
				head := strings.Repeat("a", 40)
				if _, err := f.service.SubmitForReview(f.ctx, f.job.ID, "implementer", head); err == nil {
					t.Fatal("expected reconciliation failure")
				}
				if _, err := st.GetDocumentationGateEvidence(f.ctx, f.task.ID, head); !errors.Is(err, store.ErrNotFound) {
					t.Errorf("failed submission left evidence: %v", err)
				}
				fail = false
				f.service.DiscoverDocumentationPolicy = func(_ context.Context, _, revision string) githubtrigger.DocumentationPolicyDiscovery {
					return githubtrigger.DocumentationPolicyDiscovery{Revision: revision, State: "absent"}
				}
				if _, err := f.service.SubmitForReview(f.ctx, f.job.ID, "implementer", head); err != nil {
					t.Fatal(err)
				}
				if _, err := st.GetDocumentationGateEvidence(f.ctx, f.task.ID, head); !errors.Is(err, store.ErrNotFound) {
					t.Errorf("absent retry retained stale evidence: %v", err)
				}
				approveDocumentationSeats(t, f, nil)
			})
			t.Run("dispatched-conflict-fix-still-runs-gate", func(t *testing.T) {
				st := documentationCorrectionStore(t, backend)
				f := newConflictSubmission(t, config.ExecutionSetup{}, func(store.Store) store.Store { return st })
				head, base := strings.Repeat("a", 40), strings.Repeat("b", 40)
				f.prHead = func(int) string { return head }
				f.service.SubmissionPR = func(_ context.Context, _, branch string) (githubtrigger.SubmissionPullRequest, error) {
					p := githubtrigger.SubmissionPullRequest{Number: 1098, URL: "https://github.com/acme/app/pull/1098"}
					p.Head.Ref, p.Head.SHA, p.Base.Ref, p.Base.SHA = branch, head, "main", base
					return p, nil
				}
				f.service.ReviewTarget = func(context.Context, string, string) (githubtrigger.ReviewTarget, error) {
					return githubtrigger.ReviewTarget{Number: 1098, BaseSHA: base, HeadSHA: head}, nil
				}
				f.service.SubmissionChangedPaths = func(context.Context, *config.Config, core.Task) ([]string, error) {
					return []string{"internal/server.go"}, nil
				}
				policy, err := docsconfig.Parse([]byte("schema_version: 1\ndocs:\n  - path: docs/api.md\n    governs: ['internal/**']\n"))
				if err != nil {
					t.Fatal(err)
				}
				calls := 0
				f.service.DiscoverDocumentationPolicy = func(_ context.Context, _, revision string) githubtrigger.DocumentationPolicyDiscovery {
					calls++
					return githubtrigger.DocumentationPolicyDiscovery{Revision: revision, State: "present", Policy: policy}
				}
				f.dispatchExistingRefresh(t, head, config.RefreshReviewDelta)
				before := f.eventCount(t)
				if result, err := f.submit("fixer", head); err == nil || !strings.Contains(err.Error(), "docs/api.md") {
					t.Fatalf("conflict fix bypassed admission: result=%v err=%v calls=%d", result, err, calls)
				}
				if calls != 1 {
					t.Fatalf("policy discovery calls=%d", calls)
				}
				if f.eventCount(t) != before || f.jobState(t, f.order.JobID) != core.JobRunning {
					t.Fatal("refused conflict fix had submission side effects")
				}
			})
			t.Run("dispatched-conflict-fix-lookup-failure-has-no-effects", func(t *testing.T) {
				for _, failure := range []string{"pr", "forge"} {
					t.Run(failure, func(t *testing.T) {
						st := documentationCorrectionStore(t, backend)
						f := newConflictSubmission(t, config.ExecutionSetup{}, func(store.Store) store.Store { return st })
						head, base := strings.Repeat("a", 40), strings.Repeat("b", 40)
						f.prHead = func(int) string { return head }
						f.dispatchExistingRefresh(t, head, config.RefreshReviewDelta)
						evidence := core.DocumentationGateEvidence{
							TaskID: f.task.ID, HeadSHA: head, BaseSHA: base,
							PolicyJSON:   []byte(`{"schema_version":1,"docs":[{"path":"docs/api.md","governs":["internal/**"]}],"rule":{"none_statement":"docs: none","reason_required":true}}`),
							ChangedPaths: []string{"internal/server.go"}, AgentAuthoredBody: "docs: none STORED-REASON",
						}
						if err := st.RecordDocumentationGateEvidence(f.ctx, evidence); err != nil {
							t.Fatal(err)
						}
						lookupErr := errors.New("transient current PR read failed")
						if failure == "pr" {
							f.service.SubmissionPR = func(context.Context, string, string) (githubtrigger.SubmissionPullRequest, error) {
								return githubtrigger.SubmissionPullRequest{}, lookupErr
							}
						} else {
							f.service.SubmissionPR = nil
						}
						before := f.eventCount(t)
						_, err := f.submit("fixer", head)
						if err == nil || failure == "pr" && !errors.Is(err, lookupErr) {
							t.Fatalf("failed %s lookup completed refresh or lost the lookup error: %v", failure, err)
						}
						order, err := st.GetWorkOrder(f.ctx, f.order.ID)
						if err != nil {
							t.Fatal(err)
						}
						if order.State != core.WorkOrderClaimed || f.jobState(t, f.order.JobID) != core.JobRunning || f.eventCount(t) != before {
							t.Fatalf("failed PR read had submission effects: order=%+v", order)
						}
						kept, err := st.GetDocumentationGateEvidence(f.ctx, f.task.ID, head)
						if err != nil || kept.AgentAuthoredBody != "docs: none STORED-REASON" {
							t.Fatalf("failed PR read changed evidence: %+v %v", kept, err)
						}
					})
				}
			})
			t.Run("refresh-evidence-and-decision-share-head", func(t *testing.T) {
				st := documentationCorrectionStore(t, backend)
				f := documentationSubmission(t, "present", "docs: none OLD-HEAD-REASON", []string{"internal/server.go"}, func(store.Store) store.Store { return st })
				oldHead, newHead := strings.Repeat("a", 40), strings.Repeat("c", 40)
				if _, err := f.service.SubmitForReview(f.ctx, f.job.ID, "implementer", oldHead); err != nil {
					t.Fatal(err)
				}
				assessment := &core.DocumentationAssessment{Applicable: true, Summary: "Documentation remains accurate", UpdatedPaths: []string{}, Unresolved: []string{}, Conflicts: []string{}}
				approveDocumentationSeats(t, f, assessment)
				if _, err := taskops.New(st).Perform(f.ctx, f.task.ID, taskops.Command{Kind: core.TaskInterventionApproveReview}); err != nil {
					t.Fatal(err)
				}
				f.service.Dispatcher.DiscoverDocumentationPolicy = f.service.DiscoverDocumentationPolicy
				f.service.Dispatcher.SubmissionPR = func(context.Context, string, string) (githubtrigger.SubmissionPullRequest, error) {
					p := githubtrigger.SubmissionPullRequest{Number: 54, URL: "https://github.com/acme/app/pull/54", Body: "docs: none NEW-HEAD-REASON"}
					p.Head.Ref, p.Head.SHA, p.Base.Ref, p.Base.SHA = f.task.Branch, newHead, "main", strings.Repeat("b", 40)
					return p, nil
				}
				f.service.Dispatcher.ReviewChangedPaths = func(context.Context, *config.Config, core.Task) ([]string, error) {
					return []string{"internal/refresh-only.go"}, nil
				}
				f.service.Dispatcher.ViewPullRequest = func(context.Context, string, string) (githubtrigger.PullRequest, error) {
					return githubtrigger.PullRequest{Number: 54, HeadSHA: newHead, BaseSHA: strings.Repeat("b", 40), Mergeable: "MERGEABLE"}, nil
				}
				task, err := st.GetTask(f.ctx, f.task.ID)
				if err != nil {
					t.Fatal(err)
				}
				_, err = f.service.Dispatcher.ReadMergeReadiness(f.ctx, task)
				if err != nil {
					t.Fatal(err)
				}
				orders, err := st.ListTaskWorkOrders(f.ctx, f.task.ID)
				if err != nil {
					t.Fatal(err)
				}
				found := false
				for _, order := range orders {
					if order.Stage != core.StageReview || order.HeadSHA != newHead {
						continue
					}
					found = true
					ctx, err := f.service.contextForOrder(f.ctx, order)
					if err != nil {
						t.Fatal(err)
					}
					if ctx.DocumentationGateEvidence == nil || ctx.DocumentationGateEvidence.HeadSHA != order.HeadSHA || strings.Join(ctx.DocumentationGateEvidence.ChangedPaths, ",") != "internal/refresh-only.go" || !strings.Contains(ctx.RolePrompt, "NEW-HEAD-REASON") || strings.Contains(ctx.RolePrompt, "OLD-HEAD-REASON") {
						t.Fatalf("refresh used wrong evidence: order_head=%s evidence=%+v", order.HeadSHA, ctx.DocumentationGateEvidence)
					}
				}
				if !found {
					t.Fatal("refresh review not created")
				}
				approveDocumentationSeats(t, f, assessment)
				events, err := st.ListEvents(f.ctx, f.task.ID)
				if err != nil {
					t.Fatal(err)
				}
				for _, event := range events {
					if event.Kind == "review.completed" && strings.Contains(string(event.Payload), newHead) && !strings.Contains(string(event.Payload), `"documentation_assessment"`) {
						t.Fatalf("new-head decision lost assessment: %s", event.Payload)
					}
				}
			})
		})
	}
}
