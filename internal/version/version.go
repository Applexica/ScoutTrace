// Package version exposes the build-time version stamp.
package version

// Version is overridable via -ldflags "-X .../internal/version.Version=...".
var Version = "0.6.0"
