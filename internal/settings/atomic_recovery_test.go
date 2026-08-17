package settings

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/quarry/quarry-wails3/internal/fileio"
)

func TestLoadFileNeverExecutesChecksumValidRecoveryJournal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, SettingsFileName)
	oldSettings := Defaults()
	newSettings := Defaults()
	newSettings.ThemeMode = ThemeModeLight
	newSettings.RecentFiles = []string{filepath.Join(dir, "important.sql")}
	oldBytes := marshalSettingsFixture(t, oldSettings)
	newBytes := marshalSettingsFixture(t, newSettings)
	artifacts := installSettingsRecoveryFixture(t, path, oldBytes, newBytes)
	journalBytes, err := os.ReadFile(artifacts[2])
	if err != nil {
		t.Fatal(err)
	}

	got, err := LoadFile(path)
	if !errors.Is(err, fileio.ErrAtomicReadRecoveryPending) {
		t.Fatalf("error = %v, want structured pending recovery error", err)
	}
	if !reflect.DeepEqual(got, AppSettings{}) {
		t.Fatalf("settings = %#v, want zero value", got)
	}
	if _, statErr := os.Lstat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("recovery created the live settings path: %v", statErr)
	}
	if got, readErr := os.ReadFile(artifacts[0]); readErr != nil || string(got) != string(newBytes) {
		t.Fatalf("temporary settings changed: %q, %v", got, readErr)
	}
	if got, readErr := os.ReadFile(artifacts[1]); readErr != nil || string(got) != string(oldBytes) {
		t.Fatalf("backup settings changed: %q, %v", got, readErr)
	}
	if got, readErr := os.ReadFile(artifacts[2]); readErr != nil || string(got) != string(journalBytes) {
		t.Fatalf("journal changed: %q, %v", got, readErr)
	}
}

func TestLoadPersistentDoesNotFallbackPastCorruptRecoveryJournal(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(ConfigDirEnv, dir)
	path := filepath.Join(dir, SettingsFileName)
	journalPath := path + ".quarry.atomic.json"
	corrupt := []byte(`{"version":1,"checksum":"not-valid"}`)
	if err := os.WriteFile(journalPath, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	store := newMemStore()
	store.SetString(keyThemeMode, ThemeModeLight)

	got, err := LoadPersistent(store)
	if !errors.Is(err, fileio.ErrAtomicReadRecoveryPending) || !errors.Is(err, fileio.ErrAtomicRecoveryNeedsInspection) {
		t.Fatalf("error = %v, want structured pending recovery error", err)
	}
	if !reflect.DeepEqual(got, AppSettings{}) {
		t.Fatalf("settings = %#v, want zero value rather than legacy fallback", got)
	}
	if data, readErr := os.ReadFile(journalPath); readErr != nil || string(data) != string(corrupt) {
		t.Fatalf("corrupt recovery evidence changed: %q, %v", data, readErr)
	}
}

type settingsAtomicJournalPayload struct {
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

type settingsAtomicJournal struct {
	settingsAtomicJournalPayload
	Checksum string `json:"checksum"`
}

func installSettingsRecoveryFixture(t *testing.T, path string, oldBytes []byte, newBytes []byte) []string {
	t.Helper()
	const operationID = "00112233445566778899aabbccddeeff"
	oldDigest := sha256.Sum256(oldBytes)
	newDigest := sha256.Sum256(newBytes)
	payload := settingsAtomicJournalPayload{
		Version:        1,
		OperationID:    operationID,
		Path:           path,
		TempSuffix:     ".settings.tmp",
		TempPath:       path + ".settings.tmp." + operationID,
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
	journalBytes, err := json.MarshalIndent(settingsAtomicJournal{
		settingsAtomicJournalPayload: payload,
		Checksum:                     hex.EncodeToString(checksum[:]),
	}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	journalBytes = append(journalBytes, '\n')
	for artifact, data := range map[string][]byte{
		payload.TempPath:             newBytes,
		payload.BackupPath:           oldBytes,
		path + ".quarry.atomic.json": journalBytes,
	} {
		if err := os.WriteFile(artifact, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return []string{payload.TempPath, payload.BackupPath, path + ".quarry.atomic.json"}
}

func marshalSettingsFixture(t *testing.T, settings AppSettings) []byte {
	t.Helper()
	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(data, '\n')
}
