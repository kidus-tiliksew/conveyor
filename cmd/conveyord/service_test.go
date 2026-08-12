package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testDaemonPlatform(t *testing.T) daemonServicePlatform {
	t.Helper()
	root := t.TempDir()
	return daemonServicePlatform{GOOS: "linux", Home: root, ConfigDir: filepath.Join(root, "config"), StateDir: filepath.Join(root, "state"), Executable: "/opt/conveyor/conveyord", UID: 1000, Run: func(context.Context, string, ...string) (string, error) { return "", nil }}
}

func TestDaemonServiceInstallIsRepeatableAndOwnershipSafe(t *testing.T) {
	platform := testDaemonPlatform(t)
	paths, err := resolveDaemonService(platform, "/etc/conveyor/conveyor.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err = installDaemonService(t.Context(), platform, paths); err != nil {
		t.Fatal(err)
	}
	if err = installDaemonService(t.Context(), platform, paths); err != nil {
		t.Fatalf("repeat install: %v", err)
	}
	data, err := os.ReadFile(paths.Unit)
	if err != nil || !isOwnedDaemonService(string(data), paths.Config) {
		t.Fatalf("definition=%q err=%v", data, err)
	}
	if removed, err := uninstallDaemonService(t.Context(), platform, paths); err != nil || !removed {
		t.Fatalf("removed=%t err=%v", removed, err)
	}
	if removed, err := uninstallDaemonService(t.Context(), platform, paths); err != nil || removed {
		t.Fatalf("repeat removed=%t err=%v", removed, err)
	}

	if err = os.MkdirAll(filepath.Dir(paths.Unit), 0o700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(paths.Unit, []byte("unrelated service"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err = installDaemonService(t.Context(), platform, paths); err == nil || !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Fatalf("install error=%v", err)
	}
	if _, err = uninstallDaemonService(t.Context(), platform, paths); err == nil || !strings.Contains(err.Error(), "refusing to remove") {
		t.Fatalf("uninstall error=%v", err)
	}
}

func TestRunServiceVerbReportsReleaseIdentity(t *testing.T) {
	var output strings.Builder
	handled, err := runServiceVerb(context.Background(), []string{"version"}, &output, &output)
	if err != nil || !handled || !strings.Contains(output.String(), "conveyord ") {
		t.Fatalf("handled=%t output=%q err=%v", handled, output.String(), err)
	}
	handled, err = runServiceVerb(context.Background(), []string{"serve"}, &output, &output)
	if err != nil || handled {
		t.Fatalf("serve handled=%t err=%v", handled, err)
	}
}

func TestInspectDaemonServiceRejectsUnownedDefinition(t *testing.T) {
	platform := testDaemonPlatform(t)
	paths, _ := resolveDaemonService(platform, "/etc/conveyor/conveyor.yaml")
	if err := os.MkdirAll(filepath.Dir(paths.Unit), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.Unit, []byte("not ours"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := inspectDaemonService(t.Context(), platform, paths)
	if err == nil || !strings.Contains(err.Error(), "not owned") {
		t.Fatalf("error=%v", err)
	}
	platform.Run = func(context.Context, string, ...string) (string, error) { return "", errors.New("unused") }
}
