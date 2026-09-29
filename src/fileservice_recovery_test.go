package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/quarry/quarry-wails3/internal/session"
)

func TestOpenFileLeavesAdjacentRecoveryArtifactsUntouched(t *testing.T) {
	source := []byte("CURRENT-SOURCE-BYTES")

	tests := []struct {
		name    string
		sidecar []byte
	}{
		{
			name: "legacy crafted uncommitted rollback",
			// If replayed, this valid-looking entry would replace the beginning
			// of the source with bytes supplied by the adjacent artifact.
			sidecar: encodeRecoverySidecarForTest(false, int64(len(source)), 0, []byte("FORGED")),
		},
		{
			name:    "invalid artifact",
			sidecar: []byte("not a Quarry recovery sidecar"),
		},
		{
			name:    "legacy committed artifact",
			sidecar: encodeRecoverySidecarForTest(true, int64(len(source)), 0, nil),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeTempFile(t, "source.txt", source)
			recoveryPath := sidecarPath(path)
			if err := os.WriteFile(recoveryPath, tt.sidecar, 0o600); err != nil {
				t.Fatal(err)
			}

			svc := NewFileService()
			meta, err := svc.OpenFile(path)
			if err == nil {
				if meta.FileID != "" {
					_ = svc.CloseFile(meta.FileID)
				}
				t.Fatal("OpenFile succeeded with adjacent recovery data")
			}
			if meta != (FileMeta{}) {
				t.Fatalf("OpenFile metadata = %+v, want zero value on blocked open", meta)
			}
			if !errors.Is(err, ErrInPlaceRecoveryPending) {
				t.Fatalf("OpenFile error = %v, want ErrInPlaceRecoveryPending", err)
			}
			var pending *InPlaceRecoveryPendingError
			if !errors.As(err, &pending) {
				t.Fatalf("OpenFile error type = %T, want *InPlaceRecoveryPendingError", err)
			}
			if pending.SidecarPath != recoveryPath {
				t.Fatalf("sidecar path = %q, want %q", pending.SidecarPath, recoveryPath)
			}
			if !pending.State.Detected || pending.State.Status != "invalid" || pending.State.ArtifactKind != "regular" {
				t.Fatalf("structured recovery state = %+v, want detected invalid regular artifact", pending.State)
			}
			if pending.State.RecommendedAction == "" {
				t.Fatal("blocked open did not provide a recommended recovery action")
			}

			gotSource, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if !bytes.Equal(gotSource, source) {
				t.Fatalf("source mutated during ordinary open: got %q, want %q", gotSource, source)
			}
			gotSidecar, readErr := os.ReadFile(recoveryPath)
			if readErr != nil {
				t.Fatalf("recovery artifact was not preserved: %v", readErr)
			}
			if !bytes.Equal(gotSidecar, tt.sidecar) {
				t.Fatal("recovery artifact changed during ordinary open")
			}
		})
	}
}

func TestInspectInPlaceRecoveryIsStructuredAndMutationFree(t *testing.T) {
	path := writeTempFile(t, "inspect.txt", []byte("important source"))
	svc := NewFileService()

	none, err := svc.InspectInPlaceRecovery(path)
	if err != nil {
		t.Fatal(err)
	}
	if none.Detected || none.Status != "none" || none.SidecarPath != sidecarPath(path) {
		t.Fatalf("no-artifact state = %+v", none)
	}

	evidence := []byte("malformed evidence must survive")
	if err := os.WriteFile(sidecarPath(path), evidence, 0o600); err != nil {
		t.Fatal(err)
	}
	state, err := svc.InspectInPlaceRecovery(path)
	if err != nil {
		t.Fatal(err)
	}
	if !state.Detected || state.Status != "invalid" || state.ArtifactSize != int64(len(evidence)) || state.CanRollback || state.CanClear {
		t.Fatalf("invalid-artifact state = %+v", state)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "important source" {
		t.Fatalf("inspection changed source: %q, %v", got, err)
	}
	if got, err := os.ReadFile(sidecarPath(path)); err != nil || !bytes.Equal(got, evidence) {
		t.Fatalf("inspection changed evidence: %q, %v", got, err)
	}
}

func TestInspectInPlaceRecoveryClassifiesUnsafeArtifactWithoutFollowingIt(t *testing.T) {
	path := writeTempFile(t, "unsafe.txt", []byte("source"))
	recoveryPath := sidecarPath(path)
	if err := os.Mkdir(recoveryPath, 0o700); err != nil {
		t.Fatal(err)
	}
	svc := NewFileService()
	state, err := svc.InspectInPlaceRecovery(path)
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != "unsafe_artifact" || state.ArtifactKind != "directory" || state.CanRollback || state.CanClear {
		t.Fatalf("unsafe-artifact state = %+v", state)
	}
}

func TestInspectInPlaceRecoveryPreservesExactPathSpelling(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, " source with spaces.txt")
	if err := os.WriteFile(path, []byte("exact spelling"), 0o600); err != nil {
		t.Fatal(err)
	}
	evidence := []byte("invalid but exact-path recovery evidence")
	if err := os.WriteFile(sidecarPath(path), evidence, 0o600); err != nil {
		t.Fatal(err)
	}

	svc := NewFileService()
	state, err := svc.InspectInPlaceRecovery(path)
	if err != nil {
		t.Fatal(err)
	}
	if state.SourcePath != path || state.SidecarPath != sidecarPath(path) || !state.Detected || state.Status != "invalid" {
		t.Fatalf("exact-path inspection = %+v, want source %q and sidecar %q", state, path, sidecarPath(path))
	}
	_, err = svc.OpenFile(path)
	var pending *InPlaceRecoveryPendingError
	if !errors.As(err, &pending) || pending.SidecarPath != sidecarPath(path) {
		t.Fatalf("exact-path blocked open error = %v", err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "exact spelling" {
		t.Fatalf("exact-path inspection changed source: %q, %v", got, err)
	}
}

func TestOpenFileWithoutRecoveryArtifactStillOpens(t *testing.T) {
	path := writeTempFile(t, "ordinary.txt", []byte("ordinary source"))
	svc := NewFileService()

	meta, err := svc.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.CloseFile(meta.FileID)
	if meta.FileID == "" {
		t.Fatal("OpenFile returned an empty file id")
	}
}

func TestOpenFileRejectsRecoveryAppearingAfterRegistryAdmission(t *testing.T) {
	path := writeTempFile(t, "admission-race.txt", []byte("important source"))
	recoveryPath := sidecarPath(path)
	evidence := []byte("concurrent recovery evidence")
	svc := NewFileService()

	originalHook := openFilePostRegistryHook
	var admittedFileID string
	hookCalls := 0
	openFilePostRegistryHook = func(file *session.File, added bool) {
		if file == nil || file.Path != path || !added {
			return
		}
		hookCalls++
		admittedFileID = file.ID
		if err := os.WriteFile(recoveryPath, evidence, 0o600); err != nil {
			t.Fatalf("create recovery evidence at admission seam: %v", err)
		}
	}
	t.Cleanup(func() { openFilePostRegistryHook = originalHook })

	meta, err := svc.OpenFile(path)
	if meta != (FileMeta{}) {
		t.Fatalf("metadata = %+v, want zero after recovery refusal", meta)
	}
	if !errors.Is(err, ErrInPlaceRecoveryPending) {
		t.Fatalf("error = %v, want ErrInPlaceRecoveryPending", err)
	}
	if hookCalls != 1 || admittedFileID == "" {
		t.Fatalf("admission hook calls/file = %d/%q", hookCalls, admittedFileID)
	}
	if _, retained := svc.reg.Get(admittedFileID); retained {
		t.Fatalf("newly admitted session %q survived recovery refusal", admittedFileID)
	}
	if got, readErr := os.ReadFile(path); readErr != nil || string(got) != "important source" {
		t.Fatalf("source changed: %q, %v", got, readErr)
	}
	if got, readErr := os.ReadFile(recoveryPath); readErr != nil || !bytes.Equal(got, evidence) {
		t.Fatalf("recovery evidence changed: %q, %v", got, readErr)
	}
}

func TestOpenFileRecoveryRecheckDoesNotCloseExistingSession(t *testing.T) {
	path := writeTempFile(t, "existing-session.txt", []byte("important source"))
	recoveryPath := sidecarPath(path)
	svc := NewFileService()
	first, err := svc.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Remove(recoveryPath)
		if _, retained := svc.reg.Get(first.FileID); retained {
			_ = svc.CloseFile(first.FileID)
		}
	})

	originalHook := openFilePostRegistryHook
	hookCalls := 0
	openFilePostRegistryHook = func(file *session.File, added bool) {
		if file == nil || file.ID != first.FileID || added {
			return
		}
		hookCalls++
		if err := os.WriteFile(recoveryPath, []byte("pending"), 0o600); err != nil {
			t.Fatalf("create recovery evidence at existing-session seam: %v", err)
		}
	}
	t.Cleanup(func() { openFilePostRegistryHook = originalHook })

	meta, err := svc.OpenFile(path)
	if meta != (FileMeta{}) || !errors.Is(err, ErrInPlaceRecoveryPending) {
		t.Fatalf("second open = %+v, %v; want recovery refusal", meta, err)
	}
	if hookCalls != 1 {
		t.Fatalf("admission hook calls = %d, want 1", hookCalls)
	}
	if retained, ok := svc.reg.Get(first.FileID); !ok || retained == nil {
		t.Fatal("recovery refusal closed the pre-existing session")
	}
}

func TestRefreshFileRejectsRecoveryAppearingAfterCandidateOpen(t *testing.T) {
	path := writeTempFile(t, "refresh-race.txt", []byte("important source"))
	recoveryPath := sidecarPath(path)
	evidence := []byte("recovery appeared during refresh")
	svc := NewFileService()
	meta, err := svc.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	before, ok := svc.reg.Snapshot(meta.FileID)
	if !ok {
		t.Fatal("opened session snapshot is missing")
	}
	t.Cleanup(func() {
		_ = os.Remove(recoveryPath)
		if _, retained := svc.reg.Get(meta.FileID); retained {
			_ = svc.CloseFile(meta.FileID)
		}
	})

	hookCalls := 0
	svc.refreshFileCandidate = func(candidate *session.File) {
		if candidate == nil || candidate.ID != meta.FileID {
			return
		}
		hookCalls++
		if err := os.WriteFile(recoveryPath, evidence, 0o600); err != nil {
			t.Fatalf("create recovery evidence at refresh seam: %v", err)
		}
	}
	refreshed, err := svc.RefreshFile(meta.FileID)
	if refreshed != (FileMeta{}) || !errors.Is(err, ErrInPlaceRecoveryPending) {
		t.Fatalf("RefreshFile = %+v, %v; want recovery refusal", refreshed, err)
	}
	if hookCalls != 1 {
		t.Fatalf("candidate hook calls = %d, want 1", hookCalls)
	}
	after, ok := svc.reg.Snapshot(meta.FileID)
	if !ok || after.Generation != before.Generation || after.Doc != before.Doc {
		t.Fatalf("rejected refresh replaced generation: before=%+v after=%+v present=%v", before, after, ok)
	}
	if got, readErr := os.ReadFile(path); readErr != nil || string(got) != "important source" {
		t.Fatalf("source changed: %q, %v", got, readErr)
	}
	if got, readErr := os.ReadFile(recoveryPath); readErr != nil || !bytes.Equal(got, evidence) {
		t.Fatalf("recovery evidence changed: %q, %v", got, readErr)
	}
}

func encodeRecoverySidecarForTest(committed bool, fileSize, offset int64, old []byte) []byte {
	var b bytes.Buffer
	b.Write([]byte{'Q', 'R', 'Y', 'R', 'P', 0, 0, 1})
	if committed {
		b.WriteByte(1)
	} else {
		b.WriteByte(0)
	}
	_ = binary.Write(&b, binary.LittleEndian, fileSize)
	count := int64(0)
	if old != nil {
		count = 1
	}
	_ = binary.Write(&b, binary.LittleEndian, count)
	if old != nil {
		_ = binary.Write(&b, binary.LittleEndian, offset)
		_ = binary.Write(&b, binary.LittleEndian, int64(len(old)))
		b.Write(old)
	}
	return b.Bytes()
}
