package csv

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	stdcsv "encoding/csv"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/quarry/quarry-wails3/internal/fileio"
)

// RedactMode is how a column's values are masked.
type RedactMode string

const (
	RedactNull  RedactMode = "null"  // empty value
	RedactFixed RedactMode = "fixed" // a constant replacement
	RedactHash  RedactMode = "hash"  // operation-keyed, 128-bit HMAC pseudonym
	RedactEmail RedactMode = "email" // keep first char + domain: a***@x.com
)

const (
	PseudonymKeyBytes    = 32
	MaxPseudonymKeyBytes = 64
	pseudonymOutputBytes = 16
)

type RedactOptions struct {
	Delimiter      rune
	HasHeader      bool
	Columns        map[int]RedactMode // 0-based source column → mask
	Replacement    string             // used by RedactFixed (default "REDACTED")
	PseudonymKey   []byte
	MaxRecordBytes int64
	ExpectedSource *SourceExpectation
	Progress       func(records int64, bytes int64)
}

type RedactSummary struct {
	RecordsRead    int64
	RecordsWritten int64
	CellsMasked    int64 // selected cells whose value changed under the mask
	Delimiter      rune
}

// ValidateRedactOptions verifies caller-controlled options before any output is
// created. Modes are deliberately exact and case-sensitive: accepting a
// future, misspelled, or differently-cased mode as a no-op would leak selected
// values into an apparently redacted file.
func ValidateRedactOptions(opts RedactOptions) error {
	if err := validateTransformColumnMappingCount("CSV redaction", len(opts.Columns), true); err != nil {
		if len(opts.Columns) == 0 {
			return errors.New("select at least one column to redact")
		}
		return err
	}
	configStringBytes := 0
	if err := addTransformConfigString(&configStringBytes, "CSV redaction", opts.Replacement); err != nil {
		return err
	}
	for _, mode := range opts.Columns {
		if err := addTransformConfigString(&configStringBytes, "CSV redaction", string(mode)); err != nil {
			return err
		}
	}
	if err := ValidateDelimiter(opts.Delimiter); err != nil {
		return err
	}
	if _, err := normalizeLogicalRecordLimit(opts.MaxRecordBytes); err != nil {
		return err
	}
	for _, index := range sortedRedactColumnIndexes(opts.Columns) {
		if index < 0 {
			return fmt.Errorf("redaction column index %d is negative", index)
		}
		if !validRedactMode(opts.Columns[index]) {
			return fmt.Errorf("invalid redaction mode %q for column %d", opts.Columns[index], index)
		}
	}
	if redactUsesHash(opts.Columns) {
		if len(opts.PseudonymKey) < PseudonymKeyBytes {
			return fmt.Errorf("hash redaction requires a per-operation key of at least %d bytes", PseudonymKeyBytes)
		}
		if len(opts.PseudonymKey) > MaxPseudonymKeyBytes {
			return fmt.Errorf("hash redaction key exceeds %d-byte limit", MaxPseudonymKeyBytes)
		}
	}
	return nil
}

func redactUsesHash(columns map[int]RedactMode) bool {
	for _, mode := range columns {
		if mode == RedactHash {
			return true
		}
	}
	return false
}

// NewPseudonymKey creates an operation-local key. Reusing a key deliberately
// links identical values; Quarry's service creates a fresh key per export.
func NewPseudonymKey() ([]byte, error) {
	key := make([]byte, PseudonymKeyBytes)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate redaction pseudonym key: %w", err)
	}
	return key, nil
}

func validRedactMode(mode RedactMode) bool {
	switch mode {
	case RedactNull, RedactFixed, RedactHash, RedactEmail:
		return true
	default:
		return false
	}
}

func sortedRedactColumnIndexes(columns map[int]RedactMode) []int {
	indexes := make([]int, 0, len(columns))
	for index := range columns {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	return indexes
}

func validateRedactColumnRange(columns map[int]RedactMode, fieldCount int) error {
	for _, index := range sortedRedactColumnIndexes(columns) {
		if index >= fieldCount {
			return fmt.Errorf("redaction column index %d is outside the first record's %d fields", index, fieldCount)
		}
	}
	return nil
}

func maskValue(value string, mode RedactMode, replacement string, pseudonymKey []byte) (string, error) {
	switch mode {
	case RedactNull:
		return "", nil
	case RedactFixed:
		return replacement, nil
	case RedactHash:
		if value == "" {
			return "", nil
		}
		if len(pseudonymKey) < PseudonymKeyBytes || len(pseudonymKey) > MaxPseudonymKeyBytes {
			return "", errors.New("hash redaction requires a valid per-operation pseudonym key")
		}
		mac := hmac.New(sha256.New, pseudonymKey)
		_, _ = mac.Write([]byte(value))
		sum := mac.Sum(nil)
		return hex.EncodeToString(sum[:pseudonymOutputBytes]), nil
	case RedactEmail:
		if !utf8.ValidString(value) {
			return "", errors.New("cannot email-mask invalid UTF-8")
		}
		at := strings.LastIndexByte(value, '@')
		if at <= 0 {
			if value == "" {
				return "", nil
			}
			_, size := utf8.DecodeRuneInString(value)
			return value[:size] + "***", nil
		}
		_, size := utf8.DecodeRuneInString(value)
		return value[:size] + "***" + value[at:], nil
	default:
		return "", fmt.Errorf("invalid redaction mode %q", mode)
	}
}

// RedactColumnsFile streams src to dst (CSV), masking the configured columns.
// The header row (if any) is passed through unchanged.
func RedactColumnsFile(ctx context.Context, srcPath, dstPath string, opts RedactOptions) (_ RedactSummary, retErr error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if opts.Replacement == "" {
		opts.Replacement = "REDACTED"
	}
	if err := ValidateRedactOptions(opts); err != nil {
		return RedactSummary{}, err
	}
	// Own the operation key for the full stream so a direct caller cannot
	// accidentally change pseudonyms mid-output by reusing its input buffer.
	opts.PseudonymKey = append([]byte(nil), opts.PseudonymKey...)
	in, err := openCSVSource(ctx, srcPath, opts.ExpectedSource)
	if err != nil {
		return RedactSummary{}, err
	}
	defer func() { retErr = errors.Join(retErr, in.Close()) }()

	br := bufio.NewReader(in)
	if err := skipInputBOM(br); err != nil {
		return RedactSummary{}, err
	}
	// Redaction must not guess through malformed or shifted records. The first
	// record establishes the exact width; later ragged rows and malformed quote
	// syntax fail and the incomplete output is removed.
	reader, err := newBoundedCSVReader(ctx, br, csvReaderConfig{
		Delimiter: opts.Delimiter, MaxRecordBytes: opts.MaxRecordBytes,
		FieldsPerRecord: 0, LazyQuotes: false,
	})
	if err != nil {
		return RedactSummary{}, err
	}
	firstRecord, err := reader.Read()
	if errors.Is(err, io.EOF) {
		return RedactSummary{}, errors.New("cannot redact an empty CSV")
	}
	if err != nil {
		return RedactSummary{}, err
	}
	if err := validateRedactColumnRange(opts.Columns, len(firstRecord)); err != nil {
		return RedactSummary{}, err
	}

	// The first record establishes the available columns. Only after it and all
	// caller-controlled options have been validated may the destination exist.
	out, err := fileio.OpenAtomicOutput(dstPath, []string{srcPath}, 0o600)
	if err != nil {
		return RedactSummary{}, err
	}
	defer func() { retErr = errors.Join(retErr, out.Cleanup()) }()

	bw := bufio.NewWriter(out)
	writer := stdcsv.NewWriter(bw)
	writer.Comma = opts.Delimiter

	summary := RedactSummary{Delimiter: opts.Delimiter}
	first := true
	record := firstRecord
	for {
		if err := ctx.Err(); err != nil {
			return summary, err
		}
		if record == nil {
			var err error
			record, err = reader.Read()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return summary, err
			}
		}
		summary.RecordsRead++
		if first && opts.HasHeader {
			first = false
			if err := writer.Write(record); err != nil {
				return summary, err
			}
			summary.RecordsWritten++
			record = nil
			continue
		}
		first = false
		for i := range record {
			if mode, ok := opts.Columns[i]; ok {
				masked, err := maskValue(record[i], mode, opts.Replacement, opts.PseudonymKey)
				if err != nil {
					return summary, err
				}
				if masked != record[i] {
					summary.CellsMasked++
				}
				record[i] = masked
			}
		}
		if err := writer.Write(record); err != nil {
			return summary, err
		}
		summary.RecordsWritten++
		if opts.Progress != nil && summary.RecordsRead%5000 == 0 {
			opts.Progress(summary.RecordsRead, 0)
		}
		record = nil
	}
	writer.Flush()
	if err := writer.Error(); err != nil {
		return summary, err
	}
	if err := bw.Flush(); err != nil {
		return summary, err
	}
	if err := out.CommitContextValidated(ctx, in.ValidateContext); err != nil {
		return summary, err
	}
	return summary, nil
}
