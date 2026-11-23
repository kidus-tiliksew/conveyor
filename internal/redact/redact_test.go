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

	"github.com/kidus-tiliksew/conveyor/internal/core"
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
		{"OAuth prefixes named in prose", "the cv_oac_, cv_oat_, and cv_ort_ prefixes"},
		{"short OAuth identifier", "cv_oat_short"},
		{"OAuth prefixes after word characters", "xcv_oac_, 9cv_oat_, and _cv_ort_ are prefixes"},
		{"short OAuth identifier after a letter", "xcv_ort_short_value"},
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

// TestRedactsOAuthCredentialShapes mints each OAuth credential with the
// encoding the store issues and proves that pattern redaction removes the
// full wire value, counts it once by pattern class, and never by value
// (req-security-boundaries REQ-3, AC-3.1; component-identity-membership
// invariant 46).
func TestRedactsOAuthCredentialShapes(t *testing.T) {
	t.Parallel()
	for _, kind := range []core.OAuthCredentialKind{core.OAuthCredentialCode, core.OAuthCredentialAccess, core.OAuthCredentialRefresh} {
		_, value, digest, err := core.NewOAuthCredential(kind)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(value, "cv_"+string(kind)+"_") || len(digest) != 32 {
			t.Fatalf("%s value does not have the cv_%s_<id>_<secret> shape", kind, kind)
		}
		secret := value[len("cv_"+string(kind)+"_")+17:]
		for _, form := range []struct{ name, text string }{
			{"bare", "leaked " + value + " here"},
			{"quoted", `{"access_token":"` + value + `"}`},
			{"form body", "grant_type=refresh_token&refresh_token=" + value + "&client_id=ocl_x"},
			{"bearer", "Authorization: Bearer " + value},
			// A word character directly before the value defeats a leading
			// word boundary, so the OAuth pattern carries none.
			{"after a letter", "Client x" + value},
			{"after a digit", "client-9" + value + "-name"},
			{"after an underscore", "https://client.example.test/meta_" + value + "/client.json"},
		} {
			t.Run(string(kind)+"/"+form.name, func(t *testing.T) {
				t.Parallel()
				clean, stats := New(nil).Redact(form.text)
				if strings.Contains(clean, value) || strings.Contains(clean, secret) {
					t.Fatal("OAuth credential survived redaction")
				}
				if stats.Pattern != 1 || stats.Exact+stats.Encoded+stats.Entropy != 0 {
					t.Fatalf("stats=%+v, want one pattern removal", stats)
				}
				raw, err := json.Marshal(stats)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(raw), value) || strings.Contains(string(raw), secret) {
					t.Fatal("redaction stats carry the value")
				}
			})
		}
	}
}

// credentialShapeFixtures holds one synthetic value for every credential
// shape the redactor knows: each entry of credentialPatternSources and the
// assignment rule. No value is a real credential.
var credentialShapeFixtures = []struct{ name, value string }{
	{"JWT", "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJjb25mb3JtYW5jZSJ9.c2lnbmF0dXJlLXZhbHVl"},
	{"Google API key", "AIzaSyConformance0123456789abcdefgh"},
	{"sk- key", "sk-conformance0123456789abcdef"},
	{"GitHub ghp_ token", "ghp_Conformance0123456789abcdefABCDEF"},
	{"GitHub gho_ token", "gho_Conformance0123456789abcdefABCDEF"},
	{"GitHub ghu_ token", "ghu_Conformance0123456789abcdefABCDEF"},
	{"GitHub ghs_ token", "ghs_Conformance0123456789abcdefABCDEF"},
	{"GitHub ghr_ token", "ghr_Conformance0123456789abcdefABCDEF"},
	{"fine-grained GitHub token", "github_pat_11AAAAAAA0conformance_abcdefghij"},
	{"Conveyor personal access token", "cv_pat_fixtureid_Ab3dE6gH9jK2mN5pQ8sT1vW4y"},
	{"retired Conveyor agent token", "cv_agent_fixtureid_Ab3dE6gH9jK2mN5pQ8sT1vW4y"},
	{"Conveyor session token", "cv_session_fixtureid_Ab3dE6gH9jK2mN5pQ8sT1vW4y"},
	{"Conveyor sign-in token", "cv_signin_fixtureid_Ab3dE6gH9jK2mN5pQ8sT1vW4y"},
	{"OAuth authorization code", "cv_oac_fixtureidAb3dE6g_Ab3dE6gH9jK2mN5pQ8sT1vW4y"},
	{"OAuth access token", "cv_oat_fixtureidAb3dE6g_Ab3dE6gH9jK2mN5pQ8sT1vW4y"},
	{"OAuth refresh token", "cv_ort_fixtureidAb3dE6g_Ab3dE6gH9jK2mN5pQ8sT1vW4y"},
	{"bearer credential", "Bearer conformanceopaquevalue0123456789"},
	{"PEM private key", "-----BEGIN PRIVATE KEY-----MIIBconformance-----END PRIVATE KEY-----"},
	{"assigned secret", "token=conformanceassigned0123456789"},
}

// percentEncodeAll encodes every byte of value as %XX, so no credential
// pattern matches the encoded text itself.
func percentEncodeAll(value string) string {
	var out strings.Builder
	for i := 0; i < len(value); i++ {
		fmt.Fprintf(&out, "%%%02X", value[i])
	}
	return out.String()
}

// TestHoldsCredentialShapeAnywhere proves the boundary-free refusal check
// finds every credential shape directly after a letter, a digit, and an
// underscore, and in percent-encoded form, and that redaction itself now
// removes every Conveyor kind in the glued forms. Ordinary text, bare
// prefixes, and short values stay clean, and redaction keeps the leading
// boundary of the non-Conveyor shapes (req-security-boundaries REQ-2, REQ-3;
// component-identity-membership invariants 45 and 46).
func TestHoldsCredentialShapeAnywhere(t *testing.T) {
	t.Parallel()
	for _, shape := range credentialShapeFixtures {
		for _, form := range []struct{ name, text string }{
			{"as given", shape.value},
			{"after a letter", "Client x" + shape.value},
			{"after a digit", "client-9" + shape.value},
			{"after an underscore", "https://client.example.test/meta_" + shape.value + "/client.json"},
			{"percent-encoded", "https://client.example.test/" + percentEncodeAll(shape.value)},
		} {
			t.Run(shape.name+"/"+form.name, func(t *testing.T) {
				t.Parallel()
				if !HoldsCredentialShape(form.text) {
					t.Fatal("credential shape not found")
				}
				if strings.HasPrefix(shape.value, "cv_") && form.name != "percent-encoded" {
					clean, stats := New(nil).Redact(form.text)
					if strings.Contains(clean, shape.value) || stats.Pattern != 1 {
						t.Fatalf("Conveyor credential survived redaction: stats=%+v", stats)
					}
				}
			})
		}
	}
	for _, clean := range []string{
		"Conformance Client",
		"https://client.example.test/oauth/client.json",
		"http://127.0.0.1:43123/callback",
		"the cv_pat_, cv_agent_, cv_session_, cv_signin_, cv_oac_, cv_oat_, and cv_ort_ prefixes",
		"xcv_pat_, 9cv_session_, and _cv_ort_ are prefixes",
		"cv_pat_short and xcv_oat_short_value",
		"the sk- prefix, ghp_ tokens, and Bearer authentication",
		"%zz is not a valid escape",
	} {
		if HoldsCredentialShape(clean) {
			t.Errorf("clean text %q holds a credential shape", clean)
		}
	}
	// Redaction keeps the leading boundary of the non-Conveyor shapes, so a
	// task name stays readable in logs.
	if clean, stats := New(nil).Redact("task-0123456789abcdefghij"); clean != "task-0123456789abcdefghij" || stats.Total() != 0 {
		t.Fatalf("redaction changed a task name: %q %+v", clean, stats)
	}
	for _, kind := range []string{"pat", "agent", "session", "signin"} {
		value := "xcv_" + kind + "_fixtureid_Ab3dE6gH9jK2mN5pQ8sT1vW4y"
		if clean, stats := New(nil).Redact(value); clean != "x"+patternPlaceholder || stats.Pattern != 1 {
			t.Errorf("glued %s token: clean=%q stats=%+v", kind, clean, stats)
		}
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

// A deployment token's bare 64-hex form passes the patterns on purpose, because
// artifact IDs and digests have that shape. Only registration removes it, with
// an exact count, and no general 64-hex pattern reaches an unrelated digest
// (req-security-boundaries REQ-3).
func TestRegisteredBareHexTokenRedactsExactWhileDigestStays(t *testing.T) {
	token := strings.Repeat("1a2b3c4d", 8)
	digest := strings.Repeat("d4c3b2a1", 8)
	text := "token value " + token + " digest " + digest
	if clean, stats := New(nil).Redact(text); clean != text || stats.Total() != 0 {
		t.Fatalf("control: an unregistered 64-hex value was redacted: %+v", stats)
	}
	RegisterSecret(token, time.Time{})
	t.Cleanup(func() {
		processSecrets.Lock()
		defer processSecrets.Unlock()
		delete(processSecrets.values, token)
	})
	clean, stats := New(nil).Redact(text)
	if strings.Contains(clean, token) || !strings.Contains(clean, digest) {
		t.Fatal("the registered token survived or the digest was redacted")
	}
	if stats != (Stats{Exact: 1}) {
		t.Fatalf("counts = %+v, want one exact", stats)
	}
}

func TestRegisterSecretIgnoresEmptyValue(t *testing.T) {
	RegisterSecret("", time.Time{})
	processSecrets.Lock()
	_, registered := processSecrets.values[""]
	processSecrets.Unlock()
	if registered {
		t.Fatal("an empty value was registered")
	}
}
