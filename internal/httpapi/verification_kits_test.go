package httpapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

// kitForgeRepo is one fixture repository: the commit its base branch names
// and, per commit, the manifest text and the kit roots present in the tree.
type kitForgeRepo struct {
	truncated bool
	head      string
	manifests map[string]string
	roots     map[string][]string
}

type kitForge struct {
	mu    sync.Mutex
	repos map[string]*kitForgeRepo
	trees atomic.Int64
	// kitsTrees counts tree reads for org/kits alone, whose results are cached.
	kitsTrees atomic.Int64
	server    *httptest.Server
}

func fixtureSHA(parts ...string) string {
	sum := sha1.Sum([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])
}

func newKitForge(t *testing.T, private string, repos map[string]*kitForgeRepo) *kitForge {
	f := &kitForge{repos: repos}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		p := r.URL.Path
		switch {
		case p == "/app/installations/12":
			fmt.Fprint(w, `{"id":12,"app_id":41,"account":{"login":"org"},"suspended_at":null}`)
			return
		case p == "/app/installations/12/access_tokens":
			_ = json.NewEncoder(w).Encode(map[string]any{"token": "kit-installation-token", "expires_at": time.Now().Add(time.Hour)})
			return
		case p == "/installation/repositories":
			names := []map[string]string{}
			for slug := range f.repos {
				names = append(names, map[string]string{"full_name": slug})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"repositories": names})
			return
		}
		rest, ok := strings.CutPrefix(p, "/repos/")
		if !ok {
			t.Errorf("unexpected forge path %s", p)
			w.WriteHeader(404)
			return
		}
		fields := strings.SplitN(rest, "/", 3)
		slug := fields[0] + "/" + fields[1]
		repo := f.repos[slug]
		tail := "/" + fields[2]
		switch {
		case repo == nil:
			w.WriteHeader(404)
		case strings.HasPrefix(tail, "/git/ref/heads/"):
			if strings.TrimPrefix(tail, "/git/ref/heads/") != "main" {
				w.WriteHeader(404)
				return
			}
			if repo.head == "" {
				w.WriteHeader(503)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"object": map[string]string{"sha": repo.head, "type": "commit"}})
		case strings.HasPrefix(tail, "/git/commits/"):
			sha := strings.TrimPrefix(tail, "/git/commits/")
			_ = json.NewEncoder(w).Encode(map[string]any{"sha": sha, "tree": map[string]string{"sha": fixtureSHA("tree", slug, sha)}})
		case strings.HasPrefix(tail, "/git/trees/"):
			f.trees.Add(1)
			if slug == "org/kits" {
				f.kitsTrees.Add(1)
			}
			tree := strings.TrimPrefix(tail, "/git/trees/")
			for sha, manifest := range repo.manifests {
				if fixtureSHA("tree", slug, sha) != tree {
					continue
				}
				entries := []map[string]string{}
				if manifest != "" {
					entries = append(entries, map[string]string{"path": ".conveyor/kits/manifest.yaml", "mode": "100644", "type": "blob", "sha": fixtureSHA("manifest", slug, sha)})
				}
				for _, root := range repo.roots[sha] {
					entries = append(entries,
						map[string]string{"path": root, "mode": "040000", "type": "tree", "sha": fixtureSHA("dir", root)},
						map[string]string{"path": root + "/check.sh", "mode": "100755", "type": "blob", "sha": fixtureSHA("script", root)})
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"sha": tree, "truncated": repo.truncated, "tree": entries})
				return
			}
			w.WriteHeader(404)
		case strings.HasPrefix(tail, "/git/blobs/"):
			blob := strings.TrimPrefix(tail, "/git/blobs/")
			for sha, manifest := range repo.manifests {
				if fixtureSHA("manifest", slug, sha) == blob {
					_ = json.NewEncoder(w).Encode(map[string]any{"encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte(manifest)), "size": len(manifest)})
					return
				}
			}
			w.WriteHeader(404)
		default:
			t.Errorf("unexpected forge path %s", p)
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(f.server.Close)
	return f
}

// kitManifest renders a schema-2 manifest; each kit is {id, pins YAML}.
func kitManifest(schema int, kits ...[2]string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "schema_version: %d\nkits:\n", schema)
	for _, k := range kits {
		fmt.Fprintf(&b, "  - id: %s\n    name: Kit %s\n", k[0], k[0])
		if schema == 2 {
			fmt.Fprintf(&b, "    description: Exercises the %s fixture.\n", k[0])
		}
		fmt.Fprintf(&b, "    version: \"1\"\n    path: .conveyor/kits/%s\n    governing_pins:\n%s    exercises:\n      - id: check\n", k[0], k[1])
		if schema == 2 {
			b.WriteString("        description: Runs the fixture check.\n")
		}
		b.WriteString("        stages: [verify]\n        kind: script\n        argv: [\"./check.sh\"]\n        cwd: \".\"\n        timeout_seconds: 30\n        prerequisites: []\n        permissions: []\n        inputs: []\n")
		if schema == 2 {
			b.WriteString("        required_assertions:\n          - id: observed\n            description: The fixture state is observed.\n")
		} else {
			b.WriteString("        required_assertions: [observed]\n")
		}
		b.WriteString("        retry_policy: safe_to_replay\n        safety_basis: read-only\n        operations: []\n        evidence_outputs: []\n        supports: []\n")
	}
	return b.String()
}

func pinsYAML(requirements, designs string) string {
	return "      requirements: [" + requirements + "]\n      system_designs: [" + designs + "]\n"
}

type kitRegistryGuardStore struct {
	store.Store
	t *testing.T
}

func (g kitRegistryGuardStore) AppendEvent(context.Context, core.Event) error {
	g.t.Error("kit registry read appended an event")
	return nil
}

// Corpus failures for the pin-resolution timeout and error paths: a failing
// requirement read, a failing requirement version list, a design read that
// blocks until the repository deadline, and a failing design version list.
var errKitCorpusUnavailable = errors.New("corpus store unavailable")

func (g kitRegistryGuardStore) GetRequirement(ctx context.Context, id string) (core.Requirement, error) {
	switch id {
	case "req-fail":
		return core.Requirement{}, errKitCorpusUnavailable
	case "req-versions-fail":
		return core.Requirement{ID: id, CurrentVersion: 1}, nil
	}
	return g.Store.GetRequirement(ctx, id)
}

func (g kitRegistryGuardStore) ListRequirementVersions(ctx context.Context, id string) ([]core.RequirementVersion, error) {
	if id == "req-versions-fail" {
		return nil, errKitCorpusUnavailable
	}
	return g.Store.ListRequirementVersions(ctx, id)
}

func (g kitRegistryGuardStore) GetSystemDesign(ctx context.Context, id string) (core.SystemDesign, error) {
	switch id {
	case "design-slow":
		<-ctx.Done()
		return core.SystemDesign{}, ctx.Err()
	case "design-versions-fail":
		return core.SystemDesign{ID: id, CurrentVersion: 1}, nil
	}
	return g.Store.GetSystemDesign(ctx, id)
}

func (g kitRegistryGuardStore) ListSystemDesignVersions(ctx context.Context, id string) ([]core.SystemDesignVersion, error) {
	if id == "design-versions-fail" {
		return nil, errKitCorpusUnavailable
	}
	return g.Store.ListSystemDesignVersions(ctx, id)
}

func TestWorkspaceVerificationKits(t *testing.T) {
	st := store.NewVolatileBackend()
	defer st.Close()
	ctx := store.WithWorkspace(t.Context(), "alpha")
	cfg := &config.Config{Workspace: "alpha", Repos: []config.Repo{
		{Name: "kits", GitHub: "org/kits", Base: "main"},
		{Name: "legacy", GitHub: "org/legacy", Base: "main"},
		{Name: "empty", GitHub: "org/empty", Base: "main"},
		{Name: "broken", GitHub: "org/broken", Base: "main"},
		{Name: "future", GitHub: "org/future", Base: "main"},
		{Name: "gone", GitHub: "org/gone", Base: "release"},
		{Name: "denied", GitHub: "org/denied", Base: "main"},
		{Name: "flaky", GitHub: "org/flaky", Base: "main"},
		{Name: "cut", GitHub: "org/cut", Base: "main"},
		{Name: "pin-fail", GitHub: "org/pin-fail", Base: "main"},
		{Name: "pin-versions", GitHub: "org/pin-versions", Base: "main"},
		{Name: "pin-slow", GitHub: "org/pin-slow", Base: "main"},
		{Name: "pin-design-versions", GitHub: "org/pin-design-versions", Base: "main"},
	}}
	if _, err := st.BootstrapWorkspaceConfig(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	// Corpus: req-a confirmed v1, v2 (current) and pending v3; req-dismissed
	// has a dismissed v2; design-a confirmed v1.
	requirement := func(id string) core.RequirementVersion {
		return core.RequirementVersion{Content: "# " + id + "\n\n```conveyor:requirements\n- id: REQ-1\n  statement: Keep the " + id + " fixture.\n```", Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Keep the " + id + " fixture."}}, Origin: core.RequirementOriginOperator}
	}
	for _, id := range []string{"req-a", "req-dismissed"} {
		if _, _, err := st.CreateRequirement(ctx, core.Requirement{ID: id, Title: id}, requirement(id)); err != nil {
			t.Fatal(err)
		}
		if _, _, err := st.ConfirmRequirementVersion(ctx, id, 1); err != nil {
			t.Fatal(err)
		}
		v := requirement(id)
		v.RequirementID = id
		if _, err := st.ProposeRequirementVersion(ctx, v); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := st.ConfirmRequirementVersion(ctx, "req-a", 2); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.DismissRequirementVersion(ctx, "req-dismissed", 2); err != nil {
		t.Fatal(err)
	}
	pending := requirement("req-a")
	pending.RequirementID = "req-a"
	pending.Content = strings.Replace(pending.Content, "Keep", "Retain", 1)
	pending.Statements[0].Statement = strings.Replace(pending.Statements[0].Statement, "Keep", "Retain", 1)
	if _, err := st.ProposeRequirementVersion(ctx, pending); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.CreateSystemDesign(ctx, core.SystemDesign{ID: "design-a", Title: "Design A", Category: "Architecture"}, core.SystemDesignVersion{Content: "# Design A\n\n```conveyor:governs\n- repo: kits\n  paths:\n    - internal/**\n```", Origin: core.SystemDesignOriginOperator}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.ConfirmSystemDesignVersion(ctx, "design-a", 1); err != nil {
		t.Fatal(err)
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	private := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))

	headA, headB := fixtureSHA("kits", "a"), fixtureSHA("kits", "b")
	kitsV2 := kitManifest(2,
		[2]string{"k-current", pinsYAML("{document_id: req-a, version: 2}", "{document_id: design-a, version: 1}")},
		[2]string{"k-behind", pinsYAML("{document_id: req-a, version: 1}", "")},
		[2]string{"k-pending", pinsYAML("{document_id: req-a, version: 3}", "")},
		[2]string{"k-missing-doc", pinsYAML("{document_id: req-missing, version: 1}", "")},
		[2]string{"k-dismissed", pinsYAML("{document_id: req-dismissed, version: 2}", "")},
		[2]string{"k-precedence", pinsYAML("{document_id: req-a, version: 1}, {document_id: req-missing, version: 1}", "")},
		[2]string{"k-unpinned", pinsYAML("", "")},
		[2]string{"k-no-root", pinsYAML("{document_id: req-a, version: 2}", "")},
	)
	roots := []string{}
	for _, id := range []string{"k-current", "k-behind", "k-pending", "k-missing-doc", "k-dismissed", "k-precedence", "k-unpinned"} {
		roots = append(roots, ".conveyor/kits/"+id)
	}
	pinRepo := func(name, pins string) *kitForgeRepo {
		head := fixtureSHA(name)
		return &kitForgeRepo{head: head, manifests: map[string]string{head: kitManifest(2, [2]string{"k-" + name, pins})}, roots: map[string][]string{head: {".conveyor/kits/k-" + name}}}
	}
	// A short budget keeps the deadline-bound design read fast.
	previousLimit := kitRegistryRepositoryLimit
	kitRegistryRepositoryLimit = 2 * time.Second
	t.Cleanup(func() { kitRegistryRepositoryLimit = previousLimit })
	legacyHead, emptyHead, brokenHead, futureHead := fixtureSHA("legacy"), fixtureSHA("empty"), fixtureSHA("broken"), fixtureSHA("future")
	forge := newKitForge(t, private, map[string]*kitForgeRepo{
		"org/kits":                {head: headA, manifests: map[string]string{headA: kitsV2, headB: strings.Replace(kitsV2, "id: k-current\n    name: Kit k-current", "id: k-current\n    name: Kit k-current advanced", 1)}, roots: map[string][]string{headA: roots, headB: roots}},
		"org/legacy":              {head: legacyHead, manifests: map[string]string{legacyHead: kitManifest(1, [2]string{"k-legacy", pinsYAML("{document_id: req-a, version: 2}", "")})}, roots: map[string][]string{legacyHead: {".conveyor/kits/k-legacy"}}},
		"org/empty":               {head: emptyHead, manifests: map[string]string{emptyHead: ""}, roots: map[string][]string{}},
		"org/broken":              {head: brokenHead, manifests: map[string]string{brokenHead: "schema_version: [broken"}, roots: map[string][]string{}},
		"org/gone":                {head: fixtureSHA("gone"), manifests: map[string]string{}, roots: map[string][]string{}},
		"org/flaky":               {manifests: map[string]string{}, roots: map[string][]string{}},
		"org/cut":                 {truncated: true, head: fixtureSHA("cut"), manifests: map[string]string{fixtureSHA("cut"): kitManifest(2, [2]string{"k-cut", pinsYAML("", "")})}, roots: map[string][]string{fixtureSHA("cut"): {".conveyor/kits/k-cut"}}},
		"org/pin-fail":            pinRepo("pin-fail", pinsYAML("{document_id: req-fail, version: 1}", "")),
		"org/pin-versions":        pinRepo("pin-versions", pinsYAML("{document_id: req-versions-fail, version: 1}", "")),
		"org/pin-slow":            pinRepo("pin-slow", pinsYAML("", "{document_id: design-slow, version: 1}")),
		"org/pin-design-versions": pinRepo("pin-design-versions", pinsYAML("", "{document_id: design-versions-fail, version: 1}")),
		"org/future":              {head: futureHead, manifests: map[string]string{futureHead: strings.Replace(kitManifest(2, [2]string{"k-future", pinsYAML("{document_id: req-a, version: 2}", "")}), "schema_version: 2", "schema_version: 3", 1)}, roots: map[string][]string{futureHead: {".conveyor/kits/k-future"}}},
	})

	memberships := &membershipFixture{workspaces: []core.Workspace{{ID: "alpha", Name: "Alpha"}}, roles: map[string]map[string]core.WorkspaceRole{"operator": {"alpha": core.WorkspaceRoleOperator}, "maintainer": {"alpha": core.WorkspaceRoleMaintainer}}}
	newServer := func() *Server {
		server := NewServer(st)
		server.Store = kitRegistryGuardStore{Store: st, t: t}
		server.Workspaces = memberships
		server.Memberships = memberships
		server.ConfigProvider = func(context.Context) (*config.Config, error) { return cfg, nil }
		server.Credentials = staticCredentialVerifier{"operator": {ID: "pat", OwnerUserID: "operator", Kind: core.CredentialUser, Scope: core.CredentialScopeOperator}, "maintainer": {ID: "pat2", OwnerUserID: "maintainer", Kind: core.CredentialUser, Scope: core.CredentialScopeUser}, "stranger": {ID: "pat3", OwnerUserID: "stranger", Kind: core.CredentialUser, Scope: core.CredentialScopeUser}}
		server.GitHubApps = github.NewAppClient(forge.server.Client(), forge.server.URL)
		return server
	}
	server := newServer()
	call := func(s *Server, bearer string) (*httptest.ResponseRecorder, kitRegistryResponse) {
		t.Helper()
		r := httptest.NewRequest(http.MethodGet, "https://conveyor.example/v1/workspaces/alpha/verification-kits", nil)
		r.Header.Set("Authorization", "Bearer "+bearer)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		var body kitRegistryResponse
		if w.Code == http.StatusOK {
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode: %v %s", err, w.Body.String())
			}
		}
		if strings.Contains(w.Body.String(), "kit-installation-token") {
			t.Fatal("response leaked the installation token")
		}
		return w, body
	}

	t.Run("NoApp", func(t *testing.T) {
		before := forge.trees.Load()
		w, body := call(server, "maintainer")
		if w.Code != 200 || len(body.Repositories) != len(cfg.Repos) {
			t.Fatalf("no-app response %d %s", w.Code, w.Body.String())
		}
		for i, repo := range body.Repositories {
			if repo.Repository != cfg.Repos[i].Name || repo.State != "unavailable" || repo.Reason != "no_app" || repo.CommitSHA != "" || len(repo.Kits) != 0 {
				t.Fatalf("repository %d: %+v", i, repo)
			}
		}
		if forge.trees.Load() != before {
			t.Fatal("no-app read contacted the forge")
		}
	})

	st.ConfigureGitHubAppKeyEncryptionKey(bytes.Repeat([]byte{41}, 32))
	if _, err := st.StoreWorkspaceGitHubApp(ctx, "alpha", core.WorkspaceGitHubAppCredential{WorkspaceGitHubAppStatus: core.WorkspaceGitHubAppStatus{AppID: 41, AppSlug: "conveyor-alpha", ClientID: "client"}, PrivateKey: private}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RecordWorkspaceGitHubAppInstallation(ctx, "alpha", 41, 12, "org"); err != nil {
		t.Fatal(err)
	}

	t.Run("Refusal", func(t *testing.T) {
		for _, bearer := range []string{"stranger", ""} {
			w, _ := call(server, bearer)
			if w.Code == http.StatusOK || strings.Contains(w.Body.String(), "k-current") || strings.Contains(w.Body.String(), "check.sh") {
				t.Fatalf("%q: %d %s", bearer, w.Code, w.Body.String())
			}
		}
	})

	w, body := call(server, "maintainer")
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" || len(body.Repositories) != len(cfg.Repos) {
		t.Fatalf("registry response %d %s", w.Code, w.Body.String())
	}
	byName := map[string]kitRegistryRepository{}
	for i, repo := range body.Repositories {
		if repo.Repository != cfg.Repos[i].Name || repo.Base != cfg.Repos[i].Base {
			t.Fatalf("configuration order lost at %d: %+v", i, repo)
		}
		byName[repo.Repository] = repo
	}

	t.Run("RepositoryStates", func(t *testing.T) {
		for name, want := range map[string][2]string{
			"kits": {"invalid", ""}, "legacy": {"ok", ""}, "empty": {"no_manifest", ""}, "broken": {"invalid", ""}, "future": {"invalid", ""},
			"gone": {"unavailable", "unknown_revision"}, "denied": {"unavailable", "permission"}, "flaky": {"unavailable", "transport"}, "cut": {"unavailable", "truncated"},
			"pin-fail": {"unavailable", "transport"}, "pin-versions": {"unavailable", "transport"},
			"pin-slow": {"unavailable", "transport"}, "pin-design-versions": {"unavailable", "transport"},
		} {
			repo := byName[name]
			if repo.State != want[0] || repo.Reason != want[1] {
				t.Fatalf("%s: state %q reason %q diagnostics %+v", name, repo.State, repo.Reason, repo.Diagnostics)
			}
		}
		if byName["kits"].CommitSHA != headA || byName["gone"].CommitSHA != "" || byName["denied"].CommitSHA != "" {
			t.Fatal("commit SHA not reported exactly")
		}
		// A corpus read failure or an expired budget during pin resolution
		// reports the resolved commit and no kits, never derived statuses.
		for _, name := range []string{"pin-fail", "pin-versions", "pin-slow", "pin-design-versions"} {
			repo := byName[name]
			if repo.CommitSHA != fixtureSHA(name) || len(repo.Kits) != 0 {
				t.Fatalf("%s: %+v", name, repo)
			}
		}
		// k-no-root makes discovery malformed; only that kit is invalid and
		// the repository carries no manifest-level diagnostic.
		if len(byName["kits"].Diagnostics) != 0 {
			t.Fatalf("entry diagnostic leaked to the repository: %+v", byName["kits"].Diagnostics)
		}
		if len(byName["broken"].Diagnostics) == 0 || len(byName["empty"].Kits) != 0 {
			t.Fatalf("broken/empty projection: %+v %+v", byName["broken"], byName["empty"])
		}
		// A manifest-level diagnostic invalidates the repository and every
		// recoverable kit, which stays listed (AC-11.4).
		future := byName["future"]
		if len(future.Diagnostics) == 0 || future.Diagnostics[0].Path != "manifest.schema_version" || len(future.Kits) != 1 || future.Kits[0].Status != "invalid" {
			t.Fatalf("future projection: %+v", future)
		}
	})

	t.Run("KitStatus", func(t *testing.T) {
		kits := map[string]kitRegistryKit{}
		for _, k := range byName["kits"].Kits {
			kits[k.ID] = k
		}
		for id, want := range map[string]string{
			"k-current": "current", "k-behind": "behind", "k-pending": "pending", "k-missing-doc": "unresolved",
			"k-dismissed": "unresolved", "k-precedence": "unresolved", "k-unpinned": "unpinned", "k-no-root": "invalid",
		} {
			if kits[id].Status != want {
				t.Fatalf("%s status %q want %q (%+v)", id, kits[id].Status, want, kits[id])
			}
		}
		behind := kits["k-behind"].Pins[0]
		if behind.Status != "behind" || behind.CurrentVersion != 2 {
			t.Fatalf("behind pin: %+v", behind)
		}
		if p := kits["k-current"].Pins; len(p) != 2 || p[0].Status != "current" || p[1].Kind != "system_design" || p[1].Status != "current" {
			t.Fatalf("current pins: %+v", p)
		}
		if kits["k-pending"].Pins[0].Status != "pending" || kits["k-dismissed"].Pins[0].Status != "unresolved" {
			t.Fatal("pending or dismissed pin status wrong")
		}
		noRoot := kits["k-no-root"]
		if noRoot.Digest != "" || len(noRoot.Diagnostics) == 0 {
			t.Fatalf("invalid kit projection: %+v", noRoot)
		}
		current := kits["k-current"]
		if len(current.Digest) != 64 || current.Description != "Exercises the k-current fixture." || len(current.Stages) != 1 || current.Stages[0] != "verify" {
			t.Fatalf("current kit: %+v", current)
		}
		ex := current.Exercises[0]
		if ex.Description != "Runs the fixture check." || len(ex.RequiredAssertions) != 1 || ex.RequiredAssertions[0] != (kitRegistryAssertion{ID: "observed", Description: "The fixture state is observed."}) {
			t.Fatalf("exercise projection: %+v", ex)
		}
		if strings.Contains(w.Body.String(), `"ui"`) {
			t.Fatal("ui launch fields were returned")
		}
	})

	t.Run("Schema1", func(t *testing.T) {
		kit := byName["legacy"].Kits[0]
		if byName["legacy"].SchemaVersion != 1 || kit.Status != "current" || kit.Description != "" || kit.Exercises[0].Description != "" || kit.Exercises[0].RequiredAssertions[0] != (kitRegistryAssertion{ID: "observed"}) {
			t.Fatalf("schema-1 kit: %+v", kit)
		}
		if !strings.Contains(w.Body.String(), `"required_assertions":[{"id":"observed","description":""}]`) {
			t.Fatal("schema-1 assertions not returned as objects with empty descriptions")
		}
	})

	t.Run("BaseAdvance", func(t *testing.T) {
		trees := forge.kitsTrees.Load()
		if _, again := call(server, "maintainer"); again.Repositories[0].CommitSHA != headA {
			t.Fatal("repeat read changed head")
		}
		if forge.kitsTrees.Load() != trees {
			t.Fatalf("memoized commits re-read the forge tree: %d -> %d", trees, forge.kitsTrees.Load())
		}
		forge.mu.Lock()
		forge.repos["org/kits"].head = headB
		forge.mu.Unlock()
		_, advanced := call(server, "maintainer")
		if advanced.Repositories[0].CommitSHA != headB || advanced.Repositories[0].Kits[0].Name != "Kit k-current advanced" {
			t.Fatalf("base advance not read: %+v", advanced.Repositories[0])
		}
		if server.verificationKitMemo.size() != 10 {
			t.Fatalf("memo holds %d entries", server.verificationKitMemo.size())
		}
	})

	t.Run("StatusTracksCorpus", func(t *testing.T) {
		if _, _, err := st.ConfirmRequirementVersion(ctx, "req-a", 3); err != nil {
			t.Fatal(err)
		}
		_, after := call(server, "maintainer")
		statuses := map[string]string{}
		for _, k := range after.Repositories[0].Kits {
			statuses[k.ID] = k.Status
		}
		if statuses["k-pending"] != "current" || statuses["k-current"] != "behind" {
			t.Fatalf("pin status did not follow confirmation: %+v", statuses)
		}
	})
}

func TestKitRegistryMemoBound(t *testing.T) {
	var m kitRegistryMemo
	for i := 0; i < kitRegistryMemoSize+10; i++ {
		m.put(kitRegistryKey{workspace: "w", repository: "org/r", sha: fmt.Sprint(i)}, github.VerificationDiscovery{State: "present"})
	}
	if m.size() != kitRegistryMemoSize {
		t.Fatalf("memo size %d", m.size())
	}
	if _, ok := m.get(kitRegistryKey{workspace: "w", repository: "org/r", sha: "0"}); ok {
		t.Fatal("oldest entry was not evicted")
	}
	if _, ok := m.get(kitRegistryKey{workspace: "other", repository: "org/r", sha: fmt.Sprint(kitRegistryMemoSize + 9)}); ok {
		t.Fatal("memo shared across workspaces")
	}
}
