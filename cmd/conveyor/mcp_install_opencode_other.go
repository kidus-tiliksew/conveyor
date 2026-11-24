//go:build !linux

package main

import (
	"fmt"
	"os"
	"os/exec"
)

// openCodeContained refuses, because no containment is proven on this
// platform (component-cli-onboarding invariant 11).
func openCodeContained(string, []string, *os.File) (*exec.Cmd, error) {
	return nil, errOpenCodeContainment
}

func runOpenCodeInit([]string) int {
	fmt.Fprintln(os.Stderr, "error: this internal command runs only inside the OpenCode check")
	return 125
}
