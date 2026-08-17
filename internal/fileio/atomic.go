package fileio

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const defaultTempSuffix = ".quarry.tmp"

var (
	statPath           = os.Stat
	openPath           = os.OpenFile
	chmodOpenFile      = func(file *os.File, mode os.FileMode) error { return file.Chmod(mode) }
	linkPath           = os.Link
	renamePath         = os.Rename
	removePath         = os.Remove
	chmodPath          = os.Chmod
	syncDirPath        = syncDirectory
	ErrExists          = errors.New("output file already exists")
	ErrTempExists      = errors.New("temporary output file already exists")
	ErrBackupExists    = errors.New("overwrite backup file already exists")
	ErrBackupRecovered = errors.New("orphaned overwrite backup recovered")
)

// beforeAtomicNewFileCommit is a deterministic test seam for a concurrent
// creator that appears after Quarry has finished its private temporary output
// but before the no-clobber publication point.
var beforeAtomicNewFileCommit = func() {}

// These overwrite seams let tests coordinate independent Quarry processes at
// the two ownership boundaries. Production leaves them as no-ops.
var (
	beforeAtomicOverwriteJournal      = func() {}
	afterAtomicOverwriteJournal       = func() {}
	beforeAtomicOverwritePublish      = func() {}
	afterAtomicOverwritePublish       = func() {}
	afterAtomicOverwriteBackupRemoval = func() {}
)

type AtomicWriteOptions struct {
	Mode       os.FileMode
	Overwrite  bool
	TempSuffix string
}

type AtomicWriteSummary struct {
	Path                  string
	TempPath              string
	BackupPath            string
	JournalPath           string
	BytesWritten          int64
	Overwritten           bool
	LegacyBackupRecovered bool
	PreviousRecovery      *AtomicRecoveryState
}

// WriteFileAtomic writes data to an exclusive temp file and publishes it with a
// final rename. The default policy refuses to overwrite an existing output.
func WriteFileAtomic(path string, data []byte, opts AtomicWriteOptions) (AtomicWriteSummary, error) {
	atomicWriteMu.Lock()
	defer atomicWriteMu.Unlock()
	return writeFileAtomicUnlocked(path, data, opts)
}

func writeFileAtomicUnlocked(path string, data []byte, opts AtomicWriteOptions) (AtomicWriteSummary, error) {
	if path == "" {
		return AtomicWriteSummary{}, errors.New("output path is required")
	}
	if err := ValidateExactOutputPath(path); err != nil {
		return AtomicWriteSummary{}, err
	}
	suffix := opts.TempSuffix
	if suffix == "" {
		suffix = defaultTempSuffix
	}
	mode := opts.Mode.Perm()
	if mode == 0 {
		// Atomic outputs may contain transformed source data, settings, or
		// recovery metadata. A caller that wants broader access must request it
		// explicitly; the shared fallback must never depend on a permissive
		// process umask.
		mode = 0o600
	}

	summary := AtomicWriteSummary{
		Path:        path,
		JournalPath: path + atomicWriteJournalSuffix,
	}
	legacyTempPath := path + suffix
	legacyBackupPath := path + ".quarry.overwrite.bak"
	if err := ValidateExactOutputPath(legacyTempPath); err != nil {
		return summary, err
	}
	if err := ValidateExactOutputPath(legacyBackupPath); err != nil {
		return summary, err
	}

	previousRecovery, err := inspectAtomicWriteRecoveryUnlocked(path)
	if err != nil {
		return summary, err
	}
	if previousRecovery.JournalPresent {
		summary.PreviousRecovery = &previousRecovery
		return summary, fmt.Errorf("%w: unresolved journal for %s requests action %s; inspect it explicitly before writing", ErrAtomicRecoveryNeedsInspection, path, previousRecovery.Action)
	}

	// A fixed-name backup from a pre-journal release has no nonce, hash, or
	// ownership proof. Detect it before writing, but never treat an adjacent
	// file as authority to create/replace the destination automatically.
	if _, err := recoverOverwriteBackupUnlocked(path); err != nil {
		return summary, err
	}

	var oldFingerprint atomicFingerprint
	destinationExists := false
	if info, err := statPath(path); err == nil {
		destinationExists = true
		if !opts.Overwrite {
			return summary, fmt.Errorf("%w: %s", ErrExists, path)
		}
		summary.Overwritten = true
		// Preserve the existing permission bits exactly, including mode 000.
		// Special bits are deliberately excluded by Perm().
		mode = info.Mode().Perm()
		oldFingerprint, err = fingerprintAtomicArtifact(path)
		if err != nil {
			return summary, err
		}
		if !oldFingerprint.exists {
			return summary, errors.New("destination disappeared while preparing atomic overwrite")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return summary, err
	}
	if !destinationExists {
		return writeNewFileAtomicNoClobber(summary, data, mode)
	}
	if int64(len(data)) > atomicWriteGenerationMax {
		return summary, fmt.Errorf("atomic overwrite new generation exceeds the %d-byte limit", atomicWriteGenerationMax)
	}

	operationID, err := newAtomicOperationID()
	if err != nil {
		return summary, err
	}
	journal, err := makeAtomicWriteJournal(path, suffix, operationID, summary.Overwritten, oldFingerprint, data)
	if err != nil {
		return summary, err
	}
	summary.TempPath = journal.TempPath
	summary.BackupPath = journal.BackupPath
	if err := ValidateExactOutputPath(summary.TempPath); err != nil {
		return summary, err
	}
	if err := ValidateExactOutputPath(summary.BackupPath); err != nil {
		return summary, err
	}

	// The named operation temp must be private while partial bytes are visible.
	// Apply the intended/inherited final mode only after the complete write.
	f, err := openPath(summary.TempPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return summary, fmt.Errorf("%w: %s", ErrTempExists, summary.TempPath)
		}
		return summary, err
	}

	var writeErr error
	if len(data) > 0 {
		n, err := f.Write(data)
		summary.BytesWritten = int64(n)
		if err != nil {
			writeErr = err
		} else if n != len(data) {
			writeErr = errors.New("short write")
		}
	}
	// os.OpenFile applies the process umask on POSIX systems. Reapply the
	// requested/existing mode on the private file descriptor before it can be
	// published so an overwrite does not silently lose permission bits.
	if writeErr == nil {
		writeErr = chmodOpenFile(f, mode)
	}
	if writeErr == nil {
		writeErr = f.Sync()
	}
	closeErr := f.Close()
	if writeErr != nil {
		cleanupErr := removeIncompleteAtomicTemp(summary.TempPath)
		return summary, errors.Join(writeErr, cleanupErr)
	}
	if closeErr != nil {
		cleanupErr := removeIncompleteAtomicTemp(summary.TempPath)
		return summary, errors.Join(closeErr, cleanupErr)
	}

	// The complete, synced temp receives an atomically published checksummed
	// journal before any authoritative pathname changes. If journal publication
	// fails, the operation-ID temp is retained as evidence but cannot block a
	// later save.
	beforeAtomicOverwriteJournal()
	if err := writeAtomicWriteJournal(journal); err != nil {
		return summary, fmt.Errorf("publish atomic-write journal; complete temporary output is preserved at %s: %w", summary.TempPath, err)
	}
	afterAtomicOverwriteJournal()
	if _, err := recoverAtomicWriteOperationUnlocked(path, journal.atomicWriteJournalPayload); err != nil {
		return summary, err
	}
	return summary, nil
}

func removeIncompleteAtomicTemp(path string) error {
	if err := removePath(path); err != nil {
		return fmt.Errorf("remove incomplete atomic-write temporary output %s: %w", path, err)
	}
	return nil
}

// writeNewFileAtomicNoClobber uses the platform's handle-bound publication
// primitive instead of the overwrite journal. There is no old generation to
// recover, and a concurrent creator must win rather than be replaced.
func writeNewFileAtomicNoClobber(summary AtomicWriteSummary, data []byte, mode os.FileMode) (_ AtomicWriteSummary, retErr error) {
	out, err := OpenAtomicOutput(summary.Path, nil, mode)
	if err != nil {
		return summary, err
	}
	summary.TempPath = out.TempPath()
	defer func() {
		retErr = errors.Join(retErr, out.Cleanup())
	}()
	if len(data) > 0 {
		n, writeErr := out.Write(data)
		summary.BytesWritten = int64(n)
		if writeErr != nil {
			return summary, writeErr
		}
		if n != len(data) {
			return summary, io.ErrShortWrite
		}
	}
	beforeAtomicNewFileCommit()
	if err := out.Commit(); err != nil {
		return summary, err
	}
	return summary, nil
}

// RecoverOverwriteBackup is retained as a compatibility inspection boundary.
// Fixed-name backups written by pre-journal releases are not authenticated or
// operation-bound, so this function never renames them. It returns an
// inspection-needed error when such an artifact is the only pathname present.
func RecoverOverwriteBackup(path string) (bool, error) {
	atomicWriteMu.Lock()
	defer atomicWriteMu.Unlock()
	return recoverOverwriteBackupUnlocked(path)
}

func recoverOverwriteBackupUnlocked(path string) (bool, error) {
	if path == "" {
		return false, errors.New("output path is required")
	}
	if err := ValidateExactOutputPath(path); err != nil {
		return false, err
	}
	backupPath := path + ".quarry.overwrite.bak"
	if err := ValidateExactOutputPath(backupPath); err != nil {
		return false, err
	}
	if _, err := statPath(path); err == nil {
		return false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	backupInfo, err := os.Lstat(backupPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	kind := "non-regular"
	if backupInfo.Mode().IsRegular() {
		kind = "regular"
	}
	return false, fmt.Errorf("%w: unjournaled legacy overwrite backup (%s) is preserved at %s; inspect and restore it explicitly", ErrAtomicRecoveryNeedsInspection, kind, backupPath)
}

func syncDirectory(path string) error {
	dir, _ := filepath.Split(path)
	if dir == "" {
		dir = "."
	}
	return syncDirectoryPath(dir)
}
