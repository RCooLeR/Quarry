package exportx

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/quarry/quarry-wails3/internal/encodingx"
	"github.com/quarry/quarry-wails3/internal/fileio"
)

const (
	// MaxVisibleSelectionUTF8Bytes and MaxVisibleSelectionRunes bound the
	// caller-owned Go string before encoding creates another full representation.
	MaxVisibleSelectionUTF8Bytes = 8 * 1024 * 1024
	MaxVisibleSelectionRunes     = 8 * 1024 * 1024
	// MaxVisibleSelectionOutputBytes includes the largest UTF-16 expansion of
	// an allowed ASCII selection and its two-byte BOM.
	MaxVisibleSelectionOutputBytes int64 = 16*1024*1024 + 2
)

// ExportVisibleText exports a complete valid-text selection.
func ExportVisibleText(ctx context.Context, outputPath string, text string, encodingName string) (Summary, error) {
	return ExportVisibleTextWithOptions(ctx, outputPath, text, encodingName, Options{})
}

// ExportVisibleTextWithOptions rejects an invalid UTF-8 Go string before
// creating an output and emits the configured target encoding only when the
// complete selection is representable.
func ExportVisibleTextWithOptions(ctx context.Context, outputPath string, text string, encodingName string, opts Options) (_ Summary, retErr error) {
	if outputPath == "" {
		return Summary{}, errors.New("output path is required")
	}
	if err := fileio.ValidateExactOutputPath(outputPath); err != nil {
		return Summary{}, err
	}
	if ctx == nil {
		ctx = context.Background()
	}

	if err := ctx.Err(); err != nil {
		return Summary{}, err
	}
	if !utf8.ValidString(text) {
		return Summary{}, fmt.Errorf("%w: visible selection contains invalid UTF-8", ErrInvalidSourceText)
	}
	if len(text) > MaxVisibleSelectionUTF8Bytes {
		return Summary{}, &MaterializedLimitError{Limit: "visible selection UTF-8 bytes", Value: int64(len(text)), Max: MaxVisibleSelectionUTF8Bytes}
	}
	targetKind, err := parseTextEncoding(encodingName)
	if err != nil {
		return Summary{}, err
	}
	bom := targetBOM(encodingName)
	expectedOutputBytes, err := visibleSelectionOutputSize(text, targetKind, len(bom))
	if err != nil {
		return Summary{}, err
	}

	encoded, err := encodingx.EncodeString(encodingName, text)
	if err != nil {
		return Summary{}, err
	}
	if err := validateCompleteText(targetKind, encoded); err != nil {
		return Summary{}, err
	}
	actualOutputBytes := int64(len(bom)) + int64(len(encoded))
	if actualOutputBytes != expectedOutputBytes || actualOutputBytes > MaxVisibleSelectionOutputBytes {
		return Summary{}, &MaterializedLimitError{Limit: "visible selection encoded bytes", Value: actualOutputBytes, Max: MaxVisibleSelectionOutputBytes}
	}
	summary := Summary{
		OutputPath:     outputPath,
		BytesWritten:   actualOutputBytes,
		Mode:           "visible-selection",
		SourceEncoding: encodingName,
		TargetEncoding: encodingName,
	}
	if opts.ComputeSHA256 {
		hash := sha256.New()
		_, _ = hash.Write(bom)
		_, _ = hash.Write(encoded)
		summary.ChecksumAlgorithm = "sha256"
		summary.SHA256 = hex.EncodeToString(hash.Sum(nil))
	}
	if shouldWriteExportManifest(opts) {
		summary.ManifestPath = exportManifestPathFor(outputPath, opts.ManifestPath)
		if err := ensureExportManifestAvailable(summary.ManifestPath, outputPath); err != nil {
			return summary, err
		}
	}

	dst, err := openCreatedOutput(outputPath)
	if err != nil {
		return summary, err
	}
	defer func() { retErr = errors.Join(retErr, dst.Cleanup()) }()
	for _, part := range [][]byte{bom, encoded} {
		if len(part) == 0 {
			continue
		}
		if n, err := dst.Write(part); err != nil {
			return summary, err
		} else if n != len(part) {
			return summary, io.ErrShortWrite
		}
	}
	var commitErr error
	if opts.ValidateSource != nil {
		commitErr = dst.CommitContextValidated(ctx, opts.ValidateSource)
	} else {
		commitErr = dst.CommitContext(ctx)
	}
	if commitErr != nil {
		markUncertainPublication(&summary, commitErr)
		return summary, commitErr
	}

	if shouldWriteExportManifest(opts) {
		if err := writeExportManifest(ctx, summary, opts.ValidateSource); err != nil {
			return summary, exportManifestPublicationError(summary, err)
		}
	}
	return summary, nil
}

func visibleSelectionOutputSize(text string, kind textEncodingKind, bomBytes int) (int64, error) {
	var runes int64
	var payloadBytes int64
	for _, value := range text {
		runes++
		if runes > MaxVisibleSelectionRunes {
			return 0, &MaterializedLimitError{Limit: "visible selection runes", Value: runes, Max: MaxVisibleSelectionRunes}
		}
		switch kind {
		case textEncodingUTF8:
			payloadBytes += int64(utf8.RuneLen(value))
		case textEncodingUTF16LE, textEncodingUTF16BE:
			payloadBytes += int64(2 * utf16.RuneLen(value))
		case textEncodingWindows1251, textEncodingWindows1252:
			payloadBytes++
		default:
			return 0, errors.New("unsupported visible-selection encoding")
		}
		if payloadBytes+int64(bomBytes) > MaxVisibleSelectionOutputBytes {
			return 0, &MaterializedLimitError{Limit: "visible selection encoded bytes", Value: payloadBytes + int64(bomBytes), Max: MaxVisibleSelectionOutputBytes}
		}
	}
	return payloadBytes + int64(bomBytes), nil
}
