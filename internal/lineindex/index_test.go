package lineindex

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

func TestBuildRecordsSparseEntries(t *testing.T) {
	idx := New(2)
	if err := idx.Build(context.Background(), strings.NewReader("a\nb\nc\nd\n")); err != nil {
		t.Fatal(err)
	}

	entries := idx.Entries()
	if len(entries) != 3 {
		t.Fatalf("entries = %#v, want 3 entries", entries)
	}
	if entries[0] != (Entry{Line: 1, Offset: 0}) {
		t.Fatalf("first entry = %#v", entries[0])
	}
	if entries[1] != (Entry{Line: 3, Offset: 4}) {
		t.Fatalf("second entry = %#v", entries[1])
	}
	if entries[2] != (Entry{Line: 5, Offset: 8}) {
		t.Fatalf("third entry = %#v", entries[2])
	}
	if !idx.Done() {
		t.Fatal("index should be done")
	}
}

func TestEveryLinesForSizeKeepsSmallFilesAtDefault(t *testing.T) {
	if got := EveryLinesForSize(512 << 20); got != defaultEveryLines {
		t.Fatalf("EveryLinesForSize(512MiB) = %d, want default %d", got, defaultEveryLines)
	}
}

func TestEveryLinesForSizeUsesOneByteLineWorstCase(t *testing.T) {
	got := EveryLinesForSize(1 << 30)
	if got != 8192 {
		t.Fatalf("EveryLinesForSize(1GiB) = %d, want 8192", got)
	}
	anchors := (int64(1)<<30)/got + 1
	if anchors > MaxIndexEntries {
		t.Fatalf("worst-case anchors = %d, limit %d", anchors, MaxIndexEntries)
	}
}

func TestEveryLinesForSizeScalesHugeShortLineFiles(t *testing.T) {
	got := EveryLinesForSize(500 << 30)
	if got < 65536 {
		t.Fatalf("EveryLinesForSize(500GiB) = %d, want at least 65536", got)
	}
	if got&(got-1) != 0 {
		t.Fatalf("EveryLinesForSize(500GiB) = %d, want power-of-two stride", got)
	}
}

func TestApproxLineToOffset(t *testing.T) {
	idx := New(2)
	if err := idx.Build(context.Background(), strings.NewReader("a\nb\nc\nd\n")); err != nil {
		t.Fatal(err)
	}

	entry, ok := idx.ApproxLineToOffset(4)
	if !ok {
		t.Fatal("expected entry")
	}
	if entry != (Entry{Line: 3, Offset: 4}) {
		t.Fatalf("entry = %#v, want line 3 offset 4", entry)
	}
}

func TestApproxOffsetToLine(t *testing.T) {
	idx := New(2)
	if err := idx.Build(context.Background(), strings.NewReader("aa\nbb\ncc\ndd\n")); err != nil {
		t.Fatal(err)
	}

	entry, ok := idx.ApproxOffsetToLine(6)
	if !ok {
		t.Fatal("expected entry")
	}
	if entry.Line < 2 || entry.Line > 3 {
		t.Fatalf("line estimate = %d, want around 2-3", entry.Line)
	}
	if entry.Offset != 6 {
		t.Fatalf("offset = %d, want 6", entry.Offset)
	}
}

func TestApproxUsesPriorityEntries(t *testing.T) {
	idx := New(4)
	idx.AddPriorityEntry(Entry{Line: 1, Offset: 0})
	idx.AddPriorityEntry(Entry{Line: 9, Offset: 24})

	entry, ok := idx.ApproxLineToOffset(10)
	if !ok {
		t.Fatal("expected line estimate")
	}
	if entry != (Entry{Line: 9, Offset: 24}) {
		t.Fatalf("entry = %#v, want priority entry", entry)
	}

	offsetEntry, ok := idx.ApproxOffsetToLine(24)
	if !ok {
		t.Fatal("expected offset estimate")
	}
	if offsetEntry.Line != 9 {
		t.Fatalf("line = %d, want 9", offsetEntry.Line)
	}
}

func TestBuildClearsPriorityEntries(t *testing.T) {
	idx := New(2)
	idx.AddPriorityEntry(Entry{Line: 99, Offset: 999})
	if err := idx.Build(context.Background(), strings.NewReader("a\nb\nc\nd\n")); err != nil {
		t.Fatal(err)
	}

	entry, ok := idx.ApproxLineToOffset(100)
	if !ok {
		t.Fatal("expected entry")
	}
	if entry == (Entry{Line: 99, Offset: 999}) {
		t.Fatal("priority entry should not survive completed exact index")
	}
}

func TestBuildWithEncodingUTF16LE(t *testing.T) {
	idx := New(2)
	data := []byte{
		0x61, 0x00, 0x0D, 0x00, 0x0A, 0x00,
		0x62, 0x00, 0x0D, 0x00, 0x0A, 0x00,
		0x63, 0x00, 0x0D, 0x00, 0x0A, 0x00,
		0x64, 0x00, 0x0D, 0x00, 0x0A, 0x00,
	}
	if err := idx.BuildWithEncoding(context.Background(), bytes.NewReader(data), "UTF-16LE"); err != nil {
		t.Fatal(err)
	}

	entries := idx.Entries()
	if len(entries) != 3 {
		t.Fatalf("entries = %#v, want 3 entries", entries)
	}
	if entries[1] != (Entry{Line: 3, Offset: 12}) {
		t.Fatalf("second entry = %#v, want line 3 offset 12", entries[1])
	}
	if entries[2] != (Entry{Line: 5, Offset: 24}) {
		t.Fatalf("third entry = %#v, want line 5 offset 24", entries[2])
	}
}

func TestBuildCRLFAtProductionChunkBoundaryIsOneLineBreak(t *testing.T) {
	const chunkSize = 4 * 1024 * 1024
	data := make([]byte, chunkSize+2)
	for i := range data {
		data[i] = 'a'
	}
	data[chunkSize-1] = '\r'
	data[chunkSize] = '\n'

	idx := New(1)
	if err := idx.Build(context.Background(), bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	want := []Entry{{Line: 1, Offset: 0}, {Line: 2, Offset: chunkSize + 1}}
	if got := idx.Entries(); len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("entries = %#v, want %#v", got, want)
	}
}

func TestBuildForcedSmallChunksMatchesLogicalNewlineOracle(t *testing.T) {
	data := []byte("a\r\nb\rc\nd\r")
	for chunk := 1; chunk <= len(data); chunk++ {
		idx := New(1)
		if err := idx.buildWithEncodingChunkSize(context.Background(), bytes.NewReader(data), "UTF-8", chunk); err != nil {
			t.Fatal(err)
		}
		want := []Entry{{Line: 1, Offset: 0}, {Line: 2, Offset: 3}, {Line: 3, Offset: 5}, {Line: 4, Offset: 7}, {Line: 5, Offset: 9}}
		got := idx.Entries()
		if len(got) != len(want) {
			t.Fatalf("chunk %d entries = %#v, want %#v", chunk, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("chunk %d entry %d = %#v, want %#v", chunk, i, got[i], want[i])
			}
		}
	}
}

func TestBuildUTF16IgnoresUnalignedNewlinePatterns(t *testing.T) {
	for _, tt := range []struct {
		name string
		data []byte
	}{
		{name: "UTF-16LE", data: []byte{0x00, 0x0A, 0x00, 0x0D, 0x00, 0x41}},
		{name: "UTF-16BE", data: []byte{0x0A, 0x00, 0x0D, 0x00, 0x41, 0x00}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for chunk := 1; chunk <= len(tt.data); chunk++ {
				idx := New(1)
				if err := idx.buildWithEncodingChunkSize(context.Background(), bytes.NewReader(tt.data), tt.name, chunk); err != nil {
					t.Fatal(err)
				}
				if got := idx.Entries(); len(got) != 1 || got[0] != (Entry{Line: 1, Offset: 0}) {
					t.Fatalf("chunk %d entries = %#v, want only line 1", chunk, got)
				}
			}
		})
	}
}

func TestValidateSnapshotRejectsMalformedAnchors(t *testing.T) {
	valid := Snapshot{
		EveryLines: 2,
		Entries:    []Entry{{Line: 1, Offset: 0}, {Line: 3, Offset: 4}},
		Lines:      3,
		Bytes:      8,
		Done:       true,
	}
	if err := ValidateSnapshot(valid, 8); err != nil {
		t.Fatalf("valid snapshot: %v", err)
	}
	tests := map[string]Snapshot{
		"zero stride":      {EveryLines: 0, Entries: valid.Entries, Lines: 3, Bytes: 8, Done: true},
		"wrong totals":     {EveryLines: 2, Entries: valid.Entries, Lines: 3, Bytes: 9, Done: true},
		"missing first":    {EveryLines: 2, Entries: []Entry{{Line: 3, Offset: 4}}, Lines: 3, Bytes: 8, Done: true},
		"negative line":    {EveryLines: 2, Entries: []Entry{{Line: 1, Offset: 0}, {Line: -1, Offset: 4}}, Lines: 3, Bytes: 8, Done: true},
		"negative offset":  {EveryLines: 2, Entries: []Entry{{Line: 1, Offset: 0}, {Line: 3, Offset: -1}}, Lines: 3, Bytes: 8, Done: true},
		"offset past EOF":  {EveryLines: 2, Entries: []Entry{{Line: 1, Offset: 0}, {Line: 3, Offset: 9}}, Lines: 3, Bytes: 8, Done: true},
		"line past total":  {EveryLines: 2, Entries: []Entry{{Line: 1, Offset: 0}, {Line: 5, Offset: 4}}, Lines: 3, Bytes: 8, Done: true},
		"stride violation": {EveryLines: 2, Entries: []Entry{{Line: 1, Offset: 0}, {Line: 2, Offset: 4}}, Lines: 3, Bytes: 8, Done: true},
		"duplicate line":   {EveryLines: 2, Entries: []Entry{{Line: 1, Offset: 0}, {Line: 3, Offset: 4}, {Line: 3, Offset: 6}}, Lines: 4, Bytes: 8, Done: true},
		"reverse offset":   {EveryLines: 2, Entries: []Entry{{Line: 1, Offset: 0}, {Line: 3, Offset: 6}, {Line: 5, Offset: 4}}, Lines: 5, Bytes: 8, Done: true},
	}
	for name, snapshot := range tests {
		t.Run(name, func(t *testing.T) {
			sourceBytes := int64(8)
			if name == "wrong totals" {
				sourceBytes = 8
			}
			if err := ValidateSnapshot(snapshot, sourceBytes); err == nil {
				t.Fatalf("snapshot unexpectedly accepted: %#v", snapshot)
			}
		})
	}
}

func TestBuildStopsAtHardAnchorLimit(t *testing.T) {
	idx := New(1)
	data := bytes.Repeat([]byte{'\n'}, MaxIndexEntries+16)
	err := idx.buildWithEncodingChunkSize(context.Background(), bytes.NewReader(data), "UTF-8", 17)
	if !errors.Is(err, ErrIndexEntryLimit) {
		t.Fatalf("error = %v, want ErrIndexEntryLimit", err)
	}
	if got := len(idx.Entries()); got != MaxIndexEntries {
		t.Fatalf("entries = %d, want hard limit %d", got, MaxIndexEntries)
	}
	if idx.Done() {
		t.Fatal("entry-limited index must not be marked complete")
	}
}

func TestRestoreSnapshotKeepsIndexPointerAndCopiesEntries(t *testing.T) {
	idx := New(2)
	snapshot := Snapshot{EveryLines: 2, Entries: []Entry{{Line: 1, Offset: 0}, {Line: 3, Offset: 4}}, Lines: 3, Bytes: 8, Done: true}
	if err := idx.RestoreSnapshot(snapshot); err != nil {
		t.Fatal(err)
	}
	snapshot.Entries[1] = Entry{Line: 99, Offset: 99}
	if got := idx.Entries()[1]; got != (Entry{Line: 3, Offset: 4}) {
		t.Fatalf("restored entry mutated through caller slice: %#v", got)
	}
}
