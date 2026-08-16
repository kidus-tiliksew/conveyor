package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/serviceunit"
	"github.com/spf13/cobra"
)

const workerServiceOwner = "conveyor-worker-service-v1"

var workerServiceWorkspacePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

type workerServicePaths struct {
	Name       string `json:"name"`
	Unit       string `json:"unit_path"`
	Stdout     string `json:"stdout_log"`
	Stderr     string `json:"stderr_log"`
	Config     string `json:"execution_config,omitempty"`
	ConfigDir  string `json:"-"`
	Home       string `json:"-"`
	Definition string `json:"-"`
}

type workerServicePlatform struct {
	GOOS       string
	Home       string
	ConfigDir  string
	StateDir   string
	Executable string
	UID        int
	Run        func(context.Context, string, ...string) (string, error)
}

type workerLocalStatus struct {
	Installed bool   `json:"installed"`
	State     string `json:"state"`
	Detail    string `json:"detail,omitempty"`
}

type workerRemoteStatus struct {
	State           string              `json:"state"`
	LastHeartbeatAt time.Time           `json:"last_heartbeat_at,omitempty"`
	LeaseExpiresAt  time.Time           `json:"lease_expires_at,omitempty"`
	Probes          []core.HarnessProbe `json:"harness_probes"`
	Error           string              `json:"error,omitempty"`
}

type workerServiceStatus struct {
	Workspace  string                 `json:"workspace"`
	Enrollment *workerEnrollmentState `json:"enrollment"`
	Service    workerLocalStatus      `json:"local_service"`
	Remote     workerRemoteStatus     `json:"remote_worker"`
	Paths      workerServicePaths     `json:"paths"`
}

type workerEnrollmentState struct {
	Present  bool   `json:"present"`
	WorkerID string `json:"worker_id,omitempty"`
	Error    string `json:"error,omitempty"`
}

func workerInstallCmd() *cobra.Command {
	configPath := defaultLocalExecutionConfigPath()
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Install and start the enrolled worker as a user service",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			platform, err := currentWorkerServicePlatform()
			if err != nil {
				return err
			}
			client := newClient()
			paths, err := installWorkerService(cmd.Context(), client, platform, configPath)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "installed worker service for workspace %s\nunit=%s\nexecution_config=%s\nstdout_log=%s\nstderr_log=%s\n",
				client.workspace, paths.Unit, paths.Config, paths.Stdout, paths.Stderr)
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", configPath, "local execution configuration")
	return cmd
}

func workerUninstallCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "uninstall",
		Short: "Stop and remove the workspace worker user service",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			platform, err := currentWorkerServicePlatform()
			if err != nil {
				return err
			}
			client := newClient()
			paths, removed, err := uninstallWorkerService(cmd.Context(), client, platform)
			if err != nil {
				return err
			}
			if removed {
				fmt.Fprintf(cmd.OutOrStdout(), "uninstalled worker service for workspace %s; enrollment preserved\nunit=%s\n", client.workspace, paths.Unit)
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "worker service for workspace %s is already uninstalled; enrollment preserved\nunit=%s\n", client.workspace, paths.Unit)
			}
			return nil
		},
	}
}

func workerStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show local service state and remote worker liveness",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			platform, err := currentWorkerServicePlatform()
			if err != nil {
				return err
			}
			status, err := inspectWorkerService(cmd.Context(), newClient(), platform)
			if err != nil {
				return err
			}
			data, err := json.MarshalIndent(status, "", "  ")
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), string(data))
			return nil
		},
	}
}

func currentWorkerServicePlatform() (workerServicePlatform, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return workerServicePlatform{}, fmt.Errorf("resolve user home: %w", err)
	}
	configDir, err := os.UserConfigDir()
	if err != nil {
		return workerServicePlatform{}, fmt.Errorf("resolve user config directory: %w", err)
	}
	executable, err := os.Executable()
	if err != nil {
		return workerServicePlatform{}, fmt.Errorf("resolve Conveyor executable: %w", err)
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return workerServicePlatform{}, fmt.Errorf("resolve Conveyor executable symlinks: %w", err)
	}
	stateDir := strings.TrimSpace(os.Getenv("XDG_STATE_HOME"))
	if stateDir == "" {
		stateDir = filepath.Join(home, ".local", "state")
	}
	return workerServicePlatform{
		GOOS:       runtime.GOOS,
		Home:       home,
		ConfigDir:  configDir,
		StateDir:   stateDir,
		Executable: executable,
		UID:        os.Getuid(),
		Run: func(ctx context.Context, name string, args ...string) (string, error) {
			output, runErr := exec.CommandContext(ctx, name, args...).CombinedOutput()
			return strings.TrimSpace(string(output)), runErr
		},
	}, nil
}

func requireWorkerServiceWorkspace(workspace string) error {
	if workspace == "" {
		return fmt.Errorf("--workspace is required for worker service management")
	}
	if !workerServiceWorkspacePattern.MatchString(workspace) {
		return fmt.Errorf("workspace %q is not a valid immutable workspace id", workspace)
	}
	return nil
}

func workerServiceIdentity(workspace string) string {
	sum := sha256.Sum256([]byte(workspace))
	return workspace + "-" + hex.EncodeToString(sum[:4])
}

func resolveWorkerService(platform workerServicePlatform, workspace, address string) (workerServicePaths, error) {
	if err := requireWorkerServiceWorkspace(workspace); err != nil {
		return workerServicePaths{}, err
	}
	if platform.GOOS != "darwin" && platform.GOOS != "linux" {
		return workerServicePaths{}, fmt.Errorf("worker service management is unsupported on %s; use `conveyor worker run`", platform.GOOS)
	}
	if !filepath.IsAbs(platform.Executable) {
		return workerServicePaths{}, fmt.Errorf("resolved Conveyor executable must be absolute: %s", platform.Executable)
	}
	parsed, err := url.Parse(address)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return workerServicePaths{}, fmt.Errorf("CONVEYOR_ADDR must be an absolute URL without credentials, query, or fragment")
	}
	identity := workerServiceIdentity(workspace)
	if platform.GOOS == "darwin" {
		name := "com.conveyor.worker." + identity
		logDir := filepath.Join(platform.Home, "Library", "Logs", "Conveyor", "workers", identity)
		paths := workerServicePaths{
			Name:   name,
			Unit:   filepath.Join(platform.Home, "Library", "LaunchAgents", name+".plist"),
			Stdout: filepath.Join(logDir, "stdout.log"),
			Stderr: filepath.Join(logDir, "stderr.log"),
			Home:   platform.Home,
		}
		return paths, nil
	}
	name := "conveyor-worker-" + identity + ".service"
	logDir := filepath.Join(platform.StateDir, "conveyor", "workers", identity)
	paths := workerServicePaths{
		Name:      name,
		Unit:      filepath.Join(platform.ConfigDir, "systemd", "user", name),
		Stdout:    filepath.Join(logDir, "stdout.log"),
		Stderr:    filepath.Join(logDir, "stderr.log"),
		Home:      platform.Home,
		ConfigDir: platform.ConfigDir,
	}
	return paths, nil
}

func workerServiceDefinition(platform workerServicePlatform, paths workerServicePaths, workspace, address, configPath string) string {
	if platform.GOOS == "darwin" {
		return launchdWorkerDefinition(paths, workspace, address, platform.Executable, configPath)
	}
	return systemdWorkerDefinition(paths, workspace, address, platform.Executable, configPath)
}

func launchdWorkerDefinition(paths workerServicePaths, workspace, address, executable, configPath string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>` + xmlEscape(paths.Name) + `</string>
  <key>ProgramArguments</key>
  <array>
    <string>` + xmlEscape(executable) + `</string>
    <string>--workspace</string><string>` + xmlEscape(workspace) + `</string>
    <string>worker</string><string>run</string>
    <string>--config</string><string>` + xmlEscape(configPath) + `</string>
  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>CONVEYOR_ADDR</key><string>` + xmlEscape(address) + `</string>
    <key>HOME</key><string>` + xmlEscape(paths.Home) + `</string>
  </dict>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>
  <key>StandardOutPath</key><string>` + xmlEscape(paths.Stdout) + `</string>
  <key>StandardErrorPath</key><string>` + xmlEscape(paths.Stderr) + `</string>
  <key>ProcessType</key><string>Background</string>
  <key>ConveyorOwner</key><string>` + workerServiceOwner + `</string>
  <key>ConveyorWorkspace</key><string>` + xmlEscape(workspace) + `</string>
</dict>
</plist>
`
}

func systemdWorkerDefinition(paths workerServicePaths, workspace, address, executable, configPath string) string {
	return `[Unit]
Description=Conveyor worker for workspace ` + workspace + `
After=network-online.target

[Service]
Type=simple
ExecStart=` + serviceunit.QuoteArg(executable) + ` --workspace ` + serviceunit.QuoteArg(workspace) + ` worker run --config ` + serviceunit.QuoteArg(configPath) + `
Environment="CONVEYOR_ADDR=` + systemdEscape(address) + `"
Environment="HOME=` + systemdEscape(paths.Home) + `"
Environment="XDG_CONFIG_HOME=` + systemdEscape(paths.ConfigDir) + `"
Restart=on-failure
RestartSec=2
StandardOutput=append:` + serviceunit.DirectivePath(paths.Stdout) + `
StandardError=append:` + serviceunit.DirectivePath(paths.Stderr) + `
# ConveyorOwner=` + workerServiceOwner + `
# ConveyorWorkspace=` + workspace + `

[Install]
WantedBy=default.target
`
}

func xmlEscape(value string) string {
	replacer := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;")
	return replacer.Replace(value)
}

func systemdEscape(value string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, `%`, `%%`, `$`, `$$`).Replace(value)
}

func loadSavedWorkerCredential(workspace string) (workerCredentialFile, error) {
	path, err := credentialPath(workspace)
	if err != nil {
		return workerCredentialFile{}, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return workerCredentialFile{}, fmt.Errorf("worker is not enrolled for workspace %s; run `conveyor --workspace %s worker pair`, then `conveyor --workspace %s worker run --pairing-token <token>` before installing", workspace, workspace, workspace)
	}
	if err != nil {
		return workerCredentialFile{}, fmt.Errorf("read saved worker enrollment: %w", err)
	}
	var saved workerCredentialFile
	if err = json.Unmarshal(data, &saved); err != nil || saved.Workspace != workspace || saved.WorkerID == "" || saved.Credential == "" {
		return workerCredentialFile{}, fmt.Errorf("saved worker enrollment for workspace %s is invalid; pair the worker again before installing", workspace)
	}
	return saved, nil
}

func installWorkerService(ctx context.Context, c *client, platform workerServicePlatform, configPath string) (workerServicePaths, error) {
	workspace := c.workspace
	if err := requireWorkerServiceWorkspace(workspace); err != nil {
		return workerServicePaths{}, err
	}
	configPath = strings.TrimSpace(configPath)
	if configPath == "" {
		return workerServicePaths{}, fmt.Errorf("local execution config path is required")
	}
	configPath, err := filepath.Abs(configPath)
	if err != nil {
		return workerServicePaths{}, fmt.Errorf("resolve local execution config path: %w", err)
	}
	if _, err = loadLocalExecutionSetup(configPath); err != nil {
		return workerServicePaths{}, err
	}
	if _, err = loadSavedWorkerCredential(workspace); err != nil {
		return workerServicePaths{}, err
	}
	paths, err := resolveWorkerService(platform, workspace, c.base)
	if err != nil {
		return workerServicePaths{}, err
	}
	paths.Config = configPath
	paths.Definition = workerServiceDefinition(platform, paths, workspace, c.base, configPath)
	if err = ensureWorkerServiceOwnership(paths, workspace); err != nil {
		return workerServicePaths{}, err
	}
	for _, directory := range []string{filepath.Dir(paths.Unit), filepath.Dir(paths.Stdout)} {
		if err = os.MkdirAll(directory, 0o700); err != nil {
			return workerServicePaths{}, fmt.Errorf("create worker service directory: %w", err)
		}
		if err = os.Chmod(directory, 0o700); err != nil {
			return workerServicePaths{}, fmt.Errorf("secure worker service directory: %w", err)
		}
	}
	for _, logPath := range []string{paths.Stdout, paths.Stderr} {
		if err = ensureOwnerOnlyLog(logPath); err != nil {
			return workerServicePaths{}, err
		}
	}
	if err = writeOwnerOnlyFile(paths.Unit, []byte(paths.Definition)); err != nil {
		return workerServicePaths{}, fmt.Errorf("write worker service definition: %w", err)
	}
	if platform.GOOS == "darwin" {
		target := "gui/" + strconv.Itoa(platform.UID)
		if err = runWorkerServiceCommandAllowAbsent(ctx, platform, "launchctl", "bootout", target+"/"+paths.Name); err != nil {
			return workerServicePaths{}, err
		}
		if err = runWorkerServiceCommand(ctx, platform, "launchctl", "bootstrap", target, paths.Unit); err != nil {
			return workerServicePaths{}, err
		}
		if err = runWorkerServiceCommand(ctx, platform, "launchctl", "kickstart", "-k", target+"/"+paths.Name); err != nil {
			return workerServicePaths{}, err
		}
		return paths, nil
	}
	if err = runWorkerServiceCommand(ctx, platform, "systemctl", "--user", "daemon-reload"); err != nil {
		return workerServicePaths{}, err
	}
	if err = runWorkerServiceCommand(ctx, platform, "systemctl", "--user", "enable", "--now", paths.Name); err != nil {
		return workerServicePaths{}, err
	}
	return paths, nil
}

func uninstallWorkerService(ctx context.Context, c *client, platform workerServicePlatform) (workerServicePaths, bool, error) {
	paths, err := resolveWorkerService(platform, c.workspace, c.base)
	if err != nil {
		return workerServicePaths{}, false, err
	}
	data, readErr := os.ReadFile(paths.Unit)
	if errors.Is(readErr, os.ErrNotExist) {
		return paths, false, nil
	}
	if readErr != nil {
		return paths, false, fmt.Errorf("read worker service definition: %w", readErr)
	}
	if !isOwnedWorkerService(string(data), c.workspace) {
		return paths, false, fmt.Errorf("refusing to remove non-Conveyor or mismatched service definition %s", paths.Unit)
	}
	if platform.GOOS == "darwin" {
		target := "gui/" + strconv.Itoa(platform.UID) + "/" + paths.Name
		if err = runWorkerServiceCommandAllowAbsent(ctx, platform, "launchctl", "bootout", target); err != nil {
			return paths, false, err
		}
	} else {
		if err = runWorkerServiceCommandAllowAbsent(ctx, platform, "systemctl", "--user", "disable", "--now", paths.Name); err != nil {
			return paths, false, err
		}
	}
	if err = os.Remove(paths.Unit); err != nil && !errors.Is(err, os.ErrNotExist) {
		return paths, false, fmt.Errorf("remove worker service definition: %w", err)
	}
	if platform.GOOS == "linux" {
		if err = runWorkerServiceCommand(ctx, platform, "systemctl", "--user", "daemon-reload"); err != nil {
			return paths, false, err
		}
	}
	return paths, true, nil
}

func inspectWorkerService(ctx context.Context, c *client, platform workerServicePlatform) (workerServiceStatus, error) {
	paths, err := resolveWorkerService(platform, c.workspace, c.base)
	if err != nil {
		return workerServiceStatus{}, err
	}
	status := workerServiceStatus{
		Workspace:  c.workspace,
		Paths:      paths,
		Service:    workerLocalStatus{State: "stopped"},
		Remote:     workerRemoteStatus{State: "not_enrolled", Probes: []core.HarnessProbe{}},
		Enrollment: &workerEnrollmentState{},
	}
	saved, enrollmentErr := readSavedWorkerCredential(c.workspace)
	if enrollmentErr == nil {
		status.Enrollment.Present = true
		status.Enrollment.WorkerID = saved.WorkerID
	} else if !errors.Is(enrollmentErr, os.ErrNotExist) {
		status.Enrollment.Error = enrollmentErr.Error()
	}
	data, readErr := os.ReadFile(paths.Unit)
	switch {
	case errors.Is(readErr, os.ErrNotExist):
		status.Service.State = "stopped"
	case readErr != nil:
		return workerServiceStatus{}, fmt.Errorf("read worker service definition: %w", readErr)
	case !isOwnedWorkerService(string(data), c.workspace):
		return workerServiceStatus{}, fmt.Errorf("service definition at %s is not owned by Conveyor for workspace %s", paths.Unit, c.workspace)
	default:
		status.Service.Installed = true
		status.Service.State, status.Service.Detail = queryWorkerServiceState(ctx, platform, paths)
	}
	if enrollmentErr != nil {
		return status, nil
	}
	result, remoteErr := c.listWorkers()
	if remoteErr != nil {
		status.Remote.State = "unavailable"
		status.Remote.Error = remoteErr.Error()
		return status, nil
	}
	status.Remote.State = "not_seen"
	for _, worker := range result.Workers {
		if worker.ID != saved.WorkerID {
			continue
		}
		status.Remote.LastHeartbeatAt = worker.LastSeenAt
		status.Remote.LeaseExpiresAt = worker.LeaseExpiresAt
		status.Remote.Probes = append([]core.HarnessProbe(nil), worker.Probes...)
		switch {
		case !worker.RevokedAt.IsZero():
			status.Remote.State = "revoked"
		case worker.Live(time.Now()):
			status.Remote.State = "live"
		default:
			status.Remote.State = "stale"
		}
		break
	}
	return status, nil
}

func readSavedWorkerCredential(workspace string) (workerCredentialFile, error) {
	path, err := credentialPath(workspace)
	if err != nil {
		return workerCredentialFile{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return workerCredentialFile{}, err
	}
	var saved workerCredentialFile
	if err = json.Unmarshal(data, &saved); err != nil {
		return workerCredentialFile{}, err
	}
	if saved.Workspace != workspace || saved.WorkerID == "" || saved.Credential == "" {
		return workerCredentialFile{}, fmt.Errorf("invalid saved worker enrollment")
	}
	return saved, nil
}

func queryWorkerServiceState(ctx context.Context, platform workerServicePlatform, paths workerServicePaths) (string, string) {
	if platform.GOOS == "darwin" {
		output, err := platform.Run(ctx, "launchctl", "print", "gui/"+strconv.Itoa(platform.UID)+"/"+paths.Name)
		if err != nil {
			if workerServiceManagerStateAbsent(output) {
				return "stopped", strings.TrimSpace(output)
			}
			return "unknown", strings.TrimSpace(output)
		}
		lower := strings.ToLower(output)
		if strings.Contains(lower, "state = running") {
			return "running", ""
		}
		if strings.Contains(lower, "last exit code =") && !strings.Contains(lower, "last exit code = 0") {
			return "failed", strings.TrimSpace(output)
		}
		return "stopped", strings.TrimSpace(output)
	}
	output, err := platform.Run(ctx, "systemctl", "--user", "show", paths.Name, "--property=ActiveState,SubState,Result")
	if err != nil {
		if workerServiceManagerStateAbsent(output) {
			return "stopped", strings.TrimSpace(output)
		}
		return "unknown", strings.TrimSpace(output)
	}
	values := map[string]string{}
	for _, line := range strings.Split(output, "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok {
			values[key] = value
		}
	}
	if values["ActiveState"] == "active" {
		return "running", values["SubState"]
	}
	if values["ActiveState"] == "failed" || (values["Result"] != "" && values["Result"] != "success") {
		return "failed", values["Result"]
	}
	return "stopped", values["SubState"]
}

func ensureWorkerServiceOwnership(paths workerServicePaths, workspace string) error {
	data, err := os.ReadFile(paths.Unit)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read existing worker service definition: %w", err)
	}
	if !isOwnedWorkerService(string(data), workspace) {
		return fmt.Errorf("refusing to overwrite non-Conveyor or mismatched service definition %s", paths.Unit)
	}
	return nil
}

func isOwnedWorkerService(definition, workspace string) bool {
	return strings.Contains(definition, workerServiceOwner) &&
		(strings.Contains(definition, "ConveyorWorkspace</key><string>"+xmlEscape(workspace)+"</string>") ||
			strings.Contains(definition, "# ConveyorWorkspace="+workspace))
}

func writeOwnerOnlyFile(path string, data []byte) error {
	temp, err := os.CreateTemp(filepath.Dir(path), ".conveyor-worker-*")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err = temp.Chmod(0o600); err == nil {
		_, err = temp.Write(data)
	}
	if closeErr := temp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(tempPath, path)
}

func ensureOwnerOnlyLog(path string) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("create worker service log %s: %w", path, err)
	}
	if err = file.Chmod(0o600); err != nil {
		_ = file.Close()
		return fmt.Errorf("secure worker service log %s: %w", path, err)
	}
	if err = file.Close(); err != nil {
		return fmt.Errorf("close worker service log %s: %w", path, err)
	}
	return nil
}

func runWorkerServiceCommand(ctx context.Context, platform workerServicePlatform, name string, args ...string) error {
	output, err := platform.Run(ctx, name, args...)
	if err == nil {
		return nil
	}
	if output != "" {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, output)
	}
	return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
}

func runWorkerServiceCommandAllowAbsent(ctx context.Context, platform workerServicePlatform, name string, args ...string) error {
	output, err := platform.Run(ctx, name, args...)
	if err == nil {
		return nil
	}
	if workerServiceManagerStateAbsent(output) {
		return nil
	}
	if output != "" {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, output)
	}
	return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
}

func workerServiceManagerStateAbsent(output string) bool {
	lower := strings.ToLower(output)
	for _, marker := range []string{"no such process", "not loaded", "not-found", "not found", "does not exist", "could not find specified service"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}
