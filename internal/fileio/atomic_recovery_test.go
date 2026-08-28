package fileio

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOperationBoundAtomicWriteCrashMatrixIsIdempotent(t *testing.T) {
	oldBytes := []byte("old generation")
	newBytes := []byte("new generation")
	tests := []struct {
		name           string
		hadDestination bool
		destination    []byte
		temporary      []byte
		backup         []byte
		wantAction     AtomicRecoveryAction
		wantOutput     []byte
	}{
		{name: "resume before destination backup", hadDestination: true, destination: oldBytes, temporary: newBytes, wantAction: AtomicRecoveryResume, wantOutput: newBytes},
		{name: "finalize after publish", hadDestination: true, destination: newBytes, backup: oldBytes, wantAction: AtomicRecoveryFinalize, wantOutput: newBytes},
		{name: "finalize after backup removal", hadDestination: true, destination: newBytes, wantAction: AtomicRecoveryFinalize, wantOutput: newBytes},
		{name: "rollback with old destination intact", hadDestination: true, destination: oldBytes, wantAction: AtomicRecoveryRollback, wantOutput: oldBytes},
		{name: "finalize new file", destination: newBytes, wantAction: AtomicRecoveryFinalize, wantOutput: newBytes},
		{name: "rollback empty new file transaction", wantAction: AtomicRecoveryRollback},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "settings.json")
			journal := installAtomicRecoveryFixture(t, path, ".settings.tmp", test.hadDestination, oldBytes, newBytes)
			writeOptionalRecoveryArtifact(t, path, test.destination)
			writeOptionalRecoveryArtifact(t, journal.TempPath, test.temporary)
			writeOptionalRecoveryArtifact(t, journal.BackupPath, test.backup)

			inspected, err := InspectAtomicWriteRecovery(path)
			if err != nil {
				t.Fatal(err)
			}
			if inspected.Action != test.wantAction || inspected.Resolved {
				t.Fatalf("inspection = %#v, want action %q unresolved", inspected, test.wantAction)
			}

			recovered, err := recoverAtomicWriteOperationUnlocked(path, journal.atomicWriteJournalPayload)
			if err != nil {
				t.Fatal(err)
			}
			if recovered.Action != test.wantAction || !recovered.Resolved {
				t.Fatalf("recovery = %#v, want action %q resolved", recovered, test.wantAction)
			}
			assertOptionalRecoveryArtifact(t, path, test.wantOutput)
			for _, artifact := range []string{journal.TempPath, journal.BackupPath, path + atomicWriteJournalSuffix} {
				if _, err := os.Lstat(artifact); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("resolved artifact %q remains: %v", artifact, err)
				}
			}

			again, err := recoverAtomicWriteOperationUnlocked(path, journal.atomicWriteJournalPayload)
			if err != nil {
				t.Fatal(err)
			}
			if again.Action != AtomicRecoveryNone || again.JournalPresent || again.Resolved {
				t.Fatalf("second recovery = %#v, want idempotent no-op", again)
			}
			assertOptionalRecoveryArtifact(t, path, test.wantOutput)
		})
	}
}

func TestOperationBoundAtomicWriteResumesFromLinkedBackupWithoutPathGap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	oldBytes := []byte("old generation")
	newBytes := []byte("new generation")
	journal := installAtomicRecoveryFixture(t, path, ".settings.tmp", true, oldBytes, newBytes)
	writeOptionalRecoveryArtifact(t, path, oldBytes)
	writeOptionalRecoveryArtifact(t, journal.TempPath, newBytes)
	if err := os.Link(path, journal.BackupPath); err != nil {
		t.Fatal(err)
	}

	state, err := recoverAtomicWriteOperationUnlocked(path, journal.atomicWriteJournalPayload)
	if err != nil {
		t.Fatal(err)
	}
	if !state.Resolved || state.Action != AtomicRecoveryResume {
		t.Fatalf("state = %#v, want resolved resume", state)
	}
	assertOptionalRecoveryArtifact(t, path, newBytes)
	for _, artifact := range []string{journal.TempPath, journal.BackupPath, path + atomicWriteJournalSuffix} {
		if _, err := os.Lstat(artifact); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("resolved artifact %q remains: %v", artifact, err)
		}
	}
}

func TestOperationBoundAtomicWritePreservesPathMissingRecoveryEvidence(t *testing.T) {
	oldBytes := []byte("old generation")
	newBytes := []byte("new generation")
	tests := []struct {
		name           string
		hadDestination bool
		temporary      []byte
		backup         []byte
	}{
		{name: "old destination moved to backup", hadDestination: true, temporary: newBytes, backup: oldBytes},
		{name: "old destination only in backup", hadDestination: true, backup: oldBytes},
		{name: "obsolete journaled new-file publication", temporary: newBytes},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "settings.json")
			journal := installAtomicRecoveryFixture(t, path, ".settings.tmp", test.hadDestination, oldBytes, newBytes)
			writeOptionalRecoveryArtifact(t, journal.TempPath, test.temporary)
			writeOptionalRecoveryArtifact(t, journal.BackupPath, test.backup)
			journalBytes, err := os.ReadFile(path + atomicWriteJournalSuffix)
			if err != nil {
				t.Fatal(err)
			}

			_, err = recoverAtomicWriteOperationUnlocked(path, journal.atomicWriteJournalPayload)
			if !errors.Is(err, ErrAtomicRecoveryNeedsInspection) {
				t.Fatalf("error = %v, want inspection-needed refusal", err)
			}
			assertOptionalRecoveryArtifact(t, path, nil)
			assertOptionalRecoveryArtifact(t, journal.TempPath, test.temporary)
			assertOptionalRecoveryArtifact(t, journal.BackupPath, test.backup)
			assertOptionalRecoveryArtifact(t, path+atomicWriteJournalSuffix, journalBytes)
		})
	}
}

func TestRecoverAtomicWritePreservesMismatchedArtifacts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rules.txt")
	oldBytes := []byte("old rules")
	newBytes := []byte("new rules")
	foreignTemp := []byte("unrelated bytes")
	journal := installAtomicRecoveryFixture(t, path, defaultTempSuffix, true, oldBytes, newBytes)
	writeOptionalRecoveryArtifact(t, path, oldBytes)
	writeOptionalRecoveryArtifact(t, journal.TempPath, foreignTemp)

	state, err := RecoverAtomicWrite(path)
	if !errors.Is(err, ErrAtomicRecoveryNeedsInspection) {
		t.Fatalf("error = %v, want ErrAtomicRecoveryNeedsInspection", err)
	}
	if state.Action != AtomicRecoveryInspect || state.Reason == "" {
		t.Fatalf("state = %#v, want inspect with reason", state)
	}
	assertOptionalRecoveryArtifact(t, path, oldBytes)
	assertOptionalRecoveryArtifact(t, journal.TempPath, foreignTemp)
	if _, err := os.Lstat(path + atomicWriteJournalSuffix); err != nil {
		t.Fatalf("journal was not preserved: %v", err)
	}
}

func TestOperationBoundAtomicWriteRejectsChangedPayloadWithSameOperationID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	oldBytes := []byte("old settings")
	requestedBytes := []byte("requested settings")
	forgedBytes := []byte("different settings")
	expected := installAtomicRecoveryFixture(t, path, ".settings.tmp", true, oldBytes, requestedBytes)
	writeOptionalRecoveryArtifact(t, path, oldBytes)

	oldSize, oldDigest := fingerprintBytes(oldBytes)
	forged, err := makeAtomicWriteJournal(
		path,
		".settings.tmp",
		expected.OperationID,
		true,
		atomicFingerprint{exists: true, size: oldSize, digest: oldDigest},
		forgedBytes,
	)
	if err != nil {
		t.Fatal(err)
	}
	forgedJSON, err := json.MarshalIndent(forged, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	forgedJSON = append(forgedJSON, '\n')
	if err := os.WriteFile(path+atomicWriteJournalSuffix, forgedJSON, 0o600); err != nil {
		t.Fatal(err)
	}
	writeOptionalRecoveryArtifact(t, forged.TempPath, forgedBytes)

	_, err = recoverAtomicWriteOperationUnlocked(path, expected.atomicWriteJournalPayload)
	if !errors.Is(err, ErrAtomicRecoveryNeedsInspection) {
		t.Fatalf("error = %v, want inspection-needed refusal", err)
	}
	assertOptionalRecoveryArtifact(t, path, oldBytes)
	assertOptionalRecoveryArtifact(t, forged.TempPath, forgedBytes)
	assertOptionalRecoveryArtifact(t, path+atomicWriteJournalSuffix, forgedJSON)
}

func TestRecoverAtomicWritePreservesCorruptJournalAndNamedArtifacts(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cache.json")
	journalPath := path + atomicWriteJournalSuffix
	if err := os.WriteFile(path, []byte("current"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(journalPath, []byte(`{"version":1,"checksum":"wrong"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	state, err := RecoverAtomicWrite(path)
	if !errors.Is(err, ErrAtomicRecoveryNeedsInspection) {
		t.Fatalf("error = %v, want ErrAtomicRecoveryNeedsInspection", err)
	}
	if !state.JournalPresent || state.Action != AtomicRecoveryInspect {
		t.Fatalf("state = %#v, want preserved inspect state", state)
	}
	assertOptionalRecoveryArtifact(t, path, []byte("current"))
	assertOptionalRecoveryArtifact(t, journalPath, []byte(`{"version":1,"checksum":"wrong"}`))
}

func TestAtomicWriteJournalStrictJSONDecoding(t *testing.T) {
	tests := []struct {
		name   string
		mutate func([]byte, atomicWriteJournal) []byte
	}{
		{name: "unknown field", mutate: func(data []byte, _ atomicWriteJournal) []byte {
			return bytes.Replace(data, []byte(`"checksum"`), []byte(`"forged":true,"checksum"`), 1)
		}},
		{name: "duplicate field", mutate: func(data []byte, _ atomicWriteJournal) []byte {
			return bytes.Replace(data, []byte(`"version": 1`), []byte(`"version": 1,"version": 1`), 1)
		}},
		{name: "invalid UTF-8", mutate: func(data []byte, journal atomicWriteJournal) []byte {
			invalidID := append([]byte(nil), journal.OperationID...)
			invalidID[0] = 0xff
			return bytes.Replace(data, []byte(journal.OperationID), invalidID, 1)
		}},
		{name: "case-mismatched field", mutate: func(data []byte, _ atomicWriteJournal) []byte {
			return bytes.Replace(data, []byte(`"operationId"`), []byte(`"OperationId"`), 1)
		}},
		{name: "trailing value", mutate: func(data []byte, _ atomicWriteJournal) []byte {
			return append(data, []byte(` {}`)...)
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "settings.json")
			journal := installAtomicRecoveryFixture(t, path, ".settings.tmp", false, nil, []byte("new settings"))
			journalPath := path + atomicWriteJournalSuffix
			data, err := os.ReadFile(journalPath)
			if err != nil {
				t.Fatal(err)
			}
			mutated := test.mutate(data, journal)
			if err := os.WriteFile(journalPath, mutated, 0o600); err != nil {
				t.Fatal(err)
			}

			if _, present, err := loadAtomicWriteJournal(path); !present || !errors.Is(err, ErrAtomicRecoveryJournal) {
				t.Fatalf("loadAtomicWriteJournal = present %v, error %v; want present invalid journal", present, err)
			}
		})
	}
}

func TestAtomicWriteJournalPreservesNullAndZeroSemantics(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	journal := installAtomicRecoveryFixture(t, path, ".settings.tmp", false, nil, []byte("new settings"))
	journalPath := path + atomicWriteJournalSuffix
	data, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	data = bytes.Replace(data, []byte(`"newSize":`), []byte(`"oldSize":null,"oldSha256":null,"newSize":`), 1)
	if err := os.WriteFile(journalPath, data, 0o600); err != nil {
		t.Fatal(err)
	}

	got, present, err := loadAtomicWriteJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	if !present || got.OldSize != 0 || got.OldSHA256 != "" || got != journal.atomicWriteJournalPayload {
		t.Fatalf("decoded journal = %#v, present %v; want original zero-value payload", got, present)
	}
}

func TestInspectAtomicWriteRejectsChecksumValidOversizedGenerationBeforeArtifacts(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "oversized.dat")
	oldBytes := []byte("old")
	newBytes := []byte("new")
	journal := installAtomicRecoveryFixture(t, path, defaultTempSuffix, true, oldBytes, newBytes)

	journal.NewSize = atomicWriteGenerationMax + 1
	checksum, err := atomicJournalChecksum(journal.atomicWriteJournalPayload)
	if err != nil {
		t.Fatal(err)
	}
	journal.Checksum = checksum
	journalBytes, err := json.MarshalIndent(journal, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	journalBytes = append(journalBytes, '\n')
	journalPath := path + atomicWriteJournalSuffix
	if err := os.WriteFile(journalPath, journalBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, atomicWriteGenerationMax+1); err != nil {
		t.Fatal(err)
	}

	state, err := InspectAtomicWriteRecovery(path)
	if !errors.Is(err, ErrAtomicRecoveryNeedsInspection) {
		t.Fatalf("error = %v, want inspection-needed refusal", err)
	}
	if !state.JournalPresent || state.Action != AtomicRecoveryInspect {
		t.Fatalf("state = %#v, want preserved inspect state", state)
	}
	if state.Reason == "" || state.Reason != "invalid atomic write recovery journal: invalid new-generation fingerprint" {
		t.Fatalf("reason = %q, want journal generation-limit rejection", state.Reason)
	}
	info, statErr := os.Stat(path)
	if statErr != nil || info.Size() != atomicWriteGenerationMax+1 {
		t.Fatalf("oversized destination changed: size=%d err=%v", info.Size(), statErr)
	}
	assertOptionalRecoveryArtifact(t, journalPath, journalBytes)
}

func TestWriteFileAtomicRefusesOversizedExistingGenerationBeforeCreatingArtifacts(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "oversized.dat")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, atomicWriteGenerationMax+1); err != nil {
		t.Fatal(err)
	}

	_, err := WriteFileAtomic(path, []byte("replacement"), AtomicWriteOptions{Overwrite: true})
	if err == nil {
		t.Fatal("oversized journaled overwrite unexpectedly succeeded")
	}
	info, statErr := os.Stat(path)
	if statErr != nil || info.Size() != atomicWriteGenerationMax+1 {
		t.Fatalf("destination changed: size=%d err=%v", info.Size(), statErr)
	}
	entries, readErr := os.ReadDir(dir)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(path) {
		t.Fatalf("overwrite created adjacent artifacts: %#v", entries)
	}
}

func TestOperationBoundAtomicWriteRetriesAfterCleanupInterruption(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	oldBytes := []byte("old")
	newBytes := []byte("new")
	journal := installAtomicRecoveryFixture(t, path, ".settings.tmp", true, oldBytes, newBytes)
	writeOptionalRecoveryArtifact(t, path, newBytes)
	writeOptionalRecoveryArtifact(t, journal.BackupPath, oldBytes)

	originalRemove := removePath
	sentinel := errors.New("interrupted backup cleanup")
	failed := false
	removePath = func(candidate string) error {
		if candidate == journal.BackupPath && !failed {
			failed = true
			return sentinel
		}
		return os.Remove(candidate)
	}
	_, err := recoverAtomicWriteOperationUnlocked(path, journal.atomicWriteJournalPayload)
	removePath = originalRemove
	if !errors.Is(err, sentinel) {
		t.Fatalf("first recovery error = %v, want sentinel", err)
	}
	assertOptionalRecoveryArtifact(t, path, newBytes)
	assertOptionalRecoveryArtifact(t, journal.BackupPath, oldBytes)

	state, err := recoverAtomicWriteOperationUnlocked(path, journal.atomicWriteJournalPayload)
	if err != nil {
		t.Fatal(err)
	}
	if !state.Resolved || state.Action != AtomicRecoveryFinalize {
		t.Fatalf("retry state = %#v", state)
	}
	assertOptionalRecoveryArtifact(t, path, newBytes)
}

func TestOperationBoundAtomicWriteReportsExpectedCleanupArtifactDisappearance(t *testing.T) {
	oldBytes := []byte("old")
	newBytes := []byte("new")
	tests := []struct {
		name       string
		withBackup bool
		removePath func(path string, journal atomicWriteJournal) string
	}{
		{
			name:       "verified backup disappears",
			withBackup: true,
			removePath: func(_ string, journal atomicWriteJournal) string { return journal.BackupPath },
		},
		{
			name:       "verified journal disappears",
			removePath: func(path string, _ atomicWriteJournal) string { return path + atomicWriteJournalSuffix },
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "settings.json")
			journal := installAtomicRecoveryFixture(t, path, ".settings.tmp", true, oldBytes, newBytes)
			writeOptionalRecoveryArtifact(t, path, newBytes)
			if test.withBackup {
				writeOptionalRecoveryArtifact(t, journal.BackupPath, oldBytes)
			}
			disappearingPath := test.removePath(path, journal)

			originalRemove := removePath
			removePath = func(candidate string) error {
				if candidate == disappearingPath {
					if err := os.Remove(candidate); err != nil {
						return err
					}
					return &os.PathError{Op: "remove", Path: candidate, Err: os.ErrNotExist}
				}
				return os.Remove(candidate)
			}
			t.Cleanup(func() { removePath = originalRemove })

			_, err := recoverAtomicWriteOperationUnlocked(path, journal.atomicWriteJournalPayload)
			removePath = originalRemove
			if !errors.Is(err, ErrAtomicRecoveryNeedsInspection) {
				t.Fatalf("error = %v, want inspection-required disappearance", err)
			}
			if err == nil || !strings.Contains(err.Error(), disappearingPath) {
				t.Fatalf("error %q does not identify disappeared artifact %q", err, disappearingPath)
			}
			assertOptionalRecoveryArtifact(t, path, newBytes)
			if _, statErr := os.Lstat(disappearingPath); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("disappeared artifact is present: %v", statErr)
			}
			if test.withBackup {
				state, inspectErr := InspectAtomicWriteRecovery(path)
				if inspectErr != nil || state.Action != AtomicRecoveryFinalize || !state.JournalPresent {
					t.Fatalf("post-interference state = %#v, %v; want preserved finalization journal", state, inspectErr)
				}
			} else {
				state, inspectErr := InspectAtomicWriteRecovery(path)
				if inspectErr != nil || state.Action != AtomicRecoveryNone || state.JournalPresent {
					t.Fatalf("post-journal-removal state = %#v, %v; want no remaining recovery evidence", state, inspectErr)
				}
			}
		})
	}
}

func TestRecoverAtomicWriteBeforeReadNeverResolvesChecksumValidState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	oldBytes := []byte("old settings")
	newBytes := []byte("new settings")
	journal := installAtomicRecoveryFixture(t, path, ".settings.tmp", true, oldBytes, newBytes)
	writeOptionalRecoveryArtifact(t, journal.BackupPath, oldBytes)
	writeOptionalRecoveryArtifact(t, journal.TempPath, newBytes)

	journalBytes, err := os.ReadFile(path + atomicWriteJournalSuffix)
	if err != nil {
		t.Fatal(err)
	}
	if err := RecoverAtomicWriteBeforeRead(path); !errors.Is(err, ErrAtomicReadRecoveryPending) {
		t.Fatalf("error = %v, want pending recovery", err)
	}
	assertOptionalRecoveryArtifact(t, path, nil)
	assertOptionalRecoveryArtifact(t, journal.BackupPath, oldBytes)
	assertOptionalRecoveryArtifact(t, journal.TempPath, newBytes)
	assertOptionalRecoveryArtifact(t, path+atomicWriteJournalSuffix, journalBytes)
}

func TestRequireAtomicWriteReadReadyReturnsStructuredStateWithoutMutation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "document.txt")
	oldBytes := []byte("old document")
	newBytes := []byte("new document")
	journal := installAtomicRecoveryFixture(t, path, defaultTempSuffix, true, oldBytes, newBytes)
	writeOptionalRecoveryArtifact(t, path, oldBytes)
	writeOptionalRecoveryArtifact(t, journal.TempPath, newBytes)

	err := RequireAtomicWriteReadReady(path)
	if !errors.Is(err, ErrAtomicReadRecoveryPending) {
		t.Fatalf("error = %v, want ErrAtomicReadRecoveryPending", err)
	}
	var structured *AtomicReadRecoveryError
	if !errors.As(err, &structured) {
		t.Fatalf("error %T does not expose AtomicReadRecoveryError", err)
	}
	if structured.State.Action != AtomicRecoveryResume || !structured.State.JournalPresent {
		t.Fatalf("structured state = %#v", structured.State)
	}
	assertOptionalRecoveryArtifact(t, path, oldBytes)
	assertOptionalRecoveryArtifact(t, journal.TempPath, newBytes)
	if _, err := os.Lstat(path + atomicWriteJournalSuffix); err != nil {
		t.Fatalf("readiness check changed journal: %v", err)
	}
}

func TestRecoverAtomicWriteBeforeReadWrapsMismatchedEvidence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rules.txt")
	oldBytes := []byte("old rules")
	newBytes := []byte("new rules")
	foreignBytes := []byte("foreign temp")
	journal := installAtomicRecoveryFixture(t, path, defaultTempSuffix, true, oldBytes, newBytes)
	writeOptionalRecoveryArtifact(t, path, oldBytes)
	writeOptionalRecoveryArtifact(t, journal.TempPath, foreignBytes)

	err := RecoverAtomicWriteBeforeRead(path)
	if !errors.Is(err, ErrAtomicReadRecoveryPending) || !errors.Is(err, ErrAtomicRecoveryNeedsInspection) {
		t.Fatalf("error = %v, want pending and needs-inspection identities", err)
	}
	var structured *AtomicReadRecoveryError
	if !errors.As(err, &structured) || structured.State.Action != AtomicRecoveryInspect {
		t.Fatalf("structured error = %#v", structured)
	}
	assertOptionalRecoveryArtifact(t, path, oldBytes)
	assertOptionalRecoveryArtifact(t, journal.TempPath, foreignBytes)
}

func installAtomicRecoveryFixture(t *testing.T, path string, suffix string, hadDestination bool, oldBytes []byte, newBytes []byte) atomicWriteJournal {
	t.Helper()
	oldSize, oldDigest := fingerprintBytes(oldBytes)
	oldFingerprint := atomicFingerprint{exists: hadDestination, size: oldSize, digest: oldDigest}
	journal, err := makeAtomicWriteJournal(path, suffix, "00112233445566778899aabbccddeeff", hadDestination, oldFingerprint, newBytes)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.MarshalIndent(journal, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path+atomicWriteJournalSuffix, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return journal
}

func writeOptionalRecoveryArtifact(t *testing.T, path string, data []byte) {
	t.Helper()
	if data == nil {
		return
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertOptionalRecoveryArtifact(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if want == nil {
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("artifact %q = %q, %v; want absent", path, got, err)
		}
		return
	}
	if err != nil || string(got) != string(want) {
		t.Fatalf("artifact %q = %q, %v; want %q", path, got, err, want)
	}
}
