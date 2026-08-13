package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/gitx"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	postgresstore "github.com/kidus-tiliksew/conveyor/internal/store/postgres"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

type initAnswers struct {
	Organization, OperatorName, OperatorEmail string
	WorkspaceID, WorkspaceName                string
	RepositoryName, RepositoryURL, BaseBranch string
	ClonePath                                 string
}

type initPrerequisites struct {
	lookPath func(string) (string, error)
	run      func(context.Context, string, ...string) ([]byte, error)
	stat     func(string) (os.FileInfo, error)
	getenv   func(string) string
}

func initCmd() *cobra.Command {
	var configPath string
	command := &cobra.Command{
		Use:   "init",
		Short: "Initialize a Conveyor organization and first workspace",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			answers, err := readInitAnswers(cmd.InOrStdin(), cmd.OutOrStdout())
			if err != nil {
				return err
			}
			prerequisites := initPrerequisites{
				lookPath: exec.LookPath,
				run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
					return exec.CommandContext(ctx, name, args...).CombinedOutput()
				},
				stat:   os.Stat,
				getenv: os.Getenv,
			}
			if err = checkInitPrerequisites(cmd.Context(), prerequisites, answers); err != nil {
				return err
			}
			return initializeDeployment(cmd.Context(), cmd.OutOrStdout(), configPath, answers)
		},
	}
	command.Flags().StringVar(&configPath, "config", "conveyor.yaml", "deployment config to create")
	return command
}

func readInitAnswers(input io.Reader, output io.Writer) (initAnswers, error) {
	reader := bufio.NewReader(input)
	identity := config.FirstOperatorIdentityFromEnvironment()
	current, _ := os.Getwd()
	fields := []struct {
		label        string
		defaultValue string
		destination  *string
	}{
		{"Organization name", identity.OrganizationName, nil},
		{"First operator display name", identity.DisplayName, nil},
		{"First operator email", identity.Email, nil},
		{"Workspace id", "default", nil},
		{"Workspace name", "Default", nil},
		{"Repository name", filepath.Base(current), nil},
		{"Repository URL", "", nil},
		{"Default branch", "main", nil},
		{"Repository clone path", current, nil},
	}
	answers := initAnswers{}
	destinations := []*string{&answers.Organization, &answers.OperatorName, &answers.OperatorEmail, &answers.WorkspaceID, &answers.WorkspaceName, &answers.RepositoryName, &answers.RepositoryURL, &answers.BaseBranch, &answers.ClonePath}
	for i := range fields {
		fields[i].destination = destinations[i]
		if fields[i].defaultValue == "" {
			fmt.Fprintf(output, "%s: ", fields[i].label)
		} else {
			fmt.Fprintf(output, "%s [%s]: ", fields[i].label, fields[i].defaultValue)
		}
		line, err := reader.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return initAnswers{}, fmt.Errorf("read %s: %w", strings.ToLower(fields[i].label), err)
		}
		value := strings.TrimSpace(line)
		if value == "" {
			value = fields[i].defaultValue
		}
		*fields[i].destination = value
		if errors.Is(err, io.EOF) && i != len(fields)-1 {
			return initAnswers{}, fmt.Errorf("read %s: input ended before initialization answers were complete", strings.ToLower(fields[i].label))
		}
	}
	for label, value := range map[string]string{
		"organization name": answers.Organization, "first operator display name": answers.OperatorName,
		"first operator email": answers.OperatorEmail, "workspace id": answers.WorkspaceID,
		"workspace name": answers.WorkspaceName, "repository name": answers.RepositoryName,
		"repository URL": answers.RepositoryURL, "default branch": answers.BaseBranch,
		"repository clone path": answers.ClonePath,
	} {
		if strings.TrimSpace(value) == "" {
			return initAnswers{}, fmt.Errorf("%s is required", label)
		}
	}
	return answers, nil
}

func checkInitPrerequisites(ctx context.Context, prerequisites initPrerequisites, answers initAnswers) error {
	git, err := prerequisites.lookPath("git")
	if err != nil {
		return errors.New("git is required on the Conveyor host; install git and rerun `conveyor init`")
	}
	gh, err := prerequisites.lookPath("gh")
	if err != nil {
		return errors.New("GitHub CLI `gh` is required on the Conveyor host; install it, then run `gh auth login`")
	}
	if output, authErr := prerequisites.run(ctx, gh, "auth", "status"); authErr != nil {
		detail := strings.TrimSpace(string(output))
		if detail != "" {
			return fmt.Errorf("GitHub CLI is not authenticated; run `gh auth login` and retry (%s)", detail)
		}
		return errors.New("GitHub CLI is not authenticated; run `gh auth login` and retry")
	}
	clone, err := resolvedInitClonePath(answers.ClonePath)
	if err != nil {
		return fmt.Errorf("resolve repository clone path %q: %w", answers.ClonePath, err)
	}
	info, err := prerequisites.stat(clone)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("repository clone for %q is missing at %s; run `gh repo clone %s %s`", answers.RepositoryName, clone, answers.RepositoryURL, clone)
	}
	if output, gitErr := prerequisites.run(ctx, git, "-C", clone, "rev-parse", "--show-toplevel"); gitErr != nil || !sameFilesystemPath(strings.TrimSpace(string(output)), clone) {
		return fmt.Errorf("repository path %s is not a filesystem clone; run `gh repo clone %s %s`", clone, answers.RepositoryURL, clone)
	}
	packDir := filepath.Join(clone, "pack")
	for _, role := range []string{"triage", "planning", "spec", "implement", "review"} {
		rolePath := filepath.Join(packDir, "roles", role+".md")
		roleInfo, roleErr := prerequisites.stat(rolePath)
		if roleErr != nil || roleInfo.IsDir() {
			return fmt.Errorf("Conveyor prompt pack is missing required role %s", rolePath)
		}
	}
	generated, err := defaultInitConfig("postgres://init-prerequisite", answers)
	if err != nil {
		return err
	}
	getenv := prerequisites.getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	if configUsesInProcessExecution(generated) && strings.TrimSpace(getenv("CONVEYOR_API_KEY")) == "" {
		return errors.New("CONVEYOR_API_KEY is required for in-process triage and spec stages; set it and rerun `conveyor init`")
	}
	return nil
}

func resolvedInitClonePath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve repository clone path %q: %w", path, err)
	}
	if resolved, resolveErr := filepath.EvalSymlinks(absolute); resolveErr == nil {
		absolute = resolved
	}
	return filepath.Clean(absolute), nil
}

func configUsesInProcessExecution(candidate config.Config) bool {
	if candidate.ExecutionSettings != nil {
		return true
	}
	for _, route := range candidate.Routing.Stages {
		if route.Execution == "" || route.Execution == config.ExecutionInProcess {
			return true
		}
	}
	return false
}

func sameFilesystemPath(left, right string) bool {
	leftResolved, leftErr := filepath.EvalSymlinks(left)
	rightResolved, rightErr := filepath.EvalSymlinks(right)
	return leftErr == nil && rightErr == nil && filepath.Clean(leftResolved) == filepath.Clean(rightResolved)
}

func initializeDeployment(ctx context.Context, output io.Writer, configPath string, answers initAnswers) error {
	databaseURL := strings.TrimSpace(os.Getenv("CONVEYOR_DATABASE_URL"))
	if databaseURL == "" {
		return errors.New("CONVEYOR_DATABASE_URL is required; set it to the running Postgres database and rerun `conveyor init`")
	}
	apiToken := strings.TrimSpace(os.Getenv("CONVEYOR_API_TOKEN"))
	if apiToken == "" {
		return errors.New("CONVEYOR_API_TOKEN is required; set the first operator's bearer token and rerun `conveyor init`")
	}
	absoluteConfig, err := filepath.Abs(configPath)
	if err != nil {
		return fmt.Errorf("resolve config path: %w", err)
	}
	_, statErr := os.Stat(absoluteConfig)
	configExists := statErr == nil
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return fmt.Errorf("inspect deployment config: %w", statErr)
	}
	directory := filepath.Dir(absoluteConfig)
	temporaryPath := ""
	var validated *config.Config
	if configExists {
		validated, err = config.Load(absoluteConfig)
		if err != nil {
			return fmt.Errorf("load existing deployment config: %w", err)
		}
		if err = requireInitAPIKey(*validated); err != nil {
			return err
		}
		if validated.Workspace != answers.WorkspaceID || len(validated.Repos) != 1 || validated.Repos[0].Name != answers.RepositoryName || validated.Repos[0].URL != answers.RepositoryURL || validated.Database.URL != databaseURL {
			return fmt.Errorf("deployment config already exists at %s and does not match these initialization answers; refusing to overwrite it", absoluteConfig)
		}
	} else {
		candidate, candidateErr := defaultInitConfig(databaseURL, answers)
		if candidateErr != nil {
			return candidateErr
		}
		if err = requireInitAPIKey(candidate); err != nil {
			return err
		}
		data, marshalErr := yaml.Marshal(candidate)
		if marshalErr != nil {
			return fmt.Errorf("render deployment config: %w", marshalErr)
		}
		if err = os.MkdirAll(directory, 0o700); err != nil {
			return fmt.Errorf("create deployment config directory: %w", err)
		}
		temporary, createErr := os.CreateTemp(directory, ".conveyor-init-*.yaml")
		if createErr != nil {
			return fmt.Errorf("stage deployment config: %w", createErr)
		}
		temporaryPath = temporary.Name()
		defer os.Remove(temporaryPath) //nolint:errcheck
		if _, err = temporary.Write(data); err != nil {
			temporary.Close()
			return fmt.Errorf("stage deployment config: %w", err)
		}
		if err = temporary.Chmod(0o600); err != nil {
			temporary.Close()
			return fmt.Errorf("secure deployment config: %w", err)
		}
		if err = temporary.Close(); err != nil {
			return fmt.Errorf("stage deployment config: %w", err)
		}
		validated, err = config.Load(temporaryPath)
		if err != nil {
			return fmt.Errorf("validate generated deployment config: %w", err)
		}
	}
	pgStore, err := postgresstore.Open(ctx, databaseURL)
	if err != nil {
		return fmt.Errorf("initialize Postgres store: %w", err)
	}
	defer pgStore.Close()
	seeded, err := pgStore.BootstrapIdentity(ctx, config.FirstOperatorIdentity{
		OrganizationName: answers.Organization, Email: answers.OperatorEmail, DisplayName: answers.OperatorName,
	}, apiToken)
	if err != nil {
		return err
	}
	if !seeded {
		if _, workspaceErr := pgStore.GetWorkspace(ctx, answers.WorkspaceID); workspaceErr == nil {
			fmt.Fprintln(output, "Conveyor is already initialized; no changes were made.")
			return nil
		}
	}
	owner, err := pgStore.VerifyPersonalAccessToken(ctx, apiToken)
	if err != nil {
		return fmt.Errorf("resolve first operator after bootstrap: %w", err)
	}
	operatorCtx := store.WithCredential(ctx, core.AuthenticatedCredential{ID: "bootstrap", OwnerUserID: owner.ID, Kind: core.CredentialUser, Scope: core.CredentialScopeOperator})
	operatorCtx = store.WithActor(operatorCtx, store.Actor{ID: store.UserActorID(owner.ID), Role: core.ActorUser})
	if _, err = pgStore.CreateWorkspace(operatorCtx, answers.WorkspaceID, answers.WorkspaceName, validated); err != nil {
		if !errors.Is(err, store.ErrWorkspaceConflict) {
			return fmt.Errorf("create first workspace: %w", err)
		}
	}
	if !configExists {
		if err = os.Rename(temporaryPath, absoluteConfig); err != nil {
			return fmt.Errorf("publish deployment config %s: %w", absoluteConfig, err)
		}
	}
	fmt.Fprintf(output, "Initialized Conveyor organization %q and workspace %q.\nConfig: %s\nNext: run `conveyord install --config %s`.\n", answers.Organization, answers.WorkspaceID, absoluteConfig, absoluteConfig)
	return nil
}

func requireInitAPIKey(candidate config.Config) error {
	if configUsesInProcessExecution(candidate) && strings.TrimSpace(os.Getenv("CONVEYOR_API_KEY")) == "" {
		return errors.New("CONVEYOR_API_KEY is required for in-process triage and spec stages; set it and rerun `conveyor init`")
	}
	return nil
}

func defaultInitConfig(databaseURL string, answers initAnswers) (config.Config, error) {
	clone, err := resolvedInitClonePath(answers.ClonePath)
	if err != nil {
		return config.Config{}, err
	}
	harness := config.HarnessTemplates()[0].Harness
	settings := config.ContextualExecutionSettings{
		ControlPlane: config.ControlPlaneSettings{
			Triage:   config.ModelTimeoutSettings{Model: "gpt-5.6-luna", Effort: "high", TimeoutText: "20m"},
			Planning: config.PlanningSettings{Model: "gpt-5.6-luna", Effort: "high", TimeoutText: "20m"},
		},
		Spec:           config.ImplementationSettings{Harness: harness.Name, Model: "gpt-5.6-sol", ModelPolicy: config.ModelPolicyExplicit, Effort: "high", TimeoutText: "30m"},
		Implementation: config.ImplementationSettings{Harness: harness.Name, Model: "gpt-5.6-sol", ModelPolicy: config.ModelPolicyExplicit, Effort: "high", TimeoutText: "4h"},
		Review:         config.ReviewExecutionSettings{Execution: config.ExecutionMCP, TimeoutText: "1h"},
	}
	review := config.ReviewPanel{Seats: []config.ReviewSeat{{Model: "gpt-5.6-terra", Harness: harness.Name, Effort: "high"}}}
	return config.Config{
		Workspace: answers.WorkspaceID, PackDir: filepath.Join(clone, "pack"), MaxBounces: 10,
		WorkOrderQueueTimeoutText: config.DefaultWorkOrderQueueTimeoutText,
		Database:                  config.Database{Backend: "postgres", URL: databaseURL},
		ExecutionSettings:         &settings,
		Harnesses:                 []config.Harness{harness},
		Review:                    review,
		Setups:                    []config.ExecutionSetup{{Name: "default", ExecutionSettings: settings, Review: review, RefreshReview: config.RefreshReviewDelta}},
		DefaultSetup:              "default",
		Execution:                 config.ExecutionPolicy{SpecApproval: true, MergeApproval: true, ImplementConcurrency: 1, ReviewConcurrency: 1, FirstActivityTimeoutText: config.DefaultFirstActivityTimeoutText},
		Repos:                     []config.Repo{{Name: answers.RepositoryName, URL: answers.RepositoryURL, GitHub: initGitHubSlug(answers.RepositoryURL), Base: answers.BaseBranch, Checkout: clone}},
		Monitor:                   config.MonitorConfig{PollIntervalText: "1m", StartupWindowText: "24h"},
	}, nil
}

func initGitHubSlug(repositoryURL string) string {
	identity, err := gitx.NormalizeRepositoryIdentity(repositoryURL)
	if err != nil || !strings.HasPrefix(identity, "github.com/") {
		return ""
	}
	return strings.TrimPrefix(identity, "github.com/")
}
