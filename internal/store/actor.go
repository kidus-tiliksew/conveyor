package store

import (
	"context"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

type Actor struct {
	ID   string
	Role core.ActorRole
}

type actorContextKey struct{}

type workspaceContextKey struct{}

func WithActor(ctx context.Context, actor Actor) context.Context {
	return context.WithValue(ctx, actorContextKey{}, actor)
}

func ActorFromContext(ctx context.Context) Actor {
	if actor, ok := ctx.Value(actorContextKey{}).(Actor); ok && actor.ID != "" && actor.Role != "" {
		return actor
	}
	return Actor{ID: "conveyor", Role: core.ActorSystem}
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
