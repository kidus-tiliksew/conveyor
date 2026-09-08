package config

import (
	"encoding/json"
	"gopkg.in/yaml.v3"
	"testing"
)

func TestRepositoryInstallWriteAndStoredDefaults(t *testing.T) {
	for _, data := range []string{`{"name":"repo","url":"https://example.test/repo","base":"main"}`, `{"name":"repo","url":"https://example.test/repo","base":"main","install_conveyor":false}`, `{"name":"repo","url":"https://example.test/repo","base":"main","install_conveyor":true}`} {
		var repo Repo
		if err := json.Unmarshal([]byte(data), &repo); err != nil {
			t.Fatal(err)
		}
		cfg := &Config{Workspace: "demo", Repos: []Repo{repo}}
		stored, err := MarshalPolicyDocument(cfg)
		if err != nil {
			t.Fatal(err)
		}
		var doc WorkspaceDocument
		if err = yaml.Unmarshal(stored, &doc); err != nil {
			t.Fatal(err)
		}
		if doc.Repos[0].InstallConveyor == nil || doc.Repos[0].InstallEnabled() != repo.InstallEnabled() {
			t.Fatalf("write default lost: %s", stored)
		}
	}
	var legacy WorkspaceDocument
	if err := yaml.Unmarshal([]byte("repos:\n  - name: repo\n    url: https://example.test/repo\n    base: main\n"), &legacy); err != nil {
		t.Fatal(err)
	}
	StoredRepositoryDefaults(legacy.Repos)
	if legacy.Repos[0].InstallConveyor == nil || legacy.Repos[0].InstallEnabled() {
		t.Fatal("legacy read enabled installation")
	}
}
