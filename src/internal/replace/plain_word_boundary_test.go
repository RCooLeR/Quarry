package replace

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestBatchPlainWholeWordUnicodeBoundaryChunkInvariant(t *testing.T) {
	const supplementaryLetter = "𐐀"
	rules := []BatchRule{{Name: "word", Find: []byte("cat"), Replace: []byte("dog")}}
	for chunkSize := len("cat") + utf8.UTFMax; chunkSize <= 13; chunkSize++ {
		for padding := 0; padding < chunkSize; padding++ {
			t.Run(fmt.Sprintf("chunk=%d/padding=%d", chunkSize, padding), func(t *testing.T) {
				prefix := strings.Repeat(".", padding)
				source := prefix + supplementaryLetter + "cat cat cat" + supplementaryLetter
				want := prefix + supplementaryLetter + "cat dog cat" + supplementaryLetter

				src, err := os.CreateTemp("", "q-batch-word-src-*")
				if err != nil {
					t.Fatal(err)
				}
				defer os.Remove(src.Name())
				defer src.Close()
				dst, err := os.CreateTemp("", "q-batch-word-dst-*")
				if err != nil {
					t.Fatal(err)
				}
				defer os.Remove(dst.Name())
				defer dst.Close()
				if _, err := src.WriteString(source); err != nil {
					t.Fatal(err)
				}
				if _, err := src.Seek(0, 0); err != nil {
					t.Fatal(err)
				}
				matches, conflicts, err := replaceBatchPlain(context.Background(), src, dst, rules, BatchOptions{
					ChunkSize: chunkSize,
					WholeWord: true,
				})
				if err != nil {
					t.Fatal(err)
				}
				if matches != 1 || conflicts != 0 {
					t.Fatalf("matches/conflicts=%d/%d, want 1/0", matches, conflicts)
				}
				got, err := os.ReadFile(dst.Name())
				if err != nil {
					t.Fatal(err)
				}
				if string(got) != want {
					t.Fatalf("output=%q, want %q", got, want)
				}

				previews, previewConflicts, err := PreviewBatchPlain(context.Background(), memReaderAt{data: []byte(source)}, rules, PreviewOptions{
					ChunkSize: chunkSize,
					MaxHits:   4,
					WholeWord: true,
				})
				if err != nil {
					t.Fatal(err)
				}
				if len(previews) != 1 || previewConflicts != 0 {
					t.Fatalf("previews/conflicts=%#v/%d, want one/0", previews, previewConflicts)
				}
			})
		}
	}
}

func TestBatchPlainWholeWordCombiningMarkIsNotBoundary(t *testing.T) {
	rules := []BatchRule{{Name: "letter", Find: []byte("e"), Replace: []byte("X")}}
	previews, conflicts, err := PreviewBatchPlain(context.Background(), memReaderAt{data: []byte("e\u0301 e")}, rules, PreviewOptions{
		ChunkSize: 5,
		MaxHits:   4,
		WholeWord: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(previews) != 1 || previews[0].Offset != int64(len("e\u0301 ")) || conflicts != 0 {
		t.Fatalf("previews/conflicts=%#v/%d", previews, conflicts)
	}
}
