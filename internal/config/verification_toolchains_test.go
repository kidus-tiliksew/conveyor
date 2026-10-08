package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func validToolchain() VerificationToolchain {
	return VerificationToolchain{Server: "https://factory.test", Workspace: "demo", Repository: "repo", SearchPaths: []string{"/opt/homebrew/bin", "/usr/bin"}, Home: "/Users/operator", Settings: map[string]string{"GOPATH": "/Users/operator/go:/srv/go", "GOENV": "off", "GOCACHE": "/srv/cache/go-build"}}
}

// component-verification-runner: an absent section keeps the minimal default; one scoped record is
// selected by server, workspace and repository.
func TestVerificationToolchainScope(t *testing.T) {
	c := &Config{}
	if got, err := c.VerificationToolchainFor("https://factory.test", "demo", "repo"); err != nil || got != nil {
		t.Fatalf("absent section selected %+v %v", got, err)
	}
	c.VerificationToolchains = []VerificationToolchain{validToolchain()}
	got, err := c.VerificationToolchainFor("HTTPS://Factory.test/", "demo", "repo")
	if err != nil || got == nil || got.SearchPaths[0] != "/opt/homebrew/bin" {
		t.Fatalf("scoped record not selected: %+v %v", got, err)
	}
	got.SearchPaths[0], got.Settings["GOENV"] = "/changed", "/changed"
	if c.VerificationToolchains[0].SearchPaths[0] != "/opt/homebrew/bin" || c.VerificationToolchains[0].Settings["GOENV"] != "off" {
		t.Fatal("selected record aliases configuration")
	}
	for _, scope := range [][3]string{{"https://other.test", "demo", "repo"}, {"https://factory.test", "other", "repo"}, {"https://factory.test", "demo", "other"}} {
		if got, err := c.VerificationToolchainFor(scope[0], scope[1], scope[2]); err != nil || got != nil {
			t.Fatalf("foreign scope %v selected %+v %v", scope, got, err)
		}
	}
	duplicate := validToolchain()
	duplicate.Server = "https://FACTORY.test/"
	c.VerificationToolchains = append(c.VerificationToolchains, duplicate)
	if _, err := c.VerificationToolchainFor("https://factory.test", "demo", "repo"); err == nil || !strings.Contains(err.Error(), "duplicate scope") {
		t.Fatalf("ambiguous scope accepted: %v", err)
	}
}

func TestVerificationToolchainValidation(t *testing.T) {
	if err := validToolchain().Validate(); err != nil {
		t.Fatal(err)
	}
	long := "/" + strings.Repeat("a", MaxVerificationToolchainPathBytes)
	many := make([]string, MaxVerificationToolchainSearchPaths+1)
	for i := range many {
		many[i] = "/bin"
	}
	for name, mutate := range map[string]func(*VerificationToolchain){
		"missing server":        func(t *VerificationToolchain) { t.Server = "" },
		"server credentials":    func(t *VerificationToolchain) { t.Server = "https://user:pass@factory.test" },
		"missing repository":    func(t *VerificationToolchain) { t.Repository = "" },
		"no search paths":       func(t *VerificationToolchain) { t.SearchPaths = nil },
		"too many search paths": func(t *VerificationToolchain) { t.SearchPaths = many },
		"relative search path":  func(t *VerificationToolchain) { t.SearchPaths = []string{"bin"} },
		"empty search path":     func(t *VerificationToolchain) { t.SearchPaths = []string{""} },
		"separator in entry": func(t *VerificationToolchain) {
			t.SearchPaths = []string{"/usr/bin" + string(os.PathListSeparator) + "/bin"}
		},
		"variable":             func(t *VerificationToolchain) { t.SearchPaths = []string{"$HOME/go/bin"} },
		"tilde":                func(t *VerificationToolchain) { t.Home = "~/" },
		"control character":    func(t *VerificationToolchain) { t.Home = "/home/op\nerator" },
		"NUL":                  func(t *VerificationToolchain) { t.Home = "/home/\x00" },
		"over-long path":       func(t *VerificationToolchain) { t.SearchPaths = []string{long} },
		"relative home":        func(t *VerificationToolchain) { t.Home = "home" },
		"unknown setting":      func(t *VerificationToolchain) { t.Settings["GH_TOKEN"] = "/x" },
		"loader injection":     func(t *VerificationToolchain) { t.Settings["LD_PRELOAD"] = "/x.so" },
		"PATH setting":         func(t *VerificationToolchain) { t.Settings["PATH"] = "/bin" },
		"empty GOPATH element": func(t *VerificationToolchain) { t.Settings["GOPATH"] = "/go" + string(os.PathListSeparator) },
		"relative GOPATH":      func(t *VerificationToolchain) { t.Settings["GOPATH"] = "go" },
		"GOENV word":           func(t *VerificationToolchain) { t.Settings["GOENV"] = "on" },
		"empty setting":        func(t *VerificationToolchain) { t.Settings["GOROOT"] = "" },
	} {
		t.Run(name, func(t *testing.T) {
			record := validToolchain()
			mutate(&record)
			if err := record.Validate(); err == nil {
				t.Fatal("invalid record accepted")
			}
		})
	}
	records := make([]VerificationToolchain, MaxVerificationToolchains+1)
	for i := range records {
		records[i] = validToolchain()
		records[i].Repository = "repo" + strings.Repeat("x", i)
	}
	if err := validateVerificationToolchains(records); err == nil {
		t.Fatal("unbounded section accepted")
	}
	if err := validateVerificationToolchains(records[:MaxVerificationToolchains]); err != nil {
		t.Fatal(err)
	}
}

// The section is strict at load and never crosses the workspace document or
// any JSON projection.
func TestVerificationToolchainsLoadAndStayLocal(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	example, err := os.ReadFile(filepath.Join("..", "..", "conveyor.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	load := func(suffix string) (*Config, error) {
		path := filepath.Join(t.TempDir(), "conveyor.yaml")
		if err := os.WriteFile(path, append(append([]byte(nil), example...), []byte(suffix)...), 0o600); err != nil {
			t.Fatal(err)
		}
		return Load(path)
	}
	section := "\nverification_toolchains:\n  - server: https://factory.test\n    workspace: demo\n    repository: repo\n    search_paths: [/opt/homebrew/bin, /usr/bin]\n    home: /Users/operator\n    settings:\n      GOPATH: /Users/operator/go\n"
	cfg, err := load(section)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := cfg.VerificationToolchainFor("https://factory.test", "demo", "repo"); err != nil || got == nil || got.Settings["GOPATH"] != "/Users/operator/go" {
		t.Fatalf("loaded record: %+v %v", got, err)
	}
	encoded, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	document, err := json.Marshal(cfg.WorkspaceDocument())
	if err != nil {
		t.Fatal(err)
	}
	for _, data := range [][]byte{encoded, document} {
		if strings.Contains(string(data), "verification_toolchains") || strings.Contains(string(data), "/opt/homebrew/bin") {
			t.Fatalf("toolchain profile escaped the local configuration: %s", data)
		}
	}
	if _, err := load(strings.Replace(section, "home:", "shell_rc:", 1)); err == nil {
		t.Fatal("unknown field accepted")
	}
	if _, err := load(strings.Replace(section, "GOPATH", "LD_LIBRARY_PATH", 1)); err == nil {
		t.Fatal("unknown setting accepted")
	}
	if _, err := load(section + strings.Replace(section, "\nverification_toolchains:\n", "", 1)); err == nil {
		t.Fatal("duplicate scope accepted")
	}
}
