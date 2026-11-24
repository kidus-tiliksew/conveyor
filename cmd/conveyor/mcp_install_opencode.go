package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	// openCodeInitArg selects the internal helper that prepares the
	// namespaces and then becomes the OpenCode command. Only
	// openCodeContained starts it.
	openCodeInitArg = "__opencode-check"
	// The helper reports on this descriptor. It sends openCodeReady after the
	// setup and immediately before exec, and openCodeExecFailed after a failed
	// exec. The descriptor closes on exec, so the client never holds it.
	openCodeStatusFD   = 3
	openCodeReady      = 'r'
	openCodeExecFailed = 'x'

	openCodeCheckTimeout    = 30 * time.Second
	openCodeDrainBudget     = 5 * time.Second
	openCodeVersionLimit    = 4 << 10
	openCodeDocumentLimit   = 4 << 20
	openCodePlaceholder     = "conveyor-install-validation"
	openCodeConfigCommand   = "opencode debug config"
	openCodeConfigCommandV2 = "opencode debug config --standalone"
)

// Each refusal of the OpenCode check is one fixed message. None holds client
// output, a resolved value, or a decoder error, because client output can
// hold resolved values (component-cli-onboarding invariants 6 and 11).
var (
	errOpenCodeContainment = errors.New("the OpenCode check needs Linux with user, PID, and network namespaces available to this user. Run the install on a Linux host that has them")
	errOpenCodeLaunch      = errors.New("the installed opencode executable did not start. Check the OpenCode installation, then run the install again")
	errOpenCodeTimeout     = errors.New("the OpenCode check timed out. Check that `opencode --version` and `opencode debug config` finish, then run the install again")
	errOpenCodeOutputSize  = errors.New("the OpenCode check refused an output that is larger than its limit")
	errOpenCodeCleanup     = errors.New("the OpenCode check did not finish its cleanup. Run the install again")
	errOpenCodeVersion     = errors.New("the installer did not read a supported OpenCode version from `opencode --version`. Install an OpenCode 1 or OpenCode 2 release, then run the install again")
	errOpenCodeFailed      = errors.New(openCodeConfigCommand + " failed. Check the OpenCode installation and configuration, then run the install again")
	errOpenCodeInvalid     = errors.New(openCodeConfigCommand + " reported an invalid configuration. Correct the OpenCode configuration, then run the install again")
	errOpenCodeV2Failed    = errors.New(openCodeConfigCommandV2 + " failed. Check the OpenCode installation and configuration, then run the install again")
	errOpenCodeServerStart = errors.New(openCodeConfigCommandV2 + " did not start its private server. Check the OpenCode installation, then run the install again")
	errOpenCodeMalformed   = errors.New(openCodeConfigCommandV2 + " printed a result that the installer does not recognize. Install a supported OpenCode 2 release, then run the install again")
	errOpenCodeNoDocument  = errors.New(openCodeConfigCommandV2 + " did not read the staged file. Check that the OpenCode configuration file is valid JSON, then run the install again")
	errOpenCodeAmbiguous   = errors.New(openCodeConfigCommandV2 + " reported the staged file more than one time. Install a supported OpenCode 2 release, then run the install again")
	errOpenCodeNoEntry     = errors.New(openCodeConfigCommandV2 + " did not report the registration. OpenCode ignored it, so check the `mcp` section of the OpenCode configuration, then run the install again")
	errOpenCodeMismatch    = errors.New(openCodeConfigCommandV2 + " reported a registration with a different type, endpoint, or OAuth setting. Check the `mcp` section of the OpenCode configuration, then run the install again")
)

// openCodeContain builds the command that runs the client in containment. It
// refuses before a client starts when the platform has none. Tests replace it
// where a parser test must not depend on host namespaces.
var openCodeContain = openCodeContained

// validateOpenCodeMCP checks a copy of the staged file with the installed
// OpenCode and returns the command that accepted it. It validates a copy
// because OpenCode can add $schema while it loads a file. Only the original
// staged bytes can be published. The isolation keeps existing configuration
// and stored accounts out of the check (component-cli-onboarding invariant
// 11; req-mcp-oauth AC-5.3).
func validateOpenCodeMCP(binary, staged, name string, identity mcpIdentity, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return validateOpenCodeMCPContext(ctx, binary, staged, name, identity)
}

func validateOpenCodeMCPContext(ctx context.Context, binary, staged, name string, identity mcpIdentity) (command string, err error) {
	content, err := os.ReadFile(staged)
	if err != nil {
		return "", err
	}
	// The client runs in another directory. A shim can depend on its own
	// path, so make the path absolute and keep its symlinks.
	if binary, err = filepath.Abs(binary); err != nil {
		return "", err
	}
	isolated, err := os.MkdirTemp(filepath.Dir(staged), ".conveyor-opencode-*")
	if err != nil {
		return "", err
	}
	defer func() {
		if os.RemoveAll(isolated) != nil && err == nil {
			command, err = "", errOpenCodeCleanup
		}
	}()
	// Keep file references relative to the real config directory.
	copied, err := os.CreateTemp(filepath.Dir(staged), ".conveyor-opencode-*.json")
	if err != nil {
		return "", err
	}
	configPath := copied.Name()
	defer func() {
		if removeErr := os.Remove(configPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) && err == nil {
			command, err = "", errOpenCodeCleanup
		}
	}()
	_, err = copied.Write(content)
	if closeErr := copied.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return "", err
	}
	// Send the known native log to the null device. The directory that holds
	// any other log is removed after the check.
	logs := filepath.Join(isolated, "data", "opencode", "log")
	if err = os.MkdirAll(logs, 0o700); err != nil {
		return "", err
	}
	if err = os.Symlink(os.DevNull, filepath.Join(logs, "opencode.log")); err != nil {
		return "", err
	}
	run := openCodeRun{binary: binary, dir: isolated, env: openCodeEnvironment(isolated, configPath, content)}

	run.args, run.limit = []string{"--version"}, openCodeVersionLimit
	result, err := run.execute(ctx)
	if err != nil {
		return "", err
	}
	major := ""
	if !result.failed {
		major = openCodeMajor(result.stdout)
	}
	switch major {
	case "1":
		run.args, run.limit = []string{"debug", "config"}, 0
		if result, err = run.execute(ctx); err != nil {
			return "", err
		}
		if result.stderr.invalid {
			return "", errOpenCodeInvalid
		}
		if result.failed {
			return "", errOpenCodeFailed
		}
		return openCodeConfigCommand, nil
	case "2":
		// The command without --standalone uses the background service of
		// the user. It is never a fallback.
		run.args, run.limit = []string{"debug", "config", "--standalone"}, openCodeDocumentLimit
		if result, err = run.execute(ctx); err != nil {
			return "", err
		}
		if result.failed {
			if result.stderr.serverStart {
				return "", errOpenCodeServerStart
			}
			return "", errOpenCodeV2Failed
		}
		if err = checkOpenCodeDocument(result.stdout, configPath, name, identity); err != nil {
			return "", err
		}
		return openCodeConfigCommandV2, nil
	}
	return "", errOpenCodeVersion
}

// openCodeVersionLine is one semantic version. OpenCode 1 prints the bare
// version, and OpenCode 2 prints `opencode v<version>`.
var openCodeVersionLine = regexp.MustCompile(`^(?:opencode )?v?(0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)` +
	`(?:-(?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(?:\.(?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*)?` +
	`(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?\r?\n?$`)

// openCodeMajor returns the major version, or "" when the output is not
// exactly one version line.
func openCodeMajor(output []byte) string {
	match := openCodeVersionLine.FindSubmatch(output)
	if match == nil {
		return ""
	}
	return string(match[1])
}

var openCodeEnvReference = regexp.MustCompile(`\{env:([A-Za-z_][A-Za-z0-9_]*)\}`)

// openCodeEnvironment isolates the client and gives each environment
// reference in the staged file a placeholder value. A reference never
// replaces an isolation variable or a loader setting.
func openCodeEnvironment(isolated, configPath string, content []byte) []string {
	var environment []string
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		if strings.HasPrefix(key, "OPENCODE_") || strings.HasPrefix(key, "XDG_") || key == "HOME" || key == "PWD" || key == "OLDPWD" || key == mcpAddressEnv || key == mcpTokenEnv || strings.HasPrefix(key, "CONVEYOR_MCP_TOKEN_") {
			continue
		}
		environment = append(environment, item)
	}
	environment = append(environment,
		"HOME="+isolated,
		"PWD="+isolated,
		"XDG_CONFIG_HOME="+filepath.Join(isolated, "config"),
		"XDG_DATA_HOME="+filepath.Join(isolated, "data"),
		"XDG_STATE_HOME="+filepath.Join(isolated, "state"),
		"XDG_CACHE_HOME="+filepath.Join(isolated, "cache"),
		"OPENCODE_CONFIG="+configPath,
		"OPENCODE_DISABLE_PROJECT_CONFIG=true",
		mcpAddressEnv+"=https://conveyor.invalid/mcp",
		mcpTokenEnv+"="+openCodePlaceholder,
	)
	for _, match := range openCodeEnvReference.FindAllSubmatch(content, -1) {
		if name := string(match[1]); !openCodeReservedEnv(name) {
			environment = append(environment, name+"="+openCodePlaceholder)
		}
	}
	return environment
}

// openCodeReservedEnv reports whether a staged reference must not set the
// variable. A name with a credential suffix is a value, not a setting.
func openCodeReservedEnv(name string) bool {
	switch name {
	case "HOME", "PWD", "OLDPWD", "PATH", "SHELL", "ENV", "BASH_ENV", "IFS", "TMPDIR", "TMP", "TEMP", "GODEBUG", "GOTRACEBACK", "GCONV_PATH", "LOCPATH", mcpAddressEnv:
		return true
	}
	for _, suffix := range []string{"_KEY", "_TOKEN", "_SECRET"} {
		if strings.HasSuffix(name, suffix) {
			return false
		}
	}
	for _, prefix := range []string{"XDG_", "OPENCODE_", "LD_", "DYLD_", "GLIBC_", "NODE_", "BUN_"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// openCodeRun is one client command. A limit of 0 discards standard output.
type openCodeRun struct {
	binary string
	args   []string
	dir    string
	env    []string
	limit  int
}

type openCodeResult struct {
	stdout []byte
	stderr openCodeValidationStderr
	failed bool
}

// execute runs the command and returns only after the kernel reaped its
// direct child. In containment that child is the init of a PID namespace, so
// the reap also ends each descendant, including one in a new session. A
// deadline or a closed pipe alone proves no cleanup, so a teardown that is
// slow in the kernel delays the return. Each pipe belongs to this function
// and no drain is part of the child wait, so a descendant that holds a pipe
// cannot delay the reap.
func (run openCodeRun) execute(ctx context.Context) (result openCodeResult, err error) {
	var readers, writers [3]*os.File
	closeAll := func(files []*os.File) {
		for _, file := range files {
			if file != nil {
				file.Close()
			}
		}
	}
	for index := range readers {
		if readers[index], writers[index], err = os.Pipe(); err != nil {
			closeAll(readers[:])
			closeAll(writers[:])
			return result, err
		}
	}
	defer closeAll(readers[:])
	cmd, err := openCodeContain(run.binary, run.args, writers[2])
	if err == nil && ctx.Err() != nil {
		err = errOpenCodeTimeout
	}
	if err != nil {
		closeAll(writers[:])
		return result, err
	}
	// A nil Stdin reads from os.DevNull.
	cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = run.dir, run.env, writers[0], writers[1]
	// The kernel ends the contained init when the thread that started it
	// ends, so keep that thread until the reap.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	err = cmd.Start()
	closeAll(writers[:])
	if err != nil {
		return result, errOpenCodeContainment
	}

	overflow := make(chan struct{})
	var status bytes.Buffer
	var drainErrs [3]error
	var drains sync.WaitGroup
	drains.Add(3)
	go func() {
		defer drains.Done()
		buffer := make([]byte, 32<<10)
		full := false
		for {
			n, readErr := readers[0].Read(buffer)
			if run.limit > 0 && !full {
				if len(result.stdout)+n > run.limit {
					full = true
					result.stdout = nil
					close(overflow)
				} else {
					result.stdout = append(result.stdout, buffer[:n]...)
				}
			}
			if readErr != nil {
				if readErr != io.EOF {
					drainErrs[0] = readErr
				}
				return
			}
		}
	}()
	go func() {
		defer drains.Done()
		_, drainErrs[1] = io.Copy(&result.stderr, readers[1])
	}()
	go func() {
		defer drains.Done()
		_, drainErrs[2] = io.Copy(&status, io.LimitReader(readers[2], 2))
	}()
	drained := make(chan struct{})
	go func() { drains.Wait(); close(drained) }()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	var waitErr error
	var timedOut, tooLarge bool
	select {
	case waitErr = <-done:
	case <-ctx.Done():
		timedOut = true
	case <-overflow:
		tooLarge = true
	}
	if timedOut || tooLarge {
		// This handle names the owned child only. It stays valid until the
		// wait, so it never signals a reused PID.
		_ = cmd.Process.Kill()
		waitErr = <-done
	}
	timer := time.NewTimer(openCodeDrainBudget)
	defer timer.Stop()
	select {
	case <-drained:
	case <-timer.C:
		closeAll(readers[:])
		<-drained
		return openCodeResult{}, errOpenCodeCleanup
	}
	for _, drainErr := range drainErrs {
		if drainErr != nil {
			return openCodeResult{}, errOpenCodeCleanup
		}
	}
	// The client can also exit by itself after it passes the limit.
	select {
	case <-overflow:
		tooLarge = true
	default:
	}
	switch {
	case tooLarge:
		return openCodeResult{}, errOpenCodeOutputSize
	case timedOut:
		return openCodeResult{}, errOpenCodeTimeout
	case status.String() == string([]byte{openCodeReady, openCodeExecFailed}):
		return openCodeResult{}, errOpenCodeLaunch
	case status.String() != string([]byte{openCodeReady}):
		// Without the ready report no client started.
		return openCodeResult{}, errOpenCodeContainment
	}
	var exit *exec.ExitError
	if waitErr != nil && !errors.As(waitErr, &exit) {
		return openCodeResult{}, errOpenCodeCleanup
	}
	result.failed = waitErr != nil
	return result, nil
}

// Retain only enough stderr to recognize markers split across writes. Never
// include debug output in diagnostics, since it can contain resolved values.
type openCodeValidationStderr struct {
	tail        string
	invalid     bool
	serverStart bool
}

func (w *openCodeValidationStderr) Write(p []byte) (int, error) {
	text := w.tail + string(p)
	w.invalid = w.invalid || strings.Contains(text, "Unrecognized key") || strings.Contains(text, "ConfigInvalid")
	w.serverStart = w.serverStart || strings.Contains(text, "Server process exited") || strings.Contains(text, "Managed service port")
	if len(text) > 32 {
		text = text[len(text)-32:]
	}
	w.tail = text
	return len(p), nil
}

// checkOpenCodeDocument accepts the output of `opencode debug config
// --standalone` only when the row of the staged copy holds the registration.
// OpenCode 2 exits with status 0 for an entry that it drops, so the exit
// status proves nothing. Other rows, unknown keys that OpenCode dropped, and
// header values are not read (req-mcp-oauth AC-5.1, AC-5.2, AC-5.3).
func checkOpenCodeDocument(output []byte, configPath, name string, identity mcpIdentity) error {
	var rows []json.RawMessage
	decoder := json.NewDecoder(bytes.NewReader(output))
	if err := decoder.Decode(&rows); err != nil || rows == nil {
		return errOpenCodeMalformed
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errOpenCodeMalformed
	}
	want := canonicalOpenCodeMCPPath(configPath)
	var document map[string]json.RawMessage
	for _, raw := range rows {
		row, err := openCodeObject(raw)
		if err != nil {
			return errOpenCodeMalformed
		}
		path, ok := openCodeString(row["path"])
		if kind, _ := openCodeString(row["type"]); kind != "document" || !ok || !filepath.IsAbs(path) || canonicalOpenCodeMCPPath(path) != want {
			continue
		}
		if document != nil {
			return errOpenCodeAmbiguous
		}
		document = row
	}
	if document == nil {
		return errOpenCodeNoDocument
	}
	object := document
	for _, key := range []string{"info", "mcp", "servers"} {
		raw, ok := object[key]
		if !ok {
			return errOpenCodeNoEntry
		}
		var err error
		if object, err = openCodeObject(raw); err != nil {
			return errOpenCodeMalformed
		}
	}
	raw, ok := object[name]
	if !ok {
		return errOpenCodeNoEntry
	}
	entry, err := openCodeObject(raw)
	if err != nil {
		return errOpenCodeMismatch
	}
	kind, _ := openCodeString(entry["type"])
	url, _ := openCodeString(entry["url"])
	oauth, hasOAuth := entry["oauth"]
	if kind != "remote" || url != identity.endpoint {
		return errOpenCodeMismatch
	}
	if identity.oauth() == hasOAuth || (hasOAuth && string(oauth) != "false") {
		return errOpenCodeMismatch
	}
	return nil
}

// openCodeObject returns the members of a JSON object. A value of another
// type and a duplicate key are errors.
func openCodeObject(raw json.RawMessage) (map[string]json.RawMessage, error) {
	scanned, err := scanJSONObject(raw)
	if err != nil {
		return nil, err
	}
	members := make(map[string]json.RawMessage, len(scanned.members))
	for _, member := range scanned.members {
		members[member.key] = raw[member.valueStart:member.valueEnd]
	}
	return members, nil
}

func openCodeString(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || raw[0] != '"' {
		return "", false
	}
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return "", false
	}
	return value, true
}
