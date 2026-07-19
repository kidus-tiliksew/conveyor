package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/pelletier/go-toml/v2"
)

const (
	grokAttachmentURLTemplate  = "${CONVEYOR_ADDR}"
	grokAttachmentAuthTemplate = "Bearer ${CONVEYOR_API_TOKEN}"
)

type grokInspectDocument struct {
	MCPServers []struct {
		Name      string `json:"name"`
		Transport string `json:"transport"`
		Target    string `json:"target"`
		Source    struct {
			Type string `json:"type"`
			Path string `json:"path"`
		} `json:"source"`
	} `json:"mcpServers"`
}

type grokDoctorDocument struct {
	HealthyCount int `json:"healthy_count"`
	FailingCount int `json:"failing_count"`
	Servers      []struct {
		Name      string `json:"name"`
		Transport string `json:"transport"`
		Target    string `json:"target"`
		Source    string `json:"source"`
		Healthy   bool   `json:"healthy"`
	} `json:"servers"`
}

type grokConfigDocument struct {
	MCPServers map[string]struct {
		URL     string            `toml:"url"`
		Headers map[string]string `toml:"headers"`
	} `toml:"mcp_servers"`
}

// validateGrokEnvironmentAttachment proves that the effective Grok
// registration is the intended non-secret config.toml entry and completes a
// real MCP handshake before any model turn (spec §21.29 changes 4-5).
func validateGrokEnvironmentAttachment(ctx context.Context, harness config.Harness, env []string, directory string) error {
	return validateGrokEnvironmentAttachmentWithRunner(ctx, harness, env, directory, runGrokJSON)
}

type grokJSONRunner func(context.Context, string, []string, string, []string, any) error

func validateGrokEnvironmentAttachmentWithRunner(ctx context.Context, harness config.Harness, env []string, directory string, run grokJSONRunner) error {
	if len(harness.Command) == 0 || filepath.Base(harness.Command[0]) != "grok" {
		return fmt.Errorf("environment MCP readiness is currently supported only for the Grok Build harness")
	}
	if err := config.ValidateHarness(harness); err != nil {
		return fmt.Errorf("environment MCP harness definition is invalid")
	}
	address := environmentValue(env, "CONVEYOR_ADDR")
	if address == "" || environmentValue(env, "CONVEYOR_API_TOKEN") == "" ||
		environmentValue(env, "CONVEYOR_SESSION_ID") == "" || environmentValue(env, "CONVEYOR_CLIENT_TOKEN") == "" {
		return fmt.Errorf("environment MCP readiness requires complete child launch identity")
	}

	timeout := harness.ProbeTimeout
	if timeout <= 0 {
		timeout, _ = time.ParseDuration(harness.ProbeTimeoutText)
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var inspected grokInspectDocument
	if err := run(probeCtx, directory, env, harness.Command[0], []string{"inspect", "--json"}, &inspected); err != nil {
		return fmt.Errorf("Grok MCP readiness inspection failed")
	}
	var selected *struct {
		Name      string `json:"name"`
		Transport string `json:"transport"`
		Target    string `json:"target"`
		Source    struct {
			Type string `json:"type"`
			Path string `json:"path"`
		} `json:"source"`
	}
	for i := range inspected.MCPServers {
		if inspected.MCPServers[i].Name == harness.MCPAttachment {
			if selected != nil {
				return fmt.Errorf("Grok MCP readiness found an ambiguous attachment")
			}
			selected = &inspected.MCPServers[i]
		}
	}
	if selected == nil || selected.Transport != "http" || selected.Target != address || selected.Source.Type != "configToml" || selected.Source.Path == "" {
		return fmt.Errorf("Grok MCP readiness could not identify the intended environment-backed registration")
	}
	if err := validateGrokConfigSource(selected.Source.Path, harness.MCPAttachment); err != nil {
		return err
	}

	var doctor grokDoctorDocument
	if err := run(probeCtx, directory, env, harness.Command[0], []string{"mcp", "doctor", harness.MCPAttachment, "--json"}, &doctor); err != nil {
		return fmt.Errorf("Grok MCP readiness doctor failed")
	}
	if doctor.HealthyCount != 1 || doctor.FailingCount != 0 || len(doctor.Servers) != 1 {
		return fmt.Errorf("Grok MCP readiness handshake failed")
	}
	server := doctor.Servers[0]
	if server.Name != harness.MCPAttachment || server.Transport != "http" || server.Target != address || server.Source != "config" || !server.Healthy {
		return fmt.Errorf("Grok MCP readiness did not prove the intended registration")
	}
	return nil
}

func validateGrokConfigSource(path, attachment string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("Grok MCP readiness could not read the intended registration source")
	}
	var document grokConfigDocument
	if err = toml.Unmarshal(data, &document); err != nil {
		return fmt.Errorf("Grok MCP readiness found malformed registration configuration")
	}
	server, ok := document.MCPServers[attachment]
	if !ok || server.URL != grokAttachmentURLTemplate {
		return fmt.Errorf("Grok MCP readiness requires an environment-backed URL")
	}
	authorization := ""
	foundAuthorization := false
	for name, value := range server.Headers {
		if strings.EqualFold(name, "Authorization") {
			if foundAuthorization {
				return fmt.Errorf("Grok MCP readiness found an ambiguous authorization header")
			}
			foundAuthorization = true
			authorization = value
		}
	}
	if authorization != grokAttachmentAuthTemplate {
		return fmt.Errorf("Grok MCP readiness requires an environment-backed authorization header")
	}
	return nil
}

func runGrokJSON(ctx context.Context, directory string, env []string, binary string, args []string, target any) error {
	command := exec.CommandContext(ctx, binary, args...)
	command.Dir = directory
	command.Env = env
	output := boundedBuffer{remaining: 2 << 20}
	command.Stdout = &output
	command.Stderr = io.Discard
	runErr := command.Run()
	if err := json.Unmarshal(output.Bytes(), target); err != nil {
		return err
	}
	return runErr
}

type boundedBuffer struct {
	bytes.Buffer
	remaining int
}

func (b *boundedBuffer) Write(value []byte) (int, error) {
	written := len(value)
	if len(value) > b.remaining {
		value = value[:b.remaining]
	}
	b.remaining -= len(value)
	_, err := b.Buffer.Write(value)
	return written, err
}

func environmentValue(env []string, name string) string {
	prefix := name + "="
	value := ""
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			value = strings.TrimPrefix(entry, prefix)
		}
	}
	return value
}
