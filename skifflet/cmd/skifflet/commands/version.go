package commands

import "skifflet/pkg/version"

var (
	BuildVersion = version.Version
	BuildTime    = version.BuildTime
	GitCommit    = version.GitCommit
)
