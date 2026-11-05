// Package backend selects the deployment store without exposing its driver.
package backend

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"

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
	st, _, err := OpenWithStats(ctx, database, options...)
	return st, err
}

// PoolStatsFunc reads a backend's process-wide pool counters.
type PoolStatsFunc func() dbmetrics.PoolStats

// ReadStatsFunc drains a backend's read ledger: the bytes, rows, and queries
// received since the previous drain and the top labels by bytes.
type ReadStatsFunc func(top int) dbmetrics.ReadInterval

// Stats holds the statistics an opened backend reports; a nil function
// reports nothing (component-runtime, Durable backend selection).
type Stats struct {
	Pool  PoolStatsFunc
	Reads ReadStatsFunc
}

var errMaxConnsRange = errors.New("database.max_conns must be an integer from 1 to 2147483647")

// errDatabaseURLRequired refuses a blank connection string. PostgreSQL would
// otherwise read it as the driver's defaults and dial a local socket.
var errDatabaseURLRequired = errors.New("the database URL is empty; set CONVEYOR_DATABASE_URL")

// OpenWithStats is Open plus the opened backend's statistics functions:
// pool statistics for PostgreSQL and read statistics for both durable
// backends. Each capability is a compiler-checked method value, not a
// discovered interface (component-runtime, Durable backend selection).
//
// This is the one path from a connection string to a durable store, for
// conveyord, conveyor init, and conveyor user. The deployment loader keeps
// CONVEYOR_DATABASE_URL untrimmed and a file init wrote holds no URL, so the
// surrounding whitespace comes off here. conveyord then connects with the URL
// init used, and nothing inside the string changes (req-deployment-and-releases
// REQ-2, AC-2.3).
func OpenWithStats(ctx context.Context, database config.Database, options ...Option) (store.Backend, Stats, error) {
	connection := strings.TrimSpace(database.URL)
	switch database.Backend {
	case "postgres":
		if connection == "" {
			return nil, Stats{}, errDatabaseURLRequired
		}
		var postgresOptions []postgres.OpenOption
		if database.MaxConns != nil {
			if *database.MaxConns < 1 || *database.MaxConns > math.MaxInt32 {
				return nil, Stats{}, errMaxConnsRange
			}
			postgresOptions = append(postgresOptions, postgres.WithMaxConns(int32(*database.MaxConns)))
		}
		st, err := postgres.Open(ctx, connection, postgresOptions...)
		if err != nil {
			return nil, Stats{}, err
		}
		return st, Stats{Pool: st.PoolStats, Reads: st.ReadStats}, nil
	case "singlestore":
		if connection == "" {
			return nil, Stats{}, errDatabaseURLRequired
		}
		st, err := singlestore.Open(ctx, connection)
		if err != nil {
			return nil, Stats{}, err
		}
		return st, Stats{Reads: st.ReadStats}, nil
	case "memory":
		for _, option := range options {
			if option == AllowVolatile {
				return store.NewVolatileBackend(), Stats{}, nil
			}
		}
		return nil, Stats{}, ErrVolatileBackend
	default:
		return nil, Stats{}, fmt.Errorf("%w: %q", ErrUnknownBackend, database.Backend)
	}
}
