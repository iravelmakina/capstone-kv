// Package buildinfo holds the version the binary was built from. The release build sets it
// with the linker (Dockerfile: -ldflags "-X …/buildinfo.Version=<commit sha>"), so a running
// node reports exactly what was compiled into it, whatever its configuration says.
package buildinfo

// Version is the commit SHA of the release, or "dev" for local builds.
var Version = "dev"

// BuildTime is when the release was built, in RFC 3339 UTC (set by the linker like
// Version), or "" for local builds made without it.
var BuildTime = ""
