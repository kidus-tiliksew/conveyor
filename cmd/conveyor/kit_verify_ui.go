package main

import (
	"crypto/sha256"
	"fmt"
	"os/exec"
	"runtime"
	"syscall"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/redact"
	"github.com/kidus-tiliksew/conveyor/internal/verification"
)

type kitUIProcess struct {
	command        *exec.Cmd
	group          harnessProcessGroup
	stdout, stderr *kitBoundedOutput
	started        time.Time
	digest         string
}

func startKitUI(ui *verification.UI, cwd string, env []string) (*kitUIProcess, error) {
	if ui.Port < 1 || ui.Port > 65535 || len(ui.Argv) == 0 {
		return nil, fmt.Errorf("invalid loopback UI contract")
	}
	tool, err := kitExecutable(ui.Argv[0], cwd)
	if err != nil {
		return nil, err
	}
	digest, err := kitToolDigest(tool)
	if err != nil {
		return nil, err
	}
	u := &kitUIProcess{command: exec.Command(tool, ui.Argv[1:]...), stdout: &kitBoundedOutput{limit: 1 << 20}, stderr: &kitBoundedOutput{limit: 1 << 20}, started: time.Now().UTC(), digest: digest}
	u.command.Dir = cwd
	u.command.Env = env
	u.command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	u.command.Stdout = u.stdout
	u.command.Stderr = u.stderr
	if err = u.command.Start(); err != nil {
		return nil, err
	}
	done := make(chan error, 1)
	go func() { done <- u.command.Wait() }()
	u.group = harnessProcessGroup{pgid: u.command.Process.Pid, done: done}
	return u, nil
}
func (u *kitUIProcess) stop(redactor *redact.Redactor) core.ExecutionReportPayload {
	var finished *error
	select {
	case err := <-u.group.done:
		finished = &err
	default:
	}
	_ = u.group.terminate(finished)
	stdout, _ := redactor.Redact(u.stdout.String())
	stderr, _ := redactor.Redact(u.stderr.String())
	zero := false
	cancelled := finished == nil
	exit := u.command.ProcessState.ExitCode()
	return core.ExecutionReportPayload{Argv: u.command.Args, Tool: u.command.Path, ToolVersion: "sha256:" + u.digest, Runtime: runtime.Version(), StartedAt: u.started.Format(time.RFC3339Nano), EndedAt: time.Now().UTC().Format(time.RFC3339Nano), ExitCode: &exit, TimedOut: &zero, Cancelled: &cancelled, StdoutSHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(stdout))), StderrSHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(stderr))), StdoutTruncated: &u.stdout.truncated, StderrTruncated: &u.stderr.truncated}
}
