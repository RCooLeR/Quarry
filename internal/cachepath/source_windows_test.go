//go:build windows

package cachepath

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/quarry/quarry-wails3/internal/settings"
)

func TestSourcePathOnlyConvergesWindowsCaseVariantsThatResolveToSameFile(t *testing.T) {
	t.Setenv(settings.ConfigDirEnv, t.TempDir())
	dir := t.TempDir()
	source := filepath.Join(dir, "MixedCase.sql")
	if err := os.WriteFile(source, []byte("select 1;"), 0o600); err != nil {
		t.Fatal(err)
	}
	caseVariant := strings.ToUpper(source)
	originalCache, err := SourcePath("indexes", source, ".json")
	if err != nil {
		t.Fatal(err)
	}
	variantCache, err := SourcePath("indexes", caseVariant, ".json")
	if err != nil {
		t.Fatal(err)
	}
	originalInfo, originalErr := os.Stat(source)
	variantInfo, variantErr := os.Stat(caseVariant)
	sameExistingFile := originalErr == nil && variantErr == nil && os.SameFile(originalInfo, variantInfo)
	if sameExistingFile && originalCache != variantCache {
		t.Fatalf("aliases of the same file produced different cache paths: %q and %q", originalCache, variantCache)
	}
	if !sameExistingFile && originalCache == variantCache {
		t.Fatalf("distinct or unresolved case variants collided at %q", originalCache)
	}
}
