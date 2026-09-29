//go:build windows

package document

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/quarry/quarry-wails3/internal/settings"
)

func TestIndexCachePublicationRetriesTransientWindowsReader(t *testing.T) {
	t.Setenv(settings.ConfigDirEnv, filepath.Join(t.TempDir(), "config"))
	fixture := newIndexCacheFixture(t, t.TempDir(), "source.txt", bytes.Repeat([]byte("row\n"), 2_000))
	fixture.save(t)

	cachePath := indexCachePath(fixture.path)
	reader, err := os.Open(cachePath)
	if err != nil {
		t.Fatal(err)
	}
	readerClosed := false
	t.Cleanup(func() {
		if !readerClosed {
			_ = reader.Close()
		}
	})

	result := make(chan error, 1)
	go func() {
		result <- saveIndexCache(fixture.path, fixture.info.Size(), fixture.info.ModTime(), fixture.hash, fixture.idx)
	}()

	deadline := time.Now().Add(5 * time.Second)
	for !indexCacheTempExists(t, filepath.Dir(cachePath)) {
		select {
		case err := <-result:
			t.Fatalf("publication returned before the blocking reader closed: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for cache publication temp")
		}
		time.Sleep(time.Millisecond)
	}

	// The temp is fully written before publication. Holding the destination a
	// little longer ensures at least one Windows replace attempt encounters the
	// reader that intentionally lacks delete sharing.
	time.Sleep(25 * time.Millisecond)
	select {
	case err := <-result:
		t.Fatalf("publication did not retry the transient reader: %v", err)
	default:
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	readerClosed = true

	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for cache publication after closing reader")
	}
	if _, ok := fixture.load(); !ok {
		t.Fatal("published cache was not readable")
	}
}

func indexCacheTempExists(t testing.TB, dir string) bool {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if isRecognizedIndexCacheTempName(entry.Name()) {
			return true
		}
	}
	return false
}
