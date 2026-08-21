package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

const localExecutionConfigName = "conveyor.yaml"

type localExecutionConfigPath struct {
	Path   string
	Source string
}

func defaultLocalExecutionConfigPath() string {
	resolved, err := resolveLocalExecutionConfigPath(nil, "")
	if err == nil {
		return resolved.Path
	}
	return localExecutionConfigName
}

func resolveLocalExecutionConfigPath(cmd *cobra.Command, flagValue string) (localExecutionConfigPath, error) {
	// req-260811-0ee057 REQ-14/AC-14.1-14.3: every executor-side
	// consumer shares one local-only path and never sends setup content to the
	// control plane.
	if cmd != nil && (cmd.Flags().Changed("config") || cmd.InheritedFlags().Changed("config")) {
		return resolvedLocalExecutionConfigPath(flagValue, "flag")
	}
	if value := strings.TrimSpace(os.Getenv("CONVEYOR_CONFIG")); value != "" {
		return resolvedLocalExecutionConfigPath(value, "environment CONVEYOR_CONFIG")
	}
	cwdPath, err := filepath.Abs(localExecutionConfigName)
	if err != nil {
		return localExecutionConfigPath{}, fmt.Errorf("resolve working-directory execution config: %w", err)
	}
	if _, statErr := os.Stat(cwdPath); statErr == nil || !errors.Is(statErr, os.ErrNotExist) {
		return localExecutionConfigPath{Path: cwdPath, Source: "working-directory file"}, nil
	}
	root, err := userConfigDir()
	if err != nil {
		return localExecutionConfigPath{}, fmt.Errorf("resolve user config directory: %w", err)
	}
	return resolvedLocalExecutionConfigPath(filepath.Join(root, "conveyor", localExecutionConfigName), "user default")
}

func resolvedLocalExecutionConfigPath(path, source string) (localExecutionConfigPath, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return localExecutionConfigPath{}, fmt.Errorf("local execution config path selected by %s is empty", source)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return localExecutionConfigPath{}, fmt.Errorf("resolve local execution config path selected by %s: %w", source, err)
	}
	return localExecutionConfigPath{Path: filepath.Clean(absolute), Source: source}, nil
}
