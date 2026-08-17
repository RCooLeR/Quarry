package session

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"
)

func TestRegistryReopenReplacesImmutableDocumentGeneration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source.txt")
	if err := os.WriteFile(path, []byte("first generation\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	registry := New()
	opened, err := registry.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, ok := registry.Get(opened.ID); ok {
			_ = registry.Close(opened.ID)
		}
	})
	before, ok := registry.Snapshot(opened.ID)
	if !ok {
		t.Fatal("missing opened generation")
	}

	reopened, err := registry.Reopen(opened.ID)
	if err != nil {
		t.Fatal(err)
	}
	after, ok := registry.Snapshot(opened.ID)
	if !ok {
		t.Fatal("missing reopened generation")
	}
	if reopened != mustRegistryFile(t, registry, opened.ID) {
		t.Fatal("Reopen did not return the installed registry entry")
	}
	if before.Generation != 1 || after.Generation != 2 {
		t.Fatalf("generations = %d -> %d, want 1 -> 2", before.Generation, after.Generation)
	}
	if before.Doc == after.Doc || opened == reopened {
		t.Fatal("reopen mutated an existing file/document object instead of replacing the generation")
	}
	if before.Path != after.Path || before.ID != after.ID {
		t.Fatalf("identity changed across reopen: before=%+v after=%+v", before, after)
	}
}

func TestRegistryReopenReturnsInstalledGenerationWithCommittedCloseError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "committed-close-error.txt")
	if err := os.WriteFile(path, []byte("current source\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	registry := New()
	opened, err := registry.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, ok := registry.Get(opened.ID); ok {
			_ = registry.Close(opened.ID)
		}
	})
	before, ok := registry.Snapshot(opened.ID)
	if !ok {
		t.Fatal("missing opened generation")
	}

	injected := errors.New("injected previous-generation close failure")
	registry.replacedDocumentCloseResult = func(closeErr error) error {
		return errors.Join(closeErr, injected)
	}
	replacement, reopenErr := registry.Reopen(opened.ID)
	if replacement == nil {
		t.Fatalf("committed reopen returned nil replacement: %v", reopenErr)
	}
	var committed *ReopenCommittedError
	if !errors.As(reopenErr, &committed) || !errors.Is(reopenErr, ErrReopenCommitted) || !errors.Is(reopenErr, injected) {
		t.Fatalf("reopen error = %v, want typed committed error wrapping sentinels", reopenErr)
	}
	if committed.FileID != opened.ID {
		t.Fatalf("committed error file id = %q, want %q", committed.FileID, opened.ID)
	}

	after, ok := registry.Snapshot(opened.ID)
	if !ok || after.Doc != replacement.Doc || after.Generation != before.Generation+1 {
		t.Fatalf("installed generation after close error = %+v, present=%v; replacement=%+v", after, ok, replacement)
	}
	if current := mustRegistryFile(t, registry, opened.ID); current != replacement {
		t.Fatal("registry did not retain the replacement returned with committed error")
	}
	if _, err := before.Doc.ReadRange(0, 1); err == nil {
		t.Fatal("previous document remained readable after its Close returned an injected error")
	}
	if got, err := replacement.Doc.ReadRange(0, 7); err != nil || string(got) != "current" {
		t.Fatalf("replacement read = %q, err=%v", got, err)
	}
}

func TestRegistryValidatedReopenKeepsOldGenerationWhenCandidateRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reopen.txt")
	if err := os.WriteFile(path, []byte("one\ntwo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	registry := New()
	opened, err := registry.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = registry.Close(opened.ID) })
	before, _ := registry.Snapshot(opened.ID)
	transition, err := registry.BeginTransition(opened.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer transition.Abort()
	lease, err := transition.Wait()
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	sentinel := errors.New("reject candidate")
	validatorCalls := 0
	if replacement, err := registry.ReopenUnderLeaseValidated(lease, func(candidate *File) error {
		validatorCalls++
		if candidate == nil || candidate.Doc == nil || candidate.Doc == before.Doc || candidate.generation != before.Generation+1 {
			t.Fatalf("candidate = %+v, before = %+v", candidate, before)
		}
		return sentinel
	}); replacement != nil || !errors.Is(err, sentinel) {
		t.Fatalf("validated reopen = %+v, %v; want nil/sentinel", replacement, err)
	}
	if validatorCalls != 1 {
		t.Fatalf("validator calls = %d, want 1", validatorCalls)
	}
	after, ok := registry.Snapshot(opened.ID)
	if !ok || after.Generation != before.Generation || after.Doc != before.Doc {
		t.Fatalf("rejected candidate changed generation: before=%+v after=%+v present=%v", before, after, ok)
	}
	if _, err := before.Doc.ReadRange(0, 3); err != nil {
		t.Fatalf("old document was closed after candidate rejection: %v", err)
	}
}

func TestRegistryFileGenerationStopsAtBridgeSafeInteger(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bridge-generation.txt")
	if err := os.WriteFile(path, []byte("bridge-safe generation\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	registry := New()
	opened, err := registry.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = registry.Close(opened.ID) })

	// The counter is process-local and private. Position it at the last value
	// before the JSON bridge ceiling so both the final exact value and the first
	// rejected transition are covered without billions of refreshes.
	opened.generation = MaxBridgeFileGeneration - 1
	last, err := registry.Reopen(opened.ID)
	if err != nil {
		t.Fatalf("last bridge-safe reopen: %v", err)
	}
	lastSnapshot, ok := registry.Snapshot(opened.ID)
	if !ok || lastSnapshot.Generation != MaxBridgeFileGeneration || lastSnapshot.Doc != last.Doc {
		t.Fatalf("last bridge-safe generation = %+v, present=%v", lastSnapshot, ok)
	}

	if replacement, err := registry.Reopen(opened.ID); replacement != nil || !errors.Is(err, ErrFileGenerationExhausted) {
		t.Fatalf("reopen after bridge-safe generation = %+v, err=%v", replacement, err)
	}
	after, ok := registry.Snapshot(opened.ID)
	if !ok || after.Generation != lastSnapshot.Generation || after.Doc != lastSnapshot.Doc {
		t.Fatalf("generation exhaustion changed installed source: before=%+v after=%+v present=%v", lastSnapshot, after, ok)
	}
	if got, err := after.Doc.ReadRange(0, 6); err != nil || string(got) != "bridge" {
		t.Fatalf("retained generation read = %q, err=%v", got, err)
	}
}

func TestRegistryCloseRemovesGenerationSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source.txt")
	if err := os.WriteFile(path, []byte("source\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	registry := New()
	opened, err := registry.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := registry.Snapshot(opened.ID); !ok {
		t.Fatal("missing opened generation")
	}
	if err := registry.Close(opened.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := registry.Snapshot(opened.ID); ok {
		t.Fatal("closed generation remains visible")
	}
}

func TestRegistryFileIDSequenceExhaustionDoesNotInstallUnaddressableFile(t *testing.T) {
	dir := t.TempDir()
	firstPath := filepath.Join(dir, "last-addressable.txt")
	secondPath := filepath.Join(dir, "must-not-open.txt")
	for _, path := range []string{firstPath, secondPath} {
		if err := os.WriteFile(path, []byte(path), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	registry := New()
	registry.seq = math.MaxInt64 - 1
	opened, added, err := registry.OpenWithStatus(firstPath)
	if err != nil {
		t.Fatal(err)
	}
	if !added || opened.ID != "f9223372036854775807" {
		t.Fatalf("last addressable open = %+v, added=%v", opened, added)
	}
	t.Cleanup(func() { _ = registry.Close(opened.ID) })

	// Exhaustion applies only when a new public ID is needed. An alias of an
	// already-open source still resolves to its existing, addressable session.
	duplicate, added, err := registry.OpenWithStatus(firstPath)
	if err != nil || added || duplicate != opened {
		t.Fatalf("duplicate at sequence limit = %+v, added=%v, err=%v", duplicate, added, err)
	}

	if candidate, added, err := registry.OpenWithStatus(secondPath); candidate != nil || added || !errors.Is(err, ErrFileIDSequenceExhausted) {
		t.Fatalf("open after sequence exhaustion = %+v, added=%v, err=%v", candidate, added, err)
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if registry.seq != math.MaxInt64 || registry.openCount != 1 || len(registry.files) != 1 || registry.opening != 0 {
		t.Fatalf("registry state after exhaustion: seq=%d openCount=%d files=%d opening=%d", registry.seq, registry.openCount, len(registry.files), registry.opening)
	}
}

func mustRegistryFile(t *testing.T, registry *Registry, id string) *File {
	t.Helper()
	file, ok := registry.Get(id)
	if !ok {
		t.Fatalf("missing registry file %q", id)
	}
	return file
}
