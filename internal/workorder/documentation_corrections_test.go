package workorder

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
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

type documentationRefreshNoneStore struct{ store.Store }

func (s documentationRefreshNoneStore) CreateTask(ctx context.Context, task core.Task) error {
	task.SetupContract.RefreshReview = config.RefreshReviewNone
	return s.Store.CreateTask(ctx, task)
}

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
				f.service.DiscoverDocumentationPolicy = func(_ context.Context, _, revision string) githubtrigger.DocumentationPolicyDiscovery {
					return githubtrigger.DocumentationPolicyDiscovery{Revision: revision, State: "present", Policy: policy}
				}
				f.dispatchExistingRefresh(t, head, config.RefreshReviewDelta)
				before := f.eventCount(t)
				if result, err := f.submit("fixer", head); err == nil || !strings.Contains(err.Error(), "docs/api.md") {
					t.Fatalf("conflict fix bypassed admission: result=%v err=%v", result, err)
				}
				if f.eventCount(t) != before || f.jobState(t, f.order.JobID) != core.JobRunning {
					t.Fatal("refused conflict fix had submission side effects")
				}
			})
			t.Run("dispatched-conflict-fix-legacy-off-removes-same-head-evidence", func(t *testing.T) {
				for _, state := range []string{"absent", "unknown"} {
					for _, failure := range []string{"pr", "forge"} {
						t.Run(state+"/"+failure, func(t *testing.T) {
							st := documentationCorrectionStore(t, backend)
							f := newConflictSubmission(t, config.ExecutionSetup{}, func(store.Store) store.Store { return st })
							head, base := strings.Repeat("a", 40), strings.Repeat("b", 40)
							f.prHead = func(int) string { return head }
							f.dispatchExistingRefresh(t, head, config.RefreshReviewDelta)
							prior := core.DocumentationGateEvidence{TaskID: f.task.ID, HeadSHA: head, BaseSHA: base,
								PolicyJSON:   []byte(`{"schema_version":1,"docs":[{"path":"docs/api.md","governs":["internal/**"]}],"rule":{"none_statement":"docs: none","reason_required":true}}`),
								ChangedPaths: []string{"internal/server.go"}, AgentAuthoredBody: "docs: none STORED-REASON"}
							if err := st.RecordDocumentationGateEvidence(f.ctx, prior); err != nil {
								t.Fatal(err)
							}
							f.service.DiscoverDocumentationPolicy = func(_ context.Context, _, revision string) githubtrigger.DocumentationPolicyDiscovery {
								return githubtrigger.DocumentationPolicyDiscovery{Revision: revision, State: state}
							}
							if failure == "pr" {
								f.service.SubmissionPR = func(context.Context, string, string) (githubtrigger.SubmissionPullRequest, error) {
									t.Fatal("legacy-off submission read the PR before establishing opt-in")
									return githubtrigger.SubmissionPullRequest{}, errors.New("transient read failed")
								}
							} else {
								f.service.SubmissionPR = nil
							}
							if _, err := f.submit("fixer", head); err != nil {
								t.Fatal(err)
							}
							order, err := st.GetWorkOrder(f.ctx, f.order.ID)
							if err != nil {
								t.Fatal(err)
							}
							if order.State != core.WorkOrderSubmitted || f.jobState(t, f.order.JobID) != core.JobDone {
								t.Fatalf("legacy-off conflict fix did not complete: %+v", order)
							}
							if got, err := st.GetDocumentationGateEvidence(f.ctx, f.task.ID, head); !errors.Is(err, store.ErrNotFound) {
								t.Fatalf("legacy-off submission retained same-head evidence: %+v %v", got, err)
							}
						})
					}
				}
			})
			t.Run("known-policy-retries-transient-conflict-pr-read", func(t *testing.T) {
				st := documentationCorrectionStore(t, backend)
				f := newConflictSubmission(t, config.ExecutionSetup{}, func(store.Store) store.Store { return st })
				head, base := strings.Repeat("a", 40), strings.Repeat("b", 40)
				f.prHead = func(int) string { return head }
				f.dispatchExistingRefresh(t, head, config.RefreshReviewDelta)
				policy, err := docsconfig.Parse([]byte("schema_version: 1\ndocs:\n  - path: docs/api.md\n    governs: ['internal/**']\n"))
				if err != nil {
					t.Fatal(err)
				}
				known, calls := false, 0
				f.service.DiscoverDocumentationPolicy = func(_ context.Context, _, revision string) githubtrigger.DocumentationPolicyDiscovery {
					known = true
					return githubtrigger.DocumentationPolicyDiscovery{Revision: revision, State: "present", Policy: policy}
				}
				f.service.SubmissionPR = func(_ context.Context, _, branch string) (githubtrigger.SubmissionPullRequest, error) {
					if !known {
						t.Fatal("PR read preceded documentation discovery")
					}
					calls++
					if calls < 3 {
						return githubtrigger.SubmissionPullRequest{}, githubtrigger.StatusError(503, errors.New("server unavailable"))
					}
					p := githubtrigger.SubmissionPullRequest{Number: 1098, Body: "docs: none conflict-only correction"}
					p.Head.Ref, p.Head.SHA, p.Base.Ref, p.Base.SHA = branch, head, "main", base
					return p, nil
				}
				f.service.SubmissionChangedPaths = func(context.Context, *config.Config, core.Task) ([]string, error) {
					return []string{"internal/server.go"}, nil
				}
				if _, err := f.submit("fixer", head); err != nil {
					t.Fatal(err)
				}
				if calls != 3 {
					t.Fatalf("transient PR reads=%d", calls)
				}
				gate, err := st.GetDocumentationGateEvidence(f.ctx, f.task.ID, head)
				if err != nil || gate.Status != "" || gate.NoneReason != "conflict-only correction" {
					t.Fatalf("retried admission=%+v %v", gate, err)
				}
			})
			t.Run("failed-refresh-reviews-and-does-not-block-later-merge", func(t *testing.T) {
				for _, failure := range []string{"operator-only-none", "known-policy-unavailable"} {
					t.Run(failure, func(t *testing.T) {
						st := documentationCorrectionStore(t, backend)
						f := documentationSubmission(t, "present", "docs: none ORIGINAL-REASON", []string{"internal/server.go"}, func(store.Store) store.Store { return documentationRefreshNoneStore{st} })
						oldHead, newHead, base := strings.Repeat("a", 40), strings.Repeat("c", 40), strings.Repeat("b", 40)
						if _, err := f.service.SubmitForReview(f.ctx, f.job.ID, "implementer", oldHead); err != nil {
							t.Fatal(err)
						}
						assessment := &core.DocumentationAssessment{Applicable: true, Summary: "Original behavior documented", UpdatedPaths: []string{}, Unresolved: []string{}, Conflicts: []string{}}
						approveDocumentationSeats(t, f, assessment)
						if _, err := taskops.New(st).Perform(f.ctx, f.task.ID, taskops.Command{Kind: core.TaskInterventionApproveReview}); err != nil {
							t.Fatal(err)
						}
						current, err := st.GetTask(f.ctx, f.task.ID)
						if err != nil {
							t.Fatal(err)
						}
						clean := core.Task{ID: "zz-clean-later", Workspace: current.Workspace, Repo: current.Repo, Title: "Clean later task", Branch: "conveyor/zz-clean-later", BaseBranch: "main", State: core.TaskApproved, ApprovedHeadSHA: oldHead, ReviewedHeadSHA: oldHead, CreatedAt: time.Now()}
						if err := st.CreateTask(f.ctx, clean); err != nil {
							t.Fatal(err)
						}
						d := f.service.Dispatcher
						d.DiscoverDocumentationPolicy = f.service.DiscoverDocumentationPolicy
						if failure == "known-policy-unavailable" {
							d.DiscoverDocumentationPolicy = func(_ context.Context, _, revision string) githubtrigger.DocumentationPolicyDiscovery {
								return githubtrigger.DocumentationPolicyDiscovery{State: "unavailable", Revision: revision}
							}
						}
						d.SubmissionPR = func(context.Context, string, string) (githubtrigger.SubmissionPullRequest, error) {
							p := githubtrigger.SubmissionPullRequest{Number: 54, Body: "<!-- conveyor:task-link -->\ndocs: none OPERATOR-ONLY-REASON\n<!-- conveyor:lifecycle-end -->"}
							p.Head.Ref, p.Head.SHA, p.Base.Ref, p.Base.SHA = current.Branch, newHead, "main", base
							return p, nil
						}
						d.ReviewChangedPaths = func(context.Context, *config.Config, core.Task) ([]string, error) {
							return []string{"internal/human-push.go"}, nil
						}
						merged := false
						d.RequestMerge = func(_ context.Context, _ string, number int) error {
							if number != 55 {
								t.Fatalf("merge requested for failed documentation task: %d", number)
							}
							merged = true
							return nil
						}
						d.ViewPullRequest = func(_ context.Context, _, branch string) (githubtrigger.PullRequest, error) {
							if branch == clean.Branch {
								return githubtrigger.PullRequest{Number: 55, State: "open", HeadSHA: oldHead, BaseSHA: base, Mergeable: "MERGEABLE", Merged: merged}, nil
							}
							return githubtrigger.PullRequest{Number: 54, State: "open", HeadSHA: newHead, BaseSHA: base, Mergeable: "MERGEABLE"}, nil
						}
						if _, err := d.ReconcileMergeReadiness(f.ctx); err != nil {
							t.Fatal(err)
						}
						failed, err := st.GetTask(f.ctx, current.ID)
						if err != nil {
							t.Fatal(err)
						}
						later, err := st.GetTask(f.ctx, clean.ID)
						if err != nil {
							t.Fatal(err)
						}
						if current.SetupContract.RefreshReview != config.RefreshReviewNone || !failed.ApprovalStale || failed.RefreshReviewScope != config.RefreshReviewDelta || failed.ReviewedHeadSHA != newHead || !merged || later.State != core.TaskMerged {
							t.Fatalf("frozen none policy did not escalate failed refresh to delta: first=%+v later=%+v merged=%t", failed, later, merged)
						}
						events, err := st.ListEvents(f.ctx, current.ID)
						if err != nil {
							t.Fatal(err)
						}
						for _, event := range events {
							if event.Kind == "review.refresh_skipped" {
								t.Fatalf("failed gate took clean-update skip: %s", event.Payload)
							}
						}
						gate, err := st.GetDocumentationGateEvidence(f.ctx, current.ID, newHead)
						if err != nil || gate.Status == "" || gate.RefusalReason == "" || strings.Contains(gate.AgentAuthoredBody, "OPERATOR-ONLY-REASON") {
							t.Fatalf("failed current-head gate=%+v %v", gate, err)
						}
						orders, err := st.ListTaskWorkOrders(f.ctx, current.ID)
						if err != nil {
							t.Fatal(err)
						}
						found := false
						for _, order := range orders {
							if order.Stage != core.StageReview || order.HeadSHA != newHead || order.State != core.WorkOrderQueued {
								continue
							}
							found = true
							if order.ReviewKind != "refresh" || order.ReviewScope != config.RefreshReviewDelta || order.BaselineSHA != oldHead {
								t.Fatalf("failed gate did not mint a delta refresh seat: %+v", order)
							}
							reviewCtx, err := f.service.contextForOrder(f.ctx, order)
							if err != nil {
								t.Fatal(err)
							}
							if reviewCtx.DocumentationGateEvidence == nil || reviewCtx.DocumentationGateEvidence.Status != gate.Status || !strings.Contains(reviewCtx.RolePrompt, "approve is forbidden") || !strings.Contains(reviewCtx.RolePrompt, newHead) {
								t.Fatalf("failed gate not surfaced in actual review context: %+v", reviewCtx.DocumentationGateEvidence)
							}
							session := "failed-review-" + order.ID
							if _, err := f.service.Claim(f.ctx, order.ID, core.WorkOrderClaim{SessionID: session, ClientToken: session, ClaimantID: "reviewer", OwnerUserID: "usr-reviewer", Lease: time.Minute}); err != nil {
								t.Fatal(err)
							}
							no := false
							result := pipeline.Review{Verdict: "approve", ReasonCode: "verified", Summary: "Known-policy gate failed", RequirementCitations: &core.RequirementCitationAssessment{Applicable: false}, DoneCriteriaCoverage: &core.DoneCriteriaAssessment{Applicable: false, Summary: "no plan"}, GovernanceAssessment: &core.GovernanceAssessment{DesignApplicable: &no, DecisionCitable: &no}}
							if _, err := f.service.SubmitVerdict(f.ctx, order.ID, session, result); !errors.Is(err, store.ErrDocumentationUnresolved) {
								t.Fatalf("failed gate accepted approve: %v", err)
							}
							result.Verdict = "changes_requested"
							result.Feedback = "Resolve the known repository documentation gate before approval"
							if order.ReviewSeat == 2 {
								result.DocumentationAssessment = &core.DocumentationAssessment{Applicable: true, Summary: "Policy unavailable; documentation coverage cannot be determined", UpdatedPaths: []string{"unobserved/document.md"}, Unresolved: []string{}, Conflicts: []string{}}
							}
							if _, err := f.service.SubmitVerdict(f.ctx, order.ID, session, result); err != nil {
								t.Fatalf("failed gate rejected changes_requested without unavailable paths: %v", err)
							}
						}
						if !found {
							t.Fatal("failed known-policy refresh did not dispatch actual-head review")
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
				refreshReads := 0
				f.service.Dispatcher.SubmissionPR = func(context.Context, string, string) (githubtrigger.SubmissionPullRequest, error) {
					refreshReads++
					if refreshReads < 3 {
						return githubtrigger.SubmissionPullRequest{}, githubtrigger.StatusError(503, errors.New("transient refreshed PR read"))
					}
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
				if refreshReads != 3 {
					t.Fatalf("transient refresh reads=%d", refreshReads)
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

type documentationAppStore struct {
	store.WorkspaceGitHubAppStore
	credential core.WorkspaceGitHubAppCredential
}

func (s documentationAppStore) GetWorkspaceGitHubAppForUse(context.Context, string) (core.WorkspaceGitHubAppCredential, error) {
	return s.credential, nil
}

// Exercise conveyord's nil-discoverer prelude, not the injected policy shortcut.
func TestProductionConflictFixDocumentationDiscovery(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	private := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	for _, backend := range []string{"memory", "postgres", "singlestore"} {
		t.Run(backend, func(t *testing.T) {
			for _, persistent := range []bool{false, true} {
				t.Run(fmt.Sprintf("persistent-5xx=%t", persistent), func(t *testing.T) {
					st := documentationCorrectionStore(t, backend)
					f := newConflictSubmission(t, config.ExecutionSetup{}, func(store.Store) store.Store { return st })
					head, base := strings.Repeat("a", 40), strings.Repeat("b", 40)
					tree, directory, blob := strings.Repeat("c", 40), strings.Repeat("d", 40), strings.Repeat("e", 40)
					f.prHead = func(int) string { return head }
					f.dispatchExistingRefresh(t, head, config.RefreshReviewDelta)
					f.service.DiscoverDocumentationPolicy = nil
					f.service.WorkspaceGitHubApps = documentationAppStore{credential: core.WorkspaceGitHubAppCredential{
						WorkspaceGitHubAppStatus: core.WorkspaceGitHubAppStatus{WorkspaceID: "test", AppID: 41, InstallationID: 12, Connected: true},
						PrivateKey:               private,
					}}
					refReads, policyReads, prReads := 0, 0, 0
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						switch r.URL.Path {
						case "/app/installations/12":
							fmt.Fprint(w, `{"id":12,"app_id":41,"account":{"login":"acme"},"suspended_at":null}`)
						case "/app/installations/12/access_tokens":
							_ = json.NewEncoder(w).Encode(map[string]any{"token": "fixture-secret", "expires_at": time.Now().Add(time.Hour)})
						case "/installation/repositories":
							fmt.Fprint(w, `{"repositories":[{"full_name":"acme/app"}]}`)
						case "/repos/acme/app/git/ref/heads/main":
							refReads++
							if persistent || refReads == 1 {
								w.WriteHeader(http.StatusBadGateway)
								return
							}
							_ = json.NewEncoder(w).Encode(map[string]any{"object": map[string]string{"sha": base, "type": "commit"}})
						case "/repos/acme/app/git/commits/" + base:
							policyReads++
							_ = json.NewEncoder(w).Encode(map[string]any{"sha": base, "tree": map[string]string{"sha": tree}})
						case "/repos/acme/app/git/trees/" + tree:
							_ = json.NewEncoder(w).Encode(map[string]any{"sha": tree, "tree": []map[string]string{{"path": ".conveyor", "mode": "040000", "type": "tree", "sha": directory}}})
						case "/repos/acme/app/git/trees/" + directory:
							_ = json.NewEncoder(w).Encode(map[string]any{"sha": directory, "tree": []map[string]string{{"path": "docs.yaml", "mode": "100644", "type": "blob", "sha": blob}}})
						case "/repos/acme/app/git/blobs/" + blob:
							content := "schema_version: 1\ndocs:\n  - path: docs/api.md\n    governs: ['internal/**']\n"
							_ = json.NewEncoder(w).Encode(map[string]any{"encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte(content)), "size": len(content)})
						default:
							t.Errorf("unexpected GitHub endpoint: %s", r.URL.Path)
							w.WriteHeader(http.StatusNotFound)
						}
					}))
					defer server.Close()
					f.service.GitHubApps = githubtrigger.NewAppClient(server.Client(), server.URL)
					f.service.SubmissionPR = func(_ context.Context, _, branch string) (githubtrigger.SubmissionPullRequest, error) {
						prReads++
						p := githubtrigger.SubmissionPullRequest{Number: 1098, URL: "https://github.com/acme/app/pull/1098"}
						p.Head.Ref, p.Head.SHA, p.Base.Ref, p.Base.SHA = branch, head, "main", base
						return p, nil
					}
					f.service.SubmissionChangedPaths = func(context.Context, *config.Config, core.Task) ([]string, error) {
						return []string{"internal/server.go"}, nil
					}
					before := f.eventCount(t)
					result, err := f.submit("fixer", head)
					if persistent {
						if err != nil || result["refresh_already_dispatched"] != true || refReads != 3 || policyReads != 0 || prReads != 0 {
							t.Fatalf("exhausted discovery did not retain legacy admission: result=%v err=%v refs=%d policy=%d PR=%d", result, err, refReads, policyReads, prReads)
						}
					} else {
						if err == nil || !strings.Contains(err.Error(), "docs/api.md") || refReads != 2 || policyReads != 1 || prReads != 1 {
							t.Fatalf("transient base discovery bypassed gate: result=%v err=%v refs=%d policy=%d PR=%d", result, err, refReads, policyReads, prReads)
						}
						if f.eventCount(t) != before || f.jobState(t, f.order.JobID) != core.JobRunning {
							t.Fatal("refused production conflict-fix wrote submission side effects")
						}
					}
				})
			}
		})
	}
}
