package planning

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/gitx"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

func planningTestCredential(ctx context.Context, _ string) (context.Context, error) {
	return github.WithCredential(ctx, "fixture", "workspace demo GitHub App"), nil
}
func planningSnapshotManager(t *testing.T, cfg *config.Config) *gitx.Manager {
	t.Helper()
	directories := map[string]string{}
	for i := range cfg.Repos {
		repo := &cfg.Repos[i]
		directories[repo.Name] = strings.TrimPrefix(repo.URL, "file://")
		repo.URL = "https://github.com/planning/" + repo.Name
	}
	var mu sync.Mutex
	archives := map[string][]byte{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer fixture" {
			http.Error(w, "auth", 401)
			return
		}
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) < 4 {
			http.NotFound(w, r)
			return
		}
		directory := directories[parts[2]]
		if parts[3] == "tarball" {
			w.Write(archives[parts[4]])
			return
		}
		commit := map[string]any{"sha": r.URL.Query().Get("sha"), "commit": map[string]any{"message": "initial"}, "files": []any{map[string]any{"filename": "internal/eligibility.go", "status": "added", "additions": 3, "deletions": 0, "changes": 3}}}
		if len(parts) == 4 {
			json.NewEncoder(w).Encode([]any{commit})
			return
		}
		if parts[4] != "main" {
			commit["sha"] = parts[4]
			json.NewEncoder(w).Encode(commit)
			return
		}
		var data bytes.Buffer
		gz := gzip.NewWriter(&data)
		tw := tar.NewWriter(gz)
		// The archive takes a GitHub tarball's shape: a global header first, the
		// root directory, the files, and a symbolic link.
		for _, header := range []*tar.Header{
			{Name: "pax_global_header", Typeflag: tar.TypeXGlobalHeader, PAXRecords: map[string]string{"comment": parts[2]}},
			{Name: "root/", Typeflag: tar.TypeDir, Mode: 0755},
		} {
			if err := tw.WriteHeader(header); err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
		}
		err := filepath.WalkDir(directory, func(file string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				return nil
			}
			name, _ := filepath.Rel(directory, file)
			raw, err := os.ReadFile(file)
			if err != nil {
				return err
			}
			if err = tw.WriteHeader(&tar.Header{Name: "root/" + filepath.ToSlash(name), Mode: 0600, Size: int64(len(raw))}); err != nil {
				return err
			}
			_, err = tw.Write(raw)
			return err
		})
		if err == nil {
			err = tw.WriteHeader(&tar.Header{Name: "root/AGENTS.md", Typeflag: tar.TypeSymlink, Linkname: "CLAUDE.md", Mode: 0777})
		}
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		tw.Close()
		gz.Close()
		sum := sha1.Sum(data.Bytes())
		sha := hex.EncodeToString(sum[:])
		archives[sha] = data.Bytes()
		commit["sha"] = sha
		json.NewEncoder(w).Encode(commit)
	}))
	t.Cleanup(server.Close)
	manager := gitx.NewManager(github.NewSnapshotClient(server.Client(), server.URL), 0)
	manager.Root = filepath.Join(t.TempDir(), "snapshots")
	return manager
}

func TestSnapshotCleanupAfterFinalizeAndDaemonStartup(t *testing.T) {
	ctx, st, session := planningFixture(t, "session-260730-a1b2c3")
	local := createPlanningRepo(t, filepath.Join(t.TempDir(), "repo"), "README.md", "planning\n")
	cfg := &config.Config{Workspace: "demo", Repos: []config.Repo{{Name: "repo", URL: "file://" + local, Base: "main"}}}
	manager := planningSnapshotManager(t, cfg)
	credentialCtx, _ := planningTestCredential(ctx, "")
	pin, err := manager.PinSnapshot(credentialCtx, cfg.Repos[0].URL, "main")
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := manager.OpenSnapshot(credentialCtx, "demo", session.ID, cfg.Repos[0].URL, pin.Revision)
	if err != nil {
		t.Fatal(err)
	}
	args := requirementArgs{Title: "Retry policy", Prose: "# Retries remain bounded.", Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Retry attempts stop at the configured bound."}}}
	agent := &scriptedAgent{outputs: []string{decisionJSON(t, "", []toolCall{{ID: "call-final", Name: "finalize_requirement", ArgumentsJSON: jsonString(t, args)}})}}
	service := &Service{Store: st, Agent: agent, Git: manager, Model: "planner", Prompt: testPlanningPrompt}
	if err = service.Run(ctx, session.ID, UserMessage{Content: "Capture this requirement."}, func(map[string]any) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(snapshot.Repository); !os.IsNotExist(err) {
		t.Fatalf("finalized snapshot remains: %v", err)
	}
	restarted := gitx.NewManager(manager.API, 0)
	restarted.Root = manager.Root
	orphan, err := restarted.OpenSnapshot(credentialCtx, "demo", "absent-session", cfg.Repos[0].URL, pin.Revision)
	if err != nil {
		t.Fatal(err)
	}
	service.Git = restarted
	if err = service.CleanupSnapshots(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(orphan.Repository); !os.IsNotExist(err) {
		t.Fatalf("startup retained closed orphan: %v", err)
	}
}

type snapshotAppStore struct {
	store.Store
	credential core.WorkspaceGitHubAppCredential
}

func (s snapshotAppStore) GetWorkspaceGitHubAppForUse(context.Context, string) (core.WorkspaceGitHubAppCredential, error) {
	return s.credential, nil
}
func TestSnapshotWorkspaceCredentialPermissionAndCoverage(t *testing.T) {
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	missing := &Service{Store: store.NewMemory()}
	if _, err := missing.workspaceCredential(ctx, "https://github.com/owner/repo"); github.ErrorCategory(err) != github.ForgePermission || !strings.Contains(err.Error(), "owner/repo") || !strings.Contains(err.Error(), "workspace settings") {
		t.Fatalf("missing app: %v", err)
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	private := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	covered := false
	app := github.NewAppClient(nil, "")
	app.Runner = func(token, identity string) github.AppRunner {
		return func(_ context.Context, args ...string) ([]byte, error) {
			switch args[1] {
			case "/app/installations/1":
				return []byte(`{"id":1,"app_id":1,"account":{"login":"owner"},"permissions":{"contents":"write","issues":"write","pull_requests":"write","statuses":"write","metadata":"read"}}`), nil
			case "/app/installations/1/access_tokens":
				return json.Marshal(map[string]any{"token": "fixture-installation", "expires_at": time.Now().Add(59 * time.Minute)})
			case "/installation/repositories?per_page=100":
				if covered {
					return []byte(`[{"repositories":[{"full_name":"owner/repo"}]}]`), nil
				}
				return []byte(`[{"repositories":[]}]`), nil
			}
			return nil, fmt.Errorf("unexpected fixture request")
		}
	}
	service := &Service{Store: snapshotAppStore{Store: store.NewMemory(), credential: core.WorkspaceGitHubAppCredential{WorkspaceGitHubAppStatus: core.WorkspaceGitHubAppStatus{WorkspaceID: "demo", Connected: true, AppID: 1, InstallationID: 1}, PrivateKey: private}}, GitHubApps: app}
	if _, err = service.workspaceCredential(ctx, "https://github.com/owner/repo"); github.ErrorCategory(err) != github.ForgePermission || !strings.Contains(err.Error(), "owner/repo") || !strings.Contains(err.Error(), "workspace settings") {
		t.Fatalf("uncovered repo: %v", err)
	}
	covered = true
	if _, err = service.workspaceCredential(ctx, "https://github.com/owner/repo"); err != nil {
		t.Fatalf("covered repo: %v", err)
	}
}

// The exploration tools and history read a repository whose archive holds a
// global header first and a symbolic link, and the link stays unreadable
// (component-planning invariants 22 and 24).
func TestExplorationReadsArchiveWithGlobalHeaderAndSymlink(t *testing.T) {
	ctx, st, session := planningFixture(t, "session-github-archive")
	local := createPlanningRepo(t, filepath.Join(t.TempDir(), "repo"), "internal/eligibility.go", "package eligibility\n")
	cfg := &config.Config{Workspace: "demo", Repos: []config.Repo{{Name: "repo", URL: "file://" + local, Base: "main"}}}
	service := &Service{
		Store: st, Git: planningSnapshotManager(t, cfg), CredentialContext: planningTestCredential,
		ConfigProvider: func(context.Context) (*config.Config, error) { return cfg, nil },
	}
	for _, test := range []struct{ tool, args, want string }{
		{"list_files", `{"repo":"repo","path":"internal","depth":0}`, "internal/eligibility.go"},
		{"read_file", `{"repo":"repo","path":"internal/eligibility.go","offset":1,"limit":10}`, "package eligibility"},
		{"grep", `{"repo":"repo","pattern":"eligibility","context":0,"mode":"content"}`, "internal/eligibility.go"},
		{"history", `{"repo":"repo","path":"internal/eligibility.go","n":20}`, "Latest commit context"},
	} {
		result, err := service.explorationTool(ctx, session, toolCall{Name: test.tool, ArgumentsJSON: test.args})
		if err != nil {
			t.Fatalf("%s err=%v", test.tool, err)
		}
		if output, _ := result.Output.(string); !strings.Contains(output, test.want) {
			t.Fatalf("%s output=%q", test.tool, output)
		}
	}
	listed, err := service.explorationTool(ctx, session, toolCall{Name: "list_files", ArgumentsJSON: `{"repo":"repo","depth":0}`})
	if err != nil {
		t.Fatal(err)
	}
	if output, _ := listed.Output.(string); strings.Contains(output, "AGENTS.md") || strings.Contains(output, "pax_global_header") {
		t.Fatalf("skipped entry listed: %q", output)
	}
	if _, err = service.explorationTool(ctx, session, toolCall{
		Name: "read_file", ArgumentsJSON: `{"repo":"repo","path":"AGENTS.md","offset":1,"limit":10}`,
	}); err == nil {
		t.Fatal("read a skipped symbolic link")
	}
}

// With no connected App, exploration fails as a permission failure that names
// the repository and the remedy (component-planning invariant 20).
func TestExplorationWithoutWorkspaceAppNamesRepository(t *testing.T) {
	ctx, st, session := planningFixture(t, "session-no-app")
	cfg := &config.Config{Workspace: "demo", Repos: []config.Repo{{Name: "repo", URL: "https://github.com/planning/repo", Base: "main"}}}
	service := &Service{
		Store: st, Git: gitx.NewManager(github.NewSnapshotClient(nil, "http://127.0.0.1:1"), 0),
		ConfigProvider: func(context.Context) (*config.Config, error) { return cfg, nil },
	}
	service.Git.Root = filepath.Join(t.TempDir(), "snapshots")
	_, err := service.explorationTool(ctx, session, toolCall{Name: "list_files", ArgumentsJSON: `{"repo":"repo","depth":0}`})
	var infrastructure *planningInfrastructureError
	if !errors.As(err, &infrastructure) || github.ErrorCategory(err) != github.ForgePermission ||
		!strings.Contains(err.Error(), "planning/repo") || !strings.Contains(err.Error(), "workspace settings") {
		t.Fatalf("err=%v", err)
	}
}
