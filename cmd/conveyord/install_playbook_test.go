package main

import (
	"flag"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/playbookcheck"
)

// conveyordSurface is the daemon's command surface built from the same
// definitions main and runServiceVerb use.
func conveyordSurface() playbookcheck.FlagSurface {
	daemon := flag.NewFlagSet("conveyord", flag.ContinueOnError)
	registerDaemonFlags(daemon)
	verbs := map[string]*flag.FlagSet{}
	for _, verb := range daemonVersionVerbs {
		verbs[verb] = flag.NewFlagSet("conveyord "+verb, flag.ContinueOnError)
	}
	for _, verb := range daemonServiceVerbs {
		verbs[verb], _, _ = newDaemonServiceFlagSet(verb)
	}
	return playbookcheck.FlagSurface{Name: "conveyord", Default: daemon, Verbs: verbs}
}

// Every conveyord verb and flag the setup playbook directs exists in this
// binary (req-agent-guided-setup AC-5.3).
func TestInstallPlaybookConveyordCommandsExist(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test source path")
	}
	const source = "docs/playbooks/conveyor-install.md"
	content, err := os.ReadFile(filepath.Join(filepath.Dir(currentFile), "..", "..", filepath.FromSlash(source)))
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, invocation := range playbookcheck.Extract(content) {
		if invocation.Program != "conveyord" {
			continue
		}
		checked++
		if err := playbookcheck.CheckFlagSurface(conveyordSurface(), invocation.Args); err != nil {
			t.Errorf("%s %s: %v", source, invocation, err)
		}
	}
	if checked < 5 {
		t.Fatalf("checked %d conveyord invocations in %s, want at least 5", checked, source)
	}
}

// A misspelled verb or flag in a playbook fixture fails the same check.
func TestInstallPlaybookConveyordCheckRefusesMisspellings(t *testing.T) {
	fixture := "```sh\n" +
		"conveyord install --config conveyor.yaml --env-file .env\n" +
		"conveyord instal --config conveyor.yaml\n" +
		"conveyord status --confg conveyor.yaml\n" +
		"conveyord -adr 0.0.0.0:8080\n" +
		"```\n"
	invocations := playbookcheck.Extract([]byte(fixture))
	if len(invocations) != 4 {
		t.Fatalf("fixture invocations = %d, want 4", len(invocations))
	}
	if err := playbookcheck.CheckFlagSurface(conveyordSurface(), invocations[0].Args); err != nil {
		t.Errorf("%s: %v", invocations[0], err)
	}
	for index, want := range map[int]string{
		1: `unknown command or argument "instal" for "conveyord"`,
		2: `unknown flag "--confg" for "conveyord status"`,
		3: `unknown flag "-adr" for "conveyord"`,
	} {
		err := playbookcheck.CheckFlagSurface(conveyordSurface(), invocations[index].Args)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: error = %v, want %s", invocations[index], err, want)
		}
	}
}
