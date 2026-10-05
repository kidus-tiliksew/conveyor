package main

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
)

// probeFixtureRecordEnv switches writeProbeFixture into the slow-first-start
// mode that TestProbeFixtureUsersSurviveSlowFirstExecution uses in its child
// processes. Each fixture then records its invocations under the named
// directory, and a healthy fixture's first start sleeps past its production
// probe timeout.
const probeFixtureRecordEnv = "CONVEYOR_TEST_PROBE_FIXTURE_RECORD_DIR"

// writeProbeFixture writes a harness probe fixture and runs it once before
// returning. A host can delay the first start of a newly written executable
// (macOS assesses each one), and that delay must not count against the
// production probe timeout that detectLocalHarnessHealth applies later
// (component-verification-strategy Known gaps).
func writeProbeFixture(t *testing.T, directory, name, output string, healthy bool) {
	t.Helper()
	exit := "0"
	if !healthy {
		exit = "1"
	}
	path := filepath.Join(directory, name)
	recordDir := os.Getenv(probeFixtureRecordEnv)
	var record string
	var delay time.Duration
	prelude := ""
	prepared := "prepared"
	if recordDir != "" {
		record = filepath.Join(recordDir, strings.ReplaceAll(t.Name(), "/", "_")+"-"+filepath.Base(directory)+"-"+name+".log")
		firstStart := ""
		if healthy {
			delay = probeFixtureSlowStart(t, name)
			sleep, err := exec.LookPath("sleep")
			if err != nil {
				t.Fatal(err)
			}
			firstStart = "\t" + shellQuote(t, sleep) + " " + strconv.Itoa(int(delay/time.Second)) + " || exit 97\n"
			prepared = "prepared-delayed"
		}
		prelude = "record=" + shellQuote(t, record) + "\n" +
			"if [ -e \"$record\" ]; then\n" +
			"\tprintf 'reused\\n' >> \"$record\"\n" +
			"else\n" + firstStart +
			"\tprintf '" + prepared + "\\n' > \"$record\"\n" +
			"fi\n"
	}
	contents := "#!/bin/sh\n" + prelude + "printf '%s\\n' '" + output + "'\nexit " + exit + "\n"
	if err := os.WriteFile(path, []byte(contents), 0o700); err != nil {
		t.Fatal(err)
	}

	started := time.Now()
	combined, err := exec.Command(path, "--version").CombinedOutput()
	elapsed := time.Since(started)
	if got := strings.TrimSpace(string(combined)); got != output {
		t.Fatalf("preparing probe fixture %s printed %q, want %q (err %v)", name, got, output, err)
	}
	var exitErr *exec.ExitError
	if healthy && err != nil {
		t.Fatalf("preparing healthy probe fixture %s: %v", name, err)
	}
	if !healthy && (!errors.As(err, &exitErr) || exitErr.ExitCode() != 1) {
		t.Fatalf("preparing broken probe fixture %s: err %v, want exit status 1", name, err)
	}
	if record == "" {
		return
	}
	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != prepared+"\n" {
		t.Fatalf("probe fixture %s preparation record = %q, want %q", name, data, prepared+"\n")
	}
	if elapsed < delay {
		t.Fatalf("probe fixture %s preparation took %s, want at least the %s first-start delay", name, elapsed, delay)
	}
}

// probeFixtureSlowStart returns a whole-second first-start delay that exceeds
// the production probe timeout of the harness template probing name.
func probeFixtureSlowStart(t *testing.T, name string) time.Duration {
	t.Helper()
	for _, template := range config.HarnessTemplates() {
		if template.Harness.ProbeCommand[0] != name {
			continue
		}
		timeout, err := time.ParseDuration(template.Harness.ProbeTimeoutText)
		if err != nil || timeout <= 0 {
			t.Fatalf("harness template %s probe timeout %q: %v", template.Harness.Name, template.Harness.ProbeTimeoutText, err)
		}
		return timeout.Truncate(time.Second) + time.Second
	}
	t.Fatalf("no harness template probes %q", name)
	return 0
}

func shellQuote(t *testing.T, value string) string {
	t.Helper()
	if strings.Contains(value, "'") {
		t.Fatalf("cannot single-quote %q", value)
	}
	return "'" + value + "'"
}

// TestProbeFixtureUsersSurviveSlowFirstExecution reruns every test that calls
// writeProbeFixture in a child process whose healthy fixtures sleep past the
// production probe timeout on their first start. Each child must pass with
// unchanged probe deadlines, and each fixture's record must show that
// preparation consumed the first start and the real probe reused the prepared
// executable.
func TestProbeFixtureUsersSurviveSlowFirstExecution(t *testing.T) {
	if os.Getenv(probeFixtureRecordEnv) != "" {
		t.Skip("running as a probe fixture regression child")
	}
	t.Parallel()
	users := probeFixtureUsers(t)
	for _, name := range []string{
		"TestDetectLocalHarnessesOffersOnlyHealthyPresentTemplates",
		"TestConfigInitExecutionWritesUserDefaultForOtherConsumers",
		"TestExecutionWizardDeclineWritesNothing",
		"TestExecutionSetupDefaultsWritesWithoutTerminal",
		"TestExecutionWizardPreservesExistingLocalConfiguration",
		"TestExecutionWizardUsesDetectionProbeWithoutDiscardingAnswers",
	} {
		if !slices.Contains(users, name) {
			t.Fatalf("writeProbeFixture users %v omit %s", users, name)
		}
	}
	directory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()

	type child struct {
		records string
		output  []byte
		err     error
	}
	children := make([]child, len(users))
	var running sync.WaitGroup
	for index, name := range users {
		children[index].records = t.TempDir()
		running.Add(1)
		go func() {
			defer running.Done()
			command := exec.CommandContext(ctx, os.Args[0], "-test.run=^"+name+"$", "-test.count=1", "-test.v")
			command.Dir = directory
			command.Env = append(os.Environ(), probeFixtureRecordEnv+"="+children[index].records)
			children[index].output, children[index].err = command.CombinedOutput()
		}()
	}
	running.Wait()

	for index, name := range users {
		result := children[index]
		if result.err != nil || !strings.Contains(string(result.output), "--- PASS: "+name+" (") {
			t.Errorf("%s failed under slow first fixture starts: %v\n%s", name, result.err, result.output)
			continue
		}
		entries, err := os.ReadDir(result.records)
		if err != nil {
			t.Fatal(err)
		}
		delayed := 0
		for _, entry := range entries {
			data, err := os.ReadFile(filepath.Join(result.records, entry.Name()))
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimSpace(string(data)), "\n")
			if lines[0] == "prepared-delayed" {
				delayed++
			} else if lines[0] != "prepared" {
				t.Errorf("%s fixture record %s did not start with preparation: %q", name, entry.Name(), data)
				continue
			}
			if len(lines) < 2 {
				t.Errorf("%s fixture record %s shows no probe after preparation: %q", name, entry.Name(), data)
			}
			for _, line := range lines[1:] {
				if line != "reused" {
					t.Errorf("%s fixture record %s has unexpected invocation %q: %q", name, entry.Name(), line, data)
				}
			}
		}
		if delayed == 0 {
			t.Errorf("%s wrote no healthy fixture with a delayed first start; records %v", name, entries)
		}
	}
}

// probeFixtureUsers lists the package's top-level tests that call
// writeProbeFixture, so a new user joins the slow-first-start regression
// without editing it.
func probeFixtureUsers(t *testing.T) []string {
	t.Helper()
	paths, err := filepath.Glob("*_test.go")
	if err != nil {
		t.Fatal(err)
	}
	files := token.NewFileSet()
	var users []string
	for _, path := range paths {
		file, err := parser.ParseFile(files, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Recv != nil || function.Body == nil || !strings.HasPrefix(function.Name.Name, "Test") {
				continue
			}
			calls := false
			ast.Inspect(function.Body, func(node ast.Node) bool {
				if call, ok := node.(*ast.CallExpr); ok {
					if identifier, ok := call.Fun.(*ast.Ident); ok && identifier.Name == "writeProbeFixture" {
						calls = true
					}
				}
				return !calls
			})
			if calls {
				users = append(users, function.Name.Name)
			}
		}
	}
	slices.Sort(users)
	return users
}
