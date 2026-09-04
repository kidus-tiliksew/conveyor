package logqueue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/queue"
)

// clockLoop invokes the order clock handler on a ticker, once at start.
func (rt *Runtime) clockLoop(ws *workspace) {
	defer rt.wg.Done()
	reg, ok := rt.registration(rt.clockKind)
	if !ok {
		return
	}
	args, _ := json.Marshal(queue.OrderClockArgs{WorkspaceID: ws.id})
	tick := 0
	fire := func() {
		tick++
		job := queue.Job{ID: fmt.Sprintf("clock/%s@%d", ws.id, tick), Kind: rt.clockKind, Attempt: 1, MaxAttempts: 1, Args: args}
		if err := reg.Handle(rt.runCtx, job); err != nil && !errors.Is(err, context.Canceled) {
			rt.opts.Logf("logqueue: order clock %s: %v", ws.id, err)
		}
	}
	fire()
	ticker := time.NewTicker(rt.opts.ClockInterval)
	defer ticker.Stop()
	for {
		select {
		case <-rt.loopCtx.Done():
			return
		case <-ticker.C:
			fire()
		}
	}
}
