package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/verification"
)

const cliKitManifest = `schema_version: 1
kits:
  - id: sample
    name: Sample
    version: "1"
    path: kits/sample
    governing_pins:
      requirements: [{document_id: req-sample, version: 1}]
    exercises:
      - id: observe
        stages: [verify]
        kind: script
        argv: ["./check.sh"]
        cwd: .
        timeout_seconds: 10
        required_assertions: []
        retry_policy: safe_to_replay
        safety_basis: read-only
        operations: []
`

func kitCLIRepo(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s %v", args, out, err)
		}
	}
	git("init", "-q")
	git("config", "user.name", "Fixture")
	git("config", "user.email", "fixture@example.invalid")
	for _, dir := range []string{".conveyor/kits", "kits/sample"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	manifest := filepath.Join(root, ".conveyor/kits/manifest.yaml")
	if err := os.WriteFile(manifest, []byte(cliKitManifest), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "kits/sample/check.sh"), []byte("#!/bin/sh\nexit 97\n"), 0700); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-qm", "fixture")
	return root, manifest
}
func runKitCLI(t *testing.T, args ...string) (verification.SelectionReceipt, error) {
	t.Helper()
	cmd := kitCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs(append([]string{"validate"}, args...))
	err := cmd.Execute()
	var receipt verification.SelectionReceipt
	if decodeErr := json.Unmarshal(out.Bytes(), &receipt); decodeErr != nil {
		t.Fatalf("receipt: %s %v (command %v)", out.String(), decodeErr, err)
	}
	return receipt, err
}
func TestKitValidateOffline(t *testing.T) {
	root, _ := kitCLIRepo(t)
	t.Setenv("CONVEYOR_SERVER", "http://127.0.0.1:1")
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	r, err := runKitCLI(t, root, "--pin", "requirement:req-sample:1")
	if err != nil || len(r.Kits) != 1 || r.Kits[0].Eligibility != "eligible" || len(r.Kits[0].Digest) != 64 || len(r.SourceRevision) != 40 || !strings.HasPrefix(r.ManifestRevision, "working-tree:sha256:") {
		t.Fatalf("%+v %v", r, err)
	}
	r, err = runKitCLI(t, root, "--stage", "review", "--pin", "requirement:req-sample:1")
	if err != nil || r.Kits[0].Eligibility != "ineligible" {
		t.Fatalf("%+v %v", r, err)
	}
	r, err = runKitCLI(t, root)
	if err != nil || r.Kits[0].Reasons[0].Code != "pin_not_authoritative" {
		t.Fatalf("%+v %v", r, err)
	}
}
func TestKitValidateInvalidReceipt(t *testing.T) {
	root, manifest := kitCLIRepo(t)
	if err := os.WriteFile(manifest, []byte(strings.Replace(cliKitManifest, "retry_policy: safe_to_replay", "retry_policy: always", 1)), 0600); err != nil {
		t.Fatal(err)
	}
	r, err := runKitCLI(t, manifest)
	if err == nil || !r.Invalid() || r.Kits[0].Eligibility != "invalid" {
		t.Fatalf("%+v %v", r, err)
	}
	if err := os.WriteFile(manifest, []byte("kits: ["), 0600); err != nil {
		t.Fatal(err)
	}
	r, err = runKitCLI(t, manifest)
	if err == nil || !r.Invalid() {
		t.Fatalf("%+v %v", r, err)
	}
	if err := os.WriteFile(manifest, []byte(cliKitManifest), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "kits/sample/check.sh"), []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	r, err = runKitCLI(t, manifest)
	if err == nil || !r.Invalid() {
		t.Fatalf("dirty kit: %+v %v", r, err)
	}
}
func TestKitValidatePins(t *testing.T) {
	for _, pin := range []string{"req-sample:1", "unknown:req:1", "requirement::1", "requirement:req:0", "requirement:req:no"} {
		if _, err := parseKitPins([]string{pin}); err == nil {
			t.Fatal(pin)
		}
	}
	if _, err := parseKitPins([]string{"requirement:req:1", "requirement:req:2"}); err == nil {
		t.Fatal("conflicting pins accepted")
	}
	pins, err := parseKitPins([]string{"requirement:req:1", "system_design:component:2"})
	if err != nil || len(pins) != 2 {
		t.Fatal(pins, err)
	}
	cmd := kitCmd()
	cmd.SetArgs([]string{"validate"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("missing path accepted")
	}
}

func TestKitValidateIgnoredFilesAndDraftIdentity(t *testing.T) {
	root, manifest := kitCLIRepo(t)
	first, err := runKitCLI(t, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifest, []byte("# draft annotation\n"+cliKitManifest), 0600); err != nil {
		t.Fatal(err)
	}
	next, err := runKitCLI(t, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if first.ManifestRevision == next.ManifestRevision || first.SourceRevision != next.SourceRevision || first.Kits[0].Digest != next.Kits[0].Digest {
		t.Fatalf("draft/tree identity confused: %+v %+v", first, next)
	}
	if err := os.WriteFile(filepath.Join(root, ".git/info/exclude"), []byte("kits/sample/ignored.sh\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "kits/sample/ignored.sh"), []byte("untracked input"), 0600); err != nil {
		t.Fatal(err)
	}
	receipt, err := runKitCLI(t, manifest)
	if err == nil || !receipt.Invalid() {
		t.Fatalf("ignored file admitted: %+v %v", receipt, err)
	}
}
