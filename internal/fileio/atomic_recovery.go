package fileio

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/quarry/quarry-wails3/internal/regularfile"
)

const (
	atomicWriteJournalSuffix = ".quarry.atomic.json"
	atomicWriteJournalMax    = 16 * 1024
	atomicWriteJournalV1     = 1
	// Journaled overwrite is deliberately a small-file primitive. The only
	// production data caller is the bounded in-memory editor (whose configured
	// ceiling is 64 MiB); settings, rules, and manifests are much smaller. This
	// cap also prevents an untrusted adjacent journal from making an ordinary
	// read hash arbitrarily large destination/temp/backup files.
	atomicWriteGenerationMax = 64 * 1024 * 1024
)

var (
	atomicWriteMu = sync.Mutex{}
	readRandom    = rand.Read

	ErrAtomicRecoveryNeedsInspection = errors.New("atomic write recovery needs inspection")
	ErrAtomicRecoveryJournal         = errors.New("invalid atomic write recovery journal")
	ErrAtomicReadRecoveryPending     = errors.New("atomic write recovery must be resolved before reading")
)

// AtomicRecoveryAction is the deterministic next step for a journaled atomic
// write. Inspect is fail-closed: no recovery artifact is changed automatically.
type AtomicRecoveryAction string

const (
	AtomicRecoveryNone     AtomicRecoveryAction = "none"
	AtomicRecoveryResume   AtomicRecoveryAction = "resume"
	AtomicRecoveryRollback AtomicRecoveryAction = "rollback"
	AtomicRecoveryFinalize AtomicRecoveryAction = "finalize"
	AtomicRecoveryInspect  AtomicRecoveryAction = "inspect"
)

// AtomicRecoveryArtifact describes one pathname without exposing its bytes.
// MatchesOld and MatchesNew are set only after a complete SHA-256 and size
// comparison against the checksummed journal.
type AtomicRecoveryArtifact struct {
	Path       string `json:"path"`
	Exists     bool   `json:"exists"`
	Size       int64  `json:"size,omitempty"`
	SHA256     string `json:"sha256,omitempty"`
	MatchesOld bool   `json:"matchesOld,omitempty"`
	MatchesNew bool   `json:"matchesNew,omitempty"`
}

// AtomicRecoveryState is a read-only classification of an interrupted write.
// A caller may display this state, but an adjacent checksum-valid journal is
// not authentication that Quarry created it and therefore grants no mutation
// authority to an ordinary reader or recovery caller.
type AtomicRecoveryState struct {
	Path           string                 `json:"path"`
	JournalPath    string                 `json:"journalPath"`
	OperationID    string                 `json:"operationId,omitempty"`
	JournalPresent bool                   `json:"journalPresent"`
	Action         AtomicRecoveryAction   `json:"action"`
	Reason         string                 `json:"reason,omitempty"`
	Resolved       bool                   `json:"resolved"`
	Destination    AtomicRecoveryArtifact `json:"destination"`
	Temporary      AtomicRecoveryArtifact `json:"temporary"`
	Backup         AtomicRecoveryArtifact `json:"backup"`
}

// AtomicReadRecoveryError carries the complete recovery classification to a
// reader that cannot safely continue. Every read path leaves recovery evidence
// untouched; only the in-flight writer retaining the complete expected journal
// payload may finish its own transaction before returning.
type AtomicReadRecoveryError struct {
	State AtomicRecoveryState
	Cause error
}

func (e *AtomicReadRecoveryError) Error() string {
	if e == nil {
		return ErrAtomicReadRecoveryPending.Error()
	}
	detail := e.State.Reason
	if detail == "" && e.State.Action != AtomicRecoveryNone {
		detail = "recovery action " + string(e.State.Action) + " is pending"
	}
	if detail == "" && e.Cause != nil {
		detail = e.Cause.Error()
	}
	if detail == "" {
		return fmt.Sprintf("%v for %q", ErrAtomicReadRecoveryPending, e.State.Path)
	}
	return fmt.Sprintf("%v for %q: %s", ErrAtomicReadRecoveryPending, e.State.Path, detail)
}

func (e *AtomicReadRecoveryError) Unwrap() []error {
	if e == nil || e.Cause == nil {
		return []error{ErrAtomicReadRecoveryPending}
	}
	return []error{ErrAtomicReadRecoveryPending, e.Cause}
}

// RecoverAtomicWriteBeforeRead is retained for source compatibility. Recovery
// evidence is inspection-only, so it now has the same non-mutating behavior as
// RequireAtomicWriteReadReady.
func RecoverAtomicWriteBeforeRead(path string) error {
	return RequireAtomicWriteReadReady(path)
}

// RequireAtomicWriteReadReady checks arbitrary source-file reads without
// mutating them. A valid pending journal and malformed/mismatched evidence both
// become structured errors rather than misleading os.ErrNotExist fallbacks.
func RequireAtomicWriteReadReady(path string) error {
	state, err := InspectAtomicWriteRecovery(path)
	if err != nil {
		return &AtomicReadRecoveryError{State: state, Cause: err}
	}
	if state.JournalPresent {
		return &AtomicReadRecoveryError{State: state}
	}
	return nil
}

type atomicWriteJournalPayload struct {
	Version        int    `json:"version"`
	OperationID    string `json:"operationId"`
	Path           string `json:"path"`
	TempSuffix     string `json:"tempSuffix"`
	TempPath       string `json:"tempPath"`
	BackupPath     string `json:"backupPath"`
	HadDestination bool   `json:"hadDestination"`
	OldSize        int64  `json:"oldSize,omitempty"`
	OldSHA256      string `json:"oldSha256,omitempty"`
	NewSize        int64  `json:"newSize"`
	NewSHA256      string `json:"newSha256"`
}

type atomicWriteJournal struct {
	atomicWriteJournalPayload
	Checksum string `json:"checksum"`
}

type atomicFingerprint struct {
	exists bool
	size   int64
	digest string
}

// InspectAtomicWriteRecovery validates and classifies a journal without
// changing the destination or any recovery artifact.
func InspectAtomicWriteRecovery(path string) (AtomicRecoveryState, error) {
	atomicWriteMu.Lock()
	defer atomicWriteMu.Unlock()
	return inspectAtomicWriteRecoveryUnlocked(path)
}

// RecoverAtomicWrite is an inspection-only compatibility boundary. A checksum
// detects accidental journal damage but cannot prove who created the adjacent
// file. Consequently this function never renames or removes artifacts. Only
// WriteFileAtomic's in-flight, complete-payload-bound completion path may
// mutate a transaction it just published.
func RecoverAtomicWrite(path string) (AtomicRecoveryState, error) {
	atomicWriteMu.Lock()
	defer atomicWriteMu.Unlock()
	return recoverAtomicWriteUnlocked(path)
}

func inspectAtomicWriteRecoveryUnlocked(path string) (AtomicRecoveryState, error) {
	state := AtomicRecoveryState{
		Path:        path,
		JournalPath: path + atomicWriteJournalSuffix,
		Action:      AtomicRecoveryNone,
		Destination: AtomicRecoveryArtifact{Path: path},
	}
	if path == "" {
		return state, errors.New("output path is required")
	}
	if err := ValidateExactOutputPath(path); err != nil {
		return state, err
	}
	if err := ValidateExactOutputPath(state.JournalPath); err != nil {
		return state, err
	}

	journal, present, err := loadAtomicWriteJournal(path)
	if err != nil {
		state.JournalPresent = present
		state.Action = AtomicRecoveryInspect
		state.Reason = err.Error()
		return state, fmt.Errorf("%w: %v", ErrAtomicRecoveryNeedsInspection, err)
	}
	if !present {
		return state, nil
	}

	state.JournalPresent = true
	state.OperationID = journal.OperationID
	state.Temporary.Path = journal.TempPath
	state.Backup.Path = journal.BackupPath

	destination, err := fingerprintAtomicArtifact(path)
	if err != nil {
		return atomicRecoveryInspectionError(state, "inspect destination", err)
	}
	temporary, err := fingerprintAtomicArtifact(journal.TempPath)
	if err != nil {
		return atomicRecoveryInspectionError(state, "inspect temporary output", err)
	}
	backup, err := fingerprintAtomicArtifact(journal.BackupPath)
	if err != nil {
		return atomicRecoveryInspectionError(state, "inspect backup", err)
	}

	state.Destination = describeAtomicArtifact(path, destination, journal)
	state.Temporary = describeAtomicArtifact(journal.TempPath, temporary, journal)
	state.Backup = describeAtomicArtifact(journal.BackupPath, backup, journal)
	classifyAtomicRecovery(&state, journal)
	if state.Action == AtomicRecoveryInspect {
		return state, fmt.Errorf("%w: %s", ErrAtomicRecoveryNeedsInspection, state.Reason)
	}
	return state, nil
}

func atomicRecoveryInspectionError(state AtomicRecoveryState, operation string, err error) (AtomicRecoveryState, error) {
	state.Action = AtomicRecoveryInspect
	state.Reason = operation + ": " + err.Error()
	return state, fmt.Errorf("%w: %s", ErrAtomicRecoveryNeedsInspection, state.Reason)
}

func describeAtomicArtifact(path string, fingerprint atomicFingerprint, journal atomicWriteJournalPayload) AtomicRecoveryArtifact {
	artifact := AtomicRecoveryArtifact{
		Path:   path,
		Exists: fingerprint.exists,
		Size:   fingerprint.size,
		SHA256: fingerprint.digest,
	}
	if !fingerprint.exists {
		return artifact
	}
	artifact.MatchesNew = fingerprint.size == journal.NewSize && fingerprint.digest == journal.NewSHA256
	artifact.MatchesOld = journal.HadDestination && fingerprint.size == journal.OldSize && fingerprint.digest == journal.OldSHA256
	return artifact
}

func classifyAtomicRecovery(state *AtomicRecoveryState, journal atomicWriteJournalPayload) {
	if state == nil {
		return
	}
	destination := state.Destination
	temporary := state.Temporary
	backup := state.Backup

	if temporary.Exists && !temporary.MatchesNew {
		state.Action = AtomicRecoveryInspect
		state.Reason = "temporary output does not match the journaled new generation"
		return
	}
	if backup.Exists && (!journal.HadDestination || !backup.MatchesOld) {
		state.Action = AtomicRecoveryInspect
		state.Reason = "backup does not match the journaled old generation"
		return
	}
	if destination.Exists && !destination.MatchesOld && !destination.MatchesNew {
		state.Action = AtomicRecoveryInspect
		state.Reason = "destination matches neither journaled generation"
		return
	}

	if !journal.HadDestination {
		switch {
		case !destination.Exists && temporary.Exists && !backup.Exists:
			state.Action = AtomicRecoveryResume
			state.Reason = "complete new generation is ready to publish"
		case destination.Exists && destination.MatchesNew && !temporary.Exists && !backup.Exists:
			state.Action = AtomicRecoveryFinalize
			state.Reason = "new generation is published; journal cleanup remains"
		case !destination.Exists && !temporary.Exists && !backup.Exists:
			state.Action = AtomicRecoveryRollback
			state.Reason = "no pre-existing destination and no recoverable output remain"
		default:
			state.Action = AtomicRecoveryInspect
			state.Reason = "artifact combination is not valid for a new-file publication"
		}
		return
	}

	switch {
	case destination.Exists && destination.MatchesOld && !backup.Exists && temporary.Exists:
		state.Action = AtomicRecoveryResume
		state.Reason = "old destination and complete new temporary output are present"
	case destination.Exists && destination.MatchesOld && backup.Exists && temporary.Exists:
		state.Action = AtomicRecoveryResume
		state.Reason = "old destination remains authoritative, its operation backup is linked, and complete new output is ready"
	case !destination.Exists && backup.Exists && temporary.Exists:
		state.Action = AtomicRecoveryResume
		state.Reason = "old destination is backed up and complete new output is ready"
	case destination.Exists && destination.MatchesNew && backup.Exists && !temporary.Exists:
		state.Action = AtomicRecoveryFinalize
		state.Reason = "new destination is published and old backup cleanup remains"
	case destination.Exists && destination.MatchesNew && !backup.Exists && !temporary.Exists:
		state.Action = AtomicRecoveryFinalize
		state.Reason = "new destination is published and journal cleanup remains"
	case destination.Exists && destination.MatchesOld && !backup.Exists && !temporary.Exists:
		state.Action = AtomicRecoveryRollback
		state.Reason = "old destination is intact and no new output remains"
	case !destination.Exists && backup.Exists && !temporary.Exists:
		state.Action = AtomicRecoveryRollback
		state.Reason = "new output is unavailable; restore the verified old backup"
	default:
		state.Action = AtomicRecoveryInspect
		state.Reason = "artifact combination is not a valid atomic-write phase"
	}
}

func recoverAtomicWriteUnlocked(path string) (AtomicRecoveryState, error) {
	state, err := inspectAtomicWriteRecoveryUnlocked(path)
	if err != nil {
		return state, err
	}
	if state.JournalPresent {
		return state, fmt.Errorf("%w: recovery evidence is inspection-only", ErrAtomicRecoveryNeedsInspection)
	}
	return state, nil
}

// recoverAtomicWriteOperationUnlocked binds mutation to the complete journal
// payload retained in memory by the current WriteFileAtomic call. An operation
// ID alone is insufficient because an adjacent checksum is not authentication:
// another process could copy that public ID into a different valid payload.
func recoverAtomicWriteOperationUnlocked(path string, expectedJournal atomicWriteJournalPayload) (AtomicRecoveryState, error) {
	if expectedJournal.OperationID == "" {
		return AtomicRecoveryState{Path: path}, fmt.Errorf("%w: a complete expected journal is required for mutation", ErrAtomicRecoveryNeedsInspection)
	}
	state, err := inspectAtomicWriteRecoveryUnlocked(path)
	if err != nil || state.Action == AtomicRecoveryNone {
		return state, err
	}
	if err := verifyExpectedAtomicJournal(path, expectedJournal); err != nil {
		state.Action = AtomicRecoveryInspect
		state.Reason = err.Error()
		return state, err
	}

	switch state.Action {
	case AtomicRecoveryResume:
		err = resumeAtomicWrite(&state, expectedJournal)
	case AtomicRecoveryRollback:
		err = rollbackAtomicWrite(&state, expectedJournal)
	case AtomicRecoveryFinalize:
		err = finalizeAtomicWrite(&state, expectedJournal)
	default:
		err = fmt.Errorf("%w: %s", ErrAtomicRecoveryNeedsInspection, state.Reason)
	}
	if err != nil {
		return state, err
	}
	state.Resolved = true
	return state, nil
}

func resumeAtomicWrite(state *AtomicRecoveryState, expectedJournal atomicWriteJournalPayload) error {
	if state == nil {
		return errors.New("atomic recovery state is required")
	}
	performedAction := state.Action
	performedReason := state.Reason
	if err := verifyExpectedAtomicJournal(state.Path, expectedJournal); err != nil {
		return err
	}
	if state.Destination.Exists && state.Destination.MatchesNew {
		return finalizeAtomicWrite(state, expectedJournal)
	}
	if !state.Destination.Exists || !state.Destination.MatchesOld {
		// The current writer never removes an old authoritative pathname. States
		// produced by older releases remain valuable evidence, but even a valid
		// checksum and operation ID do not justify recreating a missing user path.
		return fmt.Errorf("%w: old destination is not continuously present; preserve the recovery artifacts for inspection", ErrAtomicRecoveryNeedsInspection)
	}
	if !state.Temporary.Exists || !state.Temporary.MatchesNew {
		return fmt.Errorf("%w: complete new temporary output is unavailable", ErrAtomicRecoveryNeedsInspection)
	}
	if err := verifyAtomicRecoveryArtifact(state.Destination); err != nil {
		return err
	}
	if !state.Backup.Exists {
		if err := linkPath(state.Path, state.Backup.Path); err != nil {
			return fmt.Errorf("link old destination to operation backup without removing its authoritative name: %w", err)
		}
		if err := syncDirPath(state.Path); err != nil {
			return err
		}
		refreshed, err := inspectAtomicWriteRecoveryUnlocked(state.Path)
		if err != nil {
			return err
		}
		if refreshed.Action != AtomicRecoveryResume || !refreshed.Destination.MatchesOld || !refreshed.Backup.MatchesOld {
			return fmt.Errorf("%w: linked backup did not enter a resumable state", ErrAtomicRecoveryNeedsInspection)
		}
		*state = refreshed
	}

	// Give deterministic tests a point to introduce a competing generation,
	// then revalidate every pathname immediately before the single atomic
	// replacement. The remaining check-to-rename interval is an unavoidable
	// pathname race on the portable os.Rename API and is documented as such.
	beforeAtomicOverwritePublish()
	if err := verifyExpectedAtomicJournal(state.Path, expectedJournal); err != nil {
		return err
	}
	if err := verifyAtomicRecoveryArtifact(state.Destination); err != nil {
		return err
	}
	if err := verifyAtomicRecoveryArtifact(state.Temporary); err != nil {
		return err
	}
	if err := verifyAtomicRecoveryArtifact(state.Backup); err != nil {
		return err
	}
	if err := verifyAtomicHardLink(state.Path, state.Backup.Path); err != nil {
		return err
	}
	if err := renamePath(state.Temporary.Path, state.Path); err != nil {
		// Atomic replacement either leaves the old authoritative pathname in
		// place or publishes the complete new generation. The linked old
		// generation and journal remain untouched for inspection on failure.
		return fmt.Errorf("atomically replace old destination with recovered temporary output: %w", err)
	}
	afterAtomicOverwritePublish()
	temporaryPath := state.Temporary.Path
	state.Destination = state.Temporary
	state.Destination.Path = state.Path
	state.Temporary = AtomicRecoveryArtifact{Path: temporaryPath}
	if err := syncDirPath(state.Path); err != nil {
		return err
	}
	refreshed, err := inspectAtomicWriteRecoveryUnlocked(state.Path)
	if err != nil {
		return err
	}
	if refreshed.Action != AtomicRecoveryFinalize {
		return fmt.Errorf("%w: published write did not enter a finalizable state", ErrAtomicRecoveryNeedsInspection)
	}
	if err := finalizeAtomicWrite(&refreshed, expectedJournal); err != nil {
		return err
	}
	refreshed.Action = performedAction
	refreshed.Reason = performedReason
	*state = refreshed
	return nil
}

func rollbackAtomicWrite(state *AtomicRecoveryState, expectedJournal atomicWriteJournalPayload) error {
	if state == nil {
		return errors.New("atomic recovery state is required")
	}
	if err := verifyExpectedAtomicJournal(state.Path, expectedJournal); err != nil {
		return err
	}
	if !state.Destination.Exists && state.Backup.Exists {
		return fmt.Errorf("%w: destination is missing; backup restoration is inspection-only", ErrAtomicRecoveryNeedsInspection)
	}
	return removeAtomicJournal(state, expectedJournal)
}

func finalizeAtomicWrite(state *AtomicRecoveryState, expectedJournal atomicWriteJournalPayload) error {
	if state == nil {
		return errors.New("atomic recovery state is required")
	}
	if err := verifyExpectedAtomicJournal(state.Path, expectedJournal); err != nil {
		return err
	}
	if state.Backup.Exists {
		if err := verifyAtomicRecoveryArtifact(state.Destination); err != nil {
			return err
		}
		if err := verifyAtomicRecoveryArtifact(state.Backup); err != nil {
			return err
		}
		if err := removePath(state.Backup.Path); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("%w: verified overwrite backup disappeared before cleanup: %s", ErrAtomicRecoveryNeedsInspection, state.Backup.Path)
			}
			return fmt.Errorf("remove completed overwrite backup: %w", err)
		}
		afterAtomicOverwriteBackupRemoval()
		if err := syncDirPath(state.Path); err != nil {
			return err
		}
	}
	return removeAtomicJournal(state, expectedJournal)
}

func verifyAtomicHardLink(path string, backupPath string) error {
	destinationInfo, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("%w: inspect destination identity before publication: %v", ErrAtomicRecoveryNeedsInspection, err)
	}
	backupInfo, err := os.Lstat(backupPath)
	if err != nil {
		return fmt.Errorf("%w: inspect operation backup identity before publication: %v", ErrAtomicRecoveryNeedsInspection, err)
	}
	if !destinationInfo.Mode().IsRegular() || !backupInfo.Mode().IsRegular() || !os.SameFile(destinationInfo, backupInfo) {
		return fmt.Errorf("%w: operation backup is not a hard link to the current destination", ErrAtomicRecoveryNeedsInspection)
	}
	return nil
}

func verifyAtomicRecoveryArtifact(expected AtomicRecoveryArtifact) error {
	current, err := fingerprintAtomicArtifact(expected.Path)
	if err != nil {
		return fmt.Errorf("%w: verify %s: %v", ErrAtomicRecoveryNeedsInspection, expected.Path, err)
	}
	if !current.exists || !expected.Exists || current.size != expected.Size || current.digest != expected.SHA256 {
		return fmt.Errorf("%w: recovery artifact changed before mutation: %s", ErrAtomicRecoveryNeedsInspection, expected.Path)
	}
	return nil
}

func removeAtomicJournal(state *AtomicRecoveryState, expectedJournal atomicWriteJournalPayload) error {
	if state == nil {
		return errors.New("atomic recovery state is required")
	}
	journal, present, err := loadAtomicWriteJournal(state.Path)
	if err != nil {
		return fmt.Errorf("%w: journal changed before cleanup: %v", ErrAtomicRecoveryNeedsInspection, err)
	}
	if !present {
		return nil
	}
	if journal != expectedJournal || journal.OperationID != state.OperationID {
		return fmt.Errorf("%w: complete journal payload changed before cleanup", ErrAtomicRecoveryNeedsInspection)
	}
	if err := removePath(state.JournalPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: verified atomic-write journal disappeared before cleanup: %s", ErrAtomicRecoveryNeedsInspection, state.JournalPath)
		}
		return fmt.Errorf("remove completed atomic-write journal: %w", err)
	}
	return syncDirPath(state.Path)
}

func verifyExpectedAtomicJournal(path string, expected atomicWriteJournalPayload) error {
	if expected.OperationID == "" || expected.Path != path {
		return fmt.Errorf("%w: expected journal capability is incomplete or bound to another path", ErrAtomicRecoveryNeedsInspection)
	}
	journal, present, err := loadAtomicWriteJournal(path)
	if err != nil {
		return fmt.Errorf("%w: verify current operation journal: %v", ErrAtomicRecoveryNeedsInspection, err)
	}
	if !present || journal != expected {
		return fmt.Errorf("%w: complete journal payload changed after publication", ErrAtomicRecoveryNeedsInspection)
	}
	return nil
}

func newAtomicOperationID() (string, error) {
	var raw [16]byte
	if _, err := readRandom(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

func fingerprintBytes(data []byte) (int64, string) {
	digest := sha256.Sum256(data)
	return int64(len(data)), hex.EncodeToString(digest[:])
}

func fingerprintAtomicArtifact(path string) (atomicFingerprint, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return atomicFingerprint{}, nil
		}
		return atomicFingerprint{}, err
	}
	if !info.Mode().IsRegular() {
		return atomicFingerprint{}, fmt.Errorf("recovery artifact is not a regular file: %s", path)
	}
	if info.Size() > atomicWriteGenerationMax {
		return atomicFingerprint{}, fmt.Errorf("recovery artifact exceeds the %d-byte atomic-overwrite limit: %s", atomicWriteGenerationMax, path)
	}
	file, err := regularfile.OpenNoFollow(path)
	if err != nil {
		return atomicFingerprint{}, err
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil {
		return atomicFingerprint{}, err
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) {
		return atomicFingerprint{}, fmt.Errorf("recovery artifact identity changed while opening: %s", path)
	}
	if openedInfo.Size() > atomicWriteGenerationMax {
		return atomicFingerprint{}, fmt.Errorf("recovery artifact exceeds the %d-byte atomic-overwrite limit: %s", atomicWriteGenerationMax, path)
	}
	hash := sha256.New()
	size, err := io.Copy(hash, io.LimitReader(file, atomicWriteGenerationMax+1))
	if err != nil {
		return atomicFingerprint{}, err
	}
	if size > atomicWriteGenerationMax {
		return atomicFingerprint{}, fmt.Errorf("recovery artifact grew beyond the %d-byte atomic-overwrite limit while hashing: %s", atomicWriteGenerationMax, path)
	}
	finalInfo, err := file.Stat()
	if err != nil {
		return atomicFingerprint{}, err
	}
	if size != openedInfo.Size() || finalInfo.Size() != openedInfo.Size() || !finalInfo.ModTime().Equal(openedInfo.ModTime()) {
		return atomicFingerprint{}, fmt.Errorf("recovery artifact changed while hashing: %s", path)
	}
	return atomicFingerprint{exists: true, size: size, digest: hex.EncodeToString(hash.Sum(nil))}, nil
}

func makeAtomicWriteJournal(path string, suffix string, operationID string, hadDestination bool, oldFingerprint atomicFingerprint, data []byte) (atomicWriteJournal, error) {
	newSize, newDigest := fingerprintBytes(data)
	payload := atomicWriteJournalPayload{
		Version:        atomicWriteJournalV1,
		OperationID:    operationID,
		Path:           path,
		TempSuffix:     suffix,
		TempPath:       path + suffix + "." + operationID,
		BackupPath:     path + ".quarry.overwrite." + operationID + ".bak",
		HadDestination: hadDestination,
		NewSize:        newSize,
		NewSHA256:      newDigest,
	}
	if hadDestination {
		payload.OldSize = oldFingerprint.size
		payload.OldSHA256 = oldFingerprint.digest
	}
	if err := validateAtomicWriteJournal(path, payload); err != nil {
		return atomicWriteJournal{}, err
	}
	checksum, err := atomicJournalChecksum(payload)
	if err != nil {
		return atomicWriteJournal{}, err
	}
	return atomicWriteJournal{atomicWriteJournalPayload: payload, Checksum: checksum}, nil
}

func writeAtomicWriteJournal(journal atomicWriteJournal) (retErr error) {
	data, err := json.MarshalIndent(journal, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if len(data) > atomicWriteJournalMax {
		return fmt.Errorf("%w: journal exceeds %d bytes", ErrAtomicRecoveryJournal, atomicWriteJournalMax)
	}
	journalPath := journal.Path + atomicWriteJournalSuffix
	out, err := OpenAtomicOutput(journalPath, []string{journal.Path, journal.TempPath, journal.BackupPath}, 0o600)
	if err != nil {
		if errors.Is(err, ErrExists) {
			ownershipErr := fmt.Errorf("%w: another write or interrupted recovery owns %s", ErrAtomicRecoveryNeedsInspection, journalPath)
			return errors.Join(ownershipErr, err)
		}
		return err
	}
	defer func() { retErr = errors.Join(retErr, out.Cleanup()) }()
	written, err := out.Write(data)
	if err != nil {
		return err
	}
	if written != len(data) {
		return io.ErrShortWrite
	}
	if err := out.Commit(); err != nil {
		// Two writers can both observe the journal pathname as absent and create
		// private temporary journal files. The handle-bound no-clobber commit is
		// the ownership boundary, so a collision there has the same meaning as a
		// journal found by the initial check: another operation owns recovery.
		if errors.Is(err, ErrExists) {
			ownershipErr := fmt.Errorf("%w: another write or interrupted recovery owns %s", ErrAtomicRecoveryNeedsInspection, journalPath)
			return errors.Join(ownershipErr, err)
		}
		return err
	}
	return nil
}

func loadAtomicWriteJournal(path string) (atomicWriteJournalPayload, bool, error) {
	journalPath := path + atomicWriteJournalSuffix
	info, err := os.Lstat(journalPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return atomicWriteJournalPayload{}, false, nil
		}
		return atomicWriteJournalPayload{}, false, err
	}
	if !info.Mode().IsRegular() {
		return atomicWriteJournalPayload{}, true, fmt.Errorf("%w: journal is not a regular file", ErrAtomicRecoveryJournal)
	}
	if info.Size() > atomicWriteJournalMax {
		return atomicWriteJournalPayload{}, true, fmt.Errorf("%w: journal exceeds %d bytes", ErrAtomicRecoveryJournal, atomicWriteJournalMax)
	}
	file, err := regularfile.OpenNoFollow(journalPath)
	if err != nil {
		return atomicWriteJournalPayload{}, true, err
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil {
		return atomicWriteJournalPayload{}, true, err
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) {
		return atomicWriteJournalPayload{}, true, fmt.Errorf("%w: journal identity changed while opening", ErrAtomicRecoveryJournal)
	}
	data, err := io.ReadAll(io.LimitReader(file, atomicWriteJournalMax+1))
	if err != nil {
		return atomicWriteJournalPayload{}, true, err
	}
	if len(data) > atomicWriteJournalMax {
		return atomicWriteJournalPayload{}, true, fmt.Errorf("%w: journal exceeds %d bytes", ErrAtomicRecoveryJournal, atomicWriteJournalMax)
	}
	var journal atomicWriteJournal
	if err := jsonv2.Unmarshal(data, &journal, jsonv2.RejectUnknownMembers(true)); err != nil {
		return atomicWriteJournalPayload{}, true, fmt.Errorf("%w: %v", ErrAtomicRecoveryJournal, err)
	}
	if err := validateAtomicWriteJournal(path, journal.atomicWriteJournalPayload); err != nil {
		return atomicWriteJournalPayload{}, true, err
	}
	wantChecksum, err := atomicJournalChecksum(journal.atomicWriteJournalPayload)
	if err != nil {
		return atomicWriteJournalPayload{}, true, err
	}
	if journal.Checksum == "" || !strings.EqualFold(journal.Checksum, wantChecksum) {
		return atomicWriteJournalPayload{}, true, fmt.Errorf("%w: checksum mismatch", ErrAtomicRecoveryJournal)
	}
	return journal.atomicWriteJournalPayload, true, nil
}

func validateAtomicWriteJournal(path string, journal atomicWriteJournalPayload) error {
	if journal.Version != atomicWriteJournalV1 {
		return fmt.Errorf("%w: unsupported version %d", ErrAtomicRecoveryJournal, journal.Version)
	}
	decodedID, err := hex.DecodeString(journal.OperationID)
	if err != nil || len(decodedID) != 16 || strings.ToLower(journal.OperationID) != journal.OperationID {
		return fmt.Errorf("%w: invalid operation id", ErrAtomicRecoveryJournal)
	}
	if journal.Path != path {
		return fmt.Errorf("%w: journal is not bound to the requested path", ErrAtomicRecoveryJournal)
	}
	if journal.TempSuffix == "" {
		return fmt.Errorf("%w: temporary suffix is empty", ErrAtomicRecoveryJournal)
	}
	if journal.TempPath != path+journal.TempSuffix+"."+journal.OperationID {
		return fmt.Errorf("%w: temporary path is not operation-bound", ErrAtomicRecoveryJournal)
	}
	if journal.BackupPath != path+".quarry.overwrite."+journal.OperationID+".bak" {
		return fmt.Errorf("%w: backup path is not operation-bound", ErrAtomicRecoveryJournal)
	}
	for _, candidate := range []string{path, path + atomicWriteJournalSuffix, journal.TempPath, journal.BackupPath} {
		if err := ValidateExactOutputPath(candidate); err != nil {
			return fmt.Errorf("%w: %v", ErrAtomicRecoveryJournal, err)
		}
	}
	if journal.NewSize < 0 || journal.NewSize > atomicWriteGenerationMax || !validSHA256Hex(journal.NewSHA256) {
		return fmt.Errorf("%w: invalid new-generation fingerprint", ErrAtomicRecoveryJournal)
	}
	if journal.HadDestination {
		if journal.OldSize < 0 || journal.OldSize > atomicWriteGenerationMax || !validSHA256Hex(journal.OldSHA256) {
			return fmt.Errorf("%w: invalid old-generation fingerprint", ErrAtomicRecoveryJournal)
		}
	} else if journal.OldSize != 0 || journal.OldSHA256 != "" {
		return fmt.Errorf("%w: unexpected old-generation fingerprint", ErrAtomicRecoveryJournal)
	}
	return nil
}

func validSHA256Hex(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size && strings.ToLower(value) == value
}

func atomicJournalChecksum(payload atomicWriteJournalPayload) (string, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}
