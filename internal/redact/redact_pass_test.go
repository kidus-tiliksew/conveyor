package redact

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// isolateRegistry empties the process registry for one test and restores the
// earlier entries through RegisterSecret afterward, so a test does not depend
// on secrets that an earlier test registered.
func isolateRegistry(tb testing.TB) {
	tb.Helper()
	processSecrets.Lock()
	saved := processSecrets.values
	processSecrets.values = map[string]time.Time{}
	processSecrets.Unlock()
	tb.Cleanup(func() {
		processSecrets.Lock()
		processSecrets.values = map[string]time.Time{}
		processSecrets.Unlock()
		for value, expiry := range saved {
			RegisterSecret(value, expiry)
		}
	})
}

// fixtureKey is a synthetic 1,700-byte stand-in for a decrypted GitHub App
// key: PEM-like lines of 64 characters from a fixed generator. It is not a
// key and matches no credential pattern.
func fixtureKey(index int) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	state := uint32(2166136261 + index*16777619)
	var builder strings.Builder
	builder.WriteString(fmt.Sprintf("fixture-app-key-%d\n", index))
	for line := 0; builder.Len() < 1700; line++ {
		for column := 0; column < 64 && builder.Len() < 1699; column++ {
			state = state*1664525 + 1013904223
			builder.WriteByte(alphabet[state>>26])
		}
		builder.WriteByte('\n')
	}
	return builder.String()[:1700]
}

// fixtureToken is a synthetic installation-token stand-in.
func fixtureToken(index int) string {
	return fmt.Sprintf("fixture-installation-token-%03d-%s", index, strings.Repeat("t", 12))
}

// shortStringDocument is one JSON array of count short strings.
func shortStringDocument(count int) []byte {
	values := make([]string, count)
	for i := range values {
		values[i] = fmt.Sprintf("event note %04d", i)
	}
	data, err := json.Marshal(values)
	if err != nil {
		panic(err)
	}
	return data
}

// registerFixtureSecrets registers the given number of fixture keys with a
// zero expiry, as private keys are, and fixture tokens that expire in an hour.
func registerFixtureSecrets(keys, tokens int) {
	for i := 0; i < keys; i++ {
		RegisterSecret(fixtureKey(i), time.Time{})
	}
	for i := 0; i < tokens; i++ {
		RegisterSecret(fixtureToken(i), time.Now().Add(time.Hour))
	}
}

// TestRedactJSONBuildsSecretFormsOncePerPass fails when one RedactJSON pass
// over 1,000 short strings with three registered 1,700-byte keys builds the
// secret forms per string. It compares allocations with and without the keys
// for a fresh redactor, so the first build counts. One build costs a fixed
// number of allocations; one extra allocation per string would cost 1,000.
func TestRedactJSONBuildsSecretFormsOncePerPass(t *testing.T) {
	isolateRegistry(t)
	document := shortStringDocument(1000)
	measure := func() float64 {
		return testing.AllocsPerRun(5, func() {
			if _, _, err := New(nil).RedactJSON(document); err != nil {
				panic(err)
			}
		})
	}
	without := measure()
	registerFixtureSecrets(3, 0)
	with := measure()
	// Three keys build 3 exact and 21 encoded forms, a map, and a sorted
	// slice: well under 200 allocations, and far below one per string.
	const perPassBudget = 200
	t.Logf("allocations per pass over 1,000 strings: %.0f with three keys, %.0f without", with, without)
	if extra := with - without; extra > perPassBudget {
		t.Fatalf("three registered keys added %.0f allocations to one pass over 1,000 strings (%.0f with keys, %.0f without); want at most %d, so the secret forms are built once per pass and not once per string", extra, with, without, perPassBudget)
	}
}

// BenchmarkRedactJSON measures one redaction pass with 0, 6 (2 keys and
// 4 tokens), and 13 (3 keys and 10 tokens) registered secrets. The "pass"
// cases build a fresh redactor and redact one document of 1,000 short
// strings. The "reuse" cases redact eight such documents and eight plain text
// strings with one redactor, as an event page handler does.
func BenchmarkRedactJSON(b *testing.B) {
	document := shortStringDocument(1000)
	for _, secrets := range []struct{ keys, tokens int }{{0, 0}, {2, 4}, {3, 10}} {
		name := fmt.Sprintf("secrets=%d", secrets.keys+secrets.tokens)
		b.Run(name+"/pass", func(b *testing.B) {
			isolateRegistry(b)
			registerFixtureSecrets(secrets.keys, secrets.tokens)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, _, err := New(nil).RedactJSON(document); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(name+"/reuse", func(b *testing.B) {
			isolateRegistry(b)
			registerFixtureSecrets(secrets.keys, secrets.tokens)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				redactor := New(nil)
				for j := 0; j < 8; j++ {
					if _, _, err := redactor.RedactJSON(document); err != nil {
						b.Fatal(err)
					}
					redactor.Redact("plain diagnostic text")
				}
			}
		})
	}
}
