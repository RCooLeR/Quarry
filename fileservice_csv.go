package main

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/wailsapp/wails/v3/pkg/application"

	"github.com/quarry/quarry-wails3/internal/plugins/csv"
)

const csvSampleBytes int64 = 2 << 20 // bounded sample for inspect/schema/preview

// delimiterRune parses a delimiter string ("," ";" "|" or a tab) to a rune.
func delimiterRune(s string) rune {
	switch s {
	case "", "comma":
		return ','
	case "\t", "\\t", "tab":
		return '\t'
	case "semicolon":
		return ';'
	case "pipe":
		return '|'
	case "space":
		return ' '
	}
	r := []rune(s)
	if len(r) == 0 {
		return ','
	}
	return r[0]
}

func delimiterString(r rune) string {
	if r == 0 {
		return ","
	}
	return string(r)
}

func (s *FileService) csvSampleReader(fileID string) (io.Reader, string, error) {
	f, ok := s.reg.Get(fileID)
	if !ok {
		return nil, "", fmt.Errorf("unknown file id %q", fileID)
	}
	n := f.Doc.Size()
	if n > csvSampleBytes {
		n = csvSampleBytes
	}
	return io.NewSectionReader(f.Doc, 0, n), f.Path, nil
}

// ---- inspect (delimiter detection) -------------------------------------

type CsvDelimiterOption struct {
	Delimiter string  `json:"delimiter"`
	Name      string  `json:"name"`
	Columns   int     `json:"columns"`
	Score     float64 `json:"score"`
}

type CsvInspectResult struct {
	Delimiter     string               `json:"delimiter"`
	DelimiterName string               `json:"delimiterName"`
	Confidence    string               `json:"confidence"`
	Columns       int                  `json:"columns"`
	HasHeader     bool                 `json:"hasHeader"`
	Candidates    []CsvDelimiterOption `json:"candidates"`
	Warnings      []string             `json:"warnings"`
}

// CsvInspect detects the most likely delimiter (and ranked alternatives) from a
// bounded sample, so the UI can preselect it and offer overrides.
func (s *FileService) CsvInspect(fileID string) (CsvInspectResult, error) {
	r, _, err := s.csvSampleReader(fileID)
	if err != nil {
		return CsvInspectResult{}, err
	}
	rep, err := csv.InspectReaderContext(context.Background(), r, csv.InspectOptions{MaxBytes: csvSampleBytes, MaxRows: 1000})
	if err != nil {
		return CsvInspectResult{}, err
	}
	cands := make([]CsvDelimiterOption, 0, len(rep.Candidates))
	for _, c := range rep.Candidates {
		cands = append(cands, CsvDelimiterOption{
			Delimiter: delimiterString(c.Delimiter),
			Name:      c.Name,
			Columns:   c.Columns,
			Score:     c.Score,
		})
	}
	return CsvInspectResult{
		Delimiter:     delimiterString(rep.Delimiter),
		DelimiterName: rep.DelimiterName,
		Confidence:    rep.Confidence,
		Columns:       rep.Columns,
		HasHeader:     rep.HasHeader,
		Candidates:    cands,
		Warnings:      rep.Warnings,
	}, nil
}

// ---- schema / preview ---------------------------------------------------

type CsvColumn struct {
	Name    string   `json:"name"`
	SQLType string   `json:"sqlType"`
	NonNull int      `json:"nonNull"`
	Null    int      `json:"null"`
	Samples []string `json:"samples"`
}

type CsvSchemaResult struct {
	Columns  []CsvColumn `json:"columns"`
	HasHeader bool       `json:"hasHeader"`
	Warnings []string    `json:"warnings"`
}

func (s *FileService) CsvSchema(fileID string, delimiter string, hasHeader bool) (CsvSchemaResult, error) {
	r, _, err := s.csvSampleReader(fileID)
	if err != nil {
		return CsvSchemaResult{}, err
	}
	rep, err := csv.InferSchemaContext(context.Background(), r, csv.SchemaOptions{
		Delimiter: delimiterRune(delimiter),
		HasHeader: hasHeader,
		MaxBytes:  csvSampleBytes,
		MaxRows:   2000,
	})
	if err != nil {
		return CsvSchemaResult{}, err
	}
	cols := make([]CsvColumn, 0, len(rep.Columns))
	for _, c := range rep.Columns {
		cols = append(cols, CsvColumn{Name: c.Name, SQLType: c.SQLType, NonNull: c.NonNullCount, Null: c.NullCount, Samples: c.SampleValues})
	}
	return CsvSchemaResult{Columns: cols, HasHeader: rep.HasHeader, Warnings: rep.Warnings}, nil
}

type CsvPreviewResult struct {
	Header   []string   `json:"header"`
	Rows     [][]string `json:"rows"`
	Warnings []string   `json:"warnings"`
}

func (s *FileService) CsvPreview(fileID string, delimiter string, hasHeader bool, maxRows int) (CsvPreviewResult, error) {
	r, _, err := s.csvSampleReader(fileID)
	if err != nil {
		return CsvPreviewResult{}, err
	}
	if maxRows <= 0 {
		maxRows = 50
	}
	rep, err := csv.PreviewRowsContext(context.Background(), r, csv.PreviewOptions{
		Delimiter: delimiterRune(delimiter),
		HasHeader: hasHeader,
		MaxBytes:  csvSampleBytes,
		MaxRows:   maxRows,
	})
	if err != nil {
		return CsvPreviewResult{}, err
	}
	return CsvPreviewResult{Header: rep.Header, Rows: rep.Rows, Warnings: rep.Warnings}, nil
}

// ---- transforms (stream to a chosen output file) ------------------------

type TransformResult struct {
	OutputPath     string `json:"outputPath"`
	RecordsRead    int64  `json:"recordsRead"`
	RecordsWritten int64  `json:"recordsWritten"`
	Note           string `json:"note"`
}

func saveDialog(message, defaultName string) (string, error) {
	d := application.Get().Dialog.SaveFile().SetMessage(message)
	if defaultName != "" {
		d = d.SetFilename(defaultName)
	}
	return d.PromptForSingleSelection()
}

// CsvProjectViaDialog writes a new CSV keeping only keepIndices (0-based) in the
// given order — used for drop-column and reorder.
func (s *FileService) CsvProjectViaDialog(fileID string, delimiter string, keepIndices []int) (TransformResult, error) {
	f, ok := s.reg.Get(fileID)
	if !ok {
		return TransformResult{}, fmt.Errorf("unknown file id %q", fileID)
	}
	dst, err := saveDialog("Save projected CSV as", "projected.csv")
	if err != nil || strings.TrimSpace(dst) == "" {
		return TransformResult{}, err
	}
	sum, err := csv.ProjectColumnsFile(context.Background(), f.Path, dst, csv.ProjectOptions{
		Delimiter: delimiterRune(delimiter),
		Columns:   keepIndices,
	})
	if err != nil {
		return TransformResult{}, err
	}
	return TransformResult{OutputPath: dst, RecordsRead: sum.RecordsRead, RecordsWritten: sum.RecordsWritten, Note: fmt.Sprintf("%d columns kept", sum.ColumnsWritten)}, nil
}

// CsvAddColumnViaDialog writes a new CSV with a constant column appended to every
// row (including the header, which becomes the value).
func (s *FileService) CsvAddColumnViaDialog(fileID string, delimiter string, columnCount int, value string) (TransformResult, error) {
	f, ok := s.reg.Get(fileID)
	if !ok {
		return TransformResult{}, fmt.Errorf("unknown file id %q", fileID)
	}
	dst, err := saveDialog("Save CSV with added column as", "with-column.csv")
	if err != nil || strings.TrimSpace(dst) == "" {
		return TransformResult{}, err
	}
	keep := make([]int, 0, columnCount+1)
	for i := 0; i < columnCount; i++ {
		keep = append(keep, i)
	}
	keep = append(keep, columnCount) // out-of-range index -> filled with MissingValue
	sum, err := csv.ProjectColumnsFile(context.Background(), f.Path, dst, csv.ProjectOptions{
		Delimiter:         delimiterRune(delimiter),
		Columns:           keep,
		AllowShortRecords: true,
		MissingValue:      value,
	})
	if err != nil {
		return TransformResult{}, err
	}
	return TransformResult{OutputPath: dst, RecordsRead: sum.RecordsRead, RecordsWritten: sum.RecordsWritten, Note: "constant column added"}, nil
}

// CsvToSQLPreview returns a short sample of the generated SQL.
func (s *FileService) CsvToSQLPreview(fileID, delimiter, tableName string, hasHeader, includeCreate bool) (string, error) {
	r, _, err := s.csvSampleReader(fileID)
	if err != nil {
		return "", err
	}
	rep, err := csv.PreviewSQLConversionContext(context.Background(), r, csv.SQLPreviewOptions{
		SQLConvertOptions: csv.SQLConvertOptions{
			Delimiter:          delimiterRune(delimiter),
			TableName:          sqlTableName(tableName),
			HasHeader:          hasHeader,
			IncludeCreateTable: includeCreate,
		},
		MaxBytes: csvSampleBytes,
		MaxRows:  20,
	})
	if err != nil {
		return "", err
	}
	return rep.SQL, nil
}

// CsvToSQLViaDialog streams the whole CSV to a .sql file of INSERTs.
func (s *FileService) CsvToSQLViaDialog(fileID, delimiter, tableName string, hasHeader, includeCreate bool) (TransformResult, error) {
	f, ok := s.reg.Get(fileID)
	if !ok {
		return TransformResult{}, fmt.Errorf("unknown file id %q", fileID)
	}
	dst, err := saveDialog("Save SQL as", sqlTableName(tableName)+".sql")
	if err != nil || strings.TrimSpace(dst) == "" {
		return TransformResult{}, err
	}
	sum, err := csv.ConvertToSQLFile(context.Background(), f.Path, dst, csv.SQLConvertOptions{
		Delimiter:          delimiterRune(delimiter),
		TableName:          sqlTableName(tableName),
		HasHeader:          hasHeader,
		IncludeCreateTable: includeCreate,
	})
	if err != nil {
		return TransformResult{}, err
	}
	return TransformResult{OutputPath: dst, RecordsRead: sum.RecordsRead, RecordsWritten: sum.RowsWritten, Note: fmt.Sprintf("table %q", sum.TableName)}, nil
}

func sqlTableName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "imported"
	}
	return name
}
