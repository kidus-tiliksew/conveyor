package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	workerservice "github.com/kidus-tiliksew/conveyor/internal/worker"
)

// workerAttemptCaptureTimeout bounds the single parent-launcher delivery of
// one attempt's ending capture. Delivery happens after the child process group
// has stopped and its output is flushed, never as a continuous stream (DEC-26).
var workerAttemptCaptureTimeout = 10 * time.Second

// workerAttemptCaptureTestHook observes each finished capture delivery so
// tests can assert ordering and once-only delivery without sleeping.
var workerAttemptCaptureTestHook func(core.WorkOrderAttemptCapture, error)

// captureDispatchAttemptContext delivers one ended attempt's observational
// capture through the parent plane that claimed it: the worker enrollment
// credential on the worker plane, or the invoking user's credential on the
// task-run plane (req-260820-221be8 AC-2.1; component-mcp-protocol).
func (c *client) captureDispatchAttemptContext(ctx context.Context, credential string, item workerservice.DispatchOrder, capture core.WorkOrderAttemptCapture) (core.WorkOrderAttemptCaptureResult, error) {
	var result core.WorkOrderAttemptCaptureResult
	payload, err := json.Marshal(capture)
	if err != nil {
		return result, err
	}
	path := "/v1/worker/work-orders/" + url.PathEscape(item.Order.ID) + "/attempt-observability"
	if item.Dispatch == "run" {
		path = taskRunOrderPath(item, "/attempt-observability")
	}
	err = c.workerDoContext(ctx, http.MethodPost, path, payload, &result, credential)
	return result, err
}

// attemptCaptureFinalizer records the first mediated ending of one launched
// attempt and delivers its bounded, redacted transcript at most once. Every
// stage uses it; Git preservation for implementation attempts stays a
// separate, independently authorized step (component-attempt-checkpoints).
type attemptCaptureFinalizer struct {
	mu        sync.Mutex
	armed     bool
	reason    string
	delivered sync.Once

	sessionID string
	attemptID string
	snapshot  func() (string, bool, error)
	deliver   func(context.Context, core.WorkOrderAttemptCapture) error
	warn      io.Writer
}

func newAttemptCaptureFinalizer(sessionID, attemptID string, deliver func(context.Context, core.WorkOrderAttemptCapture) error, warn io.Writer) *attemptCaptureFinalizer {
	return &attemptCaptureFinalizer{sessionID: sessionID, attemptID: strings.TrimSpace(attemptID), deliver: deliver, warn: warn}
}

// arm records the attempt's mediated ending. The first ending wins so racing
// exit, timeout, and renewal paths cannot relabel or duplicate a capture. An
// empty reason asks Conveyor to bind the capture to the attempt's persisted
// ending without a launcher declaration (used where authority was lost and
// the launcher did not commit the ending itself).
func (f *attemptCaptureFinalizer) arm(reason string) {
	if f == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.armed {
		return
	}
	f.armed, f.reason = true, strings.TrimSpace(reason)
}

// setSnapshot attaches the bounded spool once the attempt directory exists.
func (f *attemptCaptureFinalizer) setSnapshot(snapshot func() (string, bool, error)) {
	if f == nil {
		return
	}
	f.mu.Lock()
	f.snapshot = snapshot
	f.mu.Unlock()
}

// finish delivers the armed capture once. An unarmed attempt (run
// interruption, pre-launch failure, unmediated loss) produces no capture.
// Delivery failure is only a warning: it never changes the ending's release,
// exit status, checkpoint, or verdict (DEC-26).
func (f *attemptCaptureFinalizer) finish() {
	if f == nil {
		return
	}
	f.delivered.Do(func() {
		f.mu.Lock()
		armed, reason, snapshot := f.armed, f.reason, f.snapshot
		f.mu.Unlock()
		if !armed || f.attemptID == "" || f.deliver == nil {
			return
		}
		capture := core.WorkOrderAttemptCapture{SessionID: f.sessionID, AttemptID: f.attemptID, TerminationReason: reason}
		if snapshot != nil {
			content, truncated, err := snapshot()
			if err != nil {
				f.warnf("warning: read attempt transcript spool: %v; delivering the ending without a transcript because capture is best-effort\n", err)
			} else {
				capture.Transcript = &core.WorkOrderAttemptTranscript{Content: content, Truncated: truncated}
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), workerAttemptCaptureTimeout)
		defer cancel()
		err := f.deliver(ctx, capture)
		var response *workerHTTPError
		if errors.As(err, &response) && response.StatusCode == http.StatusNotFound {
			// A server from before the capture route has nowhere to put it;
			// that is an absent observation, not an operator-facing fault.
			err = nil
		}
		if err != nil {
			f.warnf("warning: deliver attempt transcript capture: %v; the attempt ending is unchanged because capture is best-effort\n", err)
		}
		if hook := workerAttemptCaptureTestHook; hook != nil {
			hook(capture, err)
		}
	})
}

func (f *attemptCaptureFinalizer) warnf(format string, args ...any) {
	if f.warn != nil {
		_, _ = fmt.Fprintf(f.warn, format, args...)
	}
}
