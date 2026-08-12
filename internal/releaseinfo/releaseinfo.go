// Package releaseinfo exposes the immutable release identity embedded in both
// Conveyor binaries at build time.
package releaseinfo

// Version is replaced by the Makefile's -ldflags value for versioned builds.
var Version = "dev"
