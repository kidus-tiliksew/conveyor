package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/config"
)

// failingReader fails the test when init reads standard input.
type failingReader struct{ t *testing.T }

func (r failingReader) Read([]byte) (int, error) {
	r.t.Helper()
	r.t.Error("conveyor init read standard input on the answer-flag path")
	return 0, errors.New("standard input must not be read")
}

func allInitAnswerFlags() []string {
	return []string{
		"--organization", "Flag Org", "--operator-name", "Flag Operator", "--operator-email", "owner@example.test",
		"--workspace-id", "flags", "--workspace-name", "Flags", "--repository-name", "app",
		"--repository-url", "https://github.com/example/app", "--default-branch", "trunk",
	}
}

// req-agent-guided-setup AC-5.1: every answer flag initializes without
// reading standard input; the flagged answers reach the shared init path.
func TestInitAnswerFlagsSkipStandardInput(t *testing.T) {
	command := initCmd()
	command.SetIn(failingReader{t})
	var output strings.Builder
	command.SetOut(&output)
	if err := command.ParseFlags(allInitAnswerFlags()); err != nil {
		t.Fatal(err)
	}
	if !initAnswerFlagsGiven(command) {
		t.Fatal("answer flags were not detected")
	}
	values := map[string]*string{}
	for _, field := range initAnswerFields() {
		value, err := command.Flags().GetString(field.flag)
		if err != nil {
			t.Fatal(err)
		}
		values[field.flag] = &value
	}
	answers, err := initAnswersFromFlags(command, values)
	if err != nil {
		t.Fatal(err)
	}
	want := initAnswers{
		Organization: "Flag Org", OperatorName: "Flag Operator", OperatorEmail: "owner@example.test",
		WorkspaceID: "flags", WorkspaceName: "Flags", RepositoryName: "app",
		RepositoryURL: "https://github.com/example/app", BaseBranch: "trunk",
	}
	if answers != want {
		t.Fatalf("answers = %+v, want %+v", answers, want)
	}
	if output.Len() != 0 {
		t.Fatalf("answer-flag path printed prompts: %q", output.String())
	}
}

// The whole command takes the flag path past the answers: with every answer
// flag it reads no input, prints no prompt, and stops at the first deployment
// precondition instead.
func TestInitCommandWithEveryAnswerFlagReachesDeploymentChecks(t *testing.T) {
	t.Setenv("CONVEYOR_DATABASE_URL", "")
	t.Setenv(config.LLMAPIKeyEnv, "present")
	configPath := filepath.Join(t.TempDir(), "conveyor.yaml")
	command := initCmd()
	command.SetIn(failingReader{t})
	var output strings.Builder
	command.SetOut(&output)
	command.SetErr(&output)
	command.SetArgs(append(allInitAnswerFlags(), "--config", configPath))
	err := command.Execute()
	if err == nil || !strings.Contains(err.Error(), "CONVEYOR_DATABASE_URL is required") {
		t.Fatalf("error = %v, want the database precondition after flag answers", err)
	}
	if strings.Contains(output.String(), "Organization name") {
		t.Fatalf("answer-flag path printed prompts: %q", output.String())
	}
}

// Absent flags take the prompt defaults; required answers that are absent
// without a default or given blank are all named, and nothing is written.
func TestInitAnswerFlagsNameEveryMissingAnswerAndWriteNothing(t *testing.T) {
	t.Setenv(config.OrganizationNameEnv, "Env Org")
	t.Setenv("CONVEYOR_DATABASE_URL", "postgres://example")
	t.Setenv("CONVEYOR_API_TOKEN", "operator-token")
	t.Setenv(config.LLMAPIKeyEnv, "present")
	configPath := filepath.Join(t.TempDir(), "nested", "conveyor.yaml")
	command := initCmd()
	command.SetIn(failingReader{t})
	var output strings.Builder
	command.SetOut(&output)
	command.SetErr(&output)
	command.SetArgs([]string{"--workspace-id", "demo", "--operator-email", " ", "--default-branch", "", "--config", configPath})
	err := command.Execute()
	if err == nil {
		t.Fatal("init with missing answers succeeded")
	}
	for _, want := range []string{"--operator-email (first operator email)", "--repository-url (repository URL)", "--default-branch (default branch)"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not name %s", err, want)
		}
	}
	for _, defaulted := range []string{"--organization", "--operator-name", "--workspace-name", "--repository-name", "--workspace-id"} {
		if strings.Contains(err.Error(), defaulted+" ") {
			t.Fatalf("error %q names %s, which has a default or a value", err, defaulted)
		}
	}
	if _, statErr := os.Stat(filepath.Dir(configPath)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("init with missing answers wrote state: %v", statErr)
	}
	if strings.Contains(output.String(), "Organization name") {
		t.Fatalf("answer-flag path printed prompts: %q", output.String())
	}
}

// With no answer flag, init keeps its prompts and reads line-by-line answers
// piped on standard input (req-deployment-and-releases AC-2.3).
func TestInitCommandWithoutAnswerFlagsKeepsPrompts(t *testing.T) {
	t.Setenv("CONVEYOR_DATABASE_URL", "")
	t.Setenv(config.LLMAPIKeyEnv, "present")
	command := initCmd()
	command.SetIn(strings.NewReader("Org\nOwner\nowner@example.test\ndemo\nDemo\napp\nhttps://github.com/example/app\nmain\n"))
	var output strings.Builder
	command.SetOut(&output)
	command.SetErr(&output)
	command.SetArgs([]string{"--config", filepath.Join(t.TempDir(), "conveyor.yaml")})
	err := command.Execute()
	if err == nil || !strings.Contains(err.Error(), "CONVEYOR_DATABASE_URL is required") {
		t.Fatalf("error = %v, want the database precondition after prompted answers", err)
	}
	for _, prompt := range []string{"Organization name", "Repository URL: ", "Default branch [main]: "} {
		if !strings.Contains(output.String(), prompt) {
			t.Fatalf("prompt %q missing from %q", prompt, output.String())
		}
	}
}

// Every answer readInitAnswers collects has exactly one flag, and no flag
// carries a secret.
func TestInitAnswerFlagsCoverEveryPromptAndNoSecret(t *testing.T) {
	command := initCmd()
	fields := initAnswerFields()
	if len(fields) != 8 {
		t.Fatalf("init answer fields = %d, want the eight prompted answers", len(fields))
	}
	for _, field := range fields {
		if command.Flags().Lookup(field.flag) == nil {
			t.Fatalf("answer %q has no --%s flag", field.label, field.flag)
		}
		for _, secret := range []string{"token", "password", "secret", "key"} {
			if strings.Contains(field.flag, secret) {
				t.Fatalf("answer flag --%s names a secret", field.flag)
			}
		}
	}
}
