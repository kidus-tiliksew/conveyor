// Package verification implements the shared kit contract in
// feature-verification-kit-execution VK-2/VK-3 (req-verification-kits REQ-1/REQ-2).
package verification

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

const MaxManifestBytes = 1 << 20
const MaxKits = 100
const MaxExercises = 100

type Manifest struct {
	SchemaVersion int   `yaml:"schema_version" json:"schema_version"`
	Kits          []Kit `yaml:"kits" json:"kits"`
	// Diagnostics are derived, never accepted from repository YAML.
	Diagnostics []Diagnostic `yaml:"-" json:"diagnostics,omitempty"`
}
type Diagnostic struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

func (d Diagnostic) Error() string { return d.Path + ": " + d.Message }

type DocumentPin struct {
	DocumentID string `yaml:"document_id" json:"document_id"`
	Version    int    `yaml:"version" json:"version"`
}
type GoverningPins struct {
	Requirements  []DocumentPin `yaml:"requirements" json:"requirements"`
	SystemDesigns []DocumentPin `yaml:"system_designs" json:"system_designs"`
}
type Kit struct {
	ID            string        `yaml:"id" json:"id"`
	Name          string        `yaml:"name" json:"name"`
	Version       string        `yaml:"version" json:"version"`
	Path          string        `yaml:"path" json:"path"`
	GoverningPins GoverningPins `yaml:"governing_pins" json:"governing_pins"`
	Exercises     []Exercise    `yaml:"exercises" json:"exercises"`
	UI            *UI           `yaml:"ui,omitempty" json:"ui,omitempty"`
	Diagnostics   []Diagnostic  `yaml:"-" json:"-"`
}
type Exercise struct {
	ID                 string           `yaml:"id" json:"id"`
	Stages             []string         `yaml:"stages" json:"stages"`
	Kind               string           `yaml:"kind" json:"kind"`
	Argv               []string         `yaml:"argv" json:"argv"`
	Cwd                string           `yaml:"cwd" json:"cwd"`
	TimeoutSeconds     int              `yaml:"timeout_seconds" json:"timeout_seconds"`
	Prerequisites      []Prerequisite   `yaml:"prerequisites" json:"prerequisites"`
	Permissions        []Permission     `yaml:"permissions" json:"permissions"`
	Inputs             []Input          `yaml:"inputs" json:"inputs"`
	RequiredAssertions []string         `yaml:"required_assertions" json:"required_assertions"`
	RetryPolicy        string           `yaml:"retry_policy" json:"retry_policy"`
	SafetyBasis        string           `yaml:"safety_basis,omitempty" json:"safety_basis,omitempty"`
	Operations         []Operation      `yaml:"operations" json:"operations"`
	EvidenceOutputs    []EvidenceOutput `yaml:"evidence_outputs" json:"evidence_outputs"`
	Supports           []Support        `yaml:"supports" json:"supports"`
}
type Prerequisite struct {
	ID                 string `yaml:"id" json:"id"`
	Kind               string `yaml:"kind" json:"kind"`
	EnvironmentBinding string `yaml:"environment_binding" json:"environment_binding"`
}
type Permission struct {
	Kind          string `yaml:"kind" json:"kind"`
	TargetBinding string `yaml:"target_binding,omitempty" json:"target_binding,omitempty"`
	Path          string `yaml:"path,omitempty" json:"path,omitempty"`
}
type Input struct {
	Name      string `yaml:"name" json:"name"`
	Type      string `yaml:"type" json:"type"`
	Required  bool   `yaml:"required" json:"required"`
	Sensitive bool   `yaml:"sensitive" json:"sensitive"`
}
type Operation struct {
	ID             string      `yaml:"id" json:"id"`
	TargetBinding  string      `yaml:"target_binding" json:"target_binding"`
	Reconciliation *Entrypoint `yaml:"reconciliation,omitempty" json:"reconciliation,omitempty"`
}
type Entrypoint struct {
	Argv           []string `yaml:"argv" json:"argv"`
	TimeoutSeconds int      `yaml:"timeout_seconds" json:"timeout_seconds"`
}
type EvidenceOutput struct {
	Type          string `yaml:"type" json:"type"`
	SchemaVersion int    `yaml:"schema_version" json:"schema_version"`
	MinimumItems  int    `yaml:"minimum_items" json:"minimum_items"`
}
type Support struct {
	DocumentID            string `yaml:"document_id" json:"document_id"`
	Version               int    `yaml:"version" json:"version"`
	AcceptanceCriterionID string `yaml:"acceptance_criterion_id" json:"acceptance_criterion_id"`
}
type UI struct {
	Argv   []string `yaml:"argv" json:"argv"`
	Port   int      `yaml:"port" json:"port"`
	Assets []string `yaml:"assets" json:"assets"`
}

// PathCheck validates an existing repository-relative reference against its
// repository-relative boundary. Forge callers can check their exact tree;
// checkout callers use FilesystemPathCheck. No executable is launched.
type PathCheck func(boundary, relative string) error

// Parse returns the recoverable entries even on failure, so selection can report
// invalid entries separately from valid, ineligible kits (AC-1.3 and AC-2.4).
// A nil checker performs lexical validation only; callers doing discovery must
// also supply a checker for availability and symlink containment.
func Parse(r io.Reader, check PathCheck) (*Manifest, error) {
	data, err := io.ReadAll(io.LimitReader(r, MaxManifestBytes+1))
	if err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	if len(data) > MaxManifestBytes {
		return nil, fmt.Errorf("manifest: exceeds 1 MiB")
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var node yaml.Node
	if err = dec.Decode(&node); err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	var extra yaml.Node
	if err = dec.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("manifest: expected exactly one YAML document")
	}
	if len(node.Content) != 1 || node.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("manifest: expected mapping")
	}
	root := node.Content[0]
	m := &Manifest{}
	if root.Anchor != "" {
		return nil, fmt.Errorf("manifest: anchors are forbidden")
	}
	// Validate top-level structure separately so a bad kit does not erase siblings.
	seen := map[string]bool{}
	for i := 0; i < len(root.Content); i += 2 {
		k, v := root.Content[i], root.Content[i+1]
		p := "manifest." + k.Value
		if k.Kind != yaml.ScalarNode || k.Tag != "!!str" || k.Anchor != "" {
			m.Diagnostics = append(m.Diagnostics, Diagnostic{p, "expected string key"})
			continue
		}
		if v.Kind == yaml.AliasNode || v.Anchor != "" {
			m.Diagnostics = append(m.Diagnostics, Diagnostic{p, "aliases and anchors are forbidden"})
			continue
		}
		if seen[k.Value] {
			m.Diagnostics = append(m.Diagnostics, Diagnostic{p, "duplicate key"})
			continue
		}
		seen[k.Value] = true
		switch k.Value {
		case "schema_version":
			if ds := validateNode(v, reflect.TypeOf(0), p); len(ds) > 0 {
				m.Diagnostics = append(m.Diagnostics, ds...)
			} else if err := v.Decode(&m.SchemaVersion); err != nil {
				m.Diagnostics = append(m.Diagnostics, Diagnostic{p, err.Error()})
			}
		case "kits":
			if v.Kind != yaml.SequenceNode {
				m.Diagnostics = append(m.Diagnostics, Diagnostic{p, "expected sequence"})
				continue
			}
			if len(v.Content) > MaxKits {
				m.Diagnostics = append(m.Diagnostics, Diagnostic{p, "exceeds 100 kits"})
				continue
			}
			for j, n := range v.Content {
				kp := fmt.Sprintf("manifest.kits[%d]", j)
				kit := Kit{}
				ds := validateNode(n, reflect.TypeOf(kit), kp)
				if len(ds) == 0 {
					if err := n.Decode(&kit); err != nil {
						ds = append(ds, Diagnostic{kp, err.Error()})
					}
				} else {
					// Retain only a scalar ID for identifying the invalid entry in receipts.
					if n.Kind == yaml.MappingNode {
						for a := 0; a < len(n.Content); a += 2 {
							if n.Content[a].Value == "id" && n.Content[a+1].Kind == yaml.ScalarNode {
								kit.ID = n.Content[a+1].Value
								break
							}
						}
					}
				}
				if len(ds) == 0 {
					ds = validateKit(kit, kp, check)
				}
				kit.Diagnostics = ds
				m.Kits = append(m.Kits, kit)
			}
		default:
			m.Diagnostics = append(m.Diagnostics, Diagnostic{p, "unknown field"})
		}
	}
	if m.SchemaVersion != 1 {
		m.Diagnostics = append(m.Diagnostics, Diagnostic{"manifest.schema_version", "unsupported schema; expected 1"})
	}
	if !seen["kits"] {
		m.Diagnostics = append(m.Diagnostics, Diagnostic{"manifest.kits", "required field"})
	}
	ids := map[string]int{}
	for i := range m.Kits {
		k := &m.Kits[i]
		if k.ID == "" {
			continue
		}
		if j, ok := ids[k.ID]; ok {
			d := Diagnostic{fmt.Sprintf("manifest.kits[%d].id", i), "duplicate kit ID " + k.ID}
			k.Diagnostics = append(k.Diagnostics, d)
			m.Kits[j].Diagnostics = append(m.Kits[j].Diagnostics, d)
		} else {
			ids[k.ID] = i
		}
	}
	var errs []error
	for _, d := range m.Diagnostics {
		errs = append(errs, d)
	}
	for _, k := range m.Kits {
		for _, d := range k.Diagnostics {
			errs = append(errs, d)
		}
	}
	return m, errors.Join(errs...)
}

// validateNode enforces a closed shape before Decode, including aliases and
// duplicate keys that ordinary struct decoding cannot safely normalize.
func validateNode(n *yaml.Node, t reflect.Type, p string) []Diagnostic {
	bad := func(s string) []Diagnostic { return []Diagnostic{{p, s}} }
	if n.Kind == yaml.AliasNode || n.Anchor != "" {
		return bad("aliases and anchors are forbidden")
	}
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Struct:
		if n.Kind != yaml.MappingNode {
			return bad("expected mapping")
		}
		fields := map[string]reflect.Type{}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			name := strings.Split(f.Tag.Get("yaml"), ",")[0]
			if name != "-" {
				fields[name] = f.Type
			}
		}
		var ds []Diagnostic
		seen := map[string]bool{}
		for i := 0; i < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			kp := p + "." + k.Value
			if k.Kind != yaml.ScalarNode || k.Tag != "!!str" || k.Anchor != "" {
				ds = append(ds, Diagnostic{kp, "expected string key"})
				continue
			}
			if seen[k.Value] {
				ds = append(ds, Diagnostic{kp, "duplicate key"})
				continue
			}
			seen[k.Value] = true
			ft, ok := fields[k.Value]
			if !ok {
				ds = append(ds, Diagnostic{kp, "unknown field"})
				continue
			}
			ds = append(ds, validateNode(v, ft, kp)...)
		}
		return ds
	case reflect.Slice:
		if n.Kind != yaml.SequenceNode {
			return bad("expected sequence")
		}
		var ds []Diagnostic
		for i, v := range n.Content {
			ds = append(ds, validateNode(v, t.Elem(), fmt.Sprintf("%s[%d]", p, i))...)
		}
		return ds
	default:
		tag := map[reflect.Kind]string{reflect.String: "!!str", reflect.Int: "!!int", reflect.Bool: "!!bool"}[t.Kind()]
		if n.Kind != yaml.ScalarNode || n.Tag != tag {
			return bad("expected " + t.Kind().String())
		}
	}
	return nil
}

func safePath(p string) error {
	if p == "" || path.IsAbs(p) || filepath.IsAbs(p) || strings.ContainsAny(p, "\\\x00\r\n") || strings.Contains(p, ":") {
		return fmt.Errorf("unsafe relative path %q", p)
	}
	for _, part := range strings.Split(p, "/") {
		if part == ".." {
			return fmt.Errorf("path traversal %q", p)
		}
	}
	return nil
}

// FilesystemPathCheck resolves both boundaries and candidates, rejecting missing
// inputs, symlink escapes and traversal through a nested git repository/submodule.
func FilesystemPathCheck(repositoryRoot string) (PathCheck, error) {
	root, err := filepath.EvalSymlinks(repositoryRoot)
	if err != nil {
		return nil, err
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	return func(boundary, relative string) error {
		if err := safePath(boundary); err != nil {
			return err
		}
		if err := safePath(relative); err != nil {
			return err
		}
		b := filepath.Join(root, filepath.FromSlash(boundary))
		candidate := filepath.Join(root, filepath.FromSlash(relative))
		// A dangling link must not look like an absent write destination.
		// Otherwise a caller checking the nearest existing parent could accept
		// a link that will later resolve outside the kit.
		for current := candidate; current != root && current != filepath.Dir(current); current = filepath.Dir(current) {
			if st, statErr := os.Lstat(current); statErr == nil && st.Mode()&os.ModeSymlink != 0 {
				if _, linkErr := filepath.EvalSymlinks(current); linkErr != nil {
					return fmt.Errorf("unresolvable symlink %s: %v", relative, linkErr)
				}
			}
		}
		rb, err := filepath.EvalSymlinks(b)
		if err != nil {
			return err
		}
		rc, err := filepath.EvalSymlinks(candidate)
		if err != nil {
			return err
		}
		for _, pair := range [][2]string{{root, rb}, {root, rc}, {rb, rc}} {
			rel, e := filepath.Rel(pair[0], pair[1])
			if e != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return fmt.Errorf("symlink escapes root: %s", relative)
			}
		}
		for current := candidate; current != root && current != filepath.Dir(current); current = filepath.Dir(current) {
			if _, e := os.Lstat(filepath.Join(current, ".git")); e == nil {
				return fmt.Errorf("nested repository or submodule: %s", relative)
			} else if !os.IsNotExist(e) && !errors.Is(e, os.ErrInvalid) {
				if st, se := os.Stat(current); se == nil && st.IsDir() {
					return e
				}
			}
		}
		return nil
	}, nil
}

func validateKit(k Kit, p string, check PathCheck) []Diagnostic {
	var ds []Diagnostic
	add := func(field, msg string) { ds = append(ds, Diagnostic{p + "." + field, msg}) }
	required := func(field, s string) {
		if strings.TrimSpace(s) == "" {
			add(field, "required field")
		}
	}
	ref := func(field, boundary, relative string) {
		if err := safePath(relative); err != nil {
			add(field, err.Error())
			return
		}
		if check != nil {
			if err := check(boundary, relative); err != nil {
				add(field, err.Error())
			}
		}
	}
	required("id", k.ID)
	required("name", k.Name)
	required("version", k.Version)
	ref("path", ".", k.Path)
	for kind, pins := range map[string][]DocumentPin{"requirements": k.GoverningPins.Requirements, "system_designs": k.GoverningPins.SystemDesigns} {
		seen := map[string]bool{}
		for i, pin := range pins {
			f := fmt.Sprintf("governing_pins.%s[%d]", kind, i)
			if pin.DocumentID == "" || pin.Version <= 0 {
				add(f, "document_id and positive version required")
			}
			if seen[pin.DocumentID] {
				add(f, "duplicate document pin")
			}
			seen[pin.DocumentID] = true
		}
	}
	if len(k.Exercises) == 0 {
		add("exercises", "at least one exercise required")
	}
	if len(k.Exercises) > MaxExercises {
		add("exercises", "exceeds 100 exercises")
	}
	ids := map[string]bool{}
	for i, e := range k.Exercises {
		ep := fmt.Sprintf("exercises[%d]", i)
		if e.ID == "" || ids[e.ID] {
			add(ep+".id", "missing or duplicate exercise ID")
		}
		ids[e.ID] = true
		if len(e.Stages) == 0 {
			add(ep+".stages", "required stage list")
		}
		stages := map[string]bool{}
		for _, s := range e.Stages {
			if s != "verify" || stages[s] {
				add(ep+".stages", "expected unique verify stage")
			}
			stages[s] = true
		}
		if e.Kind != "script" && e.Kind != "interactive" && e.Kind != "hybrid" {
			add(ep+".kind", "unknown exercise kind")
		}
		argvCheck := func(field string, argv []string, cwd string) {
			if len(argv) == 0 || strings.TrimSpace(argv[0]) == "" {
				add(field, "entrypoint required")
				return
			}
			if err := safePath(argv[0]); err != nil {
				add(field, err.Error())
			} else if strings.Contains(argv[0], "/") {
				ref(field, k.Path, path.Join(k.Path, cwd, argv[0]))
			}
		}
		argvCheck(ep+".argv", e.Argv, e.Cwd)
		if err := safePath(e.Cwd); err != nil {
			add(ep+".cwd", err.Error())
		} else {
			ref(ep+".cwd", k.Path, path.Join(k.Path, e.Cwd))
		}
		if e.TimeoutSeconds <= 0 {
			add(ep+".timeout_seconds", "positive timeout required")
		}
		if e.RequiredAssertions == nil {
			add(ep+".required_assertions", "explicit list required")
		}
		seen := map[string]bool{}
		for _, a := range e.RequiredAssertions {
			if strings.TrimSpace(a) == "" || seen[a] {
				add(ep+".required_assertions", "empty or duplicate assertion ID")
			}
			seen[a] = true
		}
		switch e.RetryPolicy {
		case "safe_to_replay":
			if strings.TrimSpace(e.SafetyBasis) == "" {
				add(ep+".safety_basis", "safe_to_replay requires safety basis")
			}
		case "reconciliation_required", "operator_action_required":
		default:
			add(ep+".retry_policy", "required policy: safe_to_replay, reconciliation_required, or operator_action_required")
		}
		bindings := map[string]bool{}
		for j, v := range e.Permissions {
			f := fmt.Sprintf("%s.permissions[%d]", ep, j)
			switch v.Kind {
			case "filesystem_read", "filesystem_write":
				if err := safePath(v.Path); err != nil {
					add(f+".path", err.Error())
				} else if check != nil {
					candidate := path.Join(k.Path, v.Path)
					for {
						err := check(k.Path, candidate)
						// Write destinations can be new. Validate their nearest
						// existing parent without waiving containment checks.
						if err != nil && v.Kind == "filesystem_write" && os.IsNotExist(err) && candidate != path.Clean(k.Path) {
							candidate = path.Dir(candidate)
							continue
						}
						if err != nil {
							add(f+".path", err.Error())
						}
						break
					}
				}
			case "network":
				if v.TargetBinding == "" {
					add(f+".target_binding", "required binding")
				}
			case "operator_interaction":
			default:
				add(f+".kind", "unknown permission kind")
			}
			if v.TargetBinding != "" {
				bindings[v.TargetBinding] = true
			}
		}
		seen = map[string]bool{}
		for j, v := range e.Prerequisites {
			f := fmt.Sprintf("%s.prerequisites[%d]", ep, j)
			if v.ID == "" || seen[v.ID] {
				add(f+".id", "missing or duplicate prerequisite ID")
			}
			seen[v.ID] = true
			switch v.Kind {
			case "executable", "service", "credential", "operator_interaction":
			default:
				add(f+".kind", "unknown prerequisite kind")
			}
			if v.EnvironmentBinding == "" {
				add(f+".environment_binding", "required binding")
			}
		}
		seen = map[string]bool{}
		for j, v := range e.Inputs {
			f := fmt.Sprintf("%s.inputs[%d]", ep, j)
			if v.Name == "" || seen[v.Name] {
				add(f+".name", "missing or duplicate input name")
			}
			seen[v.Name] = true
			switch v.Type {
			case "string", "boolean", "integer", "number":
			default:
				add(f+".type", "unknown input type")
			}
		}
		if e.Operations == nil {
			add(ep+".operations", "explicit list required")
		}
		seen = map[string]bool{}
		for j, o := range e.Operations {
			f := fmt.Sprintf("%s.operations[%d]", ep, j)
			if o.ID == "" || seen[o.ID] {
				add(f+".id", "missing or duplicate operation ID")
			}
			seen[o.ID] = true
			if !bindings[o.TargetBinding] {
				add(f+".target_binding", "must name a permission binding")
			}
			if e.RetryPolicy == "reconciliation_required" && o.Reconciliation == nil {
				add(f+".reconciliation", "required entrypoint")
			}
			if o.Reconciliation != nil {
				argvCheck(f+".reconciliation.argv", o.Reconciliation.Argv, e.Cwd)
				if o.Reconciliation.TimeoutSeconds <= 0 {
					add(f+".reconciliation.timeout_seconds", "positive timeout required")
				}
			}
		}
		for j, v := range e.EvidenceOutputs {
			f := fmt.Sprintf("%s.evidence_outputs[%d]", ep, j)
			if !evidenceType(v.Type) || v.SchemaVersion != 1 {
				add(f, "unknown evidence type/schema")
			}
			if v.MinimumItems < 0 {
				add(f+".minimum_items", "must be nonnegative")
			}
		}
		for j, s := range e.Supports {
			f := fmt.Sprintf("%s.supports[%d]", ep, j)
			found := false
			for _, pin := range k.GoverningPins.Requirements {
				if pin.DocumentID == s.DocumentID && pin.Version == s.Version {
					found = true
				}
			}
			if !found || !validAC(s.AcceptanceCriterionID) {
				add(f, "support must name an AC within a declared requirement pin")
			}
		}
	}
	if k.UI != nil {
		u := k.UI
		if len(u.Argv) == 0 {
			add("ui.argv", "entrypoint required")
		} else if err := safePath(u.Argv[0]); err != nil {
			add("ui.argv", err.Error())
		} else if strings.Contains(u.Argv[0], "/") {
			ref("ui.argv", k.Path, path.Join(k.Path, u.Argv[0]))
		}
		if u.Port < 1 || u.Port > 65535 {
			add("ui.port", "loopback port must be 1..65535")
		}
		for i, a := range u.Assets {
			if err := safePath(a); err != nil {
				add(fmt.Sprintf("ui.assets[%d]", i), err.Error())
			} else {
				ref(fmt.Sprintf("ui.assets[%d]", i), k.Path, path.Join(k.Path, a))
			}
		}
	}
	sort.SliceStable(ds, func(i, j int) bool { return ds[i].Path < ds[j].Path })
	return ds
}
func evidenceType(t string) bool {
	switch t {
	case "api_exchange", "state_observation", "assertion_result", "execution_report", "visual_capture", "operator_observation":
		return true
	}
	return false
}
func validAC(s string) bool {
	var req, ac int
	var rest string
	n, _ := fmt.Sscanf(s, "AC-%d.%d%s", &req, &ac, &rest)
	return n == 2 && req > 0 && ac > 0 && s == fmt.Sprintf("AC-%d.%d", req, ac)
}
