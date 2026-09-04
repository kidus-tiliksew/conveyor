package main

import "strings"

// queueShadowEnabled reads CONVEYOR_QUEUE_SHADOW. Accepted values are on,
// true, 1, and yes; anything else, including unset, leaves the shadow off.
func queueShadowEnabled(getenv func(string) string) bool {
	switch strings.ToLower(strings.TrimSpace(getenv("CONVEYOR_QUEUE_SHADOW"))) {
	case "on", "true", "1", "yes":
		return true
	}
	return false
}
