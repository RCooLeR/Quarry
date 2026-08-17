package main

import (
	"bytes"
	"errors"
	"math"
	"os"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/quarry/quarry-wails3/internal/encodingx"
)

func TestBoundedEditWindowEndCannotOverflow(t *testing.T) {
	tests := []struct {
		start  int64
		size   int64
		budget int64
		want   int64
	}{
		{start: math.MaxInt64 - 8, size: math.MaxInt64, budget: 1 << 20, want: math.MaxInt64},
		{start: 10, size: 100, budget: 20, want: 30},
		{start: 100, size: 100, budget: 20, want: 100},
	}
	for _, tt := range tests {
		if got := boundedEditWindowEnd(tt.start, tt.size, tt.budget); got != tt.want {
			t.Fatalf("boundedEditWindowEnd(%d,%d,%d) = %d, want %d", tt.start, tt.size, tt.budget, got, tt.want)
		}
	}
}

func TestEditRPCsRejectReadOnlyFormats(t *testing.T) {
	tests := []struct {
		name string
		data []byte
	}{
		{name: "CRLF", data: []byte("alpha\r\nbeta\r\n")},
		{name: "CR", data: []byte("alpha\rbeta\r")},
		{name: "mixed line endings", data: []byte("alpha\nbeta\r\ngamma\r")},
		{name: "binary", data: []byte{'a', 0, 'b', 0, 'c'}},
		{name: "UTF-16LE", data: append(encodingx.BOMBytes("UTF-16LE"), mustEncodeForEditSafetyTest(t, "UTF-16LE", "alpha\nbeta\n")...)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeTempFile(t, "read-only.dat", tt.data)
			svc := NewFileService()
			meta, err := svc.OpenFile(path)
			if err != nil {
				t.Fatal(err)
			}
			defer svc.CloseFile(meta.FileID)
			if meta.Editable {
				t.Fatalf("metadata unexpectedly marks %s input editable", tt.name)
			}

			if _, err := svc.GetEditWindow(meta.FileID, 0, 1024); !errors.Is(err, ErrFileNotEditable) {
				t.Fatalf("GetEditWindow error = %v, want ErrFileNotEditable", err)
			}
			if _, err := svc.GetDiffWindow(meta.FileID, 0, 1024); !errors.Is(err, ErrFileNotEditable) {
				t.Fatalf("GetDiffWindow error = %v, want ErrFileNotEditable", err)
			}
			if _, err := svc.StageEdit(meta.FileID, 0, 1, "x"); !errors.Is(err, ErrFileNotEditable) {
				t.Fatalf("StageEdit error = %v, want ErrFileNotEditable", err)
			}
			state, err := svc.GetStagingState(meta.FileID)
			if err != nil {
				t.Fatal(err)
			}
			if state.EditCount != 0 {
				t.Fatalf("rejected edit created staging state: %+v", state)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, tt.data) {
				t.Fatal("rejected edit changed source bytes")
			}
		})
	}
}

func TestEditRPCsRejectUnsupportedBytesBeyondMetadataSample(t *testing.T) {
	tests := []struct {
		name   string
		suffix []byte
	}{
		{name: "CRLF", suffix: []byte{'\r', '\n', 'z'}},
		{name: "NUL", suffix: []byte{0, 'z'}},
		{name: "invalid UTF-8", suffix: []byte{0xff, 'z'}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Detection samples the first 1 MiB. Put a normal LF after that sample
			// so the requested bounded window begins just before the unsupported
			// bytes without relying on pathological long-line navigation.
			prefix := append(bytes.Repeat([]byte{'a'}, 1<<20), bytes.Repeat([]byte{'b'}, 64)...)
			prefix = append(prefix, '\n')
			lateOffset := int64(len(prefix))
			data := append(append([]byte(nil), prefix...), tt.suffix...)
			path := writeTempFile(t, "late-unsupported.txt", data)
			svc := NewFileService()
			meta, err := svc.OpenFile(path)
			if err != nil {
				t.Fatal(err)
			}
			defer svc.CloseFile(meta.FileID)
			mustPrepareEditSession(t, svc, meta.FileID)

			if _, err := svc.GetEditWindow(meta.FileID, lateOffset+1, 1024); !errors.Is(err, ErrFileNotEditable) {
				t.Fatalf("GetEditWindow error = %v, want ErrFileNotEditable", err)
			}
			if _, err := svc.GetDiffWindow(meta.FileID, lateOffset+1, 1024); !errors.Is(err, ErrFileNotEditable) {
				t.Fatalf("GetDiffWindow error = %v, want ErrFileNotEditable", err)
			}
			if _, err := svc.StageEdit(meta.FileID, lateOffset, int64(len(tt.suffix)), strings.Repeat("x", len(tt.suffix))); !errors.Is(err, ErrFileNotEditable) {
				t.Fatalf("StageEdit error = %v, want ErrFileNotEditable", err)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, data) {
				t.Fatal("rejected late unsupported edit changed source bytes")
			}
		})
	}
}

func TestRefreshFilePreservesStagingAndRecoveryEvidence(t *testing.T) {
	original := []byte("alpha\nbeta\n")
	path := writeTempFile(t, "refresh.txt", original)
	svc := NewFileService()
	meta, err := svc.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.CloseFile(meta.FileID)
	mustPrepareEditSession(t, svc, meta.FileID)
	before, err := svc.StageEdit(meta.FileID, 0, 5, "omega")
	if err != nil {
		t.Fatal(err)
	}

	recoveryPath := sidecarPath(path)
	evidence := []byte("preserve recovery evidence")
	if err := os.WriteFile(recoveryPath, evidence, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RefreshFile(meta.FileID); !errors.Is(err, ErrInPlaceRecoveryPending) {
		t.Fatalf("RefreshFile recovery error = %v, want ErrInPlaceRecoveryPending", err)
	}
	assertRefreshSafetyState(t, svc, meta.FileID, path, recoveryPath, original, evidence, before)

	if err := os.Remove(recoveryPath); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RefreshFile(meta.FileID); !errors.Is(err, ErrStagedEditsPending) {
		t.Fatalf("RefreshFile staging error = %v, want ErrStagedEditsPending", err)
	}
	after, err := svc.GetStagingState(meta.FileID)
	if err != nil {
		t.Fatal(err)
	}
	if after.EditCount != before.EditCount || after.EditedSize != before.EditedSize {
		t.Fatalf("blocked refresh discarded staging: before=%+v after=%+v", before, after)
	}
}

func TestCloseFileRejectsStagedEditsAndPreservesSession(t *testing.T) {
	original := []byte("alpha\nbeta\n")
	path := writeTempFile(t, "close-dirty.txt", original)
	svc := NewFileService()
	meta, err := svc.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	t.Cleanup(func() {
		if !closed {
			_, _ = svc.DiscardEdits(meta.FileID)
			_ = svc.CloseFile(meta.FileID)
		}
	})
	mustPrepareEditSession(t, svc, meta.FileID)

	before, err := svc.StageEdit(meta.FileID, 0, 5, "omega")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.CloseFile(meta.FileID); !errors.Is(err, ErrStagedEditsPending) {
		t.Fatalf("CloseFile error = %v, want ErrStagedEditsPending", err)
	}
	after, err := svc.GetStagingState(meta.FileID)
	if err != nil {
		t.Fatalf("rejected close removed the session: %v", err)
	}
	if after.EditCount != before.EditCount || after.EditedSize != before.EditedSize {
		t.Fatalf("blocked close discarded staging: before=%+v after=%+v", before, after)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, original) {
		t.Fatalf("blocked close changed source: got %q want %q", got, original)
	}

	if _, err := svc.DiscardEdits(meta.FileID); err != nil {
		t.Fatal(err)
	}
	if err := svc.CloseFile(meta.FileID); err != nil {
		t.Fatal(err)
	}
	closed = true
}

func TestDiffWindowMapsLengthChangingEditedCoordinatesToSource(t *testing.T) {
	t.Run("insertion before requested window", func(t *testing.T) {
		const source = "zero\nalpha\nbeta\ngamma\n"
		path := writeTempFile(t, "diff-insert.txt", []byte(source))
		svc := NewFileService()
		meta, err := svc.OpenFile(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_, _ = svc.DiscardEdits(meta.FileID)
			_ = svc.CloseFile(meta.FileID)
		})
		mustPrepareEditSession(t, svc, meta.FileID)
		const inserted = "prefix\n"
		if _, err := svc.StageEdit(meta.FileID, 0, 0, inserted); err != nil {
			t.Fatal(err)
		}

		editedStart := int64(len(inserted + "zero\n"))
		window, err := svc.GetDiffWindow(meta.FileID, editedStart, 128)
		if err != nil {
			t.Fatal(err)
		}
		const want = "alpha\nbeta\ngamma\n"
		if window.Edited != want || window.Original != want {
			t.Fatalf("mapped diff = original %q edited %q, want %q", window.Original, window.Edited, want)
		}
		if window.StartByte != editedStart || window.EditedStartByte != editedStart || window.OriginalStartByte != int64(len("zero\n")) {
			t.Fatalf("mapped starts = legacy %d edited %d original %d", window.StartByte, window.EditedStartByte, window.OriginalStartByte)
		}
		if window.NextByte != window.EditedNextByte || window.OriginalNextByte != int64(len(source)) {
			t.Fatalf("mapped ends = legacy %d edited %d original %d", window.NextByte, window.EditedNextByte, window.OriginalNextByte)
		}
	})

	t.Run("deletion before and inside windows", func(t *testing.T) {
		const source = "zero\nremove\nalpha\nbeta\n"
		path := writeTempFile(t, "diff-delete.txt", []byte(source))
		svc := NewFileService()
		meta, err := svc.OpenFile(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_, _ = svc.DiscardEdits(meta.FileID)
			_ = svc.CloseFile(meta.FileID)
		})
		mustPrepareEditSession(t, svc, meta.FileID)
		removeStart := int64(len("zero\n"))
		if _, err := svc.StageEdit(meta.FileID, removeStart, int64(len("remove\n")), ""); err != nil {
			t.Fatal(err)
		}

		afterDeletion, err := svc.GetDiffWindow(meta.FileID, removeStart, 128)
		if err != nil {
			t.Fatal(err)
		}
		const remaining = "alpha\nbeta\n"
		if afterDeletion.Edited != remaining || afterDeletion.Original != remaining {
			t.Fatalf("post-deletion diff = original %q edited %q", afterDeletion.Original, afterDeletion.Edited)
		}
		if afterDeletion.OriginalStartByte != int64(len("zero\nremove\n")) {
			t.Fatalf("post-deletion original start = %d", afterDeletion.OriginalStartByte)
		}

		includingDeletion, err := svc.GetDiffWindow(meta.FileID, 0, 128)
		if err != nil {
			t.Fatal(err)
		}
		if includingDeletion.Edited != "zero\n"+remaining || includingDeletion.Original != source {
			t.Fatalf("deletion hunk = original %q edited %q", includingDeletion.Original, includingDeletion.Edited)
		}
	})

	t.Run("replacement-only window maps complete replaced span", func(t *testing.T) {
		const source = "before\nold!!\nlater line\n"
		path := writeTempFile(t, "diff-replace.txt", []byte(source))
		svc := NewFileService()
		meta, err := svc.OpenFile(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_, _ = svc.DiscardEdits(meta.FileID)
			_ = svc.CloseFile(meta.FileID)
		})
		mustPrepareEditSession(t, svc, meta.FileID)
		start := int64(len("before\n"))
		if _, err := svc.StageEdit(meta.FileID, start, int64(len("old!!\n")), "N\n"); err != nil {
			t.Fatal(err)
		}

		window, err := svc.GetDiffWindow(meta.FileID, start, len("old!!\n"))
		if err != nil {
			t.Fatal(err)
		}
		if window.Edited != "N\n" || window.Original != "old!!\n" {
			t.Fatalf("replacement hunk = original %q edited %q", window.Original, window.Edited)
		}
		if window.OriginalStartByte != start || window.OriginalNextByte != start+int64(len("old!!\n")) {
			t.Fatalf("replacement source range = [%d,%d)", window.OriginalStartByte, window.OriginalNextByte)
		}
	})

	t.Run("oversized mapped source span fails closed", func(t *testing.T) {
		const oldLine = "0123456789abcdef\n"
		path := writeTempFile(t, "diff-map-budget.txt", []byte(oldLine+"later\n"))
		svc := NewFileService()
		meta, err := svc.OpenFile(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_, _ = svc.DiscardEdits(meta.FileID)
			_ = svc.CloseFile(meta.FileID)
		})
		mustPrepareEditSession(t, svc, meta.FileID)
		if _, err := svc.StageEdit(meta.FileID, 0, int64(len(oldLine)), "N\n"); err != nil {
			t.Fatal(err)
		}

		if _, err := svc.GetDiffWindow(meta.FileID, 0, utf8.UTFMax); !errors.Is(err, ErrEditRequestTooLarge) {
			t.Fatalf("GetDiffWindow error = %v, want bounded fail-closed error", err)
		}
	})
}

func assertRefreshSafetyState(t *testing.T, svc *FileService, fileID, sourcePath, recoveryPath string, source, evidence []byte, before StagingState) {
	t.Helper()
	gotSource, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotSource, source) {
		t.Fatal("blocked refresh changed source bytes")
	}
	gotEvidence, err := os.ReadFile(recoveryPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotEvidence, evidence) {
		t.Fatal("blocked refresh changed recovery evidence")
	}
	after, err := svc.GetStagingState(fileID)
	if err != nil {
		t.Fatal(err)
	}
	if after.EditCount != before.EditCount || after.EditedSize != before.EditedSize {
		t.Fatalf("blocked refresh discarded staging: before=%+v after=%+v", before, after)
	}
}

func TestSavePatchIsDisabledAndPreservesSourceAndStaging(t *testing.T) {
	original := []byte("alpha\nbeta\n")
	path := writeTempFile(t, "editable.txt", original)
	svc := NewFileService()
	meta, err := svc.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.CloseFile(meta.FileID)
	if !meta.Editable {
		t.Fatal("UTF-8 LF fixture should be editable")
	}
	mustPrepareEditSession(t, svc, meta.FileID)

	state, err := svc.StageEdit(meta.FileID, 0, 5, "omega")
	if err != nil {
		t.Fatal(err)
	}
	if state.EditCount != 1 || !state.LengthPreserving || state.InPlaceEligible {
		t.Fatalf("staging state = %+v, want one copy-only length-preserving edit", state)
	}
	if _, err := svc.SavePatch(meta.FileID); !errors.Is(err, ErrInPlaceSaveDisabled) {
		t.Fatalf("SavePatch error = %v, want ErrInPlaceSaveDisabled", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, original) {
		t.Fatalf("disabled SavePatch changed source: got %q want %q", got, original)
	}
	after, err := svc.GetStagingState(meta.FileID)
	if err != nil {
		t.Fatal(err)
	}
	if after.EditCount != state.EditCount || after.EditedSize != state.EditedSize {
		t.Fatalf("disabled SavePatch discarded staging: before=%+v after=%+v", state, after)
	}
	if _, err := os.Stat(sidecarPath(path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("disabled SavePatch created recovery sidecar: %v", err)
	}
}

func TestSavePatchFailsClosedBeforeFileLookup(t *testing.T) {
	svc := NewFileService()
	if _, err := svc.SavePatch("unknown-file-id"); !errors.Is(err, ErrInPlaceSaveDisabled) {
		t.Fatalf("SavePatch error = %v, want stable ErrInPlaceSaveDisabled before lookup", err)
	}
	if _, err := svc.SavePatch(""); !errors.Is(err, ErrInPlaceSaveDisabled) {
		t.Fatalf("blank SavePatch error = %v, want stable ErrInPlaceSaveDisabled before lookup", err)
	}
}

func TestEditRPCRequestBudgetsFailBeforeStaging(t *testing.T) {
	path := writeTempFile(t, "bounded.txt", []byte("alpha\nbeta\n"))
	svc := NewFileService()
	meta, err := svc.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.CloseFile(meta.FileID)

	if _, err := svc.GetEditWindow(meta.FileID, 0, editWindowBytes+1); !errors.Is(err, ErrEditRequestTooLarge) {
		t.Fatalf("GetEditWindow error = %v, want ErrEditRequestTooLarge", err)
	}
	if _, err := svc.GetDiffWindow(meta.FileID, 0, editWindowBytes+1); !errors.Is(err, ErrEditRequestTooLarge) {
		t.Fatalf("GetDiffWindow error = %v, want ErrEditRequestTooLarge", err)
	}
	for maxBytes := 1; maxBytes < utf8.UTFMax; maxBytes++ {
		if _, err := svc.GetEditWindow(meta.FileID, 0, maxBytes); !errors.Is(err, ErrEditRequestTooLarge) {
			t.Fatalf("GetEditWindow(%d) error = %v, want ErrEditRequestTooLarge", maxBytes, err)
		}
		if _, err := svc.GetDiffWindow(meta.FileID, 0, maxBytes); !errors.Is(err, ErrEditRequestTooLarge) {
			t.Fatalf("GetDiffWindow(%d) error = %v, want ErrEditRequestTooLarge", maxBytes, err)
		}
	}
	if _, err := svc.StageEdit(meta.FileID, 0, int64(editRPCTextBytes)+1, "x"); !errors.Is(err, ErrEditRequestTooLarge) {
		t.Fatalf("StageEdit oversized range error = %v, want ErrEditRequestTooLarge", err)
	}
	tooLarge := strings.Repeat("x", editRPCTextBytes+1)
	if _, err := svc.StageEdit(meta.FileID, 0, 1, tooLarge); !errors.Is(err, ErrEditRequestTooLarge) {
		t.Fatalf("StageEdit oversized text error = %v, want ErrEditRequestTooLarge", err)
	}
	state, err := svc.GetStagingState(meta.FileID)
	if err != nil {
		t.Fatal(err)
	}
	if state.EditCount != 0 {
		t.Fatalf("rejected oversized RPC created staging: %+v", state)
	}
}

func TestEditWindowsRejectNegativeCoordinatesBeforeFileLookup(t *testing.T) {
	svc := NewFileService()
	for _, request := range []struct {
		name  string
		start int64
		max   int
	}{{name: "start", start: -1, max: 16}, {name: "budget", start: 0, max: -1}} {
		t.Run(request.name, func(t *testing.T) {
			if _, err := svc.GetEditWindow("f1", request.start, request.max); !errors.Is(err, ErrEditRequestTooLarge) {
				t.Fatalf("GetEditWindow error = %v, want ErrEditRequestTooLarge", err)
			}
			if _, err := svc.GetDiffWindow("f1", request.start, request.max); !errors.Is(err, ErrEditRequestTooLarge) {
				t.Fatalf("GetDiffWindow error = %v, want ErrEditRequestTooLarge", err)
			}
		})
	}
}

func TestEditWindowRejectsIsolatedContinuationAtArbitraryStart(t *testing.T) {
	prefix := append(bytes.Repeat([]byte{'a'}, 1<<20), '\n')
	offset := int64(len(prefix))
	data := append(append([]byte(nil), prefix...), 0x80, 'x', '\n')
	path := writeTempFile(t, "isolated-continuation.txt", data)
	svc := NewFileService()
	meta, err := svc.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.CloseFile(meta.FileID)

	if _, err := svc.GetEditWindow(meta.FileID, offset, 1024); !errors.Is(err, ErrFileNotEditable) {
		t.Fatalf("GetEditWindow error = %v, want ErrFileNotEditable", err)
	}
	if _, err := svc.GetDiffWindow(meta.FileID, offset, 1024); !errors.Is(err, ErrFileNotEditable) {
		t.Fatalf("GetDiffWindow error = %v, want ErrFileNotEditable", err)
	}
}

func TestEditWindowArbitraryStartSkipsOnlyValidatedRuneTail(t *testing.T) {
	prefix := bytes.Repeat([]byte{'a'}, 1<<20)
	runeStart := int64(len(prefix))
	data := append(append([]byte(nil), prefix...), []byte("éx\n")...)
	path := writeTempFile(t, "valid-continuation.txt", data)
	svc := NewFileService()
	meta, err := svc.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.CloseFile(meta.FileID)

	window, err := svc.GetEditWindow(meta.FileID, runeStart+1, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if window.StartByte != runeStart+2 || window.Text != "x\n" {
		t.Fatalf("window = start %d text %q, want start %d text %q", window.StartByte, window.Text, runeStart+2, "x\n")
	}
}

func TestEditWindowLongLineMakesForwardProgress(t *testing.T) {
	data := bytes.Repeat([]byte{'a'}, 3*editWindowBytes)
	path := writeTempFile(t, "long-line.txt", data)
	svc := NewFileService()
	meta, err := svc.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.CloseFile(meta.FileID)

	requested := int64(editWindowBytes)
	window, err := svc.GetEditWindow(meta.FileID, requested, editWindowBytes)
	if err != nil {
		t.Fatal(err)
	}
	if window.StartByte != requested {
		t.Fatalf("StartByte = %d, want bounded long-line fallback %d", window.StartByte, requested)
	}
	if window.NextByte <= requested {
		t.Fatalf("NextByte = %d did not advance beyond requested %d", window.NextByte, requested)
	}
	if len(window.Text) > editWindowBytes {
		t.Fatalf("window text = %d bytes, exceeds %d-byte cap", len(window.Text), editWindowBytes)
	}
}

func TestEditWindowsPreserveUTF8AcrossByteBudgetBoundary(t *testing.T) {
	tests := []struct {
		name string
		rune string
	}{
		{name: "two-byte", rune: "é"},
		{name: "three-byte", rune: "€"},
		{name: "four-byte", rune: "😀"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			const initialOffset = 512
			// Keep the first 1 MiB metadata sample valid, then place the rune so
			// a later bounded window ends after only its first byte.
			prefix := bytes.Repeat([]byte{'a'}, initialOffset+editWindowBytes-1)
			data := append(prefix, []byte(tt.rune)...)
			data = append(data, []byte("-tail\n")...)
			path := writeTempFile(t, "utf8-boundary.txt", data)
			svc := NewFileService()
			meta, err := svc.OpenFile(path)
			if err != nil {
				t.Fatal(err)
			}
			defer svc.CloseFile(meta.FileID)

			var reconstructed []byte
			next := int64(initialOffset)
			for windows := 0; windows < 4; windows++ {
				window, err := svc.GetEditWindow(meta.FileID, next, editWindowBytes)
				if err != nil {
					t.Fatal(err)
				}
				if !utf8.ValidString(window.Text) {
					t.Fatal("window contains invalid UTF-8")
				}
				if window.StartByte != next {
					t.Fatalf("StartByte = %d, want contiguous %d", window.StartByte, next)
				}
				if !window.AtEOF && window.NextByte <= next {
					t.Fatalf("NextByte = %d did not advance beyond %d", window.NextByte, next)
				}
				reconstructed = append(reconstructed, []byte(window.Text)...)
				next = window.NextByte
				if window.AtEOF {
					break
				}
			}
			if !bytes.Equal(reconstructed, data[initialOffset:]) {
				t.Fatalf("reconstructed %d bytes, want exact %d-byte source suffix", len(reconstructed), len(data)-initialOffset)
			}

			reconstructed = nil
			next = int64(initialOffset)
			for windows := 0; windows < 4; windows++ {
				diff, err := svc.GetDiffWindow(meta.FileID, next, editWindowBytes)
				if err != nil {
					t.Fatal(err)
				}
				if !utf8.ValidString(diff.Original) || !utf8.ValidString(diff.Edited) || diff.Original != diff.Edited {
					t.Fatal("diff window is not an exact valid UTF-8 view")
				}
				if diff.StartByte != next {
					t.Fatalf("diff StartByte = %d, want contiguous %d", diff.StartByte, next)
				}
				if !diff.AtEOF && diff.NextByte <= next {
					t.Fatalf("diff NextByte = %d did not advance beyond %d", diff.NextByte, next)
				}
				reconstructed = append(reconstructed, []byte(diff.Edited)...)
				next = diff.NextByte
				if diff.AtEOF {
					break
				}
			}
			if !bytes.Equal(reconstructed, data[initialOffset:]) {
				t.Fatalf("diff reconstructed %d bytes, want exact %d-byte source suffix", len(reconstructed), len(data)-initialOffset)
			}
		})
	}
}

func mustEncodeForEditSafetyTest(t *testing.T, encoding, value string) []byte {
	t.Helper()
	b, err := encodingx.EncodeString(encoding, value)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
