// Package docsconfig parses the repository-declared documentation-closure
// gate (.conveyor/docs.yaml). One parser serves both the CLI validator and the
// server-side policy pin so a repository cannot weaken its own gate by
// declaring a shape the server reads differently.
package docsconfig

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/verification"
	"gopkg.in/yaml.v3"
)

// FilePath is the repository-relative path of the declaration.
const FilePath = ".conveyor/docs.yaml"

// SchemaVersion is the only supported schema_version.
const SchemaVersion = 1

// MaxFileBytes caps the declaration read from a repository. The server pins
// the policy through the same contents call as the verification manifest and
// applies the same 1 MiB ceiling.
const MaxFileBytes = verification.MaxManifestBytes

// MaxRuleTextBytes caps rule.text.
const MaxRuleTextBytes = 4 << 10

// Document is one declared durable-docs glob.
type Document struct {
	Path        string `yaml:"path" json:"path"`
	Description string `yaml:"description" json:"description"`
}

// Rule is the human-readable closure rule and the docs-none claim literal.
type Rule struct {
	NoneStatement  string `yaml:"none_statement" json:"none_statement"`
	ReasonRequired bool   `yaml:"reason_required" json:"reason_required"`
	Text           string `yaml:"text" json:"text"`
}

// Config is the parsed declaration.
type Config struct {
	SchemaVersion int        `yaml:"schema_version" json:"schema_version"`
	Docs          []Document `yaml:"docs" json:"docs"`
	Rule          Rule       `yaml:"rule" json:"rule"`
}

// Parse reads exactly one YAML document, rejects unknown keys, and validates
// the closed schema. The bytes are capped at MaxFileBytes.
func Parse(data []byte) (*Config, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("%s: empty declaration", FilePath)
	}
	if len(data) > MaxFileBytes {
		return nil, fmt.Errorf("%s: exceeds the %d byte limit", FilePath, MaxFileBytes)
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", FilePath, err)
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s: expected exactly one YAML document", FilePath)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// Validate enforces the closed schema: schema_version 1, at least one valid
// repository-relative glob, a docs-none literal, and a bounded rule text.
func (c *Config) Validate() error {
	if c.SchemaVersion != SchemaVersion {
		return fmt.Errorf("%s: unsupported schema_version %d (only %d is supported)", FilePath, c.SchemaVersion, SchemaVersion)
	}
	if len(c.Docs) == 0 {
		return fmt.Errorf("%s: docs must declare at least one path", FilePath)
	}
	seen := make(map[string]bool, len(c.Docs))
	for i, doc := range c.Docs {
		glob := strings.TrimSpace(doc.Path)
		if err := ValidateGlob(glob); err != nil {
			return fmt.Errorf("%s: docs[%d].path: %w", FilePath, i, err)
		}
		if seen[glob] {
			return fmt.Errorf("%s: docs[%d].path %q is declared more than once", FilePath, i, glob)
		}
		seen[glob] = true
		if strings.TrimSpace(doc.Description) == "" {
			return fmt.Errorf("%s: docs[%d].description is required", FilePath, i)
		}
	}
	if strings.TrimSpace(c.Rule.NoneStatement) == "" {
		return fmt.Errorf("%s: rule.none_statement is required", FilePath)
	}
	if len([]byte(c.Rule.Text)) > MaxRuleTextBytes {
		return fmt.Errorf("%s: rule.text exceeds %d bytes", FilePath, MaxRuleTextBytes)
	}
	return nil
}

// ValidateGlob accepts only the repository-relative * / ? / ** dialect used by
// governed paths: no absolute paths, no parent traversal and no character
// classes (which path.Match would otherwise interpret).
func ValidateGlob(glob string) error {
	if glob == "" {
		return fmt.Errorf("path is required")
	}
	if glob != strings.TrimSpace(glob) {
		return fmt.Errorf("path %q has surrounding whitespace", glob)
	}
	if strings.ContainsAny(glob, "[]") {
		return fmt.Errorf("path %q uses unsupported character-class syntax", glob)
	}
	if strings.HasPrefix(glob, "/") || glob == ".." || strings.HasPrefix(glob, "../") || strings.Contains(glob, "/../") || strings.HasSuffix(glob, "/..") {
		return fmt.Errorf("path %q must be repository-relative", glob)
	}
	if _, err := path.Match(strings.ReplaceAll(glob, "**", "*"), "validation"); err != nil {
		return fmt.Errorf("invalid glob %q: %w", glob, err)
	}
	return nil
}

// Paths returns the declared globs in declared order.
func (c *Config) Paths() []string {
	paths := make([]string, 0, len(c.Docs))
	for _, doc := range c.Docs {
		paths = append(paths, strings.TrimSpace(doc.Path))
	}
	return paths
}

// MatchPath reports whether a repository-relative changed path falls under any
// declared glob.
func (c *Config) MatchPath(changedPath string) bool {
	for _, doc := range c.Docs {
		if MatchGlob(strings.TrimSpace(doc.Path), changedPath) {
			return true
		}
	}
	return false
}

// MatchGlob applies one declared glob to a repository-relative path using the
// shared * / ? / ** dialect.
func MatchGlob(glob, changedPath string) bool {
	return core.MatchGovernedPath(glob, changedPath)
}

// MatchNoneStatement scans agent-authored body lines for the exact,
// case-sensitive none_statement literal at the start of a line, followed by
// end of line or whitespace (a trailing CR is trimmed first). When
// reasonRequired is true the non-empty remainder of the same line is the
// reason. It returns the reason (empty when not required) and whether a valid
// statement was found. Callers must pass only the agent-authored region of a
// pull request body, never Conveyor's generated lifecycle region.
func MatchNoneStatement(agentAuthoredBody, noneStatement string, reasonRequired bool) (string, bool) {
	if noneStatement == "" {
		return "", false
	}
	for _, line := range strings.Split(strings.ReplaceAll(agentAuthoredBody, "\r\n", "\n"), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if !strings.HasPrefix(line, noneStatement) {
			continue
		}
		rest := line[len(noneStatement):]
		if rest != "" {
			if r, _ := utf8.DecodeRuneInString(rest); !unicode.IsSpace(r) {
				continue
			}
		}
		reason := strings.TrimSpace(rest)
		if reasonRequired && reason == "" {
			continue
		}
		return reason, true
	}
	return "", false
}
