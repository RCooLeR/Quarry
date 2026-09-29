package main

import (
	"errors"
	"math"
	"os"
	"testing"
)

func TestGetHexWindowRejectsOversizedBudgetWithoutChangingSource(t *testing.T) {
	service := NewFileService()
	path := writeTempFile(t, "hex-budget.bin", []byte{0x00, 0x01, 0x02, 0xff})
	meta, err := service.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer service.CloseFile(meta.FileID)

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, budget := range []int{hexWindowBytes + 1, math.MaxInt} {
		if _, err := service.GetHexWindow(meta.FileID, 0, budget); !errors.Is(err, ErrHexRequestTooLarge) {
			t.Fatalf("budget %d error = %v, want ErrHexRequestTooLarge", budget, err)
		}
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("oversized read changed source: before %x after %x", before, after)
	}
}

func TestGetHexWindowClampsHugeStartAndKeepsResponseBounded(t *testing.T) {
	service := NewFileService()
	path := writeTempFile(t, "hex-end.bin", []byte("0123456789abcdef-tail"))
	meta, err := service.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer service.CloseFile(meta.FileID)

	window, err := service.GetHexWindow(meta.FileID, math.MaxInt64, hexWindowBytes)
	if err != nil {
		t.Fatal(err)
	}
	if !window.AtEof || window.NextByte != meta.Size || len(window.Lines) > hexWindowBytes/hexBytesPerLine+1 {
		t.Fatalf("huge-start window = %+v", window)
	}
	if window.StartByte < 0 || window.StartByte > meta.Size {
		t.Fatalf("start byte %d outside source size %d", window.StartByte, meta.Size)
	}
}

func TestGetHexWindowRejectsNegativeCoordinatesBeforeFileLookup(t *testing.T) {
	service := NewFileService()
	for _, request := range []struct {
		start int64
		max   int
	}{{start: -1, max: 1}, {start: 0, max: -1}} {
		if _, err := service.GetHexWindow("f1", request.start, request.max); !errors.Is(err, ErrHexRequestTooLarge) {
			t.Fatalf("GetHexWindow(%d,%d) error = %v, want ErrHexRequestTooLarge", request.start, request.max, err)
		}
	}
}
