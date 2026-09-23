// Package version carries build metadata injected at link time.
package version

import "fmt"

var (
	// Version is the semantic version, set via -ldflags.
	Version = "dev"
	// Commit is the git revision, set via -ldflags.
	Commit = "none"
	// Date is the build timestamp, set via -ldflags.
	Date = "unknown"
)

// String returns a human readable build identifier.
func String() string {
	return fmt.Sprintf("aiproxy %s (commit %s, built %s)", Version, Commit, Date)
}
