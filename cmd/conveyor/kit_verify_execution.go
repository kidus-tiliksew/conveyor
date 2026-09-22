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
	"regexp"
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
			for _, parentSecret := range kitParentSecrets() {
				if strings.Contains(value, parentSecret) {
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
			if _, err := kitExecutable(p.EnvironmentBinding, root); err != nil {
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
		if v.lost || !v.order.LeaseExpiresAt.After(time.Now()) || (!v.order.ExecutionDeadline.IsZero() && !v.order.ExecutionDeadline.After(time.Now())) {
			return fmt.Errorf("verification authority lost")
		}
		return nil
	}
	spool := verification.EvidenceSpool{Directory: spoolDir, Limit: 8 << 20, Check: barrier}
	upload := func(ctx context.Context, data []byte) error {
		if err := v.live(ctx, grantID); err != nil {
			return err
		}
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
			return nil, fmt.Errorf("operation was not registered before launch")
		}

		if completed[signal.StepID] && signal.Type == "operation.dispatching" {
			return map[string]any{"ID": operationID, "State": "applied", "DispatchAuthorized": false}, nil
		}
		signal.ProviderReference = kitSafeReference(signal.ProviderReference)
		signal.ProviderReference, _ = v.redactor.Redact(signal.ProviderReference)
		var receipt store.VerificationReceipt
		if err := v.rpc.call(requestCtx, "prepare_verification_operation", workorder.VerificationOperationRequest{ContextID: vc.ID, RunID: runID, Action: strings.TrimPrefix(signal.Type, "operation."), OperationID: operationID, Source: "kit-child", CapturedAt: signal.CapturedAt, ProviderReference: signal.ProviderReference}, &receipt); err != nil {
			// Resolve the durable record before replying to an uncertain request.
			// A recorded dispatch is evidence, never renewed provider-call authority.
			var current store.VerificationSnapshot
			_ = v.rpc.call(requestCtx, "get_verification_context", workorder.VerificationContextRequest{ContextID: vc.ID}, &current)
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
	// Register the entire closed operation set before any child starts (VK-4.1).
	// A child that exits without signalling must still leave durable uncertainty.
	for _, op := range e.Operations {
		if err := v.live(launchCtx, grantID); err != nil {
			return err
		}
		key := fmt.Sprintf("%x", sha256.Sum256(core.JSONPayload([]any{v.task.ID, core.VerificationOperationSubject(subject), op.ID, op.TargetBinding, vc.Revisions})))
		var receipt store.VerificationReceipt
		if err := v.rpc.call(launchCtx, "prepare_verification_operation", workorder.VerificationOperationRequest{ContextID: vc.ID, RunID: runID, Action: "prepare", Key: key, StepID: op.ID, Target: op.TargetBinding, InputDigest: store.VerificationSafeInputDigest(v.safeInputValues()), ReplayAuthorizationID: v.replayAuthorization}, &receipt); err != nil {
			return err
		}
		operations[op.ID] = receipt.ID
		if receipt.State == "applied" || receipt.State == "completed" {
			completed[op.ID] = true
		}
	}
	connection, _ := json.Marshal(map[string]any{"url": channel.URL + "/operations", "nonce": channel.Nonce, "operations": operations})
	env = append(env, "CONVEYOR_KIT_OPERATIONS="+string(connection), "CONVEYOR_KIT_ATTEMPT_DIR="+dir, "HOME="+dir, "TMPDIR="+dir)
	stdout, stderr := &kitBoundedOutput{limit: 1 << 20}, &kitBoundedOutput{limit: 1 << 20}
	tool, err := kitExecutable(e.Argv[0], cwd)
	if err != nil {
		return v.prelaunchBlocked(launchCtx, dir, runID, grantID, "exercise executable is unavailable")
	}
	before, err := kitToolDigest(tool)
	if err != nil {
		return v.prelaunchBlocked(launchCtx, dir, runID, grantID, "exercise executable digest could not be prepared")
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
	if v.ui != nil {
		for _, arg := range v.ui.Argv {
			clean, _ := v.redactor.Redact(arg)
			if clean != arg {
				return fmt.Errorf("sensitive UI argv refused")
			}
		}
	}
	if err := v.live(launchCtx, grantID); err != nil {
		return err
	}
	if err := launchCtx.Err(); err != nil {
		return err
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
		ui, uiStartErr = startKitUI(v.ui, v.uiRoot, append(env, "CONVEYOR_KIT_UI_HOST=127.0.0.1", fmt.Sprintf("CONVEYOR_KIT_UI_PORT=%d", v.ui.Port)))
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
	if hashErr != nil {
		environment.Attributes["tool_sha256_after"] = "unknown"
	}
	sourceErr := v.checkCheckout(ctx)
	if sourceErr != nil {
		environment.Attributes["source_state"] = "changed"
	}
	ended := time.Now().UTC()
	timedOut := errors.Is(launchCtx.Err(), context.DeadlineExceeded)
	cancelled := launchCtx.Err() != nil && !timedOut
	exit := command.ProcessState.ExitCode()
	cleanOut, _ := v.redactor.Redact(kitSanitizeText(stdout.String()))
	cleanErr, _ := v.redactor.Redact(kitSanitizeText(stderr.String()))
	// A truncated secret may no longer match its complete credential value.
	// Retain a marker instead of persisting a potentially revealing prefix.
	if stdout.truncated {
		cleanOut = "[output exceeded capture limit]"
	}
	if stderr.truncated {
		cleanErr = "[output exceeded capture limit]"
	}
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
	// Keep bounded sanitized output beside the spool for offline diagnosis.
	// The execution report hashes these exact bytes; output is never promoted
	// into another attempt or uploaded as a different evidence type.
	outputSpool := verification.EvidenceSpool{Directory: dir, Limit: 16 << 20, Check: barrier}
	if err := outputSpool.Put(ctx, "output", core.JSONPayload(map[string]string{"stdout": cleanOut, "stderr": cleanErr})); err != nil {
		return err
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
			if state != "timed_out" && state != "cancelled" {
				state = "blocked"
			}
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

// VK-4 / VK-10: an authorized attempt that cannot launch stays missing
// execution evidence. Report only while the original authority remains valid;
// leave prepared operations untouched for the server's reconciliation rules.
func (v *kitVerifier) prelaunchBlocked(ctx context.Context, dir, runID, grantID, diagnostic string) error {
	// Callers supply fixed diagnostics, never raw paths, argv, or credentials.
	if err := v.checkCheckout(ctx); err != nil {
		return fmt.Errorf("%s; outcome not reported: %w", diagnostic, err)
	}
	if err := v.live(ctx, grantID); err != nil {
		return fmt.Errorf("%s; outcome not reported: %w", diagnostic, err)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%s; outcome not reported: %w", diagnostic, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "prelaunch.json"), core.JSONPayload(map[string]string{"diagnostic": diagnostic}), 0600); err != nil {
		return fmt.Errorf("%s; retain diagnostic: %w", diagnostic, err)
	}
	if err := v.rpc.call(ctx, "report_verification_outcome", workorder.VerificationOutcomeRequest{ContextID: v.snapshot.Contexts[0].ID, RunID: runID, State: "blocked", Explanation: diagnostic}, nil); err != nil {
		return fmt.Errorf("%s; outcome reporting failed: %w", diagnostic, err)
	}
	_, _ = fmt.Fprintf(v.output, "%s: blocked (attempt %s)\n", diagnostic, runID)
	return fmt.Errorf("%s", diagnostic)
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
				if strings.Contains(lower, "credential") || strings.Contains(lower, "password") || strings.Contains(lower, "token") || strings.Contains(lower, "secret") || lower == "api_key" || lower == "authorization" || lower == "cookie" || lower == "set-cookie" {
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
			return kitSanitizeText(x)
		}
		return value
	}
	e.Payload = core.JSONPayload(sanitize(value))
	data := core.JSONPayload(workorder.VerificationEvidenceRequest{ContextID: vc.ID, RunID: runID, SubmissionKey: key, Evidence: []json.RawMessage{core.JSONPayload(e)}})
	clean, _, err := v.redactor.RedactJSON(data)
	if err != nil {
		return nil, err
	}
	var batch workorder.VerificationEvidenceRequest
	if err := json.Unmarshal(clean, &batch); err != nil {
		return nil, err
	}
	for _, raw := range batch.Evidence {
		var check core.VerificationEvidence
		if err := core.DecodeVerificationRequest(raw, &check); err != nil {
			return nil, err
		}
		// The server replaces submission attribution from its authenticated
		// context. This local identity is used only to validate the envelope.
		check.SubmittedBy = "runner-local-validation"
		if err := check.Validate(core.VerificationEvidenceAuthority{SubmittedBy: check.SubmittedBy}); err != nil {
			return nil, err
		}
	}
	return clean, nil
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

func kitSafeReference(raw string) string {
	if u, err := url.Parse(raw); err == nil && (u.Scheme == "http" || u.Scheme == "https") {
		u.User, u.RawQuery, u.Fragment = nil, "", ""
		return u.String()
	}
	return raw
}

var kitURLPattern = regexp.MustCompile(`https?://[^\s"<>]+`)
var kitHeaderPattern = regexp.MustCompile(`(?im)(?:authorization|proxy-authorization|cookie|set-cookie|x-api-key)\s*:[^\r\n]*`)

func kitSanitizeText(raw string) string {
	clean := kitURLPattern.ReplaceAllStringFunc(raw, kitSafeReference)
	return kitHeaderPattern.ReplaceAllString(clean, "[redacted header]")
}
