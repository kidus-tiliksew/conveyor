package gitx

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/dlclark/regexp2"
)

// Preserve git grep's default basic regular expressions, including escaped
// groups and backreferences. RE2 alone silently changes those public queries.
// The pure-Go matcher keeps the daemon's CGO-disabled build and runs no shell.
func snapshotGrepPattern(ctx context.Context, pattern string, ignoreCase bool) (func(string) (bool, error), error) {
	var expressions []*regexp2.Regexp
	for _, expression := range strings.Split(pattern, "\n") {
		converted, err := snapshotBRE(expression)
		if err != nil {
			return nil, err
		}
		options := regexp2.RegexOptions(regexp2.RE2)
		if ignoreCase {
			options |= regexp2.IgnoreCase
		}
		compiled, err := regexp2.Compile(converted, options)
		if err != nil {
			return nil, fmt.Errorf("invalid Git regular expression: %w", err)
		}
		compiled.MatchTimeout = time.Second
		expressions = append(expressions, compiled)
	}
	return func(line string) (bool, error) {
		for _, expression := range expressions {
			if err := ctx.Err(); err != nil {
				return false, err
			}
			match, err := expression.MatchString(line)
			if err != nil {
				return false, fmt.Errorf("grep regular expression exceeded its bounded matching time")
			}
			if match {
				return true, nil
			}
		}
		return false, nil
	}, nil
}
func snapshotBRE(pattern string) (string, error) {
	var out strings.Builder
	inClass := false
	classStart := 0
	for i := 0; i < len(pattern); i++ {
		c := pattern[i]
		if inClass {
			out.WriteByte(c)
			if c == ']' && i > classStart+1 && !(i == classStart+2 && pattern[classStart+1] == '^') {
				inClass = false
			}
			continue
		}
		switch c {
		case '[':
			inClass = true
			classStart = i
			out.WriteByte(c)
		case '(', ')', '{', '}', '+', '?', '|':
			out.WriteByte('\\')
			out.WriteByte(c)
		case '\\':
			i++
			if i == len(pattern) {
				return "", fmt.Errorf("invalid Git regular expression: trailing backslash")
			}
			c = pattern[i]
			switch c {
			case '(', ')', '{', '}', '+', '?', '|':
				out.WriteByte(c)
			case '<':
				out.WriteString(`\b(?=\w)`)
			case '>':
				out.WriteString(`(?<=\w)\b`)
			case '`':
				out.WriteString(`\A`)
			case '\'':
				out.WriteString(`\z`)
			case '1', '2', '3', '4', '5', '6', '7', '8', '9', 'w', 'W', 's', 'S', 'b', 'B', '\\', '.', '*', '[', ']', '^', '$':
				out.WriteByte('\\')
				out.WriteByte(c)
			default:
				out.WriteByte(c)
			}
		default:
			out.WriteByte(c)
		}
	}
	if inClass {
		return "", fmt.Errorf("invalid Git regular expression: unclosed bracket")
	}
	return out.String(), nil
}
