// Package version says which build this is.
package version

// Version is "dev" for a local build. Release builds overwrite it with the
// tag, through -ldflags "-X github.com/ponyo877/scrim/pkg/version.Version=v1.2.3".
var Version = "dev"

// SidecarImage is the sidecar image that belongs to this build. The agent
// and the sidecar speak a protocol of their own, so they ship as a pair.
func SidecarImage() string {
	if Version == "dev" {
		return "scrim-sidecar:dev" // built locally by `make sidecar-image`
	}
	return "ghcr.io/ponyo877/scrim-sidecar:" + Version
}
