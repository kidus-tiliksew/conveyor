// Package governedpath holds the System Design governed-path glob dialect:
// repository-relative patterns where ** spans separators, **/ also matches
// zero directories, and * and ? stay within one path segment. It depends on no
// other Conveyor package, so internal/core and internal/riskpolicy share one
// matcher without an import cycle (component-risk-review; req-risk-based-review
// AC-3.1).
package governedpath

import (
	"fmt"
	"regexp"
	"strings"
)

// Matcher reuses a compiled repository-relative doublestar pattern.
type Matcher struct {
	pattern *regexp.Regexp
}

// Match reports whether a repository-relative path matches. Backslashes are
// read as separators and a leading ./ is dropped; an empty, absolute, or
// parent-relative path never matches.
func (m Matcher) Match(changedPath string) bool {
	changedPath = strings.TrimPrefix(strings.ReplaceAll(changedPath, "\\", "/"), "./")
	if changedPath == "" || strings.HasPrefix(changedPath, "/") || strings.Contains(changedPath, "../") {
		return false
	}
	return m.pattern != nil && m.pattern.MatchString(changedPath)
}

// Compile translates a governed-path glob into a matcher.
func Compile(glob string) (Matcher, error) {
	var b strings.Builder
	b.WriteByte('^')
	for i := 0; i < len(glob); i++ {
		switch glob[i] {
		case '*':
			if i+1 < len(glob) && glob[i+1] == '*' {
				i++
				if i+1 < len(glob) && glob[i+1] == '/' {
					i++
					b.WriteString("(?:.*/)?")
				} else {
					b.WriteString(".*")
				}
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(glob[i])))
		}
	}
	b.WriteByte('$')
	pattern, err := regexp.Compile(b.String())
	return Matcher{pattern: pattern}, err
}

// Match compiles glob and matches one path; an invalid glob matches nothing.
func Match(glob, changedPath string) bool {
	matcher, err := Compile(glob)
	return err == nil && matcher.Match(changedPath)
}

// ValidateGlob refuses a glob that is not repository-relative: an empty or
// padded glob, a leading slash, an empty, . or .. segment, a character class,
// or a backslash. It is the rule internal/docsconfig applies to its globs.
func ValidateGlob(glob string) error {
	if glob == "" || glob != strings.TrimSpace(glob) || strings.ContainsAny(glob, "[]\\") || strings.HasPrefix(glob, "/") {
		return fmt.Errorf("invalid repository-relative glob %q", glob)
	}
	for _, segment := range strings.Split(glob, "/") {
		if segment == ".." || segment == "." || segment == "" {
			return fmt.Errorf("invalid repository-relative glob %q", glob)
		}
	}
	if _, err := Compile(glob); err != nil {
		return fmt.Errorf("invalid repository-relative glob %q", glob)
	}
	return nil
}
