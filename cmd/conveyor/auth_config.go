package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

const localAuthConfigComment = "Credentials are stored in plaintext with the same user trust model as gh and kubeconfig; OS keychain integration may be added in the future."

type localServerConfig struct {
	Token     string `json:"token,omitempty"`
	Workspace string `json:"workspace,omitempty"`
}

type localAuthConfig struct {
	Comment string                       `json:"_comment"`
	Servers map[string]localServerConfig `json:"servers"`
}

type resolvedValue struct {
	Value  string
	Source string
}

type resolvedClientConfig struct {
	Server    resolvedValue
	Token     resolvedValue
	Workspace resolvedValue
}

var userConfigDir = os.UserConfigDir

func localAuthConfigPath() (string, error) {
	root, err := userConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolve user config directory: %w", err)
	}
	return filepath.Join(root, "conveyor", "credentials.json"), nil
}

func normalizeServerURL(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", errors.New("server URL is required")
	}
	u, err := url.Parse(value)
	if err != nil || u.IsAbs() == false || u.Host == "" || u.Opaque != "" {
		return "", errors.New("server must be an absolute http or https URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", errors.New("server must use http or https")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("server URL must not contain credentials, a query, or a fragment")
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	u.Path = strings.TrimRight(u.Path, "/")
	if strings.HasSuffix(u.Path, "/mcp") {
		u.Path = strings.TrimSuffix(u.Path, "/mcp")
	}
	u.RawPath = ""
	return u.String(), nil
}

func loadLocalAuthConfig() (localAuthConfig, error) {
	config := localAuthConfig{Comment: localAuthConfigComment, Servers: map[string]localServerConfig{}}
	path, err := localAuthConfigPath()
	if err != nil {
		return config, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return config, nil
	}
	if err != nil {
		return config, fmt.Errorf("read %s: %w", path, err)
	}
	if err := json.Unmarshal(data, &config); err != nil {
		return config, fmt.Errorf("parse %s: %w", path, err)
	}
	if config.Servers == nil {
		config.Servers = map[string]localServerConfig{}
	}
	config.Comment = localAuthConfigComment
	return config, nil
}

func saveLocalAuthConfig(config localAuthConfig) error {
	path, err := localAuthConfigPath()
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create credential directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("secure credential directory: %w", err)
	}
	config.Comment = localAuthConfigComment
	if config.Servers == nil {
		config.Servers = map[string]localServerConfig{}
	}
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	temp, err := os.CreateTemp(dir, ".credentials-*.tmp")
	if err != nil {
		return fmt.Errorf("create credential update: %w", err)
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("publish credential update: %w", err)
	}
	return os.Chmod(path, 0o600)
}

func resolveClientConfig() (resolvedClientConfig, error) {
	server := resolvedValue{Value: "http://localhost:8080", Source: "default"}
	if serverFlagExplicit || (strings.TrimSpace(serverFlag) != "" && strings.TrimSpace(os.Getenv("CONVEYOR_ADDR")) == "") {
		server = resolvedValue{Value: serverFlag, Source: "flag"}
	} else if value := strings.TrimSpace(os.Getenv("CONVEYOR_ADDR")); value != "" {
		server = resolvedValue{Value: value, Source: "environment"}
	}
	canonical, err := normalizeServerURL(server.Value)
	if err != nil {
		return resolvedClientConfig{}, fmt.Errorf("resolve server: %w", err)
	}
	server.Value = canonical

	config, err := loadLocalAuthConfig()
	if err != nil {
		return resolvedClientConfig{}, err
	}
	stored := config.Servers[canonical]
	resolved := resolvedClientConfig{Server: server}
	if value := strings.TrimSpace(os.Getenv("CONVEYOR_API_TOKEN")); value != "" {
		resolved.Token = resolvedValue{Value: value, Source: "environment"}
	} else if stored.Token != "" {
		resolved.Token = resolvedValue{Value: stored.Token, Source: "stored file"}
	}
	if workspaceFlagExplicit || (strings.TrimSpace(workspaceFlag) != "" && strings.TrimSpace(os.Getenv("CONVEYOR_WORKSPACE")) == "") {
		resolved.Workspace = resolvedValue{Value: strings.TrimSpace(workspaceFlag), Source: "flag"}
	} else if value := strings.TrimSpace(os.Getenv("CONVEYOR_WORKSPACE")); value != "" {
		resolved.Workspace = resolvedValue{Value: value, Source: "environment"}
	} else if stored.Workspace != "" {
		resolved.Workspace = resolvedValue{Value: stored.Workspace, Source: "stored file"}
	} else {
		resolved.Workspace = resolvedValue{Source: "singleton fallback"}
	}
	return resolved, nil
}

func updateLocalServerConfig(server string, update func(*localServerConfig)) error {
	canonical, err := normalizeServerURL(server)
	if err != nil {
		return err
	}
	config, err := loadLocalAuthConfig()
	if err != nil {
		return err
	}
	entry := config.Servers[canonical]
	update(&entry)
	if entry.Token == "" && entry.Workspace == "" {
		delete(config.Servers, canonical)
	} else {
		config.Servers[canonical] = entry
	}
	return saveLocalAuthConfig(config)
}
