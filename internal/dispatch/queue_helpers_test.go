package dispatch

import (
	"encoding/json"
	"strconv"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	queueargs "github.com/kidus-tiliksew/conveyor/internal/queue"
	"github.com/kidus-tiliksew/conveyor/internal/queue/logqueue"
	"github.com/kidus-tiliksew/conveyor/internal/queue/riverqueue"
)

// testJob builds a port job the way a driver would hand it to a handler.
// The args types are plain structs, so marshalling cannot fail.
func testJob(args interface{ Kind() string }, id int64, attempt, maxAttempts int) queueargs.Job {
	encoded, err := json.Marshal(args)
	if err != nil {
		panic(err)
	}
	return queueargs.Job{ID: strconv.FormatInt(id, 10), Kind: args.Kind(), Attempt: attempt, MaxAttempts: maxAttempts, Args: encoded}
}

// dispatchRegistration is the dispatch-task registration, retry policy
// included, for tests that drive a single job through a driver.
func dispatchRegistration(d *Dispatcher) queueargs.Registration {
	for _, registration := range d.Registrations(&ShutdownMarker{}) {
		if registration.Kind == (queueargs.DispatchTaskArgs{}).Kind() {
			return registration
		}
	}
	panic("dispatch task registration missing")
}

// testRuntime builds the River runtime the daemon would, with every
// registration bound, for integration tests that need real workers.
func testRuntime(t *testing.T, pool *pgxpool.Pool, d *Dispatcher, shutdown *ShutdownMarker, workspaces []string, configs map[string]*config.Config, shadow *logqueue.Shadow) (*riverqueue.Runtime, error) {
	t.Helper()
	rescueAfter, err := RiverRescueStuckJobsAfter(configs)
	if err != nil {
		return nil, err
	}
	runtime, err := riverqueue.NewRuntime(pool, riverqueue.Options{RescueStuckAfter: rescueAfter, Workspaces: workspaces, Shadow: shadow})
	if err != nil {
		return nil, err
	}
	for _, registration := range d.Registrations(shutdown) {
		runtime.Register(registration)
	}
	return runtime, nil
}
