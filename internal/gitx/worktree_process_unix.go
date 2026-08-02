package gitx

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

type worktreeProcess struct {
	PID     int
	Command string
	CWD     string
}

func processesWithinPath(ctx context.Context, root string) ([]worktreeProcess, error) {
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	switch runtime.GOOS {
	case "linux":
		return linuxProcessesWithinPath(resolved)
	case "darwin":
		return darwinProcessesWithinPath(ctx, resolved)
	default:
		return nil, fmt.Errorf("process cwd inspection is unsupported on %s", runtime.GOOS)
	}
}

func linuxProcessesWithinPath(root string) ([]worktreeProcess, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var processes []worktreeProcess
	var inspectErrs []error
	for _, entry := range entries {
		pid, parseErr := strconv.Atoi(entry.Name())
		if parseErr != nil || !entry.IsDir() {
			continue
		}
		cwd, readErr := os.Readlink(filepath.Join("/proc", entry.Name(), "cwd"))
		if readErr != nil {
			if !errors.Is(readErr, os.ErrNotExist) && !errors.Is(readErr, os.ErrPermission) {
				inspectErrs = append(inspectErrs, fmt.Errorf("inspect pid %d cwd: %w", pid, readErr))
			}
			continue
		}
		if !pathWithin(root, cwd) {
			continue
		}
		commandBytes, commandErr := os.ReadFile(filepath.Join("/proc", entry.Name(), "comm"))
		command := "unknown"
		if commandErr == nil && strings.TrimSpace(string(commandBytes)) != "" {
			command = strings.TrimSpace(string(commandBytes))
		}
		processes = append(processes, worktreeProcess{PID: pid, Command: command, CWD: cwd})
	}
	sort.Slice(processes, func(i, j int) bool { return processes[i].PID < processes[j].PID })
	return processes, errors.Join(inspectErrs...)
}

func darwinProcessesWithinPath(ctx context.Context, root string) ([]worktreeProcess, error) {
	lsof, err := exec.LookPath("lsof")
	if err != nil {
		return nil, err
	}
	command := exec.CommandContext(ctx, lsof, "-d", "cwd", "-Fpcn")
	output, commandErr := command.Output()
	if exitErr := new(exec.ExitError); errors.As(commandErr, &exitErr) && exitErr.ExitCode() == 1 && len(output) == 0 {
		return nil, nil
	}
	if commandErr != nil {
		return nil, commandErr
	}
	var processes []worktreeProcess
	var current worktreeProcess
	flush := func() {
		if current.PID != 0 && pathWithin(root, current.CWD) {
			if current.Command == "" {
				current.Command = "unknown"
			}
			processes = append(processes, current)
		}
		current = worktreeProcess{}
	}
	for _, line := range strings.Split(string(output), "\n") {
		if line == "" {
			continue
		}
		switch line[0] {
		case 'p':
			flush()
			current.PID, _ = strconv.Atoi(line[1:])
		case 'c':
			current.Command = line[1:]
		case 'n':
			current.CWD = line[1:]
		}
	}
	flush()
	sort.Slice(processes, func(i, j int) bool { return processes[i].PID < processes[j].PID })
	return processes, nil
}

func pathWithin(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
