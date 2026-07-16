// Package envfile loads local development environment variables without
// making the deployment binaries depend on a dotenv library.
package envfile

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"unicode"
)

// LoadDefault loads CONVEYOR_ENV_FILE, or .env when no override is set.
// A missing default .env is allowed. Values already present in the process
// environment take precedence over file values.
func LoadDefault() error {
	path := os.Getenv("CONVEYOR_ENV_FILE")
	explicit := path != ""
	if path == "" {
		path = ".env"
	}
	if err := Load(path); err != nil {
		if !explicit && os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return nil
}

// Load reads shell-compatible KEY=value entries from path. Blank lines,
// comments, and an optional export prefix are accepted.
func Load(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for lineNumber := 1; scanner.Scan(); lineNumber++ {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		name, value, ok := strings.Cut(line, "=")
		name = strings.TrimSpace(name)
		if !ok || !validName(name) {
			return fmt.Errorf("parse %s:%d: expected KEY=value", path, lineNumber)
		}
		value, err = parseValue(strings.TrimSpace(value))
		if err != nil {
			return fmt.Errorf("parse %s:%d: %w", path, lineNumber, err)
		}
		if _, exists := os.LookupEnv(name); exists {
			continue
		}
		if err := os.Setenv(name, value); err != nil {
			return fmt.Errorf("set %s from %s:%d: %w", name, path, lineNumber, err)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	return nil
}

func validName(name string) bool {
	for i, r := range name {
		if r == '_' || unicode.IsLetter(r) || i > 0 && unicode.IsDigit(r) {
			continue
		}
		return false
	}
	return name != ""
}

func parseValue(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	if value[0] == '\'' {
		if len(value) < 2 || value[len(value)-1] != '\'' {
			return "", fmt.Errorf("unterminated single-quoted value")
		}
		return value[1 : len(value)-1], nil
	}
	if value[0] == '"' {
		if len(value) < 2 || value[len(value)-1] != '"' {
			return "", fmt.Errorf("unterminated double-quoted value")
		}
		unquoted, err := strconv.Unquote(value)
		if err != nil {
			return "", fmt.Errorf("invalid double-quoted value: %w", err)
		}
		return unquoted, nil
	}
	return value, nil
}
