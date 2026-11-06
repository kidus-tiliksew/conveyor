package config

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
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

// The two yaml.v3 errors outside *yaml.TypeError that quote document text lose
// it and keep their fixed words.
func TestRedactYAMLErrorDropsQuotedDocumentText(t *testing.T) {
	var root map[string]any
	for _, tc := range []struct{ name, document, want string }{
		{name: "value read as an alias", document: "url: *" + misplacedShortValue + "\n", want: "yaml: unknown anchor referenced"},
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
	if err := redactYAMLError(nil); err != nil {
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
}
