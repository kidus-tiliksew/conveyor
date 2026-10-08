package main

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
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
// (component-verification-runner).
var kitDefaultSearchPaths = []string{"/usr/local/bin", "/usr/bin", "/bin"}

// kitRunnerEnvironmentKeys belong to the runner's operation channel, attempt
// directory and loopback UI transport. Neither a toolchain record nor approved
// input or credential fields may supply them.
var kitRunnerEnvironmentKeys = []string{"CONVEYOR_KIT_OPERATIONS", "CONVEYOR_KIT_ATTEMPT_DIR", "CONVEYOR_KIT_UI_HOST", "CONVEYOR_KIT_UI_PORT"}

// kitOperatorConfigSources are the local configuration sources an operator
// selects explicitly or by user default. Only these may supply
// verification_toolchains or kit_permissions records (req-verification-kits
// REQ-7/AC-7.3; component-verification-runner "Local kit_permissions" and
// "Toolchain environment and preflight").
var kitOperatorConfigSources = map[string]bool{"flag": true, "environment CONVEYOR_CONFIG": true, "user default": true}

// kitConfigSourceRefusal explains why the loaded configuration cannot supply
// local execution authority, or returns "" when an operator selected a file
// outside every checkout input. A working-directory conveyor.yaml or any file
// that resolves inside the verified checkout or its Git common directory is
// repository content, so it can never widen PATH, HOME or tool settings or
// grant local kit actions. Other configuration keeps its existing precedence.
func kitConfigSourceRefusal(path, source string, checkouts []string) string {
	if !kitOperatorConfigSources[source] {
		if source == "" {
			source = "an unidentified source"
		}
		return fmt.Sprintf("was loaded from %s, not operator-selected configuration", source)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if errors.Is(err, os.ErrNotExist) {
		return ""
	}
	if err != nil {
		return "cannot be located outside the verified checkout"
	}
	for _, checkout := range checkouts {
		if real, e := filepath.EvalSymlinks(checkout); e == nil {
			checkout = real
		}
		rel, e := filepath.Rel(checkout, resolved)
		if e == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "is repository content inside the verified checkout"
		}
	}
	return ""
}

// kitPermissionsSourceRefusal refuses kit_permissions records from
// configuration that kitConfigSourceRefusal rejected, before any local action
// is resolved or any attempt starts. Checkout content is untrusted, so it
// cannot authorize local actions (req-verification-kits REQ-7/AC-7.3;
// component-verification-runner). The diagnostic carries no credential value.
func kitPermissionsSourceRefusal(cfg *config.Config, path, source, refusal string) error {
	if refusal == "" || cfg == nil || len(cfg.KitPermissions) == 0 {
		return nil
	}
	if source == "" {
		source = "an unidentified source"
	}
	return &kitPreflightError{message: fmt.Sprintf("kit_permissions_untrusted_source: the configuration file %s, selected by %s, %s, so its kit_permissions records cannot authorize local actions; no attempt was started; remedy: move the kit_permissions records to operator configuration outside the checkout and select it with --config, CONVEYOR_CONFIG or the user default", path, source, refusal)}
}

// kitToolchain is the immutable toolchain snapshot resolved once per subject.
// Executable lookup, preflight, provenance and launch all read this value
// (component-verification-runner).
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

// kitConfiguredLocation is the preflight identity of one explicitly configured
// toolchain location. A file carries its content digest; a directory carries
// only its resolved path and inode because tools legitimately change contents.
type kitConfiguredLocation struct {
	field, path, resolved, identity, digest string
	file                                    bool
}

// kitToolResolution holds the executable identities and configured-location
// fingerprints chosen by preflight. Launch and the post-execution check compare
// against these exact paths and digests.
type kitToolResolution struct {
	entrypoint    *kitResolvedTool
	prerequisites []kitResolvedTool
	ui            *kitResolvedTool
	configuration []kitConfiguredLocation
}

// kitPreflightError is a predictable refusal before any attempt exists. Its
// message is sanitized and names the remedy.
type kitPreflightError struct{ message string }

func (e *kitPreflightError) Error() string { return e.message }

func kitDefaultToolchain() kitToolchain {
	return kitToolchain{searchPaths: append([]string(nil), kitDefaultSearchPaths...), settings: map[string]string{}}
}

// resolveKitToolchain selects the scoped record or the minimal default. A
// record from configuration that is not operator-selected (refusal non-empty)
// is refused before any attempt. A configured value equal to a credential value
// is refused without echo.
func resolveKitToolchain(cfg *config.Config, configPath, refusal, server, workspace, repository string, secrets []string) (kitToolchain, error) {
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
	if refusal != "" {
		return kitToolchain{}, &kitPreflightError{message: fmt.Sprintf("toolchain preflight refused: the verification_toolchains record for server %s workspace %s repository %s in %s %s; no attempt was started; remedy: move the record to operator configuration outside the checkout and select it with --config, CONVEYOR_CONFIG or the user default", server, workspace, repository, configPath, refusal)}
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
		for _, location := range t.configuredLocations() {
			fingerprinted, err := location.fingerprint(redactor)
			if err != nil {
				return out, t.refuse(subject, fmt.Sprintf("verification_toolchains %s %s", location.field, err.Error()))
			}
			out.configuration = append(out.configuration, fingerprinted)
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

// kitMaxConfigurationFile bounds a fingerprinted configuration file.
const kitMaxConfigurationFile = 1 << 20

// configuredLocations lists the explicitly configured configuration locations
// whose identity is fixed for the attempt: HOME, GOROOT, XDG_CONFIG_HOME and a
// GOENV file. Caches and GOPATH are excluded because tools create and fill
// them; their contents and other transitive inputs remain unknown.
func (t kitToolchain) configuredLocations() []kitConfiguredLocation {
	var out []kitConfiguredLocation
	if t.home != "" {
		out = append(out, kitConfiguredLocation{field: "home", path: t.home})
	}
	for _, key := range []string{"GOENV", "GOROOT", "XDG_CONFIG_HOME"} {
		value, ok := t.settings[key]
		if !ok || (key == "GOENV" && value == "off") {
			continue
		}
		out = append(out, kitConfiguredLocation{field: "settings." + key, path: value, file: key == "GOENV"})
	}
	return out
}

// fingerprint resolves the location and records its identity. A configuration
// file's content is read once, refused when it carries a credential value or
// credential pattern, and digested; credentials therefore never reach a child
// through tool configuration, and the digest is not a credential-value hash.
func (l kitConfiguredLocation) fingerprint(redactor *redact.Redactor) (kitConfiguredLocation, error) {
	resolved, err := filepath.EvalSymlinks(l.path)
	if err != nil {
		return l, fmt.Errorf("cannot be resolved")
	}
	info, err := os.Stat(resolved)
	if err != nil || info.Mode().IsRegular() != l.file || (!l.file && !info.IsDir()) {
		return l, fmt.Errorf("is not the expected file type")
	}
	l.resolved, l.identity = resolved, kitFileIdentity(info)
	if !l.file {
		return l, nil
	}
	f, err := os.Open(resolved)
	if err != nil {
		return l, fmt.Errorf("is not a readable file")
	}
	defer func() { _ = f.Close() }()
	content, err := io.ReadAll(io.LimitReader(f, kitMaxConfigurationFile+1))
	if err != nil {
		return l, fmt.Errorf("is not a readable file")
	}
	if len(content) > kitMaxConfigurationFile {
		return l, fmt.Errorf("exceeds %d bytes", kitMaxConfigurationFile)
	}
	if redactor != nil {
		if clean, _ := redactor.Redact(string(content)); clean != string(content) {
			return l, fmt.Errorf("contains a credential value; deliver credentials only through approved CONVEYOR_KIT_SECRET_* grants")
		}
	}
	l.digest = fmt.Sprintf("%x", sha256.Sum256(content))
	return l, nil
}

func kitFileIdentity(info os.FileInfo) string {
	identity := info.Mode().Type().String()
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		identity += fmt.Sprintf(":%d:%d", st.Dev, st.Ino)
	}
	return identity
}

// recheck re-resolves every preflight identity through the same snapshot and
// re-fingerprints every configured location. A changed path, digest or
// configured identity refuses the launch or invalidates the result.
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
	for _, location := range r.configuration {
		current, err := kitConfiguredLocation{field: location.field, path: location.path, file: location.file}.fingerprint(nil)
		if err != nil || current.resolved != location.resolved || current.identity != location.identity || current.digest != location.digest {
			return fmt.Errorf("configured toolchain %s changed after preflight", location.field)
		}
	}
	return nil
}

func kitFingerprint(value string) string {
	return fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(value)))
}

// attributes records the selected scope, search order, HOME mode, setting
// fingerprints, configuration-file digests and resolved executable identities.
// Credential values never enter configuration, so no credential value or hash
// is recorded. Directory contents stay explicitly unknown.
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
	for _, location := range r.configuration {
		if location.file {
			out["toolchain_config_"+strings.TrimPrefix(location.field, "settings.")+"_sha256"] = location.digest
		}
	}
	if len(r.configuration) > 0 {
		out["toolchain_directory_contents"] = "unknown"
	}
	return out
}
