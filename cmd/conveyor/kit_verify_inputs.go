package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/verification"
	"github.com/kidus-tiliksew/conveyor/internal/workorder"
)

func kitInputValues(e verification.Exercise, provided map[string]json.RawMessage, local []verification.VerificationPermission) (map[string]json.RawMessage, []string, []verification.VerificationPermission, []string, error) {
	safe, actual := map[string]json.RawMessage{}, map[string]json.RawMessage{}
	actions := []verification.VerificationPermission{}
	secrets := []string{}
	declared := map[string]bool{}
	for _, input := range e.Inputs {
		declared[input.Name] = true
		value, present := provided[input.Name]
		if input.Sensitive {
			if present {
				return nil, nil, nil, nil, fmt.Errorf("sensitive input %s must use an approved credential handle, not an input file", input.Name)
			}
			for _, grant := range local {
				if grant.Kind == "credential" && grant.Binding == input.Name {
					if !strings.HasPrefix(grant.Target, "CONVEYOR_KIT_SECRET_") {
						return nil, nil, nil, nil, fmt.Errorf("sensitive input %s requires a kit credential handle", input.Name)
					}
					secret := os.Getenv(grant.Target)
					if secret == "" {
						continue
					}
					if present {
						return nil, nil, nil, nil, fmt.Errorf("ambiguous sensitive input binding %s", input.Name)
					}
					for _, parentSecret := range kitParentSecrets() {
						if strings.Contains(secret, parentSecret) {
							return nil, nil, nil, nil, fmt.Errorf("factory credential refused as input %s", input.Name)
						}
					}
					if input.Type == "string" {
						value, _ = json.Marshal(secret)
					} else {
						value = json.RawMessage(secret)
					}
					present = true
					actions = append(actions, grant)
					secrets = append(secrets, secret)
				}
			}
		}
		if !present {
			if input.Required {
				return nil, nil, nil, nil, fmt.Errorf("missing required input %s", input.Name)
			}
			continue
		}
		var decoded any
		decoder := json.NewDecoder(bytes.NewReader(value))
		decoder.UseNumber()
		if err := decoder.Decode(&decoded); err != nil {
			return nil, nil, nil, nil, fmt.Errorf("input %s is invalid JSON", input.Name)
		}
		valid := false
		switch input.Type {
		case "string":
			_, valid = decoded.(string)
		case "boolean":
			_, valid = decoded.(bool)
		case "number", "integer":
			if n, ok := decoded.(json.Number); ok {
				f, err := n.Float64()
				valid = err == nil && !math.IsInf(f, 0) && !math.IsNaN(f) && (input.Type != "integer" || math.Trunc(f) == f)
			}
		}
		if !valid {
			return nil, nil, nil, nil, fmt.Errorf("input %s requires %s", input.Name, input.Type)
		}
		if text, ok := decoded.(string); ok {
			for _, parentSecret := range kitParentSecrets() {
				if strings.Contains(text, parentSecret) {
					return nil, nil, nil, nil, fmt.Errorf("factory or forge credential refused as input %s", input.Name)
				}
			}
			if !input.Sensitive {
				for _, entry := range os.Environ() {
					name, secret, _ := strings.Cut(entry, "=")
					if strings.HasPrefix(name, "CONVEYOR_KIT_SECRET_") && secret != "" && strings.Contains(text, secret) {
						return nil, nil, nil, nil, fmt.Errorf("credential value refused in safe input %s", input.Name)
					}
				}
			}
		}
		actual[input.Name] = value
		if input.Sensitive {
			safe[input.Name] = json.RawMessage(`"[redacted]"`)
		} else {
			safe[input.Name] = value
		}
	}
	for name := range provided {
		if !declared[name] {
			return nil, nil, nil, nil, fmt.Errorf("undeclared input %s", name)
		}
	}
	encoded, err := json.Marshal(actual)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	return safe, []string{"CONVEYOR_KIT_INPUTS=" + string(encoded)}, actions, secrets, nil
}

func (v *kitVerifier) safeInputValues() map[string]json.RawMessage {
	if v.safeInputs == nil {
		return map[string]json.RawMessage{}
	}
	return v.safeInputs
}

func (v *kitVerifier) resumeSpool(ctx context.Context, root string, attempt store.VerificationAttempt) error {
	dir := filepath.Join(root, attempt.ID, "spool")
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	spool := verification.EvidenceSpool{Directory: dir, Limit: 8 << 20, Check: func(ctx context.Context) error { return v.live(ctx, attempt.GrantID) }}
	return spool.Flush(ctx, func(ctx context.Context, b []byte) error {
		var batch workorder.VerificationEvidenceRequest
		if err := json.Unmarshal(b, &batch); err != nil {
			return err
		}
		if batch.ContextID != attempt.ContextID || batch.RunID != attempt.ID {
			return fmt.Errorf("spool ownership differs from attempt")
		}
		clean, _, err := v.redactor.RedactJSON(b)
		if err != nil {
			return err
		}
		return v.rpc.call(ctx, "submit_verification_evidence", json.RawMessage(clean), nil)
	})
}

func (v *kitVerifier) observe(ctx context.Context, e verification.Exercise, runID, grantID string) error {
	if e.Kind != "observation" {
		return fmt.Errorf("missing runnable entrypoint")
	}
	deadline := time.Now().Add(time.Duration(e.TimeoutSeconds) * time.Second)
	if !v.order.ExecutionDeadline.IsZero() && v.order.ExecutionDeadline.Before(deadline) {
		deadline = v.order.ExecutionDeadline
	}
	waitCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	state, explanation := "waiting", "ordinary observation requires its declared completion evidence"
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if err := v.live(waitCtx, grantID); err != nil {
			if waitCtx.Err() != nil {
				break
			}
			return err
		}
		var snapshot store.VerificationSnapshot
		if err := v.rpc.call(waitCtx, "get_verification_context", workorder.VerificationContextRequest{ContextID: v.snapshot.Contexts[0].ID}, &snapshot); err != nil {
			if waitCtx.Err() != nil {
				break
			}
			return err
		}
		if store.ValidateVerificationSuccess(snapshot, runID, nil) == nil {
			state, explanation = "succeeded", ""
			break
		}
		select {
		case <-waitCtx.Done():
		case <-ticker.C:
		}
		if waitCtx.Err() != nil {
			break
		}
	}
	finishCtx, finishCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer finishCancel()
	if err := v.live(finishCtx, grantID); err != nil {
		return err
	}
	if err := v.rpc.call(finishCtx, "report_verification_outcome", workorder.VerificationOutcomeRequest{ContextID: v.snapshot.Contexts[0].ID, RunID: runID, State: state, Explanation: explanation}, nil); err != nil {
		return err
	}
	if state != "succeeded" {
		return fmt.Errorf("%s", explanation)
	}
	return nil
}

// An approved kit handle cannot alias an ambient factory, forge or harness
// credential. No ambient credential names or values enter the child environment.
func kitParentSecrets() []string {
	var secrets []string
	for _, entry := range os.Environ() {
		name, value, ok := strings.Cut(entry, "=")
		upper := strings.ToUpper(name)
		if !ok || value == "" || strings.HasPrefix(upper, "CONVEYOR_KIT_SECRET_") {
			continue
		}
		if strings.Contains(upper, "TOKEN") || strings.Contains(upper, "SECRET") || strings.Contains(upper, "PASSWORD") || strings.Contains(upper, "API_KEY") || strings.Contains(upper, "PRIVATE_KEY") {
			secrets = append(secrets, value)
		}
	}
	return secrets
}
