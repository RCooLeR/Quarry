package main

import "github.com/quarry/quarry-wails3/internal/buildinfo"

// BuildInfo is the stable bridge shape used by About and diagnostics. Values
// come from one linker-populated source instead of frontend/package templates.
type BuildInfo struct {
	ApplicationName string `json:"applicationName"`
	Version         string `json:"version"`
	Commit          string `json:"commit"`
	BuildDate       string `json:"buildDate"`
}

// GetBuildInfo returns immutable compile-time metadata for this process.
func (s *FileService) GetBuildInfo() BuildInfo {
	info := buildinfo.Current()
	return BuildInfo{
		ApplicationName: info.ApplicationName,
		Version:         info.Version,
		Commit:          info.Commit,
		BuildDate:       info.BuildDate,
	}
}
