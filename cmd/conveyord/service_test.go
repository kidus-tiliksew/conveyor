package main

import (
	"context"
	"encoding/xml"
	"errors"
	"flag"
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

// A file conveyor init wrote holds no database URL, so the install output names
// the variable and the file the service reads it from.
func TestDaemonServiceInstalledSummaryNamesDatabaseVariable(t *testing.T) {
	paths := daemonServicePaths{Unit: "/u/conveyord.service", Stdout: "/l/stdout.log", Stderr: "/l/stderr.log", EnvFile: "/opt/deploy/.env"}
	summary := daemonServiceInstalledSummary(paths)
	for _, required := range []string{"installed conveyord service\n", "unit=/u/conveyord.service\n", "env_file=/opt/deploy/.env\n", "CONVEYOR_DATABASE_URL"} {
		if !strings.Contains(summary, required) {
			t.Fatalf("summary lacks %q:\n%s", required, summary)
		}
	}
	flags, _, _ := newDaemonServiceFlagSet("install")
	if usage := flags.Lookup("env-file").Usage; !strings.Contains(usage, "CONVEYOR_DATABASE_URL") || !strings.Contains(usage, ".env beside config") {
		t.Fatalf("--env-file usage = %q", usage)
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

// runLinuxInstall runs the real conveyord install verb against a fake systemctl
// and returns the unit it wrote.
func runLinuxInstall(t *testing.T, args ...string) string {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("conveyord install writes a systemd unit only on Linux")
	}
	home := t.TempDir()
	tools := t.TempDir()
	if err := os.WriteFile(filepath.Join(tools, "systemctl"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))
	var stdout, stderr strings.Builder
	handled, err := runServiceVerb(t.Context(), append([]string{"install"}, args...), &stdout, &stderr)
	if err != nil || !handled {
		t.Fatalf("install handled=%t err=%v stderr=%s", handled, err, stderr.String())
	}
	unit, err := os.ReadFile(filepath.Join(home, "config", "systemd", "user", "conveyord.service"))
	if err != nil {
		t.Fatal(err)
	}
	return string(unit)
}

// writeOwnerFile creates path and its parents with owner-only permissions.
func writeOwnerFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// unitDirective returns the value of the one line setting name in unit.
func unitDirective(t *testing.T, unit, name string) string {
	t.Helper()
	var values []string
	for _, line := range strings.Split(unit, "\n") {
		if value, ok := strings.CutPrefix(line, name+"="); ok {
			values = append(values, value)
		}
	}
	if len(values) != 1 {
		t.Fatalf("unit sets %s %d times, want once:\n%s", name, len(values), unit)
	}
	return values[0]
}

// systemdEnvironmentWord decodes an Environment= value as systemd does: one
// double-quoted word with C escapes, then specifier expansion. It accepts only
// the single quoted word a unit must hold, so an unquoted value fails.
func systemdEnvironmentWord(t *testing.T, raw string) string {
	t.Helper()
	if len(raw) < 2 || raw[0] != '"' || raw[len(raw)-1] != '"' {
		t.Fatalf("Environment= value %q is not one double-quoted word", raw)
	}
	var word strings.Builder
	body := raw[1 : len(raw)-1]
	for i := 0; i < len(body); i++ {
		switch body[i] {
		case '"':
			t.Fatalf("Environment= value %q ends its quoted word early", raw)
		case '\\':
			i++
			if i == len(body) {
				t.Fatalf("Environment= value %q ends in a bare backslash", raw)
			}
			switch body[i] {
			case '\\', '"':
				word.WriteByte(body[i])
			case 'n':
				word.WriteByte('\n')
			default:
				t.Fatalf("Environment= value %q holds an unknown escape", raw)
			}
		case '%':
			i++
			if i == len(body) || body[i] != '%' {
				t.Fatalf("Environment= value %q holds a bare specifier", raw)
			}
			word.WriteByte('%')
		default:
			word.WriteByte(body[i])
		}
	}
	return word.String()
}

// The unit `conveyord install` writes sets CONVEYOR_ENV_FILE to the env file it
// resolved, quoted as one Environment= word, and leaves EnvironmentFile= as it
// was (component-runtime invariant 4).
func TestLinuxUnitSetsConveyorEnvFile(t *testing.T) {
	for _, tc := range []struct {
		name string
		// directory and envRelative name the deployment directory and the env
		// file inside it; flag is the --env-file value, empty for the default.
		directory, envRelative string
		flag                   func(envFile string) string
		wantRaw                func(envFile string) string
		wantDirective          func(envFile string) string
	}{
		{name: "default path", directory: "deploy", envRelative: ".env"},
		{
			name: "env-file path", directory: "deploy", envRelative: "../elsewhere/conveyor.env",
			flag: func(envFile string) string { return envFile },
		},
		{
			name: "relative env-file path", directory: "deploy", envRelative: "../elsewhere/conveyor.env",
			flag: func(string) string { return "elsewhere/conveyor.env" },
		},
		{name: "default path holding a space", directory: "deploy dir", envRelative: ".env"},
		{
			name: "env-file path holding a space", directory: "deploy", envRelative: "../else where/my conveyor.env",
			flag: func(envFile string) string { return envFile },
		},
		{
			name: "env-file path holding a percent sign", directory: "deploy", envRelative: "../100%h/conveyor.env",
			flag: func(envFile string) string { return envFile },
			wantRaw: func(envFile string) string {
				return `"CONVEYOR_ENV_FILE=` + strings.ReplaceAll(envFile, "%", "%%") + `"`
			},
			wantDirective: func(envFile string) string { return strings.ReplaceAll(envFile, "%", "%%") },
		},
		{
			name: "env-file path holding a quote, a backslash, and a dollar sign", directory: "deploy", envRelative: `../a"b\c$HOME/conveyor.env`,
			flag: func(envFile string) string { return envFile },
			wantRaw: func(envFile string) string {
				return `"CONVEYOR_ENV_FILE=` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(envFile) + `"`
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Chdir(root)
			configPath := filepath.Join(root, tc.directory, "conveyor.yaml")
			envFile := filepath.Join(root, tc.directory, tc.envRelative)
			writeOwnerFile(t, configPath, "workspace: demo\n")
			writeOwnerFile(t, envFile, "CONVEYOR_API_TOKEN=fixture\n")
			args := []string{"-config", configPath}
			if tc.flag != nil {
				args = append(args, "--env-file", tc.flag(envFile))
			}
			unit := runLinuxInstall(t, args...)
			raw := unitDirective(t, unit, "Environment")
			if got, want := systemdEnvironmentWord(t, raw), "CONVEYOR_ENV_FILE="+envFile; got != want {
				t.Fatalf("Environment= decodes to %q, want %q\n%s", got, want, unit)
			}
			if tc.wantRaw != nil {
				if want := tc.wantRaw(envFile); raw != want {
					t.Fatalf("Environment=%s, want %s", raw, want)
				}
			}
			wantDirective := envFile
			if tc.wantDirective != nil {
				wantDirective = tc.wantDirective(envFile)
			}
			if got := unitDirective(t, unit, "EnvironmentFile"); got != wantDirective {
				t.Fatalf("EnvironmentFile=%s, want %s", got, wantDirective)
			}
			if !isOwnedDaemonService(unit, configPath) {
				t.Fatalf("unit lost its ownership marker:\n%s", unit)
			}
		})
	}
}

// A newline cannot end the Environment= line and start another directive.
func TestLinuxUnitEnvironmentValueStaysOnOneLine(t *testing.T) {
	platform := testDaemonPlatform(t)
	envFile := "/opt/deploy/a\nExecStartPre=/bin/false"
	paths, err := resolveDaemonServiceWithEnvironment(platform, "/opt/deploy/conveyor.yaml", envFile)
	if err != nil {
		t.Fatal(err)
	}
	want := `Environment="CONVEYOR_ENV_FILE=/opt/deploy/a\nExecStartPre=/bin/false"` + "\n"
	if !strings.Contains(paths.Definition, want) {
		t.Fatalf("definition lacks %q:\n%s", want, paths.Definition)
	}
}

// The real systemd parser reads the unit's Environment= word back as the
// resolved env file, including every character the word has to escape.
func TestLinuxUnitEnvironmentThroughSystemdWhenAvailable(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("systemd runs only on Linux")
	}
	var manager string
	for _, candidate := range []string{"/usr/lib/systemd/systemd", "/lib/systemd/systemd"} {
		if _, err := os.Stat(candidate); err == nil {
			manager = candidate
			break
		}
	}
	if manager == "" {
		t.Skip("systemd is unavailable")
	}
	root := t.TempDir()
	platform := testDaemonPlatform(t)
	platform.Executable = "/bin/true"
	platform.ConfigDir = filepath.Join(root, "config")
	platform.StateDir = filepath.Join(root, "state")
	for _, envFile := range []string{
		filepath.Join(root, ".env"),
		filepath.Join(root, "other dir", "conveyor.env"),
		filepath.Join(root, `100%h "quoted" back\slash $HOME`, "conveyor.env"),
	} {
		paths, err := resolveDaemonServiceWithEnvironment(platform, filepath.Join(root, "conveyor.yaml"), envFile)
		if err != nil {
			t.Fatal(err)
		}
		writeOwnerFile(t, paths.Unit, paths.Definition)
		runtimeDir := t.TempDir()
		command := exec.CommandContext(t.Context(), manager, "--user", "--test", "--unit="+paths.Name, "--no-pager")
		command.Env = append(os.Environ(), "XDG_CONFIG_HOME="+platform.ConfigDir, "XDG_RUNTIME_DIR="+runtimeDir)
		output, _ := command.CombinedOutput()
		dump := string(output)
		if !strings.Contains(dump, "Unit "+paths.Name) {
			t.Skipf("systemd --test printed no unit dump in this environment (%d bytes)", len(output))
		}
		if want := "Environment: CONVEYOR_ENV_FILE=" + envFile + "\n"; !strings.Contains(dump, want) {
			t.Fatalf("systemd dump lacks %q; unit:\n%s", want, paths.Definition)
		}
	}
}

// A real conveyord process given the unit's WorkingDirectory and Environment=
// value loads the file the unit names and ignores a stale .env in its working
// directory. The control runs the same process with the variable unset, and the
// .env loads (component-runtime invariant 4). The two fixtures make startup stop
// at a different refusal, so each run's log shows which file it read.
func TestLinuxUnitEnvFileKeepsDotEnvOutOfTheProcess(t *testing.T) {
	const childEnv = "CONVEYOR_TEST_SERVICE_ENV_CHILD_CONFIG"
	if path := os.Getenv(childEnv); path != "" {
		flag.CommandLine = flag.NewFlagSet("conveyord", flag.ExitOnError)
		os.Args = []string{"conveyord", "-config", path, "-addr", "127.0.0.1:0"}
		main()
		t.Fatal("conveyord started with a refused environment")
	}
	const (
		staleRefusal = "invalid CONVEYOR_SHUTDOWN_TIMEOUT"
		namedRefusal = `refusing debug address "192.0.2.1:6060"`
	)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	deployDir := filepath.Join(root, "deploy dir")
	configPath := filepath.Join(deployDir, "conveyor.yaml")
	envFile := filepath.Join(root, "named dir", "conveyor.env")
	writeOwnerFile(t, filepath.Join(deployDir, ".env"), "CONVEYOR_SHUTDOWN_TIMEOUT=stale-dotenv-fixture\n")
	writeOwnerFile(t, envFile, "CONVEYOR_DEBUG_ADDR=192.0.2.1:6060\n")
	paths, err := resolveDaemonServiceWithEnvironment(testDaemonPlatform(t), configPath, envFile)
	if err != nil {
		t.Fatal(err)
	}
	workingDirectory := unitDirective(t, paths.Definition, "WorkingDirectory")
	if workingDirectory != deployDir {
		t.Fatalf("WorkingDirectory=%s, want %s", workingDirectory, deployDir)
	}
	assignment := systemdEnvironmentWord(t, unitDirective(t, paths.Definition, "Environment"))
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	run := func(extraEnv ...string) string {
		t.Helper()
		command := exec.CommandContext(t.Context(), executable, "-test.run=^TestLinuxUnitEnvFileKeepsDotEnvOutOfTheProcess$")
		command.Dir = workingDirectory
		for _, item := range os.Environ() {
			if !strings.HasPrefix(item, "CONVEYOR_") {
				command.Env = append(command.Env, item)
			}
		}
		command.Env = append(append(command.Env, childEnv+"="+configPath), extraEnv...)
		output, runErr := command.CombinedOutput()
		var exitErr *exec.ExitError
		if !errors.As(runErr, &exitErr) || exitErr.ExitCode() == 0 {
			t.Fatalf("conveyord exit err=%v output=%s", runErr, output)
		}
		return string(output)
	}

	withVariable := run(assignment)
	if !strings.Contains(withVariable, namedRefusal) {
		t.Fatalf("process did not load the named env file: %s", withVariable)
	}
	if strings.Contains(withVariable, staleRefusal) || strings.Contains(withVariable, "stale-dotenv-fixture") {
		t.Fatalf("process loaded .env from its working directory: %s", withVariable)
	}

	control := run()
	if !strings.Contains(control, staleRefusal) {
		t.Fatalf("control did not load .env from its working directory: %s", control)
	}
	if strings.Contains(control, namedRefusal) {
		t.Fatalf("control loaded the named env file: %s", control)
	}
}

// The startup log names the line and expected type of a misplaced value and
// never the value (req-security-boundaries REQ-2).
func TestConveyordStartupLogDropsMisplacedConfigValue(t *testing.T) {
	const childEnv = "CONVEYOR_TEST_MISPLACED_VALUE_CONFIG"
	if path := os.Getenv(childEnv); path != "" {
		flag.CommandLine = flag.NewFlagSet("conveyord", flag.ExitOnError)
		os.Args = []string{"conveyord", "-config", path, "-addr", "127.0.0.1:0"}
		main()
		t.Fatal("conveyord started with a malformed config")
	}
	for _, tc := range []struct{ name, value, printed string }{
		{name: "printed in full", value: "Zq7Kp3x-pw", printed: "Zq7Kp3x-pw"},
		{name: "printed by prefix", value: "Yt5Rm8w-fixture-secret-value", printed: "Yt5Rm8w"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			configPath := filepath.Join(dir, "conveyor.yaml")
			envPath := filepath.Join(dir, "empty.env")
			writeOwnerFile(t, configPath, "workspace: demo\nmax_bounces: "+tc.value+"\n")
			writeOwnerFile(t, envPath, "")
			command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestConveyordStartupLogDropsMisplacedConfigValue$")
			for _, item := range os.Environ() {
				if !strings.HasPrefix(item, "CONVEYOR_") {
					command.Env = append(command.Env, item)
				}
			}
			command.Env = append(command.Env, childEnv+"="+configPath, "CONVEYOR_ENV_FILE="+envPath)
			output, err := command.CombinedOutput()
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() == 0 {
				t.Fatalf("conveyord exit status is not a refusal: %v", err)
			}
			startupLog := string(output)
			if strings.Contains(startupLog, tc.printed) {
				t.Fatal("startup log holds the misplaced value")
			}
			if !strings.Contains(startupLog, "load config: ") || !strings.Contains(startupLog, "line 2: cannot unmarshal !!str into int") {
				t.Fatalf("startup log lacks the line and expected type: %s", startupLog)
			}
		})
	}
}

// A value that decodes and then breaks a validator rule stays out of the startup
// log, for a site in internal/config/deployment.go and a site in
// internal/config/config.go, while the field path and rule remain. Each control
// states an accepted value beside a later refusal, so the rule judged the value
// and the log would show a printed one (req-security-boundaries REQ-2).
func TestConveyordStartupLogDropsValidatorRefusedValue(t *testing.T) {
	const childEnv = "CONVEYOR_TEST_VALIDATOR_VALUE_CONFIG"
	if path := os.Getenv(childEnv); path != "" {
		flag.CommandLine = flag.NewFlagSet("conveyord", flag.ExitOnError)
		os.Args = []string{"conveyord", "-config", path, "-addr", "127.0.0.1:0"}
		main()
		t.Fatal("conveyord started with a refused config")
	}
	const value = "sk-live-FAKE0123456789"
	const effortRule = "must be minimal, low, medium, or high"
	run := func(t *testing.T, document string) string {
		t.Helper()
		dir := t.TempDir()
		configPath := filepath.Join(dir, "conveyor.yaml")
		envPath := filepath.Join(dir, "empty.env")
		writeOwnerFile(t, configPath, "workspace: demo\ndatabase: {backend: memory}\n"+document)
		writeOwnerFile(t, envPath, "")
		command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestConveyordStartupLogDropsValidatorRefusedValue$")
		for _, item := range os.Environ() {
			if !strings.HasPrefix(item, "CONVEYOR_") {
				command.Env = append(command.Env, item)
			}
		}
		command.Env = append(command.Env, childEnv+"="+configPath, "CONVEYOR_ENV_FILE="+envPath)
		output, err := command.CombinedOutput()
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() == 0 {
			t.Fatalf("conveyord exit status is not a refusal: %v", err)
		}
		return string(output)
	}
	triage := func(body string) string {
		return "execution_settings: {control_plane: {triage: {model: fixture-model, " + body + "}}}\n"
	}
	planning := func(body string) string {
		return "execution_settings: {control_plane: {triage: {model: fixture-model}, planning: {" + body + "}}}\n"
	}
	for _, tc := range []struct{ name, document, want string }{
		{"deployment.go plain", triage("effort: " + value), "execution_settings.control_plane.triage.effort " + effortRule},
		{"deployment.go tagged", triage("effort: !!str " + value), "execution_settings.control_plane.triage.effort " + effortRule},
		{"deployment.go aliased", "pack_dir: &misplaced " + value + "\n" + triage("effort: *misplaced"), "execution_settings.control_plane.triage.effort " + effortRule},
		{"config.go plain", planning("effort: " + value), "execution_settings.control_plane.planning.effort " + effortRule},
		{"config.go tagged", planning("effort: !!str " + value), "execution_settings.control_plane.planning.effort " + effortRule},
		{"config.go aliased", "pack_dir: &misplaced " + value + "\n" + planning("effort: *misplaced"), "execution_settings.control_plane.planning.effort " + effortRule},
	} {
		t.Run(tc.name, func(t *testing.T) {
			startupLog := run(t, tc.document)
			if strings.Contains(startupLog, value) {
				t.Fatal("startup log holds the misplaced value")
			}
			if !strings.Contains(startupLog, "load config: ") || !strings.Contains(startupLog, tc.want) {
				t.Fatalf("startup log lacks %q: %s", tc.want, startupLog)
			}
		})
	}
	for _, tc := range []struct{ name, document, later, rule string }{
		{"deployment.go control", triage("effort: low, timeout: not-a-duration"), "execution_settings.control_plane.triage.timeout must be a positive duration", "triage.effort"},
		{"config.go control", planning("effort: low, timeout: not-a-duration"), "execution_settings.control_plane.planning.timeout must be a positive duration", "planning.effort"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			startupLog := run(t, tc.document)
			if strings.Contains(startupLog, tc.rule) || !strings.Contains(startupLog, tc.later) {
				t.Fatalf("accepted value did not reach the later refusal: %s", startupLog)
			}
		})
	}
}

// A tagged or aliased value takes yaml.v3's plain errors instead of its type
// errors, and the startup log still holds no value. The control is the same file
// with a well-formed tagged value, which gets past the decoder.
func TestConveyordStartupLogDropsTaggedConfigValue(t *testing.T) {
	const childEnv = "CONVEYOR_TEST_TAGGED_VALUE_CONFIG"
	if path := os.Getenv(childEnv); path != "" {
		flag.CommandLine = flag.NewFlagSet("conveyord", flag.ExitOnError)
		os.Args = []string{"conveyord", "-config", path, "-addr", "127.0.0.1:0"}
		main()
		t.Fatal("conveyord started with a malformed config")
	}
	const value = "Zq7Kp3x-pw"
	run := func(t *testing.T, line string) string {
		t.Helper()
		dir := t.TempDir()
		configPath := filepath.Join(dir, "conveyor.yaml")
		envPath := filepath.Join(dir, "empty.env")
		writeOwnerFile(t, configPath, "workspace: demo\n"+line+"\n")
		writeOwnerFile(t, envPath, "")
		command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestConveyordStartupLogDropsTaggedConfigValue$")
		for _, item := range os.Environ() {
			if !strings.HasPrefix(item, "CONVEYOR_") {
				command.Env = append(command.Env, item)
			}
		}
		command.Env = append(command.Env, childEnv+"="+configPath, "CONVEYOR_ENV_FILE="+envPath)
		output, err := command.CombinedOutput()
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() == 0 {
			t.Fatalf("conveyord exit status is not a refusal: %v", err)
		}
		return string(output)
	}
	for _, tc := range []struct{ name, line, want string }{
		{name: "int tag", line: "max_bounces: !!int " + value, want: "line 2: cannot decode !!str as a !!int (expected int)"},
		{name: "bool tag", line: "max_bounces: !!bool " + value, want: "line 2: cannot decode !!str as a !!bool (expected int)"},
		{name: "binary tag", line: "max_bounces: !!binary " + value, want: "line 2: !!binary value contains invalid base64 data (expected int)"},
		{name: "custom tag", line: "max_bounces: !" + value + " x", want: "line 2: cannot unmarshal value into int"},
		{name: "undefined alias", line: "max_bounces: *" + value, want: "line 2: unknown anchor referenced (expected int)"},
		{name: "time key", line: "issue_publication: {enabled_at: " + value + "}", want: "line 2: cannot unmarshal value into time.Time"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			startupLog := run(t, tc.line)
			if strings.Contains(startupLog, value) {
				t.Fatal("startup log holds the misplaced value")
			}
			if !strings.Contains(startupLog, "load config: ") || !strings.Contains(startupLog, tc.want) {
				t.Fatalf("startup log lacks %q: %s", tc.want, startupLog)
			}
		})
	}
	t.Run("control", func(t *testing.T) {
		startupLog := run(t, "max_bounces: !!int 3")
		for _, refused := range []string{"cannot decode", "cannot unmarshal", "unrecognized YAML decode error"} {
			if strings.Contains(startupLog, refused) {
				t.Fatalf("well-formed tagged value was refused by the decoder: %s", startupLog)
			}
		}
	})
}
