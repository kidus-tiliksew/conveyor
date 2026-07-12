package routing

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
)

type fakePool struct {
	request ClaimRequest
	result  Credential
	err     error
	action  string
}

func (p *fakePool) RescueTaskCredentialLeases(_ context.Context, taskID, currentJobID string) error {
	p.request.TaskID = taskID
	p.request.JobID = currentJobID
	return nil
}

func (p *fakePool) ClaimCredential(_ context.Context, request ClaimRequest) (Credential, error) {
	p.request = request
	return p.result, p.err
}
func (p *fakePool) ReleaseCredential(context.Context, string, string, string) error {
	p.action = "release"
	return nil
}
func (p *fakePool) ThrottleCredential(context.Context, string, string, string, int64) error {
	p.action = "throttle"
	return nil
}

func TestRouterUsesStagePreferenceAndThrottles(t *testing.T) {
	pool := &fakePool{result: Credential{ID: "cred-1", Harness: "claude-code", Kind: "personal_sub"}}
	router := New(pool, config.Routing{
		OwnerID: "alice", LeaseSeconds: 600,
		Stages: map[string]config.StageRoute{"implement": {Harnesses: []string{"claude-code", "codex"}, ModelTier: "mid", BudgetUSD: 2}},
	})
	selection, err := router.Select(context.Background(), "task-1", "job-1", core.StageImplement, "")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(pool.request.Harnesses, []string{"claude-code", "codex"}) || selection.ModelTier != "mid" {
		t.Fatalf("request=%+v selection=%+v", pool.request, selection)
	}
	if err := router.Complete(context.Background(), selection, Outcome{RateLimited: true}); err != nil {
		t.Fatal(err)
	}
	if pool.action != "throttle" {
		t.Fatalf("action=%q", pool.action)
	}
}

func TestRouterPreservesNoCapacity(t *testing.T) {
	router := New(&fakePool{err: ErrNoCapacity}, config.Routing{OwnerID: "alice", LeaseSeconds: 60})
	_, err := router.Select(context.Background(), "task", "job", core.StageImplement, "")
	if !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("error=%v", err)
	}
}

func TestRouterExcludesImplementerHarnessForReview(t *testing.T) {
	pool := &fakePool{result: Credential{ID: "reviewer", Harness: "claude-code"}}
	router := New(pool, config.Routing{OwnerID: "alice", LeaseSeconds: 60, Stages: map[string]config.StageRoute{
		"review": {Harnesses: []string{"claude-code", "codex"}},
	}})
	if _, err := router.Select(context.Background(), "task", "review-job", core.StageReview, "codex"); err != nil {
		t.Fatal(err)
	}
	if pool.request.ExcludeHarness != "codex" {
		t.Fatalf("excluded harness=%q", pool.request.ExcludeHarness)
	}
}

func TestStaticRouterRecordsConfiguredDispatchMetadata(t *testing.T) {
	credential := Credential{ID: "local-claude", Harness: "claude-code", Kind: "personal_sub", Ref: "secretref://demo/default/TOKEN"}
	router := NewStatic(credential, config.Routing{Stages: map[string]config.StageRoute{
		"implement": {Harnesses: []string{"claude-code"}, ModelTier: "mid", BudgetUSD: 2.5},
	}})
	selection, err := router.Select(context.Background(), "task-1", "job-1", core.StageImplement, "")
	if err != nil {
		t.Fatal(err)
	}
	if selection.ID != credential.ID || selection.Harness != credential.Harness || selection.Kind != credential.Kind || selection.BudgetUSD != 2.5 {
		t.Fatalf("selection = %+v", selection)
	}
}
