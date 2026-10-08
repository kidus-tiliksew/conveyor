package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/spf13/cobra"
)

// retiredSetupServer fails the test on any request: a refused CLI act must
// never reach the server.
func retiredSetupServer(t *testing.T) *atomic.Int32 {
	t.Helper()
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "unexpected request", http.StatusTeapot)
	}))
	t.Cleanup(server.Close)
	t.Setenv("CONVEYOR_ADDR", server.URL)
	t.Setenv("CONVEYOR_API_TOKEN", "token")
	workspaceFlag = "demo"
	return &requests
}

func taskRoot() *cobra.Command {
	root := &cobra.Command{Use: "conveyor", SilenceUsage: true, SilenceErrors: true}
	root.AddCommand(taskCmd())
	return root
}

// DEC-56(2); component-harness-execution "Setup acts": task creation has no
// setup selection, and the flag is refused before any request.
func TestTaskNewRejectsSetupFlag(t *testing.T) {
	requests := retiredSetupServer(t)
	root := taskRoot()
	var output strings.Builder
	root.SetOut(&output)
	root.SetErr(&output)
	root.SetArgs([]string{"task", "new", "--repo", "app", "-m", "fix it", "--setup", "backend"})
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "unknown flag: --setup") {
		t.Fatalf("task new --setup error = %v", err)
	}
	if requests.Load() != 0 {
		t.Fatalf("refused task new sent %d request(s)", requests.Load())
	}
	help := taskRoot()
	var helpOutput strings.Builder
	help.SetOut(&helpOutput)
	help.SetArgs([]string{"task", "new", "--help"})
	if err := help.Execute(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(helpOutput.String(), "--setup") || strings.Contains(helpOutput.String(), "execution setup") {
		t.Fatalf("task new help still advertises setup selection:\n%s", helpOutput.String())
	}
	if !strings.Contains(helpOutput.String(), "--spec-approval") || !strings.Contains(helpOutput.String(), "--depends-on") {
		t.Fatalf("task new help lost policy flags:\n%s", helpOutput.String())
	}
}

// The former `conveyor task setup` act is not a command, so nothing reaches
// the server's retired 410 route.
func TestTaskSetupCommandRemoved(t *testing.T) {
	requests := retiredSetupServer(t)
	for _, command := range taskCmd().Commands() {
		if command.Name() == "setup" || command.HasAlias("setup") {
			t.Fatalf("task command still registers %q", command.CommandPath())
		}
	}
	root := taskRoot()
	var output strings.Builder
	root.SetOut(&output)
	root.SetErr(&output)
	root.SetArgs([]string{"task", "setup", "task-1", "--setup", "next", "--request-id", "request-1"})
	if err := root.Execute(); err == nil {
		t.Fatalf("task setup executed; output:\n%s", output.String())
	}
	if requests.Load() != 0 {
		t.Fatalf("removed task setup sent %d request(s)", requests.Load())
	}
	help := taskRoot()
	var helpOutput strings.Builder
	help.SetOut(&helpOutput)
	help.SetArgs([]string{"task", "--help"})
	if err := help.Execute(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(helpOutput.String(), "setup") {
		t.Fatalf("task help still lists a setup act:\n%s", helpOutput.String())
	}
}
