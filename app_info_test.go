package main

import (
	"testing"

	"github.com/quarry/quarry-wails3/internal/buildinfo"
)

func TestGetBuildInfoUsesCanonicalBuildMetadata(t *testing.T) {
	got := NewFileService().GetBuildInfo()
	want := buildinfo.Current()
	if got.ApplicationName != want.ApplicationName || got.Version != want.Version || got.Commit != want.Commit || got.BuildDate != want.BuildDate {
		t.Fatalf("GetBuildInfo() = %+v, want %+v", got, want)
	}
}
