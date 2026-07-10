// conveyor-resume-experiment executes the Phase 1 Codex continuity matrix
// and emits the per-harness routing calibration required by spec §20.2.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/resumefidelity"
)

func main() {
	home, err := os.UserHomeDir()
	if err != nil {
		log.Fatal(err)
	}
	config := resumefidelity.Config{}
	flag.StringVar(&config.BaseImage, "base-image", resumefidelity.DefaultBaseImage, "image containing the pinned Codex CLI")
	flag.StringVar(&config.BumpImage, "bump-image", resumefidelity.DefaultBumpImage, "image containing the next minor Codex CLI")
	flag.StringVar(&config.BaseVersion, "base-version", resumefidelity.DefaultBaseVersion, "pinned Codex CLI version")
	flag.StringVar(&config.BumpVersion, "bump-version", resumefidelity.DefaultBumpVersion, "next minor Codex CLI version")
	flag.StringVar(&config.AuthPath, "auth", filepath.Join(home, ".codex", "auth.json"), "host Codex auth.json (only this file is staged)")
	flag.StringVar(&config.OutputDir, "output", filepath.Join("experiments", "resume-fidelity", "results"), "result artifact directory")
	flag.StringVar(&config.WorkRoot, "work-root", "", "work directory (default: temporary)")
	flag.BoolVar(&config.KeepWork, "keep-work", false, "retain temporary session homes after the run")
	flag.DurationVar(&config.CommandTimeout, "command-timeout", 10*time.Minute, "timeout for each seed or probe")
	flag.DurationVar(&config.CrashTimeout, "crash-timeout", 2*time.Minute, "timeout waiting for the crash checkpoint")
	flag.Parse()

	result, err := resumefidelity.Run(context.Background(), config)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("resume-fidelity run %s complete\nJSON: %s\nReport: %s\nmatching-version=%s cross-version=%s\n",
		result.RunID,
		result.JSONArtifact,
		result.MarkdownArtifact,
		result.RoutingDefault.MatchingVersion,
		result.RoutingDefault.CrossVersion,
	)
}
