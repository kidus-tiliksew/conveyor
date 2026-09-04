package dispatch

import (
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/eventlog"
	queueargs "github.com/kidus-tiliksew/conveyor/internal/queue"
	"github.com/kidus-tiliksew/conveyor/internal/queue/logqueue"
)

// testJob builds a queue job the way the runtime would hand it to a
// handler. The args types are plain structs, so marshalling cannot fail.
func testJob(args interface{ Kind() string }, id int64, attempt, maxAttempts int) queueargs.Job {
	encoded, err := json.Marshal(args)
	if err != nil {
		panic(err)
	}
	return queueargs.Job{ID: strconv.FormatInt(id, 10), Kind: args.Kind(), Attempt: attempt, MaxAttempts: maxAttempts, Args: encoded}
}

// testRuntime builds the queue runtime the daemon would, with every
// registration bound, polling fast enough for tests. adjust may rewrite a
// registration before it is bound, for tests that shorten a retry policy.
func testRuntime(t *testing.T, log eventlog.Store, d *Dispatcher, shutdown *ShutdownMarker, workspaces []string, configs map[string]*config.Config, adjust func(*queueargs.Registration)) (*logqueue.Runtime, error) {
	t.Helper()
	rescueAfter, err := QueueRescueThreshold(configs)
	if err != nil {
		return nil, err
	}
	runtime := logqueue.NewRuntime(log, logqueue.Options{
		Workspaces: workspaces, RescueStuckAfter: rescueAfter, PollInterval: 50 * time.Millisecond,
		ClockInterval: 200 * time.Millisecond, WorkerID: "test", Logf: t.Logf,
	})
	for _, registration := range d.Registrations(shutdown) {
		if adjust != nil {
			adjust(&registration)
		}
		runtime.Register(registration)
	}
	return runtime, nil
}
