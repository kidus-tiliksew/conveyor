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

	"github.com/kidus-tiliksew/conveyor/internal/adapter"
	"github.com/kidus-tiliksew/conveyor/internal/adapter/codex"
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
	flag.Parse()

	if err := run(*harness, *workdir, *control); err != nil {
		log.Fatalf("conveyor-shim: %v", err)
	}
}

func run(harness, workdir, control string) error {
	if harness == "" || workdir == "" || control == "" {
		return fmt.Errorf("--harness, --workdir, and --control are required")
	}
	prompt, err := os.ReadFile(filepath.Join(control, "prompt.txt"))
	if err != nil {
		return fmt.Errorf("read prompt: %w", err)
	}

	a, err := buildAdapter(harness, control)
	if err != nil {
		return err
	}

	events, err := a.Run(context.Background(), adapter.RunSpec{
		Workdir: workdir,
		Prompt:  string(prompt),
	})
	if err != nil {
		return fmt.Errorf("start harness: %w", err)
	}

	out, err := os.Create(filepath.Join(control, "events.jsonl"))
	if err != nil {
		return err
	}
	defer out.Close()

	// The redaction choke point (spec §10.3): every byte leaving the
	// sandbox passes through this loop. Phase 2 inserts the scrubber
	// here; nothing may bypass it.
	failed := false
	for ev := range events {
		if ev.Kind == adapter.EventError {
			failed = true
		}
		line, err := json.Marshal(ev)
		if err != nil {
			continue
		}
		line = append(line, '\n')
		out.Write(line)
		os.Stdout.Write(line)
	}

	// TODO(phase1): handoff snapshot elicitation — one more Run with
	// snapshot.ElicitationPrompt, parsed into control/handoff.json
	// (spec §8.3).

	if failed {
		return fmt.Errorf("harness reported an error event")
	}
	return nil
}

func buildAdapter(harness, control string) (adapter.Adapter, error) {
	switch harness {
	case "codex":
		c := codex.New()
		src := filepath.Join(credsDir, "codex")
		if fi, err := os.Stat(src); err == nil && fi.IsDir() {
			home := filepath.Join(control, "codex-home")
			if err := copyDir(src, home); err != nil {
				return nil, fmt.Errorf("stage codex credentials: %w", err)
			}
			c.Home = home
		}
		return c, nil
	default:
		return nil, fmt.Errorf("unknown harness %q", harness)
	}
}

func copyDir(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		if !d.Type().IsRegular() {
			return nil // skip sockets, symlinks out of the creds dir
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return err
		}
		defer out.Close()
		_, err = io.Copy(out, in)
		return err
	})
}
