package main

import (
	"reflect"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/config"
)

func TestExpandHarnessUsesWholeElementSubstitutionAndOptionalModelArgs(t *testing.T) {
	harness := config.Harness{Command: []string{"codex", "exec", "{prompt}", "--config", "{mcp_config}"}, ModelArgs: []string{"--model", "{model}"}}
	want := []string{"codex", "exec", "do work", "--config", "/tmp/mcp.json", "--model", "gpt"}
	if got := expandHarness(harness, "gpt", "do work", "/tmp/mcp.json"); !reflect.DeepEqual(got, want) {
		t.Fatalf("argv=%q want=%q", got, want)
	}
	want = want[:5]
	if got := expandHarness(harness, "", "do work", "/tmp/mcp.json"); !reflect.DeepEqual(got, want) {
		t.Fatalf("without model argv=%q want=%q", got, want)
	}
}
