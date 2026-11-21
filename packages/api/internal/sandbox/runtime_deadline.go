package sandbox

import "time"

// NoRuntimeDeadline is the wire representation of a running sandbox with
// no automatic expiration. A finite RFC3339 timestamp preserves compatibility
// with the node, Redis expiration index and existing API clients.
func NoRuntimeDeadline() time.Time {
	return time.Date(9999, time.December, 31, 23, 59, 59, 0, time.UTC)
}
