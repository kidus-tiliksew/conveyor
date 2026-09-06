package storetest

import (
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/config"
)

func TestFactorySkipValidation(t *testing.T) {
	newFixture := func(*testing.T, []config.Repo) Fixture { panic("must not construct fixture") }
	for _, tc := range []struct {
		name      string
		factory   Factory
		wantError bool
	}{
		{"experimental declared", Factory{New: newFixture, Skip: []string{"WorkspaceControl"}}, false},
		{"unknown suite", Factory{New: newFixture, Skip: []string{"Typo"}}, true},
		{"production cannot skip", Factory{New: newFixture, ProductionCapable: true, Capabilities: Capabilities{true, true, true}, Skip: []string{"WorkspaceControl"}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.factory.validate(); (err != nil) != tc.wantError {
				t.Fatalf("validate: %v", err)
			}
		})
	}
}

func TestRunAllHonorsDeclaredExperimentalSkips(t *testing.T) {
	names := make([]string, 0, len(suiteMethods))
	for name := range suiteMethods {
		names = append(names, name)
	}
	RunAll(t, Factory{Skip: names, New: func(*testing.T, []config.Repo) Fixture {
		t.Fatal("skipped suite constructed a fixture")
		return Fixture{}
	}})
}
