package replace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/quarry/quarry-wails3/internal/regexutil"
)

type shortRegexPreviewReader struct{ size int64 }

func (r shortRegexPreviewReader) Size() int64 { return r.size }
func (shortRegexPreviewReader) ReadAt([]byte, int64) (int, error) {
	return 0, io.EOF
}

type cancelingRegexWriter struct {
	cancel func()
	writes int
}

func (w *cancelingRegexWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.writes == 1 {
		w.cancel()
	}
	return len(p), nil
}

func (*cancelingRegexWriter) Sync() error { return nil }

func TestReplaceRegexpExactMaximumWidthAcrossEverySeam(t *testing.T) {
	const (
		chunkSize = 11
		match     = "a12345b"
	)
	pattern := []byte(`a[0-9]{5}b`)
	for bytesBeforeSeam := 1; bytesBeforeSeam < len(match); bytesBeforeSeam++ {
		t.Run(fmt.Sprintf("split-%d", bytesBeforeSeam), func(t *testing.T) {
			prefix := strings.Repeat(".", chunkSize-bytesBeforeSeam)
			source := []byte(prefix + match + " tail")
			got, matches, err := runReplaceRegexpForFuzz(t, source, pattern, []byte("X"), RegexOptions{
				ChunkSize:      chunkSize,
				MaxMatchWindow: len(match),
			})
			if err != nil {
				t.Fatal(err)
			}
			if matches != 1 {
				t.Fatalf("matches = %d, want 1", matches)
			}
			if want := prefix + "X tail"; string(got) != want {
				t.Fatalf("output = %q, want %q", got, want)
			}
		})
	}
}

func TestReplaceRegexpMaximumWidthRejectedAtWindowMinusOneAndAcceptedExactly(t *testing.T) {
	source := []byte("before a12345b after")
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.txt")
	if err := os.WriteFile(sourcePath, source, 0o600); err != nil {
		t.Fatal(err)
	}

	openSource := func(t *testing.T) *os.File {
		t.Helper()
		file, err := os.Open(sourcePath)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = file.Close() })
		return file
	}

	rejectedOutput := &fuzzSyncBuffer{}
	matches, err := replaceRegexp(context.Background(), openSource(t), rejectedOutput, []byte(`a[0-9]{5}b`), []byte("X"), RegexOptions{
		ChunkSize:      8,
		MaxMatchWindow: 6,
	})
	if !errors.Is(err, regexutil.ErrRegexExceedsWindow) {
		t.Fatalf("error = %v, want ErrRegexExceedsWindow", err)
	}
	if matches != 0 || rejectedOutput.Len() != 0 {
		t.Fatalf("rejected replace wrote output: matches=%d bytes=%d", matches, rejectedOutput.Len())
	}

	acceptedOutput := &fuzzSyncBuffer{}
	matches, err = replaceRegexp(context.Background(), openSource(t), acceptedOutput, []byte(`a[0-9]{5}b`), []byte("X"), RegexOptions{
		ChunkSize:      8,
		MaxMatchWindow: 7,
	})
	if err != nil {
		t.Fatal(err)
	}
	if matches != 1 || acceptedOutput.String() != "before X after" {
		t.Fatalf("accepted replace: matches=%d output=%q", matches, acceptedOutput.String())
	}
}

func TestRegexBatchArbitrationIsExactAcrossCarryBoundaries(t *testing.T) {
	rules := []BatchRule{
		{Name: "earlier", Find: []byte(`xyZ`), Replace: []byte("B"), Priority: 1},
		{Name: "crossing", Find: []byte(`Zabcdef`), Replace: []byte("A"), Priority: 0},
	}
	for chunkSize := 7; chunkSize <= 16; chunkSize++ {
		for padding := 0; padding < chunkSize; padding++ {
			name := fmt.Sprintf("chunk-%d/padding-%d", chunkSize, padding)
			t.Run(name, func(t *testing.T) {
				prefix := strings.Repeat(".", padding)
				source := []byte(prefix + "xyZabcdef" + strings.Repeat(".", 3*chunkSize))
				got, matches, conflicts, err := runBatchRegexpForFuzz(t, source, rules, RegexOptions{
					ChunkSize:      chunkSize,
					MaxMatchWindow: 7,
				})
				if err != nil {
					t.Fatal(err)
				}
				want := prefix + "Babcdef" + strings.Repeat(".", 3*chunkSize)
				if string(got) != want || matches != 1 || conflicts != 1 {
					t.Fatalf("output=%q matches=%d conflicts=%d; want output=%q matches=1 conflicts=1", got, matches, conflicts, want)
				}
			})
		}
	}
}

func TestRegexBatchDifferentialAgainstWholeFileArbitration(t *testing.T) {
	rules := []BatchRule{
		{Name: "single-a", Find: []byte(`a`), Replace: []byte("A"), Priority: 2},
		{Name: "ab", Find: []byte(`ab`), Replace: []byte("X"), Priority: 1},
		{Name: "b-pair", Find: []byte(`b[ac]`), Replace: []byte("Y"), Priority: 3},
		{Name: "bounded-gap", Find: []byte(`a.{0,2}c`), Replace: []byte("Z"), Priority: 4},
		{Name: "capture", Find: []byte(`(c)(a)`), Replace: []byte(`${2}${1}`), Priority: 5},
	}
	random := rand.New(rand.NewSource(20260712))
	alphabet := []byte("abc ")

	for sample := 0; sample < 12; sample++ {
		source := make([]byte, 160+sample)
		for i := range source {
			source[i] = alphabet[random.Intn(len(alphabet))]
		}
		want, wantMatches, wantConflicts, wantSelected := wholeFileRegexBatchReference(t, source, rules)
		for chunkSize := 10; chunkSize <= 19; chunkSize++ {
			got, matches, conflicts, err := runBatchRegexpForFuzz(t, source, rules, RegexOptions{
				ChunkSize:      chunkSize,
				MaxMatchWindow: 10,
			})
			if err != nil {
				t.Fatalf("sample=%d chunk=%d: %v", sample, chunkSize, err)
			}
			if !bytes.Equal(got, want) || matches != wantMatches || conflicts != wantConflicts {
				previews, previewConflicts, previewErr := PreviewBatchRegexp(context.Background(), memReaderAt{data: source}, rules, RegexPreviewOptions{ChunkSize: chunkSize, MaxHits: len(source)}, RegexOptions{ChunkSize: chunkSize, MaxMatchWindow: 10})
				if previewErr == nil {
					for i := range previews {
						if i >= len(wantSelected) {
							t.Logf("unexpected preview i=%d", i)
							break
						}
						if previews[i].Offset != int64(wantSelected[i].start) || previews[i].Conflicts != wantSelected[i].conflicts {
							t.Logf("first preview difference i=%d got=(%d,%d) want=(%d,%d); preview total=%d", i, previews[i].Offset, previews[i].Conflicts, wantSelected[i].start, wantSelected[i].conflicts, previewConflicts)
							t.Logf("source=%q", source)
							for j := i - 2; j <= i+2; j++ {
								if j >= 0 && j < len(previews) && j < len(wantSelected) {
									t.Logf("nearby i=%d got=(%d,%d,%s) want=(%d,%d,%s)", j, previews[j].Offset, previews[j].Conflicts, previews[j].RuleName, wantSelected[j].start, wantSelected[j].conflicts, wantSelected[j].rule.Name)
								}
							}
							break
						}
					}
				}
				t.Fatalf("sample=%d chunk=%d output=%q matches=%d conflicts=%d; want output=%q matches=%d conflicts=%d", sample, chunkSize, got, matches, conflicts, want, wantMatches, wantConflicts)
			}
		}
	}
}

type wholeFileRegexCandidate struct {
	start     int
	end       int
	rule      BatchRule
	ruleIndex int
	re        *regexp.Regexp
	loc       []int
	conflicts int
}

func wholeFileRegexBatchReference(t *testing.T, source []byte, rules []BatchRule) ([]byte, int64, int64, []wholeFileRegexCandidate) {
	t.Helper()
	var candidates []wholeFileRegexCandidate
	for i, rule := range rules {
		re, err := regexp.Compile(string(rule.Find))
		if err != nil {
			t.Fatal(err)
		}
		for _, loc := range re.FindAllSubmatchIndex(source, -1) {
			candidates = append(candidates, wholeFileRegexCandidate{
				start: loc[0], end: loc[1], rule: rule, ruleIndex: i, re: re, loc: loc,
			})
		}
	}
	sort.SliceStable(candidates, func(i int, j int) bool {
		a, b := candidates[i], candidates[j]
		if a.start != b.start {
			return a.start < b.start
		}
		if a.rule.Priority != b.rule.Priority {
			return a.rule.Priority < b.rule.Priority
		}
		if a.end-a.start != b.end-b.start {
			return a.end-a.start > b.end-b.start
		}
		return a.ruleIndex < b.ruleIndex
	})

	selected := make([]wholeFileRegexCandidate, 0, len(candidates))
	cursor := 0
	for _, candidate := range candidates {
		if candidate.start < cursor {
			continue
		}
		selected = append(selected, candidate)
		cursor = candidate.end
	}

	var output bytes.Buffer
	cursor = 0
	var conflicts int64
	for _, chosen := range selected {
		output.Write(source[cursor:chosen.start])
		output.Write(chosen.re.Expand(nil, chosen.rule.Replace, source, chosen.loc))
		cursor = chosen.end
		for _, candidate := range candidates {
			same := candidate.start == chosen.start && candidate.end == chosen.end && candidate.ruleIndex == chosen.ruleIndex
			if !same && candidate.start < chosen.end && candidate.end > chosen.start {
				conflicts++
				chosen.conflicts++
			}
		}
		for i := range selected {
			if selected[i].start == chosen.start && selected[i].end == chosen.end && selected[i].ruleIndex == chosen.ruleIndex {
				selected[i].conflicts = chosen.conflicts
				break
			}
		}
	}
	output.Write(source[cursor:])
	return output.Bytes(), int64(len(selected)), conflicts, selected
}

func TestAtomicRegexBatchPreflightLeavesFilesystemUntouched(t *testing.T) {
	tests := []struct {
		name string
		rule BatchRule
		opts RegexOptions
		want error
	}{
		{
			name: "unbounded pattern",
			rule: BatchRule{Name: "unbounded", Find: []byte(`begin(?:.|\n)*end`), Replace: []byte("X")},
			opts: RegexOptions{ChunkSize: 32, MaxMatchWindow: 16},
			want: regexutil.ErrUnboundedRegex,
		},
		{
			name: "oversized chunk",
			rule: BatchRule{Name: "literal", Find: []byte(`x`), Replace: []byte("X")},
			opts: RegexOptions{ChunkSize: maxRegexChunkSize + 1, MaxMatchWindow: 16},
			want: regexutil.ErrRegexResourceLimit,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			missingSource := filepath.Join(dir, "missing-source.txt")
			outputDir := filepath.Join(dir, "must-not-exist")
			outputPath := filepath.Join(outputDir, "output.txt")
			_, err := replaceBatchRegexpFileAtomic(context.Background(), missingSource, outputPath, []BatchRule{tt.rule}, FileOptions{}, tt.opts)
			if !errors.Is(err, tt.want) {
				t.Fatalf("error = %v, want %v", err, tt.want)
			}
			if _, statErr := os.Lstat(outputDir); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("regex preflight touched output filesystem: %v", statErr)
			}
		})
	}
}

func TestRegexPreviewsRejectUnexpectedShortSource(t *testing.T) {
	r := shortRegexPreviewReader{size: 1024}
	if _, err := PreviewRegexp(context.Background(), r, []byte(`x`), []byte("y"), RegexPreviewOptions{MaxHits: 1}, RegexOptions{}); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("single preview error = %v, want io.ErrUnexpectedEOF", err)
	}
	if _, _, err := PreviewBatchRegexp(context.Background(), r, []BatchRule{{Find: []byte(`x`), Replace: []byte("y")}}, RegexPreviewOptions{MaxHits: 1}, RegexOptions{}); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("batch preview error = %v, want io.ErrUnexpectedEOF", err)
	}
}

func TestDenseRegexReplacementObservesCancellationInsideOneChunk(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.txt")
	if err := os.WriteFile(sourcePath, bytes.Repeat([]byte("a"), 1<<20), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, batch := range []bool{false, true} {
		t.Run(fmt.Sprintf("batch-%v", batch), func(t *testing.T) {
			source, err := os.Open(sourcePath)
			if err != nil {
				t.Fatal(err)
			}
			defer source.Close()
			ctx, cancel := context.WithCancel(context.Background())
			writer := &cancelingRegexWriter{cancel: cancel}
			if batch {
				_, _, err = replaceBatchRegexp(ctx, source, writer, []BatchRule{{Find: []byte(`a`), Replace: []byte("x")}}, RegexOptions{ChunkSize: 1 << 20, MaxMatchWindow: 1})
			} else {
				_, err = replaceRegexp(ctx, source, writer, []byte(`a`), []byte("x"), RegexOptions{ChunkSize: 1 << 20, MaxMatchWindow: 1})
			}
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("error = %v, want context.Canceled", err)
			}
			if writer.writes >= 2048 {
				t.Fatalf("cancellation was delayed for %d dense writes", writer.writes)
			}
		})
	}
}
