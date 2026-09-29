package search

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/quarry/quarry-wails3/internal/regexutil"
)

func TestFindRegexpExactMaximumWidthAcrossEverySeam(t *testing.T) {
	const (
		chunkSize = 11
		match     = "a12345b"
	)
	re, err := CompileRegexpForTesting([]byte(`a[0-9]{5}b`), false)
	if err != nil {
		t.Fatal(err)
	}

	for bytesBeforeSeam := 1; bytesBeforeSeam < len(match); bytesBeforeSeam++ {
		t.Run(fmt.Sprintf("split-%d", bytesBeforeSeam), func(t *testing.T) {
			prefix := strings.Repeat(".", chunkSize-bytesBeforeSeam)
			r := memReaderAt{data: []byte(prefix + match + " tail")}
			wantOffset := int64(len(prefix))

			for _, backward := range []bool{false, true} {
				var got []Match
				find := FindRegexp
				if backward {
					find = FindRegexpBackward
				}
				err := find(context.Background(), r, re, RegexOptions{
					ChunkSize:      chunkSize,
					MaxMatchWindow: len(match),
				}, func(match Match) error {
					got = append(got, match)
					return nil
				})
				if err != nil {
					t.Fatalf("backward=%v: %v", backward, err)
				}
				if len(got) != 1 || got[0].Offset != wantOffset || got[0].Length != len(match) {
					t.Fatalf("backward=%v matches=%#v, want offset=%d length=%d", backward, got, wantOffset, len(match))
				}
			}
		})
	}
}

func TestFindRegexpMaximumWidthPreflightDoesNotRead(t *testing.T) {
	r := &countingReaderAt{data: []byte("before a12345b after")}
	re, err := CompileRegexpForTesting([]byte(`a[0-9]{5}b`), false)
	if err != nil {
		t.Fatal(err)
	}

	err = FindRegexp(context.Background(), r, re, RegexOptions{
		ChunkSize:      8,
		MaxMatchWindow: 6,
	}, func(Match) error {
		t.Fatal("rejected regex emitted a match")
		return nil
	})
	if !errors.Is(err, regexutil.ErrRegexExceedsWindow) {
		t.Fatalf("error = %v, want ErrRegexExceedsWindow", err)
	}
	if r.reads != 0 {
		t.Fatalf("preflight performed %d source reads", r.reads)
	}

	var matches []Match
	err = FindRegexp(context.Background(), r, re, RegexOptions{
		ChunkSize:      8,
		MaxMatchWindow: 7,
	}, func(match Match) error {
		matches = append(matches, match)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 || matches[0].Offset != 7 || matches[0].Length != 7 {
		t.Fatalf("accepted matches = %#v", matches)
	}
}

func TestFindRegexpOversizedChunkFailsBeforeRead(t *testing.T) {
	r := &countingReaderAt{data: []byte("x")}
	re, err := CompileRegexpForTesting([]byte(`x`), false)
	if err != nil {
		t.Fatal(err)
	}
	err = FindRegexp(context.Background(), r, re, RegexOptions{
		ChunkSize:      maxRegexChunkSize + 1,
		MaxMatchWindow: 1,
	}, func(Match) error { return nil })
	if !errors.Is(err, regexutil.ErrRegexResourceLimit) {
		t.Fatalf("error = %v, want ErrRegexResourceLimit", err)
	}
	if r.reads != 0 {
		t.Fatalf("resource preflight performed %d reads", r.reads)
	}
}

func TestFindRegexpBoundaryAndUnboundedPatternsFailBeforeRead(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
		want    error
	}{
		{name: "unbounded", pattern: `begin(?:.|\n)*end`, want: regexutil.ErrUnboundedRegex},
		{name: "anchor", pattern: `^begin`, want: regexutil.ErrRegexBoundaryContext},
		{name: "word boundary", pattern: `\bword\b`, want: regexutil.ErrRegexBoundaryContext},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &countingReaderAt{data: []byte("begin word end")}
			re, err := CompileRegexpForTesting([]byte(tt.pattern), false)
			if err != nil {
				t.Fatal(err)
			}
			err = FindRegexp(context.Background(), r, re, RegexOptions{ChunkSize: 16, MaxMatchWindow: 16}, func(Match) error {
				t.Fatal("rejected regex emitted a match")
				return nil
			})
			if !errors.Is(err, tt.want) {
				t.Fatalf("error = %v, want %v", err, tt.want)
			}
			if r.reads != 0 {
				t.Fatalf("preflight performed %d source reads", r.reads)
			}
		})
	}
}

func TestFindRegexpDifferentialAgainstWholeFileRegexp(t *testing.T) {
	tests := []struct {
		pattern string
		window  int
	}{
		{pattern: `a.{0,2}c`, window: 10},
		{pattern: `(?:ab|a)c?`, window: 3},
		{pattern: `b[ac]{1,3}`, window: 4},
	}
	random := rand.New(rand.NewSource(20260712))
	alphabet := []byte("abc ")
	for _, tt := range tests {
		re := regexp.MustCompile(tt.pattern)
		for sample := 0; sample < 12; sample++ {
			source := make([]byte, 160+sample)
			for i := range source {
				source[i] = alphabet[random.Intn(len(alphabet))]
			}
			wantLocs := re.FindAllIndex(source, -1)
			wantForward := make([]Match, len(wantLocs))
			wantBackward := make([]Match, len(wantLocs))
			for i, loc := range wantLocs {
				match := Match{Offset: int64(loc[0]), Length: loc[1] - loc[0]}
				wantForward[i] = match
				wantBackward[len(wantLocs)-1-i] = match
			}

			for chunkSize := tt.window; chunkSize <= tt.window+7; chunkSize++ {
				for _, backward := range []bool{false, true} {
					var got []Match
					find := FindRegexp
					want := wantForward
					if backward {
						find = FindRegexpBackward
						want = wantBackward
					}
					err := find(context.Background(), memReaderAt{data: source}, re, RegexOptions{
						ChunkSize:      chunkSize,
						MaxMatchWindow: tt.window,
					}, func(match Match) error {
						got = append(got, match)
						return nil
					})
					if err != nil {
						t.Fatalf("pattern=%q sample=%d chunk=%d backward=%v: %v", tt.pattern, sample, chunkSize, backward, err)
					}
					if !slices.Equal(got, want) {
						t.Fatalf("pattern=%q sample=%d chunk=%d backward=%v got=%v want=%v source=%q", tt.pattern, sample, chunkSize, backward, got, want, source)
					}
				}
			}
		}
	}
}

func TestFindRegexpVariableWidthMatchesPreserveWholeStreamPartition(t *testing.T) {
	source := []byte(strings.Repeat("a", 11))
	re := regexp.MustCompile(`a{1,3}`)
	wantLocs := re.FindAllIndex(source, -1)
	wantForward := make([]Match, len(wantLocs))
	wantBackward := make([]Match, len(wantLocs))
	for i, loc := range wantLocs {
		match := Match{Offset: int64(loc[0]), Length: loc[1] - loc[0]}
		wantForward[i] = match
		wantBackward[len(wantLocs)-1-i] = match
	}

	for chunkSize := 3; chunkSize <= 8; chunkSize++ {
		for _, backward := range []bool{false, true} {
			var got []Match
			find := FindRegexp
			want := wantForward
			if backward {
				find = FindRegexpBackward
				want = wantBackward
			}
			err := find(context.Background(), memReaderAt{data: source}, re, RegexOptions{
				ChunkSize:      chunkSize,
				MaxMatchWindow: 3,
			}, func(match Match) error {
				got = append(got, match)
				return nil
			})
			if err != nil {
				t.Fatalf("chunk=%d backward=%v: %v", chunkSize, backward, err)
			}
			if !slices.Equal(got, want) {
				t.Fatalf("chunk=%d backward=%v got=%v want=%v", chunkSize, backward, got, want)
			}
		}
	}
}

func TestFindRegexpForwardStartOffsetUsesOneContinuousSuffixStream(t *testing.T) {
	source := []byte("aaaaaaaaaaa")
	re := regexp.MustCompile(`a{1,3}`)
	for start := 0; start <= len(source); start++ {
		wantLocs := re.FindAllIndex(source[start:], -1)
		want := make([]Match, len(wantLocs))
		for i, loc := range wantLocs {
			want[i] = Match{Offset: int64(start + loc[0]), Length: loc[1] - loc[0]}
		}
		for chunkSize := 3; chunkSize <= 7; chunkSize++ {
			var got []Match
			err := FindRegexp(context.Background(), memReaderAt{data: source}, re, RegexOptions{
				ChunkSize:      chunkSize,
				MaxMatchWindow: 3,
				StartOffset:    int64(start),
			}, func(match Match) error {
				got = append(got, match)
				return nil
			})
			if err != nil {
				t.Fatalf("start=%d chunk=%d: %v", start, chunkSize, err)
			}
			if !slices.Equal(got, want) {
				t.Fatalf("start=%d chunk=%d got=%v want=%v", start, chunkSize, got, want)
			}
		}
	}
}

func TestFindRegexpBackwardStartOffsetFiltersWholeStreamMatchesByStart(t *testing.T) {
	source := []byte("aaaaaaaaaaa")
	re := regexp.MustCompile(`a{1,3}`)
	all := re.FindAllIndex(source, -1)
	for start := 1; start <= len(source); start++ {
		var want []Match
		for i := len(all) - 1; i >= 0; i-- {
			if all[i][0] < start {
				want = append(want, Match{Offset: int64(all[i][0]), Length: all[i][1] - all[i][0]})
			}
		}
		for chunkSize := 3; chunkSize <= 7; chunkSize++ {
			var got []Match
			err := FindRegexpBackward(context.Background(), memReaderAt{data: source}, re, RegexOptions{
				ChunkSize:      chunkSize,
				MaxMatchWindow: 3,
				StartOffset:    int64(start),
			}, func(match Match) error {
				got = append(got, match)
				return nil
			})
			if err != nil {
				t.Fatalf("start=%d chunk=%d: %v", start, chunkSize, err)
			}
			if !slices.Equal(got, want) {
				t.Fatalf("start=%d chunk=%d got=%v want=%v", start, chunkSize, got, want)
			}
		}
	}
}

func TestFindRegexpBackwardIncludesMatchCrossingStartOffset(t *testing.T) {
	source := []byte("aaaa")
	re := regexp.MustCompile(`a{4}`)
	var got []Match
	err := FindRegexpBackward(context.Background(), memReaderAt{data: source}, re, RegexOptions{
		ChunkSize:      4,
		MaxMatchWindow: 4,
		StartOffset:    2,
	}, func(match Match) error {
		got = append(got, match)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []Match{{Offset: 0, Length: 4}}
	if !slices.Equal(got, want) {
		t.Fatalf("got=%v want=%v", got, want)
	}
}

func TestFindRegexpUTF8SeamsAndOffsetsMatchGoRegexp(t *testing.T) {
	source := []byte("é🙂界aé🙂b界é")
	re := regexp.MustCompile(`.{1,2}`)
	const matchWindow = 8 // two maximum-width UTF-8 runes

	for chunkSize := matchWindow; chunkSize <= matchWindow+5; chunkSize++ {
		for start := 0; start <= len(source); start++ {
			wantLocs := re.FindAllIndex(source[start:], -1)
			want := make([]Match, len(wantLocs))
			for i, loc := range wantLocs {
				want[i] = Match{Offset: int64(start + loc[0]), Length: loc[1] - loc[0]}
			}
			var got []Match
			err := FindRegexp(context.Background(), memReaderAt{data: source}, re, RegexOptions{
				ChunkSize:      chunkSize,
				MaxMatchWindow: matchWindow,
				StartOffset:    int64(start),
			}, func(match Match) error {
				got = append(got, match)
				return nil
			})
			if err != nil {
				t.Fatalf("forward start=%d chunk=%d: %v", start, chunkSize, err)
			}
			if !slices.Equal(got, want) {
				t.Fatalf("forward start=%d chunk=%d got=%v want=%v", start, chunkSize, got, want)
			}
		}

		all := re.FindAllIndex(source, -1)
		for start := 1; start <= len(source); start++ {
			var want []Match
			for i := len(all) - 1; i >= 0; i-- {
				if all[i][0] < start {
					want = append(want, Match{Offset: int64(all[i][0]), Length: all[i][1] - all[i][0]})
				}
			}
			var got []Match
			err := FindRegexpBackward(context.Background(), memReaderAt{data: source}, re, RegexOptions{
				ChunkSize:      chunkSize,
				MaxMatchWindow: matchWindow,
				StartOffset:    int64(start),
			}, func(match Match) error {
				got = append(got, match)
				return nil
			})
			if err != nil {
				t.Fatalf("backward start=%d chunk=%d: %v", start, chunkSize, err)
			}
			if !slices.Equal(got, want) {
				t.Fatalf("backward start=%d chunk=%d got=%v want=%v", start, chunkSize, got, want)
			}
		}
	}
}

func TestRegexStreamWorkingBufferBound(t *testing.T) {
	opts := RegexOptions{
		ChunkSize:      maxRegexChunkSize,
		MaxMatchWindow: regexutil.MaxExactMatchWindowBytes,
	}
	if got := regexStreamWorkingBytes(opts); got != maxRegexStreamWorkingBytes {
		t.Fatalf("maximum stream buffer = %d, want %d", got, maxRegexStreamWorkingBytes)
	}
	if got := regexStreamCarryBytes(opts); got != 2*regexutil.MaxExactMatchWindowBytes+3 {
		t.Fatalf("maximum carry = %d, want %d", got, 2*regexutil.MaxExactMatchWindowBytes+3)
	}
}
