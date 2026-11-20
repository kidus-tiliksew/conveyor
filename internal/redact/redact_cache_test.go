package redact

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// legacyNeedles is the compile step of the redactor before the per-redactor
// cache, with the live registry values passed in explicitly.
func legacyNeedles(values, live []string) []needle {
	values = append(append([]string(nil), values...), live...)
	byValue := make(map[string]needle)
	add := func(value, placeholder, class string) {
		if value == "" {
			return
		}
		if existing, ok := byValue[value]; ok && existing.class == "exact" {
			return
		}
		byValue[value] = needle{value: value, placeholder: placeholder, class: class}
	}
	for _, value := range values {
		add(value, exactPlaceholder, "exact")
		encoded := []string{
			base64.StdEncoding.EncodeToString([]byte(value)),
			base64.RawStdEncoding.EncodeToString([]byte(value)),
			base64.URLEncoding.EncodeToString([]byte(value)),
			base64.RawURLEncoding.EncodeToString([]byte(value)),
			url.QueryEscape(value),
			url.PathEscape(value),
			legacyQuoted(value),
		}
		for _, candidate := range encoded {
			if candidate != value {
				add(candidate, encodedPlaceholder, "encoded")
			}
		}
	}
	needles := make([]needle, 0, len(byValue))
	for _, item := range byValue {
		needles = append(needles, item)
	}
	sort.Slice(needles, func(i, j int) bool { return len(needles[i].value) > len(needles[j].value) })
	return needles
}

func legacyQuoted(value string) string {
	quoted := strconv.Quote(value)
	return quoted[1 : len(quoted)-1]
}

// legacyRedact is the string redaction before the cache: it compiles the
// whole list again for every call.
func legacyRedact(source, live []string, text string) (string, Stats) {
	var stats Stats
	for _, item := range legacyNeedles(source, live) {
		count := strings.Count(text, item.value)
		if count == 0 {
			continue
		}
		text = strings.ReplaceAll(text, item.value, item.placeholder)
		if item.class == "exact" {
			stats.Exact += int64(count)
		} else {
			stats.Encoded += int64(count)
		}
	}
	for _, pattern := range credentialPatterns {
		matches := pattern.FindAllStringIndex(text, -1)
		if len(matches) == 0 {
			continue
		}
		stats.Pattern += int64(len(matches))
		text = pattern.ReplaceAllString(text, patternPlaceholder)
	}
	matches := assignedSecret.FindAllStringSubmatchIndex(text, -1)
	if len(matches) > 0 {
		stats.Entropy += int64(len(matches))
		text = assignedSecret.ReplaceAllString(text, `${1}`+entropyPlaceholder)
	}
	return text, stats
}

// legacyRedactJSON is the JSON walk before the cache, one compile per key and
// per string value.
func legacyRedactJSON(source, live []string, data []byte) ([]byte, Stats, error) {
	var value any
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, Stats{}, err
	}
	var stats Stats
	var walk func(any) any
	walk = func(value any) any {
		switch item := value.(type) {
		case string:
			clean, found := legacyRedact(source, live, item)
			stats.Add(found)
			return clean
		case []any:
			for i := range item {
				item[i] = walk(item[i])
			}
			return item
		case map[string]any:
			cleaned := make(map[string]any, len(item))
			for key, child := range item {
				cleanKey, found := legacyRedact(source, live, key)
				stats.Add(found)
				cleaned[cleanKey] = walk(child)
			}
			return cleaned
		default:
			return value
		}
	}
	value = walk(value)
	out, err := json.Marshal(value)
	return out, stats, err
}

// setClock replaces the registry clock for one test.
func setClock(tb testing.TB, at *time.Time) {
	tb.Helper()
	previous := clock
	clock = func() time.Time { return *at }
	tb.Cleanup(func() { clock = previous })
}

// TestRedactMatchesLegacyAlgorithm compares the cached redactor with the
// rebuild-per-string algorithm on the same caller values and live registry:
// redacted bytes and each of the four counts must be equal.
func TestRedactMatchesLegacyAlgorithm(t *testing.T) {
	isolateRegistry(t)
	short := "fixture-short-secret"
	long := short + "-with-a-longer-tail"
	spaced := "fixture value/with?punctuation"
	// The query escape of spaced is itself a caller value, so the exact form
	// must win over the equal encoded form.
	collision := url.QueryEscape(spaced)
	quoted := "fixture\"quoted\\value\nline"
	registered := "fixture-registered-token-value"
	permanent := fixtureKey(7)
	RegisterSecret(registered, time.Now().Add(time.Hour))
	RegisterSecret(permanent, time.Time{})
	source := []string{short, long, spaced, collision, quoted}
	live := []string{registered, permanent}
	redactor := New(source)

	texts := []string{
		"",
		"nothing to redact here",
		"exact " + short + " and " + long + " and " + short + "-other",
		"base64 " + base64.StdEncoding.EncodeToString([]byte(long)) + " raw " + base64.RawURLEncoding.EncodeToString([]byte(short)),
		"query " + url.QueryEscape(spaced) + " path " + url.PathEscape(spaced),
		"quoted " + strconv.Quote(quoted),
		"registered " + registered + " encoded " + base64.StdEncoding.EncodeToString([]byte(registered)),
		"key " + permanent + " quoted key " + strconv.Quote(permanent),
		"pattern sk-abcdefghijklmnopqrstuvwxyz012345 ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ012345",
		"entropy token=abcdefghijklmnopqrstuvwxyz012345 password: \"ABCDEFGHIJKLMNOPQRSTUV\"",
		"mixed " + short + " sk-abcdefghijklmnopqrstuvwxyz012345 secret=" + registered + "0123456789",
	}
	for index, text := range texts {
		gotText, gotStats := redactor.Redact(text)
		wantText, wantStats := legacyRedact(source, live, text)
		if gotText != wantText || gotStats != wantStats {
			t.Errorf("text %d: got %q %+v, want %q %+v", index, gotText, gotStats, wantText, wantStats)
		}
	}

	documents := []string{
		`{"payload":{"output":"` + short + `","list":["` + long + `",12345678901234567890.5,true,null]}}`,
		`{"` + registered + `":"key holds a token","escaped":` + strconv.Quote(quoted) + `}`,
		`["sk-abcdefghijklmnopqrstuvwxyz012345","token=abcdefghijklmnopqrstuvwxyz012345",` + strconv.Quote(permanent) + `]`,
		`{"note":"` + base64.StdEncoding.EncodeToString([]byte(spaced)) + `","n":1e400}`,
		`"plain string document ` + collision + `"`,
		`{"number":-0.000000000000000000001,"nested":{"` + short + `":{"` + long + `":"x"}}}`,
	}
	for index, document := range documents {
		gotJSON, gotStats, gotErr := redactor.RedactJSON([]byte(document))
		wantJSON, wantStats, wantErr := legacyRedactJSON(source, live, []byte(document))
		if (gotErr == nil) != (wantErr == nil) || !bytes.Equal(gotJSON, wantJSON) || gotStats != wantStats {
			t.Errorf("document %d: got %s %+v %v, want %s %+v %v", index, gotJSON, gotStats, gotErr, wantJSON, wantStats, wantErr)
		}
	}
	if _, _, err := redactor.RedactJSON([]byte(`{"broken":`)); err == nil {
		t.Error("malformed JSON did not return an error")
	}
}

// TestRedactJSONKeyCollisionMatchesALegacyOutcome covers two keys that redact
// to the same key. The legacy walk keeps whichever key map iteration visits
// last, so the cached redactor must return one of the legacy outcomes.
func TestRedactJSONKeyCollisionMatchesALegacyOutcome(t *testing.T) {
	isolateRegistry(t)
	source := []string{"fixture-key-one", "fixture-key-two"}
	document := []byte(`{"fixture-key-one":"first","fixture-key-two":"second"}`)
	outcomes := map[string]Stats{}
	for i := 0; i < 200; i++ {
		out, stats, err := legacyRedactJSON(source, nil, document)
		if err != nil {
			t.Fatal(err)
		}
		outcomes[string(out)] = stats
	}
	redactor := New(source)
	for i := 0; i < 20; i++ {
		out, stats, err := redactor.RedactJSON(document)
		if err != nil {
			t.Fatal(err)
		}
		want, ok := outcomes[string(out)]
		if !ok || stats != want {
			t.Fatalf("got %s %+v, not a legacy outcome %v", out, stats, outcomes)
		}
	}
}

// TestRedactorReusesCompiledListWhileRegistryIsStable checks that strings,
// keys, documents, and plain text calls share one list while the registry
// does not change, and that a registration takes effect at the next lookup.
func TestRedactorReusesCompiledListWhileRegistryIsStable(t *testing.T) {
	isolateRegistry(t)
	RegisterSecret(fixtureKey(1), time.Time{})
	redactor := New([]string{"fixture-caller-value"})
	first := redactor.compiled.Load()
	for i := 0; i < 3; i++ {
		if _, _, err := redactor.RedactJSON([]byte(`{"fixture-caller-value":["a","b",{"c":"d"}]}`)); err != nil {
			t.Fatal(err)
		}
		redactor.Redact("plain text")
	}
	if redactor.compiled.Load() != first {
		t.Fatal("a stable registry rebuilt the compiled list")
	}

	late := "fixture-late-registered-token"
	RegisterSecret(late, time.Now().Add(time.Hour))
	clean, stats := redactor.Redact("value " + late)
	if strings.Contains(clean, late) || stats != (Stats{Exact: 1}) {
		t.Fatalf("a token registered after the redactor was built survived: %q %+v", clean, stats)
	}
	second := redactor.compiled.Load()
	if second == first || second.revision <= first.revision {
		t.Fatal("the registration did not produce a newer compiled list")
	}
	redactor.Redact("plain text")
	if redactor.compiled.Load() != second {
		t.Fatal("a stable registry rebuilt the compiled list after a refresh")
	}
}

// TestWriterMatchesLegacyAcrossLateRegistration writes through one Writer
// before and after a token is registered and compares the output with the
// legacy algorithm at each write.
func TestWriterMatchesLegacyAcrossLateRegistration(t *testing.T) {
	isolateRegistry(t)
	source := []string{"fixture-writer-caller-value"}
	token := "fixture-writer-late-token"
	var destination bytes.Buffer
	writer := &Writer{Destination: &destination, Redactor: New(source)}
	var want strings.Builder

	before := "before " + token + " " + source[0] + "\n"
	if _, err := writer.Write([]byte(before)); err != nil {
		t.Fatal(err)
	}
	clean, _ := legacyRedact(source, nil, before)
	want.WriteString(clean)

	RegisterSecret(token, time.Now().Add(time.Hour))
	after := "after " + token + " " + base64.StdEncoding.EncodeToString([]byte(token)) + "\ntail " + token
	if _, err := writer.Write([]byte(after)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Flush(); err != nil {
		t.Fatal(err)
	}
	live := []string{token}
	clean, _ = legacyRedact(source, live, after[:strings.LastIndexByte(after, '\n')+1])
	want.WriteString(clean)
	clean, _ = legacyRedact(source, live, after[strings.LastIndexByte(after, '\n')+1:])
	want.WriteString(clean)
	if destination.String() != want.String() {
		t.Fatalf("writer output %q, want %q", destination.String(), want.String())
	}
	if !strings.Contains(destination.String(), "before "+token) {
		t.Fatal("the line written before registration changed")
	}
}

// TestRegistryExpiryAndExtension crosses expiries with the clock seam: a
// token stops matching at its expiry, a later registration extends it, an
// earlier one does not shorten it, and an unchanged registry keeps its list.
func TestRegistryExpiryAndExtension(t *testing.T) {
	isolateRegistry(t)
	now := time.Now()
	setClock(t, &now)
	expiring := "fixture-expiring-token-value"
	extended := "fixture-extended-token-value"
	RegisterSecret(expiring, now.Add(time.Minute))
	RegisterSecret(extended, now.Add(time.Minute))
	RegisterSecret(extended, now.Add(2*time.Hour))
	RegisterSecret(extended, now.Add(time.Second))
	redactor := New(nil)
	text := expiring + " " + extended

	check := func(step string, live []string) {
		t.Helper()
		gotText, gotStats := redactor.Redact(text)
		wantText, wantStats := legacyRedact(nil, live, text)
		if gotText != wantText || gotStats != wantStats {
			t.Fatalf("%s: got %q %+v, want %q %+v", step, gotText, gotStats, wantText, wantStats)
		}
	}
	check("before expiry", []string{expiring, extended})
	stable := redactor.compiled.Load()
	now = now.Add(time.Minute - time.Nanosecond)
	check("just before expiry", []string{expiring, extended})
	if redactor.compiled.Load() != stable {
		t.Fatal("an unchanged registry rebuilt the compiled list")
	}
	now = now.Add(time.Nanosecond)
	check("at expiry", []string{extended})
	processSecrets.Lock()
	_, kept := processSecrets.values[expiring]
	processSecrets.Unlock()
	if kept {
		t.Fatal("the expired token stayed registered")
	}
	now = now.Add(2 * time.Hour)
	check("after the extended expiry", nil)

	// A redactor built after expiry and a permanent value registered later.
	permanent := "fixture-permanent-value"
	RegisterSecret(permanent, time.Time{})
	RegisterSecret(permanent, now.Add(time.Minute))
	now = now.Add(time.Hour)
	if clean, stats := New(nil).Redact(permanent); clean != exactPlaceholder || stats != (Stats{Exact: 1}) {
		t.Fatalf("a permanent value expired: %q %+v", clean, stats)
	}
}

// TestSharedRedactorUnderConcurrentRegistration shares one redactor across
// goroutines while others register tokens. Run it with -race. Each token
// registered before a lookup must match at that lookup.
func TestSharedRedactorUnderConcurrentRegistration(t *testing.T) {
	isolateRegistry(t)
	redactor := New([]string{"fixture-shared-caller-value"})
	const writers, tokensPerWriter, readers = 4, 25, 4
	var wait sync.WaitGroup
	failures := make(chan string, writers*tokensPerWriter+readers)
	for w := 0; w < writers; w++ {
		wait.Add(1)
		go func(w int) {
			defer wait.Done()
			for i := 0; i < tokensPerWriter; i++ {
				token := fmt.Sprintf("fixture-concurrent-token-%d-%03d", w, i)
				RegisterSecret(token, time.Now().Add(time.Hour))
				if clean, stats := redactor.Redact("value " + token); strings.Contains(clean, token) || stats.Exact != 1 {
					failures <- fmt.Sprintf("token %s survived: %q %+v", token, clean, stats)
				}
			}
		}(w)
	}
	for r := 0; r < readers; r++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for i := 0; i < 50; i++ {
				out, stats, err := redactor.RedactJSON([]byte(`{"fixture-shared-caller-value":["fixture-shared-caller-value"]}`))
				if err != nil || strings.Contains(string(out), "fixture-shared-caller-value") || stats.Exact != 2 {
					failures <- fmt.Sprintf("shared document: %s %+v %v", out, stats, err)
				}
			}
		}()
	}
	wait.Wait()
	close(failures)
	for failure := range failures {
		t.Error(failure)
	}
}
