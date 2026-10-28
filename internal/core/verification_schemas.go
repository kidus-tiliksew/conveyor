package core

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/verification"
)

// VerificationEvidenceSchemas describes the same six typed payloads used by
// DecodeVerificationEvidence. Ownership, graph integrity and authenticated
// attribution are runtime constraints, not claims a JSON schema can authorize.
func VerificationEvidenceSchemas() map[string]any {
	payloads := map[string]any{"api_exchange": APIExchangePayload{}, "state_observation": StateObservationPayload{}, "assertion_result": AssertionResultPayload{}, "execution_report": ExecutionReportPayload{}, "visual_capture": VisualCapturePayload{}, "operator_observation": OperatorObservationPayload{}}
	schemas := map[string]any{}
	for kind, payload := range payloads {
		schema := VerificationJSONSchema(reflect.TypeOf(VerificationEvidence{}))
		props := schema["properties"].(map[string]any)
		props["schema_version"] = map[string]any{"type": "integer", "const": 1}
		props["type"] = map[string]any{"type": "string", "const": kind}
		props["payload"] = VerificationJSONSchema(reflect.TypeOf(payload))
		schema["$schema"] = "https://json-schema.org/draft/2020-12/schema"
		schema["x-runtime-validator"] = "core.DecodeVerificationEvidence"
		schema["x-max-bytes"] = MaxVerificationEvidenceBytes
		schemas[kind] = schema
	}
	return schemas
}

// VerificationJSONSchema derives field spelling and unknown-field refusal from
// the Go wire types; callers add operation-specific semantic constraints.
func VerificationJSONSchema(t reflect.Type) map[string]any {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == reflect.TypeOf(time.Time{}) {
		return map[string]any{"type": "string", "format": "date-time"}
	}
	if t == reflect.TypeOf(json.RawMessage{}) {
		return map[string]any{}
	}
	// A required assertion is a bare ID string or an id/description object
	// (component-verification-kit-contract); bare strings stay valid input.
	if t == reflect.TypeOf(verification.Assertion{}) {
		return map[string]any{"anyOf": []map[string]any{
			{"type": "string"},
			{"type": "object", "properties": map[string]any{"id": map[string]any{"type": "string"}, "description": map[string]any{"type": "string"}}, "required": []string{"id"}, "additionalProperties": false},
		}}
	}
	switch t.Kind() {
	case reflect.Struct:
		props := map[string]any{}
		required := []string{}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			tag := strings.Split(f.Tag.Get("json"), ",")
			name := tag[0]
			if name == "-" {
				continue
			}
			if name == "" {
				name = f.Name
			}
			props[name] = VerificationJSONSchema(f.Type)
			if len(tag) == 1 {
				required = append(required, name)
			}
		}
		schema := map[string]any{"type": "object", "properties": props, "required": required, "additionalProperties": false}
		verificationSchemaConstraints(t, schema)
		return schema
	case reflect.Slice, reflect.Array:
		if t.Elem().Kind() == reflect.Uint8 {
			return map[string]any{"type": "string", "contentEncoding": "base64"}
		}
		return map[string]any{"type": []string{"array", "null"}, "items": VerificationJSONSchema(t.Elem())}
	case reflect.Map:
		return map[string]any{"type": []string{"object", "null"}, "additionalProperties": VerificationJSONSchema(t.Elem())}
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Int, reflect.Int64, reflect.Int32, reflect.Uint, reflect.Uint64:
		return map[string]any{"type": "integer"}
	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}
	case reflect.String:
		return map[string]any{"type": "string"}
	}
	return map[string]any{}
}

// ValidateVerificationWire uses the published structural contract before the
// typed decoder. Semantic ownership and lifecycle checks follow in the service.
// A structural mismatch returns *VerificationSchemaError
// (component-verification-evidence).
func ValidateVerificationWire(raw []byte, t reflect.Type) error {
	var value any
	if err := DecodeVerificationRequest(raw, &value); err != nil {
		return err
	}
	return validateVerificationSchema(value, VerificationJSONSchema(t))
}

// VerificationSchemaError names the JSON path and the schema keyword of a
// structural mismatch. Path joins schema property names with ".", writes array
// indices as [i] and a map entry as *, so it never holds a caller value.
// Property is the caller's name of an unknown property (keyword
// additionalProperties); Error never prints it, so callers decide whether it
// may be shown (component-verification-service "Correctable refusals").
type VerificationSchemaError struct {
	Path     string
	Keyword  string
	Property string
	// Expected is the schema's own constraint: a type, pattern, format, enum
	// list, constant, or bound.
	Expected string
}

func (e *VerificationSchemaError) Error() string {
	path := e.Path
	if path == "" {
		path = "request"
	}
	return "verification schema mismatch at " + path + ": " + e.Keyword
}

// Rule is the server sentence for the keyword. It quotes only the schema's
// expected constraint, never the caller's value.
func (e *VerificationSchemaError) Rule() string {
	switch e.Keyword {
	case "additionalProperties":
		return "the tool's input schema defines no property with this name; remove it or correct its spelling."
	case "required":
		return "this required property is absent."
	case "type":
		return "the value must have JSON type " + e.Expected + "."
	case "pattern":
		return "the value must match the schema pattern " + strconv.Quote(e.Expected) + verificationPatternMeaning(e.Expected) + "."
	case "format":
		return "the value must be an RFC 3339 " + e.Expected + "."
	case "enum":
		return "the value must be one of: " + e.Expected + "."
	case "const":
		return "the value must equal " + e.Expected + "."
	case "minimum":
		return "the value must be at least " + e.Expected + "."
	case "maximum":
		return "the value must be at most " + e.Expected + "."
	case "minItems":
		return "the list must hold at least " + e.Expected + " items."
	case "oneOf":
		return "the value must match exactly one alternative of the tool's input schema; it matches " + e.Expected + "."
	case "anyOf":
		return "the value matches none of the alternatives of the tool's input schema."
	}
	return "the value does not match the tool's input schema."
}

func verificationPatternMeaning(pattern string) string {
	switch pattern {
	case `\S`:
		return " (a non-blank text)"
	case "^[a-f0-9]{64}$":
		return " (64 lowercase hexadecimal characters)"
	case "^([a-f0-9]{40}|[a-f0-9]{64})$":
		return " (a 40- or 64-character lowercase hexadecimal object ID)"
	case `(Z|\+00:00)$`:
		return " (a UTC time ending in Z or +00:00)"
	}
	return ""
}

func validateVerificationSchema(v any, s map[string]any) error {
	if err := checkVerificationSchema(v, s, ""); err != nil {
		return err
	}
	return nil
}

func verificationSchemaPath(path, name string) string {
	if path == "" {
		return name
	}
	return path + "." + name
}

// checkVerificationSchema reports the first failure in a fixed order: the type,
// then const, enum, pattern, format, bounds and alternatives, then absent
// required properties in schema order and properties in sorted name order. The
// accepted set is the same whatever the order.
func checkVerificationSchema(v any, s map[string]any, path string) *VerificationSchemaError {
	fail := func(keyword, expected string) *VerificationSchemaError {
		return &VerificationSchemaError{Path: path, Keyword: keyword, Expected: expected}
	}
	kind, _ := s["type"].(string)
	if kind == "" {
		if _, ok := s["properties"]; ok {
			kind = "object"
		}
		if _, ok := s["required"]; ok {
			kind = "object"
		}
	}
	nullable := false
	if types, ok := s["type"].([]string); ok {
		nullable = true
		kind = types[0]
	}
	if !(nullable && v == nil) && !verificationSchemaKind(v, kind) {
		expected := kind
		if nullable {
			expected += " or null"
		}
		return fail("type", expected)
	}
	if expected, ok := s["const"]; ok {
		if n, isInt := expected.(int); isInt {
			expected = float64(n)
		}
		if !reflect.DeepEqual(v, expected) {
			b, _ := json.Marshal(expected)
			return fail("const", string(b))
		}
	}
	if enum, ok := s["enum"].([]string); ok {
		found := false
		for _, x := range enum {
			if v == x {
				found = true
			}
		}
		if !found {
			return fail("enum", strings.Join(enum, ", "))
		}
	}
	if pattern, ok := s["pattern"].(string); ok {
		str, ok := v.(string)
		if !ok || !regexp.MustCompile(pattern).MatchString(str) {
			return fail("pattern", pattern)
		}
	}
	if s["format"] == "date-time" {
		str, ok := v.(string)
		if !ok {
			return fail("format", "date-time")
		}
		if _, err := time.Parse(time.RFC3339Nano, str); err != nil {
			return fail("format", "date-time")
		}
	}
	if n, ok := v.(float64); ok {
		if min, ok := s["minimum"].(int); ok && n < float64(min) {
			return fail("minimum", strconv.Itoa(min))
		}
		if max, ok := s["maximum"].(int); ok && n > float64(max) {
			return fail("maximum", strconv.Itoa(max))
		}
	}
	for _, group := range []string{"oneOf", "anyOf"} {
		if choices, ok := s[group].([]map[string]any); ok {
			passed := 0
			for _, choice := range choices {
				if checkVerificationSchema(v, choice, path) == nil {
					passed++
				}
			}
			if passed == 0 || (group == "oneOf" && passed != 1) {
				return fail(group, fmt.Sprintf("%d of %d alternatives", passed, len(choices)))
			}
		}
	}
	if nullable && v == nil {
		return nil
	}
	switch kind {
	case "object":
		obj := v.(map[string]any)
		props, _ := s["properties"].(map[string]any)
		required, _ := s["required"].([]string)
		for _, key := range required {
			if _, ok := obj[key]; !ok {
				return &VerificationSchemaError{Path: verificationSchemaPath(path, key), Keyword: "required"}
			}
		}
		keys := make([]string, 0, len(obj))
		for key := range obj {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			value := obj[key]
			if child, ok := props[key].(map[string]any); ok {
				if err := checkVerificationSchema(value, child, verificationSchemaPath(path, key)); err != nil {
					return err
				}
			} else if child, ok := s["additionalProperties"].(map[string]any); ok {
				if err := checkVerificationSchema(value, child, verificationSchemaPath(path, "*")); err != nil {
					return err
				}
			} else if s["additionalProperties"] == false {
				return &VerificationSchemaError{Path: path, Keyword: "additionalProperties", Property: key}
			}
		}
	case "array":
		values := v.([]any)
		if min, ok := s["minItems"].(int); ok && len(values) < min {
			return fail("minItems", strconv.Itoa(min))
		}
		child, _ := s["items"].(map[string]any)
		for i, value := range values {
			if err := checkVerificationSchema(value, child, fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
	}
	return nil
}

func verificationSchemaKind(v any, kind string) bool {
	switch kind {
	case "object":
		_, ok := v.(map[string]any)
		return ok
	case "array":
		_, ok := v.([]any)
		return ok
	case "string":
		_, ok := v.(string)
		return ok
	case "integer":
		n, ok := v.(float64)
		return ok && math.Trunc(n) == n
	case "number":
		_, ok := v.(float64)
		return ok
	case "boolean":
		_, ok := v.(bool)
		return ok
	}
	return true
}

func verificationSchemaConstraints(t reflect.Type, s map[string]any) {
	props, ok := s["properties"].(map[string]any)
	if !ok {
		return
	}
	property := func(name string) map[string]any { p, _ := props[name].(map[string]any); return p }
	optional := func(names ...string) {
		var required []string
		for _, name := range s["required"].([]string) {
			skip := false
			for _, omit := range names {
				if name == omit {
					skip = true
				}
			}
			if !skip {
				required = append(required, name)
			}
		}
		s["required"] = required
	}
	nonempty := func(names ...string) {
		for _, name := range names {
			property(name)["pattern"] = `\S`
		}
	}
	enum := func(name string, values ...string) { property(name)["enum"] = values }
	hashes := func(names ...string) {
		for _, name := range names {
			property(name)["pattern"] = "^[a-f0-9]{64}$"
		}
	}
	dates := func(names ...string) {
		for _, name := range names {
			property(name)["format"] = "date-time"
			property(name)["pattern"] = `(Z|\+00:00)$`
		}
	}
	switch t {
	case reflect.TypeOf(VerificationEvidence{}):
		nonempty("id", "submission_key", "workspace_id", "task_id", "work_order_id", "work_order_attempt_id", "context_id", "run_id", "submitted_by")
		dates("captured_at", "received_at")
		optional("artifacts", "submitted_by", "received_at")
		property("submitted_by")["readOnly"] = true
		property("received_at")["readOnly"] = true
		property("revisions")["type"] = "array"
		property("revisions")["minItems"] = 1
		property("governing_pins")["type"] = "array"
		property("safe_inputs")["type"] = "object"
		s["x-runtime-constraints"] = []string{"authenticated submitting/capturing authority", "unique revision, pin and artifact identities", "resolvable same-scope references", "UTC timestamps and ordered time ranges", "retained artifact integrity and media"}
	case reflect.TypeOf(VerificationEnvironment{}):
		nonempty("target", "os", "architecture", "runtime", "deployment")
		optional("attributes")
	case reflect.TypeOf(VerificationCaptureActor{}):
		nonempty("identity", "version")
		enum("kind", "actor", "tool", "operator")
		enum("attribution", "self_reported", "trusted_runner", "authenticated_operator")
	case reflect.TypeOf(VerificationPin{}):
		enum("kind", "requirement", "system_design")
		nonempty("document_id")
		property("version")["minimum"] = 1
	case reflect.TypeOf(VerificationRevision{}):
		nonempty("repository", "remote_identity")
		property("sha")["pattern"] = "^([a-f0-9]{40}|[a-f0-9]{64})$"
	case reflect.TypeOf(VerificationSubject{}):
		enum("kind", "kit", "ordinary")
		s["oneOf"] = []map[string]any{
			{"properties": map[string]any{"kind": map[string]any{"const": "kit"}, "kit_id": map[string]any{"pattern": `\S`}, "kit_version": map[string]any{"pattern": `\S`}, "exercise_id": map[string]any{"pattern": `\S`}, "content_digest": map[string]any{"pattern": "^[a-f0-9]{64}$"}, "obligation_id": map[string]any{"const": ""}, "contract_digest": map[string]any{"const": ""}}, "required": []string{"kit_id", "kit_version", "exercise_id", "content_digest"}},
			{"properties": map[string]any{"kind": map[string]any{"const": "ordinary"}, "obligation_id": map[string]any{"pattern": `\S`}, "contract_digest": map[string]any{"pattern": "^[a-f0-9]{64}$"}, "kit_id": map[string]any{"const": ""}, "kit_version": map[string]any{"const": ""}, "exercise_id": map[string]any{"const": ""}, "content_digest": map[string]any{"const": ""}}, "required": []string{"obligation_id", "contract_digest"}},
		}
	case reflect.TypeOf(VerificationReference{}):
		s["oneOf"] = []map[string]any{
			{"properties": map[string]any{"evidence_id": map[string]any{"pattern": `\S`}, "artifact_id": map[string]any{"const": ""}, "sha256": map[string]any{"pattern": "^([a-f0-9]{64})?$"}}, "required": []string{"evidence_id"}},
			{"properties": map[string]any{"artifact_id": map[string]any{"pattern": `\S`}, "evidence_id": map[string]any{"const": ""}, "sha256": map[string]any{"pattern": "^[a-f0-9]{64}$"}}, "required": []string{"artifact_id", "sha256"}},
		}
	case reflect.TypeOf(VerificationArtifactReference{}):
		nonempty("artifact_id", "media_type")
		hashes("sha256")
	case reflect.TypeOf(APIExchangePayload{}):
		s["oneOf"] = []map[string]any{{"required": []string{"response_status"}, "properties": map[string]any{"transport_error": map[string]any{"const": ""}}}, {"required": []string{"transport_error"}, "properties": map[string]any{"transport_error": map[string]any{"pattern": `\S`}, "response_status": map[string]any{"const": nil}}}}

		nonempty("method", "url", "request_summary", "response_summary")
		dates("request_at", "response_at")
		property("url")["pattern"] = `^https?://[^/@]+`
		property("response_status")["minimum"] = 100
		property("response_status")["maximum"] = 599
		property("request_headers")["type"] = "object"
		property("response_headers")["type"] = "object"
	case reflect.TypeOf(StateObservationPayload{}):
		nonempty("target", "method")
		dates("captured_at")
		s["anyOf"] = []map[string]any{{"required": []string{"value"}}, {"required": []string{"artifact"}}}
	case reflect.TypeOf(AssertionResultPayload{}):
		nonempty("assertion_id", "text", "expected", "actual")
		enum("outcome", "pass", "fail", "unknown")
		property("supporting")["type"] = "array"
		property("supporting")["minItems"] = 1
	case reflect.TypeOf(ExecutionReportPayload{}):
		s["anyOf"] = []map[string]any{{"required": []string{"exit_code"}}, {"required": []string{"signal"}, "properties": map[string]any{"signal": map[string]any{"pattern": `\S`}}}, {"properties": map[string]any{"timed_out": map[string]any{"const": true}}}, {"properties": map[string]any{"cancelled": map[string]any{"const": true}}}}

		nonempty("tool", "tool_version", "runtime")
		dates("started_at", "ended_at")
		hashes("stdout_sha256", "stderr_sha256")
		property("argv")["type"] = "array"
		property("argv")["minItems"] = 1
	case reflect.TypeOf(VisualCapturePayload{}):
		nonempty("capture_tool", "target")
		dates("captured_at")
		enum("media_type", VisualCaptureMediaTypes()...)
	case reflect.TypeOf(OperatorObservationPayload{}):
		nonempty("operator_id", "fact")
		dates("captured_at")
		property("supporting")["type"] = "array"
		property("supporting")["minItems"] = 1
	}
}
