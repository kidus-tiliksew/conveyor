package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"gopkg.in/yaml.v3"
)

func TestInitPrerequisitesNameEveryMissingRequirement(t *testing.T) {
	clone := t.TempDir()
	answers := initAnswers{RepositoryName: "app", RepositoryURL: "github.com/example/app", ClonePath: clone}
	tests := []struct {
		name          string
		prerequisites initPrerequisites
		want          string
	}{
		{name: "git absent", prerequisites: initPrerequisites{lookPath: func(name string) (string, error) { return "", errors.New(name + " absent") }}, want: "git is required"},
		{name: "gh absent", prerequisites: initPrerequisites{lookPath: func(name string) (string, error) {
			if name == "git" {
				return "/git", nil
			}
			return "", errors.New("gh absent")
		}}, want: "run `gh auth login`"},
		{name: "gh unauthenticated", prerequisites: initPrerequisites{lookPath: func(name string) (string, error) { return "/" + name, nil }, run: func(_ context.Context, name string, _ ...string) ([]byte, error) {
			if name == "/gh" {
				return []byte("no account"), errors.New("exit 1")
			}
			return []byte(clone), nil
		}}, want: "run `gh auth login`"},
		{name: "clone absent", prerequisites: initPrerequisites{lookPath: func(name string) (string, error) { return "/" + name, nil }, run: func(_ context.Context, _ string, _ ...string) ([]byte, error) { return nil, nil }, stat: func(string) (os.FileInfo, error) { return nil, os.ErrNotExist }}, want: "repository clone"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.prerequisites.stat == nil {
				test.prerequisites.stat = os.Stat
			}
			if test.prerequisites.run == nil {
				test.prerequisites.run = func(context.Context, string, ...string) ([]byte, error) { return nil, nil }
			}
			err := checkInitPrerequisites(t.Context(), test.prerequisites, answers)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v, want %q", err, test.want)
			}
		})
	}
}

func TestReadInitAnswersUsesDefaultsAndRequiresRepositoryURL(t *testing.T) {
	t.Setenv(config.OrganizationNameEnv, "Example Org")
	input := strings.NewReader("\nOwner\nowner@example.test\ndemo\nDemo\napp\nhttps://github.com/example/app\nmain\n/tmp/app\n")
	var output strings.Builder
	answers, err := readInitAnswers(input, &output)
	if err != nil {
		t.Fatal(err)
	}
	if answers.Organization != "Example Org" || answers.WorkspaceID != "demo" || answers.RepositoryName != "app" || answers.ClonePath != "/tmp/app" {
		t.Fatalf("answers=%+v", answers)
	}
	if !strings.Contains(output.String(), "Organization name [Example Org]") {
		t.Fatalf("prompts=%q", output.String())
	}
}

func TestDefaultInitConfigValidatesWithoutHandEditing(t *testing.T) {
	answers := initAnswers{WorkspaceID: "demo", RepositoryName: "app", RepositoryURL: "https://github.com/example/app", BaseBranch: "main"}
	data, err := yaml.Marshal(defaultInitConfig("postgres://example", answers))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "conveyor.yaml")
	if err = os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Workspace != "demo" || len(loaded.Repos) != 1 || loaded.Repos[0].Name != "app" || loaded.DefaultSetup != "default" {
		t.Fatalf("config=%+v", loaded)
	}
}
