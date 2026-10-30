// Package playbookcheck extracts the conveyor and conveyord invocations a
// Markdown playbook directs and checks each against the command surface the
// binaries register, so a playbook never names a command or flag its
// release lacks (req-agent-guided-setup AC-5.3; component-cli-onboarding).
package playbookcheck

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// Invocation is one conveyor or conveyord command line found in a playbook.
type Invocation struct {
	Line    int
	Program string
	Args    []string
	Text    string
}

func (i Invocation) String() string {
	return fmt.Sprintf("line %d: %s", i.Line, i.Text)
}

var (
	fenceOpen  = regexp.MustCompile("^\\s*(```+|~~~+)\\s*([A-Za-z0-9_+-]*)")
	inlineCode = regexp.MustCompile("`([^`]+)`")
	shellFence = map[string]bool{"": true, "sh": true, "bash": true, "shell": true, "console": true, "zsh": true}
)

// Extract returns every conveyor and conveyord invocation in shell fenced
// code blocks and in inline code spans. Shell blocks join backslash
// continuations and split on shell operators. Inline spans are read per
// paragraph, so a span may wrap lines, and `|` inside a span lists
// alternatives rather than a pipe. A program counts only in command
// position, after any environment assignments or a `nohup`, `env`, `exec`,
// or `time` prefix.
func Extract(markdown []byte) []Invocation {
	var invocations []Invocation
	scanner := bufio.NewScanner(bytes.NewReader(markdown))
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	fence, fenceShell := "", false
	pending, pendingLine := "", 0
	paragraph, paragraphLine := "", 0
	flushParagraph := func() {
		for _, span := range inlineCode.FindAllStringSubmatch(paragraph, -1) {
			invocations = append(invocations, shellInvocations(span[1], paragraphLine, true)...)
		}
		paragraph = ""
	}
	for line := 1; scanner.Scan(); line++ {
		text := scanner.Text()
		if fence != "" {
			if strings.HasPrefix(strings.TrimSpace(text), fence) {
				fence = ""
				continue
			}
			if !fenceShell {
				continue
			}
			if pending == "" {
				pendingLine = line
			}
			if continued, ok := strings.CutSuffix(text, "\\"); ok {
				pending += continued + " "
				continue
			}
			invocations = append(invocations, shellInvocations(pending+text, pendingLine, false)...)
			pending = ""
			continue
		}
		if match := fenceOpen.FindStringSubmatch(text); match != nil {
			flushParagraph()
			fence, fenceShell = match[1], shellFence[strings.ToLower(match[2])]
			continue
		}
		if strings.TrimSpace(text) == "" {
			flushParagraph()
			continue
		}
		if paragraph == "" {
			paragraphLine = line
		}
		paragraph += text + "\n"
	}
	flushParagraph()
	return invocations
}

var commandPrefix = map[string]bool{"nohup": true, "env": true, "exec": true, "time": true}

func shellInvocations(command string, line int, inline bool) []Invocation {
	var invocations []Invocation
	for _, words := range splitShell(command, inline) {
		index := 0
		for index < len(words) && (commandPrefix[words[index]] || isAssignment(words[index])) {
			index++
		}
		if index == len(words) {
			continue
		}
		if program := path.Base(words[index]); program == "conveyor" || program == "conveyord" {
			invocations = append(invocations, Invocation{
				Line: line, Program: program, Args: words[index+1:],
				Text: strings.Join(words[index:], " "),
			})
		}
		if inline {
			break
		}
	}
	return invocations
}

var assignment = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

func isAssignment(word string) bool { return assignment.MatchString(word) }

// splitShell tokenizes a shell command into operator-separated segments of
// words. Quotes group words, `<placeholder>` is one word, `$(` opens a
// nested segment, and a redirection ends the words of its segment. With
// literalPipe, `|` stays inside the word.
func splitShell(command string, literalPipe bool) [][]string {
	var segments [][]string
	var words []string
	var word strings.Builder
	inWord, quote, redirected := false, byte(0), false
	flushWord := func() {
		if inWord && !redirected {
			words = append(words, word.String())
		}
		word.Reset()
		inWord = false
	}
	flushSegment := func() {
		flushWord()
		if len(words) > 0 {
			segments = append(segments, words)
		}
		words, redirected = nil, false
	}
	for i := 0; i < len(command); i++ {
		c := command[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			} else {
				word.WriteByte(c)
			}
		case c == '\'' || c == '"':
			quote, inWord = c, true
		case c == '#' && !inWord:
			flushSegment()
			return segments
		case c == ' ' || c == '\t' || c == '\n':
			flushWord()
		case c == '<' && !inWord:
			if end := strings.IndexByte(command[i:], '>'); end > 1 && !strings.ContainsAny(command[i+1:i+end], " \t<|;&") {
				word.WriteString(command[i : i+end+1])
				inWord = true
				i += end
				continue
			}
			flushWord()
			redirected = true
		case c == '>':
			if inWord && isDigits(word.String()) {
				word.Reset()
				inWord = false
			}
			flushWord()
			redirected = true
		case c == '|' && literalPipe:
			word.WriteByte(c)
			inWord = true
		case c == '|' || c == ';' || c == '&' || c == '(' || c == ')':
			flushSegment()
		case c == '$' && i+1 < len(command) && command[i+1] == '(':
			flushSegment()
			i++
		default:
			word.WriteByte(c)
			inWord = true
		}
	}
	flushSegment()
	return segments
}

func isDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// CheckCobra resolves args against root's command tree. It fails on an
// unregistered subcommand or flag. A word may list alternatives with `|`,
// such as `create|edit`, and every alternative must resolve. A positional
// argument is accepted only by a runnable command.
func CheckCobra(root *cobra.Command, args []string) error {
	return checkCobra(root, args, false)
}

func checkCobra(command *cobra.Command, args []string, positional bool) error {
	// Cobra adds --help and the root's --version at execution; add them here
	// so the checked surface matches the executed one.
	command.InitDefaultHelpFlag()
	command.InitDefaultVersionFlag()
	for index := 0; index < len(args); index++ {
		word := args[index]
		if strings.HasPrefix(word, "-") && word != "-" && word != "--" {
			consumesValue, err := cobraFlag(command, word)
			if err != nil {
				return err
			}
			if consumesValue {
				index++
			}
			continue
		}
		if positional || !command.HasSubCommands() {
			if !command.Runnable() {
				return fmt.Errorf("%q takes no argument %q", command.CommandPath(), word)
			}
			positional = true
			continue
		}
		alternatives := strings.Split(word, "|")
		var children []*cobra.Command
		for _, alternative := range alternatives {
			child := subcommand(command, alternative)
			if child != nil {
				children = append(children, child)
			}
		}
		switch {
		case len(children) == len(alternatives):
			for _, child := range children {
				if err := checkCobra(child, args[index+1:], false); err != nil {
					return err
				}
			}
			return nil
		case len(children) == 0 && len(alternatives) == 1 && command.Runnable():
			positional = true
		default:
			return fmt.Errorf("unknown command %q for %q", word, command.CommandPath())
		}
	}
	return nil
}

func subcommand(command *cobra.Command, name string) *cobra.Command {
	for _, child := range command.Commands() {
		if child.Name() == name || child.HasAlias(name) {
			return child
		}
	}
	return nil
}

func cobraFlag(command *cobra.Command, word string) (bool, error) {
	name, _, hasValue := strings.Cut(strings.TrimLeft(word, "-"), "=")
	// InheritedFlags also merges the command's own persistent flags into
	// Flags, so it runs before either lookup.
	inherited := command.InheritedFlags()
	var found *pflag.Flag
	if strings.HasPrefix(word, "--") {
		if found = command.Flags().Lookup(name); found == nil {
			found = inherited.Lookup(name)
		}
	} else if len(name) == 1 {
		if found = command.Flags().ShorthandLookup(name); found == nil {
			found = inherited.ShorthandLookup(name)
		}
	}
	if found == nil {
		return false, fmt.Errorf("unknown flag %q for %q", word, command.CommandPath())
	}
	return !hasValue && found.NoOptDefVal == "", nil
}

// FlagSurface is a standard-library flag program: Default is the flag set
// parsed when no verb is given, and Verbs maps each verb accepted as the
// first argument to its own flag set.
type FlagSurface struct {
	Name    string
	Default *flag.FlagSet
	Verbs   map[string]*flag.FlagSet
}

// CheckFlagSurface resolves args against surface. It fails on an unknown
// verb, an unregistered flag, or any other positional argument.
func CheckFlagSurface(surface FlagSurface, args []string) error {
	flags, label := surface.Default, surface.Name
	if len(args) > 0 {
		if verbFlags, ok := surface.Verbs[args[0]]; ok {
			flags, label, args = verbFlags, surface.Name+" "+args[0], args[1:]
		}
	}
	for index := 0; index < len(args); index++ {
		word := args[index]
		if !strings.HasPrefix(word, "-") || word == "-" {
			return fmt.Errorf("unknown command or argument %q for %q", word, label)
		}
		name, _, hasValue := strings.Cut(strings.TrimLeft(word, "-"), "=")
		found := flags.Lookup(name)
		if found == nil {
			return fmt.Errorf("unknown flag %q for %q", word, label)
		}
		boolean, isBool := found.Value.(interface{ IsBoolFlag() bool })
		if !hasValue && !(isBool && boolean.IsBoolFlag()) {
			index++
		}
	}
	return nil
}
