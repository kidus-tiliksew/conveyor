package config

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// Worktree location modes recorded by the local execution document
// (component-harness-execution; component-git-delivery).
const (
	WorktreeLocationRepository   = "repository"
	WorktreeLocationMachine      = "machine"
	DefaultRepositoryWorktreeDir = "_worktrees"
)

// WorktreeLocation records where new task worktrees are created. Repository
// mode places them under <primary checkout>/<Dir>; machine mode places them
// under the absolute Root. The location is client-local and never crosses the
// workspace API boundary (req-execution-configuration REQ-9).
type WorktreeLocation struct {
	Mode string `yaml:"mode"`
	Dir  string `yaml:"dir,omitempty"`
	Root string `yaml:"root,omitempty"`
}

// ParseWorktreeLocation reads the one value syntax shared by flags,
// `conveyor config set`, and messages: repository, repository:<dir>,
// machine, or machine:<root>. The result is not yet home-expanded.
func ParseWorktreeLocation(value string) (WorktreeLocation, error) {
	mode, detail, _ := strings.Cut(strings.TrimSpace(value), ":")
	switch mode {
	case WorktreeLocationRepository:
		return WorktreeLocation{Mode: mode, Dir: strings.TrimSpace(detail)}, nil
	case WorktreeLocationMachine:
		return WorktreeLocation{Mode: mode, Root: strings.TrimSpace(detail)}, nil
	}
	return WorktreeLocation{}, fmt.Errorf("worktree location %q must be repository, repository:<dir>, machine, or machine:<root>", value)
}

// String renders the location in the shared value syntax.
func (l WorktreeLocation) String() string {
	switch {
	case l.Mode == WorktreeLocationRepository && l.Dir != "":
		return WorktreeLocationRepository + ":" + l.Dir
	case l.Mode == WorktreeLocationMachine && l.Root != "":
		return WorktreeLocationMachine + ":" + l.Root
	}
	return l.Mode
}

// Normalized validates the location and fills the per-mode default: dir
// `_worktrees` for repository mode and `~/.conveyor/worktrees` for machine
// mode. A chosen mode is an explicit choice; no mode is never defaulted.
func (l WorktreeLocation) Normalized(home string) (WorktreeLocation, error) {
	switch l.Mode {
	case WorktreeLocationRepository:
		if l.Root != "" {
			return WorktreeLocation{}, errors.New("worktree_location.root applies only to mode machine")
		}
		if l.Dir == "" {
			l.Dir = DefaultRepositoryWorktreeDir
		}
		if !safeWorktreeDir(l.Dir) {
			return WorktreeLocation{}, fmt.Errorf("worktree_location.dir %q must be one relative path component of ASCII letters, digits, '.', '_', and '-', not starting with '-', and other than ., .., and .git", l.Dir)
		}
		return l, nil
	case WorktreeLocationMachine:
		if l.Dir != "" {
			return WorktreeLocation{}, errors.New("worktree_location.dir applies only to mode repository")
		}
		l.Root = expandDefault(l.Root, home, DefaultWorktreeRoot(home))
		if !filepath.IsAbs(l.Root) {
			return WorktreeLocation{}, errors.New("worktree_location.root must be absolute after home expansion")
		}
		l.Root = filepath.Clean(l.Root)
		return l, nil
	case "":
		return WorktreeLocation{}, errors.New("worktree_location.mode is required: repository or machine")
	}
	return WorktreeLocation{}, fmt.Errorf("worktree_location.mode %q must be repository or machine", l.Mode)
}

// safeWorktreeDir keeps dir a literal Git path: no ignore-pattern comment,
// negation, or glob character and no pathspec magic (component-harness-execution).
func safeWorktreeDir(value string) bool {
	if !safePathSegment(value) || value == ".git" || strings.HasPrefix(value, "-") {
		return false
	}
	for _, character := range value {
		if !(character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '.' || character == '_' || character == '-') {
			return false
		}
	}
	return true
}

// ConfiguredWorktreeLocation reports the normalized location the document
// records. A legacy worktree_root reads as machine mode with that root. The
// second result is false when the document records no location, which callers
// must treat as undecided rather than defaulting (component-harness-execution).
func (c *Config) ConfiguredWorktreeLocation() (WorktreeLocation, bool) {
	if c == nil {
		return WorktreeLocation{}, false
	}
	if c.WorktreeLocation != nil {
		return *c.WorktreeLocation, true
	}
	if c.WorktreeRoot != "" {
		return WorktreeLocation{Mode: WorktreeLocationMachine, Root: c.WorktreeRoot}, true
	}
	return WorktreeLocation{}, false
}

func (c *Config) normalizeWorktreeLocation(home string) error {
	if c.WorktreeLocation != nil && c.WorktreeRoot != "" {
		return errors.New("set worktree_location or the legacy worktree_root, not both")
	}
	if c.WorktreeLocation != nil {
		normalized, err := c.WorktreeLocation.Normalized(home)
		if err != nil {
			return err
		}
		c.WorktreeLocation = &normalized
		return nil
	}
	if c.WorktreeRoot == "" {
		return nil
	}
	c.WorktreeRoot = expandDefault(c.WorktreeRoot, home, "")
	if !filepath.IsAbs(c.WorktreeRoot) {
		return errors.New("worktree_root must be absolute after home expansion")
	}
	c.WorktreeRoot = filepath.Clean(c.WorktreeRoot)
	return nil
}
