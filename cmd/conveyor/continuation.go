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

const (
	continuationReportTimeout = 2 * time.Second
	continuationLineLimit     = 64 * 1024
)

// continuationLaunchPlan is derived entirely by the launching client. The
// control plane supplies advisory metadata and provenance, never resume argv
// (req-260818-24dd3a AC-2.1, AC-2.3; DEC-24; design-260805-973cd4).
type continuationLaunchPlan struct {
	Resume bool
	Reason string
}

func planContinuationLaunch(order core.WorkOrder, harness config.Harness, launchEnvironment string) continuationLaunchPlan {
	if order.LastAttemptID == "" {
		return continuationLaunchPlan{Reason: "initial_attempt"}
	}
	if !order.CanResumeContinuation() {
		return continuationLaunchPlan{Reason: "server_ineligible"}
	}
	if len(harness.ResumeCommand) == 0 {
		return continuationLaunchPlan{Reason: "no_local_resume_contract"}
	}
	if order.ContinuationHarness != harness.Name {
		return continuationLaunchPlan{Reason: "harness_mismatch"}
	}
	if launchEnvironment == "" || order.ContinuationLaunchEnvironment != launchEnvironment {
		return continuationLaunchPlan{Reason: "launch_environment_mismatch"}
	}
	return continuationLaunchPlan{Resume: true, Reason: "eligible_match"}
}

func continuationLaunchEnvironment(order core.WorkOrder, dispatch, workspace, credential string) string {
	if dispatch == "worker" && strings.TrimSpace(order.WorkerID) != "" {
		return "worker:" + strings.TrimSpace(order.WorkerID)
	}
	hostname, err := os.Hostname()
	if dispatch != "run" || err != nil || strings.TrimSpace(hostname) == "" || strings.TrimSpace(credential) == "" {
		return ""
	}
	// The hash is a stable local identity for an attended client without
	// disclosing either the host name or credential (req-260818-24dd3a AC-1.1).
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
		direction = "No additional operator direction was provided."
	}
	return prompt + "\n\n# Operator direction\n\n" + direction
}

func continuationObserverEnabled(harness config.Harness, launchEnvironment string) bool {
	return len(harness.ResumeCommand) > 0 && launchEnvironment != ""
}

// continuationSessionObserver recognizes the shared stream-json init
// envelope. Its buffer is bounded and malformed or oversized lines are
// discarded without affecting the output path (req-260818-24dd3a AC-1.2).
type continuationSessionObserver struct {
	mu       sync.Mutex
	pending  []byte
	discard  bool
	observed bool
	observe  func(string)
}

func newContinuationSessionObserver(observe func(string)) *continuationSessionObserver {
	return &continuationSessionObserver{observe: observe}
}

func (w *continuationSessionObserver) Write(p []byte) (int, error) {
	w.mu.Lock()
	var sessionID string
	for _, part := range bytes.SplitAfter(p, []byte{'\n'}) {
		endsLine := len(part) > 0 && part[len(part)-1] == '\n'
		part = bytes.TrimSuffix(part, []byte{'\n'})
		if !w.discard && !w.observed {
			if len(w.pending)+len(part) > continuationLineLimit {
				w.pending = nil
				w.discard = true
			} else {
				w.pending = append(w.pending, part...)
			}
		}
		if !endsLine {
			continue
		}
		if !w.discard && !w.observed {
			sessionID = claudeInitSessionID(bytes.TrimSpace(w.pending))
			if sessionID != "" {
				w.observed = true
			}
		}
		w.pending = nil
		w.discard = false
	}
	callback := w.observe
	w.mu.Unlock()
	if sessionID != "" && callback != nil {
		callback(sessionID)
	}
	return len(p), nil
}

func claudeInitSessionID(line []byte) string {
	var event struct {
		Type      string `json:"type"`
		Subtype   string `json:"subtype"`
		SessionID string `json:"session_id"`
	}
	if len(line) == 0 || json.Unmarshal(line, &event) != nil || event.Type != "system" || event.Subtype != "init" {
		return ""
	}
	sessionID := strings.TrimSpace(event.SessionID)
	if sessionID == "" || len([]rune(sessionID)) > core.MaxWorkOrderContinuationSessionIDRunes {
		return ""
	}
	return sessionID
}

// continuationReporter serializes replacement reports so a late response for
// an older capture cannot overwrite a newer one. Its small queue never applies
// backpressure to child stdout.
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
		reports: make(chan string, 1), stop: make(chan struct{}), warn: warn,
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
