package csv

import (
	"bufio"
	"context"
	stdcsv "encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// JSONLOptions configures a CSV → JSON Lines export.
type JSONLOptions struct {
	Delimiter  rune
	HasHeader  bool
	NumberKeys bool // emit numeric-looking cells as JSON numbers, not strings
}

// ExportSummary reports a tabular export result.
type ExportSummary struct {
	RecordsRead    int64
	RecordsWritten int64
}

// ExportJSONLFile streams a CSV to newline-delimited JSON (one object per data
// row). Keys come from the header row, or col1, col2, … when there is none.
func ExportJSONLFile(ctx context.Context, srcPath, dstPath string, opts JSONLOptions) (ExportSummary, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if opts.Delimiter == 0 {
		opts.Delimiter = ','
	}
	if same, err := sameFilePath(srcPath, dstPath); err != nil {
		return ExportSummary{}, err
	} else if same {
		return ExportSummary{}, errors.New("output path must be different from input path")
	}
	in, err := os.Open(srcPath)
	if err != nil {
		return ExportSummary{}, err
	}
	defer in.Close()
	out, err := os.OpenFile(dstPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666)
	if err != nil {
		return ExportSummary{}, err
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = out.Close()
			_ = os.Remove(dstPath)
		}
	}()

	br := bufio.NewReader(in)
	if err := skipInputBOM(br); err != nil {
		return ExportSummary{}, err
	}
	reader := stdcsv.NewReader(br)
	reader.Comma = opts.Delimiter
	reader.FieldsPerRecord = -1
	reader.LazyQuotes = true
	reader.ReuseRecord = false

	bw := bufio.NewWriterSize(out, 1<<20)
	enc := json.NewEncoder(bw)
	enc.SetEscapeHTML(false)

	var sum ExportSummary
	var header []string
	first := true
	for {
		if err := contextErr(ctx); err != nil {
			return sum, err
		}
		rec, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return sum, err
		}
		sum.RecordsRead++
		if first && opts.HasHeader {
			first = false
			header = append([]string(nil), rec...)
			continue
		}
		first = false
		obj := make(map[string]any, len(rec))
		for i, cell := range rec {
			key := columnKey(header, i)
			if opts.NumberKeys {
				obj[key] = maybeNumber(cell)
			} else {
				obj[key] = cell
			}
		}
		if err := enc.Encode(obj); err != nil {
			return sum, err
		}
		sum.RecordsWritten++
	}
	if err := bw.Flush(); err != nil {
		return sum, err
	}
	if err := out.Sync(); err != nil {
		return sum, err
	}
	if err := out.Close(); err != nil {
		return sum, err
	}
	cleanup = false
	return sum, nil
}

func columnKey(header []string, i int) string {
	if i < len(header) && strings.TrimSpace(header[i]) != "" {
		return header[i]
	}
	return fmt.Sprintf("col%d", i+1)
}

// maybeNumber returns a float64/int for numeric-looking cells, else the string.
func maybeNumber(s string) any {
	t := strings.TrimSpace(s)
	if t == "" {
		return s
	}
	// Reject leading-zero integers (e.g. zip codes) so they aren't mangled.
	if len(t) > 1 && t[0] == '0' && t[1] != '.' {
		return s
	}
	if n, err := strconv.ParseInt(t, 10, 64); err == nil {
		return n
	}
	if f, err := strconv.ParseFloat(t, 64); err == nil {
		return f
	}
	return s
}

// MarkdownPreview renders a small set of rows as a GitHub-flavored Markdown
// table. Used for "copy as Markdown" of the current preview (bounded).
func MarkdownPreview(header []string, rows [][]string) string {
	cols := len(header)
	for _, r := range rows {
		if len(r) > cols {
			cols = len(r)
		}
	}
	if cols == 0 {
		return ""
	}
	var b strings.Builder
	cell := func(r []string, i int) string {
		v := ""
		if i < len(r) {
			v = r[i]
		}
		return strings.ReplaceAll(strings.ReplaceAll(v, "|", "\\|"), "\n", " ")
	}
	// header
	b.WriteByte('|')
	for i := 0; i < cols; i++ {
		h := fmt.Sprintf("col%d", i+1)
		if i < len(header) && strings.TrimSpace(header[i]) != "" {
			h = header[i]
		}
		b.WriteByte(' ')
		b.WriteString(strings.ReplaceAll(h, "|", "\\|"))
		b.WriteString(" |")
	}
	b.WriteByte('\n')
	// separator
	b.WriteByte('|')
	for i := 0; i < cols; i++ {
		b.WriteString(" --- |")
	}
	b.WriteByte('\n')
	// rows
	for _, r := range rows {
		b.WriteByte('|')
		for i := 0; i < cols; i++ {
			b.WriteByte(' ')
			b.WriteString(cell(r, i))
			b.WriteString(" |")
		}
		b.WriteByte('\n')
	}
	return b.String()
}
