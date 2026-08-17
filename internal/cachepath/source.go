package cachepath

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"strings"

	"github.com/quarry/quarry-wails3/internal/fileio"
	"github.com/quarry/quarry-wails3/internal/settings"
)

// SourcePath returns a stable Quarry-owned cache path for side data derived from
// a source file. The cache payload still stores source metadata and must validate
// it before reuse.
func SourcePath(kind string, sourcePath string, extension string) (string, error) {
	if !validCacheKind(kind) {
		return "", errors.New("cache kind must be a single path segment")
	}
	if sourcePath == "" {
		return "", errors.New("source path is empty")
	}
	extension, ok := normalizeCacheExtension(extension)
	if !ok {
		return "", errors.New("cache extension must be a simple filename extension")
	}

	cleanSource, err := filepath.Abs(sourcePath)
	if err != nil {
		cleanSource = filepath.Clean(sourcePath)
	}
	if resolved, resolveErr := filepath.EvalSymlinks(cleanSource); resolveErr == nil {
		cleanSource = resolved
	}
	cleanSource = normalizeSourceIdentity(filepath.Clean(cleanSource))
	root, err := settings.ConfigDir()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(filepath.Clean(cleanSource)))
	cacheDir, err := fileio.ExactChildPath(root, "cache")
	if err != nil {
		return "", err
	}
	kindDir, err := fileio.ExactChildPath(cacheDir, kind)
	if err != nil {
		return "", err
	}
	return fileio.ExactChildPath(kindDir, hex.EncodeToString(sum[:])+extension)
}

func validCacheKind(kind string) bool {
	if kind == "" || len(kind) > 64 || kind == "." || kind == ".." || strings.TrimSpace(kind) != kind {
		return false
	}
	for i := 0; i < len(kind); i++ {
		c := kind[i]
		if !isASCIIAlphaNumeric(c) && c != '.' && c != '_' && c != '-' {
			return false
		}
	}
	if !isASCIIAlphaNumeric(kind[0]) || !isASCIIAlphaNumeric(kind[len(kind)-1]) {
		return false
	}
	base := strings.ToUpper(strings.SplitN(kind, ".", 2)[0])
	if base == "CON" || base == "PRN" || base == "AUX" || base == "NUL" {
		return false
	}
	if len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && base[3] >= '1' && base[3] <= '9' {
		return false
	}
	return true
}

func normalizeCacheExtension(extension string) (string, bool) {
	if extension == "" || strings.TrimSpace(extension) != extension {
		return "", false
	}
	if extension[0] != '.' {
		extension = "." + extension
	}
	if len(extension) < 2 || len(extension) > 32 || !isASCIIAlphaNumeric(extension[1]) || !isASCIIAlphaNumeric(extension[len(extension)-1]) {
		return "", false
	}
	for i := 1; i < len(extension); i++ {
		c := extension[i]
		if !isASCIIAlphaNumeric(c) && c != '.' && c != '_' && c != '-' {
			return "", false
		}
	}
	return extension, true
}

func isASCIIAlphaNumeric(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}
