package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

func UserActorID(id string) string   { return "user:" + strings.TrimSpace(id) }
func AgentActorID(id string) string  { return "agent:" + strings.TrimSpace(id) }
func WorkerActorID(id string) string { return "worker:" + strings.TrimSpace(id) }

type Actor struct {
	ID   string
	Role core.ActorRole
}

type actorContextKey struct{}

type workspaceContextKey struct{}

type credentialContextKey struct{}

func WithActor(ctx context.Context, actor Actor) context.Context {
	return context.WithValue(ctx, actorContextKey{}, actor)
}

func WithCredential(ctx context.Context, credential core.AuthenticatedCredential) context.Context {
	return context.WithValue(ctx, credentialContextKey{}, credential)
}

func CredentialFromContext(ctx context.Context) (core.AuthenticatedCredential, bool) {
	credential, ok := ctx.Value(credentialContextKey{}).(core.AuthenticatedCredential)
	return credential, ok && credential.ID != "" && credential.OwnerUserID != ""
}

func ActorFromContext(ctx context.Context) Actor {
	if actor, ok := ctx.Value(actorContextKey{}).(Actor); ok && actor.ID != "" && actor.Role != "" {
		return actor
	}
	return Actor{ID: "conveyor", Role: core.ActorSystem}
}

// WorkOrderOwnerUserID resolves the executing human from durable claim state.
// Client-supplied claimant text is never treated as authority: run claimants
// use the server-derived run:<user-id> form and worker claims use durable
// enrollment ownership (req-260821-830dbf AC-3.1).
func WorkOrderOwnerUserID(ctx context.Context, st Store, order core.WorkOrder) (string, error) {
	if order.WorkerID != "" {
		workers, err := st.ListWorkers(ctx)
		if err != nil {
			return "", fmt.Errorf("list workers for work-order owner: %w", err)
		}
		for _, worker := range workers {
			if worker.ID == order.WorkerID && strings.TrimSpace(worker.OwnerUserID) != "" {
				return strings.TrimSpace(worker.OwnerUserID), nil
			}
		}
		return "", fmt.Errorf("work order %s worker %s has no durable owner", order.ID, order.WorkerID)
	}
	owner := strings.TrimSpace(strings.TrimPrefix(order.ClaimantID, core.TaskRunClaimantPrefix))
	if owner == "" || order.ClaimantID != core.TaskRunClaimantID(owner) {
		return "", fmt.Errorf("work order %s has no authenticated executing user", order.ID)
	}
	return owner, nil
}

// ApprovingOperatorUserID returns the user actor on the latest durable review
// approval. Spec and plan approvals cannot govern an already reviewed head,
// so the newest approval is the merge-releasing act.
func ApprovingOperatorUserID(ctx context.Context, st Store, taskID string) (string, bool, error) {
	items, err := st.ListInterventions(ctx, taskID)
	if err != nil {
		return "", false, err
	}
	for i := len(items) - 1; i >= 0; i-- {
		item := items[i]
		if item.Action != core.InterventionApprove || item.ActorRole != core.ActorUser {
			continue
		}
		userID := strings.TrimSpace(strings.TrimPrefix(item.ActorID, "user:"))
		if userID != "" && item.ActorID == UserActorID(userID) {
			return userID, true, nil
		}
	}
	return "", false, nil
}

func GitAuthorForUser(ctx context.Context, identities CallerIdentityStore, userID string) (core.GitAuthorIdentity, error) {
	identity, err := identities.GetCallerIdentity(ctx, userID, "")
	if err != nil {
		return core.GitAuthorIdentity{}, fmt.Errorf("resolve git author for user %s: %w", userID, err)
	}
	if strings.TrimSpace(identity.DisplayName) == "" || strings.TrimSpace(identity.Email) == "" {
		return core.GitAuthorIdentity{}, fmt.Errorf("user %s has no complete git author identity", userID)
	}
	return core.GitAuthorIdentity{Name: strings.TrimSpace(identity.DisplayName), Email: strings.TrimSpace(identity.Email)}, nil
}

// WithWorkspace binds an immutable workspace identity to one request or
// background operation. Store implementations fail closed when it is absent.
func WithWorkspace(ctx context.Context, workspace string) context.Context {
	return context.WithValue(ctx, workspaceContextKey{}, strings.TrimSpace(workspace))
}

// WorkspaceFromContext returns the explicit workspace identity carried by the
// caller. It never falls back to process-global deployment state.
func WorkspaceFromContext(ctx context.Context) (string, bool) {
	workspace, ok := ctx.Value(workspaceContextKey{}).(string)
	return workspace, ok && workspace != ""
}
