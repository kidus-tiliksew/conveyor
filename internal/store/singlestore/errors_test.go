package singlestore

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/redact"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

type lockedLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *lockedLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// captureLog redirects the standard logger for one test. Tests that call it
// do not run in parallel.
func captureLog(t *testing.T) *lockedLog {
	t.Helper()
	captured := &lockedLog{}
	previousWriter, previousFlags := log.Writer(), log.Flags()
	log.SetOutput(captured)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(previousWriter)
		log.SetFlags(previousFlags)
	})
	return captured
}

func TestUnmappedDriverErrorIsLoggedAndHidden(t *testing.T) {
	captured := captureLog(t)
	original := &mysql.MySQLError{Number: 1706, SQLState: [5]byte{'H', 'Y', '0', '0', '0'}, Message: "Feature 'COM_STMT_SEND_LONG_DATA' is not supported by SingleStore"}
	for name, input := range map[string]error{"unwrapped": original, "wrapped": fmt.Errorf("claim order: %w", original)} {
		got := translateBackendConflict(input)
		if !errors.Is(got, store.ErrBackendOperation) || got.Error() != "SingleStore database operation failed" {
			t.Fatalf("%s: translated error = %v", name, got)
		}
		var driverErr *mysql.MySQLError
		if errors.As(got, &driverErr) {
			t.Fatalf("%s: driver error escaped: %#v", name, driverErr)
		}
	}
	lines := strings.Split(strings.TrimSpace(captured.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("log lines = %q, want one per translation", lines)
	}
	for _, line := range lines {
		for _, want := range []string{"singlestore: database operation failed:", "operation=TestUnmappedDriverErrorIsLoggedAndHidden", "error=1706", "sqlstate=HY000", `message="Feature 'COM_STMT_SEND_LONG_DATA' is not supported by SingleStore"`} {
			if !strings.Contains(line, want) {
				t.Fatalf("log line %q lacks %q", line, want)
			}
		}
	}
}

func TestDriverSentinelsAreTranslatedAndLogged(t *testing.T) {
	for _, sentinel := range []error{mysql.ErrInvalidConn, mysql.ErrMalformPkt, mysql.ErrPktSync, mysql.ErrPktSyncMul, mysql.ErrPktTooLarge, mysql.ErrBusyBuffer, driver.ErrBadConn} {
		captured := captureLog(t)
		got := translateBackendConflict(fmt.Errorf("query: %w", sentinel))
		if !errors.Is(got, store.ErrBackendOperation) || errors.Is(got, sentinel) || strings.Contains(got.Error(), sentinel.Error()) {
			t.Fatalf("%v translated to %v", sentinel, got)
		}
		if line := captured.String(); !strings.Contains(line, "error=0 sqlstate= ") || !strings.Contains(line, fmt.Sprintf("message=%q", sentinel.Error())) {
			t.Fatalf("%v log = %q", sentinel, line)
		}
	}
}

// driverLogLinePattern is the whole line every test below reads.
const driverLogLinePattern = "singlestore: database operation failed: operation=%s error=%d sqlstate=%s message=%q\n"

// loggedDriverMessage returns the quoted message field of one log line.
func loggedDriverMessage(t *testing.T, line string) string {
	t.Helper()
	start := strings.Index(line, "message=")
	if start < 0 {
		t.Fatalf("no message field: %q", line)
	}
	var logged string
	if _, err := fmt.Sscanf(line[start+len("message="):], "%q", &logged); err != nil {
		t.Fatalf("message field is not quoted: %v", err)
	}
	return logged
}

func TestDriverLogRedactsAndBoundsMessage(t *testing.T) {
	const prefix = "Duplicate entry [value omitted] for key '"
	value := "password=" + strings.Repeat("s3cr3tV4lue", 4)

	t.Run("a very long identifier key name is cut at the bound", func(t *testing.T) {
		captured := captureLog(t)
		message := "Leaf Error (192.0.2.7:3307): Duplicate entry '" + value + "' for key '" + strings.Repeat("k", 2*driverLogMessageLimit) + "'"
		if err := translateBackendConflict(&mysql.MySQLError{Number: 1062, Message: message}); !errors.Is(err, store.ErrBackendOperation) {
			t.Fatalf("translated to %v", err)
		}
		line := captured.String()
		if strings.Contains(line, "s3cr3tV4lue") || strings.Contains(line, "192.0.2.7") || strings.Contains(line, "Leaf Error") {
			t.Fatalf("server text reached the log: %q", line)
		}
		logged := loggedDriverMessage(t, line)
		if len(logged) != driverLogMessageLimit || !strings.HasPrefix(logged, prefix+"kkk") {
			t.Fatalf("logged message length %d, prefix %.60q", len(logged), logged)
		}
	})

	t.Run("a secret shaped key name is redacted", func(t *testing.T) {
		captured := captureLog(t)
		name := "ghp_" + strings.Repeat("Fak3Fixture", 4)
		translateBackendConflict(&mysql.MySQLError{Number: 1062, Message: "Duplicate entry '" + value + "' for key '" + name + "'"})
		logged := loggedDriverMessage(t, captured.String())
		if strings.Contains(logged, "Fak3Fixture") || !strings.HasPrefix(logged, prefix+"[REDACTED:") {
			t.Fatalf("secret shaped key name logged as %q", logged)
		}
	})

	t.Run("a registered secret is redacted from an admitted line", func(t *testing.T) {
		captured := captureLog(t)
		name := "registered_fixture_key_" + strings.ReplaceAll(core.NewTaskID(), "-", "_")
		translateBackendConflict(&mysql.MySQLError{Number: 1062, Message: "Duplicate key " + name})
		if logged := loggedDriverMessage(t, captured.String()); logged != prefix+name+"'" {
			t.Fatalf("control failed: an unregistered key name logged as %q", logged)
		}
		redact.RegisterSecret(name, time.Time{})
		captured = captureLog(t)
		translateBackendConflict(&mysql.MySQLError{Number: 1062, Message: "Duplicate key " + name})
		if logged := loggedDriverMessage(t, captured.String()); strings.Contains(logged, name) || !strings.Contains(logged, "[REDACTED") {
			t.Fatalf("registered key name logged as %q", logged)
		}
	})

	t.Run("an oversized message with text outside the list is omitted", func(t *testing.T) {
		for _, number := range []uint16{0, 1062, 1064, 1706, 1857} {
			captured := captureLog(t)
			translateBackendConflict(&mysql.MySQLError{Number: number, Message: value + " " + strings.Repeat("é", 2*driverLogMessageLimit)})
			if logged := loggedDriverMessage(t, captured.String()); logged != omittedDriverMessage {
				t.Fatalf("number %d logged %.60q", number, logged)
			}
		}
	})
}

// driverLogForm is one logged-form case: the server's message and the only
// message text its line may hold.
type driverLogForm struct {
	name    string
	number  uint16
	state   [5]byte
	message string
	want    string
	secrets []string
}

// checkDriverLogLine holds the logged-form rule for one line, and returns what
// is wrong with it so the control can prove it rejects a bad line.
func checkDriverLogLine(line, operation string, form driverLogForm) error {
	want := fmt.Sprintf(driverLogLinePattern, operation, form.number, string(form.state[:]), form.want)
	if line != want {
		return fmt.Errorf("log line = %q, want %q", line, want)
	}
	for _, text := range form.secrets {
		if strings.Contains(line, text) {
			return fmt.Errorf("log line %q holds %q", line, text)
		}
	}
	return nil
}

// TestDriverLogAdmitsOnlyFixedForms is the task's exit proof of the logged
// forms. The control runs the same check over the line the previous rule wrote,
// which held the server's message, and over lines missing each required field,
// so the check is shown to fail on what this rule exists to stop.
func TestDriverLogAdmitsOnlyFixedForms(t *testing.T) {
	const operation = "TestDriverLogAdmitsOnlyFixedForms"
	duplicate := [5]byte{'2', '3', '0', '0', '0'}
	general := [5]byte{'H', 'Y', '0', '0', '0'}
	syntax := [5]byte{'4', '2', '0', '0', '0'}
	const omitted = "server message omitted"
	const rowValue = "conformance-fixture-row-value"
	const leaf = "Leaf Error"
	const address = "192.0.2.7:3307"
	const aborted = "The open transaction was aborted due to being idle during a concurrent DDL operation."
	const refused = "Feature 'COM_STMT_SEND_LONG_DATA' is not supported by SingleStore"
	dropped := []string{rowValue, leaf, address}
	forms := []driverLogForm{
		{"1062 leaf form keeps the key and drops the value and address", 1062, duplicate, "Leaf Error (" + address + "): Duplicate entry '" + rowValue + "' for key 'PRIMARY'", "Duplicate entry [value omitted] for key 'PRIMARY'", dropped},
		{"1062 without a leaf prefix", 1062, duplicate, "Duplicate entry '" + rowValue + "' for key 'other_key'", "Duplicate entry [value omitted] for key 'other_key'", dropped},
		{"1062 without a value", 1062, duplicate, "Duplicate entry for key 'other_key'", "Duplicate entry [value omitted] for key 'other_key'", dropped},
		{"1062 unquoted key", 1062, duplicate, "Duplicate key other_key", "Duplicate entry [value omitted] for key 'other_key'", dropped},
		{"1062 value quoting another key takes the trailing key", 1062, duplicate, "Duplicate entry '" + rowValue + "' for key 'jobs_pkey' for key 'other_key'", "Duplicate entry [value omitted] for key 'other_key'", append([]string{"jobs_pkey"}, dropped...)},
		{"1062 leaf form with a value quoting another key", 1062, duplicate, "Leaf Error (" + address + "): Duplicate entry '" + rowValue + "' for key 'jobs_pkey' for key 'other_key'", "Duplicate entry [value omitted] for key 'other_key'", append([]string{"jobs_pkey"}, dropped...)},
		{"1062 value holding a line break", 1062, duplicate, "Duplicate entry '" + rowValue + "\nnext' for key 'other_key'", "Duplicate entry [value omitted] for key 'other_key'", dropped},
		{"1062 value holding invalid UTF-8", 1062, duplicate, "Duplicate entry '" + rowValue + "\xff' for key 'other_key'", "Duplicate entry [value omitted] for key 'other_key'", dropped},
		{"1062 trailing text after the key", 1062, duplicate, "Duplicate entry '" + rowValue + "' for key 'other_key' " + rowValue, omitted, dropped},
		{"1062 key name with a space", 1062, duplicate, "Duplicate entry 'x' for key 'other " + rowValue + "'", omitted, dropped},
		{"1062 key name with a hyphen", 1062, duplicate, "Duplicate entry 'x' for key '" + rowValue + "'", omitted, dropped},
		{"1062 key name with a quote", 1062, duplicate, "Duplicate entry 'x' for key 'other'" + rowValue + "'", omitted, dropped},
		{"1062 non-ASCII key name", 1062, duplicate, "Duplicate entry 'x' for key 'clé'", omitted, []string{"clé"}},
		{"1062 empty key name", 1062, duplicate, "Duplicate entry 'x' for key ''", omitted, nil},
		{"1062 with no key", 1062, duplicate, "Duplicate entry '" + rowValue + "'", omitted, dropped},
		{"1062 unquoted key with a trailing line break", 1062, duplicate, "Duplicate key other_key\n", omitted, []string{"other_key"}},
		{"1062 unknown wording", 1062, duplicate, rowValue + " for key 'other_key'", omitted, append([]string{"other_key"}, dropped...)},
		{"syntax error quoting a value", 1064, syntax, "You have an error in your SQL syntax; check the manual near '" + rowValue + "' at line 1", omitted, append([]string{"syntax", "near"}, dropped...)},
		{"leaf syntax error", 1064, syntax, "Leaf Error (" + address + "): near '" + rowValue + "'", omitted, dropped},
		{"1857 exact text is kept", 1857, general, aborted, aborted, nil},
		{"1857 other text is omitted", 1857, general, aborted + " " + rowValue, omitted, dropped},
		{"1857 with a leaf prefix is omitted", 1857, general, "Leaf Error (" + address + "): " + aborted, omitted, dropped},
		{"1706 exact text is kept", 1706, general, refused, refused, nil},
		{"1706 other text is omitted", 1706, general, "Feature '" + rowValue + "' is not supported", omitted, dropped},
		{"1857 text under another number is omitted", 1064, syntax, aborted, omitted, []string{"aborted"}},
		{"sentinel text from a server error is omitted without a list match", 0, general, rowValue, omitted, dropped},
	}
	for _, form := range forms {
		t.Run(form.name, func(t *testing.T) {
			captured := captureLog(t)
			err := translateBackendConflict(&mysql.MySQLError{Number: form.number, SQLState: form.state, Message: form.message})
			if !errors.Is(err, store.ErrBackendOperation) || err.Error() != "SingleStore database operation failed" {
				t.Fatalf("translated to %v, want only the generic error", err)
			}
			if err := checkDriverLogLine(captured.String(), operation, form); err != nil {
				t.Fatal(err)
			}
		})
	}

	t.Run("control", func(t *testing.T) {
		rejected := 0
		for _, form := range forms {
			// The previous rule wrote the server's message after redaction.
			previous := fmt.Sprintf(driverLogLinePattern, operation, form.number, string(form.state[:]), store.DriverLogMessage(form.message))
			if form.message == form.want {
				if err := checkDriverLogLine(previous, operation, form); err != nil {
					t.Fatalf("%s: the check refused an admitted line: %v", form.name, err)
				}
				continue
			}
			if checkDriverLogLine(previous, operation, form) == nil {
				t.Fatalf("control failed: %s: the check accepted the server's message", form.name)
			}
			rejected++
		}
		if rejected == 0 {
			t.Fatal("control failed: no form differs from the server's message")
		}
		form := forms[0]
		good := fmt.Sprintf(driverLogLinePattern, operation, form.number, string(form.state[:]), form.want)
		if err := checkDriverLogLine(good, operation, form); err != nil {
			t.Fatalf("the check refused a good line: %v", err)
		}
		for _, removed := range []string{"operation=" + operation + " ", "error=1062 ", "sqlstate=23000 "} {
			if checkDriverLogLine(strings.Replace(good, removed, "", 1), operation, form) == nil {
				t.Fatalf("control failed: the check accepted a line without %q", removed)
			}
		}
		leaked := form
		leaked.want = form.message
		if checkDriverLogLine(fmt.Sprintf(driverLogLinePattern, operation, form.number, string(form.state[:]), form.message), operation, leaked) == nil {
			t.Fatal("control failed: the check accepted a line holding the row value")
		}
	})
}

// TestDriverLogThroughStoreMethod reaches the logged form the way main's CI
// recorded it, through the exported method that hit a real duplicate.
func TestDriverLogThroughStoreMethod(t *testing.T) {
	captured := captureLog(t)
	st, connector, ctx := scriptedStore(t)
	const rowValue = "conformance-fixture-req-value"
	connector.script(alwaysFail(&mysql.MySQLError{Number: 1062, SQLState: [5]byte{'2', '3', '0', '0', '0'}, Message: "Leaf Error (192.0.2.7:3307): Duplicate entry '" + rowValue + " requirements_workspace_id_slug_key' for key 'PRIMARY'"}), nil)
	requirement := core.Requirement{ID: "req-fixture", Title: "Fixture requirement"}
	first := core.RequirementVersion{Content: "# Fixture", Origin: core.RequirementOriginOperator, Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Fixture statement."}}}
	_, _, err := st.CreateRequirement(ctx, requirement, first)
	if !errors.Is(err, store.ErrBackendOperation) || errors.Is(err, store.ErrRequirementSlugConflict) || err.Error() != "SingleStore database operation failed" {
		t.Fatalf("CreateRequirement error = %v, want only the generic error", err)
	}
	want := `singlestore: database operation failed: operation=CreateRequirement error=1062 sqlstate=23000 message="Duplicate entry [value omitted] for key 'PRIMARY'"` + "\n"
	if got := captured.String(); got != want {
		t.Fatalf("log line = %q, want %q", got, want)
	}
}

// hostileJobKeyValue is a row value that closes its own quote and spells a
// conflict key's whole clause, so only the trailing key names the violation.
const hostileJobKeyValue = "a' for key 'jobs_pkey"

// TestConflictMatchingIgnoresRowValues is the task's exit proof of conflict
// matching. Each hostile message holds the conflict name inside the row value
// and names another key last. The precondition shows the whole-message match
// the previous rule used would have mapped it.
func TestConflictMatchingIgnoresRowValues(t *testing.T) {
	conflicts := map[string]error{
		"jobs_pkey":                         store.ErrDispatchJobConflict,
		"jobs_dispatch_unique":              store.ErrDispatchJobConflict,
		"reference_documents_live_name_idx": store.ErrReferenceDocumentNameConflict,
		"task_start_over_task":              store.ErrTaskAlreadyStartedOver,
	}
	t.Run("hostile value under another key", func(t *testing.T) {
		messages := []string{
			"Duplicate entry '" + hostileJobKeyValue + "' for key 'other_key'",
			"Leaf Error (192.0.2.7:3307): Duplicate entry '" + hostileJobKeyValue + "' for key 'other_key'",
			"Duplicate entry 'jobs_pkey' for key 'other_key'",
			"Duplicate entry 'jobs_pkey' for key 'PRIMARY'",
		}
		for _, message := range messages {
			if !strings.Contains(message, "jobs_pkey") {
				t.Fatalf("precondition failed: %q does not hold the conflict name", message)
			}
			captured := captureLog(t)
			err := translateBackendConflict(&mysql.MySQLError{Number: 1062, Message: message})
			if errors.Is(err, store.ErrDispatchJobConflict) || !errors.Is(err, store.ErrBackendOperation) {
				t.Fatalf("%q translated to %v", message, err)
			}
			logged := loggedDriverMessage(t, captured.String())
			if strings.Contains(logged, "jobs_pkey") || !strings.HasPrefix(logged, "Duplicate entry [value omitted] for key '") {
				t.Fatalf("%q logged as %q", message, logged)
			}
		}
	})
	t.Run("every conflict name is ignored in a value", func(t *testing.T) {
		for name, conflict := range conflicts {
			for _, message := range []string{
				"Duplicate entry '" + name + "' for key 'other_key'",
				"Duplicate entry 'a' for key '" + name + "' for key 'other_key'",
				"Duplicate entry '" + name + "'",
				"Duplicate entry 'x' for key '" + name + "' trailing",
				"Duplicate key " + name + " trailing",
				"Duplicate key x " + name,
				name,
			} {
				captureLog(t)
				err := translateBackendConflict(&mysql.MySQLError{Number: 1062, Message: message})
				if errors.Is(err, conflict) || !errors.Is(err, store.ErrBackendOperation) {
					t.Fatalf("%q translated to %v", message, err)
				}
			}
		}
	})
	t.Run("each existing form still maps and logs nothing", func(t *testing.T) {
		for name, conflict := range conflicts {
			for _, message := range []string{
				"Duplicate key " + name,
				"Duplicate entry for key '" + name + "'",
				"Duplicate entry 'value' for key '" + name + "'",
				"Leaf Error (host:port): Duplicate entry 'value' for key '" + name + "'",
				"Duplicate entry 'other_key' for key '" + name + "'",
				"Duplicate entry 'a' for key 'other_key' for key '" + name + "'",
			} {
				captured := captureLog(t)
				if err := translateBackendConflict(fmt.Errorf("query: %w", &mysql.MySQLError{Number: 1062, Message: message})); !errors.Is(err, conflict) {
					t.Fatalf("%q translated to %v, want %v", message, err, conflict)
				}
				if logged := captured.String(); logged != "" {
					t.Fatalf("%q logged %q", message, logged)
				}
			}
		}
	})
	t.Run("the name maps only under 1062", func(t *testing.T) {
		captureLog(t)
		if err := translateBackendConflict(&mysql.MySQLError{Number: 1064, Message: "Duplicate key jobs_pkey"}); errors.Is(err, store.ErrDispatchJobConflict) || !errors.Is(err, store.ErrBackendOperation) {
			t.Fatalf("1064 translated to %v", err)
		}
	})
}

// TestDocumentConflictSitesMatchOutsideValues holds the requirement, decision,
// and System Design conflict sites to the same rule, including each site's
// PRIMARY match, on the raw driver error each site inspects.
func TestDocumentConflictSitesMatchOutsideValues(t *testing.T) {
	decision := core.Decision{ID: "DEC-9000", Supersedes: "DEC-8000"}
	document := core.SystemDesign{ID: "design-fixture", Slug: "design-slug"}
	requirement := core.Requirement{ID: "req-fixture", Slug: "req-slug"}
	sites := []struct {
		name  string
		call  func(error) error
		keys  map[string]error
		other []string
	}{
		{
			name:  "requirementCreateConflict",
			call:  func(err error) error { return requirementCreateConflict(err, requirement) },
			keys:  map[string]error{"requirements_workspace_id_slug_key": store.ErrRequirementSlugConflict},
			other: []string{"PRIMARY"},
		},
		{
			name: "decisionProposalConflict",
			call: func(err error) error { return decisionProposalConflict(err, decision) },
			keys: map[string]error{"PRIMARY": store.ErrDecisionIDConflict, "decisions_confirmed_supersedes_key": store.ErrDecisionSupersessionConflict},
		},
		{
			name:  "decisionSupersessionConflict",
			call:  func(err error) error { return decisionSupersessionConflict(err, decision) },
			keys:  map[string]error{"decisions_confirmed_supersedes_key": store.ErrDecisionSupersessionConflict},
			other: []string{"PRIMARY"},
		},
		{
			name: "systemDesignCreateConflict",
			call: func(err error) error { return systemDesignCreateConflict(err, document) },
			keys: map[string]error{"PRIMARY": store.ErrSystemDesignIDConflict, "system_designs_workspace_id_slug_key": store.ErrSystemDesignSlugConflict},
		},
	}
	mapped := func(err error) bool {
		for _, conflict := range []error{store.ErrRequirementSlugConflict, store.ErrDecisionIDConflict, store.ErrDecisionSupersessionConflict, store.ErrSystemDesignIDConflict, store.ErrSystemDesignSlugConflict} {
			if errors.Is(err, conflict) {
				return true
			}
		}
		return false
	}
	for _, site := range sites {
		t.Run(site.name, func(t *testing.T) {
			for key, conflict := range site.keys {
				for _, message := range []string{
					"Duplicate key " + key,
					"Duplicate entry for key '" + key + "'",
					"Duplicate entry 'value' for key '" + key + "'",
					"Leaf Error (host:port): Duplicate entry 'value' for key '" + key + "'",
				} {
					if err := site.call(fmt.Errorf("query: %w", &mysql.MySQLError{Number: 1062, Message: message})); !errors.Is(err, conflict) {
						t.Fatalf("%q mapped to %v, want %v", message, err, conflict)
					}
				}
				for _, message := range []string{
					"Duplicate entry '" + key + "' for key 'other_key'",
					"Duplicate entry 'a' for key '" + key + "' for key 'other_key'",
					"Leaf Error (host:port): Duplicate entry 'a' for key '" + key + "' for key 'other_key'",
					"Duplicate entry '" + key + "'",
				} {
					if err := site.call(&mysql.MySQLError{Number: 1062, Message: message}); mapped(err) {
						t.Fatalf("%q mapped to %v", message, err)
					}
				}
				if err := site.call(&mysql.MySQLError{Number: 1064, Message: "Duplicate key " + key}); mapped(err) {
					t.Fatalf("1064 for %s mapped to %v", key, err)
				}
				// A value naming one of this site's keys never outranks the
				// trailing key that the server named.
				for otherKey, otherConflict := range site.keys {
					if otherKey == key {
						continue
					}
					message := "Duplicate entry 'a' for key '" + otherKey + "' for key '" + key + "'"
					if err := site.call(&mysql.MySQLError{Number: 1062, Message: message}); !errors.Is(err, conflict) || errors.Is(err, otherConflict) {
						t.Fatalf("%q mapped to %v, want %v", message, err, conflict)
					}
				}
			}
			for _, key := range site.other {
				if err := site.call(&mysql.MySQLError{Number: 1062, Message: "Duplicate entry 'value' for key '" + key + "'"}); mapped(err) {
					t.Fatalf("key %s mapped to %v", key, err)
				}
			}
			if err := site.call(nil); err != nil {
				t.Fatalf("a nil error became %v", err)
			}
		})
	}
	t.Run("an unmapped error is returned as the site found it", func(t *testing.T) {
		original := errors.New("fixture application error")
		for _, call := range []func(error) error{sites[0].call, sites[1].call, sites[2].call} {
			if got := call(original); got != original {
				t.Fatalf("error became %v", got)
			}
		}
		if got := sites[3].call(original); got != nil {
			t.Fatalf("no conflict reported as %v", got)
		}
	})
}

func TestTranslatedConditionsAreNotLogged(t *testing.T) {
	captured := captureLog(t)
	application := errors.New("work order is not claimable")
	for _, tc := range []struct {
		input error
		want  error
	}{
		{&mysql.MySQLError{Number: 1062, Message: "Duplicate entry for key 'jobs_pkey'"}, store.ErrDispatchJobConflict},
		{&mysql.MySQLError{Number: 1205, Message: "lock wait timeout"}, store.ErrRetryable},
		{&mysql.MySQLError{Number: 1213, Message: "deadlock"}, store.ErrRetryable},
		{sql.ErrNoRows, sql.ErrNoRows},
		{context.Canceled, context.Canceled},
		{context.DeadlineExceeded, context.DeadlineExceeded},
		{application, application},
		{store.ErrNotFound, store.ErrNotFound},
	} {
		if got := translateBackendConflict(tc.input); !errors.Is(got, tc.want) {
			t.Fatalf("%v translated to %v, want %v", tc.input, got, tc.want)
		}
	}
	if got := notFound(sql.ErrNoRows, "work order %s", "wo-1"); !errors.Is(got, store.ErrNotFound) || !strings.Contains(got.Error(), "work order wo-1") {
		t.Fatalf("notFound(no rows) = %v", got)
	}
	if logged := captured.String(); logged != "" {
		t.Fatalf("translated conditions logged %q", logged)
	}
}

// refusingConnector fails every connection with one driver error, as a
// SingleStore server refusing a command does.
type refusingConnector struct{ err error }

func (c refusingConnector) Connect(context.Context) (driver.Conn, error) { return nil, c.err }
func (c refusingConnector) Driver() driver.Driver                        { return refusingDriver{c.err} }

type refusingDriver struct{ err error }

func (d refusingDriver) Open(string) (driver.Conn, error) { return nil, d.err }

func TestClaimDriverFailureIsLoggedWithOperation(t *testing.T) {
	captured := captureLog(t)
	refusal := &mysql.MySQLError{Number: 1706, SQLState: [5]byte{'H', 'Y', '0', '0', '0'}, Message: "Feature 'COM_STMT_SEND_LONG_DATA' is not supported by SingleStore"}
	db := sql.OpenDB(refusingConnector{refusal})
	t.Cleanup(func() { db.Close() })
	st := &Store{db: db}
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	_, err := st.ClaimWorkOrderCommand(ctx, taskops.TaskLease{}, "wo-claim", core.WorkOrderClaim{SessionID: "session", ClientToken: "token"})
	if !errors.Is(err, store.ErrBackendOperation) || strings.Contains(err.Error(), "COM_STMT_SEND_LONG_DATA") {
		t.Fatalf("claim error = %v, want the generic backend error", err)
	}
	var driverErr *mysql.MySQLError
	if errors.As(err, &driverErr) {
		t.Fatal("driver error escaped the claim")
	}
	line := captured.String()
	for _, want := range []string{"operation=ClaimWorkOrderCommand", "error=1706", "sqlstate=HY000", "COM_STMT_SEND_LONG_DATA"} {
		if !strings.Contains(line, want) {
			t.Fatalf("claim log %q lacks %q", line, want)
		}
	}
}

func TestExportedOperationNames(t *testing.T) {
	for function, want := range map[string]string{
		storePackage + ".(*Store).ClaimWorkOrderCommand":       "ClaimWorkOrderCommand",
		storePackage + ".(*Store).ClaimWorkOrderCommand.func1": "ClaimWorkOrderCommand",
		storePackage + ".Open":                                 "Open",
		storePackage + ".Open.func2":                           "Open",
	} {
		if got, ok := exportedOperation(function); !ok || got != want {
			t.Fatalf("exportedOperation(%q) = %q, %v; want %q", function, got, ok, want)
		}
	}
	for _, function := range []string{
		storePackage + ".(*Store).withTx",
		storePackage + ".(*Store).orderTx.func1",
		storePackage + ".translateBackendConflict",
		storePackage + ".(*meteredConn).QueryContext",
		"database/sql.(*DB).QueryRowContext",
		"github.com/kidus-tiliksew/conveyor/internal/store/postgres.(*Store).ClaimWorkOrderCommand",
	} {
		if got, ok := exportedOperation(function); ok {
			t.Fatalf("exportedOperation(%q) = %q, want no operation", function, got)
		}
	}
}
