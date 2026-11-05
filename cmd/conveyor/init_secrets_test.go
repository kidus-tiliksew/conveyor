package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"gopkg.in/yaml.v3"
)

// Fixture values are fake and keep the real shapes. A failure message names
// which value appeared and never repeats the text that held it.
const (
	initFixturePassword = "fixture-db-pw-5e1c"
	initFixtureUser     = "fixture-db-user"
	initFixtureHost     = "fixture-db-host.invalid"
	initFixtureDatabase = "fixture_db_name"
	initFixtureURL      = "postgres://" + initFixtureUser + ":" + initFixturePassword + "@" + initFixtureHost + "/" + initFixtureDatabase
	initFixtureToken    = "fixture-operator-token-9b2"

	initProcessHelperEnv     = "CONVEYOR_INIT_PROCESS_HELPER"
	initProcessHelperArgsEnv = "CONVEYOR_INIT_PROCESS_HELPER_ARGS"
)

func initFixtureAnswers() initAnswers {
	return initAnswers{
		Organization: "Fixture Org", OperatorName: "Fixture Operator", OperatorEmail: "owner@example.test",
		WorkspaceID: "demo", WorkspaceName: "Demo", RepositoryName: "app",
		RepositoryURL: "https://github.com/example/app", BaseBranch: "main",
	}
}

// assertNoInitSecret fails when text holds the database password, the URL, or
// any URL part, or the operator token. It reports the label and the part only.
func assertNoInitSecret(t *testing.T, label, text string) {
	t.Helper()
	for part, value := range map[string]string{
		"password": initFixturePassword, "URL": initFixtureURL, "user": initFixtureUser,
		"host": initFixtureHost, "database name": initFixtureDatabase, "operator token": initFixtureToken,
	} {
		if strings.Contains(text, value) {
			t.Errorf("%s holds the %s", label, part)
		}
	}
}

// assertDatabaseNodeIsBackendOnly requires the file's database node to name the
// backend and nothing else. It reports key counts, never values.
func assertDatabaseNodeIsBackendOnly(t *testing.T, data []byte, backend string) {
	t.Helper()
	var document struct {
		Database map[string]any `yaml:"database"`
	}
	if err := yaml.Unmarshal(data, &document); err != nil {
		t.Fatal("generated file is not YAML")
	}
	if _, holdsURL := document.Database["url"]; holdsURL {
		t.Fatal("generated file has a database.url key")
	}
	if len(document.Database) != 1 || document.Database["backend"] != backend {
		t.Fatalf("database node has %d keys, want only backend %q", len(document.Database), backend)
	}
}

// initFixtureStore shares one volatile store across init runs, and counts the
// sign-in links issued. Each hook replaces one store method with a failure.
type initFixtureStore struct {
	store.Backend
	linksIssued      int
	failBootstrap    error
	failVerify       error
	failCreate       error
	failGetWorkspace error
	failIssue        error
}

func (s *initFixtureStore) Close() {}

func (s *initFixtureStore) BootstrapIdentity(ctx context.Context, identity config.FirstOperatorIdentity, token string) (bool, error) {
	if s.failBootstrap != nil {
		return false, s.failBootstrap
	}
	return s.Backend.BootstrapIdentity(ctx, identity, token)
}

func (s *initFixtureStore) VerifyPersonalAccessToken(ctx context.Context, token string) (core.IdentityUser, error) {
	if s.failVerify != nil {
		return core.IdentityUser{}, s.failVerify
	}
	return s.Backend.VerifyPersonalAccessToken(ctx, token)
}

func (s *initFixtureStore) CreateWorkspace(ctx context.Context, id, name string, cfg *config.Config) (core.Workspace, error) {
	if s.failCreate != nil {
		return core.Workspace{}, s.failCreate
	}
	return s.Backend.CreateWorkspace(ctx, id, name, cfg)
}

func (s *initFixtureStore) GetWorkspace(ctx context.Context, id string) (core.Workspace, error) {
	if s.failGetWorkspace != nil {
		return core.Workspace{}, s.failGetWorkspace
	}
	return s.Backend.GetWorkspace(ctx, id)
}

func (s *initFixtureStore) IssueSignInLink(ctx context.Context, email string) (core.IssuedSignInLink, error) {
	if s.failIssue != nil {
		return core.IssuedSignInLink{}, s.failIssue
	}
	s.linksIssued++
	return s.Backend.IssueSignInLink(ctx, email)
}

// useInitFixtureStore sets the environment init requires and opens the volatile
// store in place of the backend the URL selects. observe, when set, runs at the
// open, the moment a new configuration is staged on disk and not yet published.
func useInitFixtureStore(t *testing.T, observe func()) *initFixtureStore {
	t.Helper()
	t.Setenv("CONVEYOR_DATABASE_URL", initFixtureURL)
	t.Setenv("CONVEYOR_API_TOKEN", initFixtureToken)
	t.Setenv(config.LLMAPIKeyEnv, "fixture-llm-key")
	t.Setenv(config.PublicURLEnv, "https://conveyor.example/")
	fixture := &initFixtureStore{Backend: store.NewVolatileBackend()}
	previous := initOpenStore
	initOpenStore = func(context.Context, config.Database) (store.Backend, error) {
		if observe != nil {
			observe()
		}
		return fixture, nil
	}
	t.Cleanup(func() { initOpenStore = previous })
	return fixture
}

// captureInitLog returns the standard logger's output, where init writes its
// warnings, so a test can check that stream too.
func captureInitLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buffer bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&buffer)
	t.Cleanup(func() { log.SetOutput(previous) })
	return &buffer
}

// assertOnlyDirectoryEntries proves a run left no staged temporary file.
func assertOnlyDirectoryEntries(t *testing.T, directory string, want ...string) {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("directory holds %v, want %v", names, want)
	}
}

// The success path: the staged file, the published file, the "Next" output, and
// the stored workspace document hold no secret, and a rerun with the same or a
// padded environment value passes and changes nothing (req-security-boundaries
// REQ-2; component-identity-membership invariant 14).
func TestInitGeneratedConfigHoldsNoDatabaseURL(t *testing.T) {
	for _, tc := range []struct{ name, raw, backend string }{
		{"postgres URL", initFixtureURL, "postgres"},
		{"postgres keyword string", "host=" + initFixtureHost + " user=" + initFixtureUser + " password=" + initFixturePassword + " dbname=" + initFixtureDatabase, "postgres"},
		{"singlestore URL", "singlestore://" + initFixtureUser + ":" + initFixturePassword + "@" + initFixtureHost + "/" + initFixtureDatabase, "singlestore"},
		{"mysql URL", "mysql://" + initFixtureUser + ":" + initFixturePassword + "@" + initFixtureHost + "/" + initFixtureDatabase, "singlestore"},
		{"mysql DSN", initFixtureUser + ":" + initFixturePassword + "@tcp(" + initFixtureHost + ":3306)/" + initFixtureDatabase, "singlestore"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			directory := t.TempDir()
			path := filepath.Join(directory, "conveyor.yaml")
			stagedFiles := 0
			fixture := useInitFixtureStore(t, func() {
				entries, err := os.ReadDir(directory)
				if err != nil {
					t.Error(err)
					return
				}
				for _, entry := range entries {
					if !strings.HasPrefix(entry.Name(), ".conveyor-init-") {
						continue
					}
					stagedFiles++
					staged, readErr := os.ReadFile(filepath.Join(directory, entry.Name()))
					if readErr != nil {
						t.Error(readErr)
						return
					}
					assertNoInitSecret(t, "staged temporary file", string(staged))
					assertDatabaseNodeIsBackendOnly(t, staged, tc.backend)
					info, statErr := entry.Info()
					if statErr != nil || info.Mode().Perm() != 0o600 {
						t.Errorf("staged file mode=%v err=%v, want 0600", info.Mode().Perm(), statErr)
					}
				}
			})
			t.Setenv("CONVEYOR_DATABASE_URL", tc.raw)
			logged := captureInitLog(t)
			var output strings.Builder
			if err := initializeDeployment(t.Context(), &output, path, initFixtureAnswers()); err != nil {
				t.Fatalf("init failed: %v", err)
			}
			if stagedFiles != 1 {
				t.Fatalf("observed %d staged files, want 1", stagedFiles)
			}
			published, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			assertNoInitSecret(t, "published file", string(published))
			assertDatabaseNodeIsBackendOnly(t, published, tc.backend)
			assertOnlyDirectoryEntries(t, directory, "conveyor.yaml")
			if info, statErr := os.Stat(path); statErr != nil || info.Mode().Perm() != 0o600 {
				t.Fatalf("published file stat err=%v", statErr)
			}
			first := output.String()
			assertNoInitSecret(t, "init output", first)
			assertNoInitSecret(t, "init log", logged.String())
			for _, required := range []string{"Next: run `conveyord install --config " + path + "`", "CONVEYOR_DATABASE_URL", filepath.Join(directory, ".env"), "--env-file", "CONVEYOR_ENV_FILE"} {
				if !strings.Contains(first, required) {
					t.Errorf("init output lacks %q", required)
				}
			}
			if strings.Contains(first, "Notice:") {
				t.Error("a generated file drew the legacy URL notice")
			}
			if signInTokenFromOutput(t, first) == "" || fixture.linksIssued != 1 {
				t.Fatalf("first run issued %d links", fixture.linksIssued)
			}
			// conveyord loads this file: the loader takes the URL from the
			// environment and keeps the backend the file names.
			loaded, err := config.LoadDeployment(path, nil)
			if err != nil {
				t.Fatal("generated file does not load")
			}
			if loaded.Database.Backend != tc.backend || loaded.Database.URL != tc.raw {
				t.Fatalf("loaded backend=%q restored URL=%t", loaded.Database.Backend, loaded.Database.URL == tc.raw)
			}
			// The stored workspace document holds no secret either.
			document, err := fixture.WorkspaceConfig(store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo"))
			if err != nil {
				t.Fatalf("read stored workspace document: %v", err)
			}
			stored, err := yaml.Marshal(document)
			if err != nil {
				t.Fatal(err)
			}
			assertNoInitSecret(t, "stored workspace document", string(stored))

			for _, rerun := range []struct{ name, value string }{{"same value", tc.raw}, {"padded value", "  " + tc.raw + " \n"}} {
				t.Setenv("CONVEYOR_DATABASE_URL", rerun.value)
				output.Reset()
				if err = initializeDeployment(t.Context(), &output, path, initFixtureAnswers()); err != nil {
					t.Fatalf("%s rerun failed: %v", rerun.name, err)
				}
				assertNoInitSecret(t, rerun.name+" rerun output", output.String())
				assertNoInitSecret(t, rerun.name+" rerun log", logged.String())
				if !strings.Contains(output.String(), "already initialized; issuing a fresh") || signInTokenFromOutput(t, output.String()) == "" {
					t.Fatalf("%s rerun did not report the completed deployment and a fresh link", rerun.name)
				}
				if after, readErr := os.ReadFile(path); readErr != nil || !bytes.Equal(after, published) {
					t.Fatalf("%s rerun changed the file (read err %v)", rerun.name, readErr)
				}
			}
		})
	}
}

// A file an earlier init wrote still reruns, stays byte-equal, and draws a notice
// that says it holds a URL and prints no part of it.
func TestInitRerunOverLegacyURLFile(t *testing.T) {
	answers := initFixtureAnswers()
	base := legacyExecutorInitConfig(t, initFixtureURL, answers)
	withoutBackend := bytes.Replace(base, []byte("backend: postgres\n"), nil, 1)
	if bytes.Equal(base, withoutBackend) {
		t.Fatal("fixture has no explicit backend to remove")
	}
	for _, tc := range []struct {
		name  string
		file  []byte
		value string
	}{
		{"same URL", base, initFixtureURL},
		{"padded environment value", base, "\t" + initFixtureURL + "  \n"},
		{"backend inferred from the file's URL", withoutBackend, initFixtureURL},
	} {
		t.Run(tc.name, func(t *testing.T) {
			directory := t.TempDir()
			path := filepath.Join(directory, "conveyor.yaml")
			if err := os.WriteFile(path, tc.file, 0o600); err != nil {
				t.Fatal(err)
			}
			fixture := useInitFixtureStore(t, nil)
			t.Setenv("CONVEYOR_DATABASE_URL", tc.value)
			logged := captureInitLog(t)
			var output strings.Builder
			if err := initializeDeployment(t.Context(), &output, path, answers); err != nil {
				t.Fatalf("legacy rerun failed: %v", err)
			}
			text := output.String()
			assertNoInitSecret(t, "legacy rerun output", text)
			assertNoInitSecret(t, "legacy rerun log", logged.String())
			if !strings.Contains(text, "Notice: "+path+" holds a database URL in database.url") {
				t.Error("legacy rerun printed no notice that the file holds a URL")
			}
			if !strings.Contains(text, "Next: run `conveyord install --config "+path+"`.") {
				t.Error("legacy rerun printed no next step")
			}
			if strings.Contains(text, "The config holds no database URL") {
				t.Error("legacy rerun claims the file holds no URL")
			}
			if signInTokenFromOutput(t, text) == "" || fixture.linksIssued != 1 {
				t.Fatalf("legacy rerun issued %d links", fixture.linksIssued)
			}
			if after, err := os.ReadFile(path); err != nil || !bytes.Equal(after, tc.file) {
				t.Fatalf("legacy file was rewritten (read err %v)", err)
			}
			assertOnlyDirectoryEntries(t, directory, "conveyor.yaml")
		})
	}
}

// Every refusal after the environment checks, with an existing file or a
// failing store: nonzero result, no secret in the error, output, or log, no
// staged file left behind, and an existing file unchanged.
func TestInitRefusalsHoldNoSecret(t *testing.T) {
	answers := initFixtureAnswers()
	legacy := func(databaseURL string, mutate func(*initAnswers)) []byte {
		fileAnswers := answers
		if mutate != nil {
			mutate(&fileAnswers)
		}
		return legacyExecutorInitConfig(t, databaseURL, fileAnswers)
	}
	const otherPassword = "fixture-other-pw-3f8a"
	otherURL := "postgres://" + initFixtureUser + ":" + otherPassword + "@" + initFixtureHost + "/" + initFixtureDatabase
	singleStoreURL := "singlestore://" + initFixtureUser + ":" + otherPassword + "@" + initFixtureHost + "/" + initFixtureDatabase
	// A driver error that repeats the connection string, as an unmasked driver would.
	driverText := func(kind string) error {
		return fmt.Errorf("%s: cannot reach `%s` with password %s", kind, initFixtureURL, initFixturePassword)
	}
	generatedForOtherBackend, err := defaultInitConfig(singleStoreURL, answers)
	if err != nil {
		t.Fatal(err)
	}
	urlLessOtherBackend, err := config.MarshalDeployment(&generatedForOtherBackend)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		existing []byte
		prepare  func(*initFixtureStore)
		want     string
		// published names the entries the directory must hold afterwards.
		published []string
		linksMade int
	}{
		{name: "file states a different URL", existing: legacy(otherURL, nil), want: "its database.url differs from CONVEYOR_DATABASE_URL"},
		{name: "file with no URL names a different backend", existing: urlLessOtherBackend, want: "does not match these initialization answers; refusing"},
		{name: "file with another URL names a different backend", existing: legacy(singleStoreURL, nil), want: "does not match these initialization answers; refusing"},
		{name: "file names a different workspace", existing: legacy(initFixtureURL, func(a *initAnswers) { a.WorkspaceID = "other" }), want: "does not match these initialization answers"},
		{name: "file names a different repository", existing: legacy(initFixtureURL, func(a *initAnswers) { a.RepositoryName = "other" }), want: "does not match these initialization answers"},
		{name: "file with a malformed pool size", existing: []byte("database: {url: \"" + initFixtureURL + "\", max_conns: \"" + initFixtureURL + "\"}\n"), want: "load existing deployment config"},
		{name: "file with an unknown backend", existing: []byte("database: {backend: bogus, url: \"" + initFixtureURL + "\"}\n"), want: "load existing deployment config"},
		{name: "file that is not YAML", existing: []byte("database: {url: \"" + initFixtureURL + "\"\n"), want: "load existing deployment config"},
		{name: "bootstrap failure", prepare: func(s *initFixtureStore) { s.failBootstrap = driverText("bootstrap") }, want: "bootstrap"},
		{name: "operator resolution failure", prepare: func(s *initFixtureStore) { s.failVerify = driverText("verify") }, want: "resolve first operator"},
		{name: "workspace creation failure", prepare: func(s *initFixtureStore) { s.failCreate = driverText("create") }, want: "create first workspace"},
		{name: "workspace read failure after a conflict", prepare: func(s *initFixtureStore) {
			s.failCreate = store.ErrWorkspaceConflict
			s.failGetWorkspace = driverText("read")
		}, want: "inspect existing workspace"},
		{name: "sign-in link failure", prepare: func(s *initFixtureStore) { s.failIssue = driverText("issue") }, want: "safe to retry", published: []string{"conveyor.yaml"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			directory := t.TempDir()
			path := filepath.Join(directory, "conveyor.yaml")
			if tc.existing != nil {
				if err := os.WriteFile(path, tc.existing, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			fixture := useInitFixtureStore(t, nil)
			if tc.prepare != nil {
				tc.prepare(fixture)
			}
			logged := captureInitLog(t)
			var output strings.Builder
			err := initializeDeployment(t.Context(), &output, path, answers)
			if err == nil {
				t.Fatal("refusal returned no error")
			}
			assertNoInitSecret(t, "error", err.Error())
			if strings.Contains(err.Error(), otherPassword) || strings.Contains(output.String(), otherPassword) || strings.Contains(logged.String(), otherPassword) {
				t.Error("refusal repeats the password the file states")
			}
			assertNoInitSecret(t, "output", output.String())
			assertNoInitSecret(t, "log", logged.String())
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error does not contain %q", tc.want)
			}
			if fixture.linksIssued != tc.linksMade {
				t.Errorf("refusal issued %d sign-in links", fixture.linksIssued)
			}
			if signInTokenFromOutput(t, output.String()) != "" {
				t.Error("refusal printed a sign-in link")
			}
			switch {
			case tc.existing != nil:
				if after, readErr := os.ReadFile(path); readErr != nil || !bytes.Equal(after, tc.existing) {
					t.Errorf("existing file changed (read err %v)", readErr)
				}
				assertOnlyDirectoryEntries(t, directory, "conveyor.yaml")
			default:
				assertOnlyDirectoryEntries(t, directory, tc.published...)
			}
		})
	}
}

// The real backends refuse without a reachable database. A malformed or
// unreachable connection string reaches the operator through the driver's own
// error text, which must hold no password, and leaves no staged file.
func TestInitDriverFailuresHoldNoPassword(t *testing.T) {
	t.Setenv("CONVEYOR_API_TOKEN", initFixtureToken)
	t.Setenv(config.LLMAPIKeyEnv, "fixture-llm-key")
	const head, tail = "fixture-pw-head-a1", "fixture-pw-tail-b2"
	for _, tc := range []struct{ name, raw string }{
		{"postgres password with a colon in an unparseable URL", "postgres://" + initFixtureUser + ":" + head + ":" + tail + "@" + initFixtureHost + ":notaport/db"},
		{"postgres password with a slash", "postgres://" + initFixtureUser + ":" + head + "/" + tail + "@" + initFixtureHost + "/db"},
		{"postgres malformed keyword string", "host=" + initFixtureHost + " password='" + head + " " + tail + "' broken"},
		{"postgres unreachable", "postgres://" + initFixtureUser + ":" + head + "@127.0.0.1:1/db?connect_timeout=1"},
		{"singlestore unparseable URL", "singlestore://" + initFixtureUser + ":" + head + ":" + tail + "@" + initFixtureHost + ":notaport/db"},
		{"singlestore unreachable", "singlestore://" + initFixtureUser + ":" + head + "@127.0.0.1:1/db"},
		{"mysql malformed DSN", initFixtureUser + ":" + head + "@tcp(127.0.0.1:1/db"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			directory := t.TempDir()
			t.Setenv("CONVEYOR_DATABASE_URL", tc.raw)
			logged := captureInitLog(t)
			var output strings.Builder
			err := initializeDeployment(t.Context(), &output, filepath.Join(directory, "conveyor.yaml"), initFixtureAnswers())
			if err == nil {
				t.Fatal("init reached a store through a broken connection string")
			}
			for _, part := range []string{head, tail} {
				if strings.Contains(err.Error(), part) || strings.Contains(output.String(), part) || strings.Contains(logged.String(), part) {
					t.Error("failure text holds part of the password")
				}
			}
			if strings.Contains(err.Error(), tc.raw) {
				t.Error("failure text holds the connection string")
			}
			assertOnlyDirectoryEntries(t, directory)
		})
	}
}

// A collision created nothing, so it must not look like success: no published
// file, no staged file, no sign-in link, and no "already initialized" line. The
// volatile store applies the same conflict rule as the two durable backends
// (runWorkspaceNameUniqueness); assertInitCollisionsRefused proves it on each.
func TestInitRefusesWorkspaceCollision(t *testing.T) {
	first := initFixtureAnswers()
	for _, tc := range []struct {
		name string
		id   string
		as   string
		want string
	}{
		{"taken name with a new ID", "other", first.WorkspaceName, "is taken by another workspace"},
		{"taken name in another case with a new ID", "other", strings.ToUpper(first.WorkspaceName), "is taken by another workspace"},
		{"same ID with a different name", first.WorkspaceID, "Renamed", "already exists with the name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := useInitFixtureStore(t, nil)
			logged := captureInitLog(t)
			var output strings.Builder
			if err := initializeDeployment(t.Context(), &output, filepath.Join(t.TempDir(), "conveyor.yaml"), first); err != nil {
				t.Fatalf("first init failed: %v", err)
			}
			issuedBefore := fixture.linksIssued
			answers := first
			answers.WorkspaceID, answers.WorkspaceName = tc.id, tc.as
			directory := t.TempDir()
			path := filepath.Join(directory, "conveyor.yaml")
			output.Reset()
			err := initializeDeployment(t.Context(), &output, path, answers)
			if err == nil {
				t.Fatal("collision returned no error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("collision error = %v", err)
			}
			assertNoInitSecret(t, "collision error", err.Error())
			assertNoInitSecret(t, "collision output", output.String())
			assertNoInitSecret(t, "collision log", logged.String())
			if output.String() != "" {
				t.Errorf("collision printed %d bytes, want nothing", output.Len())
			}
			if fixture.linksIssued != issuedBefore {
				t.Error("collision issued a sign-in link")
			}
			assertOnlyDirectoryEntries(t, directory)
			workspaces, err := fixture.ListWorkspaces(store.WithActor(t.Context(), store.SystemActor()))
			if err != nil || len(workspaces) != 1 || workspaces[0].ID != first.WorkspaceID || workspaces[0].Name != first.WorkspaceName {
				t.Fatalf("workspaces=%+v err=%v", workspaces, err)
			}
			// The same ID and name stays a safe rerun, even at a new config path.
			output.Reset()
			if err = initializeDeployment(t.Context(), &output, path, first); err != nil {
				t.Fatalf("safe rerun after the refusal failed: %v", err)
			}
			if !strings.Contains(output.String(), "already initialized; issuing a fresh") || signInTokenFromOutput(t, output.String()) == "" {
				t.Fatal("safe rerun did not report the completed deployment and a fresh link")
			}
			if _, err = os.Stat(path); err != nil {
				t.Fatalf("safe rerun published no file: %v", err)
			}
			assertOnlyDirectoryEntries(t, directory, "conveyor.yaml")
		})
	}
}

// A workspace read that fails after a conflict is a failure to decide, never a
// collision answer and never a success.
func TestInitReportsWorkspaceReadFailureAfterConflict(t *testing.T) {
	fixture := useInitFixtureStore(t, nil)
	fixture.failCreate = store.ErrWorkspaceConflict
	fixture.failGetWorkspace = errors.New("connection reset")
	directory := t.TempDir()
	var output strings.Builder
	err := initializeDeployment(t.Context(), &output, filepath.Join(directory, "conveyor.yaml"), initFixtureAnswers())
	if err == nil || !strings.Contains(err.Error(), "connection reset") || strings.Contains(err.Error(), "is taken") {
		t.Fatalf("error = %v", err)
	}
	if !errors.Is(err, fixture.failGetWorkspace) {
		t.Fatal("scrubbed error lost its cause")
	}
	if fixture.linksIssued != 0 || output.String() != "" {
		t.Fatalf("links=%d output bytes=%d", fixture.linksIssued, output.Len())
	}
	assertOnlyDirectoryEntries(t, directory)
}

func TestDatabaseURLSecretsCoversEachForm(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{"postgres://u:plain@h/db", "plain"},
		{"postgres://u:a:b@h:bad/db", "a:b"},
		{"postgres://u:a%40b@h/db", "a@b"},
		{"postgres://u:a%40b@h/db", "a%40b"},
		{"postgres://u@h/db?password=query-pw&sslmode=disable", "query-pw"},
		{"host=h password='quoted pw' dbname=d", "quoted pw"},
		{"host=h password=plain-kw dbname=d", "plain-kw"},
		{"root:dsn-pw@tcp(h:3306)/db?param=a@b", "dsn-pw"},
		{"root:dsn:pw@unix(/tmp/s)/db", "dsn:pw"},
	} {
		found := false
		for _, value := range databaseURLSecrets(tc.raw) {
			found = found || value == tc.want
		}
		if !found {
			t.Errorf("no extracted value equals the expected form %q of a fixture password", tc.want)
		}
	}
	if values := databaseURLSecrets("  "); values != nil {
		t.Fatalf("blank string yields %d values", len(values))
	}
}

// TestInitProcessHelper runs the real CLI entry point in a child process, so a
// test can observe the exit status and both output streams.
func TestInitProcessHelper(t *testing.T) {
	if os.Getenv(initProcessHelperEnv) != "1" {
		t.Skip("child process helper")
	}
	os.Args = append([]string{"conveyor"}, strings.Split(os.Getenv(initProcessHelperArgsEnv), "\x1f")...)
	main()
	os.Exit(0)
}

// runInitProcess runs `conveyor init` in a child process with the parent's
// environment and returns its exit status and output streams.
func runInitProcess(t *testing.T, configPath string, answers initAnswers) (int, string, string) {
	t.Helper()
	args := []string{
		"init", "--config", configPath, "--organization", answers.Organization, "--operator-name", answers.OperatorName,
		"--operator-email", answers.OperatorEmail, "--workspace-id", answers.WorkspaceID, "--workspace-name", answers.WorkspaceName,
		"--repository-name", answers.RepositoryName, "--repository-url", answers.RepositoryURL, "--default-branch", answers.BaseBranch,
	}
	command := exec.Command(os.Args[0], "-test.run=^TestInitProcessHelper$")
	command.Dir = t.TempDir()
	home := t.TempDir()
	command.Env = append(os.Environ(), initProcessHelperEnv+"=1", initProcessHelperArgsEnv+"="+strings.Join(args, "\x1f"), "HOME="+home, "XDG_CONFIG_HOME="+filepath.Join(home, ".config"))
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	status := 0
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		status = exitErr.ExitCode()
	} else if err != nil {
		t.Fatalf("start init process: %v", err)
	}
	return status, stdout.String(), stderr.String()
}

// The installed binary reports a refusal as a nonzero exit with a secret-free
// stderr, here with a connection string no driver accepts.
func TestInitProcessExitsNonzeroWithoutPassword(t *testing.T) {
	t.Setenv("CONVEYOR_DATABASE_URL", "postgres://"+initFixtureUser+":fixture-pw-head-a1:fixture-pw-tail-b2@"+initFixtureHost+":notaport/db")
	t.Setenv("CONVEYOR_API_TOKEN", initFixtureToken)
	t.Setenv(config.LLMAPIKeyEnv, "fixture-llm-key")
	directory := t.TempDir()
	status, stdout, stderr := runInitProcess(t, filepath.Join(directory, "conveyor.yaml"), initFixtureAnswers())
	if status != 1 {
		t.Fatalf("exit status = %d, want 1", status)
	}
	for _, part := range []string{"fixture-pw-head-a1", "fixture-pw-tail-b2"} {
		if strings.Contains(stdout, part) || strings.Contains(stderr, part) {
			t.Error("process output holds part of the password")
		}
	}
	if !strings.Contains(stderr, "error: initialize postgres store") {
		t.Errorf("stderr has %d bytes and no store error", len(stderr))
	}
	assertOnlyDirectoryEntries(t, directory)
}
