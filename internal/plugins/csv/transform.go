package csv

import (
	"bufio"
	"context"
	stdcsv "encoding/csv"
	"errors"
	"hash/fnv"
	"io"
	"os"
	"strconv"
	"strings"
)

// TransformSummary is the result of a streaming row transform.
type TransformSummary struct {
	RecordsRead    int64
	RecordsWritten int64
	Delimiter      rune
}

type rowTransform struct {
	reader  *stdcsv.Reader
	writer  *stdcsv.Writer
	bw      *bufio.Writer
	in      *os.File
	out     *os.File
	cleanup bool
	dstPath string
}

func openRowTransform(srcPath, dstPath string, delim rune) (*rowTransform, error) {
	if delim == 0 {
		delim = ','
	}
	if same, err := sameFilePath(srcPath, dstPath); err != nil {
		return nil, err
	} else if same {
		return nil, errors.New("output path must be different from input path")
	}
	in, err := os.Open(srcPath)
	if err != nil {
		return nil, err
	}
	out, err := os.OpenFile(dstPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666)
	if err != nil {
		_ = in.Close()
		return nil, err
	}
	br := bufio.NewReader(in)
	if err := skipInputBOM(br); err != nil {
		_ = in.Close()
		_ = out.Close()
		_ = os.Remove(dstPath)
		return nil, err
	}
	reader := stdcsv.NewReader(br)
	reader.Comma = delim
	reader.FieldsPerRecord = -1
	reader.LazyQuotes = true
	reader.ReuseRecord = false
	bw := bufio.NewWriter(out)
	writer := stdcsv.NewWriter(bw)
	writer.Comma = delim
	return &rowTransform{reader: reader, writer: writer, bw: bw, in: in, out: out, cleanup: true, dstPath: dstPath}, nil
}

func (t *rowTransform) finish() error {
	t.writer.Flush()
	if err := t.writer.Error(); err != nil {
		return err
	}
	if err := t.bw.Flush(); err != nil {
		return err
	}
	if err := t.out.Sync(); err != nil {
		return err
	}
	if err := t.out.Close(); err != nil {
		return err
	}
	t.cleanup = false
	return nil
}

func (t *rowTransform) close() {
	_ = t.in.Close()
	if t.cleanup {
		_ = t.out.Close()
		_ = os.Remove(t.dstPath)
	}
}

// FilterOptions selects rows where column[Column] matches Op/Value.
type FilterOptions struct {
	Delimiter rune
	HasHeader bool
	Column    int
	Op        string // eq | ne | contains | gt | lt | empty | nonempty
	Value     string
	Negate    bool
	Progress  func(records int64)
}

// reportEvery throttles a progress callback to multiples of n records.
func reportEvery(p func(records int64), records, n int64) {
	if p != nil && records%n == 0 {
		p(records)
	}
}

func rowMatches(cell, op, value string) bool {
	switch op {
	case "eq":
		return cell == value
	case "ne":
		return cell != value
	case "contains":
		return strings.Contains(cell, value)
	case "empty":
		return strings.TrimSpace(cell) == ""
	case "nonempty":
		return strings.TrimSpace(cell) != ""
	case "gt", "lt":
		cn, e1 := strconv.ParseFloat(strings.TrimSpace(cell), 64)
		vn, e2 := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if e1 == nil && e2 == nil {
			if op == "gt" {
				return cn > vn
			}
			return cn < vn
		}
		if op == "gt" {
			return cell > value
		}
		return cell < value
	default:
		return false
	}
}

// FilterRowsFile streams src to dst keeping only matching rows (header preserved).
func FilterRowsFile(ctx context.Context, srcPath, dstPath string, opts FilterOptions) (TransformSummary, error) {
	t, err := openRowTransform(srcPath, dstPath, opts.Delimiter)
	if err != nil {
		return TransformSummary{}, err
	}
	defer t.close()
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
	if err := t.finish(); err != nil {
		return sum, err
	}
	return sum, nil
}

// DedupeOptions drops duplicate rows, by the whole row or by a key column.
type DedupeOptions struct {
	Delimiter rune
	HasHeader bool
	KeyColumn int // <0 = dedupe by whole row
	Progress  func(records int64)
}

// DedupeRowsFile keeps the first occurrence of each row/key. Remembers seen keys
// in memory (a hash set) — memory grows with the number of distinct rows.
func DedupeRowsFile(ctx context.Context, srcPath, dstPath string, opts DedupeOptions) (TransformSummary, error) {
	t, err := openRowTransform(srcPath, dstPath, opts.Delimiter)
	if err != nil {
		return TransformSummary{}, err
	}
	defer t.close()
	sum := TransformSummary{Delimiter: opts.Delimiter}
	seen := map[uint64]struct{}{}
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
		h := fnv.New64a()
		if opts.KeyColumn >= 0 {
			if opts.KeyColumn < len(rec) {
				_, _ = h.Write([]byte(rec[opts.KeyColumn]))
			}
		} else {
			for _, c := range rec {
				_, _ = h.Write([]byte(c))
				_, _ = h.Write([]byte{0})
			}
		}
		k := h.Sum64()
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		if err := t.writer.Write(rec); err != nil {
			return sum, err
		}
		sum.RecordsWritten++
	}
	if err := t.finish(); err != nil {
		return sum, err
	}
	return sum, nil
}

// SampleOptions keeps every Nth data row.
type SampleOptions struct {
	Delimiter rune
	HasHeader bool
	EveryN    int
	Progress  func(records int64)
}

// SampleRowsFile keeps every Nth data row (header preserved).
func SampleRowsFile(ctx context.Context, srcPath, dstPath string, opts SampleOptions) (TransformSummary, error) {
	if opts.EveryN <= 0 {
		opts.EveryN = 10
	}
	t, err := openRowTransform(srcPath, dstPath, opts.Delimiter)
	if err != nil {
		return TransformSummary{}, err
	}
	defer t.close()
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
	if err := t.finish(); err != nil {
		return sum, err
	}
	return sum, nil
}
