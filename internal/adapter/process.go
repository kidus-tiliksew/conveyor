package adapter

import (
	"bufio"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

type VersionSpec struct {
	Binary    string
	Name      string
	Want      string
	Normalize func(string) string
}

func CheckVersion(ctx context.Context, spec VersionSpec) error {
	out, err := exec.CommandContext(ctx, spec.Binary, "--version").CombinedOutput()
	if err != nil {
		return fmt.Errorf("check %s version: %w: %s", spec.Name, err, out)
	}
	raw := strings.TrimSpace(string(out))
	got := raw
	if spec.Normalize != nil {
		got = spec.Normalize(raw)
	}
	if got != spec.Want {
		return fmt.Errorf("%s version %q is unsupported; want %q", spec.Name, raw, spec.Want)
	}
	return nil
}

type StreamSpec struct {
	Binary       string
	Args         []string
	Dir          string
	Env          []string
	Name         string
	MaxLineBytes int
	Parse        func([]byte) Event
}

// StreamCommand owns the lifecycle shared by JSONL harness adapters: bounded
// scanning, bounded stderr diagnostics, process errors, and one terminal event.
func StreamCommand(ctx context.Context, spec StreamSpec) (<-chan Event, error) {
	cmd := exec.CommandContext(ctx, spec.Binary, spec.Args...)
	if spec.Dir != "" {
		cmd.Dir = spec.Dir
	}
	if len(spec.Env) != 0 {
		cmd.Env = append(cmd.Environ(), spec.Env...)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", spec.Name, err)
	}
	maxLineBytes := spec.MaxLineBytes
	if maxLineBytes == 0 {
		maxLineBytes = 16 * 1024 * 1024
	}
	events := make(chan Event, 64)
	go func() {
		defer close(events)
		var diagnostics strings.Builder
		stderrDone := make(chan struct{})
		go func() {
			defer close(stderrDone)
			scanner := bufio.NewScanner(stderr)
			for scanner.Scan() {
				if diagnostics.Len() < 64*1024 {
					diagnostics.WriteString(scanner.Text())
					diagnostics.WriteByte('\n')
				}
			}
		}()
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 0, 1024*1024), maxLineBytes)
		for scanner.Scan() {
			events <- spec.Parse(scanner.Bytes())
		}
		scanErr := scanner.Err()
		if scanErr != nil {
			_ = cmd.Process.Kill()
		}
		<-stderrDone
		if scanErr != nil {
			events <- Event{Kind: EventError, Err: scanErr.Error(), At: time.Now().UTC()}
			_ = cmd.Wait()
			return
		}
		if err := cmd.Wait(); err != nil {
			message := strings.TrimSpace(diagnostics.String())
			if message == "" {
				message = err.Error()
			}
			events <- Event{Kind: EventError, Err: message, At: time.Now().UTC()}
			return
		}
		events <- Event{Kind: EventDone, At: time.Now().UTC()}
	}()
	return events, nil
}
