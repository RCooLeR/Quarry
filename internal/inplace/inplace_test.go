package inplace

import (
	"encoding/binary"
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "data.bin")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func testSourceDescriptor(t *testing.T, path string) sourceDescriptor {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	identity, err := sourceIdentityForFile(f)
	if err != nil {
		t.Fatal(err)
	}
	return sourceDescriptor{size: info.Size(), identity: identity}
}

func createTestSidecar(t *testing.T, sidecarPath string, sourcePath string, patches []Patch) *ownedSidecar {
	t.Helper()
	ordered, payload, err := validate(patches)
	if err != nil {
		t.Fatal(err)
	}
	sidecar, err := createSidecar(sidecarPath, testSourceDescriptor(t, sourcePath), ordered, payload)
	if err != nil {
		t.Fatal(err)
	}
	return sidecar
}

func TestApplyIsDisabledBeforeValidationOrFilesystemAccess(t *testing.T) {
	dir := t.TempDir()
	missingSource := filepath.Join(dir, "missing", "source.bin")
	missingSidecar := filepath.Join(dir, "missing", "source.qrp")
	err := Apply(missingSource, []Patch{{Old: []byte("x"), New: []byte("longer")}}, missingSidecar)
	if !errors.Is(err, ErrMutationDisabled) {
		t.Fatalf("Apply error = %v, want ErrMutationDisabled", err)
	}
	if _, statErr := os.Stat(filepath.Dir(missingSource)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("disabled Apply created filesystem artifacts: %v", statErr)
	}

	sourcePath := writeFile(t, "hello world")
	sidecarPath := sourcePath + ".qrp"
	err = Apply(sourcePath, []Patch{{Offset: 6, Old: []byte("world"), New: []byte("WORLD")}}, sidecarPath)
	if !errors.Is(err, ErrMutationDisabled) {
		t.Fatalf("Apply error = %v, want ErrMutationDisabled", err)
	}
	if got := read(t, sourcePath); got != "hello world" {
		t.Fatalf("disabled Apply changed source to %q", got)
	}
	if _, statErr := os.Stat(sidecarPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("disabled Apply created sidecar: %v", statErr)
	}
}

func TestApplyOverwritesBytesAndRemovesSidecar(t *testing.T) {
	path := writeFile(t, "hello world, hello there")
	sidecar := path + ".qrp"
	patches := []Patch{
		{Offset: 6, Old: []byte("world"), New: []byte("WORLD")},
		{Offset: 19, Old: []byte("there"), New: []byte("THERE")},
	}
	if err := applyTransaction(path, patches, sidecar); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := read(t, path); got != "hello WORLD, hello THERE" {
		t.Fatalf("content = %q", got)
	}
	if _, err := os.Stat(sidecar); !os.IsNotExist(err) {
		t.Fatalf("sidecar should be removed, stat err = %v", err)
	}
}

func TestApplyUnsortedPatches(t *testing.T) {
	path := writeFile(t, "0123456789")
	patches := []Patch{
		{Offset: 8, Old: []byte("89"), New: []byte("YZ")},
		{Offset: 0, Old: []byte("01"), New: []byte("AB")},
	}
	if err := applyTransaction(path, patches, path+".qrp"); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := read(t, path); got != "AB234567YZ" {
		t.Fatalf("content = %q", got)
	}
}

func TestApplyRejectsLengthChange(t *testing.T) {
	path := writeFile(t, "hello world")
	err := applyTransaction(path, []Patch{{Offset: 6, Old: []byte("world"), New: []byte("worlds")}}, path+".qrp")
	if err != ErrNotLengthPreserving {
		t.Fatalf("want ErrNotLengthPreserving, got %v", err)
	}
	if got := read(t, path); got != "hello world" {
		t.Fatalf("file must be unchanged, got %q", got)
	}
}

func TestApplyRejectsDrift(t *testing.T) {
	path := writeFile(t, "hello world")
	err := applyTransaction(path, []Patch{{Offset: 6, Old: []byte("WORLD"), New: []byte("xxxxx")}}, path+".qrp")
	if err != ErrDrift {
		t.Fatalf("want ErrDrift, got %v", err)
	}
	if got := read(t, path); got != "hello world" {
		t.Fatalf("file must be unchanged, got %q", got)
	}
	if _, err := os.Stat(path + ".qrp"); !os.IsNotExist(err) {
		t.Fatalf("no sidecar should remain on drift")
	}
}

func TestApplyRejectsOutOfRange(t *testing.T) {
	path := writeFile(t, "short")
	err := applyTransaction(path, []Patch{{Offset: 4, Old: []byte("tXXXX"), New: []byte("xXXXX")}}, path+".qrp")
	if err != ErrOutOfRange {
		t.Fatalf("want ErrOutOfRange, got %v", err)
	}
}

func TestApplyRejectsOverlap(t *testing.T) {
	path := writeFile(t, "abcdefgh")
	patches := []Patch{
		{Offset: 2, Old: []byte("cde"), New: []byte("CDE")},
		{Offset: 3, Old: []byte("de"), New: []byte("DE")},
	}
	if err := applyTransaction(path, patches, path+".qrp"); err != ErrOverlap {
		t.Fatalf("want ErrOverlap, got %v", err)
	}
}

func TestApplyRefusesPreExistingSidecarWithoutChangingFiles(t *testing.T) {
	path := writeFile(t, "SOURCE-BYTES")
	sidecar := path + ".qrp"
	const sentinel = "EXISTING-RECOVERY-DATA"
	if err := os.WriteFile(sidecar, []byte(sentinel), 0o600); err != nil {
		t.Fatal(err)
	}

	err := applyTransaction(path, []Patch{{Offset: 0, Old: []byte("SOURCE"), New: []byte("CHANGE")}}, sidecar)
	if !errors.Is(err, ErrSidecarExists) {
		t.Fatalf("want ErrSidecarExists, got %v", err)
	}
	if got := read(t, path); got != "SOURCE-BYTES" {
		t.Fatalf("source changed on rejection: %q", got)
	}
	if got := read(t, sidecar); got != sentinel {
		t.Fatalf("pre-existing sidecar changed on rejection: %q", got)
	}
}

func TestApplyRefusesSourceAsSidecarWithoutChangingSource(t *testing.T) {
	path := writeFile(t, "SOURCE-BYTES")

	err := applyTransaction(path, []Patch{{Offset: 0, Old: []byte("SOURCE"), New: []byte("CHANGE")}}, path)
	if !errors.Is(err, ErrSidecarExists) {
		t.Fatalf("want ErrSidecarExists, got %v", err)
	}
	if got := read(t, path); got != "SOURCE-BYTES" {
		t.Fatalf("source changed on rejection: %q", got)
	}
}

func TestApplyRefusesHardLinkSidecarWithoutChangingTargets(t *testing.T) {
	path := writeFile(t, "SOURCE-BYTES")
	dir := filepath.Dir(path)
	patches := []Patch{{Offset: 0, Old: []byte("SOURCE"), New: []byte("CHANGE")}}

	t.Run("link to source", func(t *testing.T) {
		sidecar := filepath.Join(dir, "source-link.qrp")
		if err := os.Link(path, sidecar); err != nil {
			t.Skipf("hard links are unavailable: %v", err)
		}
		err := applyTransaction(path, patches, sidecar)
		if !errors.Is(err, ErrSidecarExists) {
			t.Fatalf("want ErrSidecarExists, got %v", err)
		}
		if got := read(t, path); got != "SOURCE-BYTES" {
			t.Fatalf("source changed on rejection: %q", got)
		}
		if got := read(t, sidecar); got != "SOURCE-BYTES" {
			t.Fatalf("hard-link target changed on rejection: %q", got)
		}
	})

	t.Run("link to sentinel", func(t *testing.T) {
		sentinelPath := filepath.Join(dir, "sentinel.bin")
		sidecar := filepath.Join(dir, "sentinel-link.qrp")
		const sentinel = "DO-NOT-CHANGE"
		if err := os.WriteFile(sentinelPath, []byte(sentinel), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(sentinelPath, sidecar); err != nil {
			t.Skipf("hard links are unavailable: %v", err)
		}
		err := applyTransaction(path, patches, sidecar)
		if !errors.Is(err, ErrSidecarExists) {
			t.Fatalf("want ErrSidecarExists, got %v", err)
		}
		if got := read(t, path); got != "SOURCE-BYTES" {
			t.Fatalf("source changed on rejection: %q", got)
		}
		if got := read(t, sentinelPath); got != sentinel {
			t.Fatalf("sentinel changed on rejection: %q", got)
		}
		if got := read(t, sidecar); got != sentinel {
			t.Fatalf("hard-link alias changed on rejection: %q", got)
		}
	})
}

func TestApplyRefusesSymlinkSidecarWithoutChangingTargets(t *testing.T) {
	path := writeFile(t, "SOURCE-BYTES")
	dir := filepath.Dir(path)
	sentinelPath := filepath.Join(dir, "sentinel.bin")
	sidecar := filepath.Join(dir, "sentinel-link.qrp")
	const sentinel = "DO-NOT-CHANGE"
	if err := os.WriteFile(sentinelPath, []byte(sentinel), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(sentinelPath, sidecar); err != nil {
		t.Skipf("symbolic links are unavailable: %v", err)
	}

	err := applyTransaction(path, []Patch{{Offset: 0, Old: []byte("SOURCE"), New: []byte("CHANGE")}}, sidecar)
	if !errors.Is(err, ErrSidecarExists) {
		t.Fatalf("want ErrSidecarExists, got %v", err)
	}
	if got := read(t, path); got != "SOURCE-BYTES" {
		t.Fatalf("source changed on rejection: %q", got)
	}
	if got := read(t, sentinelPath); got != sentinel {
		t.Fatalf("sentinel changed on rejection: %q", got)
	}
	if got := read(t, sidecar); got != sentinel {
		t.Fatalf("symlink target changed on rejection: %q", got)
	}
}

func TestSidecarCleanupRefusesSubstitutedPath(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.bin")
	if err := os.WriteFile(sourcePath, []byte("SOURCE-BYTES"), 0o600); err != nil {
		t.Fatal(err)
	}
	sidecarPath := filepath.Join(dir, "operation.qrp")
	movedPath := filepath.Join(dir, "owned-but-moved.qrp")
	const sentinel = "REPLACEMENT-MUST-SURVIVE"

	transaction := createTestSidecar(t, sidecarPath, sourcePath, []Patch{{Offset: 0, Old: []byte("SOURCE"), New: []byte("CHANGE")}})
	if err := os.Rename(sidecarPath, movedPath); err != nil {
		transaction.close()
		t.Fatalf("rename owned sidecar: %v", err)
	}
	if err := os.WriteFile(sidecarPath, []byte(sentinel), 0o600); err != nil {
		transaction.close()
		t.Fatal(err)
	}

	err := transaction.discard()
	if !errors.Is(err, ErrUnsafeSidecar) {
		t.Fatalf("want ErrUnsafeSidecar, got %v", err)
	}
	if got := read(t, sidecarPath); got != sentinel {
		t.Fatalf("substituted path was changed or removed: %q", got)
	}
	if _, err := os.Stat(movedPath); err != nil {
		t.Fatalf("owned sidecar should remain at its moved path: %v", err)
	}
}

func TestRecoverRollsBackPendingSidecar(t *testing.T) {
	// Simulate a crash: write the pending sidecar (original bytes), then apply
	// the byte changes directly, but never commit/remove the sidecar.
	path := writeFile(t, "hello world")
	sidecar := path + ".qrp"
	patches := []Patch{{Offset: 6, Old: []byte("world"), New: []byte("WORLD")}}
	transaction := createTestSidecar(t, sidecar, path, patches)
	if err := transaction.close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("hello WORLD"), 0o644); err != nil {
		t.Fatal(err)
	}

	rolledBack, err := Recover(path, sidecar)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if !rolledBack {
		t.Fatal("expected rollback")
	}
	if got := read(t, path); got != "hello world" {
		t.Fatalf("rollback content = %q", got)
	}
	if _, err := os.Stat(sidecar); !os.IsNotExist(err) {
		t.Fatal("sidecar should be removed after recover")
	}
}

func TestRecoverCommittedJustDeletes(t *testing.T) {
	path := writeFile(t, "hello WORLD") // already-applied state
	sidecar := path + ".qrp"
	transaction := createTestSidecar(t, sidecar, path, []Patch{{Offset: 6, Old: []byte("world"), New: []byte("WORLD")}})
	if err := transaction.markCommitted(); err != nil {
		transaction.close()
		t.Fatal(err)
	}
	if err := transaction.close(); err != nil {
		t.Fatal(err)
	}
	rolledBack, err := Recover(path, sidecar)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if rolledBack {
		t.Fatal("committed sidecar must NOT roll back")
	}
	if got := read(t, path); got != "hello WORLD" {
		t.Fatalf("content must be unchanged, got %q", got)
	}
	if _, err := os.Stat(sidecar); !os.IsNotExist(err) {
		t.Fatal("sidecar should be removed")
	}
}

func TestRecoverNoSidecarIsNoop(t *testing.T) {
	path := writeFile(t, "data")
	rolledBack, err := Recover(path, path+".qrp")
	if err != nil || rolledBack {
		t.Fatalf("want no-op, got rolledBack=%v err=%v", rolledBack, err)
	}
}

func TestRecoverCorruptSidecarIsPreserved(t *testing.T) {
	path := writeFile(t, "data")
	sidecar := path + ".qrp"
	if err := os.WriteFile(sidecar, []byte("not a real sidecar"), 0o600); err != nil {
		t.Fatal(err)
	}
	rolledBack, err := Recover(path, sidecar)
	if !errors.Is(err, ErrInvalidSidecar) {
		t.Fatalf("Recover error = %v, want ErrInvalidSidecar", err)
	}
	if rolledBack {
		t.Fatal("corrupt sidecar must not roll back")
	}
	if got := read(t, sidecar); got != "not a real sidecar" {
		t.Fatalf("corrupt recovery evidence changed: %q", got)
	}
	if got := read(t, path); got != "data" {
		t.Fatalf("file must be untouched, got %q", got)
	}
}

func TestApplyThenReopenCleanState(t *testing.T) {
	// After a successful Apply, Recover must be a no-op (no sidecar left).
	path := writeFile(t, "aaaa bbbb")
	sidecar := path + ".qrp"
	if err := applyTransaction(path, []Patch{{Offset: 5, Old: []byte("bbbb"), New: []byte("BBBB")}}, sidecar); err != nil {
		t.Fatal(err)
	}
	rolledBack, err := Recover(path, sidecar)
	if err != nil || rolledBack {
		t.Fatalf("post-apply recover should be no-op, got rolledBack=%v err=%v", rolledBack, err)
	}
	if got := read(t, path); got != "aaaa BBBB" {
		t.Fatalf("content = %q", got)
	}
}

func TestRecoverRejectsMalformedSidecarsWithoutWrites(t *testing.T) {
	tests := []struct {
		name   string
		mutate func([]byte) []byte
	}{
		{
			name: "bad flag",
			mutate: func(data []byte) []byte {
				data[8] = 7
				return data
			},
		},
		{
			name: "negative count",
			mutate: func(data []byte) []byte {
				binary.LittleEndian.PutUint64(data[80:88], math.MaxUint64)
				return data
			},
		},
		{
			name: "negative entry length",
			mutate: func(data []byte) []byte {
				binary.LittleEndian.PutUint64(data[104:112], math.MaxUint64)
				return data
			},
		},
		{
			name: "negative offset",
			mutate: func(data []byte) []byte {
				binary.LittleEndian.PutUint64(data[96:104], math.MaxUint64)
				return data
			},
		},
		{
			name: "beyond eof",
			mutate: func(data []byte) []byte {
				binary.LittleEndian.PutUint64(data[96:104], 8)
				return data
			},
		},
		{
			name: "unsupported identity",
			mutate: func(data []byte) []byte {
				data[56] = 99
				return data
			},
		},
		{
			name: "reserved bits",
			mutate: func(data []byte) []byte {
				data[10] = 1
				return data
			},
		},
		{
			name: "checksum",
			mutate: func(data []byte) []byte {
				data[144] ^= 0xff
				return data
			},
		},
		{
			name: "trailing data",
			mutate: func(data []byte) []byte {
				return append(data, 0x7f)
			},
		},
		{
			name: "truncated checksum",
			mutate: func(data []byte) []byte {
				return data[:len(data)-1]
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := writeFile(t, "hello world")
			sidecarPath := path + ".qrp"
			transaction := createTestSidecar(t, sidecarPath, path, []Patch{{Offset: 6, Old: []byte("world"), New: []byte("WORLD")}})
			if err := transaction.close(); err != nil {
				t.Fatal(err)
			}
			artifact, err := os.ReadFile(sidecarPath)
			if err != nil {
				t.Fatal(err)
			}
			artifact = test.mutate(artifact)
			if err := os.WriteFile(sidecarPath, artifact, 0o600); err != nil {
				t.Fatal(err)
			}

			rolledBack, err := Recover(path, sidecarPath)
			if rolledBack || !errors.Is(err, ErrInvalidSidecar) {
				t.Fatalf("Recover = rolledBack %v, error %v; want false/ErrInvalidSidecar", rolledBack, err)
			}
			if got := read(t, path); got != "hello world" {
				t.Fatalf("malformed artifact changed source: %q", got)
			}
			preserved, readErr := os.ReadFile(sidecarPath)
			if readErr != nil {
				t.Fatalf("recovery evidence was not preserved: %v", readErr)
			}
			if string(preserved) != string(artifact) {
				t.Fatal("recovery evidence changed on parser rejection")
			}
		})
	}
}

func TestRecoverRejectsOversizedArtifactBeforeOpeningSource(t *testing.T) {
	dir := t.TempDir()
	sidecarPath := filepath.Join(dir, "oversized.qrp")
	f, err := os.OpenFile(sidecarPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	oversized := maxSidecarArtifactBytes + 1
	if err := f.Truncate(oversized); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	missingSource := filepath.Join(dir, "source-does-not-exist.bin")
	if rolledBack, err := Recover(missingSource, sidecarPath); rolledBack || !errors.Is(err, ErrInvalidSidecar) {
		t.Fatalf("Recover oversized = %v, %v; want false/ErrInvalidSidecar", rolledBack, err)
	}
	if info, err := os.Stat(sidecarPath); err != nil || info.Size() != oversized {
		t.Fatalf("oversized evidence was not preserved: size=%v err=%v", info, err)
	}
}

func TestRecoverRejectsOverlappingEntriesWithoutWrites(t *testing.T) {
	path := writeFile(t, "abcdefgh")
	sidecarPath := path + ".qrp"
	transaction := createTestSidecar(t, sidecarPath, path, []Patch{
		{Offset: 0, Old: []byte("ab"), New: []byte("AB")},
		{Offset: 4, Old: []byte("ef"), New: []byte("EF")},
	})
	if err := transaction.close(); err != nil {
		t.Fatal(err)
	}
	artifact, err := os.ReadFile(sidecarPath)
	if err != nil {
		t.Fatal(err)
	}
	// Entry two begins after entry-one metadata (48 bytes) and payload (2).
	binary.LittleEndian.PutUint64(artifact[146:154], 1)
	if err := os.WriteFile(sidecarPath, artifact, 0o600); err != nil {
		t.Fatal(err)
	}
	if rolledBack, err := Recover(path, sidecarPath); rolledBack || !errors.Is(err, ErrInvalidSidecar) {
		t.Fatalf("Recover = %v, %v", rolledBack, err)
	}
	if got := read(t, path); got != "abcdefgh" {
		t.Fatalf("overlapping artifact changed source: %q", got)
	}
	if _, err := os.Stat(sidecarPath); err != nil {
		t.Fatalf("overlapping recovery evidence was not preserved: %v", err)
	}
}

func TestRecoverRejectsWrongSourceIdentity(t *testing.T) {
	dir := t.TempDir()
	sourceA := filepath.Join(dir, "a.bin")
	sourceB := filepath.Join(dir, "b.bin")
	for _, path := range []string{sourceA, sourceB} {
		if err := os.WriteFile(path, []byte("hello world"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	sidecarPath := sourceA + ".qrp"
	transaction := createTestSidecar(t, sidecarPath, sourceA, []Patch{{Offset: 6, Old: []byte("world"), New: []byte("WORLD")}})
	if err := transaction.close(); err != nil {
		t.Fatal(err)
	}

	if rolledBack, err := Recover(sourceB, sidecarPath); rolledBack || !errors.Is(err, ErrSourceMismatch) {
		t.Fatalf("Recover wrong inode = %v, %v; want false/ErrSourceMismatch", rolledBack, err)
	}
	if got := read(t, sourceB); got != "hello world" {
		t.Fatalf("wrong-inode source changed: %q", got)
	}
	if _, err := os.Stat(sidecarPath); err != nil {
		t.Fatalf("wrong-inode evidence was not preserved: %v", err)
	}
}

func TestRecoverRefusesSymlinkArtifactWithoutFollowingTarget(t *testing.T) {
	path := writeFile(t, "source bytes")
	dir := filepath.Dir(path)
	target := filepath.Join(dir, "target.bin")
	sidecarPath := path + ".qrp"
	const sentinel = "must not be read as recovery instructions"
	if err := os.WriteFile(target, []byte(sentinel), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, sidecarPath); err != nil {
		t.Skipf("symbolic links are unavailable: %v", err)
	}

	if rolledBack, err := Recover(path, sidecarPath); rolledBack || !errors.Is(err, ErrUnsafeSidecar) {
		t.Fatalf("Recover symlink = %v, %v; want false/ErrUnsafeSidecar", rolledBack, err)
	}
	if got := read(t, path); got != "source bytes" {
		t.Fatalf("symlink recovery changed source: %q", got)
	}
	if got := read(t, target); got != sentinel {
		t.Fatalf("symlink target changed: %q", got)
	}
}

func TestRecoverPreflightsEveryExtentBeforeRollback(t *testing.T) {
	path := writeFile(t, "abcdefgh")
	sidecarPath := path + ".qrp"
	transaction := createTestSidecar(t, sidecarPath, path, []Patch{
		{Offset: 0, Old: []byte("ab"), New: []byte("AB")},
		{Offset: 4, Old: []byte("ef"), New: []byte("EF")},
	})
	if err := transaction.close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("ABcdZZgh"), 0o600); err != nil {
		t.Fatal(err)
	}

	if rolledBack, err := Recover(path, sidecarPath); rolledBack || !errors.Is(err, ErrRecoveryDrift) {
		t.Fatalf("Recover = %v, %v; want false/ErrRecoveryDrift", rolledBack, err)
	}
	if got := read(t, path); got != "ABcdZZgh" {
		t.Fatalf("preflight failure performed a partial replay: %q", got)
	}
	if _, err := os.Stat(sidecarPath); err != nil {
		t.Fatalf("drifted evidence was not preserved: %v", err)
	}
}

func TestRecoverCommittedRecordRequiresPatchedBytes(t *testing.T) {
	path := writeFile(t, "hello world")
	sidecarPath := path + ".qrp"
	transaction := createTestSidecar(t, sidecarPath, path, []Patch{{Offset: 6, Old: []byte("world"), New: []byte("WORLD")}})
	if err := transaction.markCommitted(); err != nil {
		t.Fatal(err)
	}
	if err := transaction.close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("hello THERE"), 0o600); err != nil {
		t.Fatal(err)
	}

	if rolledBack, err := Recover(path, sidecarPath); rolledBack || !errors.Is(err, ErrRecoveryDrift) {
		t.Fatalf("Recover = %v, %v; want false/ErrRecoveryDrift", rolledBack, err)
	}
	if got := read(t, path); got != "hello THERE" {
		t.Fatalf("committed drift changed source: %q", got)
	}
	if _, err := os.Stat(sidecarPath); err != nil {
		t.Fatalf("committed drift evidence was not preserved: %v", err)
	}
}

func TestInspectIsReadOnlyAndReportsPartialTransaction(t *testing.T) {
	path := writeFile(t, "abcdefgh")
	sidecarPath := path + ".qrp"
	transaction := createTestSidecar(t, sidecarPath, path, []Patch{
		{Offset: 0, Old: []byte("ab"), New: []byte("AB")},
		{Offset: 4, Old: []byte("ef"), New: []byte("EF")},
	})
	if err := transaction.close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("ABcdefgh"), 0o600); err != nil {
		t.Fatal(err)
	}
	artifactBefore, err := os.ReadFile(sidecarPath)
	if err != nil {
		t.Fatal(err)
	}

	state, err := Inspect(path, sidecarPath)
	if err != nil {
		t.Fatal(err)
	}
	if state.Phase != "pending" || state.SourceState != "partial" || !state.SourceMatches || !state.CanRollback || state.CanClear {
		t.Fatalf("unexpected inspection: %+v", state)
	}
	if got := read(t, path); got != "ABcdefgh" {
		t.Fatalf("inspection changed source: %q", got)
	}
	artifactAfter, err := os.ReadFile(sidecarPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(artifactAfter) != string(artifactBefore) {
		t.Fatal("inspection changed recovery evidence")
	}
}

func TestApplyFailureRollsBackSynchronously(t *testing.T) {
	originalWriteAt := writeAtSource
	t.Cleanup(func() { writeAtSource = originalWriteAt })
	injected := errors.New("injected second write failure")
	calls := 0
	writeAtSource = func(f *os.File, data []byte, offset int64) error {
		calls++
		if calls == 2 {
			return injected
		}
		return exactWriteAt(f, data, offset)
	}

	path := writeFile(t, "abcdefgh")
	sidecarPath := path + ".qrp"
	err := applyTransaction(path, []Patch{
		{Offset: 0, Old: []byte("ab"), New: []byte("AB")},
		{Offset: 4, Old: []byte("ef"), New: []byte("EF")},
	}, sidecarPath)
	if !errors.Is(err, injected) {
		t.Fatalf("Apply error = %v, want injected error", err)
	}
	if got := read(t, path); got != "abcdefgh" {
		t.Fatalf("failed operation was not rolled back: %q", got)
	}
	if _, err := os.Stat(sidecarPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("verified rollback sidecar remains: %v", err)
	}
}

func TestApplyRollbackFailureRetainsActionableEvidence(t *testing.T) {
	originalWriteAt := writeAtSource
	t.Cleanup(func() { writeAtSource = originalWriteAt })
	operationErr := errors.New("injected operation failure")
	rollbackErr := errors.New("injected rollback failure")
	calls := 0
	writeAtSource = func(f *os.File, data []byte, offset int64) error {
		calls++
		switch calls {
		case 2:
			return operationErr
		case 3:
			return rollbackErr
		default:
			return exactWriteAt(f, data, offset)
		}
	}

	path := writeFile(t, "abcdefgh")
	sidecarPath := path + ".qrp"
	err := applyTransaction(path, []Patch{
		{Offset: 0, Old: []byte("ab"), New: []byte("AB")},
		{Offset: 4, Old: []byte("ef"), New: []byte("EF")},
	}, sidecarPath)
	var inconsistent *InconsistentStateError
	if !errors.As(err, &inconsistent) || !errors.Is(err, operationErr) || !errors.Is(inconsistent.Rollback, rollbackErr) {
		t.Fatalf("Apply error = %v, want InconsistentStateError", err)
	}
	if got := read(t, path); got != "ABcdefgh" {
		t.Fatalf("unexpected partial source state: %q", got)
	}
	if _, err := os.Stat(sidecarPath); err != nil {
		t.Fatalf("rollback failure did not retain evidence: %v", err)
	}
}

func TestApplyCommitFailureRollsBackSynchronously(t *testing.T) {
	originalWriteAt := writeAtSidecar
	t.Cleanup(func() { writeAtSidecar = originalWriteAt })
	injected := errors.New("injected commit marker failure")
	writeAtSidecar = func(*os.File, []byte, int64) error { return injected }

	path := writeFile(t, "hello world")
	sidecarPath := path + ".qrp"
	err := applyTransaction(path, []Patch{{Offset: 6, Old: []byte("world"), New: []byte("WORLD")}}, sidecarPath)
	if !errors.Is(err, injected) {
		t.Fatalf("Apply error = %v, want commit error", err)
	}
	if got := read(t, path); got != "hello world" {
		t.Fatalf("commit failure was not rolled back: %q", got)
	}
	if _, err := os.Stat(sidecarPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("verified rollback sidecar remains: %v", err)
	}
}

func TestApplySyncFailureRollsBackSynchronously(t *testing.T) {
	originalSync := syncSource
	t.Cleanup(func() { syncSource = originalSync })
	injected := errors.New("injected source sync failure")
	calls := 0
	syncSource = func(f *os.File) error {
		calls++
		if calls == 1 {
			return injected
		}
		return f.Sync()
	}

	path := writeFile(t, "hello world")
	sidecarPath := path + ".qrp"
	err := applyTransaction(path, []Patch{{Offset: 6, Old: []byte("world"), New: []byte("WORLD")}}, sidecarPath)
	if !errors.Is(err, injected) {
		t.Fatalf("Apply error = %v, want sync error", err)
	}
	if got := read(t, path); got != "hello world" {
		t.Fatalf("sync failure was not rolled back: %q", got)
	}
}
