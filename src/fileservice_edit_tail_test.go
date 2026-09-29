package main

import (
	"errors"
	"os"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestEditTailWindowAlwaysEndsAtExactUTF8EOF(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		budget     int
	}{
		{"empty", "", 4},
		{"one-newline", "\n", 4},
		{"complete-lines", "first\nlast\n", 6},
		{"partial-first-line", "prefix\nsecond\nlast\n", 8},
		{"long-no-newline", strings.Repeat("x", 2*editWindowBytes) + "end", editWindowBytes},
		{"long-with-newline", strings.Repeat("x", 2*editWindowBytes) + "end\n", editWindowBytes},
		{"long-final-scalar", strings.Repeat("x", 2*editWindowBytes) + "😀", editWindowBytes},
		{"long-final-scalar-newline", strings.Repeat("x", 2*editWindowBytes) + "😀\n", editWindowBytes},
		{"scalar-start-seam-1", "prefix\n😀😀😀\n", 4},
		{"scalar-start-seam-2", "prefix\n😀😀😀\n", 6},
		{"scalar-start-seam-3", "prefix\n😀😀😀\n", 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTempFile(t, "tail.txt", []byte(tc.text))
			svc := NewFileService()
			meta, err := svc.OpenFile(path)
			if err != nil {
				t.Fatal(err)
			}
			defer svc.CloseFile(meta.FileID)
			window, err := svc.GetEditTailWindow(meta.FileID, tc.budget)
			if err != nil {
				t.Fatal(err)
			}
			assertExactEditTail(t, window, tc.text, tc.budget)
			if tc.name == "complete-lines" && (window.StartByte != 6 || window.Text != "last\n") {
				t.Fatalf("line-aligned tail = %+v", window)
			}
			if tc.name == "partial-first-line" && window.Text != "last\n" {
				t.Fatalf("partial-line tail = %+v", window)
			}
		})
	}
}

func TestEditTailWindowUsesStagedInsertionAndDeletion(t *testing.T) {
	const source = "first\noriginal final line\n"
	for _, edited := range []string{
		source + "new final 😀\n",
		"first\n",
		"",
		strings.Repeat("😀", editWindowBytes/2) + "\n",
	} {
		path := writeTempFile(t, "staged-tail.txt", []byte(source))
		svc := NewFileService()
		meta, err := svc.OpenFile(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { svc.shutdown() })
		mustPrepareEditSession(t, svc, meta.FileID)
		if _, err := svc.StageEdit(meta.FileID, 0, int64(len(source)), edited); err != nil {
			t.Fatal(err)
		}
		window, err := svc.GetEditTailWindow(meta.FileID, 0)
		if err != nil {
			t.Fatal(err)
		}
		assertExactEditTail(t, window, edited, editWindowBytes)
		original, err := os.ReadFile(path)
		if err != nil || string(original) != source {
			t.Fatalf("tail navigation changed source: %q, %v", original, err)
		}
	}
}

func TestEditTailWindowRejectsInvalidBudgetsBeforeServiceAccess(t *testing.T) {
	var svc *FileService
	for _, budget := range []int{-1, 1, 2, 3, editWindowBytes + 1} {
		if _, err := svc.GetEditTailWindow("f1", budget); !errors.Is(err, ErrEditRequestTooLarge) {
			t.Fatalf("budget %d: error = %v", budget, err)
		}
	}
}

func assertExactEditTail(t *testing.T, window Window, content string, budget int) {
	t.Helper()
	if !window.AtEOF || window.NextByte != int64(len(content)) {
		t.Fatalf("tail ends at %d, EOF=%t, expected %d", window.NextByte, window.AtEOF, len(content))
	}
	if window.StartByte < 0 || window.StartByte > window.NextByte || window.AtBOF != (window.StartByte == 0) {
		t.Fatalf("invalid tail boundaries: %+v", window)
	}
	if window.Text != content[window.StartByte:] || len(window.Text) > budget || !utf8.ValidString(window.Text) {
		t.Fatalf("tail is not exact bounded UTF-8 suffix: start=%d length=%d budget=%d", window.StartByte, len(window.Text), budget)
	}
	if content != "" && window.Text == "" {
		t.Fatal("nonempty document returned an empty tail")
	}
	if len(window.LineOffsets) != strings.Count(window.Text, "\n")+1 || window.LineOffsets[0] != window.StartByte {
		t.Fatalf("incorrect tail line coordinates: %+v", window.LineOffsets)
	}
}
