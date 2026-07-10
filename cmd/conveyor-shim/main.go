// conveyor-shim is the job shim (spec §6.3): a small supervisor inside
// every sandbox. It resolves nothing itself (secrets arrive resolved),
// enforces the tool policy, meters resource usage, streams logs to the
// control plane with redaction applied at the edge, enforces the path
// jail, records runtime package installs, and handles pause/resume/kill.
//
// It is injected into every sandbox image regardless of the repo's
// language, so it must remain a dependency-free static binary
// (spec §17.0) — stdlib only, no external imports.
package main

import (
	"flag"
	"log"
)

func main() {
	harness := flag.String("harness", "", "harness adapter to run (codex, claude-code)")
	flag.Parse()

	// TODO(phase1): supervise the harness process via its adapter,
	// stream normalized events to stdout for the runner to collect,
	// and run the end-of-job handoff-snapshot prompt (spec §8.3).
	// Redaction (spec §10.3) lands in Phase 2 but the log stream must
	// flow through a single choke point from day one — build that
	// choke point here, not in the runner.
	log.Printf("conveyor-shim: harness=%q — not implemented", *harness)
}
