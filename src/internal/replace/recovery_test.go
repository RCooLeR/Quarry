package replace

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/quarry/quarry-wails3/internal/settings"
)

func TestFindRecoveryStatesFiltersBySourceAndSortsNewestFirst(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.sql")
	writeRecoveryFile(t, sourcePath, "select 1;\n")

	outputPath := filepath.Join(dir, "output.sql")
	tempPath := outputPath + ".quarry.tmp"
	writeRecoveryFile(t, tempPath, "partial")
	fileManifestPath := outputPath + recoveryManifestSuffix
	writeRecoveryManifest(t, fileManifestPath, Manifest{
		Operation:      "plain-replace",
		Source:         sourcePath,
		Output:         outputPath,
		TempOutput:     tempPath,
		StartedAt:      time.Unix(1716780000, 0).UTC(),
		SourceSize:     10,
		BytesProcessed: 5,
		Matches:        1,
		Status:         "canceled",
	})

	inPlaceManifestPath := sourcePath + ".quarry.inplace.20260528T120000.000000000Z.manifest.json"
	writeRecoveryManifest(t, inPlaceManifestPath, Manifest{
		Operation:      "plain-replace-in-place",
		Source:         sourcePath,
		Output:         sourcePath,
		StartedAt:      time.Unix(1716790000, 0).UTC(),
		SourceSize:     10,
		BytesProcessed: 3,
		Matches:        1,
		Status:         "failed",
		Error:          "canceled mid-run",
	})

	states, err := FindRecoveryStates(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 2 {
		t.Fatalf("states = %#v, want 2 entries", states)
	}
	if states[0].ManifestPath != inPlaceManifestPath || !states[0].InPlace || !states[0].SourceExists {
		t.Fatalf("unexpected first state: %#v", states[0])
	}
	if states[1].ManifestPath != fileManifestPath || states[1].InPlace || !states[1].TempExists {
		t.Fatalf("unexpected second state: %#v", states[1])
	}
}

func TestFindRecoveryStatesKeepsLegacyInPlaceManifest(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.sql")
	writeRecoveryFile(t, sourcePath, "select 1;\n")
	manifestPath := sourcePath + ".quarry.inplace.manifest.json"
	writeRecoveryManifest(t, manifestPath, Manifest{
		Operation: "plain-replace-in-place",
		Source:    sourcePath,
		Output:    sourcePath,
		StartedAt: time.Unix(1716790000, 0).UTC(),
		Status:    "failed",
	})

	states, err := FindRecoveryStates(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 1 || states[0].ManifestPath != manifestPath || !states[0].InPlace {
		t.Fatalf("states = %#v, want legacy in-place manifest", states)
	}
}

func TestFindRecoveryStatesLogsMalformedManifestAndContinues(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(settings.ConfigDirEnv, filepath.Join(dir, "quarry-home"))
	sourcePath := filepath.Join(dir, "source.sql")
	writeRecoveryFile(t, sourcePath, "select 1;\n")
	outputPath := filepath.Join(dir, "output.sql")
	validManifestPath := outputPath + recoveryManifestSuffix
	writeRecoveryManifest(t, validManifestPath, Manifest{
		Operation: "plain-replace",
		Source:    sourcePath,
		Output:    outputPath,
		StartedAt: time.Unix(1716780000, 0).UTC(),
		Status:    "failed",
	})
	badManifestPath := filepath.Join(dir, "bad.sql") + recoveryManifestSuffix
	writeRecoveryFile(t, badManifestPath, "{not-json")

	states, err := FindRecoveryStates(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 1 || states[0].ManifestPath != validManifestPath {
		t.Fatalf("states = %#v, want only valid manifest", states)
	}
	logData, err := os.ReadFile(filepath.Join(dir, "quarry-home", "quarry.log"))
	if err != nil {
		t.Fatal(err)
	}
	if logText := string(logData); !strings.Contains(logText, "skipping recovery manifest") || !strings.Contains(logText, filepath.Base(badManifestPath)) {
		t.Fatalf("log = %q, want malformed manifest warning", logText)
	}
}

func TestLoadManifestRejectsOversizedUnknownAndTrailingJSON(t *testing.T) {
	t.Run("oversized", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "output.sql") + recoveryManifestSuffix
		writeRecoveryFile(t, path, strings.Repeat(" ", maxRecoveryManifestBytes+1))
		if _, err := LoadManifest(path); !errors.Is(err, ErrInvalidRecoveryManifest) {
			t.Fatalf("LoadManifest error = %v, want ErrInvalidRecoveryManifest", err)
		}
	})

	for _, tc := range []struct {
		name   string
		mutate func([]byte) []byte
	}{
		{name: "unknown field", mutate: func(data []byte) []byte {
			return append(data[:len(data)-1], []byte(`,"forged":true}`)...)
		}},
		{name: "duplicate field", mutate: func(data []byte) []byte {
			return append(data[:len(data)-1], []byte(`,"operation":"regex-replace"}`)...)
		}},
		{name: "invalid UTF-8", mutate: func(data []byte) []byte {
			return bytes.Replace(data, []byte("plain-replace"), []byte{'p', 'l', 'a', 'i', 'n', '-', 0xff}, 1)
		}},
		{name: "case-mismatched field", mutate: func(data []byte) []byte {
			return bytes.Replace(data, []byte(`"operation"`), []byte(`"Operation"`), 1)
		}},
		{name: "trailing object", mutate: func(data []byte) []byte {
			return append(data, []byte(` {}`)...)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			sourcePath := filepath.Join(dir, "source.sql")
			outputPath := filepath.Join(dir, "output.sql")
			manifestPath := outputPath + recoveryManifestSuffix
			data, err := json.Marshal(Manifest{
				Operation: "plain-replace", Source: sourcePath, Output: outputPath,
				StartedAt: time.Now().UTC(), Status: "failed",
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(manifestPath, tc.mutate(data), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadManifest(manifestPath); !errors.Is(err, ErrInvalidRecoveryManifest) {
				t.Fatalf("LoadManifest error = %v, want ErrInvalidRecoveryManifest", err)
			}
		})
	}
}

func TestLoadManifestPreservesNullAndZeroSemantics(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.sql")
	outputPath := filepath.Join(dir, "output.sql")
	manifestPath := outputPath + recoveryManifestSuffix
	data, err := json.Marshal(map[string]any{
		"operation":      "plain-replace",
		"source":         sourcePath,
		"output":         outputPath,
		"tempOutput":     nil,
		"startedAt":      time.Unix(1_700_000_000, 0).UTC(),
		"completedAt":    nil,
		"sourceSize":     nil,
		"bytesProcessed": 0,
		"matches":        nil,
		"status":         "failed",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, data, 0o600); err != nil {
		t.Fatal(err)
	}

	manifest, err := LoadManifest(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.TempOutput != "" || manifest.CompletedAt != nil || manifest.SourceSize != 0 || manifest.BytesProcessed != 0 || manifest.Matches != 0 {
		t.Fatalf("null/zero fields decoded as %#v", manifest)
	}
}

func TestFindRecoveryStatesRejectsCandidateFlood(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.sql")
	writeRecoveryFile(t, sourcePath, "source")
	for i := 0; i <= maxRecoveryManifestCandidates; i++ {
		path := filepath.Join(dir, fmt.Sprintf("candidate-%04d.sql%s", i, recoveryManifestSuffix))
		writeRecoveryFile(t, path, "{}")
	}
	if _, err := FindRecoveryStates(sourcePath); !errors.Is(err, ErrRecoveryScanLimit) {
		t.Fatalf("FindRecoveryStates error = %v, want ErrRecoveryScanLimit", err)
	}
}

func TestLoadManifestRejectsSymlinkWithoutFollowingTarget(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.sql")
	outputPath := filepath.Join(dir, "output.sql")
	targetPath := filepath.Join(dir, "target.json")
	manifestPath := outputPath + recoveryManifestSuffix
	writeRecoveryManifest(t, targetPath, Manifest{
		Operation: "plain-replace", Source: sourcePath, Output: outputPath,
		StartedAt: time.Now().UTC(), Status: "failed",
	})
	before := readRecoveryFile(t, targetPath)
	if err := os.Symlink(targetPath, manifestPath); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := LoadManifest(manifestPath); !errors.Is(err, ErrInvalidRecoveryManifest) {
		t.Fatalf("LoadManifest error = %v, want ErrInvalidRecoveryManifest", err)
	}
	if after := readRecoveryFile(t, targetPath); after != before {
		t.Fatal("symlink target changed")
	}
}

func TestInspectRecoveryManifestRejectsForgedPathInstructions(t *testing.T) {
	for _, field := range []string{"source", "output", "temp", "backup", "planned backup"} {
		t.Run(field, func(t *testing.T) {
			dir := t.TempDir()
			foreignDir := t.TempDir()
			sourcePath := filepath.Join(dir, "source.sql")
			outputPath := filepath.Join(dir, "output.sql")
			tempPath := outputPath + ".quarry.tmp"
			backupPath := sourcePath + ".quarry.bak"
			foreignPath := filepath.Join(foreignDir, field+".sentinel")
			for path, body := range map[string]string{
				sourcePath: "source", outputPath: "destination", tempPath: "temp",
				backupPath: "backup", foreignPath: "foreign",
			} {
				writeRecoveryFile(t, path, body)
			}
			manifest := Manifest{
				Operation: "plain-replace", Source: sourcePath, Output: outputPath,
				TempOutput: tempPath, StartedAt: time.Now().UTC(), Status: "failed",
				Phase: "ready_to_finalize",
			}
			switch field {
			case "source":
				manifest.Source = foreignPath
			case "output":
				manifest.Output = foreignPath
			case "temp":
				manifest.TempOutput = foreignPath
			case "backup":
				manifest.SwapRequested, manifest.Backup = true, foreignPath
			case "planned backup":
				manifest.SwapRequested, manifest.BackupPlanned = true, foreignPath
			}
			manifestPath := outputPath + recoveryManifestSuffix
			writeRecoveryManifest(t, manifestPath, manifest)
			if _, err := InspectRecoveryManifest(manifestPath); !errors.Is(err, ErrInvalidRecoveryManifest) {
				t.Fatalf("InspectRecoveryManifest error = %v, want ErrInvalidRecoveryManifest", err)
			}
			assertRecoveryFile(t, sourcePath, "source")
			assertRecoveryFile(t, outputPath, "destination")
			assertRecoveryFile(t, tempPath, "temp")
			assertRecoveryFile(t, backupPath, "backup")
			assertRecoveryFile(t, foreignPath, "foreign")
		})
	}
}

func TestInspectRecoveryManifestRejectsSymlinkArtifacts(t *testing.T) {
	dir := t.TempDir()
	sourceTarget := filepath.Join(dir, "source-target.sql")
	sourcePath := filepath.Join(dir, "source.sql")
	outputPath := filepath.Join(dir, "output.sql")
	writeRecoveryFile(t, sourceTarget, "source target")
	if err := os.Symlink(sourceTarget, sourcePath); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	manifestPath := outputPath + recoveryManifestSuffix
	writeRecoveryManifest(t, manifestPath, Manifest{
		Operation: "plain-replace", Source: sourcePath, Output: outputPath,
		StartedAt: time.Now().UTC(), Status: "failed",
	})
	if _, err := InspectRecoveryManifest(manifestPath); !errors.Is(err, ErrInvalidRecoveryManifest) {
		t.Fatalf("InspectRecoveryManifest error = %v, want ErrInvalidRecoveryManifest", err)
	}
	assertRecoveryFile(t, sourceTarget, "source target")
}

func TestRecoveryMutationAPIsFailClosedAndPreserveArtifacts(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.sql")
	outputPath := filepath.Join(dir, "output.sql")
	tempPath := outputPath + ".quarry.tmp"
	manifestPath := outputPath + recoveryManifestSuffix
	writeRecoveryFile(t, sourcePath, "source")
	writeRecoveryFile(t, tempPath, "temp output")
	writeRecoveryManifest(t, manifestPath, Manifest{
		Operation: "plain-replace", Source: sourcePath, Output: outputPath,
		TempOutput: tempPath, Phase: "ready_to_finalize",
		StartedAt: time.Now().UTC(), SourceSize: int64(len("source")), Status: "failed",
	})
	state, err := InspectRecoveryManifest(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	manifestBefore := readRecoveryFile(t, manifestPath)
	if can, reason := CanResumeRecovery(state); can || reason != ErrRecoveryMutationDisabled.Error() {
		t.Fatalf("CanResumeRecovery = %v, %q", can, reason)
	}
	if _, err := ResumeRecovery(state); !errors.Is(err, ErrRecoveryMutationDisabled) {
		t.Fatalf("ResumeRecovery error = %v, want ErrRecoveryMutationDisabled", err)
	}
	if err := DeleteRecoveryTemp(state); !errors.Is(err, ErrRecoveryMutationDisabled) {
		t.Fatalf("DeleteRecoveryTemp error = %v, want ErrRecoveryMutationDisabled", err)
	}
	assertRecoveryFile(t, sourcePath, "source")
	assertRecoveryFile(t, tempPath, "temp output")
	if _, err := os.Lstat(outputPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("output should remain absent: %v", err)
	}
	if manifestAfter := readRecoveryFile(t, manifestPath); manifestAfter != manifestBefore {
		t.Fatal("manifest changed after refused recovery")
	}
}

func TestRecoveryMutationAPIsIgnoreFabricatedStatePaths(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.sql")
	outputPath := filepath.Join(dir, "destination.sql")
	tempPath := filepath.Join(dir, "temp.sql")
	backupPath := filepath.Join(dir, "backup.sql")
	for path, body := range map[string]string{
		sourcePath: "source", outputPath: "destination", tempPath: "temp", backupPath: "backup",
	} {
		writeRecoveryFile(t, path, body)
	}
	state := RecoveryState{ManifestPath: filepath.Join(dir, "forged.json"), Manifest: Manifest{
		Source: sourcePath, Output: outputPath, TempOutput: tempPath, Backup: backupPath,
	}}
	if _, err := ResumeRecovery(state); !errors.Is(err, ErrRecoveryMutationDisabled) {
		t.Fatalf("ResumeRecovery error = %v", err)
	}
	if err := DeleteRecoveryTemp(state); !errors.Is(err, ErrRecoveryMutationDisabled) {
		t.Fatalf("DeleteRecoveryTemp error = %v", err)
	}
	if _, _, ok := RecoveryOpenPath(state); ok {
		t.Fatal("fabricated state produced an open path")
	}
	assertRecoveryFile(t, sourcePath, "source")
	assertRecoveryFile(t, outputPath, "destination")
	assertRecoveryFile(t, tempPath, "temp")
	assertRecoveryFile(t, backupPath, "backup")
}

func TestInspectRecoveryManifestAndOpenPathUseValidatedArtifacts(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.sql")
	outputPath := filepath.Join(dir, "output.sql")
	tempPath := outputPath + ".quarry.tmp"
	writeRecoveryFile(t, sourcePath, "source")
	writeRecoveryFile(t, tempPath, "partial")
	manifestPath := outputPath + recoveryManifestSuffix
	writeRecoveryManifest(t, manifestPath, Manifest{
		Operation: "plain-replace", Source: sourcePath, Output: outputPath,
		TempOutput: tempPath, StartedAt: time.Now().UTC(), Status: "canceled",
	})
	state, err := InspectRecoveryManifest(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	path, label, ok := RecoveryOpenPath(state)
	if !ok || path != tempPath || label != "partial temp output" {
		t.Fatalf("RecoveryOpenPath = %q, %q, %v", path, label, ok)
	}
}

func TestCleanupCompletedRecoveryManifestsRequiresValidatedState(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(2_000_000, 0).UTC()
	oldCompletedAt := now.Add(-72 * time.Hour)
	recentCompletedAt := now.Add(-time.Hour)
	sourcePath := filepath.Join(dir, "source.sql")
	writeRecoveryFile(t, sourcePath, "source")

	makeState := func(name, status string, completedAt *time.Time) RecoveryState {
		outputPath := filepath.Join(dir, name)
		manifestPath := outputPath + recoveryManifestSuffix
		writeRecoveryManifest(t, manifestPath, Manifest{
			Operation: "plain-replace", Source: sourcePath, Output: outputPath,
			StartedAt: now.Add(-96 * time.Hour), CompletedAt: completedAt, Status: status,
		})
		state, err := InspectRecoveryManifest(manifestPath)
		if err != nil {
			t.Fatal(err)
		}
		return state
	}
	old := makeState("old.sql", "complete", &oldCompletedAt)
	recent := makeState("recent.sql", "complete", &recentCompletedAt)
	running := makeState("running.sql", "running", nil)

	removed, err := CleanupCompletedRecoveryManifests([]RecoveryState{
		old, recent, running,
		{ManifestPath: old.ManifestPath, Manifest: old.Manifest}, // fabricated copy has no validation authority
	}, 48*time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}
	if _, err := os.Lstat(old.ManifestPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old manifest should be removed: %v", err)
	}
	for _, path := range []string{recent.ManifestPath, running.ManifestPath} {
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("%s should remain: %v", path, err)
		}
	}
}

func writeRecoveryManifest(t *testing.T, path string, manifest Manifest) {
	t.Helper()
	if err := writeManifest(path, manifest, true); err != nil {
		t.Fatal(err)
	}
}

func writeRecoveryFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readRecoveryFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func assertRecoveryFile(t *testing.T, path, want string) {
	t.Helper()
	if got := readRecoveryFile(t, path); got != want {
		t.Fatalf("%s = %q, want %q", path, got, want)
	}
}
