package replace

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/quarry/quarry-wails3/internal/fileio"
)

func TestWriteManifestRefreshPreservesUnownedLegacyTemp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "output.sql.quarry.manifest.json")
	original := testManifestForAtomicRefresh(dir, "running")
	if err := writeManifest(path, original, true); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	tempPath := path + manifestAtomicTempSuffix
	tempSentinel := []byte("unowned temp sentinel")
	if err := os.WriteFile(tempPath, tempSentinel, 0o600); err != nil {
		t.Fatal(err)
	}
	updated := original
	updated.Status = "failed"
	updated.Error = "forced refresh"
	if err := writeManifest(path, updated, false); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(after, before) {
		t.Fatal("successful refresh left the previous manifest revision unchanged")
	}
	gotSentinel, err := os.ReadFile(tempPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotSentinel, tempSentinel) {
		t.Fatalf("unowned temp changed to %q", gotSentinel)
	}

	loaded, err := LoadManifest(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Status != updated.Status || loaded.Error != updated.Error {
		t.Fatalf("loaded manifest = %#v, want refreshed status/error", loaded)
	}
	if err := os.Remove(tempPath); err != nil {
		t.Fatal(err)
	}
	for _, artifact := range []string{tempPath, path + ".quarry.overwrite.bak"} {
		if _, err := os.Lstat(artifact); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("artifact %q remains: %v", artifact, err)
		}
	}
}

func TestWriteManifestRejectsOversizedRevisionWithoutArtifacts(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "output.sql.quarry.manifest.json")
	original := testManifestForAtomicRefresh(dir, "running")
	if err := writeManifest(path, original, true); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	oversized := original
	oversized.Error = strings.Repeat("x", maxRecoveryManifestBytes)
	if err := writeManifest(path, oversized, false); err == nil {
		t.Fatal("oversized manifest refresh succeeded")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatal("oversized refresh changed the live manifest")
	}
	for _, artifact := range []string{path + manifestAtomicTempSuffix, path + ".quarry.overwrite.bak"} {
		if _, err := os.Lstat(artifact); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("artifact %q remains: %v", artifact, err)
		}
	}
}

func TestWriteManifestInitialShortWriteRemovesOwnedPartial(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "output.sql.quarry.manifest.json")
	originalOpen := openManifestOut
	openManifestOut = func(gotPath string, exclusive bool) (io.WriteCloser, error) {
		if !exclusive {
			t.Fatalf("initial manifest unexpectedly opened as a refresh")
		}
		output, err := fileio.OpenExclusiveOutput(gotPath, 0o600)
		if err != nil {
			return nil, err
		}
		return &shortExclusiveManifestWriter{ExclusiveOutput: output}, nil
	}
	defer func() { openManifestOut = originalOpen }()

	err := writeManifest(path, testManifestForAtomicRefresh(dir, "running"), true)
	if !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("initial manifest error = %v, want io.ErrShortWrite", err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial initial manifest remains: %v", err)
	}
}

func TestWriteManifestRefreshDoesNotTruncateHardLinkTarget(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "output.sql.quarry.manifest.json")
	sentinelPath := filepath.Join(dir, "sentinel.txt")
	sentinel := []byte("important source bytes")
	if err := os.WriteFile(sentinelPath, sentinel, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(sentinelPath, path); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}

	manifest := testManifestForAtomicRefresh(dir, "failed")
	manifest.Error = "replacement evidence"
	if err := writeManifest(path, manifest, false); err != nil {
		t.Fatal(err)
	}
	gotSentinel, err := os.ReadFile(sentinelPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotSentinel, sentinel) {
		t.Fatalf("hard-link target changed to %q", gotSentinel)
	}
	manifestInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	sentinelInfo, err := os.Stat(sentinelPath)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(manifestInfo, sentinelInfo) {
		t.Fatal("refreshed manifest still aliases the sentinel")
	}
	if _, err := LoadManifest(path); err != nil {
		t.Fatalf("refreshed manifest is invalid: %v", err)
	}
}

func testManifestForAtomicRefresh(dir, status string) Manifest {
	source := filepath.Join(dir, "source.sql")
	output := filepath.Join(dir, "output.sql")
	return Manifest{
		Operation:  "plain-replace",
		Source:     source,
		Output:     output,
		TempOutput: output + ".quarry.tmp",
		Phase:      "processing",
		StartedAt:  time.Unix(1_700_000_000, 0).UTC(),
		SourceSize: 32,
		Status:     status,
	}
}

type shortExclusiveManifestWriter struct {
	*fileio.ExclusiveOutput
}

func (w *shortExclusiveManifestWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	return w.ExclusiveOutput.Write(p[:len(p)/2])
}
