package store

import (
	"context"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

type Actor struct {
	ID   string
	Role core.ActorRole
}

type actorContextKey struct{}

func WithActor(ctx context.Context, actor Actor) context.Context {
	return context.WithValue(ctx, actorContextKey{}, actor)
}

func ActorFromContext(ctx context.Context) Actor {
	if actor, ok := ctx.Value(actorContextKey{}).(Actor); ok && actor.ID != "" && actor.Role != "" {
		return actor
	}
	return Actor{ID: "conveyor", Role: core.ActorSystem}
}
