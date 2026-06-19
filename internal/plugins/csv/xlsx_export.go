package csv

import (
	"bufio"
	"context"
	stdcsv "encoding/csv"
	"errors"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/xuri/excelize/v2"
)

// xlsxMaxRows is the hard per-sheet row limit of the .xlsx format.
const xlsxMaxRows = 1048576

// XLSXOptions configures a CSV → Excel (.xlsx) export.
type XLSXOptions struct {
	Delimiter  rune
	HasHeader  bool
	SheetName  string
	TypedCells bool // write numeric cells as numbers, not text
}

// XLSXSummary reports an .xlsx export, including whether the row cap truncated.
type XLSXSummary struct {
	RecordsRead    int64
	RecordsWritten int64
	Truncated      bool
}

// ExportXLSXFile streams a CSV into a new .xlsx using excelize's StreamWriter,
// so it stays memory-bounded. The .xlsx format caps a sheet at ~1,048,576 rows;
// beyond that the export stops and reports Truncated.
func ExportXLSXFile(ctx context.Context, srcPath, dstPath string, opts XLSXOptions) (XLSXSummary, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if opts.Delimiter == 0 {
		opts.Delimiter = ','
	}
	sheet := strings.TrimSpace(opts.SheetName)
	if sheet == "" {
		sheet = "Sheet1"
	}
	if same, err := sameFilePath(srcPath, dstPath); err != nil {
		return XLSXSummary{}, err
	} else if same {
		return XLSXSummary{}, errors.New("output path must be different from input path")
	}
	if _, err := os.Stat(dstPath); err == nil {
		return XLSXSummary{}, errors.New("output file already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return XLSXSummary{}, err
	}

	in, err := os.Open(srcPath)
	if err != nil {
		return XLSXSummary{}, err
	}
	defer in.Close()
	br := bufio.NewReader(in)
	if err := skipInputBOM(br); err != nil {
		return XLSXSummary{}, err
	}
	reader := stdcsv.NewReader(br)
	reader.Comma = opts.Delimiter
	reader.FieldsPerRecord = -1
	reader.LazyQuotes = true
	reader.ReuseRecord = false

	fx := excelize.NewFile()
	defer fx.Close()
	if sheet != "Sheet1" {
		if err := fx.SetSheetName("Sheet1", sheet); err != nil {
			return XLSXSummary{}, err
		}
	}
	sw, err := fx.NewStreamWriter(sheet)
	if err != nil {
		return XLSXSummary{}, err
	}

	var sum XLSXSummary
	rowNum := 1
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
		if rowNum > xlsxMaxRows {
			sum.Truncated = true
			break
		}
		cell, err := excelize.CoordinatesToCellName(1, rowNum)
		if err != nil {
			return sum, err
		}
		row := make([]any, len(rec))
		for i, v := range rec {
			if opts.TypedCells {
				row[i] = typedCellXLSX(v)
			} else {
				row[i] = v
			}
		}
		if err := sw.SetRow(cell, row); err != nil {
			return sum, err
		}
		sum.RecordsWritten++
		rowNum++
	}
	if err := sw.Flush(); err != nil {
		return sum, err
	}
	if err := fx.SaveAs(dstPath); err != nil {
		return sum, err
	}
	return sum, nil
}

func typedCellXLSX(s string) any {
	t := strings.TrimSpace(s)
	if t == "" {
		return s
	}
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
