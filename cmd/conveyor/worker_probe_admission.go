package main

import (
	"fmt"
	"os"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	workerservice "github.com/kidus-tiliksew/conveyor/internal/worker"
)

// admit reports whether harness, exactly as the current iteration's loaded
// setup defines it, has a healthy and unexpired local probe observation. The
// observation is keyed by the definition's fingerprint, so a reloaded or
// edited definition can never reuse an earlier success, and a healthy result
// expires at its scheduled one-minute refresh. Admission is purely
// client-local: it adds no server registry, validation, or claim-admission
// rule (req-execution-configuration AC-10.4, AC-10.9; req-local-task-runs
// AC-1.4; DEC-56(2); component-harness-execution).
func (p *workerHarnessProbes) admit(harness config.Harness, now time.Time) error {
	fingerprint := workerservice.HarnessFingerprint(harness)
	state, ok := p.states[harness.Name+"\x00"+fingerprint]
	switch {
	case !ok:
		return fmt.Errorf("harness %q has no local probe result for its current definition; the order was left queued and nothing was claimed", harness.Name)
	case state.probe.Fingerprint != fingerprint:
		return fmt.Errorf("harness %q probe result does not match its current definition; the order was left queued and nothing was claimed", harness.Name)
	case !state.probe.Healthy:
		message := state.probe.Message
		if message == "" {
			message = "probe failed"
		}
		return fmt.Errorf("harness %q failed its local probe: %s; the order was left queued, nothing was claimed, and the probe retries after %s", harness.Name, message, state.retryDelay)
	case !state.nextProbe.After(now):
		return fmt.Errorf("harness %q local probe result expired; the order was left queued and nothing was claimed until it is probed again", harness.Name)
	}
	return nil
}

// reportWorkerProbeRefusal names a skipped order and its setup remedy. Tests
// replace it to observe refusals without parsing process stderr.
var reportWorkerProbeRefusal = func(item workerservice.DispatchOrder, err error) {
	fmt.Fprintf(os.Stderr, "skip work order %s: %v\n", item.Order.ID, err)
}
