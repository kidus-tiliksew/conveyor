package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Fake misplaced values. A secret stays the same text in every form, so each
// fixture holds redactionFixtureMark and the check looks for that mark.
const (
	redactionFixtureMark = "FAKE0123456789"
	// misplacedPath breaks the repo name rule.
	misplacedPath = "sk-live/" + redactionFixtureMark
	// misplacedSegment satisfies the repo name rule, so it reaches the duplicate
	// and unknown-name rules.
	misplacedSegment = "sk-live-" + redactionFixtureMark
)

// redactionProblem names what is wrong with a refusal, or returns "" for a
// refusal that holds the wanted text and no fixture. Its result never echoes the
// message, because the message may hold the fixture.
func redactionProblem(err error, want ...string) string {
	if err == nil {
		return "no refusal"
	}
	if strings.Contains(err.Error(), redactionFixtureMark) {
		return "refusal holds the misplaced value"
	}
	for _, text := range want {
		if !strings.Contains(err.Error(), text) {
			return "refusal lacks " + text
		}
	}
	return ""
}

// The scalar forms a misplaced value can take. Tagged and aliased scalars decode
// successfully, so they reach the validators too. The alias form needs an anchor
// on a key the document's path accepts without judging its value.
var misplacedScalarForms = []struct {
	name                                  string
	scalar                                func(value string) string
	deploymentPrologue, workspacePrologue func(value string) string
}{
	{
		name:               "plain",
		scalar:             func(value string) string { return fmt.Sprintf("%q", value) },
		deploymentPrologue: func(string) string { return "" },
		workspacePrologue:  func(string) string { return "" },
	},
	{
		name:               "tagged",
		scalar:             func(value string) string { return fmt.Sprintf("!!str %q", value) },
		deploymentPrologue: func(string) string { return "" },
		workspacePrologue:  func(string) string { return "" },
	},
	{
		name:               "aliased",
		scalar:             func(string) string { return "*misplaced" },
		deploymentPrologue: func(value string) string { return fmt.Sprintf("pack_dir: &misplaced %q\n", value) },
		workspacePrologue: func(value string) string {
			return fmt.Sprintf("monitor: {repositories: [&misplaced %q]}\n", value)
		},
	},
}

const (
	redactionDatabase = "database: {backend: memory}\n"
	redactionTriage   = "execution_settings: {control_plane: {triage: {model: fixture-model}}}\n"
)

// redactionSite is one validator refusal in item 1 of the task.
type redactionSite struct {
	name string
	// value is the fixture that breaks the rule.
	value string
	// deployment renders a deployment document that reaches the site. A nil
	// workspace means the workspace-document path does not reach the site, which
	// the test proves.
	deployment, workspace func(scalar string) string
	// deploymentControl and workspaceControl state the same document with a
	// value the rule accepts, and each must load.
	deploymentControl, workspaceControl string
	want                                []string
}

func redactionSites() []redactionSite {
	const repoRule = "name must use only ASCII letters, digits, '.', '_', or '-' and must not be '.' or '..'"
	const effortRule = "must be minimal, low, medium, or high"
	planning := func(body string) string {
		return redactionDatabase + "execution_settings: {control_plane: {triage: {model: fixture-model}, planning: {" + body + "}}}\n"
	}
	return []redactionSite{
		{
			name:  "deployment.go triage effort",
			value: misplacedSegment,
			deployment: func(scalar string) string {
				return redactionDatabase + "execution_settings: {control_plane: {triage: {model: fixture-model, effort: " + scalar + "}}}\n"
			},
			deploymentControl: redactionDatabase + "execution_settings: {control_plane: {triage: {model: fixture-model, effort: low}}}\n",
			want:              []string{"execution_settings.control_plane.triage.effort", effortRule},
		},
		{
			name:              "config.go planning effort",
			value:             misplacedSegment,
			deployment:        func(scalar string) string { return planning("effort: " + scalar) },
			deploymentControl: planning("effort: low"),
			want:              []string{"execution_settings.control_plane.planning.effort", effortRule},
		},
		{
			name:  "config.go planning model",
			value: misplacedSegment,
			deployment: func(scalar string) string {
				return planning("model: "+scalar) + "planning_models: [fixture-model]\n"
			},
			deploymentControl: planning("model: fixture-model") + "planning_models: [fixture-model]\n",
			want:              []string{"execution_settings.control_plane.planning.model", "must be included in planning_models"},
		},
		{
			name:  "config.go planning_models duplicate",
			value: misplacedSegment,
			deployment: func(scalar string) string {
				return redactionDatabase + redactionTriage + "planning_models: [fixture-model, " + scalar + ", " + scalar + "]\n"
			},
			deploymentControl: redactionDatabase + redactionTriage + "planning_models: [fixture-model, other-model]\n",
			want:              []string{"planning_models[2] duplicates planning_models[1]"},
		},
		{
			// config.go normalizePolicy holds the workspace site and
			// finishNormalization holds the deployment site.
			name:  "config.go repo name invalid",
			value: misplacedPath,
			deployment: func(scalar string) string {
				return redactionDatabase + redactionTriage + "repos: [{name: fine, url: https://example.test/a}, {name: " + scalar + ", url: https://example.test/b}]\n"
			},
			workspace: func(scalar string) string {
				return "repos: [{name: fine, url: https://example.test/a}, {name: " + scalar + ", url: https://example.test/b}]\n"
			},
			deploymentControl: redactionDatabase + redactionTriage + "repos: [{name: fine, url: https://example.test/a}, {name: other, url: https://example.test/b}]\n",
			workspaceControl:  "repos: [{name: fine, url: https://example.test/a}, {name: other, url: https://example.test/b}]\n",
			want:              []string{"repo 1: " + repoRule},
		},
		{
			// As above: the workspace site and the deployment site.
			name:  "config.go repo name duplicate",
			value: misplacedSegment,
			deployment: func(scalar string) string {
				return redactionDatabase + redactionTriage + "repos: [{name: " + scalar + ", url: https://example.test/a}, {name: fine, url: https://example.test/b}, {name: " + scalar + ", url: https://example.test/c}]\n"
			},
			workspace: func(scalar string) string {
				return "repos: [{name: " + scalar + ", url: https://example.test/a}, {name: fine, url: https://example.test/b}, {name: " + scalar + ", url: https://example.test/c}]\n"
			},
			deploymentControl: redactionDatabase + redactionTriage + "repos: [{name: one, url: https://example.test/a}, {name: fine, url: https://example.test/b}, {name: two, url: https://example.test/c}]\n",
			workspaceControl:  "repos: [{name: one, url: https://example.test/a}, {name: fine, url: https://example.test/b}, {name: two, url: https://example.test/c}]\n",
			want:              []string{"repo 2: name duplicates repo 0"},
		},
		{
			name:  "config.go monitor.repositories unknown",
			value: misplacedSegment,
			deployment: func(scalar string) string {
				return redactionDatabase + redactionTriage + "repos: [{name: fine, url: https://example.test/a}]\nmonitor: {repositories: [fine, " + scalar + "]}\n"
			},
			deploymentControl: redactionDatabase + redactionTriage + "repos: [{name: fine, url: https://example.test/a}]\nmonitor: {repositories: [fine]}\n",
			want:              []string{"monitor.repositories[1] names an unknown repo"},
		},
		{
			name:  "config.go monitor.repositories duplicate",
			value: misplacedSegment,
			deployment: func(scalar string) string {
				return redactionDatabase + redactionTriage + "repos: [{name: " + scalar + ", url: https://example.test/a}]\nmonitor: {repositories: [" + scalar + ", " + scalar + "]}\n"
			},
			deploymentControl: redactionDatabase + redactionTriage + "repos: [{name: fine, url: https://example.test/a}]\nmonitor: {repositories: [fine]}\n",
			want:              []string{"monitor.repositories[1] duplicates monitor.repositories[0]"},
		},
	}
}

func redactionWorkspaceDeployment(t *testing.T) *Config {
	t.Helper()
	deployment, err := ParseDeployment([]byte("workspace: demo\n"+redactionDatabase+redactionTriage), "deployment.yaml", nil)
	if err != nil {
		t.Fatal(err)
	}
	return deployment
}

// Every validator refusal in item 1 keeps its field path and rule and loses the
// value on each path that reaches it, in each scalar form. Each row's control
// loads the same document with an accepted value, so the value is what the
// refusal judged (req-security-boundaries REQ-2; component-runtime invariant 17).
func TestValidatorRefusalsHoldNoDocumentValue(t *testing.T) {
	for _, site := range redactionSites() {
		t.Run(site.name, func(t *testing.T) {
			for _, form := range misplacedScalarForms {
				t.Run(form.name, func(t *testing.T) {
					document := form.deploymentPrologue(site.value) + site.deployment(form.scalar(site.value))
					if !strings.Contains(document, redactionFixtureMark) {
						t.Fatal("deployment fixture holds no misplaced value")
					}
					_, err := ParseDeployment([]byte(document), "deployment.yaml", nil)
					if problem := redactionProblem(err, site.want...); problem != "" {
						t.Errorf("ParseDeployment: %s", problem)
					}
					if site.workspace == nil {
						return
					}
					workspaceDocument := form.workspacePrologue(site.value) + site.workspace(form.scalar(site.value))
					_, err = ParseWorkspaceDocument([]byte(workspaceDocument), redactionWorkspaceDeployment(t), "workspace config API")
					if problem := redactionProblem(err, site.want...); problem != "" {
						t.Errorf("ParseWorkspaceDocument: %s", problem)
					}
				})
			}
			t.Run("control", func(t *testing.T) {
				if _, err := ParseDeployment([]byte(site.deploymentControl), "deployment.yaml", nil); err != nil {
					t.Errorf("accepted deployment value was refused: %v", err)
				}
				if site.workspace != nil {
					if _, err := ParseWorkspaceDocument([]byte(site.workspaceControl), redactionWorkspaceDeployment(t), "workspace config API"); err != nil {
						t.Errorf("accepted workspace value was refused: %v", err)
					}
				}
			})
			if site.workspace == nil {
				// The workspace-document path decodes these keys and judges none
				// of them, so this rule's refusal never reaches its response.
				t.Run("workspace path does not reach the site", func(t *testing.T) {
					document := strings.ReplaceAll(site.deployment(fmt.Sprintf("%q", site.value)), redactionDatabase, "")
					_, err := ParseWorkspaceDocument([]byte(document), redactionWorkspaceDeployment(t), "workspace config API")
					if err == nil {
						return
					}
					if strings.Contains(err.Error(), redactionFixtureMark) {
						t.Fatal("workspace refusal holds the misplaced value")
					}
					if strings.Contains(err.Error(), site.want[len(site.want)-1]) {
						t.Fatal("workspace path reached the site")
					}
				})
			}
		})
	}
}

// The check itself fails on the former message shape, on a missing rule, and on
// an accepted value, so a green table is evidence.
func TestRedactionProblemDetectsEachFailure(t *testing.T) {
	const field, rule = "execution_settings.control_plane.triage.effort", "must be minimal, low, medium, or high"
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"former message", fmt.Errorf("%s %q %s", field, misplacedSegment, rule), "refusal holds the misplaced value"},
		{"no rule", errors.New(field), "refusal lacks " + rule},
		{"no field", errors.New(rule), "refusal lacks " + field},
		{"no refusal", nil, "no refusal"},
		{"redacted", fmt.Errorf("%s %s", field, rule), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := redactionProblem(tc.err, field, rule); got != tc.want {
				t.Errorf("redactionProblem = %q, want %q", got, tc.want)
			}
		})
	}
}

// Item 2 of the task found these other sites. Each loses the value and keeps
// its field path and rule.
func TestOtherConfigurationRefusalsHoldNoDocumentValue(t *testing.T) {
	const toolchain = "{server: https://factory.test, workspace: demo, repository: repo, search_paths: [/usr/bin]"
	grant := func(body string) string {
		return "kit_permissions: [{server: https://factory.test, workspace: demo, repository: repo, binding: kit, actions: [" + body + "]}]\n"
	}
	for _, tc := range []struct {
		name, document string
		want           []string
	}{
		{
			name:     "verification_toolchains unknown setting",
			document: "verification_toolchains: [" + toolchain + ", settings: {" + misplacedSegment + ": /usr/bin}}]\n",
			want:     []string{"verification_toolchains: unknown setting (allowed: "},
		},
		{
			name: "verification_toolchains duplicate scope",
			document: "verification_toolchains: [" +
				"{server: https://factory.test, workspace: " + misplacedSegment + ", repository: " + misplacedSegment + ", search_paths: [/usr/bin]}," +
				"{server: https://factory.test, workspace: " + misplacedSegment + ", repository: " + misplacedSegment + ", search_paths: [/usr/bin]}]\n",
			want: []string{"verification_toolchains[1]: duplicate scope of verification_toolchains[0]"},
		},
		{
			name:     "kit_permissions unknown action",
			document: grant("{kind: " + misplacedSegment + ", binding: kit}"),
			want:     []string{"kit_permissions: permission 0: unknown action"},
		},
		{
			name:     "kit_permissions invalid binding",
			document: grant("{kind: " + misplacedSegment + ", binding: 'not/a/binding'}"),
			want:     []string{"kit_permissions: permission 0: invalid binding"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(tc.document, redactionFixtureMark) {
				t.Fatal("fixture holds no misplaced value")
			}
			_, err := ParseDeployment([]byte(redactionDatabase+redactionTriage+tc.document), "deployment.yaml", nil)
			if problem := redactionProblem(err, tc.want...); problem != "" {
				t.Errorf("ParseDeployment: %s", problem)
			}
			path := filepath.Join(t.TempDir(), "conveyor.yaml")
			if err := os.WriteFile(path, []byte(tc.document), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err = LoadClient(path, nil)
			if problem := redactionProblem(err, tc.want...); problem != "" {
				t.Errorf("LoadClient: %s", problem)
			}
		})
	}

	t.Run("pack_dir holds only whitespace", func(t *testing.T) {
		const blank = "   \t  "
		_, err := ParseDeployment([]byte(fmt.Sprintf("pack_dir: %q\n", blank)+redactionDatabase+redactionTriage), "deployment.yaml", nil)
		if err == nil || err.Error() != "pack_dir override is empty" {
			t.Fatalf("pack_dir refusal = %v", err)
		}
	})

	t.Run("worktree_location", func(t *testing.T) {
		for _, tc := range []struct{ name, document, want string }{
			{"dir", "worktree_location: {mode: repository, dir: " + misplacedPath + "}\n", "worktree_location.dir must be one relative path component"},
			{"mode", "worktree_location: {mode: " + misplacedSegment + "}\n", "worktree_location.mode must be repository or machine"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "conveyor.yaml")
				if err := os.WriteFile(path, []byte(tc.document), 0o600); err != nil {
					t.Fatal(err)
				}
				_, err := LoadClient(path, nil)
				if problem := redactionProblem(err, tc.want); problem != "" {
					t.Error(problem)
				}
			})
		}
		_, err := ParseWorktreeLocation(misplacedSegment)
		if problem := redactionProblem(err, "worktree location must be repository"); problem != "" {
			t.Errorf("ParseWorktreeLocation: %s", problem)
		}
	})

	t.Run("kit_permissions missing grant", func(t *testing.T) {
		_, err := (&Config{}).KitActions("https://"+misplacedSegment+".test", misplacedSegment, misplacedSegment)
		if problem := redactionProblem(err, "missing local kit_permissions grant"); problem != "" {
			t.Error(problem)
		}
	})

	t.Run("JSON decoder text", func(t *testing.T) {
		// encoding/json prints a number literal that does not fit the field.
		const number = "12345.678"
		var recovery RecoveryPolicy
		err := json.Unmarshal([]byte(`{"automatic_recoveries":`+number+`}`), &recovery)
		if err == nil || strings.Contains(err.Error(), number) || !strings.Contains(err.Error(), "recovery must hold") {
			t.Error("recovery refusal holds the number or lacks its rule")
		}
		var contract PolicyContract
		err = json.Unmarshal([]byte(`{"stage_timeouts":{},"max_bounces":`+number+`}`), &contract)
		if err == nil || strings.Contains(err.Error(), number) || !strings.Contains(err.Error(), "policy contract") {
			t.Error("policy contract refusal holds the number or lacks its rule")
		}
		err = json.Unmarshal([]byte(`{"max_bounces":`+number+`}`), &contract)
		if err == nil || strings.Contains(err.Error(), number) || !strings.Contains(err.Error(), "policy contract") {
			t.Error("legacy policy contract refusal holds the number or lacks its rule")
		}
	})
}

// The execution merge walk formats an anchor name and a key name. Both pass
// through redactYAMLError before any loader returns them, so these sites stay.
func TestExecutionMergeWalkTextReachesNoLoaderError(t *testing.T) {
	for _, tc := range []struct{ name, document string }{
		{"anchor name", "execution: &" + misplacedSegment + " {<<: *" + misplacedSegment + "}\n"},
		{"unknown key", "execution: {" + misplacedSegment + ": 1}\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseDeployment([]byte(redactionDatabase+redactionTriage+tc.document), "deployment.yaml", nil)
			if problem := redactionProblem(err); problem != "" {
				t.Error(problem)
			}
		})
	}
}

// The deployment loader logs removed route keys by name. A stage is a free map
// key, so an unknown stage prints as a label and a known stage keeps its name
// (component-runtime invariant 13).
func TestRetiredRouteLogNamesNoUnknownStage(t *testing.T) {
	document := redactionDatabase + redactionTriage + "routing: {stages: {" + misplacedSegment + ": {harness: fixture}, review: {harness: fixture}}}\n"
	var lines []string
	logf := func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }
	if _, err := ParseDeployment([]byte(document), "deployment.yaml", logf); err != nil {
		if strings.Contains(err.Error(), redactionFixtureMark) {
			t.Fatal("refusal holds the misplaced value")
		}
		t.Fatalf("deployment with retired route keys was refused: %v", err)
	}
	if len(lines) != 1 {
		t.Fatalf("log has %d lines, want 1", len(lines))
	}
	if strings.Contains(lines[0], redactionFixtureMark) {
		t.Fatal("log line holds the misplaced stage")
	}
	for _, want := range []string{"routing.stages.<unrecognized>.harness", "routing.stages.review.harness"} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("log line lacks %s: %s", want, lines[0])
		}
	}
}
