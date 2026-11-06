// Package serviceunit renders the shared systemd syntax used by Conveyor's
// daemon and worker user services.
package serviceunit

import "strings"

// QuoteArg quotes one ExecStart argument. systemd accepts quoted argv words,
// and percent/dollar expansion must be escaped in generated commands.
func QuoteArg(value string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, `%`, `%%`, `$`, `$$`).Replace(value) + `"`
}

// DirectivePath renders a path-valued directive without wrapping quotes.
// systemd treats quotes in WorkingDirectory and append: paths as literal data.
func DirectivePath(value string) string {
	return strings.ReplaceAll(value, "%", "%%")
}

// EnvironmentAssignment renders one Environment= word. systemd splits the value
// into words with C unescaping and then expands specifiers, so the whole
// assignment is one double-quoted word with backslash, quote, newline, and
// percent escaped. systemd never expands dollar signs in Environment=, so they
// stay literal, unlike in QuoteArg.
func EnvironmentAssignment(name, value string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, `%`, `%%`).Replace(name+"="+value) + `"`
}
