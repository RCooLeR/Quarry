package search

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"unicode/utf8"

	"github.com/quarry/quarry-wails3/internal/regexutil"
)

const defaultRegexMatchWindow = 1 * 1024 * 1024
const maxBackwardRegexHits = MaxCollectedHits
const maxRegexChunkSize = MaxChunkBytes
const maxRegexStreamWorkingBytes = maxRegexChunkSize + 2*regexutil.MaxExactMatchWindowBytes + utf8.UTFMax - 1

// RegexOptions controls chunked regex search.
type RegexOptions struct {
	ChunkSize       int
	MaxHits         int
	StartOffset     int64
	Backward        bool
	CaseInsensitive bool
	MaxMatchWindow  int
	Progress        func(Progress)
}

// CollectRegexp returns up to opts.MaxHits regex matches with previews.
func CollectRegexp(ctx context.Context, r ReaderAtSize, pattern []byte, opts RegexOptions, previewBytes int) ([]Result, error) {
	if err := validateRegexOptions(opts); err != nil {
		return nil, err
	}
	if err := validateCollectRequest(len(pattern), opts.MaxHits, previewBytes); err != nil {
		return nil, err
	}
	opts = normalizeRegexOptions(opts)
	if err := validateRegexOptions(opts); err != nil {
		return nil, err
	}
	re, analysis, err := regexutil.CompileBounded(pattern, opts.CaseInsensitive, opts.MaxMatchWindow)
	if err != nil {
		return nil, err
	}
	if err := validateCollectRequest(int(analysis.MaxMatchBytes), opts.MaxHits, previewBytes); err != nil {
		return nil, err
	}

	var results []Result
	find := FindRegexp
	if opts.Backward {
		find = FindRegexpBackward
	}
	err = find(ctx, r, re, opts, func(m Match) error {
		preview, start, err := previewAt(r, m.Offset, m.Length, previewBytes)
		if err != nil {
			return err
		}
		results = append(results, Result{
			Match:        m,
			PreviewStart: start,
			Preview:      preview,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return results, nil
}

// FindRegexp scans a file-like object in chunks and emits regex matches.
func FindRegexp(ctx context.Context, r ReaderAtSize, re *regexp.Regexp, opts RegexOptions, emit func(Match) error) error {
	if re == nil {
		return errors.New("nil regexp")
	}

	if err := validateRegexOptions(opts); err != nil {
		return err
	}
	opts = normalizeRegexOptions(opts)
	if err := regexutil.ValidateCompiledBounded(re, opts.MaxMatchWindow); err != nil {
		return err
	}
	size := r.Size()
	if size < 0 {
		return errors.New("source size must not be negative")
	}
	startOffset := clampOffset(opts.StartOffset, size)
	hits := 0
	return scanRegexpStream(ctx, r, re, startOffset, size, opts, func(match Match) (bool, error) {
		if err := emit(match); err != nil {
			return false, err
		}
		hits++
		return opts.MaxHits > 0 && hits >= opts.MaxHits, nil
	}, func(processed int64) {
		if opts.Progress != nil {
			opts.Progress(Progress{
				BytesProcessed: processed,
				BytesTotal:     size,
				Matches:        int64(hits),
			})
		}
	})
}

// FindRegexpBackward emits whole-stream matches whose starts precede
// opts.StartOffset, in descending order. A positive MaxHits bounds the retained
// tail; an unbounded request fails at the fixed safety ceiling instead of
// materializing an arbitrary number of matches.
func FindRegexpBackward(ctx context.Context, r ReaderAtSize, re *regexp.Regexp, opts RegexOptions, emit func(Match) error) error {
	if re == nil {
		return errors.New("nil regexp")
	}

	if err := validateRegexOptions(opts); err != nil {
		return err
	}
	opts = normalizeRegexOptions(opts)
	if err := regexutil.ValidateCompiledBounded(re, opts.MaxMatchWindow); err != nil {
		return err
	}
	if opts.MaxHits < 0 {
		return errors.New("maximum hits must not be negative")
	}
	if opts.MaxHits > maxBackwardRegexHits {
		return errors.New("maximum hits exceeds the backward-search safety limit")
	}
	size := r.Size()
	if size < 0 {
		return errors.New("source size must not be negative")
	}
	endOffset := opts.StartOffset
	if endOffset <= 0 || endOffset > size {
		endOffset = size
	}

	retainLimit := opts.MaxHits
	if retainLimit == 0 {
		retainLimit = maxBackwardRegexHits
	}
	matches := make([]Match, 0, retainLimit)
	ringStart := 0
	eligible := 0

	// Go's regexp package defines a forward, leftmost-first non-overlapping
	// stream. That partition cannot be reconstructed exactly by independently
	// matching windows from right to left: a variable-width match can shift every
	// later match. Scan from the beginning and retain only the nearest bounded
	// tail. At most MaxMatchWindow bytes beyond the cutoff are needed to finalize
	// a match whose start is still eligible.
	scanEnd := size
	lookahead := int64(opts.MaxMatchWindow)
	if remaining := size - endOffset; lookahead < remaining {
		scanEnd = endOffset + lookahead
	}
	err := scanRegexpStream(ctx, r, re, 0, scanEnd, opts, func(match Match) (bool, error) {
		if match.Offset >= endOffset {
			return true, nil
		}
		eligible++
		if len(matches) < retainLimit {
			matches = append(matches, match)
			return false, nil
		}
		if opts.MaxHits == 0 {
			return false, errors.New("backward regex search exceeded the safe match limit; specify MaxHits")
		}
		matches[ringStart] = match
		ringStart = (ringStart + 1) % len(matches)
		return false, nil
	}, func(processed int64) {
		if opts.Progress == nil {
			return
		}
		if processed > endOffset {
			processed = endOffset
		}
		retained := min(eligible, len(matches))
		opts.Progress(Progress{
			BytesProcessed: int64(processed),
			BytesTotal:     endOffset,
			Matches:        int64(retained),
		})
	})
	if err != nil {
		return err
	}

	hits := 0
	for i := len(matches) - 1; i >= 0; i-- {
		if err := ctx.Err(); err != nil {
			return err
		}
		index := (ringStart + i) % len(matches)
		if err := emit(matches[index]); err != nil {
			return err
		}
		hits++
	}
	if opts.Progress != nil && endOffset > 0 {
		opts.Progress(Progress{
			BytesProcessed: endOffset,
			BytesTotal:     endOffset,
			Matches:        int64(hits),
		})
	}
	return nil
}

// scanRegexpStream applies re to one logical byte stream while retaining only
// the suffix that cannot yet be finalized. Unlike independently rescanned
// overlap windows, the carry always starts at regexp's next search position, so
// variable-width and alternative matches preserve Go's leftmost-first,
// non-overlapping semantics across every chunk seam.
func scanRegexpStream(
	ctx context.Context,
	r ReaderAtSize,
	re *regexp.Regexp,
	start int64,
	end int64,
	opts RegexOptions,
	visit func(Match) (bool, error),
	progress func(processed int64),
) error {
	if start >= end {
		return nil
	}
	carryBudget := regexStreamCarryBytes(opts)
	// This is the scanner's only grow-only source buffer. Reuse its retained
	// prefix in place so the maximum validated configuration stays below
	// maxRegexStreamWorkingBytes instead of simultaneously allocating separate
	// chunk, carry, and assembled-window buffers.
	workingBytes := regexStreamWorkingBytes(opts)
	if available := end - start; available < int64(workingBytes) {
		workingBytes = int(available)
	}
	window := make([]byte, 0, workingBytes)
	processed := start

	for processed < end {
		if err := ctx.Err(); err != nil {
			return err
		}
		want := opts.ChunkSize
		if remaining := end - processed; remaining < int64(want) {
			want = int(remaining)
		}
		carryLen := len(window)
		window = window[:carryLen+want]
		n, readErr := r.ReadAt(window[carryLen:], processed)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return readErr
		}
		if n != want {
			return io.ErrUnexpectedEOF
		}
		atEnd := processed+int64(n) == end

		windowStart := processed - int64(carryLen)
		processLimit := len(window) - opts.MaxMatchWindow
		if atEnd {
			processLimit = len(window)
		}
		if processLimit < 0 {
			processLimit = 0
		}
		if !atEnd {
			processLimit = alignRegexpProcessLimit(window, processLimit)
		}

		consumed, stopped, err := scanRegexpPrefix(ctx, window, processLimit, windowStart, re, visit)
		if err != nil {
			return err
		}
		if len(window)-consumed > carryBudget {
			return errors.New("regex match exceeded the configured match window")
		}
		retained := copy(window, window[consumed:])
		window = window[:retained]
		processed += int64(n)
		if progress != nil {
			progress(processed)
		}
		if stopped {
			return nil
		}
		if atEnd {
			return nil
		}
	}
	return nil
}

func regexStreamCarryBytes(opts RegexOptions) int {
	return opts.MaxMatchWindow*2 + utf8.UTFMax - 1
}

func regexStreamWorkingBytes(opts RegexOptions) int {
	return opts.ChunkSize + regexStreamCarryBytes(opts)
}

// alignRegexpProcessLimit avoids making the next logical regexp stream begin in
// the middle of a valid UTF-8 rune. UTF-8 is self-synchronizing within at most
// UTFMax-1 continuation bytes; invalid continuation runs may be retained a few
// extra bytes but still make bounded progress.
func alignRegexpProcessLimit(window []byte, limit int) int {
	if limit <= 0 || limit >= len(window) || utf8.RuneStart(window[limit]) {
		return limit
	}
	aligned := limit
	for retained := 0; aligned > 0 && retained < utf8.UTFMax-1 && !utf8.RuneStart(window[aligned]); retained++ {
		aligned--
	}
	return aligned
}

func scanRegexpPrefix(
	ctx context.Context,
	window []byte,
	processLimit int,
	windowStart int64,
	re *regexp.Regexp,
	visit func(Match) (bool, error),
) (consumed int, stopped bool, err error) {
	consumed = processLimit
	work := 0
	for pos := 0; pos <= len(window); {
		work++
		if work%1024 == 0 {
			if err := ctx.Err(); err != nil {
				return consumed, false, err
			}
		}
		loc := re.FindIndex(window[pos:])
		if loc == nil {
			break
		}
		matchStart, matchEnd := pos+loc[0], pos+loc[1]
		if matchEnd > processLimit {
			if matchStart < processLimit {
				consumed = matchStart
			}
			break
		}
		stop, err := visit(Match{Offset: windowStart + int64(matchStart), Length: matchEnd - matchStart})
		if err != nil {
			return consumed, false, err
		}
		if stop {
			return consumed, true, nil
		}
		pos = matchEnd
	}
	return consumed, false, nil
}

func compileRegexp(pattern []byte, caseInsensitive bool) (*regexp.Regexp, error) {
	return regexutil.Compile(pattern, caseInsensitive)
}

func normalizeRegexOptions(opts RegexOptions) RegexOptions {
	if opts.ChunkSize == 0 {
		opts.ChunkSize = 4 * 1024 * 1024
	}
	if opts.MaxMatchWindow == 0 {
		opts.MaxMatchWindow = defaultRegexMatchWindow
	}
	if opts.MaxMatchWindow > opts.ChunkSize {
		opts.MaxMatchWindow = opts.ChunkSize
	}
	return opts
}

func validateRegexOptions(opts RegexOptions) error {
	if opts.ChunkSize < 0 {
		return fmt.Errorf("%w: search chunk size must not be negative", regexutil.ErrRegexResourceLimit)
	}
	if opts.ChunkSize > maxRegexChunkSize {
		return fmt.Errorf("%w: search chunk %d exceeds %d bytes", regexutil.ErrRegexResourceLimit, opts.ChunkSize, maxRegexChunkSize)
	}
	if opts.MaxMatchWindow < 0 {
		return fmt.Errorf("%w: regex match window must not be negative", regexutil.ErrRegexResourceLimit)
	}
	if opts.MaxMatchWindow > regexutil.MaxExactMatchWindowBytes {
		return fmt.Errorf("%w: match window %d exceeds %d bytes", regexutil.ErrRegexResourceLimit, opts.MaxMatchWindow, regexutil.MaxExactMatchWindowBytes)
	}
	if opts.MaxHits < 0 {
		return searchLimit("maximum hits must not be negative")
	}
	return nil
}

func clampOffset(offset int64, size int64) int64 {
	if offset < 0 {
		return 0
	}
	if offset > size {
		return size
	}
	return offset
}

// CompileRegexpForTesting exposes regex validation to tests in sibling packages.
func CompileRegexpForTesting(pattern []byte, caseInsensitive bool) (*regexp.Regexp, error) {
	return compileRegexp(pattern, caseInsensitive)
}
