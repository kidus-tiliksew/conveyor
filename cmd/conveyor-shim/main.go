// conveyor-shim is the job shim (spec §6.3): a small supervisor inside
// every sandbox. It resolves nothing itself (secrets arrive resolved),
// runs the harness via its adapter, and streams normalized events to
// stdout and the control directory. All harness output flows through
// one choke point here — that is where redaction (spec §10.3) attaches
// in Phase 2.
//
// It is injected into every sandbox image regardless of the repo's
// language, so it must remain a dependency-free static binary
// (spec §17.0) — stdlib plus internal/adapter* only, which are
// themselves stdlib-only.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/adapter"
	"github.com/kidus-tiliksew/conveyor/internal/adapter/codex"
	"github.com/kidus-tiliksew/conveyor/internal/snapshot"
)

// credsDir is where the runner mounts harness credentials read-only
// (spec §5.2). The shim copies them into a writable harness home under
// the control dir, which also makes session state a job artifact by
// construction (spec §8.3 note 2).
const credsDir = "/conveyor/creds"

func main() {
	harness := flag.String("harness", "", "harness adapter to run (codex)")
	workdir := flag.String("workdir", "", "harness working directory (the task worktree)")
	control := flag.String("control", "", "control directory: prompt.txt in, events/artifacts out")
	taskID := flag.String("task", "", "task ID (stamped on the handoff snapshot)")
	jobID := flag.String("job", "", "job ID (stamped on the handoff snapshot)")
	flag.Parse()

	if err := run(*harness, *workdir, *control, *taskID, *jobID); err != nil {
		log.Fatalf("conveyor-shim: %v", err)
	}
}

func run(harness, workdir, control, taskID, jobID string) error {
	if harness == "" || workdir == "" || control == "" || taskID == "" || jobID == "" {
		return fmt.Errorf("--harness, --workdir, --control, --task, and --job are required")
	}
	prompt, err := os.ReadFile(filepath.Join(control, "prompt.txt"))
	if err != nil {
		return fmt.Errorf("read prompt: %w", err)
	}

	a, err := buildAdapter(harness, control)
	if err != nil {
		return err
	}

	out, err := os.Create(filepath.Join(control, "events.jsonl"))
	if err != nil {
		return err
	}
	defer out.Close()

	forward := func(ev adapter.Event) {
		// The redaction choke point (spec §10.3): every byte leaving
		// the sandbox passes through here. Phase 2 inserts the
		// scrubber in this function; nothing may bypass it.
		line, err := json.Marshal(ev)
		if err != nil {
			return
		}
		line = append(line, '\n')
		out.Write(line)
		os.Stdout.Write(line)
	}
	return supervise(a, workdir, control, harness, taskID, jobID, string(prompt), forward)
}

func supervise(a adapter.Adapter, workdir, control, harness, taskID, jobID, prompt string, forward func(adapter.Event)) error {
	events, err := a.Run(context.Background(), adapter.RunSpec{
		Workdir: workdir,
		Prompt:  prompt,
	})
	if err != nil {
		return fmt.Errorf("start harness: %w", err)
	}

	failed := false
	sessionRef := ""
	var terminalError *adapter.Event
	for ev := range events {
		switch ev.Kind {
		case adapter.EventError:
			failed = true
			if terminalError == nil {
				captured := ev
				terminalError = &captured
			}
			// Delay the job-level terminal error until after the handoff
			// attempt, just as EventDone is delayed on success.
			continue
		case adapter.EventSessionStart:
			sessionRef = ev.SessionRef
		case adapter.EventDone:
			// The adapter run is a subrun of this shim-owned job. Emit one
			// terminal job-level done event only after handoff elicitation.
			continue
		}
		forward(withPhase(ev, adapter.PhaseMain))
	}

	// Handoff snapshot — the guaranteed continuity floor between jobs
	// (spec §8.3). Attempt it even after a failed main run because partial
	// work and failure context are especially important to a successor.
	// Elicitation failure does not change the main run's outcome.
	if err := elicitHandoff(a, sessionRef, workdir, control, harness, taskID, jobID, prompt, forward); err != nil {
		log.Printf("conveyor-shim: handoff elicitation skipped: %v", err)
	}

	if failed {
		if terminalError == nil {
			terminalError = &adapter.Event{Kind: adapter.EventError, Err: "harness reported an error", At: time.Now()}
		}
		terminalError.At = time.Now()
		forward(withPhase(*terminalError, adapter.PhaseMain))
		return fmt.Errorf("harness reported an error event")
	}
	forward(adapter.Event{Kind: adapter.EventDone, Phase: adapter.PhaseJob, At: time.Now()})
	return nil
}

// elicitHandoff prefers the job's native session because it already
// holds the relevant context, then falls back to a fresh read-only run
// briefed from the task and persistent worktree. Native resume is an
// optimization; the snapshot is the continuity floor (spec §8.3).
func elicitHandoff(a adapter.Adapter, sessionRef, workdir, control, harness, taskID, jobID, taskPrompt string, forward func(adapter.Event)) error {
	resumeErr := fmt.Errorf("native resume unavailable")
	if sessionRef != "" && a.Capabilities().Resume {
		events, err := a.Resume(context.Background(), sessionRef, snapshot.ElicitationPrompt)
		if err == nil {
			h, collectErr := collectHandoff(events, adapter.PhaseHandoffResume, forward)
			if collectErr == nil {
				if saveErr := saveHandoff(h, control, harness, taskID, jobID); saveErr != nil {
					forwardWarning(forward, adapter.PhaseHandoffResume, saveErr)
					return saveErr
				}
				return nil
			}
			resumeErr = collectErr
		} else {
			resumeErr = err
			forwardWarning(forward, adapter.PhaseHandoffResume, err)
		}
	} else if sessionRef == "" {
		resumeErr = fmt.Errorf("no session ref captured from the run")
		forwardWarning(forward, adapter.PhaseHandoffResume, resumeErr)
	} else {
		resumeErr = fmt.Errorf("harness %s does not support resume", harness)
		forwardWarning(forward, adapter.PhaseHandoffResume, resumeErr)
	}

	// Snapshot generation is the continuity floor; native resume is only
	// an optimization. A fresh read-only run can reconstruct the handoff
	// from the persistent worktree and original task prompt (spec §8.3).
	events, err := a.Run(context.Background(), adapter.RunSpec{
		Workdir: workdir,
		Prompt:  snapshot.FallbackElicitationPrompt(taskPrompt),
	})
	if err != nil {
		forwardWarning(forward, adapter.PhaseHandoffFallback, err)
		return fmt.Errorf("native elicitation failed (%v); fallback start: %w", resumeErr, err)
	}
	h, err := collectHandoff(events, adapter.PhaseHandoffFallback, forward)
	if err != nil {
		return fmt.Errorf("native elicitation failed (%v); fallback: %w", resumeErr, err)
	}
	if err := saveHandoff(h, control, harness, taskID, jobID); err != nil {
		forwardWarning(forward, adapter.PhaseHandoffFallback, err)
		return err
	}
	return nil
}

func collectHandoff(events <-chan adapter.Event, phase string, forward func(adapter.Event)) (*snapshot.Handoff, error) {
	reply := ""
	for ev := range events {
		switch ev.Kind {
		case adapter.EventAssistantText:
			if ev.Text != "" {
				reply = ev.Text
			}
		case adapter.EventError:
			warning := withPhase(ev, phase)
			warning.Kind = adapter.EventWarning
			forward(warning)
			return nil, fmt.Errorf("elicitation run: %s", ev.Err)
		case adapter.EventDone:
			continue
		}
		forward(withPhase(ev, phase)) // elicitation is part of the job transcript
	}
	h, err := snapshot.ParseHandoff(reply)
	if err != nil {
		forwardWarning(forward, phase, err)
		return nil, err
	}
	return h, nil
}

func withPhase(ev adapter.Event, phase string) adapter.Event {
	ev.Phase = phase
	return ev
}

func forwardWarning(forward func(adapter.Event), phase string, err error) {
	forward(adapter.Event{Kind: adapter.EventWarning, Phase: phase, Err: err.Error(), At: time.Now()})
}

func saveHandoff(h *snapshot.Handoff, control, harness, taskID, jobID string) error {
	h.TaskID = taskID
	h.JobID = jobID
	h.Harness = harness
	h.WrittenAt = time.Now().UTC()
	path, err := snapshot.Path(control, jobID)
	if err != nil {
		return err
	}
	return h.Save(path)
}

func buildAdapter(harness, control string) (adapter.Adapter, error) {
	switch harness {
	case "codex":
		c := codex.New()
		// The shim only ever runs inside the sandbox container, which
		// is the Tier A confinement boundary (spec §8.5) — the
		// harness's own sandbox is redundant there and bwrap/Landlock
		// cannot create namespaces in an unprivileged container.
		c.ContainerConfined = true
		src := filepath.Join(credsDir, "codex")
		if fi, err := os.Stat(src); err == nil && fi.IsDir() {
			home := filepath.Join(control, "codex-home")
			// Stage only the auth material, never the whole credential
			// dir: the user's ~/.codex carries gigabytes of session
			// history, and their interactive config must not leak into
			// unattended runs — the adapter owns that posture
			// (spec §8.5 responsibility seam).
			if err := copyFiles(src, home, "auth.json"); err != nil {
				return nil, fmt.Errorf("stage codex credentials: %w", err)
			}
			c.Home = home
		}
		return c, nil
	default:
		return nil, fmt.Errorf("unknown harness %q", harness)
	}
}

// copyFiles copies the named top-level files from src into dst,
// skipping names that don't exist.
func copyFiles(src, dst string, names ...string) error {
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return err
	}
	for _, name := range names {
		in, err := os.Open(filepath.Join(src, name))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		out, err := os.OpenFile(filepath.Join(dst, name), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			in.Close()
			return err
		}
		_, err = io.Copy(out, in)
		in.Close()
		out.Close()
		if err != nil {
			return err
		}
	}
	return nil
}
