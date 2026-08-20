package main

import (
	"context"
	"encoding/xml"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestLaunchdServiceEscapesOwnershipCommentAndRemainsValidXML(t *testing.T) {
	platform := testDaemonPlatform(t)
	platform.GOOS = "darwin"
	configPath := "/etc/conveyor/config--blue<&.yaml"
	paths, err := resolveDaemonService(platform, configPath)
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
	if !isOwnedDaemonService(paths.Definition, configPath) {
		t.Fatalf("escaped launchd plist was not recognized as Conveyor-owned: %s", paths.Definition)
	}
	if isOwnedDaemonService(paths.Definition, configPath+"-other") {
		t.Fatal("ownership marker matched an unrelated config path")
	}
	if !strings.Contains(paths.Definition, "EnvironmentVariables") || strings.Contains(paths.Definition, "secret-value") {
		t.Fatalf("launchd environment reference missing or secret leaked: %s", paths.Definition)
	}
}

func testDaemonPlatform(t *testing.T) daemonServicePlatform {
	t.Helper()
	root := t.TempDir()
	return daemonServicePlatform{GOOS: "linux", Home: root, ConfigDir: filepath.Join(root, "config"), StateDir: filepath.Join(root, "state"), Executable: "/opt/conveyor/conveyord", UID: 1000, Run: func(context.Context, string, ...string) (string, error) { return "", nil }}
}

func TestDaemonServiceInstallIsRepeatableAndOwnershipSafe(t *testing.T) {
	platform := testDaemonPlatform(t)
	envFile := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(envFile, []byte("CONVEYOR_API_TOKEN=test\nCONVEYOR_LLM_API_KEY=test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	paths, err := resolveDaemonServiceWithEnvironment(platform, "/etc/conveyor/conveyor.yaml", envFile)
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

func TestDaemonServiceEnvironmentFileAndLinuxDirectives(t *testing.T) {
	platform := testDaemonPlatform(t)
	envFile := filepath.Join(t.TempDir(), "conveyor.env")
	if err := os.WriteFile(envFile, []byte("CONVEYOR_API_TOKEN=secret-value\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	paths, err := resolveDaemonServiceWithEnvironment(platform, "/opt/deploy/conveyor.yaml", envFile)
	if err != nil {
		t.Fatal(err)
	}
	if err = installDaemonService(t.Context(), platform, paths); err == nil || !strings.Contains(err.Error(), "owner-only") {
		t.Fatalf("insecure environment file error=%v", err)
	}
	if _, statErr := os.Stat(paths.Unit); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("unit mutated before environment validation: %v", statErr)
	}
	if err = os.Chmod(envFile, 0o200); err != nil {
		t.Fatal(err)
	}
	if err = installDaemonService(t.Context(), platform, paths); err == nil || !strings.Contains(err.Error(), "readable by its owner") {
		t.Fatalf("unreadable environment file error=%v", err)
	}
	if err = os.Chmod(envFile, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		"WorkingDirectory=/opt/deploy\n",
		"EnvironmentFile=" + envFile + "\n",
		"StandardOutput=append:" + paths.Stdout + "\n",
		"StandardError=append:" + paths.Stderr + "\n",
		`ExecStart="/opt/conveyor/conveyord" -config "/opt/deploy/conveyor.yaml"`,
	} {
		if !strings.Contains(paths.Definition, required) {
			t.Fatalf("definition missing %q:\n%s", required, paths.Definition)
		}
	}
	if strings.Contains(paths.Definition, "secret-value") {
		t.Fatal("environment secret leaked into unit")
	}
}

func TestDaemonServiceDefinitionNativeSyntaxWhenAvailable(t *testing.T) {
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
	platform := testDaemonPlatform(t)
	platform.GOOS = runtime.GOOS
	platform.Executable = os.Args[0]
	root := t.TempDir()
	paths, err := resolveDaemonServiceWithEnvironment(platform, filepath.Join(root, "conveyor.yaml"), filepath.Join(root, ".env"))
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
