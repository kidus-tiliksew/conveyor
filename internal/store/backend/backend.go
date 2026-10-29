// Package backend selects the deployment store without exposing its driver.
package backend

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/dbmetrics"
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

var _ store.Backend = (*postgres.Store)(nil)
var _ store.Backend = (*singlestore.Store)(nil)

func Open(ctx context.Context, database config.Database, options ...Option) (store.Backend, error) {
	st, _, err := OpenWithPoolStats(ctx, database, options...)
	return st, err
}

// PoolStatsFunc reads a backend's process-wide pool counters.
type PoolStatsFunc func() dbmetrics.PoolStats

var errMaxConnsRange = errors.New("database.max_conns must be an integer from 1 to 2147483647")

// OpenWithPoolStats is Open plus the opened backend's pool statistics
// function, which is nil for backends that report none. The capability is a
// compiler-checked method value, not a discovered interface
// (component-runtime, Durable backend selection).
func OpenWithPoolStats(ctx context.Context, database config.Database, options ...Option) (store.Backend, PoolStatsFunc, error) {
	switch database.Backend {
	case "postgres":
		var postgresOptions []postgres.OpenOption
		if database.MaxConns != nil {
			if *database.MaxConns < 1 || *database.MaxConns > math.MaxInt32 {
				return nil, nil, errMaxConnsRange
			}
			postgresOptions = append(postgresOptions, postgres.WithMaxConns(int32(*database.MaxConns)))
		}
		st, err := postgres.Open(ctx, database.URL, postgresOptions...)
		if err != nil {
			return nil, nil, err
		}
		return st, st.PoolStats, nil
	case "singlestore":
		st, err := singlestore.Open(ctx, database.URL)
		if err != nil {
			return nil, nil, err
		}
		return st, nil, nil
	case "memory":
		for _, option := range options {
			if option == AllowVolatile {
				return store.NewVolatileBackend(), nil, nil
			}
		}
		return nil, nil, ErrVolatileBackend
	default:
		return nil, nil, fmt.Errorf("%w: %q", ErrUnknownBackend, database.Backend)
	}
}
