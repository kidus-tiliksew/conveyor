package main

import (
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/redact"
)

// A bare 64-hex deployment token matches no credential pattern, so only its
// registration removes it, and an unrelated digest of the same shape stays
// (req-security-boundaries REQ-3, AC-3.1; component-identity-membership
// invariant 46).
func TestReadDeploymentTokenRegistersBareTokenForExactRedaction(t *testing.T) {
	token := strings.Repeat("0a1b2c3d", 8)
	digest := strings.Repeat("9f8e7d6c", 8)
	text := "deployment " + token + " artifact " + digest

	// Control: before the read, the bare token passes, so the test can fail and
	// nothing else in the redactor removes this shape.
	before, stats := redact.New(nil).Redact(text)
	if before != text || stats.Total() != 0 {
		t.Fatalf("an unregistered 64-hex value was redacted: %+v", stats)
	}

	got, err := readDeploymentToken(func(key string) string {
		if key != "CONVEYOR_API_TOKEN" {
			t.Errorf("read %s", key)
		}
		return token
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != token {
		t.Fatal("the read did not return the token")
	}
	clean, stats := redact.New(nil).Redact(text)
	if strings.Contains(clean, token) {
		t.Fatal("the registered token survived redaction")
	}
	if !strings.Contains(clean, digest) {
		t.Fatal("an unrelated 64-hex digest was redacted")
	}
	if stats != (core.RedactionStats{Exact: 1}) {
		t.Fatalf("counts = %+v, want one exact", stats)
	}
}

func TestReadDeploymentTokenRefusesUnsetTokenWithoutRegistering(t *testing.T) {
	const sample = "plain text with no credential"
	got, err := readDeploymentToken(func(string) string { return "" })
	if err == nil || got != "" {
		t.Fatalf("unset token: got a value (%v) or no error (%v)", got != "", err)
	}
	clean, stats := redact.New(nil).Redact(sample)
	if clean != sample || stats.Total() != 0 {
		t.Fatalf("an empty registration changed redaction: %+v", stats)
	}
}
