package replace

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/quarry/quarry-wails3/internal/fileio"
)

func TestLoadBatchRuleFileDoesNotExecuteChecksumValidRecoveryInstructions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rules.txt")
	oldText := "alpha => old\n"
	newText := "alpha => new\n"
	artifacts := installReplaceAtomicRecoveryFixture(t, path, ".quarry.tmp", []byte(oldText), []byte(newText), nil)

	text, set, err := LoadBatchRuleFile(path)
	if !errors.Is(err, fileio.ErrAtomicReadRecoveryPending) {
		t.Fatalf("error = %v, want pending recovery error", err)
	}
	if text != "" || len(set.Rules) != 0 {
		t.Fatalf("rule read returned data despite pending evidence: %q / %#v", text, set)
	}
	if _, statErr := os.Lstat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("nominal read created/replaced the rule file: %v", statErr)
	}
	assertReplaceRecoveryArtifactsPreserved(t, artifacts)
}

func TestLoadBatchRuleFilePreservesMismatchedRecoveryEvidence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rules.txt")
	oldText := []byte("alpha => old\n")
	newText := []byte("alpha => new\n")
	foreignTemp := []byte("alpha => foreign\n")
	artifacts := installReplaceAtomicRecoveryFixture(t, path, ".quarry.tmp", oldText, newText, foreignTemp)
	if err := os.WriteFile(path, oldText, 0o600); err != nil {
		t.Fatal(err)
	}

	_, _, err := LoadBatchRuleFile(path)
	if !errors.Is(err, fileio.ErrAtomicReadRecoveryPending) || !errors.Is(err, fileio.ErrAtomicRecoveryNeedsInspection) {
		t.Fatalf("error = %v, want structured inspection error", err)
	}
	if got, readErr := os.ReadFile(path); readErr != nil || string(got) != string(oldText) {
		t.Fatalf("destination changed: %q, %v", got, readErr)
	}
	if got, readErr := os.ReadFile(artifacts[0]); readErr != nil || string(got) != string(foreignTemp) {
		t.Fatalf("mismatched temp changed: %q, %v", got, readErr)
	}
	if _, err := os.Lstat(artifacts[2]); err != nil {
		t.Fatalf("journal was not preserved: %v", err)
	}
}

func TestLoadManifestDoesNotExecuteChecksumValidRecoveryInstructions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "output.sql.quarry.manifest.json")
	oldManifest := testManifestForAtomicRefresh(dir, "running")
	newManifest := oldManifest
	newManifest.Status = "failed"
	newManifest.Error = "interrupted refresh completed during read"
	oldBytes := marshalReplaceManifestFixture(t, oldManifest)
	newBytes := marshalReplaceManifestFixture(t, newManifest)
	artifacts := installReplaceAtomicRecoveryFixture(t, path, manifestAtomicTempSuffix, oldBytes, newBytes, nil)

	got, err := LoadManifest(path)
	if !errors.Is(err, fileio.ErrAtomicReadRecoveryPending) {
		t.Fatalf("error = %v, want pending recovery error", err)
	}
	if got != (Manifest{}) {
		t.Fatalf("manifest read returned data despite pending evidence: %#v", got)
	}
	if _, statErr := os.Lstat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("nominal read created/replaced the manifest: %v", statErr)
	}
	assertReplaceRecoveryArtifactsPreserved(t, artifacts)
}

type replaceAtomicJournalPayload struct {
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

type replaceAtomicJournal struct {
	replaceAtomicJournalPayload
	Checksum string `json:"checksum"`
}

func installReplaceAtomicRecoveryFixture(t *testing.T, path string, suffix string, oldBytes []byte, newBytes []byte, tempOverride []byte) []string {
	t.Helper()
	const operationID = "102132435465768798a9bacbdcedfe0f"
	oldDigest := sha256.Sum256(oldBytes)
	newDigest := sha256.Sum256(newBytes)
	payload := replaceAtomicJournalPayload{
		Version:        1,
		OperationID:    operationID,
		Path:           path,
		TempSuffix:     suffix,
		TempPath:       path + suffix + "." + operationID,
		BackupPath:     path + ".quarry.overwrite." + operationID + ".bak",
		HadDestination: true,
		OldSize:        int64(len(oldBytes)),
		OldSHA256:      hex.EncodeToString(oldDigest[:]),
		NewSize:        int64(len(newBytes)),
		NewSHA256:      hex.EncodeToString(newDigest[:]),
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	checksum := sha256.Sum256(payloadBytes)
	journalBytes, err := json.MarshalIndent(replaceAtomicJournal{
		replaceAtomicJournalPayload: payload,
		Checksum:                    hex.EncodeToString(checksum[:]),
	}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	journalBytes = append(journalBytes, '\n')
	tempBytes := newBytes
	if tempOverride != nil {
		tempBytes = tempOverride
	}
	for artifact, data := range map[string][]byte{
		payload.TempPath:             tempBytes,
		payload.BackupPath:           oldBytes,
		path + ".quarry.atomic.json": journalBytes,
	} {
		if err := os.WriteFile(artifact, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return []string{payload.TempPath, payload.BackupPath, path + ".quarry.atomic.json"}
}

func marshalReplaceManifestFixture(t *testing.T, manifest Manifest) []byte {
	t.Helper()
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(data, '\n')
}

func assertReplaceRecoveryArtifactsPreserved(t *testing.T, artifacts []string) {
	t.Helper()
	for _, artifact := range artifacts {
		if info, err := os.Lstat(artifact); err != nil || !info.Mode().IsRegular() {
			t.Fatalf("pending artifact %q was not preserved as a regular file: %v, %v", artifact, info, err)
		}
	}
}
