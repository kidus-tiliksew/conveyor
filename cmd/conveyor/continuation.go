package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	workerservice "github.com/kidus-tiliksew/conveyor/internal/worker"
)

const continuationReportTimeout = 2 * time.Second

// continuationLaunchPlan is derived entirely by the launching client. The
// control plane supplies advisory metadata and provenance, never resume argv.
type continuationLaunchPlan struct {
	Resume bool
	Reason string
}

func planContinuationLaunch(order core.WorkOrder, harness config.Harness, launchEnvironment string) continuationLaunchPlan {
	if order.LastAttemptID == "" {
		return continuationLaunchPlan{Reason: "initial attempt"}
	}
	if !order.CanResumeContinuation() {
		return continuationLaunchPlan{Reason: "continuation is not eligible"}
	}
	if order.ContinuationHarness != harness.Name {
		return continuationLaunchPlan{Reason: "harness differs from recorded continuation"}
	}
	if launchEnvironment == "" || order.ContinuationLaunchEnvironment != launchEnvironment {
		return continuationLaunchPlan{Reason: "launch environment differs from recorded continuation"}
	}
	if len(harness.ResumeCommand) == 0 {
		return continuationLaunchPlan{Reason: "harness declares no resume capability"}
	}
	return continuationLaunchPlan{Resume: true, Reason: "eligible recorded continuation"}
}

func continuationLaunchEnvironment(order core.WorkOrder, dispatch, workspace, credential string) string {
	if dispatch == "worker" && strings.TrimSpace(order.WorkerID) != "" {
		return "worker:" + strings.TrimSpace(order.WorkerID)
	}
	hostname, err := os.Hostname()
	if err != nil || strings.TrimSpace(hostname) == "" || strings.TrimSpace(credential) == "" {
		return ""
	}
	// Hash local facts so the advisory identity compares equal on the same run
	// host without disclosing its hostname or operator credential.
	sum := sha256.Sum256([]byte(workspace + "\x00" + hostname + "\x00" + credential))
	return "run:" + hex.EncodeToString(sum[:16])
}

func appendContinuationResumeArgv(argv, resumeCommand []string, sessionID string) []string {
	result := append([]string(nil), argv...)
	for _, value := range resumeCommand {
		if value == "{session_id}" {
			value = sessionID
		}
		result = append(result, value)
	}
	return result
}

func continuationRecoveryPrompt(prompt string, order core.WorkOrder) string {
	direction := strings.TrimSpace(order.OperatorDirection)
	if direction == "" {
		return prompt
	}
	return prompt + "\n\nOperator direction for this recovery:\n" + direction
}

// continuationSessionObserver recognizes only harness lifecycle envelopes that
// own a native session identifier; arbitrary session_id fields in tool output
// are deliberately ignored.
type continuationSessionObserver struct {
	mu      sync.Mutex
	pending bytes.Buffer
	last    string
	observe func(string)
}

func newContinuationSessionObserver(observe func(string)) *continuationSessionObserver {
	return &continuationSessionObserver{observe: observe}
}

func (w *continuationSessionObserver) Write(p []byte) (int, error) {
	w.mu.Lock()
	_, _ = w.pending.Write(p)
	var observed []string
	for {
		line, err := w.pending.ReadString('\n')
		if err != nil {
			w.pending.WriteString(line)
			break
		}
		if sessionID := nativeSessionID([]byte(strings.TrimSpace(line))); sessionID != "" && sessionID != w.last {
			w.last = sessionID
			observed = append(observed, sessionID)
		}
	}
	w.mu.Unlock()
	for _, sessionID := range observed {
		if w.observe != nil {
			w.observe(sessionID)
		}
	}
	return len(p), nil
}

func (w *continuationSessionObserver) Last() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.last
}

func nativeSessionID(line []byte) string {
	var event struct {
		Type      string `json:"type"`
		Subtype   string `json:"subtype"`
		ThreadID  string `json:"thread_id"`
		SessionID string `json:"session_id"`
	}
	if len(line) == 0 || json.Unmarshal(line, &event) != nil {
		return ""
	}
	switch {
	case event.Type == "thread.started":
		return strings.TrimSpace(event.ThreadID)
	case event.Type == "system" && event.Subtype == "init":
		return strings.TrimSpace(event.SessionID)
	default:
		return ""
	}
}

// continuationReporter serializes replacement reports so a late response for
// an older observed identifier cannot overwrite a newer capture.
type continuationReporter struct {
	client      *client
	credential  string
	item        workerservice.DispatchOrder
	claim       core.WorkOrder
	environment string
	reports     chan string
	stop        chan struct{}
	stopOnce    sync.Once
	warn        func(string)
}

func newContinuationReporter(c *client, credential string, item workerservice.DispatchOrder, claim core.WorkOrder, environment string, warn func(string)) *continuationReporter {
	r := &continuationReporter{
		client: c, credential: credential, item: item, claim: claim, environment: environment,
		reports: make(chan string, 4), stop: make(chan struct{}), warn: warn,
	}
	go r.run()
	return r
}

func (r *continuationReporter) Stop() {
	r.stopOnce.Do(func() { close(r.stop) })
}

func (r *continuationReporter) Observe(sessionID string) {
	select {
	case r.reports <- sessionID:
	default:
		// Reporting is operational metadata: never apply output backpressure.
		select {
		case <-r.reports:
		default:
		}
		select {
		case r.reports <- sessionID:
		default:
		}
	}
}

func (r *continuationReporter) run() {
	for {
		select {
		case sessionID := <-r.reports:
			r.report(sessionID)
		case <-r.stop:
			for {
				select {
				case sessionID := <-r.reports:
					r.report(sessionID)
				default:
					return
				}
			}
		}
	}
}

func (r *continuationReporter) report(sessionID string) {
	ctx, cancel := context.WithTimeout(context.Background(), continuationReportTimeout)
	err := r.client.reportDispatchContinuationContext(ctx, r.credential, r.item, r.claim.SessionID, core.WorkOrderContinuation{
		SessionID: sessionID, AttemptID: r.claim.AttemptID, Harness: r.item.Harness.Name, LaunchEnvironment: r.environment,
	})
	cancel()
	if err != nil && r.warn != nil {
		r.warn(fmt.Sprintf("report continuation metadata: %v", err))
	}
}
