package cachepath

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/quarry/quarry-wails3/internal/settings"
)

func TestSourcePathUsesQuarryHomeCacheDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv(settings.ConfigDirEnv, home)
	source := filepath.Join(t.TempDir(), "data", "dump.sql")

	got, err := SourcePath("indexes", source, ".json")
	if err != nil {
		t.Fatal(err)
	}
	wantPrefix := filepath.Join(home, "cache", "indexes") + string(filepath.Separator)
	if !strings.HasPrefix(got, wantPrefix) {
		t.Fatalf("cache path = %q, want prefix %q", got, wantPrefix)
	}
	if filepath.Ext(got) != ".json" {
		t.Fatalf("cache path = %q, want .json extension", got)
	}

	again, err := SourcePath("indexes", source, "json")
	if err != nil {
		t.Fatal(err)
	}
	if again != got {
		t.Fatalf("cache path is not stable: %q then %q", got, again)
	}
}

func TestSourcePathDistinguishesSameBasenameSources(t *testing.T) {
	t.Setenv(settings.ConfigDirEnv, t.TempDir())
	first := filepath.Join(t.TempDir(), "a", "dump.sql")
	second := filepath.Join(t.TempDir(), "b", "dump.sql")

	firstPath, err := SourcePath("indexes", first, ".json")
	if err != nil {
		t.Fatal(err)
	}
	secondPath, err := SourcePath("indexes", second, ".json")
	if err != nil {
		t.Fatal(err)
	}
	if firstPath == secondPath {
		t.Fatalf("cache paths collided for same basename sources: %q", firstPath)
	}
}

func TestSourcePathRejectsUnsafeKind(t *testing.T) {
	t.Setenv(settings.ConfigDirEnv, t.TempDir())
	for _, kind := range []string{"", ".", "..", " bad", "bad ", "bad/kind", `bad\kind`, "C:kind", "-bad", "bad.", "CON", "nul.txt", "COM1"} {
		t.Run(kind, func(t *testing.T) {
			if _, err := SourcePath(kind, "source.sql", ".json"); err == nil {
				t.Fatalf("expected unsafe kind %q to fail", kind)
			}
		})
	}
}

func TestSourcePathRejectsUnsafeExtension(t *testing.T) {
	t.Setenv(settings.ConfigDirEnv, t.TempDir())
	for _, extension := range []string{"", ".", "..", " json", "json ", "../json", `..\json`, ".json/child", `.json\child`, "C:json", "-json", ".json."} {
		t.Run(extension, func(t *testing.T) {
			if _, err := SourcePath("indexes", "source.sql", extension); err == nil {
				t.Fatalf("expected unsafe extension %q to fail", extension)
			}
		})
	}
}

func TestSourcePathPreservesWhitespaceInSourceSpelling(t *testing.T) {
	t.Setenv(settings.ConfigDirEnv, t.TempDir())
	withSpace, err := SourcePath("indexes", " source.sql ", ".json")
	if err != nil {
		t.Fatal(err)
	}
	withoutSpace, err := SourcePath("indexes", "source.sql", ".json")
	if err != nil {
		t.Fatal(err)
	}
	if withSpace == withoutSpace {
		t.Fatal("cache identity silently trimmed a source path")
	}
}

func TestSourcePathResolvesSymlinkAliases(t *testing.T) {
	t.Setenv(settings.ConfigDirEnv, t.TempDir())
	dir := t.TempDir()
	target := filepath.Join(dir, "source.sql")
	alias := filepath.Join(dir, "alias.sql")
	if err := os.WriteFile(target, []byte("select 1;"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, alias); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	targetCache, err := SourcePath("indexes", target, ".json")
	if err != nil {
		t.Fatal(err)
	}
	aliasCache, err := SourcePath("indexes", alias, ".json")
	if err != nil {
		t.Fatal(err)
	}
	if aliasCache != targetCache {
		t.Fatalf("symlink alias cache = %q, target cache = %q", aliasCache, targetCache)
	}
}
