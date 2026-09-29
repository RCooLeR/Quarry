package replace

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/quarry/quarry-wails3/internal/regexutil"
)

func TestPreviewAPIsRejectUnboundedCollectors(t *testing.T) {
	r := memReaderAt{data: []byte("alpha")}
	rules := []BatchRule{{Name: "letter", Find: []byte("a"), Replace: []byte("x")}}

	for _, hits := range []int{-1, 0, MaxPreviewHits + 1, int(^uint(0) >> 1)} {
		if _, err := PreviewPlain(context.Background(), r, []byte("a"), []byte("x"), PreviewOptions{ChunkSize: 1, MaxHits: hits}); !errors.Is(err, ErrResourceLimit) {
			t.Fatalf("plain MaxHits=%d error = %v, want ErrResourceLimit", hits, err)
		}
		if _, _, err := PreviewBatchPlain(context.Background(), r, rules, PreviewOptions{ChunkSize: 1, MaxHits: hits}); !errors.Is(err, ErrResourceLimit) {
			t.Fatalf("batch plain MaxHits=%d error = %v, want ErrResourceLimit", hits, err)
		}
		if _, err := PreviewRegexp(context.Background(), r, []byte("a"), []byte("x"), RegexPreviewOptions{ChunkSize: 1, MaxHits: hits}, RegexOptions{ChunkSize: 1, MaxMatchWindow: 1}); !errors.Is(err, ErrResourceLimit) {
			t.Fatalf("regexp MaxHits=%d error = %v, want ErrResourceLimit", hits, err)
		}
		if _, _, err := PreviewBatchRegexp(context.Background(), r, rules, RegexPreviewOptions{ChunkSize: 1, MaxHits: hits}, RegexOptions{ChunkSize: 1, MaxMatchWindow: 1}); !errors.Is(err, ErrResourceLimit) {
			t.Fatalf("batch regexp MaxHits=%d error = %v, want ErrResourceLimit", hits, err)
		}
	}
}

func TestPreviewOptionAndAggregateBoundaries(t *testing.T) {
	if _, err := validatePreviewBase(MaxPlainChunkBytes, MaxPreviewHits, MaxPreviewBytes); err != nil {
		t.Fatalf("individual maximum rejected: %v", err)
	}
	if err := validatePreviewBudget(1, MaxPreviewBytes, MaxPlainPatternBytes, 0, 0); err != nil {
		t.Fatalf("legal maximum-size one-hit preview rejected: %v", err)
	}
	if err := validatePreviewBudget(MaxPreviewHits, MaxPreviewBytes, MaxPlainPatternBytes, MaxPlainReplacementBytes, 0); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("aggregate error = %v, want ErrResourceLimit", err)
	}

	tests := []PreviewOptions{
		{ChunkSize: -1, MaxHits: 1},
		{ChunkSize: MaxPlainChunkBytes + 1, MaxHits: 1},
		{ChunkSize: 1, MaxHits: 1, PreviewBytes: -1},
		{ChunkSize: 1, MaxHits: 1, PreviewBytes: MaxPreviewBytes + 1},
	}
	r := memReaderAt{data: []byte("x")}
	for _, opts := range tests {
		if _, err := PreviewPlain(context.Background(), r, []byte("x"), []byte("y"), opts); !errors.Is(err, ErrResourceLimit) {
			t.Fatalf("options %+v error = %v, want ErrResourceLimit", opts, err)
		}
	}
}

func TestPlainTransformLimitsFailBeforeSourceAccess(t *testing.T) {
	if err := validatePlainTransformInputs(make([]byte, MaxPlainPatternBytes), nil, MaxPlainChunkBytes); err != nil {
		t.Fatalf("maximum plain inputs rejected: %v", err)
	}

	if _, err := replacePlain(context.Background(), nil, nil, []byte("x"), nil, PlainOptions{ChunkSize: MaxPlainChunkBytes + 1}); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("chunk error = %v, want ErrResourceLimit", err)
	}
	if _, err := replacePlain(context.Background(), nil, nil, make([]byte, MaxPlainPatternBytes+1), nil, PlainOptions{ChunkSize: 1}); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("pattern error = %v, want ErrResourceLimit", err)
	}
	if _, err := replacePlain(context.Background(), nil, nil, []byte("x"), make([]byte, MaxPlainReplacementBytes+1), PlainOptions{ChunkSize: 1}); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("replacement error = %v, want ErrResourceLimit", err)
	}

	rules := []BatchRule{{Find: []byte("x"), Replace: []byte("y")}}
	if _, _, err := replaceBatchPlain(context.Background(), nil, nil, rules, BatchOptions{ChunkSize: MaxPlainChunkBytes + 1}); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("batch chunk error = %v, want ErrResourceLimit", err)
	}
	if _, err := replaceRegexp(context.Background(), nil, nil, []byte("x"), nil, RegexOptions{ChunkSize: -1}); !errors.Is(err, regexutil.ErrRegexResourceLimit) {
		t.Fatalf("regex chunk error = %v, want ErrRegexResourceLimit", err)
	}
}

func TestRegexPreviewAccountsForMatchWidthAndExpansion(t *testing.T) {
	widePattern := []byte(strings.Repeat("a{1000}", 64))
	_, err := PreviewRegexp(context.Background(), memReaderAt{data: []byte("a")}, widePattern, []byte("x"), RegexPreviewOptions{
		ChunkSize: 64 * 1024,
		MaxHits:   2_000,
	}, RegexOptions{ChunkSize: 64 * 1024, MaxMatchWindow: 64 * 1024})
	if !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("wide-match preview error = %v, want ErrResourceLimit", err)
	}

	// The template is small, but each capture reference can append the complete
	// 1,000-byte match. Expansion must be rejected before source Stat/read.
	references := MaxRegexExpandedReplacementBytes/1000 + 1
	replacement := []byte(strings.Repeat("$1", references))
	if _, err := replaceRegexp(context.Background(), nil, nil, []byte("(a{1000})"), replacement, RegexOptions{ChunkSize: 1024, MaxMatchWindow: 1000}); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("expanded replacement error = %v, want ErrResourceLimit", err)
	}

	acceptedReferences := MaxRegexExpandedReplacementBytes / 1002
	if _, err := validateRegexReplacementExpansion([]byte(strings.Repeat("$1", acceptedReferences)), 1000); err != nil {
		t.Fatalf("boundary-safe expanded replacement rejected: %v", err)
	}
}
