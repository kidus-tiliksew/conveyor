package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/releaseinfo"
)

const daemonServiceOwner = "conveyord-service-v1"

type daemonServicePlatform struct {
	GOOS, Home, ConfigDir, StateDir, Executable string
	UID                                         int
	Run                                         func(context.Context, string, ...string) (string, error)
}

type daemonServicePaths struct {
	Name, Unit, Stdout, Stderr, Config, Definition string
}

type daemonServiceStatus struct {
	Installed bool   `json:"installed"`
	State     string `json:"state"`
	Unit      string `json:"unit_path"`
	Stdout    string `json:"stdout_log"`
	Stderr    string `json:"stderr_log"`
}

func runServiceVerb(ctx context.Context, args []string, stdout, stderr io.Writer) (bool, error) {
	if len(args) == 0 {
		return false, nil
	}
	if args[0] == "version" || args[0] == "--version" || args[0] == "-version" {
		fmt.Fprintf(stdout, "conveyord %s\n", releaseinfo.Version)
		return true, nil
	}
	if args[0] != "install" && args[0] != "uninstall" && args[0] != "status" {
		return false, nil
	}
	flags := flag.NewFlagSet("conveyord "+args[0], flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", "conveyor.yaml", "path to deployment config")
	if err := flags.Parse(args[1:]); err != nil {
		return true, err
	}
	if flags.NArg() != 0 {
		return true, fmt.Errorf("conveyord %s accepts no positional arguments", args[0])
	}
	platform, err := currentDaemonServicePlatform()
	if err != nil {
		return true, err
	}
	absConfig, err := filepath.Abs(*configPath)
	if err != nil {
		return true, fmt.Errorf("resolve config path: %w", err)
	}
	paths, err := resolveDaemonService(platform, absConfig)
	if err != nil {
		return true, err
	}
	switch args[0] {
	case "install":
		if _, err = os.Stat(absConfig); err != nil {
			return true, fmt.Errorf("deployment config is unavailable at %s: %w", absConfig, err)
		}
		if err = installDaemonService(ctx, platform, paths); err != nil {
			return true, err
		}
		fmt.Fprintf(stdout, "installed conveyord service\nunit=%s\nstdout_log=%s\nstderr_log=%s\n", paths.Unit, paths.Stdout, paths.Stderr)
	case "uninstall":
		removed, removeErr := uninstallDaemonService(ctx, platform, paths)
		if removeErr != nil {
			return true, removeErr
		}
		if removed {
			fmt.Fprintf(stdout, "uninstalled conveyord service\nunit=%s\n", paths.Unit)
		} else {
			fmt.Fprintf(stdout, "conveyord service is already uninstalled\nunit=%s\n", paths.Unit)
		}
	case "status":
		status, statusErr := inspectDaemonService(ctx, platform, paths)
		if statusErr != nil {
			return true, statusErr
		}
		data, _ := json.MarshalIndent(status, "", "  ")
		fmt.Fprintln(stdout, string(data))
	}
	return true, nil
}

func currentDaemonServicePlatform() (daemonServicePlatform, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return daemonServicePlatform{}, err
	}
	configDir, err := os.UserConfigDir()
	if err != nil {
		return daemonServicePlatform{}, err
	}
	executable, err := os.Executable()
	if err != nil {
		return daemonServicePlatform{}, err
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return daemonServicePlatform{}, err
	}
	stateDir := strings.TrimSpace(os.Getenv("XDG_STATE_HOME"))
	if stateDir == "" {
		stateDir = filepath.Join(home, ".local", "state")
	}
	return daemonServicePlatform{GOOS: runtime.GOOS, Home: home, ConfigDir: configDir, StateDir: stateDir, Executable: executable, UID: os.Getuid(), Run: func(ctx context.Context, name string, args ...string) (string, error) {
		output, runErr := exec.CommandContext(ctx, name, args...).CombinedOutput()
		return strings.TrimSpace(string(output)), runErr
	}}, nil
}

func resolveDaemonService(platform daemonServicePlatform, configPath string) (daemonServicePaths, error) {
	if platform.GOOS != "darwin" && platform.GOOS != "linux" {
		return daemonServicePaths{}, fmt.Errorf("conveyord service management is unsupported on %s", platform.GOOS)
	}
	if !filepath.IsAbs(platform.Executable) || !filepath.IsAbs(configPath) {
		return daemonServicePaths{}, errors.New("conveyord executable and config paths must be absolute")
	}
	if platform.GOOS == "darwin" {
		name := "com.conveyor.daemon"
		logDir := filepath.Join(platform.Home, "Library", "Logs", "Conveyor", "daemon")
		paths := daemonServicePaths{Name: name, Unit: filepath.Join(platform.Home, "Library", "LaunchAgents", name+".plist"), Stdout: filepath.Join(logDir, "stdout.log"), Stderr: filepath.Join(logDir, "stderr.log"), Config: configPath}
		paths.Definition = fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!-- %s config=%s -->
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict><key>Label</key><string>%s</string><key>ProgramArguments</key><array><string>%s</string><string>-config</string><string>%s</string></array><key>WorkingDirectory</key><string>%s</string><key>RunAtLoad</key><true/><key>KeepAlive</key><true/><key>StandardOutPath</key><string>%s</string><key>StandardErrorPath</key><string>%s</string></dict></plist>
`, daemonServiceOwner, xmlCommentEscape(configPath), html.EscapeString(name), html.EscapeString(platform.Executable), html.EscapeString(configPath), html.EscapeString(filepath.Dir(configPath)), html.EscapeString(paths.Stdout), html.EscapeString(paths.Stderr))
		return paths, nil
	}
	name := "conveyord.service"
	logDir := filepath.Join(platform.StateDir, "conveyor", "daemon")
	paths := daemonServicePaths{Name: name, Unit: filepath.Join(platform.ConfigDir, "systemd", "user", name), Stdout: filepath.Join(logDir, "stdout.log"), Stderr: filepath.Join(logDir, "stderr.log"), Config: configPath}
	paths.Definition = fmt.Sprintf("# %s config=%s\n[Unit]\nDescription=Conveyor control-plane daemon\nAfter=network-online.target\n\n[Service]\nWorkingDirectory=%s\nExecStart=%s -config %s\nRestart=on-failure\nStandardOutput=append:%s\nStandardError=append:%s\n\n[Install]\nWantedBy=default.target\n", daemonServiceOwner, xmlCommentEscape(configPath), strconv.Quote(filepath.Dir(configPath)), strconv.Quote(platform.Executable), strconv.Quote(configPath), strconv.Quote(paths.Stdout), strconv.Quote(paths.Stderr))
	return paths, nil
}

func isOwnedDaemonService(definition, configPath string) bool {
	return strings.Contains(definition, daemonServiceOwner+" config="+xmlCommentEscape(configPath))
}

func xmlCommentEscape(value string) string {
	return strings.ReplaceAll(html.EscapeString(value), "--", "&#45;&#45;")
}

func installDaemonService(ctx context.Context, platform daemonServicePlatform, paths daemonServicePaths) error {
	if data, err := os.ReadFile(paths.Unit); err == nil && !isOwnedDaemonService(string(data), paths.Config) {
		return fmt.Errorf("refusing to overwrite non-Conveyor service definition %s", paths.Unit)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect service definition: %w", err)
	}
	for _, directory := range []string{filepath.Dir(paths.Unit), filepath.Dir(paths.Stdout)} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return err
		}
		if err := os.Chmod(directory, 0o700); err != nil {
			return err
		}
	}
	for _, path := range []string{paths.Stdout, paths.Stderr} {
		file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		if err = file.Close(); err != nil {
			return err
		}
		if err = os.Chmod(path, 0o600); err != nil {
			return err
		}
	}
	if err := os.WriteFile(paths.Unit, []byte(paths.Definition), 0o600); err != nil {
		return err
	}
	if err := os.Chmod(paths.Unit, 0o600); err != nil {
		return err
	}
	if platform.GOOS == "darwin" {
		target := "gui/" + strconv.Itoa(platform.UID)
		_, _ = platform.Run(ctx, "launchctl", "bootout", target+"/"+paths.Name)
		if output, err := platform.Run(ctx, "launchctl", "bootstrap", target, paths.Unit); err != nil {
			return fmt.Errorf("launchctl bootstrap: %w: %s", err, output)
		}
		if output, err := platform.Run(ctx, "launchctl", "kickstart", "-k", target+"/"+paths.Name); err != nil {
			return fmt.Errorf("launchctl kickstart: %w: %s", err, output)
		}
		return nil
	}
	if output, err := platform.Run(ctx, "systemctl", "--user", "daemon-reload"); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %w: %s", err, output)
	}
	if output, err := platform.Run(ctx, "systemctl", "--user", "enable", "--now", paths.Name); err != nil {
		return fmt.Errorf("systemctl enable: %w: %s", err, output)
	}
	return nil
}

func uninstallDaemonService(ctx context.Context, platform daemonServicePlatform, paths daemonServicePaths) (bool, error) {
	data, err := os.ReadFile(paths.Unit)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !isOwnedDaemonService(string(data), paths.Config) {
		return false, fmt.Errorf("refusing to remove non-Conveyor service definition %s", paths.Unit)
	}
	if platform.GOOS == "darwin" {
		_, _ = platform.Run(ctx, "launchctl", "bootout", "gui/"+strconv.Itoa(platform.UID)+"/"+paths.Name)
	} else {
		_, _ = platform.Run(ctx, "systemctl", "--user", "disable", "--now", paths.Name)
	}
	if err = os.Remove(paths.Unit); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if platform.GOOS == "linux" {
		if output, reloadErr := platform.Run(ctx, "systemctl", "--user", "daemon-reload"); reloadErr != nil {
			return false, fmt.Errorf("systemctl daemon-reload: %w: %s", reloadErr, output)
		}
	}
	return true, nil
}

func inspectDaemonService(ctx context.Context, platform daemonServicePlatform, paths daemonServicePaths) (daemonServiceStatus, error) {
	status := daemonServiceStatus{State: "not_installed", Unit: paths.Unit, Stdout: paths.Stdout, Stderr: paths.Stderr}
	data, err := os.ReadFile(paths.Unit)
	if errors.Is(err, os.ErrNotExist) {
		return status, nil
	}
	if err != nil {
		return status, err
	}
	if !isOwnedDaemonService(string(data), paths.Config) {
		return status, fmt.Errorf("service definition at %s is not owned by Conveyor", paths.Unit)
	}
	status.Installed = true
	status.State = "stopped"
	if platform.GOOS == "darwin" {
		if _, err = platform.Run(ctx, "launchctl", "print", "gui/"+strconv.Itoa(platform.UID)+"/"+paths.Name); err == nil {
			status.State = "running"
		}
	} else if output, runErr := platform.Run(ctx, "systemctl", "--user", "is-active", paths.Name); runErr == nil && strings.TrimSpace(output) == "active" {
		status.State = "running"
	}
	return status, nil
}
