package csv

import (
	"bufio"
	"bytes"
	"context"
	stdcsv "encoding/csv"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"
)

const DefaultSQLInsertBatchSize = 500
const DefaultSQLMaxFieldBytes = 16 * 1024 * 1024

type SQLDialect string

const SQLDialectMySQL SQLDialect = "mysql"

type SQLConvertOptions struct {
	Delimiter          rune
	TableName          string
	Dialect            SQLDialect
	Columns            []string
	HasHeader          bool
	NullValues         []string
	InsertBatchSize    int
	IncludeCreateTable bool
	ColumnTypes        []string
	MaxFieldBytes      int64
	Progress           func(SQLConvertProgress)
}

type SQLConvertProgress struct {
	RecordsRead int64
	RowsWritten int64
	BytesRead   int64
}

type SQLConvertSummary struct {
	RecordsRead        int64
	RowsWritten        int64
	BytesRead          int64
	Columns            int
	TableName          string
	Dialect            SQLDialect
	Delimiter          rune
	CreateTableWritten bool
}

type SQLPreviewOptions struct {
	SQLConvertOptions
	MaxBytes int64
	MaxRows  int
}

type SQLPreviewReport struct {
	SQL             string
	Summary         SQLConvertSummary
	BytesScanned    int64
	RowsPreviewed   int
	TruncatedSample bool
	Warnings        []string
}

func ConvertToSQL(ctx context.Context, r io.Reader, w io.Writer, opts SQLConvertOptions) (SQLConvertSummary, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if r == nil {
		return SQLConvertSummary{}, errors.New("reader is required")
	}
	if w == nil {
		return SQLConvertSummary{}, errors.New("writer is required")
	}
	opts, err := normalizeSQLConvertOptions(opts)
	if err != nil {
		return SQLConvertSummary{}, err
	}

	counting := &countingReader{r: bufio.NewReader(r)}
	reader := stdcsv.NewReader(counting)
	reader.Comma = opts.Delimiter
	reader.FieldsPerRecord = -1
	reader.LazyQuotes = true
	reader.ReuseRecord = false

	writer := bufio.NewWriter(w)
	summary := SQLConvertSummary{
		TableName: opts.TableName,
		Dialect:   opts.Dialect,
		Delimiter: opts.Delimiter,
	}

	columns := append([]string(nil), opts.Columns...)
	if opts.HasHeader {
		header, err := reader.Read()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return summary, errors.New("header row is required")
			}
			return summary, err
		}
		summary.RecordsRead++
		summary.BytesRead = counting.n
		if err := validateSQLRecordFieldSizes(header, opts.MaxFieldBytes); err != nil {
			return summary, fmt.Errorf("header row: %w", err)
		}
		if len(columns) == 0 {
			columns = normalizeSQLColumnNames(header)
		}
	}
	if len(columns) == 0 {
		return summary, errors.New("columns are required when input has no header")
	}
	if err := validateSQLIdentifiers("column", columns); err != nil {
		summary.BytesRead = counting.n
		return summary, err
	}
	summary.Columns = len(columns)
	if opts.IncludeCreateTable {
		columnTypes := normalizeSQLColumnTypes(opts.ColumnTypes, len(columns))
		if err := writeSQLCreateTable(writer, opts.TableName, columns, columnTypes); err != nil {
			summary.BytesRead = counting.n
			return summary, err
		}
		summary.CreateTableWritten = true
	}

	nulls := makeSQLNullSet(opts.NullValues)
	batch := make([]string, 0, opts.InsertBatchSize)
	for {
		if err := ctx.Err(); err != nil {
			summary.BytesRead = counting.n
			return summary, err
		}

		record, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			summary.BytesRead = counting.n
			return summary, err
		}
		summary.RecordsRead++
		if len(record) != len(columns) {
			summary.BytesRead = counting.n
			return summary, fmt.Errorf("record %d has %d fields, expected %d", summary.RecordsRead, len(record), len(columns))
		}
		if err := validateSQLRecordFieldSizes(record, opts.MaxFieldBytes); err != nil {
			summary.BytesRead = counting.n
			return summary, fmt.Errorf("record %d: %w", summary.RecordsRead, err)
		}

		tuple, err := sqlValuesTuple(record, nulls)
		if err != nil {
			summary.BytesRead = counting.n
			return summary, fmt.Errorf("record %d: %w", summary.RecordsRead, err)
		}
		batch = append(batch, tuple)
		if len(batch) >= opts.InsertBatchSize {
			if err := writeSQLInsertBatch(writer, opts.TableName, columns, batch); err != nil {
				summary.BytesRead = counting.n
				return summary, err
			}
			summary.RowsWritten += int64(len(batch))
			batch = batch[:0]
			summary.BytesRead = counting.n
			if opts.Progress != nil {
				opts.Progress(SQLConvertProgress{
					RecordsRead: summary.RecordsRead,
					RowsWritten: summary.RowsWritten,
					BytesRead:   summary.BytesRead,
				})
			}
		}
	}
	if len(batch) > 0 {
		if err := writeSQLInsertBatch(writer, opts.TableName, columns, batch); err != nil {
			summary.BytesRead = counting.n
			return summary, err
		}
		summary.RowsWritten += int64(len(batch))
	}
	if err := writer.Flush(); err != nil {
		summary.BytesRead = counting.n
		return summary, err
	}
	summary.BytesRead = counting.n
	if opts.Progress != nil {
		opts.Progress(SQLConvertProgress{
			RecordsRead: summary.RecordsRead,
			RowsWritten: summary.RowsWritten,
			BytesRead:   summary.BytesRead,
		})
	}
	return summary, nil
}

func PreviewSQLConversion(r io.Reader, opts SQLPreviewOptions) (SQLPreviewReport, error) {
	return PreviewSQLConversionContext(context.Background(), r, opts)
}

func PreviewSQLConversionContext(ctx context.Context, r io.Reader, opts SQLPreviewOptions) (SQLPreviewReport, error) {
	if r == nil {
		return SQLPreviewReport{}, errors.New("reader is required")
	}
	convertOpts, maxBytes, maxRows, err := normalizeSQLPreviewOptions(opts)
	if err != nil {
		return SQLPreviewReport{}, err
	}
	data, err := readBoundedSample(ctx, r, maxBytes)
	if err != nil {
		return SQLPreviewReport{}, err
	}
	truncated := int64(len(data)) > maxBytes
	if truncated {
		data = data[:maxBytes]
	}
	bytesScanned := int64(len(data))
	if truncated {
		if trimmed, ok := trimTrailingPartialRecord(data); ok {
			data = trimmed
		}
	}

	report := SQLPreviewReport{
		BytesScanned:    bytesScanned,
		TruncatedSample: truncated,
	}
	if len(bytes.TrimSpace(data)) == 0 {
		report.Warnings = append(report.Warnings, "sample is empty")
		return report, nil
	}
	if truncated {
		report.Warnings = append(report.Warnings, fmt.Sprintf("sample limited to %d bytes", maxBytes))
		report.Warnings = append(report.Warnings, "trailing partial record omitted from preview")
	}

	limitedInput, rowsPreviewed, err := limitedSQLPreviewInput(ctx, data, convertOpts.Delimiter, convertOpts.HasHeader, maxRows)
	if err != nil {
		return report, err
	}
	report.RowsPreviewed = rowsPreviewed
	if rowsPreviewed == 0 {
		report.Warnings = append(report.Warnings, "no data rows found in bounded sample")
	} else if rowsPreviewed == maxRows {
		report.Warnings = append(report.Warnings, fmt.Sprintf("preview limited to %d data rows", maxRows))
	}
	if convertOpts.IncludeCreateTable && len(convertOpts.ColumnTypes) == 0 {
		inferred, err := InferSchemaContext(ctx, bytes.NewReader(data), SchemaOptions{
			Delimiter:  convertOpts.Delimiter,
			HasHeader:  convertOpts.HasHeader,
			NullValues: convertOpts.NullValues,
			MaxBytes:   int64(len(data)),
			MaxRows:    maxRows,
		})
		if err != nil {
			return report, err
		}
		names, types := schemaColumnsForSQL(inferred.Columns)
		if len(convertOpts.Columns) == 0 {
			convertOpts.Columns = names
		}
		convertOpts.ColumnTypes = types
	}

	var out strings.Builder
	summary, err := ConvertToSQL(ctx, strings.NewReader(limitedInput), &out, convertOpts)
	if err != nil {
		return report, err
	}
	report.SQL = out.String()
	report.Summary = summary
	return report, nil
}

func normalizeSQLPreviewOptions(opts SQLPreviewOptions) (SQLConvertOptions, int64, int, error) {
	convertOpts := opts.SQLConvertOptions
	convertOpts.Progress = nil
	normalized, err := normalizeSQLConvertOptions(convertOpts)
	if err != nil {
		return SQLConvertOptions{}, 0, 0, err
	}
	maxBytes := opts.MaxBytes
	if maxBytes <= 0 {
		maxBytes = DefaultPreviewMaxBytes
	}
	maxRows := opts.MaxRows
	if maxRows <= 0 {
		maxRows = DefaultPreviewMaxRows
	}
	return normalized, maxBytes, maxRows, nil
}

func limitedSQLPreviewInput(ctx context.Context, data []byte, delimiter rune, hasHeader bool, maxRows int) (string, int, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	reader := stdcsv.NewReader(bytes.NewReader(data))
	reader.Comma = delimiter
	reader.FieldsPerRecord = -1
	reader.LazyQuotes = true
	reader.ReuseRecord = false

	var out strings.Builder
	writer := stdcsv.NewWriter(&out)
	writer.Comma = delimiter

	if hasHeader {
		header, err := reader.Read()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return "", 0, nil
			}
			return "", 0, err
		}
		if err := writer.Write(header); err != nil {
			return "", 0, err
		}
	}

	rows := 0
	for rows < maxRows {
		if err := ctx.Err(); err != nil {
			return "", rows, err
		}
		record, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", rows, err
		}
		if err := writer.Write(record); err != nil {
			return "", rows, err
		}
		rows++
	}
	writer.Flush()
	if err := writer.Error(); err != nil {
		return "", rows, err
	}
	return out.String(), rows, nil
}

func schemaColumnsForSQL(columns []SchemaColumn) ([]string, []string) {
	names := make([]string, len(columns))
	types := make([]string, len(columns))
	for i, column := range columns {
		names[i] = column.Name
		types[i] = column.SQLType
	}
	return names, types
}

func FormatSQLPreviewReport(report SQLPreviewReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Rows previewed: %d\n", report.RowsPreviewed)
	fmt.Fprintf(&b, "Bytes scanned: %d\n", report.BytesScanned)
	if report.Summary.Columns > 0 {
		fmt.Fprintf(&b, "Columns: %d\n", report.Summary.Columns)
	}
	if report.Summary.CreateTableWritten {
		b.WriteString("CREATE TABLE: included\n")
	}
	if report.TruncatedSample {
		b.WriteString("Sample: truncated to bounded preview window\n")
	}
	if len(report.Warnings) > 0 {
		b.WriteString("\nWarnings:\n")
		for _, warning := range report.Warnings {
			b.WriteString("- ")
			b.WriteString(warning)
			b.WriteByte('\n')
		}
	}
	b.WriteString("\nSQL preview:\n")
	if strings.TrimSpace(report.SQL) == "" {
		b.WriteString("(no SQL generated from the bounded sample)\n")
		return b.String()
	}
	b.WriteString(report.SQL)
	return b.String()
}

func ConvertToSQLFile(ctx context.Context, inputPath, outputPath string, opts SQLConvertOptions) (SQLConvertSummary, error) {
	if inputPath == "" {
		return SQLConvertSummary{}, errors.New("input path is required")
	}
	if outputPath == "" {
		return SQLConvertSummary{}, errors.New("output path is required")
	}
	same, err := sameFilePath(inputPath, outputPath)
	if err != nil {
		return SQLConvertSummary{}, err
	}
	if same {
		return SQLConvertSummary{}, errors.New("output path must be different from input path")
	}

	if opts.IncludeCreateTable && len(opts.ColumnTypes) == 0 {
		inferred, err := inferSQLSchemaForFile(inputPath, opts)
		if err != nil {
			return SQLConvertSummary{}, err
		}
		if len(opts.Columns) == 0 {
			opts.Columns = inferred.columns
		}
		opts.ColumnTypes = inferred.types
	}

	input, err := os.Open(inputPath)
	if err != nil {
		return SQLConvertSummary{}, err
	}
	defer input.Close()

	output, err := os.OpenFile(outputPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666)
	if err != nil {
		return SQLConvertSummary{}, err
	}

	cleanup := true
	defer func() {
		if cleanup {
			_ = output.Close()
			_ = os.Remove(outputPath)
		}
	}()

	summary, err := ConvertToSQL(ctx, input, output, opts)
	if err != nil {
		return summary, err
	}
	if err := output.Sync(); err != nil {
		return summary, err
	}
	if err := output.Close(); err != nil {
		return summary, err
	}
	cleanup = false
	return summary, nil
}

func normalizeSQLConvertOptions(opts SQLConvertOptions) (SQLConvertOptions, error) {
	if opts.Delimiter == 0 {
		opts.Delimiter = ','
	}
	if !validProjectDelimiter(opts.Delimiter) {
		return opts, fmt.Errorf("invalid delimiter %q", opts.Delimiter)
	}
	opts.TableName = strings.TrimSpace(opts.TableName)
	if opts.TableName == "" {
		return opts, errors.New("table name is required")
	}
	if opts.InsertBatchSize <= 0 {
		opts.InsertBatchSize = DefaultSQLInsertBatchSize
	}
	if opts.Dialect == "" {
		opts.Dialect = SQLDialectMySQL
	}
	if opts.Dialect != SQLDialectMySQL {
		return opts, fmt.Errorf("unsupported SQL dialect %q; current CSV conversion emits MySQL-compatible SQL", opts.Dialect)
	}
	if opts.MaxFieldBytes <= 0 {
		opts.MaxFieldBytes = DefaultSQLMaxFieldBytes
	}
	opts.Columns = normalizeSQLColumnNames(opts.Columns)
	if err := validateSQLIdentifier("table name", opts.TableName); err != nil {
		return opts, err
	}
	if err := validateSQLIdentifiers("column", opts.Columns); err != nil {
		return opts, err
	}
	opts.ColumnTypes = normalizeSQLColumnTypes(opts.ColumnTypes, len(opts.Columns))
	return opts, nil
}

func normalizeSQLColumnNames(columns []string) []string {
	normalized := make([]string, len(columns))
	seen := make(map[string]int, len(columns))
	for i, column := range columns {
		column = strings.TrimSpace(column)
		if column == "" {
			column = fmt.Sprintf("column_%d", i+1)
		}
		count := seen[column] + 1
		seen[column] = count
		if count > 1 {
			column = fmt.Sprintf("%s_%d", column, count)
		}
		normalized[i] = column
	}
	return normalized
}

func makeSQLNullSet(values []string) map[string]struct{} {
	nulls := make(map[string]struct{}, len(values))
	for _, value := range values {
		nulls[value] = struct{}{}
	}
	return nulls
}

func normalizeSQLColumnTypes(types []string, columns int) []string {
	if columns <= 0 {
		columns = len(types)
	}
	if columns <= 0 {
		return nil
	}
	normalized := make([]string, columns)
	for i := range normalized {
		if i < len(types) {
			switch strings.ToUpper(strings.TrimSpace(types[i])) {
			case SQLTypeBigInt:
				normalized[i] = SQLTypeBigInt
			case SQLTypeDouble:
				normalized[i] = SQLTypeDouble
			case SQLTypeBoolean:
				normalized[i] = SQLTypeBoolean
			case SQLTypeText:
				normalized[i] = SQLTypeText
			default:
				normalized[i] = SQLTypeText
			}
			continue
		}
		normalized[i] = SQLTypeText
	}
	return normalized
}

func sqlValuesTuple(record []string, nulls map[string]struct{}) (string, error) {
	values := make([]string, len(record))
	for i, value := range record {
		if _, ok := nulls[value]; ok {
			values[i] = "NULL"
			continue
		}
		quoted, err := quoteSQLString(value)
		if err != nil {
			return "", fmt.Errorf("field %d: %w", i+1, err)
		}
		values[i] = quoted
	}
	return "(" + strings.Join(values, ", ") + ")", nil
}

func validateSQLRecordFieldSizes(record []string, maxFieldBytes int64) error {
	if maxFieldBytes <= 0 {
		return nil
	}
	for i, value := range record {
		if int64(len(value)) > maxFieldBytes {
			return fmt.Errorf("field %d is %d bytes, exceeding max field size %d", i+1, len(value), maxFieldBytes)
		}
	}
	return nil
}

func writeSQLInsertBatch(w io.Writer, table string, columns []string, tuples []string) error {
	if len(tuples) == 0 {
		return nil
	}
	quotedColumns := make([]string, len(columns))
	for i, column := range columns {
		quoted, err := quoteSQLIdentifier(column)
		if err != nil {
			return err
		}
		quotedColumns[i] = quoted
	}
	quotedTable, err := quoteSQLIdentifier(table)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "INSERT INTO %s (%s) VALUES\n  %s;\n", quotedTable, strings.Join(quotedColumns, ", "), strings.Join(tuples, ",\n  "))
	return err
}

func writeSQLCreateTable(w io.Writer, table string, columns []string, types []string) error {
	if len(columns) == 0 {
		return errors.New("columns are required for CREATE TABLE")
	}
	types = normalizeSQLColumnTypes(types, len(columns))
	lines := make([]string, len(columns))
	for i, column := range columns {
		quoted, err := quoteSQLIdentifier(column)
		if err != nil {
			return err
		}
		lines[i] = fmt.Sprintf("  %s %s", quoted, types[i])
	}
	quotedTable, err := quoteSQLIdentifier(table)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "CREATE TABLE IF NOT EXISTS %s (\n%s\n);\n", quotedTable, strings.Join(lines, ",\n"))
	return err
}

type inferredSQLSchema struct {
	columns []string
	types   []string
}

func inferSQLSchemaForFile(inputPath string, opts SQLConvertOptions) (inferredSQLSchema, error) {
	input, err := os.Open(inputPath)
	if err != nil {
		return inferredSQLSchema{}, err
	}
	defer input.Close()

	report, err := InferSchema(input, SchemaOptions{
		Delimiter:  opts.Delimiter,
		HasHeader:  opts.HasHeader,
		NullValues: opts.NullValues,
	})
	if err != nil {
		return inferredSQLSchema{}, err
	}
	columns, types := schemaColumnsForSQL(report.Columns)
	return inferredSQLSchema{columns: columns, types: types}, nil
}

func quoteSQLIdentifier(identifier string) (string, error) {
	if err := validateSQLIdentifier("SQL identifier", identifier); err != nil {
		return "", err
	}
	return "`" + strings.ReplaceAll(identifier, "`", "``") + "`", nil
}

func quoteSQLString(value string) (string, error) {
	var b strings.Builder
	b.Grow(len(value) + 2)
	b.WriteByte('\'')
	for i := 0; i < len(value); {
		r, size := utf8.DecodeRuneInString(value[i:])
		if r == utf8.RuneError && size == 1 {
			return "", fmt.Errorf("invalid UTF-8 byte 0x%02X cannot be emitted safely", value[i])
		}
		switch r {
		case 0:
			b.WriteString(`\0`)
		case '\b':
			b.WriteString(`\b`)
		case '\t':
			b.WriteString(`\t`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case 0x1A:
			b.WriteString(`\Z`)
		case '\\':
			b.WriteString(`\\`)
		case '\'':
			b.WriteString("''")
		default:
			if r < 0x20 || r == 0x7F {
				return "", fmt.Errorf("control character U+%04X cannot be emitted safely", r)
			}
			b.WriteString(value[i : i+size])
		}
		i += size
	}
	b.WriteByte('\'')
	return b.String(), nil
}

func validateSQLIdentifiers(label string, identifiers []string) error {
	for i, identifier := range identifiers {
		if err := validateSQLIdentifier(fmt.Sprintf("%s %d", label, i+1), identifier); err != nil {
			return err
		}
	}
	return nil
}

func validateSQLIdentifier(label, identifier string) error {
	if strings.TrimSpace(identifier) == "" {
		return fmt.Errorf("%s is required", label)
	}
	for _, r := range identifier {
		if r == 0 || r < 0x20 || r == 0x7F {
			return fmt.Errorf("%s contains unsupported control character U+%04X", label, r)
		}
	}
	return nil
}
