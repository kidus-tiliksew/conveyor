package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DefaultMaxConns sizes the pool when neither the connection string's
// pool_max_conns nor WithMaxConns names a size. The one pool serves HTTP
// handlers, the log-queue runtime, the reconcile and monitor loops, and
// long-held side-effect and planning lock connections (component-runtime,
// Durable backend selection).
const DefaultMaxConns int32 = 32

// OpenOption adjusts how Open builds the pool.
type OpenOption func(*openOptions)

type openOptions struct {
	maxConns    int32
	maxConnsSet bool
	now         func() time.Time
	// acquireStarted is a test-only hook called at every traced pool
	// acquisition start; no exported option sets it.
	acquireStarted func(context.Context)
}

// WithMaxConns sets the pool's maximum size unless the connection string
// names pool_max_conns explicitly, which wins.
func WithMaxConns(n int32) OpenOption {
	return func(options *openOptions) {
		options.maxConns = n
		options.maxConnsSet = true
	}
}

var errNonPositiveMaxConns = errors.New("configure Postgres pool: maximum connections must be positive")

func resolveOpenOptions(options []OpenOption) (openOptions, error) {
	resolved := openOptions{maxConns: DefaultMaxConns, now: time.Now}
	for _, option := range options {
		if option != nil {
			option(&resolved)
		}
	}
	if resolved.maxConnsSet && resolved.maxConns < 1 {
		return openOptions{}, errNonPositiveMaxConns
	}
	return resolved, nil
}

// poolConfig parses databaseURL with pgx's own parsers. An explicit
// pool_max_conns is detected from the parsed runtime parameters before
// pgxpool consumes them, so a value equal to pgx's CPU-derived default still
// counts as explicit. Every other parsed option is kept, and the request
// tracer is installed before the pool exists. pgconn's parse errors redact the
// password.
func poolConfig(databaseURL string, options openOptions) (*pgxpool.Config, error) {
	connConfig, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("configure Postgres pool: %w", err)
	}
	_, explicit := connConfig.RuntimeParams["pool_max_conns"]
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("configure Postgres pool: %w", err)
	}
	if !explicit {
		config.MaxConns = options.maxConns
	}
	config.ConnConfig.Tracer = newRequestTracer(config.ConnConfig.Tracer, options.now, options.acquireStarted)
	return config, nil
}
