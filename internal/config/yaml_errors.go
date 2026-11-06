package config

import (
	"bytes"
	"encoding/base64"
	"errors"
	"io"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// yamlTypeShape matches the Go type names yaml.v3 prints from reflection. The
// value text sits before the type in an entry and a greedy match takes the
// last " into ", so document text cannot pose as the type.
const yamlTypeShape = `(?:[A-Za-z0-9_.*\[\]]|interface \{\})+`

// yamlNameableKeyLength bounds a key the error may print.
const yamlNameableKeyLength = 64

// yamlUnrecognizedError replaces any decoder text this file does not recognize.
const yamlUnrecognizedError = "unrecognized YAML decode error"

// yamlInvalidBase64 is fixed text, and the rewrite adds its line.
const yamlInvalidBase64 = "!!binary value contains invalid base64 data"

// yamlMaxAliasOccurrences bounds the alias tokens locateYAMLAlias tries.
const yamlMaxAliasOccurrences = 32

var (
	yamlEntryLine       = regexp.MustCompile(`^line (\d+): `)
	yamlCannotUnmarshal = regexp.MustCompile(`(?s)^line (\d+): cannot unmarshal (\S+)(?: .*)? into (` + yamlTypeShape + `)$`)
	yamlFieldProblem    = regexp.MustCompile(`(?s)^line (\d+): field (.*) (not found|already set) in type (` + yamlTypeShape + `)$`)
	yamlDuplicateKey    = regexp.MustCompile(`(?s)^line (\d+): mapping key "(.*)" already defined at line (\d+)$`)
	yamlNameableKey     = regexp.MustCompile(`^[a-z]+(?:[_-][a-z]+)*$`)
	yamlResolverFailure = regexp.MustCompile("(?s)^cannot decode (\\S+) `(.*)` as a (\\S+)$")
	yamlUnknownAnchor   = regexp.MustCompile(`(?s)^unknown anchor '(.*)' referenced$`)
	yamlSelfAnchor      = regexp.MustCompile(`(?s)^anchor '.*' value contains itself$`)
	yamlInvalidMapKey   = regexp.MustCompile(`(?s)^invalid map key: .*$`)
	yamlCoreTags        = map[string]bool{"!!binary": true, "!!bool": true, "!!float": true, "!!int": true, "!!map": true, "!!merge": true, "!!null": true, "!!seq": true, "!!str": true, "!!timestamp": true}

	// yamlFixedForm matches the yaml.v3 messages whose only variable part is a
	// number or an event name.
	yamlFixedForm = regexp.MustCompile(`^(?:exceeded max depth of \d+|cannot decode node with unknown kind \d+|invalid array: want \d+ elements but got \d+|expected ` + yamlEventName + ` event but got ` + yamlEventName + `)$`)

	// yamlFixedProblems lists every message in yaml.v3 v3.0.1 that holds no
	// document text: the scanner, parser, and reader problems and the fixed
	// failf calls in decode.go. TestYAMLFailureFormsMatchPinnedSource compares
	// this list with the module source.
	yamlFixedProblems = map[string]bool{
		"!!binary value contains invalid base64 data":                  true,
		"attempted to go past the end of stream; corrupted value?":     true,
		"block sequence entries are not allowed in this context":       true,
		"control characters are not allowed":                           true,
		"could not find expected ':'":                                  true,
		"could not find expected directive name":                       true,
		"did not find URI escaped octet":                               true,
		"did not find expected '!'":                                    true,
		"did not find expected ',' or ']'":                             true,
		"did not find expected ',' or '}'":                             true,
		"did not find expected '-' indicator":                          true,
		"did not find expected <document start>":                       true,
		"did not find expected <stream-start>":                         true,
		"did not find expected alphabetic or numeric character":        true,
		"did not find expected comment or line break":                  true,
		"did not find expected digit or '.' character":                 true,
		"did not find expected hexdecimal number":                      true,
		"did not find expected key":                                    true,
		"did not find expected node content":                           true,
		"did not find expected tag URI":                                true,
		"did not find expected version number":                         true,
		"did not find expected whitespace":                             true,
		"did not find expected whitespace or line break":               true,
		"did not find the expected '>'":                                true,
		"document contains excessive aliasing":                         true,
		"expected low surrogate area":                                  true,
		"found a tab character that violates indentation":              true,
		"found a tab character where an indentation space is expected": true,
		"found an incorrect leading UTF-8 octet":                       true,
		"found an incorrect trailing UTF-8 octet":                      true,
		"found an indentation indicator equal to 0":                    true,
		"found character that cannot start any token":                  true,
		"found duplicate %TAG directive":                               true,
		"found duplicate %YAML directive":                              true,
		"found extremely long version number":                          true,
		"found incompatible YAML document":                             true,
		"found invalid Unicode character escape code":                  true,
		"found undefined tag handle":                                   true,
		"found unexpected document indicator":                          true,
		"found unexpected end of stream":                               true,
		"found unexpected non-alphabetical character":                  true,
		"found unknown directive name":                                 true,
		"found unknown escape character":                               true,
		"incomplete UTF-16 character":                                  true,
		"incomplete UTF-16 surrogate pair":                             true,
		"incomplete UTF-8 octet sequence":                              true,
		"invalid Unicode character":                                    true,
		"invalid leading UTF-8 octet":                                  true,
		"invalid length of a UTF-8 sequence":                           true,
		"invalid trailing UTF-8 octet":                                 true,
		"map merge requires map or sequence of maps as the value":      true,
		"mapping keys are not allowed in this context":                 true,
		"mapping values are not allowed in this context":               true,
		"unexpected low surrogate area":                                true,
		"unknown problem parsing YAML content":                         true,
	}
)

// yamlEventName is every event name yaml.v3 prints in its "expected" message.
const yamlEventName = `(?:none|stream start|stream end|document start|document end|alias|scalar|sequence start|sequence end|mapping start|mapping end|tail comment)`

// unmarshalYAML is yaml.Unmarshal with redactYAMLError applied.
func unmarshalYAML(data []byte, target any) error {
	return redactYAMLError(yaml.Unmarshal(data, target), data, target)
}

// locatedYAMLError prefers the strict decoder's error for a document whose
// generic decode failed, because only the strict decoder knows the expected Go
// type. It returns err when the strict decoder finds nothing.
func locatedYAMLError[T any](data []byte, err error) error {
	var located T
	if decodeErr := decodeKnown(data, &located); decodeErr != nil {
		return decodeErr
	}
	return err
}

// redactYAMLError removes document text from a yaml.v3 decode error. Its
// messages reach conveyor init output, the conveyord startup log, and API
// refusals, and a secret misplaced in a configuration document must reach none
// of them (req-security-boundaries REQ-2). A *yaml.TypeError stays a
// *yaml.TypeError, so errors.As keeps working, and io.EOF stays io.EOF for an
// empty document. Every other error denies by default: redactYAMLPlainError
// passes only messages it recognizes as fixed text.
//
// data and target are the document and decode target, used only to add the line
// and expected type that yaml.v3 leaves out of its plain errors.
func redactYAMLError(err error, data []byte, target any) error {
	if err == nil || errors.Is(err, io.EOF) {
		return err
	}
	var typeError *yaml.TypeError
	if errors.As(err, &typeError) {
		entries := make([]string, len(typeError.Errors))
		for i, entry := range typeError.Errors {
			entries[i] = redactYAMLTypeEntry(entry)
		}
		return &yaml.TypeError{Errors: entries}
	}
	// time.Time is the one TextUnmarshaler in a configuration document, and
	// yaml.v3 passes its error on unchanged. time.ParseError prints the whole value.
	var parseError *time.ParseError
	if errors.As(err, &parseError) {
		located, _ := locateYAMLScalar(data, target, func(node *yaml.Node) bool {
			return node.Value == parseError.Value
		})
		return errors.New("yaml: " + located + "cannot unmarshal value into time.Time")
	}
	return redactYAMLPlainError(err, data, target)
}

// redactYAMLPlainError rewrites an error that is not a *yaml.TypeError. A
// fixed-text message passes unchanged. The forms that quote document text keep
// only their fixed words, and any other form keeps only its line number.
func redactYAMLPlainError(err error, data []byte, target any) error {
	message, isYAML := strings.CutPrefix(err.Error(), "yaml: ")
	where := yamlEntryLine.FindString(message)
	message = strings.TrimPrefix(message, where)
	if !isYAML {
		return errors.New("yaml: " + where + yamlUnrecognizedError)
	}
	if message == yamlInvalidBase64 {
		located, expected := locateYAMLScalar(data, target, func(node *yaml.Node) bool {
			if node.ShortTag() != "!!binary" {
				return false
			}
			_, decodeErr := base64.StdEncoding.DecodeString(node.Value)
			return decodeErr != nil
		})
		return errors.New("yaml: " + located + yamlInvalidBase64 + expected)
	}
	if yamlFixedProblems[message] || yamlFixedForm.MatchString(message) {
		return err
	}
	if match := yamlResolverFailure.FindStringSubmatch(message); match != nil {
		value, tag := match[2], match[3]
		located, expected := locateYAMLScalar(data, target, func(node *yaml.Node) bool {
			return node.Value == value && node.ShortTag() == tag
		})
		return errors.New("yaml: " + located + "cannot decode " + redactYAMLTag(match[1]) + " as a " + redactYAMLTag(tag) + expected)
	}
	if match := yamlUnknownAnchor.FindStringSubmatch(message); match != nil {
		located, expected := locateYAMLAlias(data, target, match[1])
		return errors.New("yaml: " + located + "unknown anchor referenced" + expected)
	}
	if yamlSelfAnchor.MatchString(message) {
		return errors.New("yaml: " + where + "anchor value contains itself")
	}
	if yamlInvalidMapKey.MatchString(message) {
		return errors.New("yaml: " + where + "invalid map key")
	}
	return errors.New("yaml: " + where + yamlUnrecognizedError)
}

// redactYAMLTag keeps a core tag. Any other tag is document text.
func redactYAMLTag(tag string) string {
	if yamlCoreTags[tag] {
		return tag
	}
	return "value"
}

// locateYAMLScalar finds the first scalar match accepts, in document order, and
// returns its "line N: " prefix and its " (expected T)" suffix. Either is empty
// when the document does not show it. yaml.v3 stops on a bad tagged scalar
// before it reaches the Go target, so the type comes from decoding again with
// that scalar as an empty string.
func locateYAMLScalar(data []byte, target any, match func(*yaml.Node) bool) (string, string) {
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return "", ""
	}
	node := firstYAMLScalar(&root, match)
	if node == nil {
		return "", ""
	}
	node.Tag, node.Value = "!!str", ""
	return yamlLinePrefix(node.Line), expectedYAMLType(&root, node.Line, target)
}

// firstYAMLScalar walks the document in order. An alias is not followed, because
// its anchor is already in the document as its own node.
func firstYAMLScalar(node *yaml.Node, match func(*yaml.Node) bool) *yaml.Node {
	if node.Kind == yaml.ScalarNode && node.Tag != "" && match(node) {
		return node
	}
	for _, child := range node.Content {
		if found := firstYAMLScalar(child, match); found != nil {
			return found
		}
	}
	return nil
}

// locateYAMLAlias finds the alias token yaml.v3 refused and returns the same
// prefix and suffix as locateYAMLScalar. The parser reports no position for an
// unknown anchor, so each textual occurrence is tried as the alias: replacing
// the real one with an empty string clears the error, and an occurrence in a
// comment or a quoted string does not. With no match, both results are empty.
func locateYAMLAlias(data []byte, target any, name string) (string, string) {
	token := []byte("*" + name)
	refused := "yaml: unknown anchor '" + name + "' referenced"
	offset := 0
	for range yamlMaxAliasOccurrences {
		index := bytes.Index(data[offset:], token)
		if index < 0 {
			return "", ""
		}
		start := offset + index
		offset = start + len(token)
		if offset < len(data) && isYAMLAnchorByte(data[offset]) {
			continue
		}
		candidate := make([]byte, 0, len(data))
		candidate = append(append(append(candidate, data[:start]...), `""`...), data[offset:]...)
		var root yaml.Node
		err := yaml.Unmarshal(candidate, &root)
		line := 1 + bytes.Count(data[:start], []byte("\n"))
		switch {
		case err == nil:
			return yamlLinePrefix(line), expectedYAMLType(&root, line, target)
		case err.Error() != refused && strings.HasPrefix(err.Error(), "yaml: unknown anchor '"):
			return yamlLinePrefix(line), ""
		}
	}
	return "", ""
}

// isYAMLAnchorByte matches the bytes yaml.v3's scanner accepts in an anchor name.
func isYAMLAnchorByte(b byte) bool {
	return b >= '0' && b <= '9' || b >= 'A' && b <= 'Z' || b >= 'a' && b <= 'z' || b == '_' || b == '-'
}

func yamlLinePrefix(line int) string {
	return "line " + strconv.Itoa(line) + ": "
}

// expectedYAMLType decodes the repaired document into a fresh target and returns
// " (expected T)" from the type error on line. It returns nothing when that line
// has no type error, as for a string target.
func expectedYAMLType(root *yaml.Node, line int, target any) string {
	targetType := reflect.TypeOf(target)
	if targetType == nil || targetType.Kind() != reflect.Pointer {
		return ""
	}
	var typeError *yaml.TypeError
	if !errors.As(root.Decode(reflect.New(targetType.Elem()).Interface()), &typeError) {
		return ""
	}
	for _, entry := range typeError.Errors {
		match := yamlCannotUnmarshal.FindStringSubmatch(entry)
		if match != nil && match[1] == strconv.Itoa(line) {
			return " (expected " + match[3] + ")"
		}
	}
	return ""
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
		return "line " + match[1] + ": cannot unmarshal " + redactYAMLTag(match[2]) + " into " + match[3]
	}
	if match := yamlFieldProblem.FindStringSubmatch(entry); match != nil {
		return "line " + match[1] + ": field" + nameableYAMLKey(match[2]) + " " + match[3] + " in type " + match[4]
	}
	if match := yamlDuplicateKey.FindStringSubmatch(entry); match != nil {
		return "line " + match[1] + ": mapping key" + nameableYAMLKey(match[2]) + " already defined at line " + match[3]
	}
	if match := yamlEntryLine.FindStringSubmatch(entry); match != nil {
		return "line " + match[1] + ": " + yamlUnrecognizedError
	}
	return yamlUnrecognizedError
}

// nameableYAMLKey returns the key with a leading space when it may be printed,
// and nothing otherwise.
func nameableYAMLKey(key string) string {
	if len(key) > yamlNameableKeyLength || !yamlNameableKey.MatchString(key) {
		return ""
	}
	return " " + key
}
