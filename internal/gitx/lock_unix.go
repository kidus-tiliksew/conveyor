//go:build unix

package gitx

import (
	"os"
	"path/filepath"
	"syscall"
)

// lockRepo takes an exclusive flock on <mirror>.lock, serializing
// fetches into a bare cache (spec §8.1). flock releases on process
// exit, so a crashed fetch never wedges the cache.
func lockRepo(mirrorDir string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(mirrorDir), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(mirrorDir+".lock", os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}
