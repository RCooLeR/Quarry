package csv

import (
	"bufio"
	"context"
	stdcsv "encoding/csv"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/quarry/quarry-wails3/internal/fileio"
)

// TransformSummary is the result of a streaming row transform.
type TransformSummary struct {
	RecordsRead    int64
	RecordsWritten int64
	Delimiter      rune
}

type rowTransform struct {
	reader *stdcsv.Reader
	writer *stdcsv.Writer
	bw     *bufio.Writer
	in     *sourceHandle
	out    *fileio.AtomicOutput
}

// openRowTransform writes through an operation-owned atomic output. No final
// pathname is visible until the complete transform is synced and published.
func openRowTransform(ctx context.Context, srcPath, dstPath string, delim rune, maxRecordBytes int64, expected *SourceExpectation) (*rowTransform, error) {
	if err := ValidateDelimiter(delim); err != nil {
		return nil, err
	}
	if _, err := normalizeLogicalRecordLimit(maxRecordBytes); err != nil {
		return nil, err
	}
	in, err := openCSVSource(ctx, srcPath, expected)
	if err != nil {
		return nil, err
	}
	out, err := fileio.OpenAtomicOutput(dstPath, []string{srcPath}, 0o600)
	if err != nil {
		return nil, errors.Join(err, in.Close())
	}
	br := bufio.NewReader(in)
	if err := skipInputBOM(br); err != nil {
		return nil, errors.Join(err, in.Close(), out.Cleanup())
	}
	reader, err := newBoundedCSVReader(ctx, br, csvReaderConfig{
		Delimiter: delim, MaxRecordBytes: maxRecordBytes,
		FieldsPerRecord: -1, LazyQuotes: false,
	})
	if err != nil {
		return nil, errors.Join(err, in.Close(), out.Cleanup())
	}
	bw := bufio.NewWriter(out)
	writer := stdcsv.NewWriter(bw)
	writer.Comma = delim
	return &rowTransform{reader: reader, writer: writer, bw: bw, in: in, out: out}, nil
}

func (t *rowTransform) finish(ctx context.Context) error {
	t.writer.Flush()
	if err := t.writer.Error(); err != nil {
		return err
	}
	if err := t.bw.Flush(); err != nil {
		return err
	}
	return t.out.CommitContextValidated(ctx, t.in.ValidateContext)
}

func (t *rowTransform) close() error {
	return errors.Join(t.in.Close(), t.out.Cleanup())
}

type FilterOp string

const (
	FilterEqual    FilterOp = "eq"
	FilterNotEqual FilterOp = "ne"
	FilterContains FilterOp = "contains"
	FilterGreater  FilterOp = "gt"
	FilterLess     FilterOp = "lt"
	FilterEmpty    FilterOp = "empty"
	FilterNonEmpty FilterOp = "nonempty"
)

// FilterOptions selects rows where column[Column] matches Op/Value.
type FilterOptions struct {
	Delimiter      rune
	HasHeader      bool
	Column         int
	Op             FilterOp
	Value          string
	Negate         bool
	MaxRecordBytes int64
	ExpectedSource *SourceExpectation
	Progress       func(records int64)
}

// reportEvery throttles a progress callback to multiples of n records.
func reportEvery(p func(records int64), records, n int64) {
	if p != nil && records%n == 0 {
		p(records)
	}
}

func ValidateFilterOptions(opts FilterOptions) error {
	configStringBytes := 0
	if err := addTransformConfigString(&configStringBytes, "CSV filter", string(opts.Op)); err != nil {
		return err
	}
	if err := addTransformConfigString(&configStringBytes, "CSV filter", opts.Value); err != nil {
		return err
	}
	if err := ValidateDelimiter(opts.Delimiter); err != nil {
		return err
	}
	if opts.Column < 0 {
		return fmt.Errorf("filter column %d is negative", opts.Column)
	}
	if _, err := normalizeLogicalRecordLimit(opts.MaxRecordBytes); err != nil {
		return err
	}
	switch opts.Op {
	case FilterEqual, FilterNotEqual, FilterContains, FilterGreater, FilterLess, FilterEmpty, FilterNonEmpty:
		return nil
	default:
		return fmt.Errorf("invalid filter operation %q", opts.Op)
	}
}

func rowMatches(cell string, op FilterOp, value string) bool {
	switch op {
	case FilterEqual:
		return cell == value
	case FilterNotEqual:
		return cell != value
	case FilterContains:
		return strings.Contains(cell, value)
	case FilterEmpty:
		return strings.TrimSpace(cell) == ""
	case FilterNonEmpty:
		return strings.TrimSpace(cell) != ""
	case FilterGreater, FilterLess:
		cn, e1 := strconv.ParseFloat(strings.TrimSpace(cell), 64)
		vn, e2 := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if e1 == nil && e2 == nil {
			if op == FilterGreater {
				return cn > vn
			}
			return cn < vn
		}
		if op == FilterGreater {
			return cell > value
		}
		return cell < value
	default:
		return false
	}
}

// FilterRowsFile streams src to dst keeping only matching rows (header preserved).
func FilterRowsFile(ctx context.Context, srcPath, dstPath string, opts FilterOptions) (_ TransformSummary, retErr error) {
	if err := ValidateFilterOptions(opts); err != nil {
		return TransformSummary{}, err
	}
	t, err := openRowTransform(ctx, srcPath, dstPath, opts.Delimiter, opts.MaxRecordBytes, opts.ExpectedSource)
	if err != nil {
		return TransformSummary{}, err
	}
	defer func() { retErr = errors.Join(retErr, t.close()) }()
	sum := TransformSummary{Delimiter: opts.Delimiter}
	first := true
	for {
		if err := contextErr(ctx); err != nil {
			return sum, err
		}
		rec, err := t.reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return sum, err
		}
		sum.RecordsRead++
		reportEvery(opts.Progress, sum.RecordsRead, 50000)
		if first && opts.HasHeader {
			first = false
			if err := t.writer.Write(rec); err != nil {
				return sum, err
			}
			sum.RecordsWritten++
			continue
		}
		first = false
		cell := ""
		if opts.Column >= 0 && opts.Column < len(rec) {
			cell = rec[opts.Column]
		}
		keep := rowMatches(cell, opts.Op, opts.Value)
		if opts.Negate {
			keep = !keep
		}
		if keep {
			if err := t.writer.Write(rec); err != nil {
				return sum, err
			}
			sum.RecordsWritten++
		}
	}
	if err := t.finish(ctx); err != nil {
		return sum, err
	}
	return sum, nil
}

// DedupeOptions drops duplicate rows, by the whole row or by a key column.
type DedupeOptions struct {
	Delimiter       rune
	HasHeader       bool
	KeyColumn       int // <0 = dedupe by whole row
	MaxRecordBytes  int64
	MaxDistinctKeys int   // 0 uses DefaultDedupeMaxDistinctKeys
	MaxMemoryBytes  int64 // retained exact-set budget; 0 uses DefaultDedupeMaxMemoryBytes
	ExpectedSource  *SourceExpectation
	Progress        func(records int64)
}

// DedupeRowsFile keeps the first occurrence of each row/key. It is exact within
// validated in-memory key/cardinality budgets and fails without publishing the
// destination when either budget would be exceeded.
func DedupeRowsFile(ctx context.Context, srcPath, dstPath string, opts DedupeOptions) (_ TransformSummary, retErr error) {
	normalized, err := normalizeDedupeOptions(opts)
	if err != nil {
		return TransformSummary{}, err
	}
	opts = normalized
	seen, err := newDedupeSet(opts.MaxDistinctKeys, opts.MaxMemoryBytes)
	if err != nil {
		return TransformSummary{}, err
	}
	t, err := openRowTransform(ctx, srcPath, dstPath, opts.Delimiter, opts.MaxRecordBytes, opts.ExpectedSource)
	if err != nil {
		return TransformSummary{}, err
	}
	defer func() { retErr = errors.Join(retErr, t.close()) }()
	sum := TransformSummary{Delimiter: opts.Delimiter}
	first := true
	for {
		if err := contextErr(ctx); err != nil {
			return sum, err
		}
		rec, err := t.reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return sum, err
		}
		sum.RecordsRead++
		reportEvery(opts.Progress, sum.RecordsRead, 50000)
		if err := contextErr(ctx); err != nil {
			return sum, err
		}
		if first && opts.HasHeader {
			first = false
			if err := t.writer.Write(rec); err != nil {
				return sum, err
			}
			sum.RecordsWritten++
			continue
		}
		first = false
		key, err := dedupeKeyContext(ctx, rec, opts.KeyColumn)
		if err != nil {
			return sum, err
		}
		if err := contextErr(ctx); err != nil {
			return sum, err
		}
		added, err := seen.add(key, sum.RecordsRead)
		if err != nil {
			return sum, err
		}
		if !added {
			continue
		}
		if err := contextErr(ctx); err != nil {
			return sum, err
		}
		if err := t.writer.Write(rec); err != nil {
			return sum, err
		}
		sum.RecordsWritten++
	}
	if err := t.finish(ctx); err != nil {
		return sum, err
	}
	return sum, nil
}

// SampleOptions keeps every Nth data row.
type SampleOptions struct {
	Delimiter      rune
	HasHeader      bool
	EveryN         int
	MaxRecordBytes int64
	ExpectedSource *SourceExpectation
	Progress       func(records int64)
}

// SampleRowsFile keeps every Nth data row (header preserved).
func SampleRowsFile(ctx context.Context, srcPath, dstPath string, opts SampleOptions) (_ TransformSummary, retErr error) {
	if opts.EveryN <= 0 {
		opts.EveryN = 10
	}
	t, err := openRowTransform(ctx, srcPath, dstPath, opts.Delimiter, opts.MaxRecordBytes, opts.ExpectedSource)
	if err != nil {
		return TransformSummary{}, err
	}
	defer func() { retErr = errors.Join(retErr, t.close()) }()
	sum := TransformSummary{Delimiter: opts.Delimiter}
	first := true
	var dataIdx int64
	for {
		if err := contextErr(ctx); err != nil {
			return sum, err
		}
		rec, err := t.reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return sum, err
		}
		sum.RecordsRead++
		reportEvery(opts.Progress, sum.RecordsRead, 50000)
		if first && opts.HasHeader {
			first = false
			if err := t.writer.Write(rec); err != nil {
				return sum, err
			}
			sum.RecordsWritten++
			continue
		}
		first = false
		if dataIdx%int64(opts.EveryN) == 0 {
			if err := t.writer.Write(rec); err != nil {
				return sum, err
			}
			sum.RecordsWritten++
		}
		dataIdx++
	}
	if err := t.finish(ctx); err != nil {
		return sum, err
	}
	return sum, nil
}
