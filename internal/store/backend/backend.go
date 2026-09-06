// Package backend selects the deployment store without exposing its driver.
package backend

import (
	"context"
	"errors"
	"fmt"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres"
	"github.com/kidus-tiliksew/conveyor/internal/store/singlestore"
)

var (
	ErrVolatileBackend = errors.New("in-memory store is a test backend; explicit backend.AllowVolatile is required")
	ErrUnknownBackend  = errors.New("unknown store backend")
)

type Option uint8

// AllowVolatile permits test callers to select the in-memory backend.
const AllowVolatile Option = 1

// AllowExperimental permits incomplete backends in tests; daemon wiring never supplies it.
const AllowExperimental Option = 2

var _ store.Backend = (*postgres.Store)(nil)

func Open(ctx context.Context, database config.Database, options ...Option) (store.Backend, error) {
	switch database.Backend {
	case "postgres":
		return postgres.Open(ctx, database.URL)
	case "singlestore":
		for _, option := range options {
			if option == AllowExperimental {
				return singlestore.Open(ctx, database.URL)
			}
		}
		return nil, store.ErrBackendNotAdmitted
	case "memory":
		for _, option := range options {
			if option == AllowVolatile {
				return store.NewVolatileBackend(), nil
			}
		}
		return nil, ErrVolatileBackend
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnknownBackend, database.Backend)
	}
}
