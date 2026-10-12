package version

var (
	// Version is the current version of the application.
	// It is set at build time via -ldflags.
	Version = "N/A"

	// BuildTime is the time when the application was built.
	// It is set at build time via -ldflags.
	BuildTime = "N/A"

	// GitCommit is the git commit hash at build time.
	// It is set at build time via -ldflags.
	GitCommit = "N/A"
)
