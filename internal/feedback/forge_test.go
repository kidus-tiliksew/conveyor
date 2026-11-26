package feedback

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"

	"github.com/kidus-tiliksew/conveyor/internal/trigger/github"
)

// stubForge answers commit reads from a map and comparisons from a commit
// graph, and counts every call. It runs no git command and holds no token.
type stubForge struct {
	mu sync.Mutex
	// refs maps slug|ref to a full commit ID.
	refs map[string]string
	// fail maps slug|ref, or compare|slug|base|head, to a failure.
	fail map[string]error
	// parent maps slug|commit to its parent, so each repository is a tree.
	parent  map[string]string
	reads   []string
	compare []string
}

func newStubForge() *stubForge {
	return &stubForge{refs: map[string]string{}, fail: map[string]error{}, parent: map[string]string{}}
}

// chain records commits as a line of history in the repository, oldest
// first, starting from root ("" for none).
func (f *stubForge) chain(slug, root string, commits ...string) {
	previous := root
	for _, commit := range commits {
		f.parent[slug+"|"+commit] = previous
		f.refs[slug+"|"+commit] = commit
		f.refs[slug+"|"+commit[:7]] = commit
		previous = commit
	}
}

func (f *stubForge) ReadCommit(_ context.Context, slug, ref string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads = append(f.reads, slug+"|"+ref)
	if err := f.fail[slug+"|"+ref]; err != nil {
		return "", err
	}
	if commit, ok := f.refs[slug+"|"+ref]; ok {
		return commit, nil
	}
	// GitHub resolves an abbreviated commit ID that names one commit.
	for key := range f.parent {
		if commit, found := strings.CutPrefix(key, slug+"|"); found && len(ref) >= 7 && strings.HasPrefix(commit, ref) {
			return commit, nil
		}
	}
	return "", github.CommitNotFoundError(ref)
}

// ancestors returns head and its ancestors, newest first.
func (f *stubForge) ancestors(slug, head string) []string {
	out := []string{}
	for commit := head; commit != ""; commit = f.parent[slug+"|"+commit] {
		out = append(out, commit)
	}
	return out
}

func (f *stubForge) known(slug, commit string) bool {
	_, ok := f.parent[slug+"|"+commit]
	return ok
}

func (f *stubForge) Compare(_ context.Context, slug, base, head string, arrived int) (CompareResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.compare = append(f.compare, slug+"|"+base+"|"+head)
	if err := f.fail["compare|"+slug+"|"+base+"|"+head]; err != nil {
		return CompareResult{}, err
	}
	if !f.known(slug, base) || !f.known(slug, head) {
		return CompareResult{}, github.StatusError(404, errors.New("not found"))
	}
	if base == head {
		return CompareResult{Status: "identical", Complete: true}, nil
	}
	headLine := f.ancestors(slug, head)
	if index := slices.Index(headLine, base); index >= 0 {
		between := slices.Clone(headLine[:index])
		slices.Reverse(between)
		result := CompareResult{Status: "ahead", Complete: true}
		if arrived > 0 {
			if len(between) > arrived {
				between, result.Complete = between[:arrived], false
			}
			result.Commits = between
		}
		return result, nil
	}
	if slices.Contains(f.ancestors(slug, base), head) {
		return CompareResult{Status: "behind"}, nil
	}
	return CompareResult{Status: "diverged"}, nil
}

func (f *stubForge) calls() (reads, compares int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.reads), len(f.compare)
}
