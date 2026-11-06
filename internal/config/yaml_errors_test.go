package config

import (
	"encoding"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Fake misplaced values. yaml.v3 prints the first in full and the second by its
// first 7 bytes.
const (
	misplacedShortValue  = "Zq7Kp3x-pw"
	misplacedLongValue   = "Yt5Rm8w-fixture-secret-value"
	misplacedLongPrinted = "Yt5Rm8w"
)

// requireNoMisplacedText fails without echoing the error, because the error
// may hold the fixture it is checked for.
func requireNoMisplacedText(t *testing.T, err error, printed ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("decode returned no error")
	}
	for _, text := range printed {
		if strings.Contains(err.Error(), text) {
			t.Fatalf("error holds misplaced text of %d bytes", len(text))
		}
	}
}

// A value misplaced under a typed key is gone from the ParseDeployment error,
// and its line and expected type remain, wherever the key sits in the document
// (req-security-boundaries REQ-2).
func TestParseDeploymentTypeErrorDropsMisplacedValue(t *testing.T) {
	for _, tc := range []struct {
		name, template, want string
	}{
		{name: "top-level int", template: "workspace: demo\nmax_bounces: %VALUE%\n", want: "line 2: cannot unmarshal !!str into int"},
		{name: "struct", template: "workspace: demo\n\ndatabase: %VALUE%\n", want: "line 3: cannot unmarshal !!str into config.Database"},
		{name: "sequence", template: "repos: %VALUE%\n", want: "line 1: cannot unmarshal !!str into []config.Repo"},
		{name: "nested int", template: "database:\n  backend: postgres\n  max_conns: %VALUE%\n", want: "line 3: cannot unmarshal !!str into int"},
		{name: "through a custom unmarshaler", template: "recovery:\n  automatic_recoveries: %VALUE%\n", want: "line 2: cannot unmarshal !!str into int"},
		{name: "execution policy", template: "execution:\n  review_concurrency: %VALUE%\n", want: "line 2: cannot unmarshal !!str into int"},
		{name: "quoted value", template: "max_bounces: \"%VALUE%\"\n", want: "line 1: cannot unmarshal !!str into int"},
		{name: "value posing as the target type", template: "max_bounces: \"%VALUE% into string\"\n", want: "line 1: cannot unmarshal !!str into int"},
		{name: "multi-line value", template: "max_bounces: |\n  %VALUE%\n  into string\n", want: "line 1: cannot unmarshal !!str into int"},
		{name: "custom tag", template: "max_bounces: !%VALUE% x\n", want: "line 1: cannot unmarshal value into int"},
	} {
		for _, value := range []struct{ name, text, printed string }{
			{name: "short", text: misplacedShortValue, printed: misplacedShortValue},
			{name: "long", text: misplacedLongValue, printed: misplacedLongPrinted},
		} {
			t.Run(tc.name+"/"+value.name, func(t *testing.T) {
				unsetEnv(t, "CONVEYOR_DATABASE_URL")
				document := strings.ReplaceAll(tc.template, "%VALUE%", value.text)
				_, err := ParseDeployment([]byte(document), "deployment.yaml", nil)
				requireNoMisplacedText(t, err, value.printed)
				if !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "parse deployment.yaml: ") {
					t.Fatalf("error = %v, want %q", err, tc.want)
				}
				var typeError *yaml.TypeError
				if !errors.As(err, &typeError) {
					t.Fatalf("error %v no longer wraps *yaml.TypeError", err)
				}
			})
		}
	}
}

// Every entry of a many-entry error is rewritten.
func TestDecodeKnownRewritesEveryTypeErrorEntry(t *testing.T) {
	var document WorkspaceDocument
	err := decodeKnown([]byte("max_bounces: "+misplacedShortValue+"\nrepos: "+misplacedLongValue+"\n"), &document)
	requireNoMisplacedText(t, err, misplacedShortValue, misplacedLongPrinted)
	var typeError *yaml.TypeError
	if !errors.As(err, &typeError) || len(typeError.Errors) != 2 {
		t.Fatalf("error = %v, want two entries", err)
	}
	want := []string{"line 1: cannot unmarshal !!str into int", "line 2: cannot unmarshal !!str into []config.Repo"}
	if !reflect.DeepEqual(typeError.Errors, want) {
		t.Fatalf("entries = %q, want %q", typeError.Errors, want)
	}
}

// Every loader over a configuration document gets the fix, including the
// decodes that run before decodeKnown.
func TestConfigLoadersDropMisplacedValue(t *testing.T) {
	deployment := &Config{Workspace: "demo"}
	clientPath := func(t *testing.T, document string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "conveyor.yaml")
		if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	loaders := map[string]func(*testing.T, string) error{
		"ParseDeployment": func(_ *testing.T, document string) error {
			_, err := ParseDeployment([]byte(document), "deployment.yaml", nil)
			return err
		},
		"LoadDeployment": func(t *testing.T, document string) error {
			_, err := LoadDeployment(clientPath(t, document), nil)
			return err
		},
		"LoadClient": func(t *testing.T, document string) error {
			_, err := LoadClient(clientPath(t, document), nil)
			return err
		},
		"ParseWorkspaceDocument": func(_ *testing.T, document string) error {
			_, err := ParseWorkspaceDocument([]byte(document), deployment, "workspace")
			return err
		},
		"ParseStoredWorkspaceDocument": func(_ *testing.T, document string) error {
			_, _, err := ParseStoredWorkspaceDocument([]byte(document), deployment, "stored")
			return err
		},
		"ReadClockPolicy": func(_ *testing.T, document string) error {
			_, _, err := ReadClockPolicy([]byte(document))
			return err
		},
	}
	documents := map[string]string{
		"typed key":     "recovery: %VALUE%\n",
		"root scalar":   "%VALUE%\n",
		"unknown alias": "workspace: *%VALUE%\n",
	}
	for loaderName, load := range loaders {
		for documentName, template := range documents {
			t.Run(loaderName+"/"+documentName, func(t *testing.T) {
				unsetEnv(t, "CONVEYOR_DATABASE_URL")
				err := load(t, strings.ReplaceAll(template, "%VALUE%", misplacedShortValue))
				requireNoMisplacedText(t, err, misplacedShortValue)
				err = load(t, strings.ReplaceAll(template, "%VALUE%", misplacedLongValue))
				requireNoMisplacedText(t, err, misplacedLongPrinted)
			})
		}
	}
}

// The yaml.v3 errors outside *yaml.TypeError that quote an anchor or map key
// lose it and keep their fixed words.
func TestRedactYAMLErrorDropsQuotedDocumentText(t *testing.T) {
	var root map[string]any
	for _, tc := range []struct{ name, document, want string }{
		{name: "value read as an alias", document: "url: *" + misplacedShortValue + "\n", want: "yaml: line 1: unknown anchor referenced"},
		{name: "self-referencing anchor", document: "base: &" + misplacedShortValue + " {<<: *" + misplacedShortValue + "}\n", want: "anchor value contains itself"},
		{name: "sequence as a map key", document: "execution: {[" + misplacedShortValue + "]: 1}\n", want: "yaml: invalid map key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := unmarshalYAML([]byte(tc.document), &root)
			requireNoMisplacedText(t, err, misplacedShortValue)
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
	if err := redactYAMLError(nil, nil, nil); err != nil {
		t.Fatalf("nil error became %v", err)
	}
	if err := unmarshalYAML([]byte("a: {b: 1\n"), &root); err == nil || !strings.Contains(err.Error(), "line ") {
		t.Fatalf("syntax error = %v, want its line", err)
	}
	var document WorkspaceDocument
	if err := decodeKnown(nil, &document); !errors.Is(err, io.EOF) {
		t.Fatalf("empty document error = %v, want io.EOF", err)
	}
}

// Each recognized form keeps what it may keep, and every other form keeps only
// its line number.
func TestRedactYAMLTypeEntry(t *testing.T) {
	for _, tc := range []struct{ name, entry, want string }{
		{name: "scalar in full", entry: "line 3: cannot unmarshal !!str `hunter2` into int", want: "line 3: cannot unmarshal !!str into int"},
		{name: "scalar by prefix", entry: "line 3: cannot unmarshal !!str `hunter2...` into int", want: "line 3: cannot unmarshal !!str into int"},
		{name: "no value for a sequence", entry: "line 4: cannot unmarshal !!seq into string", want: "line 4: cannot unmarshal !!seq into string"},
		{name: "interface map target", entry: "line 1: cannot unmarshal !!str `hunter2` into map[string]interface {}", want: "line 1: cannot unmarshal !!str into map[string]interface {}"},
		{name: "pointer target", entry: "line 1: cannot unmarshal !!str `hunter2` into *int", want: "line 1: cannot unmarshal !!str into *int"},
		{name: "value holds the separator", entry: "line 1: cannot unmarshal !!str `a into b` into int", want: "line 1: cannot unmarshal !!str into int"},
		{name: "value holds a newline", entry: "line 1: cannot unmarshal !!str `a\nb` into int", want: "line 1: cannot unmarshal !!str into int"},
		{name: "custom tag", entry: "line 2: cannot unmarshal !hunter2 `x` into int", want: "line 2: cannot unmarshal value into int"},
		{name: "custom secondary tag", entry: "line 2: cannot unmarshal !!strhunter2 `x` into int", want: "line 2: cannot unmarshal value into int"},
		{name: "target that is not a type", entry: "line 2: cannot unmarshal !!str `x` into hunter2 value", want: "line 2: unrecognized YAML decode error"},

		{name: "lowercase unknown field", entry: "line 5: field require_review_evidence not found in type config.ExecutionPolicy", want: "line 5: field require_review_evidence not found in type config.ExecutionPolicy"},
		{name: "hyphenated unknown field", entry: "line 5: field pack-dir not found in type config.Config", want: "line 5: field pack-dir not found in type config.Config"},
		{name: "unknown field holding a digit", entry: "line 5: field hunter2 not found in type config.Config", want: "line 5: field not found in type config.Config"},
		{name: "unknown field holding uppercase", entry: "line 5: field Hunter not found in type config.Config", want: "line 5: field not found in type config.Config"},
		{name: "unknown field holding punctuation", entry: "line 5: field postgres://u:p@h/db not found in type config.Config", want: "line 5: field not found in type config.Config"},
		{name: "unknown field holding a space", entry: "line 5: field two words not found in type config.Config", want: "line 5: field not found in type config.Config"},
		{name: "unknown field posing as a form", entry: "line 5: field x not found in type int not found in type config.Config", want: "line 5: field not found in type config.Config"},
		{name: "overlong unknown field", entry: "line 5: field " + strings.Repeat("a", 65) + " not found in type config.Config", want: "line 5: field not found in type config.Config"},
		{name: "longest nameable field", entry: "line 5: field " + strings.Repeat("a", 64) + " not found in type config.Config", want: "line 5: field " + strings.Repeat("a", 64) + " not found in type config.Config"},
		{name: "field set twice", entry: "line 6: field workspace already set in type config.Config", want: "line 6: field workspace already set in type config.Config"},
		{name: "secret field set twice", entry: "line 6: field hunter2 already set in type config.Config", want: "line 6: field already set in type config.Config"},

		{name: "duplicate key", entry: `line 7: mapping key "workspace" already defined at line 2`, want: "line 7: mapping key workspace already defined at line 2"},
		{name: "duplicate secret key", entry: `line 7: mapping key "hunter2" already defined at line 2`, want: "line 7: mapping key already defined at line 2"},
		{name: "duplicate quoted key", entry: `line 7: mapping key "a\nb\"c" already defined at line 2`, want: "line 7: mapping key already defined at line 2"},

		{name: "other form with a line", entry: "line 9: some future form quoting hunter2", want: "line 9: unrecognized YAML decode error"},
		{name: "other form without a line", entry: "some future form quoting hunter2", want: "unrecognized YAML decode error"},
		{name: "empty entry", entry: "", want: "unrecognized YAML decode error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := redactYAMLTypeEntry(tc.entry); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// The real decoder's key forms pass through the same rewrite: a duplicate key
// and an unknown key each keep their lines, and a secret-shaped key is dropped.
func TestDecodeKnownKeyFormsFromTheDecoder(t *testing.T) {
	var document WorkspaceDocument
	for _, tc := range []struct{ name, document, want string }{
		{name: "unknown word", document: "workspace: demo\nbudget_usd: 1\n", want: "line 2: field budget_usd not found in type config.WorkspaceDocument"},
		{name: "unknown secret-shaped key", document: "workspace: demo\n" + misplacedShortValue + ": 1\n", want: "line 2: field not found in type config.WorkspaceDocument"},
		{name: "duplicate word", document: "workspace: demo\nworkspace: other\n", want: "line 2: mapping key workspace already defined at line 1"},
		{name: "duplicate secret-shaped key", document: misplacedShortValue + ": 1\n" + misplacedShortValue + ": 2\n", want: "line 2: mapping key already defined at line 1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := decodeKnown([]byte(tc.document), &document)
			requireNoMisplacedText(t, err, misplacedShortValue)
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

// Every type a configuration document decodes into matches yamlTypeShape, so a
// real target type never falls to the line-only form.
func TestConfigDecodeTargetTypesMatchRedactedShape(t *testing.T) {
	shape := regexp.MustCompile("^" + yamlTypeShape + "$")
	seen := map[reflect.Type]bool{}
	var walk func(reflect.Type)
	walk = func(current reflect.Type) {
		if seen[current] {
			return
		}
		seen[current] = true
		if !shape.MatchString(current.String()) {
			t.Errorf("type %s does not match the redacted type shape", current)
		}
		switch current.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Array:
			walk(current.Elem())
		case reflect.Map:
			walk(current.Key())
			walk(current.Elem())
		case reflect.Struct:
			for i := range current.NumField() {
				field := current.Field(i)
				if field.IsExported() && field.Tag.Get("yaml") != "-" {
					walk(field.Type)
				}
			}
		}
	}
	walk(reflect.TypeOf(Config{}))
	walk(reflect.TypeOf(WorkspaceDocument{}))
	if len(seen) < 10 {
		t.Fatalf("walk reached %d types", len(seen))
	}
	// yaml.v3 hands a decode target's own error text straight to fail(), so these
	// are the only custom routes from a scalar to a message. The two unmarshalers
	// return only yaml.v3's errors, and redactYAMLError rewrites those and
	// time.Time's.
	textUnmarshaler := reflect.TypeFor[encoding.TextUnmarshaler]()
	yamlUnmarshaler := reflect.TypeFor[yaml.Unmarshaler]()
	var custom, text []string
	for current := range seen {
		pointer := reflect.PointerTo(current)
		if current.Implements(textUnmarshaler) || pointer.Implements(textUnmarshaler) {
			text = append(text, current.String())
		}
		if current.Implements(yamlUnmarshaler) || pointer.Implements(yamlUnmarshaler) {
			custom = append(custom, current.String())
		}
	}
	if want := []string{"time.Time"}; !reflect.DeepEqual(text, want) {
		t.Errorf("TextUnmarshaler types = %v, want %v", text, want)
	}
	sort.Strings(custom)
	if want := []string{"config.ExecutionPolicy", "config.RecoveryPolicy"}; !reflect.DeepEqual(custom, want) {
		t.Errorf("custom yaml.Unmarshaler types = %v, want %v", custom, want)
	}
}

// A tagged value is gone from the ParseDeployment error, whichever tag carries
// it, and its line and expected type remain. Each resolver case first shows raw
// yaml.v3 printing the fixture, so the test exercises the real leak.
func TestParseDeploymentTaggedValueDropsMisplacedValue(t *testing.T) {
	const value = misplacedShortValue
	for _, tc := range []struct {
		name, line, want string
		rawLeak          bool
	}{
		{name: "int tag", line: "max_bounces: !!int " + value, want: "line 2: cannot decode !!str as a !!int (expected int)", rawLeak: true},
		{name: "float tag", line: "max_bounces: !!float " + value, want: "line 2: cannot decode !!str as a !!float (expected int)", rawLeak: true},
		{name: "bool tag", line: "max_bounces: !!bool " + value, want: "line 2: cannot decode !!str as a !!bool (expected int)", rawLeak: true},
		{name: "null tag", line: "max_bounces: !!null " + value, want: "line 2: cannot decode !!str as a !!null (expected int)", rawLeak: true},
		{name: "timestamp tag", line: "max_bounces: !!timestamp " + value, want: "line 2: cannot decode !!str as a !!timestamp (expected int)", rawLeak: true},
		{name: "long form tag", line: "max_bounces: !<tag:yaml.org,2002:int> " + value, want: "line 2: cannot decode !!str as a !!int (expected int)", rawLeak: true},
		{name: "quoted value", line: `max_bounces: !!int "` + value + `"`, want: "line 2: cannot decode !!str as a !!int (expected int)", rawLeak: true},
		{name: "under a string key", line: "workspace: !!int " + value, want: "line 2: cannot decode !!str as a !!int", rawLeak: true},
		{name: "through a custom unmarshaler", line: "recovery: {window: !!bool " + value + "}", want: "line 2: cannot decode !!str as a !!bool", rawLeak: true},
		{name: "through a nested custom unmarshaler", line: "execution: {review_concurrency: !!int " + value + "}", want: "line 2: cannot decode !!str as a !!int (expected int)", rawLeak: true},
		{name: "as a map key", line: "database: {!!int " + value + ": 1}", want: "line 2: cannot decode !!str as a !!int", rawLeak: true},
		{name: "binary tag", line: "max_bounces: !!binary " + value, want: "line 2: !!binary value contains invalid base64 data (expected int)"},
		{name: "custom tag", line: "max_bounces: !custom " + value, want: "line 2: cannot unmarshal value into int"},
		{name: "custom tag holding the value", line: "max_bounces: !" + value + " x", want: "line 2: cannot unmarshal value into int"},
		{name: "custom secondary tag", line: "max_bounces: !!custom " + value, want: "line 2: cannot unmarshal value into int"},
		{name: "merge tag", line: "max_bounces: !!merge " + value, want: "line 2: cannot unmarshal !!merge into int"},
		{name: "str tag", line: "max_bounces: !!str " + value, want: "line 2: cannot unmarshal !!str into int"},
		{name: "undefined alias", line: "max_bounces: *" + value, want: "line 2: unknown anchor referenced (expected int)"},
		{name: "undefined alias under a string key", line: "workspace: *" + value, want: "line 2: unknown anchor referenced"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			unsetEnv(t, "CONVEYOR_DATABASE_URL")
			document := []byte("# leading comment\n" + tc.line + "\n")
			var raw map[string]any
			if rawErr := yaml.Unmarshal(document, &raw); tc.rawLeak && (rawErr == nil || !strings.Contains(rawErr.Error(), value)) {
				t.Fatal("control: raw yaml.v3 no longer prints this value")
			}
			_, err := ParseDeployment(document, "deployment.yaml", nil)
			requireNoMisplacedText(t, err, value)
			if !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "parse deployment.yaml: ") {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

// An aliased scalar reaches the resolver at its anchor, so the error names the
// anchor's line. A later occurrence of an undefined alias's name in a comment or
// a quoted string does not move the line, nor does an earlier one.
func TestParseDeploymentAliasedValueDropsMisplacedValue(t *testing.T) {
	const value = misplacedShortValue
	for _, tc := range []struct{ name, document, want string }{
		{name: "alias of a tagged anchor", document: "base: &anchor !!int " + value + "\nmax_bounces: *anchor\n", want: "line 1: cannot decode !!str as a !!int"},
		{name: "merge of a tagged anchor", document: "base: &anchor {review_concurrency: !!int " + value + "}\nexecution: {<<: *anchor}\n", want: "line 1: cannot decode !!str as a !!int"},
		{name: "alias of a plain anchor", document: "base: &anchor " + value + "\nmax_bounces: *anchor\n", want: "line 1: field base not found"},
		{name: "undefined alias after a comment naming it", document: "# *" + value + "\nmax_bounces: *" + value + "\n", want: "line 2: unknown anchor referenced (expected int)"},
		{name: "undefined alias after a string naming it", document: "workspace: \"*" + value + "\"\nmax_bounces: *" + value + "\n", want: "line 2: unknown anchor referenced (expected int)"},
		{name: "name that extends the alias", document: "workspace: *" + value + "x\nmax_bounces: *" + value + "\n", want: "line 1: unknown anchor referenced"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			unsetEnv(t, "CONVEYOR_DATABASE_URL")
			_, err := ParseDeployment([]byte(tc.document), "deployment.yaml", nil)
			requireNoMisplacedText(t, err, value)
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

// A value misplaced under the one time.Time key reaches time.ParseError, which
// prints it whole. The control shows raw yaml.v3 doing so, and a real timestamp
// still decodes.
func TestTimeKeyDropsMisplacedValue(t *testing.T) {
	const value = misplacedShortValue
	document := "workspace: demo\nissue_publication:\n  enabled_at: %s\n"
	var raw WorkspaceDocument
	if rawErr := yaml.Unmarshal(fmt.Appendf(nil, document, value), &raw); rawErr == nil || !strings.Contains(rawErr.Error(), value) {
		t.Fatal("control: raw yaml.v3 no longer prints this value")
	}
	const want = "line 3: cannot unmarshal value into time.Time"
	t.Run("ParseDeployment", func(t *testing.T) {
		unsetEnv(t, "CONVEYOR_DATABASE_URL")
		_, err := ParseDeployment(fmt.Appendf(nil, document, value), "deployment.yaml", nil)
		requireNoMisplacedText(t, err, value)
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %v, want %q", err, want)
		}
	})
	t.Run("decodeKnown", func(t *testing.T) {
		var decoded WorkspaceDocument
		err := decodeKnown(fmt.Appendf(nil, document, value), &decoded)
		requireNoMisplacedText(t, err, value)
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %v, want %q", err, want)
		}
	})
	t.Run("control", func(t *testing.T) {
		var decoded WorkspaceDocument
		if err := decodeKnown(fmt.Appendf(nil, document, "2026-11-06T06:00:00Z"), &decoded); err != nil || decoded.IssuePublication.EnabledAt.IsZero() {
			t.Fatalf("decoded=%v err=%v", decoded.IssuePublication.EnabledAt, err)
		}
	})
}

// A well-formed tagged value decodes as before, and a valid alias resolves, so
// the rewrite refuses nothing the decoder accepted.
func TestParseDeploymentTaggedValueControl(t *testing.T) {
	var decoded struct {
		MaxBounces int    `yaml:"max_bounces"`
		Other      int    `yaml:"other"`
		Name       string `yaml:"name"`
	}
	document := "max_bounces: !!int 3\nother: &n 4\nname: !!str text\n"
	if err := unmarshalYAML([]byte(document), &decoded); err != nil || decoded.MaxBounces != 3 || decoded.Other != 4 || decoded.Name != "text" {
		t.Fatalf("decoded=%+v err=%v", decoded, err)
	}
	var workspace WorkspaceDocument
	if err := decodeKnown([]byte("max_bounces: !!int 3\n"), &workspace); err != nil || workspace.MaxBounces != 3 {
		t.Fatalf("strict decoded=%d err=%v", workspace.MaxBounces, err)
	}
}

// Every failure site in the pinned yaml.v3 is either in yamlFixedProblems or has
// a row here, and each row's sample carries a fixture the rewrite must drop.
// Reading the module source makes an upgrade fail this test until the list is
// reviewed, though an unreviewed form still keeps only its line.
func TestYAMLFailureFormsMatchPinnedSource(t *testing.T) {
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Skip("the go tool is unavailable")
	}
	output, err := exec.Command(goTool, "list", "-m", "-f", "{{.Version}} {{.Dir}}", "gopkg.in/yaml.v3").Output()
	if err != nil {
		t.Fatal(err)
	}
	version, dir, ok := strings.Cut(strings.TrimSpace(string(output)), " ")
	if !ok || version != "v3.0.1" {
		t.Fatalf("yaml.v3 is %q; review every failure form in this file before changing the pin", output)
	}
	read := func(name string) string {
		t.Helper()
		data, readErr := os.ReadFile(filepath.Join(dir, name))
		if readErr != nil {
			t.Fatal(readErr)
		}
		return string(data)
	}

	variable := map[string]struct{ sample, want string }{
		"unknown anchor '%s' referenced":             {sample: "yaml: unknown anchor 'SECRET' referenced", want: "yaml: unknown anchor referenced"},
		"anchor '%s' value contains itself":          {sample: "yaml: anchor 'SECRET' value contains itself", want: "yaml: anchor value contains itself"},
		"invalid map key: %#v":                       {sample: `yaml: invalid map key: []interface {}{"SECRET"}`, want: "yaml: invalid map key"},
		"cannot decode %s `%s` as a %s":              {sample: "yaml: cannot decode !!str `SECRET` as a !!int", want: "yaml: cannot decode !!str as a !!int"},
		"%s%s":                                       {sample: "yaml: line 4: SECRET", want: "yaml: line 4: " + yamlUnrecognizedError},
		"cannot decode node with unknown kind %d":    {sample: "yaml: cannot decode node with unknown kind 9", want: "yaml: cannot decode node with unknown kind 9"},
		"invalid array: want %d elements but got %d": {sample: "yaml: invalid array: want 1 elements but got 2", want: "yaml: invalid array: want 1 elements but got 2"},
	}
	failf := regexp.MustCompile(`\bfailf\(\s*"((?:[^"\\]|\\.)*)"`)
	seen := map[string]bool{}
	for _, name := range []string{"decode.go", "resolve.go"} {
		for _, match := range failf.FindAllStringSubmatch(read(name), -1) {
			format := match[1]
			seen[format] = true
			if _, known := variable[format]; !known && !yamlFixedProblems[format] {
				t.Errorf("%s failf form %q is not classified", name, format)
			}
		}
	}
	for format, row := range variable {
		if !seen[format] {
			t.Errorf("failf form %q is not in the pinned source", format)
		}
		got := redactYAMLError(errors.New(row.sample), nil, nil)
		if got == nil || got.Error() != row.want {
			t.Errorf("form %q rewrote to %v, want %q", format, got, row.want)
		}
	}

	problemCall := regexp.MustCompile(`(?s)yaml_parser_set_(scanner_error|scanner_tag_error|parser_error|parser_error_context|reader_error)\(parser,(.*?)\)\n`)
	literal := regexp.MustCompile(`"((?:[^"\\]|\\.)*)"`)
	dynamic := 0
	for _, name := range []string{"scannerc.go", "parserc.go", "readerc.go"} {
		for _, call := range problemCall.FindAllStringSubmatch(read(name), -1) {
			arguments := call[2]
			literals := literal.FindAllStringSubmatch(arguments, -1)
			switch {
			case strings.Contains(arguments, "max_flow_level") || strings.Contains(arguments, "max_indents"):
				dynamic++
				continue
			case strings.Contains(arguments, "err.Error()"):
				dynamic++
				continue
			case len(literals) == 0:
				continue // the scanner_tag_error wrapper forwards its own parameters
			}
			problem := literals[0][1]
			if call[1] == "scanner_error" || call[1] == "scanner_tag_error" || call[1] == "parser_error_context" {
				problem = literals[len(literals)-1][1]
			}
			seen[problem] = true
			if !yamlFixedProblems[problem] {
				t.Errorf("%s problem %q is not in yamlFixedProblems", name, problem)
			}
		}
	}
	if dynamic != 3 {
		t.Errorf("found %d computed problems, want the two depth limits and the reader's input error", dynamic)
	}
	// parser.fail() prints this when a parser sets no problem.
	if strings.Contains(read("decode.go"), `"unknown problem parsing YAML content"`) {
		seen["unknown problem parsing YAML content"] = true
	}
	for problem := range yamlFixedProblems {
		if !seen[problem] {
			t.Errorf("yamlFixedProblems entry %q is not in the pinned source", problem)
		}
	}
	for _, sample := range []string{"yaml: exceeded max depth of 10000", "yaml: line 3: expected scalar event but got mapping start"} {
		if got := redactYAMLError(errors.New(sample), nil, nil); got.Error() != sample {
			t.Errorf("fixed form %q rewrote to %q", sample, got)
		}
	}
}

// Anything the rewrite does not recognize keeps only its line, including the
// reader's input error, an error from outside yaml.v3, and a look-alike of a
// fixed form that carries extra text.
func TestRedactYAMLErrorDeniesByDefault(t *testing.T) {
	const secret = "SECRET"
	for _, tc := range []struct{ name, message, want string }{
		{name: "unknown form with a line", message: "yaml: line 7: some future form quoting " + secret, want: "yaml: line 7: " + yamlUnrecognizedError},
		{name: "unknown form without a line", message: "yaml: some future form quoting " + secret, want: "yaml: " + yamlUnrecognizedError},
		{name: "reader input error", message: "yaml: input error: read " + secret, want: "yaml: " + yamlUnrecognizedError},
		{name: "not from yaml.v3", message: "custom unmarshaler says " + secret, want: "yaml: " + yamlUnrecognizedError},
		{name: "not from yaml.v3 with a line", message: "line 2: custom unmarshaler says " + secret, want: "yaml: line 2: " + yamlUnrecognizedError},
		{name: "fixed form with a suffix", message: "yaml: did not find expected key " + secret, want: "yaml: " + yamlUnrecognizedError},
		{name: "numeric form with text", message: "yaml: exceeded max depth of " + secret, want: "yaml: " + yamlUnrecognizedError},
		{name: "event form with text", message: "yaml: expected " + secret + " event but got scalar", want: "yaml: " + yamlUnrecognizedError},
		{name: "resolver form with a custom tag", message: "yaml: cannot decode !" + secret + " `" + secret + "` as a !" + secret, want: "yaml: cannot decode value as a value"},
		{name: "resolver form across lines", message: "yaml: cannot decode !!str `a\n" + secret + "` as a !!int", want: "yaml: cannot decode !!str as a !!int"},
		{name: "resolver form posing as its own end", message: "yaml: cannot decode !!str `" + secret + "` as a !!int` as a !!bool", want: "yaml: cannot decode !!str as a !!bool"},
		{name: "walker self-anchor with a line", message: "yaml: line 3: anchor '" + secret + "' value contains itself", want: "yaml: line 3: anchor value contains itself"},
		{name: "fixed text with a line", message: "yaml: line 3: did not find expected key", want: "yaml: line 3: did not find expected key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := redactYAMLError(errors.New(tc.message), nil, nil); got.Error() != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
	eof := fmt.Errorf("wrapped: %w", io.EOF)
	if got := redactYAMLError(eof, nil, nil); !errors.Is(got, io.EOF) {
		t.Fatalf("wrapped io.EOF became %v", got)
	}
}
