package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/redact"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/verification"
	"github.com/kidus-tiliksew/conveyor/internal/workorder"
	"github.com/spf13/cobra"
)

type kitVerifyOptions struct {
	inputsPath, retryKey, replayAuthorization        string
	configPath, contextID, coveragePath, attemptRoot string
	withUI                                           bool
}

func kitVerifyCmd() *cobra.Command {
	o := kitVerifyOptions{}
	c := &cobra.Command{Use: "verify <task-id>", Short: "Run exact-revision verification under the current claim", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		root, err := os.Getwd()
		if err != nil {
			return err
		}
		return verifyKits(cmd.Context(), kitRPC{newClient(), os.Getenv("CONVEYOR_WORK_ORDER_ID"), os.Getenv("CONVEYOR_SESSION_ID"), os.Getenv("CONVEYOR_CLIENT_TOKEN")}, root, args[0], o, cmd.OutOrStdout())
	}}
	c.Flags().StringVar(&o.inputsPath, "inputs", "", "JSON safe input maps keyed by kit:<id>:<exercise> or ordinary:<id>")
	c.Flags().StringVar(&o.retryKey, "retry-key", "", "explicit new attempt key after replay admission")
	c.Flags().StringVar(&o.replayAuthorization, "replay-authorization", "", "operator recovery authorization for this retry")
	c.Flags().BoolVar(&o.withUI, "ui", false, "launch the optional loopback kit UI")
	c.Flags().StringVar(&o.configPath, "config", defaultLocalExecutionConfigPath(), "local execution configuration")
	c.Flags().StringVar(&o.contextID, "context-id", "", "prepared verification context (otherwise prepare the current claim)")
	c.Flags().StringVar(&o.coveragePath, "coverage", "", "JSON coverage mapping for registered ordinary obligations and selected kits")
	c.Flags().StringVar(&o.attemptRoot, "attempt-root", "", "private output directory outside the checkout (defaults to the task cache)")
	return c
}

type kitVerifier struct {
	retryKey, replayAuthorization string
	inputs                        map[string]map[string]json.RawMessage
	safeInputs                    map[string]json.RawMessage
	withUI                        bool
	ui                            *verification.UI
	rpc                           kitRPC
	root                          string
	order                         core.WorkOrder
	task                          core.Task
	snapshot                      store.VerificationSnapshot
	config                        *config.Config
	coverage                      store.VerificationCoverage
	output                        io.Writer
	redactor                      *redact.Redactor
	mu                            sync.Mutex
	lost                          bool
}

func verifyKits(ctx context.Context, rpc kitRPC, root, taskID string, o kitVerifyOptions, output io.Writer) error {
	if rpc.order == "" || rpc.session == "" || rpc.claimToken == "" || rpc.client.workspace == "" {
		return fmt.Errorf("kit verify requires the exact launcher work order, session, claim token and workspace")
	}
	v := &kitVerifier{retryKey: o.retryKey, replayAuthorization: o.replayAuthorization, inputs: map[string]map[string]json.RawMessage{}, withUI: o.withUI, rpc: rpc, root: root, output: output, redactor: redact.New([]string{rpc.client.token, rpc.claimToken, os.Getenv("CONVEYOR_GIT_TOKEN"), os.Getenv(gitAskPassTokenEnv)})}
	var contract struct {
		Order core.WorkOrder `json:"work_order"`
		Task  core.Task      `json:"task"`
	}
	if err := rpc.call(ctx, "get_work_order", nil, &contract); err != nil {
		return err
	}
	v.order, v.task = contract.Order, contract.Task
	if o.retryKey != "" && !verification.VerificationBindingName(o.retryKey) {
		return fmt.Errorf("invalid retry key")
	}
	if o.inputsPath != "" {
		data, e := os.ReadFile(o.inputsPath)
		if e != nil {
			return e
		}
		if len(data) > 1<<20 {
			return fmt.Errorf("input file exceeds limit")
		}
		if e = core.DecodeVerificationRequest(data, &v.inputs); e != nil {
			return e
		}
	}

	if v.task.ID != taskID || v.order.ID != rpc.order || v.order.SessionID != rpc.session || v.order.Stage != core.StageVerify || v.order.State != core.WorkOrderClaimed {
		return fmt.Errorf("kit verify requires the live verify claim for this task")
	}
	if err := v.checkCheckout(ctx); err != nil {
		return err
	}
	cfg, err := config.Load(o.configPath)
	if err != nil {
		return err
	}
	v.config = cfg
	if o.contextID == "" {
		err = rpc.call(ctx, "prepare_verification", workorder.VerificationPrepareRequest{RequestKey: "kit-" + v.order.AttemptID}, &v.snapshot)
	} else {
		err = rpc.call(ctx, "get_verification_context", workorder.VerificationContextRequest{ContextID: o.contextID}, &v.snapshot)
	}
	if err != nil {
		return err
	}
	if len(v.snapshot.Contexts) != 1 {
		return fmt.Errorf("missing verification context")
	}
	vc := v.snapshot.Contexts[0]
	if vc.WorkOrderID != v.order.ID || vc.WorkOrderAttemptID != v.order.AttemptID || vc.SealedAt != nil {
		return fmt.Errorf("verification context is stale or sealed")
	}
	// The command supports only checkouts actually present on this machine.
	if len(vc.Revisions) != 1 || vc.Revisions[0].Repository != v.task.Repo || vc.Revisions[0].SHA != v.order.HeadSHA {
		return fmt.Errorf("verification scope requires its exact repository checkouts")
	}
	if o.coveragePath == "" {
		_ = json.NewEncoder(output).Encode(v.snapshot)
		return fmt.Errorf("context %s prepared; supply --coverage after registering ordinary obligations and operator permission grants", vc.ID)
	}
	data, err := os.ReadFile(o.coveragePath)
	if err != nil {
		return err
	}
	if err = core.DecodeVerificationRequest(data, &v.coverage); err != nil {
		return err
	}
	if err = store.ValidateVerificationCoverage(v.coverage, v.snapshot); err != nil {
		return err
	}
	if o.attemptRoot == "" {
		base := os.Getenv("XDG_CACHE_HOME")
		if base == "" {
			home, e := os.UserHomeDir()
			if e != nil {
				return e
			}
			base = filepath.Join(home, ".cache")
		}
		o.attemptRoot = filepath.Join(base, "conveyor", taskID, "verification", vc.ID)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	v.root = root
	attemptRoot, err := filepath.Abs(o.attemptRoot)
	if err != nil {
		return err
	}
	attemptRoot, err = verification.ResolvePermissionPath(string(filepath.Separator), strings.TrimPrefix(attemptRoot, string(filepath.Separator)))
	if err != nil {
		return err
	}
	roots := []string{root}
	if common, e := localKitGit(ctx, root, "rev-parse", "--path-format=absolute", "--git-common-dir"); e == nil {
		roots = append(roots, filepath.Dir(strings.TrimSpace(string(common))))
	}
	for _, checkout := range roots {
		rel, e := filepath.Rel(checkout, attemptRoot)
		if e != nil || rel == "." || !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("attempt directory must be outside all checkout inputs")
		}
	}
	if err = os.MkdirAll(attemptRoot, 0700); err != nil {
		return err
	}
	if info, e := os.Stat(attemptRoot); e != nil || info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("attempt directory must be private")
	}
	var subjects []store.VerificationSubjectContract
	for _, s := range v.snapshot.Selections {
		if len(s.Receipt.Diagnostics) > 0 {
			return fmt.Errorf("manifest discovery is invalid or unavailable")
		}
		subjects = append(subjects, s.Subjects...)
	}
	for _, ob := range v.snapshot.Obligations {
		for _, id := range v.coverage.ObligationIDs {
			if ob.ID == id {
				subjects = append(subjects, store.VerificationSubjectContract{Subject: core.VerificationSubject{Kind: "ordinary", ObligationID: ob.ID, ContractDigest: ob.Digest}, Contract: ob.Contract})
			}
		}
	}
	for _, subject := range subjects {
		if err = v.run(ctx, subject, attemptRoot); err != nil {
			message, _ := v.redactor.Redact("Verification blocked for " + subject.Contract.ID + ": " + err.Error())
			v.mu.Lock()
			lost := v.lost
			v.mu.Unlock()
			if !lost {
				_ = rpc.call(ctx, "report_progress", map[string]string{"message": message}, nil)
			}
			return err
		}
	}
	_, err = fmt.Fprintf(output, "Verification exercises recorded for context %s. Submit coverage and the verification result through submit_verification.\n", vc.ID)
	return err
}

func (v *kitVerifier) checkCheckout(ctx context.Context) error {
	for _, args := range [][]string{{"symbolic-ref", "--quiet", "HEAD"}, {"rev-parse", "HEAD"}, {"status", "--porcelain=v1", "--untracked-files=all"}} {
		data, err := localKitGit(ctx, v.root, args...)
		if err != nil {
			return fmt.Errorf("verification checkout must remain attached and readable: %w", err)
		}
		if args[0] == "rev-parse" && strings.TrimSpace(string(data)) != v.order.HeadSHA {
			return fmt.Errorf("verification HEAD differs from submitted SHA")
		}
		if args[0] == "status" && len(data) > 0 {
			return fmt.Errorf("verification checkout is dirty")
		}
	}
	remote, err := localKitGit(ctx, v.root, "remote", "get-url", "origin")
	if err != nil {
		return err
	}
	if len(v.snapshot.Contexts) > 0 && len(v.snapshot.Contexts[0].Revisions) > 0 && normalizeKitRemote(string(remote)) != normalizeKitRemote(v.snapshot.Contexts[0].Revisions[0].RemoteIdentity) {
		return fmt.Errorf("verification repository identity differs from context")
	}
	return nil
}
func normalizeKitRemote(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "git@github.com:")
	s = strings.TrimPrefix(s, "https://github.com/")
	s = strings.TrimPrefix(s, "ssh://git@github.com/")
	return strings.TrimSuffix(s, ".git")
}

func (v *kitVerifier) live(ctx context.Context, grantID string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.lost || !v.order.LeaseExpiresAt.After(time.Now()) || (!v.order.ExecutionDeadline.IsZero() && !v.order.ExecutionDeadline.After(time.Now())) {
		v.lost = true
		return fmt.Errorf("verification claim lost or expired")
	}
	var order core.WorkOrder
	if err := v.rpc.call(ctx, "renew_work_order", nil, &order); err != nil {
		if ctx.Err() == nil {
			v.lost = true
		}
		return err
	}
	if order.State != core.WorkOrderClaimed || order.SessionID != v.rpc.session || order.AttemptID != v.order.AttemptID {
		v.lost = true
		return fmt.Errorf("verification claim changed")
	}
	v.order = order
	var snapshot store.VerificationSnapshot
	if err := v.rpc.call(ctx, "get_verification_context", workorder.VerificationContextRequest{ContextID: v.snapshot.Contexts[0].ID}, &snapshot); err != nil {
		if ctx.Err() == nil {
			v.lost = true
		}
		return err
	}
	for _, r := range snapshot.PermissionRevocations {
		if r.GrantID == grantID {
			v.lost = true
			return fmt.Errorf("verification permission grant revoked")
		}
	}
	return nil
}

// run uses the same process-group teardown as the worker launcher. A replayed
// start receipt never authorizes a second launch.
func (v *kitVerifier) run(ctx context.Context, subject store.VerificationSubjectContract, attemptRoot string) error {
	e := subject.Contract
	if err := v.checkCheckout(ctx); err != nil {
		return err
	}
	vc := v.snapshot.Contexts[0]
	kitRoot := v.root
	v.ui = nil
	if subject.Subject.Kind == "kit" {
		pins := []verification.Pin{}
		for _, p := range vc.GoverningPins {
			pins = append(pins, verification.Pin{Kind: p.Kind, DocumentID: p.DocumentID, Version: p.Version})
		}
		receipt, err := validateLocalKits(ctx, v.root, "verify", pins)
		if err != nil {
			return err
		}
		found := false
		for _, k := range receipt.Kits {
			if k.KitID == subject.Subject.KitID && k.Digest == subject.Subject.ContentDigest && k.Eligibility == "eligible" {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("kit content digest differs from server receipt")
		}
		manifestBytes, err := os.ReadFile(filepath.Join(v.root, ".conveyor/kits/manifest.yaml"))
		if err != nil {
			return err
		}
		check, err := verification.FilesystemPathCheck(v.root)
		if err != nil {
			return err
		}
		manifest, err := verification.Parse(strings.NewReader(string(manifestBytes)), check)
		if err != nil {
			return err
		}
		for _, k := range manifest.Kits {
			if k.ID == subject.Subject.KitID {
				kitRoot = filepath.Join(v.root, k.Path)
				if v.withUI {
					v.ui = k.UI
				}
			}
		}
	}
	var grant *store.VerificationPermissionGrant
	for i := range v.snapshot.PermissionGrants {
		g := &v.snapshot.PermissionGrants[i]
		if g.Subject == subject.Subject {
			grant = g
		}
	}
	if grant == nil {
		return fmt.Errorf("blocked: missing work-order authorization for exercise %s", e.ID)
	}
	if err := v.live(ctx, grant.ID); err != nil {
		return err
	}
	local, err := v.config.KitActions(v.rpc.client.base, v.rpc.client.workspace, v.task.Repo)
	if err != nil {
		return err
	}
	actions, env, secrets, err := kitExerciseActions(e, kitRoot, v.task.Repo, local)
	if err != nil {
		return err
	}
	safe, inputEnv, inputActions, inputSecrets, inputErr := kitInputValues(e, v.inputs[core.VerificationOperationSubject(subject.Subject)], local)
	if inputErr != nil {
		return inputErr
	}
	v.safeInputs = safe
	env = append(env, inputEnv...)
	actions = append(actions, inputActions...)
	secrets = append(secrets, inputSecrets...)
	if v.ui != nil {
		actions = append(actions, verification.VerificationPermission{Kind: "operator_interaction", Binding: v.task.Repo})
	}
	effective, err := verification.RequireVerificationPermissions(actions, grant.Actions, local)
	if err != nil {
		return err
	}
	v.redactor = redact.New(append(secrets, v.rpc.client.token, v.rpc.claimToken, os.Getenv("CONVEYOR_GIT_TOKEN"), os.Getenv(gitAskPassTokenEnv)))

	cwd, err := verification.ResolvePermissionPath(kitRoot, e.Cwd)
	if err != nil {
		return err
	}
	environment := core.VerificationEnvironment{Target: v.task.Repo, OS: runtime.GOOS, Architecture: runtime.GOARCH, Runtime: runtime.Version(), Deployment: "unknown", Attributes: map[string]string{"external_state": "unknown"}}
	startKey := "runner-" + fmt.Sprintf("%x", sha256.Sum256(core.JSONPayload(subject.Subject)))
	if v.retryKey != "" {
		startKey += "-" + v.retryKey
	}
	for _, a := range v.snapshot.Attempts {
		if a.StartKey == startKey {
			if err := v.resumeSpool(ctx, attemptRoot, a); err != nil {
				return err
			}
			return fmt.Errorf("attempt %s already exists (%s); retained evidence retried without relaunch; reconcile or supply an authorized --retry-key", a.ID, a.State)
		}
	}
	var receipt store.VerificationReceipt
	if err = v.rpc.call(ctx, "start_verification_attempt", workorder.VerificationStartRequest{ContextID: vc.ID, StartKey: startKey, Subject: subject.Subject, GrantID: grant.ID, EffectiveActions: effective, EffectivePermissions: e.Permissions, SafeInputs: v.safeInputs, Environment: environment, Coverage: v.coverage, ReplayAuthorizationID: v.replayAuthorization}, &receipt); err != nil {
		return err
	}
	if len(e.Argv) == 0 {
		return v.observe(ctx, e, receipt.ID, grant.ID)
	}
	return v.launch(ctx, e, cwd, attemptRoot, receipt.ID, grant.ID, subject.Subject, environment, env)
}
