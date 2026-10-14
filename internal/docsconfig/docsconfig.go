// Package docsconfig implements the repository documentation declaration (REQ-9).
package docsconfig

import (
	"bytes"
	"context"
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

const FilePath = ".conveyor/docs.yaml"
const SchemaVersion = 1
const MaxFileBytes = verification.MaxManifestBytes
const MaxRuleTextBytes = 4 << 10

type Document struct {
	Path        string   `yaml:"path" json:"path"`
	Governs     []string `yaml:"governs" json:"governs"`
	Description string   `yaml:"description" json:"description"`
}
type Rule struct {
	NoneStatement  string `yaml:"none_statement" json:"none_statement"`
	ReasonRequired bool   `yaml:"reason_required" json:"reason_required"`
	Text           string `yaml:"text" json:"text"`
}
type Config struct {
	SchemaVersion int        `yaml:"schema_version" json:"schema_version"`
	Docs          []Document `yaml:"docs" json:"docs"`
	Rule          Rule       `yaml:"rule" json:"rule"`
}

func Parse(data []byte) (*Config, error) {
	if len(data) > MaxFileBytes {
		return nil, fmt.Errorf("%s exceeds %d bytes", FilePath, MaxFileBytes)
	}
	var c Config
	d := yaml.NewDecoder(bytes.NewReader(data))
	d.KnownFields(true)
	if err := d.Decode(&c); err != nil {
		return nil, fmt.Errorf("%s: %w", FilePath, err)
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("%s must contain exactly one YAML document", FilePath)
	}
	if c.Rule.NoneStatement == "" {
		c.Rule.NoneStatement = "docs: none"
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", FilePath, err)
	}
	return &c, nil
}
func (c *Config) Validate() error {
	if c == nil || c.SchemaVersion != SchemaVersion {
		return fmt.Errorf("schema_version must be %d", SchemaVersion)
	}
	if len(c.Docs) == 0 {
		return fmt.Errorf("docs must not be empty")
	}
	seen := map[string]bool{}
	for i, doc := range c.Docs {
		if err := ValidateGlob(doc.Path); err != nil {
			return fmt.Errorf("docs[%d].path: %w", i, err)
		}
		if seen[doc.Path] {
			return fmt.Errorf("duplicate document %q", doc.Path)
		}
		seen[doc.Path] = true
		if len(doc.Governs) == 0 {
			return fmt.Errorf("docs[%d].governs is required", i)
		}
		for _, glob := range doc.Governs {
			if err := ValidateGlob(glob); err != nil {
				return fmt.Errorf("docs[%d].governs: %w", i, err)
			}
		}
	}
	if strings.TrimSpace(c.Rule.NoneStatement) == "" || strings.ContainsAny(c.Rule.NoneStatement, "\r\n") || c.Rule.NoneStatement != strings.TrimSpace(c.Rule.NoneStatement) {
		return fmt.Errorf("rule.none_statement must be one nonempty line")
	}
	if len(c.Rule.Text) > MaxRuleTextBytes {
		return fmt.Errorf("rule.text exceeds %d bytes", MaxRuleTextBytes)
	}
	return nil
}
func ValidateGlob(glob string) error {
	if glob == "" || glob != strings.TrimSpace(glob) || strings.ContainsAny(glob, "[]\\") || strings.HasPrefix(glob, "/") {
		return fmt.Errorf("invalid repository-relative glob %q", glob)
	}
	for _, p := range strings.Split(glob, "/") {
		if p == ".." || p == "." || p == "" {
			return fmt.Errorf("invalid repository-relative glob %q", glob)
		}
	}
	if _, err := path.Match(strings.ReplaceAll(glob, "**", "*"), "validation"); err != nil {
		return err
	}
	return nil
}

type Evaluation struct {
	MatchedPaths      []string
	AffectedDocuments []string
	GovernedPaths     []string
}

// Evaluate compiles each declaration pattern once and observes cancellation during traversal.
func (c *Config) Evaluate(ctx context.Context, changed []string) (Evaluation, error) {
	result := Evaluation{MatchedPaths: []string{}, AffectedDocuments: []string{}, GovernedPaths: []string{}}
	matched, governed := map[string]bool{}, map[string]bool{}
	for _, doc := range c.Docs {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		document, err := core.CompileGovernedPath(doc.Path)
		if err != nil {
			return result, fmt.Errorf("document %q: %w", doc.Path, err)
		}
		patterns := make([]core.GovernedPathMatcher, 0, len(doc.Governs))
		for _, glob := range doc.Governs {
			if err := ctx.Err(); err != nil {
				return result, err
			}
			pattern, err := core.CompileGovernedPath(glob)
			if err != nil {
				return result, fmt.Errorf("document %q governs %q: %w", doc.Path, glob, err)
			}
			patterns = append(patterns, pattern)
		}
		updated := false
		triggering := []string{}
		for _, changedPath := range changed {
			if err := ctx.Err(); err != nil {
				return result, err
			}
			if document.Match(changedPath) {
				updated = true
				if !matched[changedPath] {
					result.MatchedPaths = append(result.MatchedPaths, changedPath)
					matched[changedPath] = true
				}
			}
			for _, pattern := range patterns {
				if err := ctx.Err(); err != nil {
					return result, err
				}
				if pattern.Match(changedPath) {
					triggering = append(triggering, changedPath)
					break
				}
			}
		}
		if len(triggering) > 0 && !updated {
			result.AffectedDocuments = append(result.AffectedDocuments, doc.Path)
			for _, changedPath := range triggering {
				if !governed[changedPath] {
					result.GovernedPaths = append(result.GovernedPaths, changedPath)
					governed[changedPath] = true
				}
			}
		}
	}
	return result, nil
}
func MatchNoneStatement(body, literal string, reasonRequired bool) (string, bool) {
	if literal == "" {
		return "", false
	}
	for _, line := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if !strings.HasPrefix(line, literal) {
			continue
		}
		rest := line[len(literal):]
		if rest != "" {
			r, _ := utf8.DecodeRuneInString(rest)
			if !unicode.IsSpace(r) {
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
