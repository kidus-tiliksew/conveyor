package main

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/redact"
	"github.com/kidus-tiliksew/conveyor/internal/verification"
)

// kitDefaultSearchPaths is the minimal default executable search path
// (feature-verification-kit-execution VK-4.2).
var kitDefaultSearchPaths = []string{"/usr/local/bin", "/usr/bin", "/bin"}

// kitRunnerEnvironmentKeys belong to the runner's operation channel, attempt
// directory and loopback UI transport. Neither a toolchain record nor approved
// input or credential fields may supply them.
var kitRunnerEnvironmentKeys = []string{"CONVEYOR_KIT_OPERATIONS", "CONVEYOR_KIT_ATTEMPT_DIR", "CONVEYOR_KIT_UI_HOST", "CONVEYOR_KIT_UI_PORT"}

// kitToolchain is the immutable toolchain snapshot resolved once per subject.
// Executable lookup, preflight, provenance and launch all read this value
// (feature-verification-kit-execution VK-4.2; component-harness-execution
// VK-EXEC-3).
type kitToolchain struct {
	configured                    bool
	searchPaths                   []string
	home                          string
	settings                      map[string]string
	configPath                    string
	server, workspace, repository string
}

type kitResolvedTool struct {
	role, label, name, path, digest string
}

// kitToolResolution holds the executable identities chosen by preflight. Launch
// and the post-execution check compare against these exact paths and digests.
type kitToolResolution struct {
	entrypoint    *kitResolvedTool
	prerequisites []kitResolvedTool
	ui            *kitResolvedTool
}

// kitPreflightError is a predictable refusal before any attempt exists. Its
// message is sanitized and names the remedy.
type kitPreflightError struct{ message string }

func (e *kitPreflightError) Error() string { return e.message }

func kitDefaultToolchain() kitToolchain {
	return kitToolchain{searchPaths: append([]string(nil), kitDefaultSearchPaths...), settings: map[string]string{}}
}

// resolveKitToolchain selects the scoped record or the minimal default. A
// configured value equal to a credential value is refused without echo.
func resolveKitToolchain(cfg *config.Config, configPath, server, workspace, repository string, secrets []string) (kitToolchain, error) {
	t := kitDefaultToolchain()
	t.configPath, t.server, t.workspace, t.repository = configPath, server, workspace, repository
	if cfg == nil {
		return t, nil
	}
	record, err := cfg.VerificationToolchainFor(server, workspace, repository)
	if err != nil {
		return kitToolchain{}, err
	}
	if record == nil {
		return t, nil
	}
	t.configured, t.searchPaths, t.home, t.settings = true, record.SearchPaths, record.Home, record.Settings
	values := map[string]string{"home": t.home}
	for i, path := range t.searchPaths {
		values[fmt.Sprintf("search_paths[%d]", i)] = path
	}
	for key, value := range t.settings {
		values["settings."+key] = value
	}
	for field, value := range values {
		for _, secret := range secrets {
			if value != "" && secret != "" && (value == secret || (len(secret) >= 8 && strings.Contains(value, secret))) {
				return kitToolchain{}, fmt.Errorf("verification_toolchains %s aliases a credential value; remove it from %s", field, t.configPath)
			}
		}
	}
	return t, nil
}

// kitApprovedSecrets returns every approved kit credential value on this host,
// whether or not the current subject uses it.
func kitApprovedSecrets() []string {
	var secrets []string
	for _, entry := range os.Environ() {
		name, value, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "CONVEYOR_KIT_SECRET_") && value != "" {
			secrets = append(secrets, value)
		}
	}
	return secrets
}

func (t kitToolchain) scope() string {
	if t.configured {
		return "configured"
	}
	return "default"
}

func (t kitToolchain) searchPath() string {
	return strings.Join(t.searchPaths, string(os.PathListSeparator))
}

// environment materializes the toolchain fields for one attempt directory. An
// omitted home keeps the attempt-private HOME; explicit settings pass unchanged.
func (t kitToolchain) environment(attemptDir string) []string {
	home := t.home
	if home == "" {
		home = attemptDir
	}
	env := []string{"PATH=" + t.searchPath(), "LANG=C.UTF-8", "HOME=" + home, "TMPDIR=" + attemptDir}
	keys := make([]string, 0, len(t.settings))
	for key := range t.settings {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		env = append(env, key+"="+t.settings[key])
	}
	return env
}

// checkEnvironmentKeys refuses approved input, credential or binding fields
// that would collide with a toolchain or runner-owned key.
func (t kitToolchain) checkEnvironmentKeys(env []string) error {
	reserved := map[string]bool{}
	for _, key := range kitRunnerEnvironmentKeys {
		reserved[key] = true
	}
	for _, entry := range t.environment(string(filepath.Separator)) {
		key, _, _ := strings.Cut(entry, "=")
		reserved[key] = true
	}
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		if reserved[key] {
			return fmt.Errorf("child environment key %s is reserved for the runner or toolchain", key)
		}
	}
	_, err := kitMergeEnvironment(env)
	return err
}

// kitMergeEnvironment builds a unique-key child environment. Identical
// duplicates collapse; conflicting values refuse instead of relying on
// duplicate-key ordering.
func kitMergeEnvironment(groups ...[]string) ([]string, error) {
	values := map[string]string{}
	for _, group := range groups {
		for _, entry := range group {
			key, value, ok := strings.Cut(entry, "=")
			if !ok || key == "" {
				return nil, fmt.Errorf("malformed child environment entry")
			}
			if previous, seen := values[key]; seen && previous != value {
				return nil, fmt.Errorf("child environment key %s is supplied twice", key)
			}
			values[key] = value
		}
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	env := make([]string, 0, len(keys))
	for _, key := range keys {
		env = append(env, key+"="+values[key])
	}
	return env, nil
}

func kitEnvironmentKeys(env []string) string {
	keys := make([]string, 0, len(env))
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

// lookPath resolves a bare name through the snapshot's ordered search path and
// a name containing a separator against cwd. The result is the symlink-resolved
// regular file with an execute bit.
func (t kitToolchain) lookPath(name, cwd string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("executable is unavailable in the approved search path")
	}
	candidates := []string{}
	if strings.ContainsRune(name, filepath.Separator) {
		if !filepath.IsAbs(name) {
			name = filepath.Join(cwd, name)
		}
		candidates = append(candidates, name)
	} else {
		for _, dir := range t.searchPaths {
			candidates = append(candidates, filepath.Join(dir, name))
		}
	}
	for _, candidate := range candidates {
		info, err := os.Stat(candidate)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
			continue
		}
		return filepath.EvalSymlinks(candidate)
	}
	return "", fmt.Errorf("executable is unavailable in the approved search path")
}

var kitSafeToolName = regexp.MustCompile(`^[A-Za-z0-9._+-]{1,128}$`)

// kitDisplayName names a tool in a diagnostic only when it is a plain name
// that the redactor leaves unchanged; argv paths and values are never echoed.
func kitDisplayName(name string, redactor *redact.Redactor) string {
	if !kitSafeToolName.MatchString(name) {
		return ""
	}
	if redactor != nil {
		if clean, _ := redactor.Redact(name); clean != name {
			return ""
		}
	}
	return " " + name
}

func (t kitToolchain) remedy() string {
	scope := fmt.Sprintf("server %s workspace %s repository %s", t.server, t.workspace, t.repository)
	if t.configured {
		return fmt.Sprintf("correct the verification_toolchains record for %s in %s", scope, t.configPath)
	}
	return fmt.Sprintf("add a verification_toolchains record for %s with the tool's directory in search_paths to %s", scope, t.configPath)
}

func (t kitToolchain) refuse(subject, what string) error {
	return &kitPreflightError{message: fmt.Sprintf("toolchain preflight refused %s: %s; %s search path %q; no attempt was started; remedy: %s", subject, what, t.scope(), t.searchPath(), t.remedy())}
}

// preflight checks configured locations and resolves the entrypoint, every
// executable prerequisite and a requested UI before start_verification_attempt
// or operation registration. It creates and cleans nothing.
func (t kitToolchain) preflight(subject string, e verification.Exercise, cwd string, ui *verification.UI, uiRoot string, redactor *redact.Redactor) (kitToolResolution, error) {
	var out kitToolResolution
	if t.configured {
		for i, dir := range t.searchPaths {
			if !kitAccessibleDirectory(dir, false) {
				return out, t.refuse(subject, fmt.Sprintf("verification_toolchains search_paths[%d] is not an accessible directory", i))
			}
		}
		if t.home != "" && !kitAccessibleDirectory(t.home, false) {
			return out, t.refuse(subject, "verification_toolchains home is not an accessible directory")
		}
		for _, key := range config.VerificationToolchainSettingKeys() {
			value, ok := t.settings[key]
			if !ok {
				continue
			}
			if err := kitCheckSetting(key, value); err != nil {
				return out, t.refuse(subject, fmt.Sprintf("verification_toolchains settings.%s %s", key, err.Error()))
			}
		}
	}
	resolve := func(role, label, name, dir string) (*kitResolvedTool, error) {
		path, err := t.lookPath(name, dir)
		if err != nil {
			return nil, t.refuse(subject, label+kitDisplayName(name, redactor)+" is unavailable")
		}
		digest, err := kitToolDigest(path)
		if err != nil {
			return nil, t.refuse(subject, label+kitDisplayName(name, redactor)+" cannot be read for its digest")
		}
		return &kitResolvedTool{role: role, label: label, name: name, path: path, digest: digest}, nil
	}
	if len(e.Argv) > 0 {
		tool, err := resolve("entrypoint", "entrypoint", e.Argv[0], cwd)
		if err != nil {
			return out, err
		}
		out.entrypoint = tool
	}
	for _, p := range e.Prerequisites {
		if p.Kind != "executable" {
			continue
		}
		tool, err := resolve("prerequisite_"+p.ID, "executable prerequisite "+p.ID, p.EnvironmentBinding, cwd)
		if err != nil {
			return out, err
		}
		out.prerequisites = append(out.prerequisites, *tool)
	}
	if ui != nil && len(ui.Argv) > 0 {
		tool, err := resolve("ui", "UI entrypoint", ui.Argv[0], uiRoot)
		if err != nil {
			return out, err
		}
		out.ui = tool
	}
	return out, nil
}

// kitAccessibleDirectory checks a directory in the operator-authorized
// environment; writable additionally requires write access.
func kitAccessibleDirectory(path string, writable bool) bool {
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return false
	}
	mode := uint32(0x4 | 0x1) // R_OK | X_OK
	if writable {
		mode |= 0x2 // W_OK
	}
	return syscall.Access(path, mode) == nil
}

func kitCheckSetting(key, value string) error {
	switch {
	case key == "GOENV":
		if value == "off" {
			return nil
		}
		info, err := os.Stat(value)
		if err != nil || !info.Mode().IsRegular() || syscall.Access(value, 0x4) != nil {
			return fmt.Errorf("is not a readable file")
		}
	case config.VerificationToolchainCacheSetting(key):
		for _, path := range strings.Split(value, string(os.PathListSeparator)) {
			if _, err := os.Lstat(path); os.IsNotExist(err) {
				continue
			}
			if !kitAccessibleDirectory(path, true) {
				return fmt.Errorf("is not a writable directory")
			}
		}
	default:
		if !kitAccessibleDirectory(value, false) {
			return fmt.Errorf("is not an accessible directory")
		}
	}
	return nil
}

func (r kitToolResolution) tools() []kitResolvedTool {
	var all []kitResolvedTool
	if r.entrypoint != nil {
		all = append(all, *r.entrypoint)
	}
	all = append(all, r.prerequisites...)
	if r.ui != nil {
		all = append(all, *r.ui)
	}
	return all
}

// recheck re-resolves every preflight identity through the same snapshot. A
// changed path or digest refuses the launch or invalidates the result.
func (r kitToolResolution) recheck(t kitToolchain, cwd, uiRoot string) error {
	for _, tool := range r.tools() {
		dir := cwd
		if tool.role == "ui" {
			dir = uiRoot
		}
		path, err := t.lookPath(tool.name, dir)
		if err != nil || path != tool.path {
			return fmt.Errorf("%s changed after preflight", tool.label)
		}
		digest, err := kitToolDigest(path)
		if err != nil || digest != tool.digest {
			return fmt.Errorf("%s changed after preflight", tool.label)
		}
	}
	return nil
}

func kitFingerprint(value string) string {
	return fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(value)))
}

// attributes records the selected scope, search order, HOME mode, setting
// fingerprints and resolved executable identities. Credential values never
// enter configuration, so no credential value or hash is recorded.
func (t kitToolchain) attributes(r kitToolResolution) map[string]string {
	out := map[string]string{"toolchain_scope": t.scope(), "toolchain_search_path": t.searchPath(), "toolchain_home": "attempt", "transitive_dependencies": "unknown"}
	if t.home != "" {
		out["toolchain_home"] = "configured " + kitFingerprint(t.home)
	}
	for key, value := range t.settings {
		if key == "GOENV" && value == "off" {
			out["toolchain_setting_"+key] = value
			continue
		}
		out["toolchain_setting_"+key] = kitFingerprint(value)
	}
	for _, tool := range r.tools() {
		out[tool.role+"_path"] = tool.path
		out[tool.role+"_sha256"] = tool.digest
	}
	return out
}
