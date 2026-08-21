// Package pack exposes the canonical role prompts embedded in Conveyor's
// release binaries. The files under roles remain the only authored source.
package pack

import "embed"

// RoleFiles contains the canonical role pack used when no deployment override
// is configured.
//
//go:embed roles/*.md
var RoleFiles embed.FS
