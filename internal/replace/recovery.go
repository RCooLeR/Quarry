package replace

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/quarry/quarry-wails3/internal/directoryfile"
	"github.com/quarry/quarry-wails3/internal/fileio"
	"github.com/quarry/quarry-wails3/internal/logger"
	"github.com/quarry/quarry-wails3/internal/regularfile"
)

// RecoveryState describes a manifest-backed replace artifact that Quarry can inspect.
type RecoveryState struct {
	ManifestPath string
	Manifest     Manifest
	InPlace      bool
	SourceExists bool
	OutputExists bool
	TempExists   bool
	BackupExists bool

	// validated is deliberately not exported. RecoveryState values assembled by
	// callers must not acquire authority to act on paths copied from JSON.
	validated bool
}

const (
	minRecoveryManifestRetention  = 24 * time.Hour
	maxRecoveryManifestBytes      = 64 << 10
	maxRecoveryPathBytes          = 32 << 10
	maxRecoveryDirectoryEntries   = 100_000
	maxRecoveryManifestCandidates = 1_024
	recoveryManifestSuffix        = ".quarry.manifest.json"
)

var (
	ErrInvalidRecoveryManifest  = errors.New("invalid replace recovery manifest")
	ErrRecoveryMutationDisabled = errors.New("automatic replace recovery mutation is disabled; inspect the preserved artifacts and choose an explicit safe output operation")
	ErrRecoveryScanLimit        = errors.New("replace recovery scan limit exceeded")
)

// LoadManifest reads and validates a bounded replace manifest from disk. The
// manifest is untrusted input: the leaf itself must be a stable regular file,
// unknown fields and trailing JSON values are rejected, and artifact paths must
// match Quarry's deterministic sibling naming scheme.
func LoadManifest(path string) (Manifest, error) {
	// Recovery manifests are source-adjacent, untrusted inputs. The atomic
	// journal checksum detects corruption but does not authenticate provenance;
	// loading a manifest therefore classifies pending evidence without mutating
	// any pathname.
	if err := fileio.RequireAtomicWriteReadReady(path); err != nil {
		return Manifest{}, fmt.Errorf("replace manifest has unresolved atomic-write recovery data: %w", err)
	}
	before, err := os.Lstat(path)
	if err != nil {
		return Manifest{}, err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return Manifest{}, fmt.Errorf("%w: manifest must be a regular file, not a symlink", ErrInvalidRecoveryManifest)
	}
	if before.Size() > maxRecoveryManifestBytes {
		return Manifest{}, fmt.Errorf("%w: manifest exceeds %d bytes", ErrInvalidRecoveryManifest, maxRecoveryManifestBytes)
	}

	f, err := regularfile.OpenNoFollow(path)
	if err != nil {
		return Manifest{}, err
	}
	defer f.Close()

	opened, err := f.Stat()
	if err != nil {
		return Manifest{}, err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(before, opened) || opened.Size() != before.Size() || !opened.ModTime().Equal(before.ModTime()) {
		return Manifest{}, fmt.Errorf("%w: manifest changed while it was opened", ErrInvalidRecoveryManifest)
	}
	if opened.Size() > maxRecoveryManifestBytes {
		return Manifest{}, fmt.Errorf("%w: manifest exceeds %d bytes", ErrInvalidRecoveryManifest, maxRecoveryManifestBytes)
	}

	data, err := io.ReadAll(io.LimitReader(f, maxRecoveryManifestBytes+1))
	if err != nil {
		return Manifest{}, err
	}
	if len(data) > maxRecoveryManifestBytes {
		return Manifest{}, fmt.Errorf("%w: manifest exceeds %d bytes", ErrInvalidRecoveryManifest, maxRecoveryManifestBytes)
	}
	after, err := f.Stat()
	if err != nil {
		return Manifest{}, err
	}
	if !os.SameFile(opened, after) || after.Size() != opened.Size() || !after.ModTime().Equal(opened.ModTime()) || int64(len(data)) != after.Size() {
		return Manifest{}, fmt.Errorf("%w: manifest changed while it was read", ErrInvalidRecoveryManifest)
	}

	manifest, err := decodeRecoveryManifest(data)
	if err != nil {
		return Manifest{}, err
	}
	if err := validateRecoveryManifest(path, manifest); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

// InspectRecoveryManifest loads a manifest and reports which related files still exist.
func InspectRecoveryManifest(path string) (RecoveryState, error) {
	manifest, err := LoadManifest(path)
	if err != nil {
		return RecoveryState{}, err
	}
	state := RecoveryState{
		ManifestPath: path,
		Manifest:     manifest,
		InPlace:      manifest.Operation == "plain-replace-in-place",
		validated:    true,
	}
	if exists, err := recoveryArtifactExists(manifest.Source); err != nil {
		return RecoveryState{}, err
	} else if exists {
		state.SourceExists = true
	}
	if manifest.Output != "" {
		if exists, err := recoveryArtifactExists(manifest.Output); err != nil {
			return RecoveryState{}, err
		} else if exists {
			state.OutputExists = true
		}
	}
	if manifest.TempOutput != "" {
		if exists, err := recoveryArtifactExists(manifest.TempOutput); err != nil {
			return RecoveryState{}, err
		} else if exists {
			state.TempExists = true
		}
	}
	if manifest.SwapRequested || manifest.Backup != "" || manifest.BackupPlanned != "" {
		backupPath := resolveRecoveryBackupPath(manifest)
		if backupPath != "" {
			if manifest.Backup == "" {
				state.Manifest.Backup = backupPath
			}
		}
		if exists, err := recoveryArtifactExists(backupPath); err != nil {
			return RecoveryState{}, err
		} else if exists {
			state.BackupExists = true
		}
	}
	return state, nil
}

// FindRecoveryStates returns replace manifests associated with sourcePath, newest first.
func FindRecoveryStates(sourcePath string) ([]RecoveryState, error) {
	dirPrefix, sourceBase := filepath.Split(sourcePath)
	dir := dirPrefix
	if dir == "" {
		dir = "."
	}
	paths := map[string]struct{}{}

	legacyInPlacePath := sourcePath + ".quarry.inplace.manifest.json"
	paths[legacyInPlacePath] = struct{}{}

	dirHandle, err := directoryfile.Open(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer dirHandle.Close()

	inPlacePrefix := sourceBase + ".quarry.inplace."
	entriesScanned := 0
	for {
		entries, readErr := dirHandle.ReadDir(256)
		for _, entry := range entries {
			entriesScanned++
			if entriesScanned > maxRecoveryDirectoryEntries {
				return nil, fmt.Errorf("%w: directory contains more than %d entries", ErrRecoveryScanLimit, maxRecoveryDirectoryEntries)
			}
			if entry.IsDir() {
				continue
			}
			name := entry.Name()
			isInPlace := strings.HasPrefix(name, inPlacePrefix) && strings.HasSuffix(name, ".manifest.json")
			isOutput := strings.HasSuffix(name, recoveryManifestSuffix)
			if !isInPlace && !isOutput {
				continue
			}
			paths[dirPrefix+name] = struct{}{}
			if len(paths) > maxRecoveryManifestCandidates {
				return nil, fmt.Errorf("%w: more than %d candidate manifests", ErrRecoveryScanLimit, maxRecoveryManifestCandidates)
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return nil, readErr
		}
	}

	states := make([]RecoveryState, 0, len(paths))
	for path := range paths {
		state, err := InspectRecoveryManifest(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			// Ignore malformed or unrelated recovery files instead of breaking the UI.
			logger.Warn("replace.recovery", "skipping recovery manifest", map[string]string{
				"path":  path,
				"error": err.Error(),
			})
			continue
		}
		if state.Manifest.Source != sourcePath {
			continue
		}
		states = append(states, state)
	}

	sort.Slice(states, func(i, j int) bool {
		if states[i].Manifest.StartedAt.Equal(states[j].Manifest.StartedAt) {
			return states[i].ManifestPath < states[j].ManifestPath
		}
		return states[i].Manifest.StartedAt.After(states[j].Manifest.StartedAt)
	})
	return states, nil
}

// DeleteRecoveryTemp previously removed a path supplied by an unsigned legacy
// manifest. Legacy manifests are now inspection-only because they cannot prove
// that their path instructions were created by Quarry.
func DeleteRecoveryTemp(state RecoveryState) error {
	_ = state
	return ErrRecoveryMutationDisabled
}

// CleanupCompletedRecoveryManifests removes completed manifest files older than
// retention while keeping recent audit records and all incomplete recovery
// states. Retention is clamped to at least 24 hours to avoid surprise cleanup.
func CleanupCompletedRecoveryManifests(states []RecoveryState, retention time.Duration, now time.Time) (int, error) {
	if retention < minRecoveryManifestRetention {
		retention = minRecoveryManifestRetention
	}
	removed := 0
	for _, state := range states {
		if !state.validated || state.ManifestPath == "" || state.Manifest.Status != "complete" || state.Manifest.CompletedAt == nil {
			continue
		}
		refreshed, err := InspectRecoveryManifest(state.ManifestPath)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return removed, err
		}
		if refreshed.Manifest.Status != "complete" || refreshed.Manifest.CompletedAt == nil || now.Sub(*refreshed.Manifest.CompletedAt) < retention {
			continue
		}
		if err := removePath(refreshed.ManifestPath); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return removed, err
		}
		removed++
	}
	return removed, nil
}

// RecoveryOpenPath returns the most useful artifact path to inspect from a recovery manifest.
func RecoveryOpenPath(state RecoveryState) (string, string, bool) {
	if !state.validated {
		return "", "", false
	}
	switch {
	case state.TempExists:
		return state.Manifest.TempOutput, "partial temp output", true
	case state.OutputExists && state.Manifest.Output != "" && state.Manifest.Output != state.Manifest.Source:
		return state.Manifest.Output, "output file", true
	case state.BackupExists:
		return state.Manifest.Backup, "backup file", true
	default:
		return "", "", false
	}
}

// CanResumeRecovery reports whether a recovery manifest can be safely resumed.
func CanResumeRecovery(state RecoveryState) (bool, string) {
	_ = state
	return false, ErrRecoveryMutationDisabled.Error()
}

// ResumeRecovery is intentionally fail-closed for unsigned legacy manifests.
// The artifacts remain untouched so the user can inspect them and run a new,
// explicitly chosen safe-output operation.
func ResumeRecovery(state RecoveryState) (RecoveryState, error) {
	return state, ErrRecoveryMutationDisabled
}

func resolveRecoveryBackupPath(manifest Manifest) string {
	backupPath := manifest.BackupPlanned
	if backupPath == "" {
		backupPath = manifest.Backup
	}
	if backupPath == "" {
		backupPath = manifest.Source + ".quarry.bak"
	}
	return backupPath
}

func recoveryArtifactExists(path string) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return false, fmt.Errorf("%w: recovery artifact is not a regular file: %s", ErrInvalidRecoveryManifest, path)
	}
	return true, nil
}

func decodeRecoveryManifest(data []byte) (Manifest, error) {
	keys := json.NewDecoder(bytes.NewReader(data))
	token, err := keys.Token()
	if err != nil {
		return Manifest{}, fmt.Errorf("%w: %v", ErrInvalidRecoveryManifest, err)
	}
	if delimiter, ok := token.(json.Delim); !ok || delimiter != '{' {
		return Manifest{}, fmt.Errorf("%w: top-level JSON value must be an object", ErrInvalidRecoveryManifest)
	}
	seen := make(map[string]struct{}, 20)
	for keys.More() {
		keyToken, err := keys.Token()
		if err != nil {
			return Manifest{}, fmt.Errorf("%w: %v", ErrInvalidRecoveryManifest, err)
		}
		key, ok := keyToken.(string)
		if !ok {
			return Manifest{}, fmt.Errorf("%w: manifest field name is not a string", ErrInvalidRecoveryManifest)
		}
		if _, duplicate := seen[key]; duplicate {
			return Manifest{}, fmt.Errorf("%w: duplicate field %q", ErrInvalidRecoveryManifest, key)
		}
		seen[key] = struct{}{}
		var value json.RawMessage
		if err := keys.Decode(&value); err != nil {
			return Manifest{}, fmt.Errorf("%w: %v", ErrInvalidRecoveryManifest, err)
		}
	}
	if _, err := keys.Token(); err != nil {
		return Manifest{}, fmt.Errorf("%w: %v", ErrInvalidRecoveryManifest, err)
	}
	if err := keys.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		return Manifest{}, fmt.Errorf("%w: trailing data: %v", ErrInvalidRecoveryManifest, err)
	}

	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var manifest Manifest
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, fmt.Errorf("%w: %v", ErrInvalidRecoveryManifest, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		return Manifest{}, fmt.Errorf("%w: trailing data: %v", ErrInvalidRecoveryManifest, err)
	}
	return manifest, nil
}

func validateRecoveryManifest(manifestPath string, manifest Manifest) error {
	if len(manifest.Source) == 0 || len(manifest.Source) > maxRecoveryPathBytes {
		return fmt.Errorf("%w: invalid source path", ErrInvalidRecoveryManifest)
	}
	for label, value := range map[string]string{
		"output": manifest.Output, "temp output": manifest.TempOutput,
		"backup": manifest.Backup, "planned backup": manifest.BackupPlanned,
	} {
		if len(value) > maxRecoveryPathBytes {
			return fmt.Errorf("%w: %s path is too long", ErrInvalidRecoveryManifest, label)
		}
	}
	if manifest.SourceSize < 0 || manifest.SourceModTime < 0 || manifest.BytesProcessed < 0 || manifest.Matches < 0 || manifest.ConflictCount < 0 {
		return fmt.Errorf("%w: negative counters are not allowed", ErrInvalidRecoveryManifest)
	}
	if manifest.StartedAt.IsZero() {
		return fmt.Errorf("%w: startedAt is required", ErrInvalidRecoveryManifest)
	}
	if !validRecoveryStatus(manifest.Status) {
		return fmt.Errorf("%w: unsupported status %q", ErrInvalidRecoveryManifest, manifest.Status)
	}
	if !validRecoveryPhase(manifest.Phase) {
		return fmt.Errorf("%w: unsupported phase %q", ErrInvalidRecoveryManifest, manifest.Phase)
	}

	manifestAbs, err := cleanAbsoluteRecoveryPath(manifestPath)
	if err != nil {
		return err
	}
	sourceAbs, err := cleanAbsoluteRecoveryPath(manifest.Source)
	if err != nil {
		return err
	}

	if manifest.Operation == "plain-replace-in-place" {
		if manifest.Output != manifest.Source || manifest.TempOutput != "" || manifest.Backup != "" || manifest.BackupPlanned != "" || manifest.SwapRequested || manifest.Swapped {
			return fmt.Errorf("%w: invalid in-place artifact fields", ErrInvalidRecoveryManifest)
		}
		if !validInPlaceManifestPath(manifestAbs, sourceAbs) {
			return fmt.Errorf("%w: in-place manifest is not bound to its source", ErrInvalidRecoveryManifest)
		}
		return nil
	}
	if !validRecoveryOperation(manifest.Operation) {
		return fmt.Errorf("%w: unsupported operation %q", ErrInvalidRecoveryManifest, manifest.Operation)
	}
	if manifest.Output == "" {
		return fmt.Errorf("%w: output path is required", ErrInvalidRecoveryManifest)
	}
	outputAbs, err := cleanAbsoluteRecoveryPath(manifest.Output)
	if err != nil {
		return err
	}
	if manifestAbs != outputAbs+recoveryManifestSuffix {
		return fmt.Errorf("%w: manifest is not bound to its output", ErrInvalidRecoveryManifest)
	}
	if sourceAbs == outputAbs || filepath.Dir(sourceAbs) != filepath.Dir(outputAbs) {
		return fmt.Errorf("%w: source and output must be distinct siblings", ErrInvalidRecoveryManifest)
	}
	if manifest.TempOutput != "" {
		tempAbs, err := cleanAbsoluteRecoveryPath(manifest.TempOutput)
		if err != nil {
			return err
		}
		if tempAbs != outputAbs+".quarry.tmp" {
			return fmt.Errorf("%w: temp output is not bound to its output", ErrInvalidRecoveryManifest)
		}
	}

	expectedBackup := sourceAbs + ".quarry.bak"
	for _, backup := range []string{manifest.Backup, manifest.BackupPlanned} {
		if backup == "" {
			continue
		}
		backupAbs, err := cleanAbsoluteRecoveryPath(backup)
		if err != nil {
			return err
		}
		if backupAbs != expectedBackup {
			return fmt.Errorf("%w: backup is not bound to its source", ErrInvalidRecoveryManifest)
		}
	}
	if manifest.Backup != "" && manifest.BackupPlanned != "" {
		backupAbs, _ := cleanAbsoluteRecoveryPath(manifest.Backup)
		plannedAbs, _ := cleanAbsoluteRecoveryPath(manifest.BackupPlanned)
		if backupAbs != plannedAbs {
			return fmt.Errorf("%w: backup paths disagree", ErrInvalidRecoveryManifest)
		}
	}
	if (manifest.Backup != "" || manifest.BackupPlanned != "" || manifest.Swapped) && !manifest.SwapRequested {
		return fmt.Errorf("%w: swap artifacts exist without a swap request", ErrInvalidRecoveryManifest)
	}
	return nil
}

func cleanAbsoluteRecoveryPath(path string) (string, error) {
	if path == "" || strings.IndexByte(path, 0) >= 0 {
		return "", fmt.Errorf("%w: invalid empty or NUL-containing path", ErrInvalidRecoveryManifest)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("%w: resolve path: %v", ErrInvalidRecoveryManifest, err)
	}
	return filepath.Clean(abs), nil
}

func validRecoveryOperation(operation string) bool {
	switch operation {
	case "plain-replace", "regex-replace", "batch-plain-replace", "batch-regex-replace", "encoding-convert", "line-ending-convert":
		return true
	default:
		return false
	}
}

func validRecoveryStatus(status string) bool {
	switch status {
	case "running", "failed", "canceled", "complete":
		return true
	default:
		return false
	}
}

func validRecoveryPhase(phase string) bool {
	switch phase {
	case "", "processing", "ready_to_finalize", "output_written", "swapped", "complete":
		return true
	default:
		return false
	}
}

func validInPlaceManifestPath(manifestPath string, sourcePath string) bool {
	if manifestPath == sourcePath+".quarry.inplace.manifest.json" {
		return true
	}
	prefix := sourcePath + ".quarry.inplace."
	if !strings.HasPrefix(manifestPath, prefix) || !strings.HasSuffix(manifestPath, ".manifest.json") {
		return false
	}
	stamp := strings.TrimSuffix(strings.TrimPrefix(manifestPath, prefix), ".manifest.json")
	_, err := time.Parse("20060102T150405.000000000Z", stamp)
	return err == nil
}
