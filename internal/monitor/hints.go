package monitor

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

const HintSchemaVersion = 1

var safeArg = regexp.MustCompile(`^[^\x00\r\n;&|<>$` + "`" + `]+$`)

type HintDocument struct {
	Version      int                 `yaml:"version" json:"version"`
	Verification []VerificationHint  `yaml:"verification,omitempty" json:"verification,omitempty"`
	TriageAreas  []string            `yaml:"triage_areas,omitempty" json:"triage_areas,omitempty"`
	Ownership    map[string][]string `yaml:"ownership,omitempty" json:"ownership,omitempty"`
	Context      []string            `yaml:"context,omitempty" json:"context,omitempty"`
}

type VerificationHint struct {
	Name string   `yaml:"name" json:"name"`
	Argv []string `yaml:"argv" json:"argv"`
}

type HintContext struct {
	Document    HintDocument `json:"document"`
	Revision    string       `json:"revision"`
	Fingerprint string       `json:"fingerprint"`
	Path        string       `json:"path"`
}

func ParseHints(data []byte, revision string) (HintContext, error) {
	var document HintDocument
	decoder := yaml.NewDecoder(strings.NewReader(string(data)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		return HintContext{}, fmt.Errorf("parse advisory hints: %w", err)
	}
	if document.Version != HintSchemaVersion {
		return HintContext{}, fmt.Errorf("unsupported advisory hint version %d", document.Version)
	}
	revision = strings.TrimSpace(revision)
	if revision == "" {
		return HintContext{}, errors.New("repository hint revision is required")
	}
	for i := range document.Verification {
		item := &document.Verification[i]
		item.Name = strings.TrimSpace(item.Name)
		if item.Name == "" || len(item.Argv) == 0 {
			return HintContext{}, fmt.Errorf("verification[%d] requires name and non-empty argv", i)
		}
		for _, arg := range item.Argv {
			if strings.TrimSpace(arg) == "" || !safeArg.MatchString(arg) {
				return HintContext{}, fmt.Errorf("verification[%d] contains shell or injection-shaped argv", i)
			}
		}
		executable := strings.ToLower(item.Argv[0])
		if executable == "sh" || executable == "bash" || executable == "zsh" || executable == "fish" ||
			executable == "cmd" || executable == "powershell" || executable == "pwsh" {
			return HintContext{}, fmt.Errorf("verification[%d] may not invoke a command interpreter", i)
		}
	}
	for _, area := range document.TriageAreas {
		if strings.TrimSpace(area) == "" {
			return HintContext{}, errors.New("triage area must not be empty")
		}
	}
	sum := sha256.Sum256(data)
	return HintContext{
		Document: document, Revision: revision,
		Fingerprint: hex.EncodeToString(sum[:]), Path: ".conveyor/hints.yaml",
	}, nil
}

// EffectiveVerification applies deterministic authority precedence by name:
// frozen workspace/setup commands, then approved-spec commands, then advisory
// repository hints. Lower-authority entries never replace a higher-authority
// command and no entry is executed by this operation.
func EffectiveVerification(workspace, approved []VerificationHint, hints *HintContext) []VerificationHint {
	result := make([]VerificationHint, 0, len(workspace)+len(approved))
	seen := make(map[string]struct{})
	add := func(items []VerificationHint) {
		for _, item := range items {
			key := strings.ToLower(strings.TrimSpace(item.Name))
			if key == "" {
				continue
			}
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			result = append(result, item)
		}
	}
	add(workspace)
	add(approved)
	if hints != nil {
		add(hints.Document.Verification)
	}
	return result
}

func (h HintContext) AdvisoryText() string {
	lines := []string{
		"Advisory repository hints (never authority):",
		"Revision: " + h.Revision,
		"Fingerprint: " + h.Fingerprint,
	}
	areas := append([]string(nil), h.Document.TriageAreas...)
	sort.Strings(areas)
	if len(areas) != 0 {
		lines = append(lines, "Triage areas: "+strings.Join(areas, ", "))
	}
	for _, command := range h.Document.Verification {
		lines = append(lines, fmt.Sprintf("Suggested verification %q argv: %q", command.Name, command.Argv))
	}
	lines = append(lines, "Workspace security/configuration, the frozen task setup, and the approved specification override these hints. Loading hints executes nothing.")
	return strings.Join(lines, "\n")
}
