package fileio

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

type fileInfoWithMode struct {
	os.FileInfo
	mode os.FileMode
}

func (info fileInfoWithMode) Mode() os.FileMode { return info.mode }

func TestWriteFileAtomicPublishesViaTempAndRemovesTemp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.txt")

	summary, err := WriteFileAtomic(path, []byte("hello"), AtomicWriteOptions{Mode: 0o600})
	if err != nil {
		t.Fatal(err)
	}
	if summary.BytesWritten != int64(len("hello")) {
		t.Fatalf("BytesWritten = %d", summary.BytesWritten)
	}
	if _, err := os.Stat(summary.TempPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temp stat err = %v, want not exist", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello" {
		t.Fatalf("output = %q", got)
	}
}

func TestWriteFileAtomicRefusesExistingOutputByDefault(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.txt")
	if err := os.WriteFile(path, []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}

	summary, err := WriteFileAtomic(path, []byte("new"), AtomicWriteOptions{})
	if !errors.Is(err, ErrExists) {
		t.Fatalf("err = %v, want ErrExists", err)
	}
	if _, err := os.Stat(summary.TempPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temp stat err = %v, want not exist", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "existing" {
		t.Fatalf("existing output changed to %q", got)
	}
}

func TestWriteFileAtomicOverwritesOnlyWhenExplicit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.txt")
	if err := os.WriteFile(path, []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}

	summary, err := WriteFileAtomic(path, []byte("new"), AtomicWriteOptions{Overwrite: true})
	if err != nil {
		t.Fatal(err)
	}
	if !summary.Overwritten {
		t.Fatal("Overwritten = false, want true")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new" {
		t.Fatalf("output = %q, want new", got)
	}
}

func TestWriteFileAtomicOverwritePreservesExistingMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX execute permission preservation is not observable on Windows")
	}
	path := filepath.Join(t.TempDir(), "script.sh")
	if err := os.WriteFile(path, []byte("old"), 0o700); err != nil {
		t.Fatal(err)
	}

	if _, err := WriteFileAtomic(path, []byte("new"), AtomicWriteOptions{Mode: 0o600, Overwrite: true}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Fatalf("mode = %o, want preserved 700", got)
	}
}

func TestWriteFileAtomicOverwriteCreatesPrivateTempThenPreservesZeroMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "acl-readable-output.txt")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	realInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	originalStat := statPath
	originalOpen := openPath
	originalChmod := chmodOpenFile
	var creationMode os.FileMode
	var finalMode os.FileMode
	statPath = func(candidate string) (os.FileInfo, error) {
		info, statErr := originalStat(candidate)
		if candidate == path && statErr == nil {
			return fileInfoWithMode{FileInfo: realInfo, mode: 0}, nil
		}
		return info, statErr
	}
	openPath = func(candidate string, flags int, mode os.FileMode) (*os.File, error) {
		creationMode = mode
		return originalOpen(candidate, flags, mode)
	}
	chmodOpenFile = func(file *os.File, mode os.FileMode) error {
		finalMode = mode
		// Keep the fixture readable so recovery/finalization can verify the
		// requested zero mode without requiring root or an ACL fixture.
		return originalChmod(file, 0o600)
	}
	t.Cleanup(func() {
		statPath = originalStat
		openPath = originalOpen
		chmodOpenFile = originalChmod
	})

	if _, err := WriteFileAtomic(path, []byte("new"), AtomicWriteOptions{Overwrite: true}); err != nil {
		t.Fatal(err)
	}
	if creationMode.Perm() != 0o600 {
		t.Fatalf("temporary creation mode = %03o, want private 600", creationMode.Perm())
	}
	if finalMode.Perm() != 0 {
		t.Fatalf("requested final mode = %03o, want preserved 000", finalMode.Perm())
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "new" {
		t.Fatalf("destination = %q, %v; want new generation", got, err)
	}
}

func TestWriteFileAtomicChmodFailurePreservesDestinationAndCleansTemp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "output.txt")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	originalChmod := chmodOpenFile
	sentinel := errors.New("chmod failed")
	chmodOpenFile = func(*os.File, os.FileMode) error { return sentinel }
	t.Cleanup(func() { chmodOpenFile = originalChmod })

	summary, err := WriteFileAtomic(path, []byte("new"), AtomicWriteOptions{Overwrite: true})
	if !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want chmod sentinel", err)
	}
	if got, readErr := os.ReadFile(path); readErr != nil || string(got) != "old" {
		t.Fatalf("destination changed: %q, %v", got, readErr)
	}
	after, statErr := os.Stat(path)
	if statErr != nil {
		t.Fatalf("stat destination after chmod failure: %v", statErr)
	}
	if after.Mode().Perm() != before.Mode().Perm() {
		t.Fatalf("destination mode changed: before=%03o after=%03o", before.Mode().Perm(), after.Mode().Perm())
	}
	for _, artifact := range []string{summary.TempPath, summary.BackupPath, summary.JournalPath} {
		if artifact != "" {
			if _, statErr := os.Lstat(artifact); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("failure artifact remains at %q: %v", artifact, statErr)
			}
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != filepath.Base(path) {
		t.Fatalf("chmod failure created adjacent artifacts: %v, %v", entries, err)
	}
}

func TestWriteFileAtomicReportsIncompleteTempCleanupFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "output.txt")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}

	originalChmod := chmodOpenFile
	originalRemove := removePath
	writeSentinel := errors.New("chmod failed")
	cleanupSentinel := errors.New("temp cleanup failed")
	chmodOpenFile = func(*os.File, os.FileMode) error { return writeSentinel }
	removePath = func(candidate string) error {
		if strings.HasPrefix(candidate, path+defaultTempSuffix+".") {
			return cleanupSentinel
		}
		return os.Remove(candidate)
	}
	t.Cleanup(func() {
		chmodOpenFile = originalChmod
		removePath = originalRemove
	})

	summary, err := WriteFileAtomic(path, []byte("new"), AtomicWriteOptions{Overwrite: true})
	if !errors.Is(err, writeSentinel) || !errors.Is(err, cleanupSentinel) {
		t.Fatalf("error = %v, want write and cleanup failures", err)
	}
	if !strings.Contains(err.Error(), summary.TempPath) {
		t.Fatalf("error %q does not identify retained temporary output %q", err, summary.TempPath)
	}
	if got, readErr := os.ReadFile(path); readErr != nil || string(got) != "old" {
		t.Fatalf("destination changed: %q, %v", got, readErr)
	}
	if got, readErr := os.ReadFile(summary.TempPath); readErr != nil || string(got) != "new" {
		t.Fatalf("retained incomplete temp = %q, %v; want complete written bytes", got, readErr)
	}
	for _, artifact := range []string{summary.BackupPath, summary.JournalPath} {
		if artifact != "" {
			if _, statErr := os.Lstat(artifact); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("unexpected recovery artifact at %q: %v", artifact, statErr)
			}
		}
	}
}

func TestWriteFileAtomicPreservesUnownedLegacyTempWithoutBlocking(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.txt")
	tempPath := path + ".quarry.tmp"
	if err := os.WriteFile(tempPath, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}

	summary, err := WriteFileAtomic(path, []byte("new"), AtomicWriteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if summary.TempPath == tempPath {
		t.Fatal("new write reused the unowned legacy temp path")
	}
	if got, err := os.ReadFile(tempPath); err != nil || string(got) != "partial" {
		t.Fatalf("temp changed: %q, %v", got, err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "new" {
		t.Fatalf("output = %q, %v", got, err)
	}
}

func TestWriteFileAtomicPreservesUnownedLegacyBackupWithoutBlocking(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.txt")
	backupPath := path + ".quarry.overwrite.bak"
	if err := os.WriteFile(path, []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(backupPath, []byte("backup"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := WriteFileAtomic(path, []byte("new"), AtomicWriteOptions{Overwrite: true})
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new" {
		t.Fatalf("output = %q, want new", got)
	}
	if got, err := os.ReadFile(backupPath); err != nil || string(got) != "backup" {
		t.Fatalf("unowned legacy backup changed: %q, %v", got, err)
	}
}

func TestWriteFileAtomicRefusesUnjournaledLegacyOverwriteBackup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.txt")
	backupPath := path + ".quarry.overwrite.bak"
	tempPath := path + ".quarry.tmp"
	if err := os.WriteFile(backupPath, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tempPath, []byte("partial-new"), 0o600); err != nil {
		t.Fatal(err)
	}

	summary, err := WriteFileAtomic(path, []byte("new"), AtomicWriteOptions{Overwrite: true})
	if !errors.Is(err, ErrAtomicRecoveryNeedsInspection) {
		t.Fatalf("error = %v, want inspection-needed refusal", err)
	}
	if summary.LegacyBackupRecovered {
		t.Fatal("LegacyBackupRecovered = true; unjournaled backup must never be moved")
	}
	if _, statErr := os.Lstat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("destination was created from untrusted recovery evidence: %v", statErr)
	}
	if got, readErr := os.ReadFile(backupPath); readErr != nil || string(got) != "original" {
		t.Fatalf("legacy backup changed: %q, %v", got, readErr)
	}
	if got, err := os.ReadFile(tempPath); err != nil || string(got) != "partial-new" {
		t.Fatalf("legacy temp changed: %q, %v", got, err)
	}
}

func TestWriteFileAtomicNeverExecutesPreexistingChecksumValidJournal(t *testing.T) {
	for _, overwrite := range []bool{false, true} {
		t.Run(fmt.Sprintf("overwrite_%t", overwrite), func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "target.txt")
			oldBytes := []byte("attacker-selected old generation")
			forgedBytes := []byte("attacker-selected replacement")
			if err := os.WriteFile(path, oldBytes, 0o600); err != nil {
				t.Fatal(err)
			}
			journal := installAtomicRecoveryFixture(t, path, defaultTempSuffix, true, oldBytes, forgedBytes)
			if err := os.WriteFile(journal.TempPath, forgedBytes, 0o600); err != nil {
				t.Fatal(err)
			}
			journalPath := path + atomicWriteJournalSuffix
			journalBytes, err := os.ReadFile(journalPath)
			if err != nil {
				t.Fatal(err)
			}

			summary, err := WriteFileAtomic(path, []byte("requested write"), AtomicWriteOptions{Overwrite: overwrite})
			if !errors.Is(err, ErrAtomicRecoveryNeedsInspection) {
				t.Fatalf("error = %v, want inspection-needed refusal", err)
			}
			if summary.PreviousRecovery == nil || summary.PreviousRecovery.OperationID != journal.OperationID {
				t.Fatalf("summary did not retain pending recovery state: %#v", summary.PreviousRecovery)
			}
			assertOptionalRecoveryArtifact(t, path, oldBytes)
			assertOptionalRecoveryArtifact(t, journal.TempPath, forgedBytes)
			assertOptionalRecoveryArtifact(t, journal.BackupPath, nil)
			assertOptionalRecoveryArtifact(t, journalPath, journalBytes)
		})
	}
}

func TestRecoverOverwriteBackupNeverMovesUnjournaledArtifact(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.txt")
	backupPath := path + ".quarry.overwrite.bak"
	if err := os.WriteFile(backupPath, []byte("untrusted"), 0o600); err != nil {
		t.Fatal(err)
	}

	recovered, err := RecoverOverwriteBackup(path)
	if recovered || !errors.Is(err, ErrAtomicRecoveryNeedsInspection) {
		t.Fatalf("RecoverOverwriteBackup = %v, %v; want false/inspection-needed", recovered, err)
	}
	if _, statErr := os.Lstat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("recovery created destination: %v", statErr)
	}
	if got, readErr := os.ReadFile(backupPath); readErr != nil || string(got) != "untrusted" {
		t.Fatalf("legacy backup changed: %q, %v", got, readErr)
	}
}

func TestRecoverOverwriteBackupNoopsWhenOutputExists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.txt")
	if err := os.WriteFile(path, []byte("current"), 0o600); err != nil {
		t.Fatal(err)
	}

	recovered, err := RecoverOverwriteBackup(path)
	if err != nil {
		t.Fatal(err)
	}
	if recovered {
		t.Fatal("expected no recovery when output exists")
	}
}

func TestWriteFileAtomicNewFileNeverClobbersConcurrentCreator(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.txt")
	restoreBeforeCommit := beforeAtomicNewFileCommit
	beforeAtomicNewFileCommit = func() {
		if err := os.WriteFile(path, []byte("concurrent"), 0o600); err != nil {
			t.Fatalf("create concurrent destination: %v", err)
		}
	}
	t.Cleanup(func() { beforeAtomicNewFileCommit = restoreBeforeCommit })

	summary, err := WriteFileAtomic(path, []byte("quarry"), AtomicWriteOptions{})
	if !errors.Is(err, ErrExists) {
		t.Fatalf("error = %v, want ErrExists", err)
	}
	if got, readErr := os.ReadFile(path); readErr != nil || string(got) != "concurrent" {
		t.Fatalf("concurrent destination changed: %q, %v", got, readErr)
	}
	if summary.JournalPath != path+atomicWriteJournalSuffix {
		t.Fatalf("JournalPath = %q, want derived path", summary.JournalPath)
	}
	if _, statErr := os.Lstat(summary.JournalPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("new-file path unexpectedly created an overwrite journal: %v", statErr)
	}
	if summary.TempPath != "" {
		if _, statErr := os.Lstat(summary.TempPath); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("operation temp remains after no-clobber refusal: %v", statErr)
		}
	}
}

func TestWriteFileAtomicPreservesExistingOutputAndRecoveryEvidenceOnOverwriteRenameFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.txt")
	if err := os.WriteFile(path, []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}

	restoreRename := renamePath
	renamePath = func(oldPath, newPath string) error {
		if strings.HasPrefix(oldPath, path+".quarry.tmp.") && newPath == path {
			return errors.New("publish failed")
		}
		return os.Rename(oldPath, newPath)
	}
	defer func() {
		renamePath = restoreRename
	}()

	summary, err := WriteFileAtomic(path, []byte("new"), AtomicWriteOptions{Overwrite: true})
	if err == nil {
		t.Fatal("expected publish error")
	}
	got, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != "existing" {
		t.Fatalf("output = %q, want restored existing output", got)
	}
	if got, err := os.ReadFile(summary.TempPath); err != nil || string(got) != "new" {
		t.Fatalf("temp should remain for a retry: %q, %v", got, err)
	}
	if got, err := os.ReadFile(summary.BackupPath); err != nil || string(got) != "existing" {
		t.Fatalf("operation backup should preserve the old generation: %q, %v", got, err)
	}
	if _, err := os.Stat(summary.JournalPath); err != nil {
		t.Fatalf("journal should remain for retry: %v", err)
	}
}

func TestWriteFileAtomicRejectsCompetingGenerationBeforeOverwritePublication(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.txt")
	if err := os.WriteFile(path, []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}

	restoreBeforePublish := beforeAtomicOverwritePublish
	beforeAtomicOverwritePublish = func() {
		got, err := os.ReadFile(path)
		if err != nil || string(got) != "existing" {
			t.Fatalf("authoritative path disappeared before publication: %q, %v", got, err)
		}
		competing := filepath.Join(dir, "competing.txt")
		if err := os.WriteFile(competing, []byte("competing"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(competing, path); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { beforeAtomicOverwritePublish = restoreBeforePublish })

	summary, err := WriteFileAtomic(path, []byte("quarry"), AtomicWriteOptions{Overwrite: true})
	if !errors.Is(err, ErrAtomicRecoveryNeedsInspection) {
		t.Fatalf("error = %v, want inspection-needed refusal", err)
	}
	if got, readErr := os.ReadFile(path); readErr != nil || string(got) != "competing" {
		t.Fatalf("competing generation changed: %q, %v", got, readErr)
	}
	if got, readErr := os.ReadFile(summary.BackupPath); readErr != nil || string(got) != "existing" {
		t.Fatalf("old generation backup changed: %q, %v", got, readErr)
	}
	if got, readErr := os.ReadFile(summary.TempPath); readErr != nil || string(got) != "quarry" {
		t.Fatalf("complete new temp changed: %q, %v", got, readErr)
	}
	if _, statErr := os.Lstat(summary.JournalPath); statErr != nil {
		t.Fatalf("journal was not preserved: %v", statErr)
	}
}

func TestWriteFileAtomicOverwriteNeverRenamesAuthoritativePathAway(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.txt")
	if err := os.WriteFile(path, []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}

	restoreRename := renamePath
	renamePath = func(oldPath, newPath string) error {
		if oldPath == path {
			t.Fatalf("overwrite attempted to remove the authoritative path before publication: %q -> %q", oldPath, newPath)
		}
		if newPath == path {
			if _, err := os.Lstat(path); err != nil {
				t.Fatalf("authoritative path missing at atomic replacement point: %v", err)
			}
		}
		return os.Rename(oldPath, newPath)
	}
	t.Cleanup(func() { renamePath = restoreRename })

	if _, err := WriteFileAtomic(path, []byte("new"), AtomicWriteOptions{Overwrite: true}); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "new" {
		t.Fatalf("output = %q, %v", got, err)
	}
}

func TestWriteFileAtomicReportsDirectorySyncFailureAfterPublish(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.txt")
	if err := os.WriteFile(path, []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}

	restoreSyncDir := syncDirPath
	calls := 0
	syncDirPath = func(string) error {
		calls++
		if calls == 2 {
			return errors.New("dir sync failed")
		}
		return nil
	}
	defer func() {
		syncDirPath = restoreSyncDir
	}()

	summary, err := WriteFileAtomic(path, []byte("new"), AtomicWriteOptions{Overwrite: true})
	if err == nil || !strings.Contains(err.Error(), "dir sync failed") {
		t.Fatalf("err = %v, want dir sync failure", err)
	}
	if summary.BytesWritten != int64(len("new")) {
		t.Fatalf("BytesWritten = %d, want %d", summary.BytesWritten, len("new"))
	}
	got, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != "new" {
		t.Fatalf("output = %q, want new", got)
	}
}

func TestWriteFileAtomicSyncsDirectoryAfterBackupRemoval(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.txt")
	if err := os.WriteFile(path, []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}

	restoreSyncDir := syncDirPath
	calls := 0
	syncDirPath = func(string) error {
		calls++
		return nil
	}
	defer func() {
		syncDirPath = restoreSyncDir
	}()

	if _, err := WriteFileAtomic(path, []byte("new"), AtomicWriteOptions{Overwrite: true}); err != nil {
		t.Fatal(err)
	}
	if calls < 2 {
		t.Fatalf("syncDirPath called %d times, want at least publish and backup-removal syncs", calls)
	}
}

func TestWriteFileAtomicPreservesExactPathSpelling(t *testing.T) {
	originalStat := statPath
	defer func() { statPath = originalStat }()
	sentinel := errors.New("stop after path capture")
	var seen string
	statPath = func(path string) (os.FileInfo, error) {
		if seen == "" {
			seen = path
		}
		return nil, sentinel
	}

	const selected = " report.txt"
	summary, err := WriteFileAtomic(selected, []byte("data"), AtomicWriteOptions{})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
	if summary.Path != selected || seen != selected {
		t.Fatalf("summary/path lookup = %q / %q, want exact %q", summary.Path, seen, selected)
	}
}

func TestRecoverOverwriteBackupPreservesExactPathSpelling(t *testing.T) {
	originalStat := statPath
	defer func() { statPath = originalStat }()
	sentinel := errors.New("stop after path capture")
	var seen string
	statPath = func(path string) (os.FileInfo, error) {
		seen = path
		return nil, sentinel
	}

	const selected = " recovery target"
	_, err := RecoverOverwriteBackup(selected)
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
	if seen != selected {
		t.Fatalf("path lookup = %q, want exact %q", seen, selected)
	}
}

func TestWriteFileAtomicRejectsUnsafeDerivedTempPath(t *testing.T) {
	dir := t.TempDir()
	outputPath := filepath.Join(dir, "output.txt")
	victimPath := filepath.Join(dir, "victim.txt")
	if err := os.WriteFile(victimPath, []byte("victim"), 0o600); err != nil {
		t.Fatal(err)
	}
	unsafeSuffix := string(os.PathSeparator) + ".." + string(os.PathSeparator) + filepath.Base(victimPath)
	_, err := WriteFileAtomic(outputPath, []byte("new"), AtomicWriteOptions{TempSuffix: unsafeSuffix})
	if !errors.Is(err, ErrInvalidExactPath) {
		t.Fatalf("error = %v, want ErrInvalidExactPath", err)
	}
	if got, err := os.ReadFile(victimPath); err != nil || string(got) != "victim" {
		t.Fatalf("victim = %q, err %v", got, err)
	}
	if _, err := os.Stat(outputPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected output exists: %v", err)
	}
}
