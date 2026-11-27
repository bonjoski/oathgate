package version

// Version is the canonical semantic version of oathgate.
// Overridden at build time via -ldflags "-X github.com/bonjoski/oathgate/pkg/version.Version=..."
var Version = "0.1.0"
