package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/verification"
	"github.com/kidus-tiliksew/conveyor/internal/workorder"
)

func kitExerciseActions(e verification.Exercise, root, repository string, local []verification.VerificationPermission) ([]verification.VerificationPermission, []string, []string, error) {
	actions := []verification.VerificationPermission{}
	env := []string{"PATH=/usr/local/bin:/usr/bin:/bin", "LANG=C.UTF-8"}
	secrets := []string{}
	for _, p := range e.Permissions {
		a := verification.VerificationPermission{Kind: p.Kind, Binding: p.TargetBinding}
		if a.Binding == "" {
			a.Binding = repository
		}
		switch p.Kind {
		case "filesystem_read", "filesystem_write":
			target, err := verification.ResolvePermissionPath(root, p.Path)
			if err != nil {
				return nil, nil, nil, err
			}
			a.Target = target
		case "network":
			for _, g := range local {
				if g.Kind == p.Kind && g.Binding == p.TargetBinding {
					if a.Target != "" && a.Target != g.Target {
						return nil, nil, nil, fmt.Errorf("network binding %s has ambiguous destinations", p.TargetBinding)
					}
					a.Target = g.Target
				}
			}
			if a.Target == "" {
				return nil, nil, nil, fmt.Errorf("missing local grant for network binding %s", p.TargetBinding)
			}
			env = append(env, "CONVEYOR_KIT_BINDING_"+strings.ToUpper(strings.ReplaceAll(p.TargetBinding, "-", "_"))+"="+a.Target)
		case "operator_interaction":
		default:
			return nil, nil, nil, fmt.Errorf("unknown requested permission %s", p.Kind)
		}
		actions = append(actions, a)
	}
	for _, p := range e.Prerequisites {
		switch p.Kind {
		case "credential":
			var handle string
			for _, g := range local {
				if g.Kind == "credential" && g.Binding == p.EnvironmentBinding {
					if handle != "" && handle != g.Target {
						return nil, nil, nil, fmt.Errorf("ambiguous credential binding %s", p.EnvironmentBinding)
					}
					handle = g.Target
				}
			}
			if !strings.HasPrefix(handle, "CONVEYOR_KIT_SECRET_") {
				return nil, nil, nil, fmt.Errorf("missing approved credential handle for %s (use CONVEYOR_KIT_SECRET_*)", p.EnvironmentBinding)
			}
			value := os.Getenv(handle)
			if value == "" {
				return nil, nil, nil, fmt.Errorf("missing credential %s", p.EnvironmentBinding)
			}
			for _, name := range []string{"CONVEYOR_API_TOKEN", "CONVEYOR_CLIENT_TOKEN", "CONVEYOR_GIT_TOKEN", gitAskPassTokenEnv, "CONVEYOR_WORKER_TOKEN"} {
				if value == os.Getenv(name) {
					return nil, nil, nil, fmt.Errorf("factory or forge credential refused for %s", p.EnvironmentBinding)
				}
			}
			for _, arg := range e.Argv {
				if strings.Contains(arg, value) {
					return nil, nil, nil, fmt.Errorf("credential value in argv refused")
				}
			}
			secrets = append(secrets, value)
			env = append(env, handle+"="+value)
			actions = append(actions, verification.VerificationPermission{Kind: "credential", Binding: p.EnvironmentBinding, Target: handle})
		case "executable":
			if _, err := exec.LookPath(p.EnvironmentBinding); err != nil {
				return nil, nil, nil, fmt.Errorf("missing executable prerequisite %s", p.ID)
			}
		case "service":
			found := false
			for _, a := range actions {
				if a.Kind == "network" && a.Binding == p.EnvironmentBinding {
					found = true
				}
			}
			if !found {
				return nil, nil, nil, fmt.Errorf("missing service binding %s", p.EnvironmentBinding)
			}
		case "operator_interaction":
			actions = append(actions, verification.VerificationPermission{Kind: "operator_interaction", Binding: repository})
		default:
			return nil, nil, nil, fmt.Errorf("unknown prerequisite %s", p.Kind)
		}
	}
	if e.Kind == "interactive" || e.Kind == "hybrid" {
		actions = append(actions, verification.VerificationPermission{Kind: "operator_interaction", Binding: repository})
	}
	return actions, env, secrets, nil
}

type kitBoundedOutput struct {
	mu sync.Mutex
	bytes.Buffer
	limit     int
	truncated bool
}

func (b *kitBoundedOutput) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	remaining := b.limit - b.Len()
	if len(p) > remaining {
		p = p[:remaining]
		b.truncated = true
	}
	_, _ = b.Buffer.Write(p)
	return n, nil
}

func (v *kitVerifier) launch(ctx context.Context, e verification.Exercise, cwd, attemptRoot, runID, grantID string, subject core.VerificationSubject, environment core.VerificationEnvironment, env []string) error {
	vc := v.snapshot.Contexts[0]
	dir := filepath.Join(attemptRoot, runID)
	if err := os.Mkdir(dir, 0700); err != nil {
		return err
	}
	spoolDir := filepath.Join(dir, "spool")
	if err := os.Mkdir(spoolDir, 0700); err != nil {
		return err
	}
	barrier := func(ctx context.Context) error {
		v.mu.Lock()
		defer v.mu.Unlock()
		if v.lost || !v.order.LeaseExpiresAt.After(time.Now()) {
			return fmt.Errorf("verification authority lost")
		}
		return nil
	}
	spool := verification.EvidenceSpool{Directory: spoolDir, Limit: 8 << 20, Check: barrier}
	upload := func(ctx context.Context, data []byte) error {
		var receipt store.VerificationReceipt
		clean, _, err := v.redactor.RedactJSON(data)
		if err != nil {
			return err
		}
		return v.rpc.call(ctx, "submit_verification_evidence", json.RawMessage(clean), &receipt)
	}
	deadline := time.Now().Add(time.Duration(e.TimeoutSeconds) * time.Second)
	if !v.order.ExecutionDeadline.IsZero() && v.order.ExecutionDeadline.Before(deadline) {
		deadline = v.order.ExecutionDeadline
	}
	launchCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	var relayMu sync.Mutex
	accepting := true
	operations := map[string]string{}
	completed := map[string]bool{}
	origins := []string{}
	if v.ui != nil {
		origins = append(origins, fmt.Sprintf("http://127.0.0.1:%d", v.ui.Port))
	}
	channel, err := verification.NewOperationChannel(launchCtx, origins, func(requestCtx context.Context, data json.RawMessage) (any, error) {
		relayMu.Lock()
		defer relayMu.Unlock()
		if !accepting {
			return nil, fmt.Errorf("exercise has ended")
		}
		if err := v.checkCheckout(requestCtx); err != nil {
			cancel()
			return nil, err
		}
		if err := v.live(requestCtx, grantID); err != nil {
			cancel()
			return nil, err
		}
		var signal struct {
			Type              string          `json:"type"`
			StepID            string          `json:"step_id"`
			CapturedAt        time.Time       `json:"captured_at"`
			ProviderReference string          `json:"provider_reference,omitempty"`
			EvidenceType      string          `json:"evidence_type,omitempty"`
			Payload           json.RawMessage `json:"payload,omitempty"`
			Key               string          `json:"key,omitempty"`
		}
		if err := core.DecodeVerificationRequest(data, &signal); err != nil {
			return nil, err
		}
		if signal.Type == "evidence" {
			if signal.CapturedAt.IsZero() || signal.EvidenceType == "operator_observation" || signal.EvidenceType == "execution_report" || !verification.VerificationBindingName(signal.Key) {
				return nil, fmt.Errorf("evidence attribution refused")
			}
			batch, err := v.evidenceBatch(runID, subject, environment, signal.Key, signal.EvidenceType, signal.Payload, signal.CapturedAt)
			if err != nil {
				return nil, err
			}
			if err = spool.Put(requestCtx, signal.Key, batch); err != nil {
				return nil, err
			}
			if err = spool.Flush(requestCtx, upload); err != nil {
				return nil, err
			}
			return map[string]string{"state": "recorded"}, nil
		}
		if signal.Type != "operation.dispatching" && signal.Type != "operation.completed" {
			return nil, fmt.Errorf("unknown operation signal")
		}
		var declared *verification.Operation
		for i := range e.Operations {
			if e.Operations[i].ID == signal.StepID {
				declared = &e.Operations[i]
			}
		}
		if declared == nil || signal.CapturedAt.IsZero() {
			return nil, fmt.Errorf("undeclared operation or missing capture time")
		}
		operationID := operations[signal.StepID]
		if operationID == "" {
			if signal.Type != "operation.dispatching" {
				return nil, fmt.Errorf("operation was not dispatched")
			}
			key := fmt.Sprintf("%x", sha256.Sum256(core.JSONPayload([]any{v.task.ID, subject, signal.StepID})))
			var receipt store.VerificationReceipt
			if err := v.rpc.call(requestCtx, "prepare_verification_operation", workorder.VerificationOperationRequest{ContextID: vc.ID, RunID: runID, Action: "prepare", Key: key, StepID: signal.StepID, Target: declared.TargetBinding, InputDigest: fmt.Sprintf("%x", sha256.Sum256(core.JSONPayload(v.safeInputValues())))}, &receipt); err != nil {
				return nil, err
			}
			operationID = receipt.ID
			operations[signal.StepID] = operationID
		}
		var receipt store.VerificationReceipt
		if err := v.rpc.call(requestCtx, "prepare_verification_operation", workorder.VerificationOperationRequest{ContextID: vc.ID, RunID: runID, Action: strings.TrimPrefix(signal.Type, "operation."), OperationID: operationID, Source: "kit-child", CapturedAt: signal.CapturedAt, ProviderReference: signal.ProviderReference}, &receipt); err != nil {
			return nil, err
		}
		if signal.Type == "operation.dispatching" && !receipt.DispatchAuthorized {
			return nil, fmt.Errorf("operation dispatch was already acknowledged; reconcile")
		}
		if signal.Type == "operation.completed" {
			completed[signal.StepID] = true
		}
		return receipt, nil
	})
	if err != nil {
		return err
	}
	defer channel.Close()
	connection, _ := json.Marshal(map[string]string{"url": channel.URL + "/operations", "nonce": channel.Nonce})
	env = append(env, "CONVEYOR_KIT_OPERATIONS="+string(connection), "CONVEYOR_KIT_ATTEMPT_DIR="+dir, "HOME="+dir, "TMPDIR="+dir)
	stdout, stderr := &kitBoundedOutput{limit: 1 << 20}, &kitBoundedOutput{limit: 1 << 20}
	tool, err := kitExecutable(e.Argv[0], cwd)
	if err != nil {
		return err
	}
	before, err := kitToolDigest(tool)
	if err != nil {
		return err
	}
	environment.Attributes["tool_sha256_before"] = before
	command := exec.Command(tool, e.Argv[1:]...)
	command.Dir = cwd
	command.Env = env
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Stdout = stdout
	command.Stderr = stderr
	for _, arg := range e.Argv {
		clean, _ := v.redactor.Redact(arg)
		if clean != arg {
			return fmt.Errorf("sensitive argv refused")
		}
	}
	started := time.Now().UTC()
	if err = command.Start(); err != nil {
		_ = v.rpc.call(ctx, "report_verification_outcome", workorder.VerificationOutcomeRequest{ContextID: vc.ID, RunID: runID, State: "blocked", Explanation: "exercise executable could not start"}, nil)
		return fmt.Errorf("exercise executable could not start")
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	group := harnessProcessGroup{pgid: command.Process.Pid, done: done}
	var ui *kitUIProcess
	var uiStartErr error
	if v.ui != nil {
		ui, uiStartErr = startKitUI(v.ui, cwd, append(env, "CONVEYOR_KIT_UI_HOST=127.0.0.1"))
		if uiStartErr != nil {
			cancel()
		}
	}

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var waitErr error
	var finished bool
	for !finished {
		select {
		case waitErr = <-done:
			waitErr = group.terminate(&waitErr)
			finished = true
		case <-launchCtx.Done():
			waitErr = group.terminate(nil)
			finished = true
		case <-ticker.C:
			if err = v.checkCheckout(launchCtx); err != nil {
				cancel()
			}
			if err = v.live(launchCtx, grantID); err != nil {
				cancel()
			}
		}
	}
	var uiReport *core.ExecutionReportPayload
	if ui != nil {
		report := ui.stop(v.redactor)
		uiReport = &report
	}
	_ = channel.Close()
	relayMu.Lock()
	accepting = false
	relayMu.Unlock()
	ctx, finishCancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer finishCancel()
	after, hashErr := kitToolDigest(tool)
	environment.Attributes["tool_sha256_after"] = after
	sourceErr := v.checkCheckout(ctx)
	if sourceErr != nil {
		environment.Attributes["source_state"] = "changed"
	}
	ended := time.Now().UTC()
	timedOut := errors.Is(launchCtx.Err(), context.DeadlineExceeded)
	cancelled := launchCtx.Err() != nil && !timedOut
	exit := command.ProcessState.ExitCode()
	cleanOut, _ := v.redactor.Redact(stdout.String())
	cleanErr, _ := v.redactor.Redact(stderr.String())
	report := core.ExecutionReportPayload{Argv: e.Argv, Tool: tool, ToolVersion: "sha256:" + before, Runtime: environment.Runtime, StartedAt: started.Format(time.RFC3339Nano), EndedAt: ended.Format(time.RFC3339Nano), ExitCode: &exit, TimedOut: &timedOut, Cancelled: &cancelled, StdoutSHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(cleanOut))), StderrSHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(cleanErr))), StdoutTruncated: &stdout.truncated, StderrTruncated: &stderr.truncated}
	data, err := v.evidenceBatch(runID, subject, environment, "execution", "execution_report", core.JSONPayload(report))
	if err != nil {
		return err
	}
	if err = spool.Put(ctx, "execution", data); err != nil {
		_, _ = fmt.Fprintln(v.output, string(data))
		if uiReport != nil {
			uiData, _ := v.evidenceBatch(runID, subject, environment, "execution-ui", "execution_report", core.JSONPayload(uiReport))
			_, _ = fmt.Fprintln(v.output, string(uiData))
		}
		return fmt.Errorf("exercise stopped; report upload refused after authority loss: %w", err)
	}
	if uiReport != nil {
		uiData, e := v.evidenceBatch(runID, subject, environment, "execution-ui", "execution_report", core.JSONPayload(uiReport))
		if e != nil {
			return e
		}
		if e = spool.Put(ctx, "execution-ui", uiData); e != nil {
			_, _ = fmt.Fprintln(v.output, string(uiData))
			return e
		}
	}
	if err = spool.Flush(ctx, upload); err != nil {
		return fmt.Errorf("evidence retained in %s; upload unavailable: %w", spoolDir, err)
	}
	if err = v.live(ctx, grantID); err != nil {
		return err
	}
	state, explanation := "succeeded", ""
	if waitErr != nil {
		state = "failed"
	}
	if timedOut {
		state = "timed_out"
	}
	if cancelled {
		state = "cancelled"
	}
	relayMu.Lock()
	for _, op := range e.Operations {
		if !completed[op.ID] {
			state = "blocked"
			explanation = "declared operation " + op.ID + " has no acknowledged completion; reconcile before retry"
		}
	}
	relayMu.Unlock()
	if uiStartErr != nil {
		state = "blocked"
		explanation = "optional UI could not start"
	}
	if sourceErr != nil || hashErr != nil || after != before {
		state = "blocked"
		explanation = "source checkout or resolved tool changed during execution"
	}
	if state == "succeeded" {
		var snapshot store.VerificationSnapshot
		if err = v.rpc.call(ctx, "get_verification_context", workorder.VerificationContextRequest{ContextID: vc.ID}, &snapshot); err != nil {
			return err
		}
		if err = store.ValidateVerificationSuccess(snapshot, runID, &exit); err != nil {
			state = "waiting"
			explanation = "required assertion, output, or operator completion evidence is missing"
			for _, record := range snapshot.Evidence {
				if record.Envelope.RunID == runID && record.Envelope.Type == "assertion_result" {
					var assertion core.AssertionResultPayload
					if json.Unmarshal(record.Envelope.Payload, &assertion) == nil && assertion.Outcome == "fail" {
						for _, id := range e.RequiredAssertions {
							if assertion.AssertionID == id {
								state = "failed"
								explanation = "required assertion " + id + " failed"
							}
						}
					}
				}
			}
		}
	}
	if err = v.rpc.call(ctx, "report_verification_outcome", workorder.VerificationOutcomeRequest{ContextID: vc.ID, RunID: runID, State: state, Explanation: explanation, ExitCode: &exit}, nil); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(v.output, "%s: %s (attempt %s)\n", e.ID, state, runID)
	if state != "succeeded" {
		return fmt.Errorf("exercise %s: %s %s", e.ID, state, explanation)
	}
	return nil
}

func (v *kitVerifier) evidenceBatch(runID string, subject core.VerificationSubject, environment core.VerificationEnvironment, key, kind string, payload json.RawMessage, captured ...time.Time) ([]byte, error) {
	vc := v.snapshot.Contexts[0]
	at := time.Now().UTC().Format(time.RFC3339Nano)
	if len(captured) > 0 {
		at = captured[0].UTC().Format(time.RFC3339Nano)
	}
	e := core.VerificationEvidence{SchemaVersion: 1, ID: runID + "-" + key, SubmissionKey: key, Type: kind, CapturedAt: at, ReceivedAt: at, CapturedBy: core.VerificationCaptureActor{Identity: "conveyor-kit-runner", Kind: "tool", Version: "1", Attribution: "self_reported"}, WorkspaceID: vc.WorkspaceID, TaskID: vc.TaskID, WorkOrderID: vc.WorkOrderID, WorkOrderAttemptID: vc.WorkOrderAttemptID, ContextID: vc.ID, RunID: runID, Subject: subject, Revisions: vc.Revisions, GoverningPins: vc.GoverningPins, SafeInputs: v.safeInputValues(), Environment: environment, Payload: payload}
	// Strip headers and query values before the exact-secret redactor and before
	// either persistence boundary. Child output never supplies provenance.
	var value any
	if err := json.Unmarshal(payload, &value); err != nil {
		return nil, err
	}
	var sanitize func(any) any
	sanitize = func(value any) any {
		switch x := value.(type) {
		case map[string]any:
			for k, v := range x {
				lower := strings.ToLower(k)
				if strings.Contains(lower, "header") {
					x[k] = map[string]any{}
					continue
				}
				if strings.Contains(lower, "credential") || strings.Contains(lower, "password") || strings.Contains(lower, "token") {
					delete(x, k)
					continue
				}
				x[k] = sanitize(v)
			}
		case []any:
			for i, v := range x {
				x[i] = sanitize(v)
			}
		case string:
			if u, err := url.Parse(x); err == nil && (u.Scheme == "http" || u.Scheme == "https") {
				u.User = nil
				u.RawQuery = ""
				u.Fragment = ""
				return u.String()
			}
		}
		return value
	}
	e.Payload = core.JSONPayload(sanitize(value))
	data := core.JSONPayload(workorder.VerificationEvidenceRequest{ContextID: vc.ID, RunID: runID, SubmissionKey: key, Evidence: []json.RawMessage{core.JSONPayload(e)}})
	clean, _, err := v.redactor.RedactJSON(data)
	return clean, err
}

var _ io.Writer = (*kitBoundedOutput)(nil)

func kitExecutable(name, cwd string) (string, error) {
	if strings.ContainsRune(name, filepath.Separator) {
		if !filepath.IsAbs(name) {
			name = filepath.Join(cwd, name)
		}
		return filepath.EvalSymlinks(name)
	}
	for _, dir := range []string{"/usr/local/bin", "/usr/bin", "/bin"} {
		candidate := filepath.Join(dir, name)
		info, err := os.Stat(candidate)
		if err == nil && !info.IsDir() && info.Mode().Perm()&0111 != 0 {
			return filepath.EvalSymlinks(candidate)
		}
	}
	return "", fmt.Errorf("executable %s is unavailable in the approved runtime path", name)
}
func kitToolDigest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}
