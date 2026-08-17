package session

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func TestRegistryOpenDeduplicatesRelativeAndHardLinkAliases(t *testing.T) {
	dir := tempDirOnRegistryWorkingVolume(t)
	sourcePath := filepath.Join(dir, "Source.txt")
	if err := os.WriteFile(sourcePath, []byte("identity\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	workingDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	relativePath, err := filepath.Rel(workingDir, sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	hardLinkPath := filepath.Join(dir, "hard-link.txt")
	if err := os.Link(sourcePath, hardLinkPath); err != nil {
		t.Fatal(err)
	}

	registry := New()
	first, added, err := registry.OpenWithStatus(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if !added {
		t.Fatal("first open was not reported as a new session")
	}
	t.Cleanup(func() {
		if _, ok := registry.Get(first.ID); ok {
			_ = registry.Close(first.ID)
		}
	})

	aliases := []string{sourcePath, relativePath, hardLinkPath}
	for _, alias := range aliases {
		got, aliasAdded, err := registry.OpenWithStatus(alias)
		if err != nil {
			t.Fatalf("open alias %q: %v", alias, err)
		}
		if aliasAdded {
			t.Fatalf("alias %q created a second session", alias)
		}
		if got != first || got.ID != first.ID || got.Doc != first.Doc {
			t.Fatalf("alias %q returned a different session: first=%p/%s got=%p/%s", alias, first, first.ID, got, got.ID)
		}
		if got.Path != sourcePath {
			t.Fatalf("alias %q replaced the first path spelling: got %q, want %q", alias, got.Path, sourcePath)
		}
	}
	if len(registry.files) != 1 || registry.seq != 1 {
		t.Fatalf("registry identity count = files:%d seq:%d, want files:1 seq:1", len(registry.files), registry.seq)
	}
}

func tempDirOnRegistryWorkingVolume(t *testing.T) string {
	t.Helper()
	workingDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(workingDir, ".registry-open-identity-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func TestRegistryOpenDeduplicatesSymlinkAndCaseAliasesWhenSupported(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "MixedCase.txt")
	if err := os.WriteFile(sourcePath, []byte("identity\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	aliases := make([]string, 0, 2)
	symlinkPath := filepath.Join(dir, "symbolic-link.txt")
	if err := os.Symlink(sourcePath, symlinkPath); err == nil {
		aliases = append(aliases, symlinkPath)
	}
	if runtime.GOOS == "windows" {
		caseAlias := strings.ToUpper(sourcePath)
		if _, err := os.Stat(caseAlias); err == nil && caseAlias != sourcePath {
			aliases = append(aliases, caseAlias)
		}
	}
	if len(aliases) == 0 {
		t.Skip("no symlink or case alias is supported on this filesystem")
	}

	registry := New()
	first, added, err := registry.OpenWithStatus(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if !added {
		t.Fatal("first open was not reported as a new session")
	}
	t.Cleanup(func() { _ = registry.Close(first.ID) })
	for _, alias := range aliases {
		got, aliasAdded, err := registry.OpenWithStatus(alias)
		if err != nil {
			t.Fatalf("open alias %q: %v", alias, err)
		}
		if aliasAdded || got != first {
			t.Fatalf("alias %q was not deduplicated", alias)
		}
		if got.Path != sourcePath {
			t.Fatalf("alias %q replaced exact first path %q with %q", alias, sourcePath, got.Path)
		}
	}
}

func TestRegistryConcurrentOpenInstallsExactlyOneIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source.txt")
	if err := os.WriteFile(path, []byte("concurrent identity\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	registry := New()
	// Exercise the largest candidate set admitted simultaneously. Calls beyond
	// this bound are intentionally rejected before opening another OS handle and
	// are covered by TestRegistryLimitsConcurrentPreinstallationOpenHandles.
	const workers = DefaultMaxConcurrentOpens
	type result struct {
		file  *File
		added bool
		err   error
	}
	start := make(chan struct{})
	results := make(chan result, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			file, added, err := registry.OpenWithStatus(path)
			results <- result{file: file, added: added, err: err}
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	var first *File
	addedCount := 0
	for result := range results {
		if result.err != nil {
			t.Fatal(result.err)
		}
		if result.added {
			addedCount++
		}
		if first == nil {
			first = result.file
		}
		if result.file != first {
			t.Fatalf("concurrent open returned distinct File pointers: %p and %p", first, result.file)
		}
	}
	if addedCount != 1 {
		t.Fatalf("new-session results = %d, want 1", addedCount)
	}
	if first == nil {
		t.Fatal("concurrent open returned no file")
	}
	if len(registry.files) != 1 || registry.seq != 1 {
		t.Fatalf("registry identity count = files:%d seq:%d, want files:1 seq:1", len(registry.files), registry.seq)
	}
	if err := registry.Close(first.ID); err != nil {
		t.Fatal(err)
	}
}

func TestRegistryAliasOpenPreservesReopenedGeneration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "source.txt")
	alias := filepath.Join(dir, "alias.txt")
	if err := os.WriteFile(path, []byte("first\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(path, alias); err != nil {
		t.Fatal(err)
	}
	registry := New()
	first, err := registry.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := registry.Reopen(first.ID)
	if err != nil {
		t.Fatal(err)
	}
	got, added, err := registry.OpenWithStatus(alias)
	if err != nil {
		t.Fatal(err)
	}
	if added || got != reopened || got == first {
		t.Fatalf("alias changed the reopened generation: added=%v first=%p reopened=%p got=%p", added, first, reopened, got)
	}
	if snapshot, ok := registry.Snapshot(got.ID); !ok || snapshot.Generation != 2 {
		t.Fatalf("alias generation = %+v, %v; want generation 2", snapshot, ok)
	}
	if err := registry.Close(got.ID); err != nil {
		t.Fatal(err)
	}
}

func TestRegistryOpenAfterCloseCreatesNewSession(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source.txt")
	if err := os.WriteFile(path, []byte("source\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	registry := New()
	first, err := registry.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Close(first.ID); err != nil {
		t.Fatal(err)
	}
	second, added, err := registry.OpenWithStatus(path)
	if err != nil {
		t.Fatal(err)
	}
	if !added || second.ID == first.ID || second == first {
		t.Fatalf("post-close open did not create a new session: first=%p/%s second=%p/%s added=%v", first, first.ID, second, second.ID, added)
	}
	if err := registry.Close(second.ID); err != nil {
		t.Fatal(err)
	}
}

func TestRegistryOpenFailsClosedWhenExistingIdentityCannotBeRead(t *testing.T) {
	dir := t.TempDir()
	firstPath := filepath.Join(dir, "first.txt")
	secondPath := filepath.Join(dir, "second.txt")
	if err := os.WriteFile(firstPath, []byte("first\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secondPath, []byte("second\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	registry := New()
	first, err := registry.Open(firstPath)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate an invariant violation below the registry. It must not respond
	// by installing an identity it can no longer compare safely.
	if err := first.Doc.Close(); err != nil {
		t.Fatal(err)
	}
	got, added, err := registry.OpenWithStatus(secondPath)
	if err == nil {
		t.Fatal("open succeeded despite unreadable existing handle identity")
	}
	if got != nil || added {
		t.Fatalf("failed-closed open = %p, added=%v; want nil, false", got, added)
	}
	if len(registry.files) != 1 || registry.seq != 1 {
		t.Fatalf("failed identity comparison mutated registry: files=%d seq=%d", len(registry.files), registry.seq)
	}
	handle, closeErr := registry.BeginClose(first.ID)
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	_ = handle.Finish() // the deliberately pre-closed os.File may report Close twice
}
