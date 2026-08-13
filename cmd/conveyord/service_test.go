package main

import (
	"context"
	"encoding/xml"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestLaunchdServiceEscapesOwnershipCommentAndRemainsValidXML(t *testing.T) {
	platform := testDaemonPlatform(t)
	platform.GOOS = "darwin"
	configPath := "/etc/conveyor/config--blue<&.yaml"
	environmentPath := "/etc/conveyor/runtime--blue<&.env"
	paths, err := resolveDaemonService(platform, configPath, environmentPath)
	if err != nil {
		t.Fatal(err)
	}
	decoder := xml.NewDecoder(strings.NewReader(paths.Definition))
	for {
		if _, err = decoder.Token(); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatalf("rendered launchd plist is invalid XML: %v\n%s", err, paths.Definition)
		}
	}
	if !isOwnedDaemonService(paths.Definition, configPath, environmentPath) {
		t.Fatalf("escaped launchd plist was not recognized as Conveyor-owned: %s", paths.Definition)
	}
	if isOwnedDaemonService(paths.Definition, configPath+"-other", environmentPath) {
		t.Fatal("ownership marker matched an unrelated config path")
	}
	if !strings.Contains(paths.Definition, "<key>EnvironmentVariables</key>") || !strings.Contains(paths.Definition, "<key>CONVEYOR_ENV_FILE</key>") || strings.Contains(paths.Definition, "literal-secret") {
		t.Fatalf("launchd environment reference=%s", paths.Definition)
	}
}

func testDaemonPlatform(t *testing.T) daemonServicePlatform {
	t.Helper()
	root := t.TempDir()
	return daemonServicePlatform{GOOS: "linux", Home: root, ConfigDir: filepath.Join(root, "config"), StateDir: filepath.Join(root, "state"), Executable: "/opt/conveyor/conveyord", UID: 1000, Run: func(context.Context, string, ...string) (string, error) { return "", nil }}
}

func TestDaemonServiceInstallIsRepeatableAndOwnershipSafe(t *testing.T) {
	platform := testDaemonPlatform(t)
	configPath := filepath.Join(t.TempDir(), "conveyor.yaml")
	environmentPath := filepath.Join(filepath.Dir(configPath), ".env")
	if err := os.WriteFile(environmentPath, []byte("CONVEYOR_API_TOKEN=literal-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	paths, err := resolveDaemonService(platform, configPath, environmentPath)
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
	if err != nil || !isOwnedDaemonService(string(data), paths.Config, paths.Environment) {
		t.Fatalf("definition=%q err=%v", data, err)
	}
	if strings.Contains(string(data), "literal-secret") || !strings.Contains(string(data), "EnvironmentFile="+strconv.Quote(environmentPath)) {
		t.Fatalf("systemd environment reference=%q", data)
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
	paths, _ := resolveDaemonService(platform, "/etc/conveyor/conveyor.yaml", "/etc/conveyor/.env")
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

func TestDaemonEnvironmentPathDiscoveryAndPermissions(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "deploy", "conveyor.yaml")
	discovered, err := resolveDaemonEnvironmentPath("", configPath)
	if err != nil || discovered != filepath.Join(root, "deploy", ".env") {
		t.Fatalf("discovered=%q err=%v", discovered, err)
	}
	explicit, err := resolveDaemonEnvironmentPath(filepath.Join(root, "service.env"), configPath)
	if err != nil || explicit != filepath.Join(root, "service.env") {
		t.Fatalf("explicit=%q err=%v", explicit, err)
	}

	if err = validateDaemonEnvironmentFile(discovered); err == nil || !strings.Contains(err.Error(), discovered) {
		t.Fatalf("missing file error=%v", err)
	}
	if err = os.MkdirAll(filepath.Dir(discovered), 0o700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(discovered, []byte("CONVEYOR_API_TOKEN=literal-secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err = validateDaemonEnvironmentFile(discovered); err == nil || !strings.Contains(err.Error(), "owner-only") {
		t.Fatalf("open permissions error=%v", err)
	}
	if err = os.Chmod(discovered, 0o600); err != nil {
		t.Fatal(err)
	}
	if err = validateDaemonEnvironmentFile(discovered); err != nil {
		t.Fatalf("owner-only environment file: %v", err)
	}
}
