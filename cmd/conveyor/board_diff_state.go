package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const boardDiffStateVersion = 1

type boardDiffTask struct {
	Title       string     `json:"title"`
	State       string     `json:"state"`
	Stage       string     `json:"stage"`
	EnteredAt   *time.Time `json:"entered_at,omitempty"`
	PullRequest int        `json:"pull_request"`
	Decision    string     `json:"decision"`
}

type boardDiffState struct {
	Version   int                      `json:"version"`
	Server    string                   `json:"server"`
	Workspace string                   `json:"workspace"`
	Position  time.Time                `json:"position"`
	SavedAt   time.Time                `json:"saved_at"`
	Recent    map[string]time.Time     `json:"recent"`
	Tasks     map[string]boardDiffTask `json:"tasks"`
}

func boardDiffIdentity(server, workspace string) (string, string, string, error) {
	u, err := url.Parse(server)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", "", "", fmt.Errorf("invalid board server URL")
	}
	if workspace == "" {
		workspace = "default"
	}
	host := strings.ReplaceAll(u.Host, ":", "_")
	if host == "." || host == ".." || workspace == "." || workspace == ".." || strings.ContainsAny(host+workspace, `/\`) {
		return "", "", "", fmt.Errorf("invalid board snapshot identity")
	}
	return strings.TrimRight(server, "/"), workspace, host, nil
}

func boardDiffStatePath(server, workspace string) (string, error) {
	_, workspace, host, err := boardDiffIdentity(server, workspace)
	if err != nil {
		return "", err
	}
	root := os.Getenv("XDG_STATE_HOME")
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		root = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(root, "conveyor", host, workspace, "board.json"), nil
}

// loadBoardDiffState returns a reset reason for absent or unusable snapshots;
// filesystem failures are errors so they cannot silently discard a snapshot.
func loadBoardDiffState(server, workspace string, now time.Time) (boardDiffState, string, error) {
	canonical, workspace, _, err := boardDiffIdentity(server, workspace)
	if err != nil {
		return boardDiffState{}, "", err
	}
	path, err := boardDiffStatePath(server, workspace)
	if err != nil {
		return boardDiffState{}, "", err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return boardDiffState{}, "missing snapshot", nil
	}
	if err != nil {
		return boardDiffState{}, "", fmt.Errorf("read board snapshot: %w", err)
	}
	var state boardDiffState
	if json.Unmarshal(data, &state) != nil {
		return boardDiffState{}, "invalid snapshot", nil
	}
	switch {
	case state.Version != boardDiffStateVersion:
		return boardDiffState{}, "snapshot version changed", nil
	case state.Server != canonical:
		return boardDiffState{}, "server changed", nil
	case state.Workspace != workspace:
		return boardDiffState{}, "workspace changed", nil
	case state.SavedAt.IsZero() || state.Position.IsZero():
		return boardDiffState{}, "invalid snapshot position", nil
	case now.Sub(state.SavedAt) > 7*24*time.Hour:
		return boardDiffState{}, "snapshot older than seven days", nil
	}
	return state, "", nil
}

var boardDiffStateWriter = struct {
	write  func(*os.File, []byte) (int, error)
	sync   func(*os.File) error
	rename func(string, string) error
}{func(f *os.File, b []byte) (int, error) { return f.Write(b) }, func(f *os.File) error { return f.Sync() }, os.Rename}

func saveBoardDiffState(server, workspace string, state boardDiffState) (err error) {
	canonical, workspace, _, err := boardDiffIdentity(server, workspace)
	if err != nil {
		return err
	}
	path, err := boardDiffStatePath(server, workspace)
	if err != nil {
		return err
	}
	state.Version, state.Server, state.Workspace = boardDiffStateVersion, canonical, workspace
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode board snapshot: %w", err)
	}
	data = append(data, '\n')
	dir := filepath.Dir(path)
	if err = os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create board snapshot directory: %w", err)
	}
	if err = os.Chmod(dir, 0700); err != nil {
		return fmt.Errorf("secure board snapshot directory: %w", err)
	}
	temp, err := os.CreateTemp(dir, ".board-*.tmp")
	if err != nil {
		return fmt.Errorf("create board snapshot: %w", err)
	}
	tempPath := temp.Name()
	closed := false
	defer func() {
		if !closed {
			_ = temp.Close()
		}
		if err != nil {
			_ = os.Remove(tempPath)
		}
	}()
	n, err := boardDiffStateWriter.write(temp, data)
	if err != nil {
		return fmt.Errorf("write board snapshot: %w", err)
	}
	if n != len(data) {
		return io.ErrShortWrite
	}
	if err = boardDiffStateWriter.sync(temp); err != nil {
		return fmt.Errorf("sync board snapshot: %w", err)
	}
	closed = true
	if err = temp.Close(); err != nil {
		return fmt.Errorf("close board snapshot: %w", err)
	}
	if err = boardDiffStateWriter.rename(tempPath, path); err != nil {
		return fmt.Errorf("replace board snapshot: %w", err)
	}
	return nil
}
