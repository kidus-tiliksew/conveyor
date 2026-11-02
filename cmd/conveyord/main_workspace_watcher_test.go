package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/changes"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/dispatch"
)

// TestWorkspaceRegistrationWatcher: a workspace notice for a workspace
// created after start, as a peer's creation arrives, registers it; a failed
// registration writes an error line and is retried after the injected
// delay, which doubles; a later notice converges at once (component-runtime,
// Start-up checks and background loops; req-quiet-idle-runtime AC-4.3).
func TestWorkspaceRegistrationWatcher(t *testing.T) {
	var mu sync.Mutex
	persisted := []core.Workspace{{ID: "startup"}}
	failures := map[string]int{"flaky": 2}
	registered := make(chan string, 8)
	var lines []string
	logf := func(format string, args ...any) {
		mu.Lock()
		lines = append(lines, fmt.Sprintf(format, args...))
		mu.Unlock()
	}
	registrar := dispatch.NewWorkspaceQueueRegistrar([]string{"startup"}, func(workspace string) error {
		mu.Lock()
		defer mu.Unlock()
		if failures[workspace] > 0 {
			failures[workspace]--
			return errors.New("runtime config unavailable")
		}
		registered <- workspace
		return nil
	}, func(string, ...any) {})
	retries := make(chan time.Time)
	var delays []time.Duration
	hub := changes.NewHub()
	ctx, cancel := context.WithCancel(t.Context())
	done := startWorkspaceRegistrationWatcher(ctx, workspaceRegistrationWatcher{
		notices: hub.SubscribeAll(changes.Workspace),
		list: func(context.Context) ([]core.Workspace, error) {
			mu.Lock()
			defer mu.Unlock()
			return append([]core.Workspace(nil), persisted...), nil
		},
		converge: registrar.Converge, logf: logf,
		after: func(d time.Duration) <-chan time.Time {
			mu.Lock()
			delays = append(delays, d)
			mu.Unlock()
			return retries
		},
	})
	defer func() {
		cancel()
		<-done
	}()
	await := func(want string) {
		t.Helper()
		select {
		case got := <-registered:
			if got != want {
				t.Fatalf("registered %s, want %s", got, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("%s was not registered", want)
		}
	}

	// A peer created a workspace: its republished notice registers it.
	mu.Lock()
	persisted = append(persisted, core.Workspace{ID: "peer-created"})
	mu.Unlock()
	hub.Publish(changes.Notice{Workspace: "peer-created", Kind: changes.Workspace})
	await("peer-created")

	// A registration that fails is logged and retried after the delay.
	mu.Lock()
	persisted = append(persisted, core.Workspace{ID: "flaky"})
	mu.Unlock()
	hub.Publish(changes.Notice{Workspace: "flaky", Kind: changes.Workspace})
	retries <- time.Now()
	// The retry failed again; a later notice converges at once.
	hub.Publish(changes.Notice{Kind: changes.Workspace})
	await("flaky")
	mu.Lock()
	defer mu.Unlock()
	failed := 0
	for _, line := range lines {
		if strings.Contains(line, "level=error") && strings.Contains(line, "workspace queue registration failed") && strings.Contains(line, "runtime config unavailable") {
			failed++
		}
	}
	if failed != 2 || len(delays) != 2 || delays[0] != 5*time.Second || delays[1] != 10*time.Second {
		t.Fatalf("error lines=%d delays=%v lines=%v", failed, delays, lines)
	}
}
