package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

type recordedServiceCommand struct {
	name string
	args []string
}

func testWorkerServicePlatform(t *testing.T, goos string, run func(context.Context, string, ...string) (string, error)) workerServicePlatform {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	return workerServicePlatform{
		GOOS:       goos,
		Home:       root,
		ConfigDir:  filepath.Join(root, "config"),
		StateDir:   filepath.Join(root, "state"),
		Executable: filepath.Join(root, "bin", "conveyor"),
		UID:        501,
		Run:        run,
	}
}

func saveTestWorkerEnrollment(t *testing.T, workspace string) string {
	t.Helper()
	path, err := credentialPath(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(workerCredentialFile{Workspace: workspace, WorkerID: "worker-1", Credential: "saved-secret"})
	if err = os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestWorkerServiceDefinitionsAreWorkspaceSpecificAndSecretFree(t *testing.T) {
	for _, goos := range []string{"darwin", "linux"} {
		t.Run(goos, func(t *testing.T) {
			platform := testWorkerServicePlatform(t, goos, nil)
			paths, err := resolveWorkerService(platform, "demo", "https://control.example")
			if err != nil {
				t.Fatal(err)
			}
			for _, forbidden := range []string{"saved-secret", "CONVEYOR_API_TOKEN", "CONVEYOR_WORKER_TOKEN", "--pairing-token"} {
				if strings.Contains(paths.Definition, forbidden) {
					t.Fatalf("definition contains secret-bearing field %q:\n%s", forbidden, paths.Definition)
				}
			}
			for _, required := range []string{workerServiceOwner, "--workspace", "demo", "worker", "run", "https://control.example"} {
				if !strings.Contains(paths.Definition, required) {
					t.Fatalf("definition missing %q:\n%s", required, paths.Definition)
				}
			}
			other, err := resolveWorkerService(platform, "demo-2", "https://control.example")
			if err != nil {
				t.Fatal(err)
			}
			if paths.Name == other.Name || paths.Unit == other.Unit {
				t.Fatalf("workspace services collide: %#v %#v", paths, other)
			}
		})
	}
}

func TestLinuxWorkerServicePathDirectivesAreUnquoted(t *testing.T) {
	platform := testWorkerServicePlatform(t, "linux", nil)
	paths, err := resolveWorkerService(platform, "demo", "https://control.example")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		"StandardOutput=append:" + paths.Stdout + "\n",
		"StandardError=append:" + paths.Stderr + "\n",
	} {
		if !strings.Contains(paths.Definition, required) {
			t.Fatalf("definition missing unquoted directive %q:\n%s", required, paths.Definition)
		}
	}
}

func TestWorkerServiceInstallRequiresSavedEnrollmentBeforeMutation(t *testing.T) {
	var commands []recordedServiceCommand
	platform := testWorkerServicePlatform(t, "linux", func(_ context.Context, name string, args ...string) (string, error) {
		commands = append(commands, recordedServiceCommand{name: name, args: args})
		return "", nil
	})
	paths, err := resolveWorkerService(platform, "demo", "https://control.example")
	if err != nil {
		t.Fatal(err)
	}
	_, err = installWorkerService(t.Context(), &client{base: "https://control.example", workspace: "demo"}, platform)
	if err == nil || !strings.Contains(err.Error(), "worker pair") || !strings.Contains(err.Error(), "--pairing-token") {
		t.Fatalf("expected actionable enrollment error, got %v", err)
	}
	if len(commands) != 0 {
		t.Fatalf("service manager called before enrollment validation: %#v", commands)
	}
	if _, statErr := os.Stat(paths.Unit); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("unit written before enrollment validation: %v", statErr)
	}
}

func TestLinuxWorkerServiceInstallUninstallIsIdempotentAndPreservesEnrollment(t *testing.T) {
	var commands []recordedServiceCommand
	platform := testWorkerServicePlatform(t, "linux", func(_ context.Context, name string, args ...string) (string, error) {
		commands = append(commands, recordedServiceCommand{name: name, args: append([]string(nil), args...)})
		return "", nil
	})
	credential := saveTestWorkerEnrollment(t, "demo")
	c := &client{base: "https://control.example", workspace: "demo"}
	first, err := installWorkerService(t.Context(), c, platform)
	if err != nil {
		t.Fatal(err)
	}
	second, err := installWorkerService(t.Context(), c, platform)
	if err != nil {
		t.Fatal(err)
	}
	if first.Unit != second.Unit {
		t.Fatalf("repeated install created a different unit: %s != %s", first.Unit, second.Unit)
	}
	info, err := os.Stat(first.Unit)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("unit mode=%o, want 600", info.Mode().Perm())
	}
	definition, err := os.ReadFile(first.Unit)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(definition), "saved-secret") {
		t.Fatal("saved credential leaked into systemd unit")
	}
	for _, logPath := range []string{first.Stdout, first.Stderr} {
		logInfo, statErr := os.Stat(logPath)
		if statErr != nil {
			t.Fatal(statErr)
		}
		if logInfo.Mode().Perm() != 0o600 {
			t.Fatalf("log %s mode=%o, want 600", logPath, logInfo.Mode().Perm())
		}
	}
	removedPaths, removed, err := uninstallWorkerService(t.Context(), c, platform)
	if err != nil || !removed || removedPaths.Unit != first.Unit {
		t.Fatalf("uninstall=(%#v,%v,%v)", removedPaths, removed, err)
	}
	if _, err = os.Stat(first.Unit); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unit remains after uninstall: %v", err)
	}
	if _, err = os.Stat(credential); err != nil {
		t.Fatalf("uninstall removed enrollment: %v", err)
	}
	if _, removed, err = uninstallWorkerService(t.Context(), c, platform); err != nil || removed {
		t.Fatalf("repeated uninstall=(removed=%v, err=%v)", removed, err)
	}
	joined := make([]string, 0, len(commands))
	for _, command := range commands {
		joined = append(joined, command.name+" "+strings.Join(command.args, " "))
	}
	output := strings.Join(joined, "\n")
	for _, expected := range []string{
		"systemctl --user daemon-reload",
		"systemctl --user enable --now " + first.Name,
		"systemctl --user disable --now " + first.Name,
	} {
		if !strings.Contains(output, expected) {
			t.Fatalf("commands missing %q:\n%s", expected, output)
		}
	}
}

func TestWorkerServiceFailsClosedOnConflictingUnit(t *testing.T) {
	platform := testWorkerServicePlatform(t, "linux", func(context.Context, string, ...string) (string, error) {
		t.Fatal("service manager must not run for conflicting unit")
		return "", nil
	})
	saveTestWorkerEnrollment(t, "demo")
	paths, err := resolveWorkerService(platform, "demo", "https://control.example")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(filepath.Dir(paths.Unit), 0o700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(paths.Unit, []byte("[Service]\nExecStart=/tmp/not-conveyor\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := &client{base: "https://control.example", workspace: "demo"}
	if _, err = installWorkerService(t.Context(), c, platform); err == nil || !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Fatalf("install conflict error=%v", err)
	}
	if _, _, err = uninstallWorkerService(t.Context(), c, platform); err == nil || !strings.Contains(err.Error(), "refusing to remove") {
		t.Fatalf("uninstall conflict error=%v", err)
	}
}

func TestDarwinWorkerServiceLifecycleUsesPerUserLaunchAgent(t *testing.T) {
	var commands []string
	platform := testWorkerServicePlatform(t, "darwin", func(_ context.Context, name string, args ...string) (string, error) {
		commands = append(commands, name+" "+strings.Join(args, " "))
		return "", nil
	})
	saveTestWorkerEnrollment(t, "demo")
	c := &client{base: "https://control.example", workspace: "demo"}
	paths, err := installWorkerService(t.Context(), c, platform)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(paths.Unit, filepath.Join("Library", "LaunchAgents")) {
		t.Fatalf("unit path=%s", paths.Unit)
	}
	if _, _, err = uninstallWorkerService(t.Context(), c, platform); err != nil {
		t.Fatal(err)
	}
	output := strings.Join(commands, "\n")
	for _, expected := range []string{
		"launchctl bootstrap gui/501 " + paths.Unit,
		"launchctl kickstart -k gui/501/" + paths.Name,
		"launchctl bootout gui/501/" + paths.Name,
	} {
		if !strings.Contains(output, expected) {
			t.Fatalf("commands missing %q:\n%s", expected, output)
		}
	}
}

func TestWorkerServiceStatusSeparatesLocalFailureFromRemoteLiveness(t *testing.T) {
	now := time.Now().UTC()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/workers" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(workerListResponse{Workers: []core.Worker{{
			ID:             "worker-1",
			Workspace:      "demo",
			LastSeenAt:     now,
			LeaseExpiresAt: now.Add(time.Minute),
			Probes:         []core.HarnessProbe{{Harness: "codex", Healthy: true, CheckedAt: now}},
		}}})
	}))
	defer server.Close()
	managerState := "ActiveState=active\nSubState=running\nResult=success\n"
	platform := testWorkerServicePlatform(t, "linux", func(_ context.Context, _ string, args ...string) (string, error) {
		if strings.Contains(strings.Join(args, " "), "show") {
			return managerState, nil
		}
		return "", nil
	})
	saveTestWorkerEnrollment(t, "demo")
	paths, err := resolveWorkerService(platform, "demo", server.URL)
	if err != nil {
		t.Fatal(err)
	}
	c := &client{base: server.URL, workspace: "demo"}
	status, err := inspectWorkerService(t.Context(), c, platform)
	if err != nil {
		t.Fatal(err)
	}
	if status.Service.Installed || status.Service.State != "stopped" {
		t.Fatalf("before-install local status=%#v", status.Service)
	}
	if err = os.MkdirAll(filepath.Dir(paths.Unit), 0o700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(paths.Unit, []byte(paths.Definition), 0o600); err != nil {
		t.Fatal(err)
	}
	status, err = inspectWorkerService(t.Context(), c, platform)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Service.Installed || status.Service.State != "running" {
		t.Fatalf("running local status=%#v", status.Service)
	}
	managerState = "ActiveState=failed\nSubState=failed\nResult=exit-code\n"
	status, err = inspectWorkerService(t.Context(), c, platform)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Service.Installed || status.Service.State != "failed" {
		t.Fatalf("local status=%#v", status.Service)
	}
	if !status.Enrollment.Present || status.Enrollment.WorkerID != "worker-1" {
		t.Fatalf("enrollment=%#v", status.Enrollment)
	}
	if status.Remote.State != "live" || len(status.Remote.Probes) != 1 || !status.Remote.Probes[0].Healthy {
		t.Fatalf("remote status=%#v", status.Remote)
	}
	managerState = "ActiveState=active\nSubState=running\nResult=success\n"
	status, err = inspectWorkerService(t.Context(), c, platform)
	if err != nil || status.Service.State != "running" {
		t.Fatalf("restarted local status=%#v err=%v", status.Service, err)
	}
	if err = os.Remove(paths.Unit); err != nil {
		t.Fatal(err)
	}
	status, err = inspectWorkerService(t.Context(), c, platform)
	if err != nil || status.Service.Installed || status.Service.State != "stopped" {
		t.Fatalf("after-uninstall local status=%#v err=%v", status.Service, err)
	}
}

func TestWorkerServiceRejectsImplicitWorkspaceAndUnsupportedPlatform(t *testing.T) {
	platform := testWorkerServicePlatform(t, "windows", nil)
	if _, err := resolveWorkerService(platform, "", "https://control.example"); err == nil || !strings.Contains(err.Error(), "--workspace is required") {
		t.Fatalf("implicit workspace error=%v", err)
	}
	if _, err := resolveWorkerService(platform, "demo", "https://control.example"); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("unsupported platform error=%v", err)
	}
}

func TestWorkerServiceDefinitionNativeSyntaxWhenAvailable(t *testing.T) {
	var tool string
	var args func(string) []string
	switch runtime.GOOS {
	case "darwin":
		tool = "plutil"
		args = func(path string) []string { return []string{"-lint", path} }
	case "linux":
		tool = "systemd-analyze"
		args = func(path string) []string { return []string{"verify", path} }
	default:
		t.Skip("no supported native user service syntax checker")
	}
	if _, err := exec.LookPath(tool); err != nil {
		t.Skipf("%s is unavailable", tool)
	}
	platform := testWorkerServicePlatform(t, runtime.GOOS, nil)
	platform.Executable = os.Args[0]
	paths, err := resolveWorkerService(platform, "demo", "https://control.example")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(filepath.Dir(paths.Unit), 0o700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(paths.Unit, []byte(paths.Definition), 0o600); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command(tool, args(paths.Unit)...).CombinedOutput(); err != nil {
		t.Fatalf("%s rejected generated definition: %v\n%s", tool, err, output)
	}
}

func TestWorkerServiceLifecycleIgnoresOnlyAbsentManagerState(t *testing.T) {
	platform := testWorkerServicePlatform(t, "darwin", func(context.Context, string, ...string) (string, error) {
		return "Could not find specified service", errors.New("exit status 3")
	})
	if err := runWorkerServiceCommandAllowAbsent(t.Context(), platform, "launchctl", "bootout", "gui/501/example"); err != nil {
		t.Fatalf("absent service should be idempotent: %v", err)
	}
	platform.Run = func(context.Context, string, ...string) (string, error) {
		return "permission denied", errors.New("exit status 1")
	}
	if err := runWorkerServiceCommandAllowAbsent(t.Context(), platform, "launchctl", "bootout", "gui/501/example"); err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("real manager failure was hidden: %v", err)
	}
}
