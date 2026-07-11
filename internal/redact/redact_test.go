package redact

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func TestRedactsExactAndEncodedInjectedValues(t *testing.T) {
	secret := `pa$$ word/with?punctuation`
	encoded := base64.StdEncoding.EncodeToString([]byte(secret))
	r := New([]string{secret})
	clean, stats := r.Redact("raw=" + secret + " encoded=" + encoded)
	if strings.Contains(clean, secret) || strings.Contains(clean, encoded) {
		t.Fatalf("secret survived: %s", clean)
	}
	if stats.Exact != 1 || stats.Encoded != 1 {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestRedactsCredentialPatternsAndAssignedEntropy(t *testing.T) {
	r := New(nil)
	input := "key=sk-abcdefghijklmnopqrstuvwxyz012345 token=abcdefghijklmnopqrstuvwxyz012345"
	clean, stats := r.Redact(input)
	if strings.Contains(clean, "abcdefghijklmnopqrstuvwxyz") {
		t.Fatalf("credential survived: %s", clean)
	}
	if stats.Pattern != 1 || stats.Entropy != 1 {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestRedactJSONHandlesEscapedSecret(t *testing.T) {
	secret := "line\n\"quoted\""
	r := New([]string{secret})
	input, err := json.Marshal(map[string]any{"payload": map[string]string{"output": secret}})
	if err != nil {
		t.Fatal(err)
	}
	clean, stats, err := r.RedactJSON(input)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(clean), "quoted") || stats.Exact != 1 {
		t.Fatalf("clean=%s stats=%+v", clean, stats)
	}
}

func TestRedactJSONScrubsObjectKeys(t *testing.T) {
	r := New([]string{"secret-object-key"})
	clean, stats, err := r.RedactJSON([]byte(`{"secret-object-key":"value"}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(clean), "secret-object-key") || stats.Exact != 1 {
		t.Fatalf("clean=%s stats=%+v", clean, stats)
	}
}

func TestJSONStringValuesIncludesNestedOAuthTokens(t *testing.T) {
	values, err := JSONStringValues([]byte(`{"oauth":{"access":"secret-access","refresh":"secret-refresh"},"account":"user-1"}`))
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(values, ",")
	for _, want := range []string{"secret-access", "secret-refresh", "user-1"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("values = %v, missing %q", values, want)
		}
	}
}

func TestExactValueWinsOverEquivalentEncoding(t *testing.T) {
	r := New([]string{"plainvalue"})
	clean, stats := r.Redact("plainvalue")
	if clean != exactPlaceholder || stats.Exact != 1 || stats.Encoded != 0 {
		t.Fatalf("clean=%q stats=%+v", clean, stats)
	}
}
