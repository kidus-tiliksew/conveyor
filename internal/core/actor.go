package core

import (
	"errors"
	"strings"
)

// SystemActorID is the canonical identity of background work that no
// credential initiates (component-persistence, Actor context).
const SystemActorID = "conveyor"

// ErrMissingActor reports an event write whose context carries no complete
// typed actor. Every recorded event names its actor explicitly
// (req-accounts-and-membership REQ-3); no writer substitutes a default.
var ErrMissingActor = errors.New("event actor is required")

// Actor is the typed identity recorded on every event: a user, agent, worker,
// or system actor (req-accounts-and-membership REQ-3).
type Actor struct {
	ID   string
	Role ActorRole
}

// Complete reports whether the actor names both an identity and a role.
func (a Actor) Complete() bool {
	return strings.TrimSpace(a.ID) != "" && strings.TrimSpace(string(a.Role)) != ""
}

// SystemActor returns an explicit system actor. Without an argument it is the
// canonical conveyor identity; a nonblank internal subsystem ID such as
// "dispatcher" or "queue:<job-id>" replaces that identity and keeps the
// system role. Background work binds it where the work starts, never as a
// default inside a store method or request middleware.
func SystemActor(subsystem ...string) Actor {
	id := SystemActorID
	if len(subsystem) > 0 {
		if value := strings.TrimSpace(subsystem[0]); value != "" {
			id = value
		}
	}
	return Actor{ID: id, Role: ActorSystem}
}
