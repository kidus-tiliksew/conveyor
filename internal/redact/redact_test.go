package redact

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

type testSecretSource struct {
	values []string
	err    error
}

func (s testSecretSource) ListGitHubAppKeysForRedaction(context.Context) ([]string, error) {
	return s.values, s.err
}

func TestTextIncludesGitHubAppKeysAndFailsClosed(t *testing.T) {
	clean, stats, err := Text(t.Context(), testSecretSource{values: []string{"plain-forge-value"}}, "before plain-forge-value after")
	if err != nil || clean != "before [REDACTED:exact] after" || stats.Exact != 1 {
		t.Fatalf("clean=%q stats=%+v err=%v", clean, stats, err)
	}
	if _, _, err = Text(t.Context(), testSecretSource{err: errors.New("decrypt failed")}, "persist me"); err == nil {
		t.Fatal("secret resolution failure did not fail closed")
	}
}

func TestWriterRedactsExactValuesSplitAcrossWritesAndFlush(t *testing.T) {
	secret := "child-only-runtime-value"
	var destination bytes.Buffer
	writer := &Writer{Destination: &destination, Redactor: New([]string{secret})}
	for _, part := range []string{"token=child-only-", "runtime-value\nsession=child-only-", "runtime-value"} {
		if _, err := writer.Write([]byte(part)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Flush(); err != nil {
		t.Fatal(err)
	}
	if output := destination.String(); strings.Contains(output, secret) || strings.Count(output, exactPlaceholder) != 2 {
		t.Fatalf("stream output=%q", output)
	}
}

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

func TestExactValueWinsOverEquivalentEncoding(t *testing.T) {
	r := New([]string{"plainvalue"})
	clean, stats := r.Redact("plainvalue")
	if clean != exactPlaceholder || stats.Exact != 1 || stats.Encoded != 0 {
		t.Fatalf("clean=%q stats=%+v", clean, stats)
	}
}

func TestAppSecretsReachExistingDiagnosticWriters(t *testing.T) {
	var output bytes.Buffer
	writer := &Writer{Destination: &output, Redactor: New(nil)}
	first := "app-first-live-token-value"
	second := "app-second-live-token-value"
	RegisterSecret(first, time.Now().Add(time.Hour))
	RegisterSecret(second, time.Now().Add(time.Hour))
	encoded := base64.StdEncoding.EncodeToString([]byte(first))
	if _, err := writer.Write([]byte(first + " " + second + " " + encoded + "\n")); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{first, second, encoded} {
		if strings.Contains(output.String(), secret) {
			t.Fatal("registered token escaped preexisting writer")
		}
	}
}

// TestRedactsTokenShapesAndKeepsDigests pins each credential shape redaction
// recognizes without knowing the value, and the 64-hex values that must pass:
// artifact IDs and content digests have that shape (req-security-boundaries
// REQ-3, AC-3.1).
func TestRedactsTokenShapesAndKeepsDigests(t *testing.T) {
	t.Parallel()
	const tail = "Ab3dE6gH9jK2mN5pQ8sT1vW4yZ7bC0eF3hJ6kM9nP2r"
	const digest = "0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c4b5a69788796a5b4c3d2e1f0"
	tokens := []struct{ name, value string }{
		{"personal access token", "cv_pat_fixtureid_" + tail},
		{"personal access token ending in a dash", "cv_pat_fixtureid_" + tail + "-"},
		{"retired agent token", "cv_agent_fixtureid_" + tail},
		{"session token", "cv_session_fixtureid_" + tail},
		{"sign-in token", "cv_signin_fixtureid_" + tail},
		{"fine-grained GitHub token", "github_pat_11AAAAAAA0fixture_" + tail},
	}
	for _, token := range tokens {
		for _, form := range []struct{ name, text string }{
			{"bare", "leaked " + token.value + " here"},
			{"quoted", `{"value":"` + token.value + `"}`},
			{"bearer", "Authorization: Bearer " + token.value},
			{"lowercase bearer", "authorization: bearer " + token.value},
		} {
			t.Run(token.name+"/"+form.name, func(t *testing.T) {
				t.Parallel()
				clean, stats := New(nil).Redact(form.text)
				if strings.Contains(clean, token.value) || strings.Contains(clean, tail) {
					t.Fatal("token survived redaction")
				}
				if stats.Pattern != 1 || stats.Exact+stats.Encoded+stats.Entropy != 0 {
					t.Fatalf("stats=%+v, want one pattern removal", stats)
				}
				if !strings.Contains(clean, patternPlaceholder) {
					t.Fatalf("no placeholder in %q", clean)
				}
			})
		}
	}
	t.Run("assigned token counts once", func(t *testing.T) {
		t.Parallel()
		clean, stats := New(nil).Redact("token=cv_pat_fixtureid_" + tail)
		if clean != "token="+patternPlaceholder || stats.Pattern != 1 || stats.Entropy != 0 {
			t.Fatalf("clean=%q stats=%+v", clean, stats)
		}
	})
	t.Run("bearer value of another shape", func(t *testing.T) {
		t.Parallel()
		clean, stats := New(nil).Redact("Authorization: Bearer opaque-fixture." + tail)
		if strings.Contains(clean, tail) || stats.Pattern != 1 {
			t.Fatalf("clean=%q stats=%+v", clean, stats)
		}
	})
	for _, kept := range []struct{ name, text string }{
		{"bare artifact digest", digest},
		{"prefixed content digest", "sha256:" + digest},
		{"digest in a payload", `{"artifact_id":"` + digest + `","content_digest":"` + digest + `"}`},
		{"digest in a URI", "artifact://" + digest},
		{"prefix named in prose", "the cv_pat_ prefix and cv_session_ cookies"},
		{"short identifier", "cv_pat_short"},
		{"bearer in prose", "the bearer token and Bearer authentication"},
		{"github_pat prefix alone", "a github_pat_ value"},
	} {
		t.Run("keeps "+kept.name, func(t *testing.T) {
			t.Parallel()
			clean, stats := New(nil).Redact(kept.text)
			if clean != kept.text || stats.Total() != 0 {
				t.Fatalf("clean=%q stats=%+v", clean, stats)
			}
		})
	}
}

// TestRedactsQuotedFormOfKnownValues covers a known value printed with %q,
// whose escapes would otherwise defeat the exact match. A value quoting leaves
// alone counts once, as exact.
func TestRedactsQuotedFormOfKnownValues(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, secret string
		want         Stats
	}{
		{name: "quote and backslash", secret: `fixture"stored\key-value`, want: Stats{Encoded: 1}},
		{name: "newline", secret: "fixture-first-line\nfixture-second-line", want: Stats{Encoded: 1}},
		{name: "tab", secret: "fixture-left\tfixture-right", want: Stats{Encoded: 1}},
		{name: "nothing to escape", secret: "fixture-plain-value", want: Stats{Exact: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			clean, stats := New([]string{tc.secret}).Redact(fmt.Sprintf("name %q here", tc.secret))
			if stats != tc.want {
				t.Fatalf("stats=%+v, want %+v", stats, tc.want)
			}
			if strings.Contains(clean, "fixture") {
				t.Fatal("part of the known value survived")
			}
		})
	}
}
