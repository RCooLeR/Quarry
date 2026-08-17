package exportx

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/quarry/quarry-wails3/internal/document"
	"github.com/quarry/quarry-wails3/internal/encodingx"
)

const (
	DefaultClipboardMaxBytes int64 = 8 * 1024 * 1024
	// MaxClipboardBytes is an absolute engine limit. A caller can choose a
	// smaller range budget, but cannot turn a bridge value into an allocation
	// permission larger than this value.
	MaxClipboardBytes int64 = DefaultClipboardMaxBytes
)

var ErrMaterializedLimit = errors.New("export materialization limit exceeded")

type MaterializedLimitError struct {
	Limit string
	Value int64
	Max   int64
}

func (e *MaterializedLimitError) Error() string {
	return fmt.Sprintf("%s: %s is %d, maximum is %d", ErrMaterializedLimit, e.Limit, e.Value, e.Max)
}

func (e *MaterializedLimitError) Unwrap() error { return ErrMaterializedLimit }

type CopySummary struct {
	StartOffset   int64
	EndOffset     int64
	BytesRead     int64
	StartLine     int64
	EndLine       int64
	Mode          string
	UsedLineRange bool
}

type RangeTooLargeError struct {
	RequestedBytes int64
	MaxBytes       int64
}

func (e *RangeTooLargeError) Error() string {
	return "clipboard range exceeds safe limit"
}

func CopyByteRange(ctx context.Context, doc *document.FileDocument, start int64, end int64, maxBytes int64) (CopySummary, string, error) {
	if doc == nil {
		return CopySummary{}, "", errors.New("document is required")
	}
	var err error
	maxBytes, err = normalizeClipboardMaxBytes(maxBytes)
	if err != nil {
		return CopySummary{}, "", err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if start < 0 {
		start = 0
	}
	size := doc.Size()
	if end <= 0 || end > size {
		end = size
	}
	if end < start {
		return CopySummary{}, "", errors.New("end offset must be greater than or equal to start offset")
	}

	total := end - start
	if total > maxBytes {
		return CopySummary{}, "", &RangeTooLargeError{
			RequestedBytes: total,
			MaxBytes:       maxBytes,
		}
	}

	reader := io.NewSectionReader(doc, start, total)
	buf := make([]byte, 256*1024)
	data := make([]byte, 0, total)

	for {
		select {
		case <-ctx.Done():
			return CopySummary{}, "", ctx.Err()
		default:
		}

		n, readErr := reader.Read(buf)
		if n > 0 {
			data = append(data, buf[:n]...)
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return CopySummary{}, "", readErr
		}
	}
	if int64(len(data)) != total {
		return CopySummary{}, "", io.ErrUnexpectedEOF
	}

	return CopySummary{
		StartOffset: start,
		EndOffset:   end,
		BytesRead:   int64(len(data)),
		Mode:        "byte-range",
	}, encodingx.DecodeBytesBestEffort(doc.Metadata().Encoding, data), nil
}

func CopyVisibleRange(ctx context.Context, doc *document.FileDocument, start int64, end int64, maxBytes int64) (CopySummary, string, error) {
	summary, text, err := CopyByteRange(ctx, doc, start, end, maxBytes)
	if err != nil {
		return CopySummary{}, "", err
	}
	summary.Mode = "visible-range"
	return summary, text, nil
}

func CopyLineRange(ctx context.Context, doc *document.FileDocument, startLine int64, endLine int64, maxBytes int64) (CopySummary, string, error) {
	if doc == nil {
		return CopySummary{}, "", errors.New("document is required")
	}
	var err error
	maxBytes, err = normalizeClipboardMaxBytes(maxBytes)
	if err != nil {
		return CopySummary{}, "", err
	}
	startOffset, endOffset, err := doc.LineRangeOffsets(startLine, endLine)
	if err != nil {
		return CopySummary{}, "", err
	}
	summary, text, err := CopyByteRange(ctx, doc, startOffset, endOffset, maxBytes)
	if err != nil {
		return CopySummary{}, "", err
	}
	summary.Mode = "line-range"
	summary.StartLine = startLine
	summary.EndLine = endLine
	summary.UsedLineRange = true
	return summary, text, nil
}

func normalizeClipboardMaxBytes(maxBytes int64) (int64, error) {
	if maxBytes < 0 {
		return 0, &MaterializedLimitError{Limit: "clipboard maximum bytes", Value: maxBytes, Max: MaxClipboardBytes}
	}
	if maxBytes == 0 {
		return DefaultClipboardMaxBytes, nil
	}
	if maxBytes > MaxClipboardBytes {
		return 0, &MaterializedLimitError{Limit: "clipboard maximum bytes", Value: maxBytes, Max: MaxClipboardBytes}
	}
	return maxBytes, nil
}
