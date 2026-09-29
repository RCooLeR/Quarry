package fileio

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestPublishExistingNoClobberMovesCompleteRegularFile(t *testing.T) {
	requireExistingNoClobberPlatform(t)
	dir := t.TempDir()
	tempPath := filepath.Join(dir, "output.txt.quarry.tmp")
	finalPath := filepath.Join(dir, "output.txt")
	if err := os.WriteFile(tempPath, []byte("complete output"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := PublishExistingNoClobber(tempPath, finalPath); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(tempPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary path error = %v, want removed by atomic move", err)
	}
	assertFileContents(t, finalPath, "complete output")
}

func TestPublishExistingNoClobberNeverReplacesConcurrentCreator(t *testing.T) {
	requireExistingNoClobberPlatform(t)
	dir := t.TempDir()
	tempPath := filepath.Join(dir, "output.txt.quarry.tmp")
	finalPath := filepath.Join(dir, "output.txt")
	if err := os.WriteFile(tempPath, []byte("Quarry output"), 0o600); err != nil {
		t.Fatal(err)
	}

	originalHook := beforePublishExistingNoClobber
	beforePublishExistingNoClobber = func() {
		file, err := os.OpenFile(finalPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			t.Fatalf("create competing destination: %v", err)
		}
		if _, err := file.WriteString("competing output"); err != nil {
			_ = file.Close()
			t.Fatalf("write competing destination: %v", err)
		}
		if err := file.Close(); err != nil {
			t.Fatalf("close competing destination: %v", err)
		}
	}
	t.Cleanup(func() { beforePublishExistingNoClobber = originalHook })

	err := PublishExistingNoClobber(tempPath, finalPath)
	if !errors.Is(err, ErrExists) || !errors.Is(err, os.ErrExist) {
		t.Fatalf("publication error = %v, want ErrExists and os.ErrExist", err)
	}
	assertFileContents(t, finalPath, "competing output")
	assertFileContents(t, tempPath, "Quarry output")
}

func TestPublishExistingNoClobberReportsPostPublicationSyncFailure(t *testing.T) {
	requireExistingNoClobberPlatform(t)
	dir := t.TempDir()
	tempPath := filepath.Join(dir, "output.txt.quarry.tmp")
	finalPath := filepath.Join(dir, "output.txt")
	if err := os.WriteFile(tempPath, []byte("complete output"), 0o600); err != nil {
		t.Fatal(err)
	}

	syncFailure := errors.New("simulated parent sync failure")
	originalSync := syncDirPath
	syncDirPath = func(string) error { return syncFailure }
	t.Cleanup(func() { syncDirPath = originalSync })

	err := PublishExistingNoClobber(tempPath, finalPath)
	var publication *PublicationError
	if !errors.As(err, &publication) {
		t.Fatalf("publication error = %v, want PublicationError", err)
	}
	if !errors.Is(err, syncFailure) || publication.FinalPath != finalPath || publication.Durable || publication.LocationUncertain {
		t.Fatalf("publication = %#v, error = %v", publication, err)
	}
	assertFileContents(t, finalPath, "complete output")
	if _, statErr := os.Lstat(tempPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("temporary path error = %v, want promoted despite sync failure", statErr)
	}
}

func requireExistingNoClobberPlatform(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" && runtime.GOOS != "windows" {
		t.Skip("host intentionally fails closed without an atomic no-replace rename primitive")
	}
}

func assertFileContents(t *testing.T, path string, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("%s = %q, want %q", path, got, want)
	}
}
