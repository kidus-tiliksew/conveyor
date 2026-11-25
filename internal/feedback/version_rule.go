// Package feedback resolves the versions that watchers report, compares
// commits for descent, and derives live status from stored answers
// (component-production-feedback; feature-production-feedback-live FB-3). The
// status function does no I/O. Nothing here is read by dispatch, claim, gate,
// review, merge, next-action, lineage, or work-order code (invariant 1;
// DEC-149(1)).
package feedback

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/gitx"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// RuleTarget is what a version rule reads from a version: a commit to
// resolve, or a tag name to read.
type RuleTarget struct {
	Commit string
	Tag    string
}

// commitInPattern is the only text a {commit} placeholder takes: 7 to 40
// hexadecimal characters (component-production-feedback invariant 3).
const commitInPattern = `([0-9A-Fa-f]{7,40})`

// ApplyVersionRule reads a commit or a tag from a reported version through
// the environment's version rule (component-production-feedback invariant 7;
// req-environments AC-1.2). The rule is checked by the same form rules that
// environment writes apply. A commit pattern matches the whole version: each
// * matches any characters and every other character is literal. A tag
// template replaces {version} with the version and {service} with the
// service once, and the result must name a valid Git tag. ok is false when
// the version does not fit the rule.
func ApplyVersionRule(rule store.EnvironmentVersionRule, version, service string) (target RuleTarget, ok bool, err error) {
	normalized, err := store.NormalizeEnvironmentVersionRule(&rule)
	if err != nil {
		return RuleTarget{}, false, err
	}
	switch normalized.Kind {
	case store.EnvironmentCommitPattern:
		pattern, err := compileCommitPattern(normalized.Text)
		if err != nil {
			return RuleTarget{}, false, err
		}
		match := pattern.FindStringSubmatch(version)
		if match == nil {
			return RuleTarget{}, false, nil
		}
		return RuleTarget{Commit: strings.ToLower(match[1])}, true, nil
	case store.EnvironmentTagTemplate:
		tag := strings.NewReplacer("{version}", version, "{service}", service).Replace(normalized.Text)
		if tag == "" || strings.HasPrefix(tag, "-") || !gitx.LegalBranchName("tag/"+tag) {
			return RuleTarget{}, false, nil
		}
		return RuleTarget{Tag: tag}, true, nil
	}
	return RuleTarget{}, false, fmt.Errorf("unknown version rule kind %q", normalized.Kind)
}

// compileCommitPattern turns a commit pattern into an anchored expression.
func compileCommitPattern(text string) (*regexp.Regexp, error) {
	prefix, suffix, found := strings.Cut(text, "{commit}")
	if !found {
		return nil, fmt.Errorf("a commit pattern must hold {commit}")
	}
	literal := func(part string) string {
		pieces := strings.Split(part, "*")
		for i, piece := range pieces {
			pieces[i] = regexp.QuoteMeta(piece)
		}
		return strings.Join(pieces, ".*")
	}
	return regexp.Compile(`(?s)^` + literal(prefix) + commitInPattern + literal(suffix) + `$`)
}
