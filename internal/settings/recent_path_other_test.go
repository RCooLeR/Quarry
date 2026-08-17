//go:build !windows

package settings

import (
	"path/filepath"
	"testing"
)

func TestRecentFilesPreservePathWhitespace(t *testing.T) {
	selected := filepath.Join(t.TempDir(), " report.sql ")
	settings := Defaults().WithRecentFile(selected)
	if len(settings.RecentFiles) != 1 || settings.RecentFiles[0] != selected {
		t.Fatalf("recent files = %#v, want exact selected spelling %q", settings.RecentFiles, selected)
	}
	trimmed := filepath.Join(filepath.Dir(selected), "report.sql")
	if settings.RecentFiles[0] == trimmed {
		t.Fatal("recent-file path was silently trimmed")
	}
}

func TestRecentFilesPreserveAllWhitespacePath(t *testing.T) {
	settings := Defaults().WithRecentFile("   ")
	if len(settings.RecentFiles) != 1 || settings.RecentFiles[0] != "   " {
		t.Fatalf("recent files = %#v, want exact all-whitespace path", settings.RecentFiles)
	}
}

func TestConfigDirPreservesWhitespaceInOverridePath(t *testing.T) {
	selected := filepath.Join(t.TempDir(), " config ")
	t.Setenv(ConfigDirEnv, selected)
	got, err := ConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Clean(selected) {
		t.Fatalf("config dir = %q, want exact selected spelling %q", got, filepath.Clean(selected))
	}
}
