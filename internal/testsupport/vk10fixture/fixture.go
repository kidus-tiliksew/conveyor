// Package vk10fixture drives VK-10 through the shipped CLI and authenticated
// HTTP, work-order, store and dispatcher boundaries using disposable fixtures.
package vk10fixture

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/dispatch"
	"github.com/kidus-tiliksew/conveyor/internal/httpapi"
	"github.com/kidus-tiliksew/conveyor/internal/pack"
	"github.com/kidus-tiliksew/conveyor/internal/pipeline"
	"github.com/kidus-tiliksew/conveyor/internal/queue"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/trigger/github"
	"github.com/kidus-tiliksew/conveyor/internal/verification"
	"github.com/kidus-tiliksew/conveyor/internal/worker"
	"github.com/kidus-tiliksew/conveyor/internal/workorder"
)

type fixture struct {
	t                                            *testing.T
	b                                            store.Backend
	ctx                                          context.Context
	ws, token, claimToken, root, head, base, cli string
	cfg                                          *config.Config
	d                                            *dispatch.Dispatcher
	s                                            *workorder.Service
	api, forge, provider                         *httptest.Server
	task                                         core.Task
	order                                        core.WorkOrder
	snapshot                                     store.VerificationSnapshot
	coverage                                     store.VerificationCoverage
	mu                                           sync.Mutex
	uiObserved                                   bool
	uiComplete                                   bool
	fail                                         bool
	pr                                           github.SubmissionPullRequest
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func git(t *testing.T, root string, args ...string) []byte {
	t.Helper()
	c := exec.CommandContext(t.Context(), "git", args...)
	c.Dir = root
	c.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + root, "GIT_CONFIG_NOSYSTEM=1", "GIT_AUTHOR_NAME=VK fixture", "GIT_AUTHOR_EMAIL=fixture@example.test", "GIT_COMMITTER_NAME=VK fixture", "GIT_COMMITTER_EMAIL=fixture@example.test"}
	b, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, b)
	}
	return b
}

func write(t *testing.T, path string, b []byte) {
	t.Helper()
	must(t, os.MkdirAll(filepath.Dir(path), 0700))
	must(t, os.WriteFile(path, b, 0600))
}

func repository(t *testing.T, mode string) (string, string, string) {
	t.Helper()
	source := os.Getenv("CONVEYOR_VK10_SOURCE")
	if source == "" {
		t.Fatal("missing evidence: run the VK-10 Make prerequisite")
	}
	root := filepath.Join(t.TempDir(), "repository")
	must(t, os.Mkdir(root, 0700))
	git(t, root, "init", "-b", "main")
	write(t, filepath.Join(root, "fixture.txt"), []byte(core.NewTaskID()))
	git(t, root, "add", ".")
	git(t, root, "commit", "-m", "fixture: unique base")
	base := strings.TrimSpace(string(git(t, root, "rev-parse", "HEAD")))
	git(t, root, "checkout", "-b", "conveyor/fixture")
	git(t, root, "remote", "add", "origin", "https://github.com/fixture/conveyor")
	for _, path := range []string{".conveyor/kits/manifest.yaml", ".conveyor/kits/vk10-script/run.sh", ".conveyor/kits/vk10-loopback-ui/server.sh", ".conveyor/kits/vk10-loopback-ui/index.html"} {
		if mode == "absent" && strings.HasSuffix(path, "manifest.yaml") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(source, path))
		must(t, err)
		if strings.HasSuffix(path, "manifest.yaml") {
			var m verification.Manifest
			must(t, json.Unmarshal(b, &m))
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			must(t, err)
			port := listener.Addr().(*net.TCPAddr).Port
			must(t, listener.Close())
			for i := range m.Kits {
				if m.Kits[i].UI != nil {
					m.Kits[i].UI.Port = port
				}
			}
			b, err = json.Marshal(m)
			must(t, err)
		}
		write(t, filepath.Join(root, path), b)
	}
	git(t, root, "add", ".")
	git(t, root, "commit", "-m", "fixture: verification kits")
	return root, base, strings.TrimSpace(string(git(t, root, "rev-parse", "HEAD")))
}

func newFixture(t *testing.T, b store.Backend, mode string) *fixture {
	t.Helper()
	f := &fixture{t: t, b: b, ws: "vk10-" + core.NewTaskID(), token: "fixture-pat-" + core.NewTaskID(), cli: os.Getenv("CONVEYOR_VK10_CLI")}
	if !filepath.IsAbs(f.cli) {
		t.Fatal("missing evidence: exact-source CLI prerequisite was not supplied")
	}
	f.ctx = store.WithWorkspace(t.Context(), f.ws)
	f.root, f.base, f.head = repository(t, mode)
	f.cfg = &config.Config{Workspace: f.ws, Repos: []config.Repo{{Name: "conveyor", URL: "https://github.com/fixture/conveyor", GitHub: "fixture/conveyor", Base: "main"}}, Routing: config.Routing{Stages: map[string]config.StageRoute{}}}
	for _, stage := range []string{"implement", "verify", "review"} {
		f.cfg.Routing.Stages[stage] = config.StageRoute{Execution: config.ExecutionMCP, Timeout: time.Hour, TimeoutText: "1h"}
	}
	_, err := b.BootstrapWorkspaceConfig(f.ctx, f.cfg)
	must(t, err)
	_, err = b.BootstrapIdentity(f.ctx, config.FirstOperatorIdentity{OrganizationName: "Fixture", Email: "fixture@example.test", DisplayName: "Fixture operator"}, f.token)
	must(t, err)
	owner, err := b.VerifyPersonalAccessToken(f.ctx, f.token)
	must(t, err)
	f.ctx = store.WithActor(store.WithCredential(f.ctx, core.AuthenticatedCredential{ID: "fixture-pat", Kind: core.CredentialUser, Scope: core.CredentialScopeUser, OwnerUserID: owner.ID}), store.Actor{ID: store.UserActorID(owner.ID), Role: core.ActorUser})
	_, rv, err := b.CreateRequirement(f.ctx, core.Requirement{ID: "req-verification-kits", Title: "Fixture verification contract"}, core.RequirementVersion{Content: "# Disposable fixture contract", Origin: core.RequirementOriginOperator, Statements: []core.RequirementStatement{{ID: "REQ-3", Statement: "Observe the disposable API.", AcceptanceCriteria: []core.AcceptanceCriterion{{ID: "AC-3.1", Statement: "Retain the API observation."}}}}})
	must(t, err)
	_, _, err = b.ConfirmRequirementVersion(f.ctx, "req-verification-kits", rv.Version)
	must(t, err)
	if mode == "ineligible" {
		rv, err = b.ProposeRequirementVersion(f.ctx, core.RequirementVersion{RequirementID: "req-verification-kits", Content: rv.Content, Origin: core.RequirementOriginOperator, Statements: rv.Statements})
		must(t, err)
		_, _, err = b.ConfirmRequirementVersion(f.ctx, "req-verification-kits", rv.Version)
		must(t, err)
	}
	content := "# Disposable VK-10 fixture\n\nVK-10: Observe the fixture.\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - .conveyor/kits/**\n```"
	_, dv, err := b.CreateSystemDesign(f.ctx, core.SystemDesign{ID: "feature-verification-kit-execution", Title: "Fixture kit execution", Category: "Feature"}, core.SystemDesignVersion{Content: content, Origin: core.SystemDesignOriginOperator})
	must(t, err)
	for version := 1; version <= 5; version++ {
		if version > 1 {
			dv, err = b.ProposeSystemDesignVersion(f.ctx, core.SystemDesignVersion{DocumentID: "feature-verification-kit-execution", Content: content, Origin: core.SystemDesignOriginOperator})
			must(t, err)
		}
		_, _, err = b.ConfirmSystemDesignVersion(f.ctx, "feature-verification-kit-execution", dv.Version)
		must(t, err)
	}
	f.provider = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.URL.Path == "/ui-done" {
			f.uiObserved = !f.fail
		}
		if r.URL.Path == "/ui-release" {
			f.uiComplete = true
		}
		observed := !f.fail
		if r.URL.Path == "/ui-status" {
			observed = f.uiObserved
		}
		if r.URL.Path == "/ui-complete" {
			observed = f.uiComplete
		}
		if r.URL.Path != "/observe" && r.URL.Path != "/ui-observe" && r.URL.Path != "/ui-status" && r.URL.Path != "/ui-done" && r.URL.Path != "/ui-release" && r.URL.Path != "/ui-complete" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"observed": observed, "fixture": f.ws})
	}))
	t.Cleanup(f.provider.Close)
	f.forge = httptest.NewServer(http.HandlerFunc(f.serveForge))
	t.Cleanup(f.forge.Close)
	b.ConfigureForgeTokenEncryptionKey(bytes.Repeat([]byte{37}, 32))
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	must(t, err)
	private := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	_, err = b.StoreWorkspaceGitHubApp(f.ctx, f.ws, core.WorkspaceGitHubAppCredential{WorkspaceGitHubAppStatus: core.WorkspaceGitHubAppStatus{AppID: 41, AppSlug: "fixture", ClientID: "fixture"}, PrivateKey: private})
	must(t, err)
	_, err = b.RecordWorkspaceGitHubAppInstallation(f.ctx, f.ws, 41, 12, "fixture")
	must(t, err)
	apps := github.NewAppClient(f.forge.Client(), f.forge.URL)
	f.d = dispatch.New(b, f.cfg, nil)
	f.d.GitHubApps = apps
	f.d.ReviewChangedPaths = func(context.Context, *config.Config, core.Task) ([]string, error) {
		return []string{".conveyor/kits/manifest.yaml"}, nil
	}
	pk, err := pack.Load("")
	must(t, err)
	f.d.Pack = pk
	f.s = &workorder.Service{Store: b, Dispatcher: f.d, Pack: pk, ConfigProvider: func(context.Context) (*config.Config, error) { return f.cfg, nil }, WorkspaceGitHubApps: b, GitHubApps: apps}
	f.s.SubmissionPR = func(context.Context, string, string) (github.SubmissionPullRequest, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.pr, nil
	}
	f.s.SubmissionChangedPaths = f.d.ReviewChangedPaths
	f.s.ReviewTarget = func(context.Context, string, string) (github.ReviewTarget, error) {
		return github.ReviewTarget{Number: f.pr.Number, URL: f.pr.URL, HeadSHA: f.head, BaseSHA: f.base}, nil
	}
	f.s.ReviewPRDescription = func(context.Context, string, string) (string, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.pr.Body, nil
	}
	f.s.ReviewDiffBetween = func(context.Context, string, string, string) (string, error) {
		return string(git(t, f.root, "diff", f.base, f.head)), nil
	}
	f.s.ReconcileSubmissionPR = func(_ context.Context, _ string, _ github.SubmissionPullRequest, body string) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.pr.Body = body
		return nil
	}
	f.d.ReadVerificationPR = func(context.Context, string, int) (github.VerificationPullRequest, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var pr github.VerificationPullRequest
		pr.Number, pr.Body, pr.Head.SHA = f.pr.Number, f.pr.Body, f.pr.Head.SHA
		return pr, nil
	}
	f.d.WriteVerificationPR = func(_ context.Context, _ string, _ int, body string) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.pr.Body = body
		return nil
	}
	server := httpapi.NewServer(b)
	server.WorkOrders, server.Workspaces, server.Workspace = f.s, b, f.ws
	server.Workers = &worker.Service{Store: b, WorkOrders: f.s}
	server.ConfigProvider = f.s.ConfigProvider
	f.api = httptest.NewServer(server.Handler())
	t.Cleanup(f.api.Close)
	f.task = core.Task{ID: core.NewTaskID(), Workspace: f.ws, Repo: "conveyor", Title: "VK-10 disposable fixture", Branch: "conveyor/fixture", BaseBranch: "main", State: core.TaskQueued, NextStage: core.StageImplement, MergeApproval: true, CreatedAt: time.Now().UTC()}
	f.task.SetupContract.VerifyStage = mode != "toggle-off"
	must(t, b.CreateTaskWithDependenciesAndContext(f.ctx, f.task, nil, store.TaskContextInput{RequirementIDs: []string{"req-verification-kits"}, DesignIDs: []string{"feature-verification-kit-execution"}}))
	f.pr.Number, f.pr.URL, f.pr.Head.Ref, f.pr.Head.SHA, f.pr.Base.Ref, f.pr.Base.SHA = 42, "https://github.com/fixture/conveyor/pull/42", f.task.Branch, f.head, "main", f.base
	return f
}

func (f *fixture) serveForge(w http.ResponseWriter, r *http.Request) {
	var value any
	switch {
	case r.URL.Path == "/app/installations/12":
		value = map[string]any{"id": 12, "app_id": 41, "account": map[string]string{"login": "fixture"}}
	case r.URL.Path == "/app/installations/12/access_tokens":
		value = map[string]any{"token": "fixture-installation-token", "expires_at": time.Now().Add(time.Hour).Truncate(time.Second)}
	case r.URL.Path == "/installation/repositories":
		value = map[string]any{"repositories": []any{map[string]string{"full_name": "fixture/conveyor"}}}
	case strings.HasPrefix(r.URL.Path, "/repos/fixture/conveyor/git/commits/"):
		sha := strings.TrimPrefix(r.URL.Path, "/repos/fixture/conveyor/git/commits/")
		value = map[string]any{"sha": sha, "tree": map[string]string{"sha": strings.TrimSpace(string(git(f.t, f.root, "rev-parse", sha+"^{tree}")))}}
	case strings.HasPrefix(r.URL.Path, "/repos/fixture/conveyor/git/trees/"):
		sha := strings.TrimPrefix(r.URL.Path, "/repos/fixture/conveyor/git/trees/")
		entries := []map[string]string{}
		for _, line := range strings.Split(strings.TrimSpace(string(git(f.t, f.root, "ls-tree", "-r", "-t", sha))), "\n") {
			fields := strings.Fields(line)
			if len(fields) != 4 {
				http.Error(w, "invalid tree", 500)
				return
			}
			entries = append(entries, map[string]string{"mode": fields[0], "type": fields[1], "sha": fields[2], "path": fields[3]})
		}
		value = map[string]any{"sha": sha, "truncated": false, "tree": entries}
	case strings.HasPrefix(r.URL.Path, "/repos/fixture/conveyor/git/blobs/"):
		b := git(f.t, f.root, "cat-file", "blob", strings.TrimPrefix(r.URL.Path, "/repos/fixture/conveyor/git/blobs/"))
		value = map[string]any{"encoding": "base64", "content": base64.StdEncoding.EncodeToString(b), "size": len(b)}
	default:
		http.NotFound(w, r)
		return
	}
	_ = json.NewEncoder(w).Encode(value)
}

func (f *fixture) rpc(name string, input any, out any) error {
	raw, err := json.Marshal(input)
	if err != nil {
		return err
	}
	args := map[string]any{}
	if input != nil {
		if err = json.Unmarshal(raw, &args); err != nil {
			return err
		}
	}
	args["workspace_id"], args["work_order_id"], args["session_id"], args["client_token"] = f.ws, f.order.ID, f.order.SessionID, f.claimToken
	body := core.JSONPayload(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}})
	req, err := http.NewRequestWithContext(f.t.Context(), "POST", f.api.URL+"/mcp", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+f.token)
	req.Header.Set("Content-Type", "application/json")
	res, err := f.api.Client().Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		return err
	}
	if res.StatusCode != 200 {
		return fmt.Errorf("%s HTTP %d: %s", name, res.StatusCode, b)
	}
	var response struct {
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err = json.Unmarshal(b, &response); err != nil {
		return err
	}
	if len(response.Result.Content) != 1 {
		return fmt.Errorf("%s invalid response: %s", name, b)
	}
	text := response.Result.Content[0].Text
	if response.Result.IsError {
		return fmt.Errorf("%s: %s", name, text)
	}
	if out != nil {
		reflect.ValueOf(out).Elem().SetZero()
		return json.Unmarshal([]byte(text), out)
	}
	return nil
}

func (f *fixture) claim(stage core.Stage) {
	f.t.Helper()
	must(f.t, f.d.DispatchNow(f.ctx, f.task.ID))
	orders, err := f.b.ListTaskWorkOrders(f.ctx, f.task.ID)
	must(f.t, err)
	for _, o := range orders {
		if o.Stage == stage && o.State == core.WorkOrderQueued {
			f.order = o
			f.order.SessionID = "fixture-" + core.NewTaskID()
			f.claimToken = "fixture-claim-" + core.NewTaskID()
			must(f.t, f.rpc("claim_work_order", map[string]any{"lease_seconds": 3600}, &f.order))
			return
		}
	}
	f.t.Fatalf("missing queued %s order: %+v", stage, orders)
}

func (f *fixture) rest(method, path string, input, out any) error {
	b := core.JSONPayload(input)
	req, err := http.NewRequestWithContext(f.t.Context(), method, f.api.URL+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+f.token)
	req.Header.Set("X-Workspace-ID", f.ws)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Conveyor-CSRF", "1")
	res, err := f.api.Client().Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	b, err = io.ReadAll(res.Body)
	if err != nil {
		return err
	}
	if res.StatusCode != 200 {
		return fmt.Errorf("%s: HTTP %d: %s", path, res.StatusCode, b)
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}

func (f *fixture) refresh() {
	f.t.Helper()
	must(f.t, f.rpc("get_verification_context", workorder.VerificationContextRequest{ContextID: f.snapshot.Contexts[0].ID}, &f.snapshot))
}

func (f *fixture) prepareRunner() (string, string, string) {
	t := f.t
	vc := f.snapshot.Contexts[0]
	inputs := map[string]any{}
	all := []core.VerificationSubject{}
	for _, selection := range f.snapshot.Selections {
		for _, s := range selection.Subjects {
			all = append(all, s.Subject)
		}
	}
	f.coverage = store.VerificationCoverage{ObligationIDs: []string{}, Justification: "The two fixture kits observe the same disposable API.", Sources: []store.VerificationCoverageSource{}}
	if len(all) == 0 {
		data, err := os.ReadFile(filepath.Join(os.Getenv("CONVEYOR_VK10_SOURCE"), ".conveyor/kits/manifest.yaml"))
		must(t, err)
		var m verification.Manifest
		must(t, json.Unmarshal(data, &m))
		contract := m.Kits[0].Exercises[0]
		contract.Argv = []string{"sh", ".conveyor/kits/vk10-script/run.sh"}
		version := 1
		for _, pin := range vc.GoverningPins {
			if pin.DocumentID == "req-verification-kits" {
				version = pin.Version
			}
		}
		contract.Supports[0].Version = version
		var receipt store.VerificationReceipt
		must(t, f.rpc("register_verification_obligation", workorder.VerificationObligationRequest{ContextID: vc.ID, ObligationID: "ordinary", Description: "Ordinary API observation remains required without eligible kits.", Sources: []workorder.VerificationSource{{DocumentID: "req-verification-kits", Version: version, SectionID: "AC-3.1"}}, Contract: contract}, &receipt))
		all = append(all, core.VerificationSubject{Kind: "ordinary", ObligationID: "ordinary", ContractDigest: receipt.Digest})
		f.coverage.ObligationIDs = []string{"ordinary"}
		f.coverage.Justification = "No eligible kits; the sourced ordinary API check remains required."
	}
	for _, p := range vc.GoverningPins {
		section := "VK-10"
		if p.Kind == "requirement" {
			section = "AC-3.1"
		}
		f.coverage.Sources = append(f.coverage.Sources, store.VerificationCoverageSource{Source: store.VerificationCoverageReference{DocumentID: p.DocumentID, Version: p.Version, SectionID: section}, Disposition: "covered", Explanation: "Inspect current fixture API observations.", Subjects: all})
	}
	network := core.VerificationPermission{Kind: "network", Binding: "fixture", Target: f.provider.URL}
	interaction := core.VerificationPermission{Kind: "operator_interaction", Binding: "conveyor"}
	for _, s := range all {
		actions := []core.VerificationPermission{network}
		if s.KitID == "vk10-loopback-ui" {
			actions = append(actions, interaction)
		}
		key := s.KitID
		if key == "" {
			key = s.ObligationID
		}
		must(t, f.rest("POST", "/v1/work-orders/"+f.order.ID+"/verification/permissions?workspace_id="+f.ws, store.VerificationPermissionRequest{ContextID: vc.ID, RequestKey: "grant-" + key, Subject: s, Actions: actions}, nil))
		inputs[core.VerificationOperationSubject(s)] = map[string]string{"context": vc.ID}
	}
	f.refresh()
	dir := t.TempDir()
	coveragePath, inputsPath, configPath := filepath.Join(dir, "coverage.json"), filepath.Join(dir, "inputs.json"), filepath.Join(dir, "config.yaml")
	write(t, coveragePath, core.JSONPayload(f.coverage))
	write(t, inputsPath, core.JSONPayload(inputs))
	grants := []config.KitPermissionGrant{{Server: f.api.URL, Workspace: f.ws, Repository: "conveyor", Binding: "fixture", Actions: []verification.VerificationPermission{network}}, {Server: f.api.URL, Workspace: f.ws, Repository: "conveyor", Binding: "conveyor", Actions: []verification.VerificationPermission{interaction}}}
	routes := map[string]any{}
	for _, stage := range []string{"triage", "spec", "implement", "verify", "review"} {
		execution := "mcp"
		if stage == "triage" {
			execution = "in_process"
		}
		routes[stage] = map[string]string{"model": "fixture", "execution": execution, "timeout": "1h"}
	}
	write(t, configPath, core.JSONPayload(map[string]any{"kit_permissions": grants, "routing": map[string]any{"stages": routes}}))
	_, err := config.Load(configPath)
	must(t, err)
	return coveragePath, inputsPath, configPath
}

func (f *fixture) runner(coveragePath, inputsPath, configPath string, ui bool, extra ...string) *exec.Cmd {
	args := []string{"--server", f.api.URL, "--workspace", f.ws, "kit", "verify", f.task.ID, "--config", configPath, "--coverage", coveragePath, "--inputs", inputsPath, "--context-id", f.snapshot.Contexts[0].ID, "--attempt-root", filepath.Join(f.t.TempDir(), "attempts")}
	if ui {
		args = append(args, "--ui")
	}
	args = append(args, extra...)
	c := exec.CommandContext(f.t.Context(), f.cli, args...)
	c.Dir = f.root
	c.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + f.t.TempDir(), "CONVEYOR_ADDR=" + f.api.URL, "CONVEYOR_API_TOKEN=" + f.token, "CONVEYOR_WORK_ORDER_ID=" + f.order.ID, "CONVEYOR_SESSION_ID=" + f.order.SessionID, "CONVEYOR_CLIENT_TOKEN=" + f.claimToken}
	return c
}

func (f *fixture) browser() []byte {
	t := f.t
	b, err := os.ReadFile(filepath.Join(f.root, ".conveyor/kits/manifest.yaml"))
	must(t, err)
	var m verification.Manifest
	must(t, json.Unmarshal(b, &m))
	port := m.Kits[1].UI.Port
	shot := filepath.Join(t.TempDir(), "capture.png")
	program := `const {chromium}=require('playwright');
(async()=>{ const browser=await chromium.launch({headless:true}); try {
const page=await browser.newPage();
const url=process.argv[1];
let ready=false;for(let i=0;i<200;i++){try{await page.goto(url);ready=true;break}catch{await new Promise(r=>setTimeout(r,100))}}
if(!ready)throw Error('missing evidence: loopback UI unavailable');
for(const expected of ['conveyor','vk10-loopback-ui','req-verification-kits v1','feature-verification-kit-execution v5',process.argv[4]])
 if(!(await page.locator('body').innerText()).includes(expected))throw Error('missing UI identity '+expected);
await page.keyboard.press('Tab');
if(await page.locator('#observe').evaluate(e=>e!==document.activeElement))throw Error('keyboard control unavailable');
await page.keyboard.press('Enter');
await page.locator('#result[data-observed="true"]').waitFor();
await page.screenshot({path:process.argv[2]});
await page.request.get(process.argv[3]+'/ui-done');
}finally{await browser.close()}})().catch(e=>{console.error(e);process.exit(1)});`
	c := exec.CommandContext(t.Context(), "node", "-e", program, fmt.Sprintf("http://127.0.0.1:%d", port), shot, f.provider.URL, f.snapshot.Contexts[0].ID)
	c.Dir = filepath.Join(os.Getenv("CONVEYOR_VK10_SOURCE"), "web")
	c.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "PLAYWRIGHT_BROWSERS_PATH=" + os.Getenv("PLAYWRIGHT_BROWSERS_PATH"), "TMPDIR=" + os.Getenv("TMPDIR")}
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("missing browser evidence: %v: %s", err, out)
	}
	b, err = os.ReadFile(shot)
	must(t, err)
	return b
}

func (f *fixture) addCaptures(png []byte) {
	t := f.t
	f.refresh()
	var template core.VerificationEvidence
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		f.refresh()
		for _, e := range f.snapshot.Evidence {
			if e.Envelope.Subject.KitID == "vk10-loopback-ui" {
				template = e.Envelope
				break
			}
		}
		if template.ID != "" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if template.ID == "" {
		t.Fatal("UI evidence unavailable")
	}
	hash := fmt.Sprintf("%x", sha256.Sum256(png))
	index := 0
	request := workorder.VerificationArtifactRequest{ContextID: template.ContextID, RunID: template.RunID, UploadID: "screenshot", Index: &index, Content: png}
	must(t, f.rpc("upload_verification_artifact", request, nil))
	request.Index, request.Content = nil, nil
	request.Finalize = &workorder.VerificationArtifactFinalize{Name: "vk10.png", ContentType: "image/png", SizeBytes: int64(len(png)), SHA256: hash, SanitationRecord: "Disposable fixture UI contains only fixture identity and API observations.", MaskingAttestation: "No production identities, credentials or provider data are rendered."}
	must(t, f.rpc("upload_verification_artifact", request, nil))
	template.ID, template.SubmissionKey, template.Type = template.RunID+"-visual", "visual", "visual_capture"
	template.CapturedAt = time.Now().UTC().Format(time.RFC3339Nano)
	template.CapturedBy = core.VerificationCaptureActor{Identity: "playwright-fixture", Kind: "tool", Version: "repository-lockfile", Attribution: "self_reported"}
	template.Artifacts = []core.VerificationArtifactReference{{ArtifactID: hash, SHA256: hash, MediaType: "image/png"}}
	template.Payload = core.JSONPayload(core.VisualCapturePayload{Artifact: core.VerificationReference{ArtifactID: hash, SHA256: hash}, MediaType: "image/png", CaptureTool: "Playwright Chromium", Target: "VK-10 loopback UI", CapturedAt: template.CapturedAt})
	req := workorder.VerificationEvidenceRequest{ContextID: template.ContextID, RunID: template.RunID, SubmissionKey: "visual", Evidence: []json.RawMessage{core.JSONPayload(template)}}
	var first, second store.VerificationReceipt
	must(t, f.rpc("submit_verification_evidence", req, &first))
	must(t, f.rpc("submit_verification_evidence", req, &second))
	if first.ID != second.ID {
		t.Fatal("evidence retry changed identity")
	}
	must(t, f.rest("POST", "/v1/tasks/"+f.task.ID+"/verification/observations?workspace_id="+f.ws, store.VerificationOperatorObservation{ContextID: template.ContextID, RunID: template.RunID, Key: "operator-fixture", Fact: "Fixture operator records the browser observation, without accepting the task.", Supporting: []core.VerificationReference{{EvidenceID: template.ID}}}, nil))
	f.refresh()
	types := map[string]bool{}
	for _, record := range f.snapshot.Evidence {
		e := record.Envelope
		types[e.Type] = true
		if e.WorkspaceID != f.ws || e.TaskID != f.task.ID || e.WorkOrderID != f.order.ID || e.WorkOrderAttemptID != f.order.AttemptID || e.ContextID != template.ContextID || e.Revisions[0].SHA != f.head {
			t.Fatal("evidence lost exact provenance")
		}
		var read store.VerificationEvidenceRecord
		must(t, f.rpc("read_verification_evidence", workorder.VerificationReadRequest{ContextID: e.ContextID, EvidenceID: e.ID}, &read))
		var before, after any
		must(t, json.Unmarshal(core.JSONPayload(e), &before))
		must(t, json.Unmarshal(core.JSONPayload(read.Envelope), &after))
		if !reflect.DeepEqual(before, after) {
			t.Fatalf("evidence read changed envelope: before=%s after=%s", core.JSONPayload(e), core.JSONPayload(read.Envelope))
		}
	}
	for _, kind := range []string{"api_exchange", "state_observation", "assertion_result", "execution_report", "visual_capture", "operator_observation"} {
		if !types[kind] {
			t.Fatalf("missing %s", kind)
		}
	}
	var artifact struct {
		Content  []byte `json:"content"`
		Complete bool   `json:"complete"`
	}
	must(t, f.rpc("read_verification_evidence", workorder.VerificationReadRequest{ContextID: template.ContextID, EvidenceID: template.ID, ArtifactID: hash}, &artifact))
	if !artifact.Complete || !bytes.Equal(artifact.Content, png) {
		t.Fatal("artifact readback or hash mismatch")
	}
}

func (f *fixture) publish() {
	args := queue.VerificationPublicationArgs{WorkspaceID: f.ws, Repository: "fixture/conveyor", PullRequestNumber: 42}
	for _, r := range f.d.Registrations(nil) {
		if r.Kind == args.Kind() {
			job := queue.Job{WorkspaceID: f.ws, ID: "vk10-publication", Args: core.JSONPayload(args), Attempt: 1, MaxAttempts: 5}
			must(f.t, r.Handle(f.ctx, job))
			f.mu.Lock()
			before := f.pr.Body
			f.mu.Unlock()
			must(f.t, r.Handle(f.ctx, job))
			f.mu.Lock()
			after := f.pr.Body
			f.mu.Unlock()
			if before != after || !strings.Contains(after, "conveyor:verification") {
				f.t.Fatalf("publication not marked or idempotent: %s", after)
			}
			return
		}
	}
	f.t.Fatal("publication worker unavailable")
}

func (f *fixture) review() {
	t := f.t
	verifyOrder := f.order
	f.claim(core.StageReview)
	if f.order.ID == verifyOrder.ID || f.order.AttemptID == verifyOrder.AttemptID {
		t.Fatal("review reused verify identity")
	}
	var contract workorder.Context
	must(t, f.rpc("get_work_order", nil, &contract))
	if contract.Order.HeadSHA != f.head {
		t.Fatal("review did not bind verified head")
	}
	f.refresh()
	vc := f.snapshot.Contexts[0]
	if vc.SealedAt == nil || vc.Result == nil {
		t.Fatal("review lacks sealed result")
	}
	a := &core.VerificationAssessment{ContextIDs: []string{vc.ID}, RunIDs: []string{}, EvidenceIDs: []string{}, Mappings: []core.VerificationAssessmentMapping{}}
	for _, run := range f.snapshot.Attempts {
		a.RunIDs = append(a.RunIDs, run.ID)
	}
	for _, e := range f.snapshot.Evidence {
		a.EvidenceIDs = append(a.EvidenceIDs, e.Envelope.ID)
	}
	for _, pin := range vc.GoverningPins {
		ac := ""
		if pin.Kind == "requirement" {
			ac = "AC-3.1"
		}
		a.Mappings = append(a.Mappings, core.VerificationAssessmentMapping{DocumentID: pin.DocumentID, Version: pin.Version, AcceptanceCriterionID: ac, EvidenceIDs: a.EvidenceIDs, Assessment: "Fixture reviewer inspected the sealed current observations."})
	}
	yes, no := true, false
	review := pipeline.Review{Verdict: "approve", ReasonCode: "approved", Summary: "Fixture reviewer inspected sealed evidence.", VerificationAssessment: a,
		RequirementCitations: &core.RequirementCitationAssessment{Applicable: true, CitedIDs: []string{"REQ-3", "AC-3.1"}, UnknownIDs: []string{}, UnservedIDs: []string{}, Conflicts: []string{}},
		DoneCriteriaCoverage: &core.DoneCriteriaAssessment{Applicable: false, Summary: "Fixture has no execution plan.", Satisfied: []string{}, Unsatisfied: []string{}, Unverified: []string{}, Conflicts: []string{}},
		GovernanceAssessment: &core.GovernanceAssessment{DesignApplicable: &yes, DecisionCitable: &no, CitedIDs: []string{"feature-verification-kit-execution"}, UnknownIDs: []string{}, UngovernedIDs: []string{}, SupersededIDs: []string{}, Conflicts: []string{}}}
	if err := f.rpc("submit_review_verdict", review, nil); err != nil {
		actual, getErr := f.b.GetWorkOrder(f.ctx, f.order.ID)
		must(t, getErr)
		t.Fatalf("%v; context scope=%q base=%q head=%s; review scope=%q base=%q head=%s", err, vc.ReviewScope, vc.BaselineSHA, vc.Revisions[0].SHA, actual.ReviewScope, actual.BaselineSHA, actual.HeadSHA)
	}
	events, err := f.b.ListEvents(f.ctx, f.task.ID)
	must(t, err)
	found := false
	for _, e := range events {
		if strings.Contains(string(e.Payload), vc.ID) && strings.Contains(string(e.Payload), "verification_assessment") {
			found = true
		}
	}
	if !found {
		t.Fatal("review decision lost sealed evidence references")
	}
}

// Run executes the same scenario adapter against an isolated backend.
func Run(t *testing.T, factory func(*testing.T) store.Backend) {
	t.Helper()
	t.Run("Primary", func(t *testing.T) {
		f := newFixture(t, factory(t), "primary")
		f.claim(core.StageImplement)
		must(t, f.rpc("submit_for_review", map[string]string{"head_sha": f.head}, nil))
		f.claim(core.StageVerify)
		must(t, f.rpc("prepare_verification", workorder.VerificationPrepareRequest{RequestKey: "scenario"}, &f.snapshot))
		if len(f.snapshot.Contexts) != 1 || len(f.snapshot.Selections) != 1 || len(f.snapshot.Selections[0].Subjects) != 2 {
			t.Fatalf("kit discovery: %+v", f.snapshot)
		}
		coverage, inputs, cfg := f.prepareRunner()
		if err := f.rpc("submit_verification", workorder.VerificationSubmitRequest{ContextID: f.snapshot.Contexts[0].ID, Outcome: "succeeded", Coverage: f.coverage}, nil); err == nil {
			t.Fatal("review admitted before exercises")
		}
		c := f.runner(coverage, inputs, cfg, true)
		var output bytes.Buffer
		c.Stdout = &output
		c.Stderr = &output
		must(t, c.Start())
		done := false
		t.Cleanup(func() {
			if !done {
				_ = c.Process.Kill()
				_ = c.Wait()
				t.Logf("CLI output: %s", output.String())
			}
		})
		capture := f.browser()
		f.addCaptures(capture)
		f.mu.Lock()
		f.uiComplete = true
		f.mu.Unlock()
		err := c.Wait()
		done = true
		if err != nil {
			t.Fatalf("CLI: %v: %s", err, output.String())
		}
		must(t, f.rpc("submit_verification", workorder.VerificationSubmitRequest{ContextID: f.snapshot.Contexts[0].ID, Outcome: "succeeded", Coverage: f.coverage}, nil))
		f.publish()
		f.review()
	})
	for _, mode := range []string{"absent", "ineligible"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t, factory(t), mode)
			f.claim(core.StageImplement)
			must(t, f.rpc("submit_for_review", map[string]string{"head_sha": f.head}, nil))
			f.claim(core.StageVerify)
			must(t, f.rpc("prepare_verification", workorder.VerificationPrepareRequest{RequestKey: "scenario"}, &f.snapshot))
			if len(f.snapshot.Selections) != 1 || len(f.snapshot.Selections[0].Subjects) != 0 || f.snapshot.Selections[0].Receipt.Invalid() {
				t.Fatal("expected valid no-kit receipt")
			}
			if mode == "ineligible" {
				for _, kit := range f.snapshot.Selections[0].Receipt.Kits {
					if kit.Eligibility != "ineligible" || len(kit.Reasons) == 0 {
						t.Fatal("missing exclusion explanation")
					}
				}
			}
			coverage, inputs, cfg := f.prepareRunner()
			if err := f.rpc("submit_verification", workorder.VerificationSubmitRequest{ContextID: f.snapshot.Contexts[0].ID, Outcome: "succeeded", Coverage: f.coverage}, nil); err == nil {
				t.Fatal("no-kit disposition bypassed ordinary check")
			}
			out, err := f.runner(coverage, inputs, cfg, false).CombinedOutput()
			if err != nil {
				t.Fatalf("ordinary CLI: %v: %s", err, out)
			}
			f.refresh()
			if len(f.snapshot.Attempts) != 1 || f.snapshot.Attempts[0].Subject.Kind != "ordinary" || f.snapshot.Attempts[0].State != "succeeded" {
				t.Fatal("ordinary attempt missing")
			}
			must(t, f.rpc("submit_verification", workorder.VerificationSubmitRequest{ContextID: f.snapshot.Contexts[0].ID, Outcome: "succeeded", Coverage: f.coverage}, nil))
			f.review()
		})
	}
	t.Run("ToggleOff", func(t *testing.T) {
		f := newFixture(t, factory(t), "toggle-off")
		f.claim(core.StageImplement)
		must(t, f.rpc("submit_for_review", map[string]string{"head_sha": f.head}, nil))
		f.claim(core.StageReview)
		orders, err := f.b.ListTaskWorkOrders(f.ctx, f.task.ID)
		must(t, err)
		for _, o := range orders {
			if o.Stage == core.StageVerify {
				t.Fatal("toggle-off created verify order")
			}
		}
		var summary json.RawMessage
		must(t, f.rest("GET", "/v1/tasks/"+f.task.ID+"/verification?workspace_id="+f.ws, nil, &summary))
		if strings.Contains(string(summary), "selected_kits") {
			t.Fatal("toggle-off created verification result")
		}
	})
}
