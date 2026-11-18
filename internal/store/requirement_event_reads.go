package store

import (
	"context"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// RequirementEventReadStore reads workspace events for exactly the requested requirements.
// Every requested ID has a non-nil slice, including IDs without events.
type RequirementEventReadStore interface {
	ListRequirementEventsForRequirements(context.Context, []string) (map[string][]core.Event, error)
}
