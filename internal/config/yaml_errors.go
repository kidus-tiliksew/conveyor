package config

import (
	"errors"
	"regexp"

	"gopkg.in/yaml.v3"
)

// yamlTypeShape matches the Go type names yaml.v3 prints from reflection. The
// value text sits before the type in an entry and a greedy match takes the
// last " into ", so document text cannot pose as the type.
const yamlTypeShape = `(?:[A-Za-z0-9_.*\[\]]|interface \{\})+`

// yamlNameableKeyLength bounds a key the error may print.
const yamlNameableKeyLength = 64

var (
	yamlEntryLine       = regexp.MustCompile(`^line (\d+): `)
	yamlCannotUnmarshal = regexp.MustCompile(`(?s)^line (\d+): cannot unmarshal (\S+)(?: .*)? into (` + yamlTypeShape + `)$`)
	yamlFieldProblem    = regexp.MustCompile(`(?s)^line (\d+): field (.*) (not found|already set) in type (` + yamlTypeShape + `)$`)
	yamlDuplicateKey    = regexp.MustCompile(`(?s)^line (\d+): mapping key "(.*)" already defined at line (\d+)$`)
	yamlNameableKey     = regexp.MustCompile(`^[a-z]+(?:[_-][a-z]+)*$`)
	yamlUnknownAnchor   = regexp.MustCompile(`(?s)^yaml: (line \d+: )?unknown anchor '.*' referenced$`)
	yamlSelfAnchor      = regexp.MustCompile(`(?s)^yaml: (line \d+: )?anchor '.*' value contains itself$`)
	yamlInvalidMapKey   = regexp.MustCompile(`(?s)^yaml: invalid map key: .*$`)
	yamlCoreTags        = map[string]bool{"!!binary": true, "!!bool": true, "!!float": true, "!!int": true, "!!map": true, "!!merge": true, "!!null": true, "!!seq": true, "!!str": true, "!!timestamp": true}
)

// unmarshalYAML is yaml.Unmarshal with redactYAMLError applied.
func unmarshalYAML(data []byte, target any) error {
	return redactYAMLError(yaml.Unmarshal(data, target))
}

// redactYAMLError removes document text from a yaml.v3 decode error. Its
// messages reach conveyor init output, the conveyord startup log, and API
// refusals, and a secret misplaced in a configuration document must reach none
// of them (req-security-boundaries REQ-2). A *yaml.TypeError stays a
// *yaml.TypeError, so errors.As keeps working. Any other yaml.v3 error is fixed
// text unless it quotes an anchor name or map key, because `url: *pw` makes a
// value look like an anchor.
func redactYAMLError(err error) error {
	if err == nil {
		return nil
	}
	var typeError *yaml.TypeError
	if errors.As(err, &typeError) {
		entries := make([]string, len(typeError.Errors))
		for i, entry := range typeError.Errors {
			entries[i] = redactYAMLTypeEntry(entry)
		}
		return &yaml.TypeError{Errors: entries}
	}
	message := err.Error()
	switch {
	case yamlUnknownAnchor.MatchString(message):
		return errors.New(yamlUnknownAnchor.ReplaceAllString(message, "yaml: ${1}unknown anchor referenced"))
	case yamlSelfAnchor.MatchString(message):
		return errors.New(yamlSelfAnchor.ReplaceAllString(message, "yaml: ${1}anchor value contains itself"))
	case yamlInvalidMapKey.MatchString(message):
		return errors.New("yaml: invalid map key")
	}
	return err
}

// redactYAMLTypeEntry rewrites one *yaml.TypeError entry from its recognized
// forms and denies by default: an entry in any other form keeps only its line
// number.
//
// yaml.v3 prints a short value in full and the first 7 bytes of a longer one in
// "cannot unmarshal", so that form always loses the value. A custom tag is
// document text too, so only a core tag survives. The other forms print a key,
// not a value. A key is kept only as lowercase words, which is the shape of
// Conveyor's own configuration keys, because the workspace configuration routes
// name the refused field (component-runtime invariant 17). A key holding a
// digit, an uppercase letter, or punctuation is dropped.
func redactYAMLTypeEntry(entry string) string {
	if match := yamlCannotUnmarshal.FindStringSubmatch(entry); match != nil {
		kind := "value"
		if yamlCoreTags[match[2]] {
			kind = match[2]
		}
		return "line " + match[1] + ": cannot unmarshal " + kind + " into " + match[3]
	}
	if match := yamlFieldProblem.FindStringSubmatch(entry); match != nil {
		return "line " + match[1] + ": field" + nameableYAMLKey(match[2]) + " " + match[3] + " in type " + match[4]
	}
	if match := yamlDuplicateKey.FindStringSubmatch(entry); match != nil {
		return "line " + match[1] + ": mapping key" + nameableYAMLKey(match[2]) + " already defined at line " + match[3]
	}
	if match := yamlEntryLine.FindStringSubmatch(entry); match != nil {
		return "line " + match[1] + ": unrecognized YAML decode error"
	}
	return "unrecognized YAML decode error"
}

// nameableYAMLKey returns the key with a leading space when it may be printed,
// and nothing otherwise.
func nameableYAMLKey(key string) string {
	if len(key) > yamlNameableKeyLength || !yamlNameableKey.MatchString(key) {
		return ""
	}
	return " " + key
}
