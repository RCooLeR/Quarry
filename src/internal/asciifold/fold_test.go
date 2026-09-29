package asciifold

import (
	"bytes"
	"testing"
)

func TestLowerFoldsOnlyASCIIUppercase(t *testing.T) {
	tests := []struct {
		name string
		in   byte
		want byte
	}{
		{name: "uppercase", in: 'Q', want: 'q'},
		{name: "lowercase unchanged", in: 'q', want: 'q'},
		{name: "digit unchanged", in: '7', want: '7'},
		{name: "non ascii unchanged", in: 0xc4, want: 0xc4},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Lower(tt.in); got != tt.want {
				t.Fatalf("Lower(%#x) = %#x, want %#x", tt.in, got, tt.want)
			}
		})
	}
}

func TestFoldPreservesByteLengthAndNonASCIIBytes(t *testing.T) {
	in := []byte("Straße KELVIN")
	got := Fold(in)
	if len(got) != len(in) {
		t.Fatalf("Fold changed byte length from %d to %d", len(in), len(got))
	}
	if string(got) != "straße Kelvin" {
		t.Fatalf("Fold = %q", got)
	}
}

func TestIndexFoldedUsesByteStableASCIIFold(t *testing.T) {
	haystack := []byte("abc KELVIN Kelvin")
	foldedNeedle := Fold([]byte("kelvin"))
	if got := IndexFolded(haystack, foldedNeedle); got != 4 {
		t.Fatalf("IndexFolded ASCII match = %d, want 4", got)
	}

	kelvinSignOffset := len("abc KELVIN ")
	if got := IndexFolded(haystack[kelvinSignOffset:], foldedNeedle); got != -1 {
		t.Fatalf("IndexFolded matched Unicode kelvin sign at %d", got)
	}
}

func TestIndexFoldedEmptyAndOversizedNeedles(t *testing.T) {
	if got := IndexFolded([]byte("abc"), nil); got != 0 {
		t.Fatalf("IndexFolded empty needle = %d, want 0", got)
	}
	if got := IndexFolded([]byte("abc"), []byte("abcd")); got != -1 {
		t.Fatalf("IndexFolded oversized needle = %d, want -1", got)
	}
}

func TestLastIndexFoldedUsesByteStableASCIIFold(t *testing.T) {
	haystack := []byte("ABC abc AbC \xe2\x84\xaa")
	foldedNeedle := Fold([]byte("abc"))
	if got := LastIndexFolded(haystack, foldedNeedle); got != 8 {
		t.Fatalf("LastIndexFolded ASCII match = %d, want 8", got)
	}
	if got := LastIndexFolded(haystack[12:], Fold([]byte("k"))); got != -1 {
		t.Fatalf("LastIndexFolded matched Unicode kelvin sign at %d", got)
	}
}

func TestLastIndexFoldedEmptyAndOversizedNeedles(t *testing.T) {
	if got := LastIndexFolded([]byte("abc"), nil); got != 3 {
		t.Fatalf("LastIndexFolded empty needle = %d, want 3", got)
	}
	if got := LastIndexFolded([]byte("abc"), []byte("abcd")); got != -1 {
		t.Fatalf("LastIndexFolded oversized needle = %d, want -1", got)
	}
}

func FuzzFoldedIndexes(f *testing.F) {
	f.Add([]byte("ABC abc AbC \xe2\x84\xaa"), []byte("abc"))
	f.Add(bytes.Repeat([]byte("A"), 4096), append(bytes.Repeat([]byte("a"), 255), 'b'))
	f.Add([]byte("abABabaBAba"), []byte("aba"))
	f.Add([]byte{}, []byte{})
	f.Fuzz(func(t *testing.T, haystack, needle []byte) {
		foldedHaystack := Fold(haystack)
		foldedNeedle := Fold(needle)
		if got, want := IndexFolded(haystack, foldedNeedle), bytes.Index(foldedHaystack, foldedNeedle); got != want {
			t.Fatalf("IndexFolded=%d, want %d", got, want)
		}
		if got, want := LastIndexFolded(haystack, foldedNeedle), bytes.LastIndex(foldedHaystack, foldedNeedle); got != want {
			t.Fatalf("LastIndexFolded=%d, want %d", got, want)
		}
	})
}

func TestFoldedIndexesRepeatedPrefixes(t *testing.T) {
	for _, size := range []int{8, 64, 256, 1024} {
		needle := append(bytes.Repeat([]byte("a"), size), 'b')
		for _, haystack := range [][]byte{
			bytes.Repeat([]byte("A"), 4096),
			append(bytes.Repeat([]byte("A"), 4096), 'B'),
			append(append(bytes.Repeat([]byte("A"), size), 'B'), bytes.Repeat([]byte("A"), 4096)...),
			bytes.Repeat(append(bytes.Repeat([]byte("A"), size+1), 'B'), 8),
		} {
			folded := Fold(haystack)
			if got, want := IndexFolded(haystack, needle), bytes.Index(folded, needle); got != want {
				t.Fatalf("size=%d forward=%d, want %d", size, got, want)
			}
			if got, want := LastIndexFolded(haystack, needle), bytes.LastIndex(folded, needle); got != want {
				t.Fatalf("size=%d backward=%d, want %d", size, got, want)
			}
		}
	}
}

func BenchmarkFoldedIndexesRepetitive(b *testing.B) {
	haystack := bytes.Repeat([]byte("A"), 1024*1024)
	needle := append(bytes.Repeat([]byte("a"), 255), 'b')
	for name, find := range map[string]func([]byte, []byte) int{
		"forward":  IndexFolded,
		"backward": LastIndexFolded,
	} {
		b.Run(name, func(b *testing.B) {
			b.SetBytes(int64(len(haystack)))
			b.ReportAllocs()
			for b.Loop() {
				if got := find(haystack, needle); got != -1 {
					b.Fatalf("index=%d, want -1", got)
				}
			}
		})
	}
}
