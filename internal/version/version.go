package version

// Build is injected at compile time via -ldflags "-X github.com/ylnhari/rover/internal/version.Build=1.2.3".
// Falls back to "dev" for local builds.
var Build = "dev"
