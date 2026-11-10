// Package version reports the version of the running binary.
package version

// version is the CLI's release version. Bump it when releasing; release
// builds can also override it at link time:
//
//	go build -ldflags "-X github.com/biga-app/cli/internal/version.version=1.2.3"
var version = "1.1.0"

// String returns the release version, e.g. "1.0.0".
func String() string { return version }
