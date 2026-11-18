package storetest

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/changes"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// StatementMemoFunc builds the HTTP handler of a server over st for
// workspace, authenticating the bearer token StatementMemoToken. storetest
// cannot import httpapi: httpapi tests import storetest.
type StatementMemoFunc func(st store.Store, workspace string) http.Handler

// StatementMemoToken is the bearer token a StatementMemoFunc server accepts.
const StatementMemoToken = "statement-memo-reader"

// memoProbeStore counts the validation reads a statement read makes. The
// response memo makes them only when it builds, so a read that makes none
// was answered from a held entry.
type memoProbeStore struct {
	store.Store
	hub         *changes.Hub
	validations atomic.Int64
}

// Changes hands the server the backend's hub, so the memo hears its notices.
func (s *memoProbeStore) Changes() *changes.Hub { return s.hub }

func (s *memoProbeStore) ReadActivityValidation(ctx context.Context, now time.Time) (store.ActivityValidation, error) {
	s.validations.Add(1)
	return s.Store.ReadActivityValidation(ctx, now)
}

// RunWorkspaceNoticeConformance proves every backend raises one workspace
// notice when a requirement version, System Design version, or decision is
// proposed, confirmed, or dismissed, and when a workspace role is granted,
// changed, or revoked, an invitation included, and when an invitation is
// redeemed at sign-up. Each notice reaches a workspace subscriber before the
// write returns, so the response memo is dropped within the next request:
// with memo set, a warm GET /v1/next-actions builds again right after each
// write. A refused write raises no notice (DEC-140(3); component-persistence
// invariant 23; component-next-action invariant 58).
func RunWorkspaceNoticeConformance(t *testing.T, x Fixture, memo StatementMemoFunc, membership bool) {
	st, ws := x.Backend, x.Workspace
	hub := st.Changes()
	if hub == nil {
		t.Fatal("backend has no change-notice hub")
	}
	ctx := store.WithActor(x.Context, store.Actor{ID: requirementConformanceActor, Role: core.ActorHuman})
	var probe *memoProbeStore
	var handler http.Handler
	if memo != nil {
		probe = &memoProbeStore{Store: st, hub: hub}
		handler = memo(probe, ws)
	}
	read := func(t *testing.T) int64 {
		t.Helper()
		before := probe.validations.Load()
		request := httptest.NewRequest(http.MethodGet, "/v1/next-actions", nil)
		request.Header.Set("Authorization", "Bearer "+StatementMemoToken)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("next-actions status=%d body=%s", response.Code, response.Body.String())
		}
		return probe.validations.Load() - before
	}
	// expectNotice runs write and proves that its workspace notice was
	// already delivered when write returned, and that a warm memo entry was
	// built again by the next read.
	expectNotice := func(t *testing.T, what string, write func(t *testing.T)) {
		t.Helper()
		notices := hub.Subscribe(ws, changes.Workspace)
		defer notices.Close()
		if handler != nil {
			read(t)
			if read(t) != 0 {
				t.Fatalf("%s: the memo was not warm before the write", what)
			}
		}
		write(t)
		select {
		case n := <-notices.C():
			if n.Kind != changes.Workspace || n.Workspace != ws {
				t.Fatalf("%s: notice=%+v", what, n)
			}
		default:
			t.Fatalf("%s raised no workspace notice before it returned", what)
		}
		if handler != nil && read(t) == 0 {
			t.Fatalf("%s: the next read answered from the memo", what)
		}
		// With peers on, PostgreSQL's listener republishes the notice.
		settle(notices)
	}

	t.Run("Requirement", func(t *testing.T) {
		var version core.RequirementVersion
		expectNotice(t, "requirement proposal", func(t *testing.T) {
			var err error
			_, version, err = st.CreateRequirement(ctx, core.Requirement{ID: "req-workspace-notice", Title: "Workspace notice"}, core.RequirementVersion{Content: "# v1", Origin: core.RequirementOriginOperator, Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "First."}}})
			requireOK(t, err)
		})
		expectNotice(t, "requirement confirmation", func(t *testing.T) {
			_, _, err := st.ConfirmRequirementVersion(ctx, "req-workspace-notice", version.Version)
			requireOK(t, err)
		})
		var proposed core.RequirementVersion
		expectNotice(t, "requirement revision", func(t *testing.T) {
			var err error
			proposed, err = st.ProposeRequirementVersion(ctx, core.RequirementVersion{RequirementID: "req-workspace-notice", Content: "# v2", Origin: core.RequirementOriginOperator, Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Second."}}})
			requireOK(t, err)
		})
		expectNotice(t, "requirement dismissal", func(t *testing.T) {
			_, _, err := st.DismissRequirementVersion(ctx, "req-workspace-notice", proposed.Version)
			requireOK(t, err)
		})
	})
	t.Run("SystemDesign", func(t *testing.T) {
		var version core.SystemDesignVersion
		expectNotice(t, "System Design proposal", func(t *testing.T) {
			var err error
			_, version, err = st.CreateSystemDesign(ctx, core.SystemDesign{ID: "component-workspace-notice", Title: "Workspace notice", Category: "Component design"}, core.SystemDesignVersion{Content: designContent("workspace notice"), Origin: core.SystemDesignOriginOperator})
			requireOK(t, err)
		})
		expectNotice(t, "System Design confirmation", func(t *testing.T) {
			_, _, err := st.ConfirmSystemDesignVersion(ctx, "component-workspace-notice", version.Version)
			requireOK(t, err)
		})
		var proposed core.SystemDesignVersion
		expectNotice(t, "System Design revision", func(t *testing.T) {
			var err error
			proposed, err = st.ProposeSystemDesignVersion(ctx, core.SystemDesignVersion{DocumentID: "component-workspace-notice", Content: designContent("workspace notice revised"), Origin: core.SystemDesignOriginOperator})
			requireOK(t, err)
		})
		expectNotice(t, "System Design dismissal", func(t *testing.T) {
			_, _, err := st.DismissSystemDesignVersion(ctx, "component-workspace-notice", proposed.Version)
			requireOK(t, err)
		})
	})
	t.Run("Decision", func(t *testing.T) {
		var confirmed, dismissed core.Decision
		expectNotice(t, "decision proposal", func(t *testing.T) {
			var err error
			confirmed, err = st.ProposeDecision(ctx, core.Decision{Statement: "Workspace notice decision.", Context: "Notice fixture.", AlternativesRejected: "None.", Origin: core.DecisionOriginOperator})
			requireOK(t, err)
		})
		expectNotice(t, "decision confirmation", func(t *testing.T) {
			_, err := st.ConfirmDecision(ctx, confirmed.ID)
			requireOK(t, err)
		})
		var err error
		dismissed, err = st.ProposeDecision(ctx, core.Decision{Statement: "Dismissed notice decision.", Context: "Notice fixture.", AlternativesRejected: "None.", Origin: core.DecisionOriginOperator})
		requireOK(t, err)
		expectNotice(t, "decision dismissal", func(t *testing.T) {
			_, err := st.DismissDecision(ctx, dismissed.ID)
			requireOK(t, err)
		})
	})
	if !membership {
		return
	}
	t.Run("Membership", func(t *testing.T) {
		ownerCtx, owner := bootstrapOwner(t, x)
		var member core.IdentityUser
		member, err := st.ProvisionIdentityUser(ownerCtx, "workspace-notice-member@example.test", "Notice Member")
		requireOK(t, err)
		expectNotice(t, "role grant", func(t *testing.T) {
			_, err := st.GrantWorkspaceRole(ownerCtx, member.Email, ws, core.WorkspaceRoleViewer)
			requireOK(t, err)
		})
		expectNotice(t, "role change", func(t *testing.T) {
			_, err := st.GrantWorkspaceRole(ownerCtx, member.Email, ws, core.WorkspaceRoleExecutor)
			requireOK(t, err)
		})
		expectNotice(t, "role revocation", func(t *testing.T) {
			requireOK(t, st.RevokeWorkspaceRole(ownerCtx, member.ID, ws))
		})
		expectNotice(t, "invitation", func(t *testing.T) {
			_, err := st.GrantWorkspaceRole(ownerCtx, "workspace-notice-invitee@example.test", ws, core.WorkspaceRoleViewer)
			requireOK(t, err)
		})
		expectNotice(t, "invitation revocation", func(t *testing.T) {
			requireOK(t, st.RevokeWorkspaceInvitation(ownerCtx, "workspace-notice-invitee@example.test", ws))
		})
		_, err = st.GrantWorkspaceRole(ownerCtx, "workspace-notice-signup@example.test", ws, core.WorkspaceRoleContributor)
		requireOK(t, err)
		expectNotice(t, "invitation redeemed at sign-up", func(t *testing.T) {
			_, err := st.ProvisionIdentityUser(x.Context, "workspace-notice-signup@example.test", "Notice Signup")
			requireOK(t, err)
		})

		// A refused revocation of the last operator rolls back and raises
		// nothing.
		notices := hub.Subscribe(ws, changes.Workspace)
		defer notices.Close()
		settle(notices)
		if err := st.RevokeWorkspaceRole(ownerCtx, owner.ID, ws); !errors.Is(err, store.ErrLastWorkspaceOperator) {
			t.Fatalf("last operator revocation err=%v, want ErrLastWorkspaceOperator", err)
		}
		expectQuiet(t, notices, "refused revocation of the last operator")
	})
}
