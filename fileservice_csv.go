package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/wailsapp/wails/v3/pkg/application"

	"github.com/quarry/quarry-wails3/internal/encodingx"
	"github.com/quarry/quarry-wails3/internal/plugins/csv"
	"github.com/quarry/quarry-wails3/internal/session"
	"github.com/quarry/quarry-wails3/internal/sourceio"
)

const (
	csvSampleBytes        int64 = 2 << 20 // raw source bytes for inspect/schema/preview
	csvSampleDecodedBytes int64 = 3 * csvSampleBytes

	csvGridDefaultRawBytes = 128 * 1024
	csvGridMaxRawBytes     = 1 * 1024 * 1024
	csvGridMaxRows         = 500
	csvGridMaxColumns      = 256
	csvGridMaxCells        = 10_000
	csvGridCursorLimit     = 8_192

	csvPreviewDefaultRows = 50
	csvPreviewMaxRows     = 500
)

var (
	ErrCSVTransformEncodingUnsupported = errors.New("CSV transform source encoding is unsupported")
	ErrCSVGridCursorInvalid            = errors.New("CSV grid cursor was not issued for this file generation and delimiter")
	ErrCSVSourceGenerationRequired     = errors.New("CSV source generation is required")
	ErrCSVSampleIntervalInvalid        = errors.New("CSV sample interval must not be negative")
)

type csvGridCursorKey struct {
	fileID     string
	generation uint64
	delimiter  rune
	offset     int64
}

func (s *FileService) validateCSVGridCursor(fileID string, generation uint64, delimiter rune, offset int64) error {
	if offset == 0 {
		return nil
	}
	key := csvGridCursorKey{fileID: fileID, generation: generation, delimiter: delimiter, offset: offset}
	s.csvCursorMu.Lock()
	_, ok := s.csvCursors[key]
	s.csvCursorMu.Unlock()
	if !ok {
		return fmt.Errorf("%w: raw byte %d", ErrCSVGridCursorInvalid, offset)
	}
	return nil
}

func (s *FileService) issueCSVGridCursor(fileID string, generation uint64, delimiter rune, offset int64) {
	if offset <= 0 {
		return
	}
	key := csvGridCursorKey{fileID: fileID, generation: generation, delimiter: delimiter, offset: offset}
	s.csvCursorMu.Lock()
	defer s.csvCursorMu.Unlock()
	// The caller retains this generation's read lease through publication.
	if s.csvCursors == nil {
		s.csvCursors = make(map[csvGridCursorKey]struct{})
	}
	if _, exists := s.csvCursors[key]; exists {
		return
	}
	s.csvCursors[key] = struct{}{}
	if len(s.csvCursorOrder) < csvGridCursorLimit {
		s.csvCursorOrder = append(s.csvCursorOrder, key)
		return
	}
	if s.csvCursorNext < 0 || s.csvCursorNext >= len(s.csvCursorOrder) {
		s.csvCursorNext = 0
	}
	evicted := s.csvCursorOrder[s.csvCursorNext]
	delete(s.csvCursors, evicted)
	s.csvCursorOrder[s.csvCursorNext] = key
	s.csvCursorNext = (s.csvCursorNext + 1) % csvGridCursorLimit
}

func (s *FileService) invalidateCSVGridCursors(fileID string) {
	s.csvCursorMu.Lock()
	defer s.csvCursorMu.Unlock()
	if len(s.csvCursorOrder) == 0 {
		return
	}
	kept := s.csvCursorOrder[:0]
	for _, key := range s.csvCursorOrder {
		if key.fileID == fileID {
			delete(s.csvCursors, key)
			continue
		}
		kept = append(kept, key)
	}
	s.csvCursorOrder = kept
	s.csvCursorNext = 0
}

// parseCSVDelimiter accepts only an exact symbolic name or one valid delimiter
// rune. Empty, ambiguous, normalized, and multi-rune requests fail closed.
func parseCSVDelimiter(s string) (rune, error) {
	var delimiter rune
	switch s {
	case ",", "comma":
		delimiter = ','
	case "\t", "\\t", "tab":
		delimiter = '\t'
	case ";", "semicolon":
		delimiter = ';'
	case "|", "pipe":
		delimiter = '|'
	case " ", "space":
		delimiter = ' '
	default:
		if len(s) > utf8.UTFMax {
			return 0, fmt.Errorf("CSV delimiter must be one exact valid rune or known name, got %d bytes", len(s))
		}
		decoded, width := utf8.DecodeRuneInString(s)
		if width == 0 || width != len(s) || (decoded == utf8.RuneError && width == 1) {
			return 0, fmt.Errorf("CSV delimiter must be one exact valid rune or known name, got %q", s)
		}
		delimiter = decoded
	}
	if err := csv.ValidateDelimiter(delimiter); err != nil {
		return 0, err
	}
	return delimiter, nil
}

func delimiterString(r rune) string {
	if r == 0 {
		return ","
	}
	return string(r)
}

type csvDecodedSample struct {
	data       []byte
	encoding   string
	truncated  bool
	generation uint64
}

func (s *FileService) csvSample(fileID string) (csvDecodedSample, error) {
	lease, _, err := s.acquireReadFile(fileID)
	if err != nil {
		return csvDecodedSample{}, err
	}
	defer lease.Release()
	snapshot := lease.Snapshot()
	meta := snapshot.Doc.Metadata()
	if meta.EncodingRequiresConfirmation {
		return csvDecodedSample{}, fmt.Errorf("%w: %s", encodingx.ErrEncodingConfirmationRequired, meta.Encoding)
	}
	if err := snapshot.Doc.ValidateUnchanged(); err != nil {
		return csvDecodedSample{}, err
	}
	decoded, err := csv.DecodeSourceWindow(context.Background(), io.NewSectionReader(snapshot.Doc, 0, snapshot.Doc.Size()), csv.DecodeWindowOptions{
		Encoding:        meta.Encoding,
		AtBOF:           true,
		MaxRawBytes:     csvSampleBytes,
		MaxDecodedBytes: csvSampleDecodedBytes,
	})
	if err != nil {
		return csvDecodedSample{}, err
	}
	if err := snapshot.Doc.ValidateUnchanged(); err != nil {
		return csvDecodedSample{}, err
	}
	return csvDecodedSample{
		data: decoded.Data, encoding: meta.Encoding, truncated: decoded.Truncated, generation: snapshot.Generation,
	}, nil
}

func prepareCSVSample(ctx context.Context, sample csvDecodedSample, delimiter rune) ([]byte, []string, error) {
	data, omitted, err := csv.CompleteRecordPrefix(ctx, sample.data, delimiter, csvSampleDecodedBytes, sample.truncated)
	if err != nil {
		return nil, nil, err
	}
	warnings := csvSampleWarnings(sample)
	if omitted {
		if len(data) == 0 && len(sample.data) > 0 {
			return nil, nil, fmt.Errorf("the first CSV logical record exceeds the %d-byte raw preview budget", csvSampleBytes)
		}
		warnings = append(warnings, "trailing partial logical record omitted from the bounded sample")
	}
	return data, warnings, nil
}

func csvSampleWarnings(sample csvDecodedSample) []string {
	if !sample.truncated {
		return nil
	}
	return []string{fmt.Sprintf("source sample limited to %d raw bytes and decoded as %s", csvSampleBytes, sample.encoding)}
}

func decodedSampleLimit(data []byte) int64 {
	if len(data) == 0 {
		return 1
	}
	return int64(len(data))
}

func requireCSVTransformEncoding(file *session.File) error {
	if file == nil || file.Doc == nil {
		return errors.New("CSV source document is required")
	}
	meta := file.Doc.Metadata()
	if meta.EncodingRequiresConfirmation {
		return fmt.Errorf("%w: choose the source encoding explicitly before transforming", encodingx.ErrEncodingConfirmationRequired)
	}
	if meta.Encoding != "" && !strings.EqualFold(meta.Encoding, "UTF-8") && !strings.EqualFold(meta.Encoding, "ASCII") {
		return fmt.Errorf("%w: %s input must be converted to UTF-8 before CSV transforms so output semantics are explicit", ErrCSVTransformEncodingUnsupported, meta.Encoding)
	}
	if err := file.Doc.ValidateUnchanged(); err != nil {
		return fmt.Errorf("%w: CSV source generation changed: %w", sourceio.ErrSourceChanged, err)
	}
	return nil
}

func expectedCSVSource(ctx context.Context, file *session.File) (*csv.SourceExpectation, error) {
	if file == nil || file.Doc == nil {
		return nil, errors.New("CSV source document is required")
	}
	return sourceio.ExpectDocumentContext(ctx, file.Doc)
}

func validateCSVSourceGeneration(snapshot session.FileSnapshot, expectedGeneration uint64) error {
	if expectedGeneration == 0 {
		return ErrCSVSourceGenerationRequired
	}
	if snapshot.Generation != expectedGeneration {
		return fmt.Errorf("%w: CSV preview generation %d is no longer current (current generation %d)", sourceio.ErrSourceChanged, expectedGeneration, snapshot.Generation)
	}
	return nil
}

func (s *FileService) csvTransformPreflight(fileID string, expectedGeneration uint64) error {
	lease, file, finish, err := s.acquireReadFilePreflight(fileID)
	if err != nil {
		return err
	}
	defer finish()
	if err := validateCSVSourceGeneration(lease.Snapshot(), expectedGeneration); err != nil {
		return err
	}
	return requireCSVTransformEncoding(file)
}

func (s *FileService) withCSVTransformJob(fileID string, expectedGeneration uint64, title string, fn func(context.Context, *session.File, func(int64, string)) (TransformResult, error)) (TransformResult, error) {
	return s.withFileJob(fileID, title, func(ctx context.Context, progress func(int64, string)) (TransformResult, error) {
		lease, file, err := s.acquireReadFileContext(ctx, fileID)
		if err != nil {
			return TransformResult{}, err
		}
		defer lease.Release()
		if err := validateCSVSourceGeneration(lease.Snapshot(), expectedGeneration); err != nil {
			return TransformResult{}, err
		}
		if err := requireCSVTransformEncoding(file); err != nil {
			return TransformResult{}, err
		}
		return fn(ctx, file, progress)
	})
}

// ---- inspect (delimiter detection) -------------------------------------

type CsvDelimiterOption struct {
	Delimiter string  `json:"delimiter"`
	Name      string  `json:"name"`
	Columns   int     `json:"columns"`
	Score     float64 `json:"score"`
}

type CsvInspectResult struct {
	Generation    uint64               `json:"generation"`
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
	sample, err := s.csvSample(fileID)
	if err != nil {
		return CsvInspectResult{}, err
	}
	rep, err := csv.InspectReaderContext(context.Background(), bytes.NewReader(sample.data), csv.InspectOptions{
		MaxBytes: decodedSampleLimit(sample.data), MaxRows: 1000, SourceTruncated: sample.truncated,
	})
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
		Generation:    sample.generation,
		Delimiter:     delimiterString(rep.Delimiter),
		DelimiterName: rep.DelimiterName,
		Confidence:    rep.Confidence,
		Columns:       rep.Columns,
		HasHeader:     rep.HasHeader,
		Candidates:    cands,
		Warnings:      append(rep.Warnings, csvSampleWarnings(sample)...),
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
	Generation uint64      `json:"generation"`
	Columns    []CsvColumn `json:"columns"`
	HasHeader  bool        `json:"hasHeader"`
	Warnings   []string    `json:"warnings"`
}

func (s *FileService) CsvSchema(fileID string, delimiter string, hasHeader bool) (CsvSchemaResult, error) {
	parsedDelimiter, err := parseCSVDelimiter(delimiter)
	if err != nil {
		return CsvSchemaResult{}, err
	}
	sample, err := s.csvSample(fileID)
	if err != nil {
		return CsvSchemaResult{}, err
	}
	data, warnings, err := prepareCSVSample(context.Background(), sample, parsedDelimiter)
	if err != nil {
		return CsvSchemaResult{}, err
	}
	rep, err := csv.InferSchemaContext(context.Background(), bytes.NewReader(data), csv.SchemaOptions{
		Delimiter: parsedDelimiter,
		HasHeader: hasHeader,
		MaxBytes:  decodedSampleLimit(data),
		MaxRows:   2000,
	})
	if err != nil {
		return CsvSchemaResult{}, err
	}
	cols := make([]CsvColumn, 0, len(rep.Columns))
	for _, c := range rep.Columns {
		cols = append(cols, CsvColumn{Name: c.Name, SQLType: c.SQLType, NonNull: c.NonNullCount, Null: c.NullCount, Samples: c.SampleValues})
	}
	return CsvSchemaResult{Generation: sample.generation, Columns: cols, HasHeader: rep.HasHeader, Warnings: append(rep.Warnings, warnings...)}, nil
}

type CsvPreviewResult struct {
	Generation uint64     `json:"generation"`
	Header     []string   `json:"header"`
	Rows       [][]string `json:"rows"`
	Warnings   []string   `json:"warnings"`
}

func (s *FileService) CsvPreview(fileID string, delimiter string, hasHeader bool, maxRows int) (CsvPreviewResult, error) {
	parsedDelimiter, err := parseCSVDelimiter(delimiter)
	if err != nil {
		return CsvPreviewResult{}, err
	}
	maxRows, err = normalizeCSVPreviewRows(maxRows)
	if err != nil {
		return CsvPreviewResult{}, err
	}
	sample, err := s.csvSample(fileID)
	if err != nil {
		return CsvPreviewResult{}, err
	}
	data, warnings, err := prepareCSVSample(context.Background(), sample, parsedDelimiter)
	if err != nil {
		return CsvPreviewResult{}, err
	}
	rep, err := csv.PreviewRowsContext(context.Background(), bytes.NewReader(data), csv.PreviewOptions{
		Delimiter: parsedDelimiter,
		HasHeader: hasHeader,
		MaxBytes:  decodedSampleLimit(data),
		MaxRows:   maxRows,
	})
	if err != nil {
		return CsvPreviewResult{}, err
	}
	return CsvPreviewResult{Generation: sample.generation, Header: rep.Header, Rows: rep.Rows, Warnings: append(rep.Warnings, warnings...)}, nil
}

// ---- windowed grid (spreadsheet view) -----------------------------------

type CsvGridResult struct {
	FileID       string     `json:"fileId"`
	Generation   uint64     `json:"generation"`
	Encoding     string     `json:"encoding"`
	StartByte    int64      `json:"startByte"`
	NextByte     int64      `json:"nextByte"`
	StartRow     int64      `json:"startRow"` // approximate physical line of the first record
	Rows         [][]string `json:"rows"`
	Columns      int        `json:"columns"`
	SourceBytes  int64      `json:"sourceBytes"`
	DecodedBytes int        `json:"decodedBytes"`
	AtBof        bool       `json:"atBof"`
	AtEof        bool       `json:"atEof"`
}

// GetCsvGrid parses a bounded raw-byte window into complete logical CSV
// records. startByte must be zero or a NextByte returned by an earlier call for
// the same file generation and delimiter. Physical newlines inside quoted
// fields never become cursors. Rows are returned as data (no header
// special-casing); the frontend supplies column labels from the schema.
func (s *FileService) GetCsvGrid(fileID string, delimiter string, startByte int64, maxBytes int) (CsvGridResult, error) {
	parsedDelimiter, err := parseCSVDelimiter(delimiter)
	if err != nil {
		return CsvGridResult{}, err
	}
	lease, _, err := s.acquireReadFile(fileID)
	if err != nil {
		return CsvGridResult{}, err
	}
	defer lease.Release()
	snapshot := lease.Snapshot()
	if maxBytes < 0 {
		return CsvGridResult{}, errors.New("CSV grid raw-byte budget must not be negative")
	}
	if maxBytes == 0 {
		maxBytes = csvGridDefaultRawBytes
	}
	if maxBytes > csvGridMaxRawBytes {
		return CsvGridResult{}, fmt.Errorf("CSV grid raw-byte budget %d exceeds hard limit %d", maxBytes, csvGridMaxRawBytes)
	}
	meta := snapshot.Doc.Metadata()
	if meta.EncodingRequiresConfirmation {
		return CsvGridResult{}, fmt.Errorf("%w: %s", encodingx.ErrEncodingConfirmationRequired, meta.Encoding)
	}
	size := snapshot.Doc.Size()
	if startByte < 0 || startByte > size {
		return CsvGridResult{}, fmt.Errorf("CSV grid cursor %d is outside [0, %d]", startByte, size)
	}
	if err := s.validateCSVGridCursor(fileID, snapshot.Generation, parsedDelimiter, startByte); err != nil {
		return CsvGridResult{}, err
	}
	aligned, err := snapshot.Doc.AlignTextOffsetBackward(startByte)
	if err != nil {
		return CsvGridResult{}, err
	}
	if aligned != startByte {
		return CsvGridResult{}, fmt.Errorf("CSV grid cursor %d splits a %s source character", startByte, meta.Encoding)
	}
	if err := snapshot.Doc.ValidateUnchanged(); err != nil {
		return CsvGridResult{}, err
	}
	decoded, err := csv.DecodeSourceWindow(context.Background(), io.NewSectionReader(snapshot.Doc, startByte, size-startByte), csv.DecodeWindowOptions{
		Encoding:        meta.Encoding,
		AtBOF:           startByte == 0,
		MaxRawBytes:     int64(maxBytes),
		MaxDecodedBytes: int64(3 * maxBytes),
		TrackOffsets:    true,
	})
	if err != nil {
		return CsvGridResult{}, err
	}
	records, err := csv.ParseRecordWindow(context.Background(), decoded.Data, csv.RecordWindowOptions{
		Delimiter:       parsedDelimiter,
		SourceTruncated: decoded.Truncated,
		MaxRecordBytes:  int64(3 * maxBytes),
		MaxRows:         csvGridMaxRows,
		MaxColumns:      csvGridMaxColumns,
		MaxCells:        csvGridMaxCells,
	})
	if err != nil {
		return CsvGridResult{}, err
	}
	rawEnd, err := decoded.RawOffsetForDecodedEnd(records.DecodedEnd)
	if err != nil {
		return CsvGridResult{}, err
	}
	if records.DecodedEnd == 0 && len(decoded.Data) == 0 && !decoded.Truncated {
		rawEnd = decoded.RawBytesDecoded
	}
	nextByte := startByte + rawEnd
	if nextByte == startByte && startByte < size {
		return CsvGridResult{}, errors.New("CSV grid raw-byte budget is too small for one complete source character or logical record")
	}
	if nextByte > size {
		return CsvGridResult{}, errors.New("CSV parser continuation exceeds the opened source size")
	}
	if err := snapshot.Doc.ValidateUnchanged(); err != nil {
		return CsvGridResult{}, err
	}
	s.issueCSVGridCursor(fileID, snapshot.Generation, parsedDelimiter, nextByte)
	startRow, _ := snapshot.Doc.ApproxOffsetToLine(startByte)
	if startRow <= 0 {
		startRow = 1
	}
	return CsvGridResult{
		FileID:       fileID,
		Generation:   snapshot.Generation,
		Encoding:     meta.Encoding,
		StartByte:    startByte,
		NextByte:     nextByte,
		StartRow:     startRow,
		Rows:         records.Rows,
		Columns:      records.Columns,
		SourceBytes:  nextByte - startByte,
		DecodedBytes: records.DecodedEnd,
		AtBof:        startByte == 0,
		AtEof:        nextByte >= size,
	}, nil
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

// csvSaveDialog is an indirection for deterministic lifecycle tests. Keeping
// it CSV-specific prevents test overrides from affecting unrelated exports.
var csvSaveDialog = saveDialog

// dirDialog prompts for an existing/new folder and returns its path ("" if cancelled).
func dirDialog(message string) (string, error) {
	d := application.Get().Dialog.OpenFile().
		SetMessage(message).
		CanChooseDirectories(true).
		CanChooseFiles(false).
		CanCreateDirectories(true)
	return d.PromptForSingleSelection()
}

// CsvProfileResult is a per-column data profile over a bounded sample.
type CsvProfileResult struct {
	Generation     uint64              `json:"generation"`
	Columns        []csv.ColumnProfile `json:"columns"`
	RecordsScanned int                 `json:"recordsScanned"`
	RaggedRows     int                 `json:"raggedRows"`
	Truncated      bool                `json:"truncated"`
}

// CsvProfile profiles each column (null %, distinct, min/max, top values) and
// counts ragged rows over a bounded sample.
func (s *FileService) CsvProfile(fileID, delimiter string, hasHeader bool) (CsvProfileResult, error) {
	parsedDelimiter, err := parseCSVDelimiter(delimiter)
	if err != nil {
		return CsvProfileResult{}, err
	}
	sample, err := s.csvSample(fileID)
	if err != nil {
		return CsvProfileResult{}, err
	}
	data, _, err := prepareCSVSample(context.Background(), sample, parsedDelimiter)
	if err != nil {
		return CsvProfileResult{}, err
	}
	rep, err := csv.ProfileColumns(context.Background(), bytes.NewReader(data), csv.SchemaOptions{
		Delimiter:  parsedDelimiter,
		HasHeader:  hasHeader,
		MaxBytes:   decodedSampleLimit(data),
		NullValues: []string{"", "NULL", "null", "\\N"},
	})
	if err != nil {
		return CsvProfileResult{}, err
	}
	return CsvProfileResult{
		Generation:     sample.generation,
		Columns:        rep.Columns,
		RecordsScanned: rep.RecordsScanned,
		RaggedRows:     rep.RaggedRows,
		Truncated:      rep.TruncatedSample,
	}, nil
}

// CsvTextPreviewResult binds rendered preview text to the exact open session
// generation that supplied its bytes. A caller must keep this token with any
// later transform request instead of assuming that fileID still names the same
// document generation.
type CsvTextPreviewResult struct {
	Generation uint64 `json:"generation"`
	Text       string `json:"text"`
}

// CsvProjectViaDialog writes a new CSV keeping only keepIndices (0-based) in the
// given order — used for drop-column and reorder.
func (s *FileService) CsvProjectViaDialog(fileID string, sourceGeneration uint64, delimiter string, keepIndices []int) (TransformResult, error) {
	if err := validateCSVProjectColumns(keepIndices); err != nil {
		return TransformResult{}, err
	}
	parsedDelimiter, err := parseCSVDelimiter(delimiter)
	if err != nil {
		return TransformResult{}, err
	}
	if err := s.csvTransformPreflight(fileID, sourceGeneration); err != nil {
		return TransformResult{}, err
	}
	dst, err := csvSaveDialog("Save projected CSV as", "projected.csv")
	if err != nil || dst == "" {
		return TransformResult{}, err
	}
	return s.withCSVTransformJob(fileID, sourceGeneration, "Project CSV columns", func(ctx context.Context, file *session.File, _ func(int64, string)) (TransformResult, error) {
		expected, err := expectedCSVSource(ctx, file)
		if err != nil {
			return TransformResult{}, err
		}
		sum, err := csv.ProjectColumnsFile(ctx, file.Path, dst, csv.ProjectOptions{Delimiter: parsedDelimiter, Columns: keepIndices, ExpectedSource: expected})
		result := TransformResult{OutputPath: dst, RecordsRead: sum.RecordsRead, RecordsWritten: sum.RecordsWritten, Note: fmt.Sprintf("%d columns kept", sum.ColumnsWritten)}
		if err != nil {
			return transformResultAfterPublication(result, err)
		}
		return result, nil
	})
}

// validateCSVProjectColumns runs before any lease, dialog, filesystem work, or
// plugin-owned per-record allocation. It deliberately uses a bounded,
// allocation-free duplicate check so the bridge cannot size a map from an
// unchecked collection.
func validateCSVProjectColumns(columns []int) error {
	if len(columns) == 0 {
		return errors.New("select at least one column to project")
	}
	if len(columns) > csv.MaxTransformColumnMappings {
		return fmt.Errorf("CSV projection has %d column mappings; maximum is %d", len(columns), csv.MaxTransformColumnMappings)
	}
	for i, column := range columns {
		if column < 0 {
			return fmt.Errorf("projection column index %d at position %d is negative", column, i)
		}
		for previous := 0; previous < i; previous++ {
			if columns[previous] == column {
				return fmt.Errorf("projection column index %d is selected more than once", column)
			}
		}
	}
	return nil
}

func csvAddColumnOptions(delimiter string, position int, value string) (csv.AddColumnOptions, error) {
	parsedDelimiter, err := parseCSVDelimiter(delimiter)
	if err != nil {
		return csv.AddColumnOptions{}, err
	}
	opts := csv.AddColumnOptions{Delimiter: parsedDelimiter, Position: position, Value: value}
	if err := csv.ValidateAddColumnOptions(opts); err != nil {
		return csv.AddColumnOptions{}, err
	}
	return opts, nil
}

// CsvAddColumnViaDialog inserts a constant field at a validated zero-based
// position in every record. Short records are padded; wider records retain all
// fields in order.
func (s *FileService) CsvAddColumnViaDialog(fileID string, sourceGeneration uint64, delimiter string, position int, value string) (TransformResult, error) {
	opts, err := csvAddColumnOptions(delimiter, position, value)
	if err != nil {
		return TransformResult{}, err
	}
	if err := s.csvTransformPreflight(fileID, sourceGeneration); err != nil {
		return TransformResult{}, err
	}
	dst, err := csvSaveDialog("Save CSV with added column as", "with-column.csv")
	if err != nil || dst == "" {
		return TransformResult{}, err
	}
	return s.withCSVTransformJob(fileID, sourceGeneration, "Add CSV column", func(ctx context.Context, file *session.File, progress func(int64, string)) (TransformResult, error) {
		expected, err := expectedCSVSource(ctx, file)
		if err != nil {
			return TransformResult{}, err
		}
		opts.ExpectedSource = expected
		opts.Progress = func(p csv.AddColumnProgress) { progress(p.RecordsRead, "rows read") }
		sum, err := csv.AddColumnFile(ctx, file.Path, dst, opts)
		result := TransformResult{
			OutputPath:     dst,
			RecordsRead:    sum.RecordsRead,
			RecordsWritten: sum.RecordsWritten,
			Note:           fmt.Sprintf("constant column inserted at zero-based position %d; short records padded with empty fields", sum.Position),
		}
		if err != nil {
			return transformResultAfterPublication(result, err)
		}
		return result, nil
	})
}

// CsvToSQLPreview returns a short sample of the generated SQL.
func (s *FileService) CsvToSQLPreview(fileID, delimiter, tableName string, hasHeader, includeCreate bool) (CsvTextPreviewResult, error) {
	opts, err := csvSimpleSQLOptions(delimiter, tableName, hasHeader, includeCreate)
	if err != nil {
		return CsvTextPreviewResult{}, err
	}
	sample, err := s.csvSample(fileID)
	if err != nil {
		return CsvTextPreviewResult{}, err
	}
	data, _, err := prepareCSVSample(context.Background(), sample, opts.Delimiter)
	if err != nil {
		return CsvTextPreviewResult{}, err
	}
	rep, err := csv.PreviewSQLConversionContext(context.Background(), bytes.NewReader(data), csv.SQLPreviewOptions{
		SQLConvertOptions: opts,
		MaxBytes:          decodedSampleLimit(data),
		MaxRows:           20,
	})
	if err != nil {
		return CsvTextPreviewResult{}, err
	}
	return CsvTextPreviewResult{Generation: sample.generation, Text: rep.SQL}, nil
}

// CsvToSQLViaDialog streams the whole CSV to a .sql file of INSERTs.
func (s *FileService) CsvToSQLViaDialog(fileID string, sourceGeneration uint64, delimiter, tableName string, hasHeader, includeCreate bool) (TransformResult, error) {
	opts, err := csvSimpleSQLOptions(delimiter, tableName, hasHeader, includeCreate)
	if err != nil {
		return TransformResult{}, err
	}
	if err := s.csvTransformPreflight(fileID, sourceGeneration); err != nil {
		return TransformResult{}, err
	}
	dst, err := csvSaveDialog("Save SQL as", safeFileName(opts.TableName)+".sql")
	if err != nil || dst == "" {
		return TransformResult{}, err
	}
	return s.withCSVTransformJob(fileID, sourceGeneration, "Convert CSV to SQL", func(ctx context.Context, file *session.File, _ func(int64, string)) (TransformResult, error) {
		expected, err := expectedCSVSource(ctx, file)
		if err != nil {
			return TransformResult{}, err
		}
		jobOpts := opts
		jobOpts.ExpectedSource = expected
		sum, err := csv.ConvertToSQLFile(ctx, file.Path, dst, jobOpts)
		result := TransformResult{OutputPath: dst, RecordsRead: sum.RecordsRead, RecordsWritten: sum.RowsWritten, Note: fmt.Sprintf("table %q", sum.TableName)}
		if err != nil {
			return transformResultAfterPublication(result, err)
		}
		return result, nil
	})
}

func csvSimpleSQLOptions(delimiter, tableName string, hasHeader, includeCreate bool) (csv.SQLConvertOptions, error) {
	// Bound the raw bridge value before TrimSpace or any Unicode scan.
	if len(tableName) > csv.MaxSQLIdentifierBytes {
		return csv.SQLConvertOptions{}, fmt.Errorf("table name exceeds the %d-byte SQL identifier input limit", csv.MaxSQLIdentifierBytes)
	}
	parsedDelimiter, err := parseCSVDelimiter(delimiter)
	if err != nil {
		return csv.SQLConvertOptions{}, err
	}
	opts := csv.SQLConvertOptions{
		Delimiter:          parsedDelimiter,
		TableName:          sqlTableName(tableName),
		HasHeader:          hasHeader,
		IncludeCreateTable: includeCreate,
	}
	if err := csv.ValidateSQLConvertOptions(opts); err != nil {
		return csv.SQLConvertOptions{}, err
	}
	return opts, nil
}

func sqlTableName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "imported"
	}
	return name
}

// CsvSqlColumnConfig describes one output column of a CSV→SQL conversion.
type CsvSqlColumnConfig struct {
	Source  int    `json:"source"`  // 0-based source field index
	Name    string `json:"name"`    // output column name
	Type    string `json:"type"`    // SQL type (used only when includeCreate)
	Include bool   `json:"include"` // whether to emit this column
}

// CsvSqlConfig is the full, flexible CSV→SQL conversion request.
type CsvSqlConfig struct {
	Delimiter     string               `json:"delimiter"`
	HasHeader     bool                 `json:"hasHeader"`
	TableName     string               `json:"tableName"`
	Columns       []CsvSqlColumnConfig `json:"columns"`
	IncludeCreate bool                 `json:"includeCreate"`
	InsertMode    string               `json:"insertMode"` // insert | ignore | replace
	BatchSize     int                  `json:"batchSize"`
	NullValues    []string             `json:"nullValues"`
	OnInvalid     string               `json:"onInvalid"` // fail | skip-row | replace
}

func csvSqlOptions(cfg CsvSqlConfig) (csv.SQLConvertOptions, error) {
	if err := validateCsvSqlConfigCollections(cfg); err != nil {
		return csv.SQLConvertOptions{}, err
	}
	parsedDelimiter, err := parseCSVDelimiter(cfg.Delimiter)
	if err != nil {
		return csv.SQLConvertOptions{}, err
	}
	var src []int
	var names, types []string
	for _, c := range cfg.Columns {
		if !c.Include {
			continue
		}
		src = append(src, c.Source)
		names = append(names, strings.TrimSpace(c.Name))
		types = append(types, c.Type)
	}
	if len(names) == 0 {
		return csv.SQLConvertOptions{}, fmt.Errorf("select at least one column to convert")
	}
	var insertMode csv.SQLInsertMode
	switch cfg.InsertMode {
	case "insert":
		insertMode = csv.SQLInsertModeInsert
	case "ignore":
		insertMode = csv.SQLInsertModeInsertIgnore
	case "replace":
		insertMode = csv.SQLInsertModeReplace
	default:
		return csv.SQLConvertOptions{}, fmt.Errorf("invalid CSV-to-SQL insert mode %q", cfg.InsertMode)
	}
	opts := csv.SQLConvertOptions{
		Delimiter:          parsedDelimiter,
		TableName:          sqlTableName(cfg.TableName),
		HasHeader:          cfg.HasHeader,
		Columns:            names,
		ColumnTypes:        types,
		SourceColumns:      src,
		IncludeCreateTable: cfg.IncludeCreate,
		InsertMode:         insertMode,
		InsertBatchSize:    cfg.BatchSize,
		NullValues:         cfg.NullValues,
		OnInvalidValue:     csv.InvalidValuePolicy(strings.TrimSpace(cfg.OnInvalid)),
	}
	if err := csv.ValidateSQLConvertOptions(opts); err != nil {
		return csv.SQLConvertOptions{}, err
	}
	return opts, nil
}

func addCSVTransformConfigString(total *int, label, value string) error {
	remaining := csv.MaxTransformConfigStringBytes - *total
	if len(value) > remaining {
		return fmt.Errorf("%s configuration strings exceed %d-byte aggregate limit", label, csv.MaxTransformConfigStringBytes)
	}
	*total += len(value)
	return nil
}

func validateCsvSqlConfigCollections(cfg CsvSqlConfig) error {
	if len(cfg.Columns) > csv.MaxTransformColumnMappings {
		return fmt.Errorf("CSV-to-SQL config has %d column mappings; maximum is %d", len(cfg.Columns), csv.MaxTransformColumnMappings)
	}
	if len(cfg.NullValues) > csv.MaxTransformColumnMappings {
		return fmt.Errorf("CSV-to-SQL config has %d null sentinels; maximum is %d", len(cfg.NullValues), csv.MaxTransformColumnMappings)
	}
	stringBytes := 0
	if err := addCSVTransformConfigString(&stringBytes, "CSV-to-SQL", cfg.Delimiter); err != nil {
		return err
	}
	if err := addCSVTransformConfigString(&stringBytes, "CSV-to-SQL", cfg.TableName); err != nil {
		return err
	}
	if err := addCSVTransformConfigString(&stringBytes, "CSV-to-SQL", cfg.InsertMode); err != nil {
		return err
	}
	if err := addCSVTransformConfigString(&stringBytes, "CSV-to-SQL", cfg.OnInvalid); err != nil {
		return err
	}
	for i, column := range cfg.Columns {
		if column.Source < 0 {
			return fmt.Errorf("CSV-to-SQL source column %d at position %d is negative", column.Source, i)
		}
		for previous := 0; previous < i; previous++ {
			if cfg.Columns[previous].Source == column.Source {
				return fmt.Errorf("CSV-to-SQL source column %d is selected more than once", column.Source)
			}
		}
		if err := addCSVTransformConfigString(&stringBytes, "CSV-to-SQL", column.Name); err != nil {
			return err
		}
		if err := addCSVTransformConfigString(&stringBytes, "CSV-to-SQL", column.Type); err != nil {
			return err
		}
	}
	for _, value := range cfg.NullValues {
		if err := addCSVTransformConfigString(&stringBytes, "CSV-to-SQL", value); err != nil {
			return err
		}
	}
	return nil
}

// CsvRedactColumn is one column to mask in a redaction.
type CsvRedactColumn struct {
	Index int    `json:"index"`
	Mode  string `json:"mode"` // null | fixed | hash | email
}

func csvRedactOptions(delimiter string, hasHeader bool, columns []CsvRedactColumn, replacement string) (csv.RedactOptions, error) {
	if err := validateCsvRedactConfigCollections(delimiter, columns, replacement); err != nil {
		return csv.RedactOptions{}, err
	}
	parsedDelimiter, err := parseCSVDelimiter(delimiter)
	if err != nil {
		return csv.RedactOptions{}, err
	}
	cols := make(map[int]csv.RedactMode, len(columns))
	for _, column := range columns {
		if _, duplicate := cols[column.Index]; duplicate {
			return csv.RedactOptions{}, fmt.Errorf("redaction column index %d is selected more than once", column.Index)
		}
		// Do not trim or case-fold modes. The RPC contract is an exact enum, and
		// silently normalizing an unknown value risks treating PII as redacted.
		cols[column.Index] = csv.RedactMode(column.Mode)
	}
	opts := csv.RedactOptions{
		Delimiter:   parsedDelimiter,
		HasHeader:   hasHeader,
		Columns:     cols,
		Replacement: replacement,
	}
	for _, mode := range opts.Columns {
		if mode != csv.RedactHash {
			continue
		}
		key, err := csv.NewPseudonymKey()
		if err != nil {
			return csv.RedactOptions{}, err
		}
		opts.PseudonymKey = key
		break
	}
	if err := csv.ValidateRedactOptions(opts); err != nil {
		return csv.RedactOptions{}, err
	}
	return opts, nil
}

func validateCsvRedactConfigCollections(delimiter string, columns []CsvRedactColumn, replacement string) error {
	if len(columns) == 0 {
		return errors.New("select at least one column to redact")
	}
	if len(columns) > csv.MaxTransformColumnMappings {
		return fmt.Errorf("CSV redaction has %d column mappings; maximum is %d", len(columns), csv.MaxTransformColumnMappings)
	}
	stringBytes := 0
	if err := addCSVTransformConfigString(&stringBytes, "CSV redaction", delimiter); err != nil {
		return err
	}
	if err := addCSVTransformConfigString(&stringBytes, "CSV redaction", replacement); err != nil {
		return err
	}
	for i, column := range columns {
		if column.Index < 0 {
			return fmt.Errorf("redaction column index %d is negative", column.Index)
		}
		for previous := 0; previous < i; previous++ {
			if columns[previous].Index == column.Index {
				return fmt.Errorf("redaction column index %d is selected more than once", column.Index)
			}
		}
		switch csv.RedactMode(column.Mode) {
		case csv.RedactNull, csv.RedactFixed, csv.RedactHash, csv.RedactEmail:
		default:
			return fmt.Errorf("invalid redaction mode %q for column %d", column.Mode, column.Index)
		}
		if err := addCSVTransformConfigString(&stringBytes, "CSV redaction", column.Mode); err != nil {
			return err
		}
	}
	return nil
}

func validateCsvRedactEncoding(encoding string, opts csv.RedactOptions) error {
	for _, mode := range opts.Columns {
		if mode != csv.RedactEmail {
			continue
		}
		enc := strings.ToLower(strings.TrimSpace(encoding))
		if enc != "utf-8" && enc != "utf8" && enc != "ascii" {
			return fmt.Errorf("email redaction requires a UTF-8 source; detected %q", encoding)
		}
	}
	return nil
}

// CsvRedactViaDialog writes a pseudonymized/masked copy of selected CSV
// columns. Pseudonymization reduces direct exposure but is not anonymization.
// Source is never modified.
func (s *FileService) CsvRedactViaDialog(fileID string, sourceGeneration uint64, delimiter string, hasHeader bool, columns []CsvRedactColumn, replacement string) (TransformResult, error) {
	opts, err := csvRedactOptions(delimiter, hasHeader, columns, replacement)
	if err != nil {
		return TransformResult{}, err
	}
	preflight, file, finishPreflight, err := s.acquireReadFilePreflight(fileID)
	if err != nil {
		return TransformResult{}, err
	}
	defer finishPreflight()
	if err := validateCSVSourceGeneration(preflight.Snapshot(), sourceGeneration); err != nil {
		return TransformResult{}, err
	}
	if err := requireCSVTransformEncoding(file); err != nil {
		return TransformResult{}, err
	}
	if err := validateCsvRedactEncoding(file.Doc.Metadata().Encoding, opts); err != nil {
		return TransformResult{}, err
	}
	finishPreflight()
	dst, err := csvSaveDialog("Save redacted copy as", "redacted.csv")
	if err != nil || dst == "" {
		return TransformResult{}, err
	}
	return s.withCSVTransformJob(fileID, sourceGeneration, "Redact CSV", func(ctx context.Context, current *session.File, progress func(int64, string)) (TransformResult, error) {
		if err := validateCsvRedactEncoding(current.Doc.Metadata().Encoding, opts); err != nil {
			return TransformResult{}, err
		}
		expected, err := expectedCSVSource(ctx, current)
		if err != nil {
			return TransformResult{}, err
		}
		opts.ExpectedSource = expected
		opts.Progress = func(r int64, _ int64) { progress(r, "rows read") }
		sum, err := csv.RedactColumnsFile(ctx, current.Path, dst, opts)
		result := TransformResult{
			OutputPath:     dst,
			RecordsRead:    sum.RecordsRead,
			RecordsWritten: sum.RecordsWritten,
			Note:           fmt.Sprintf("%d cell values changed by masking across %d columns", sum.CellsMasked, len(opts.Columns)),
		}
		if err != nil {
			return transformResultAfterPublication(result, err)
		}
		return result, nil
	})
}

// CsvFilterViaDialog writes a new CSV keeping only rows where the chosen column
// matches op/value (eq, ne, contains, gt, lt, empty, nonempty). Source untouched.
func csvFilterOptions(delimiter string, hasHeader bool, column int, op, value string, negate bool) (csv.FilterOptions, error) {
	parsedDelimiter, err := parseCSVDelimiter(delimiter)
	if err != nil {
		return csv.FilterOptions{}, err
	}
	opts := csv.FilterOptions{
		Delimiter: parsedDelimiter,
		HasHeader: hasHeader,
		Column:    column,
		Op:        csv.FilterOp(op),
		Value:     value,
		Negate:    negate,
	}
	if err := csv.ValidateFilterOptions(opts); err != nil {
		return csv.FilterOptions{}, err
	}
	return opts, nil
}

func (s *FileService) CsvFilterViaDialog(fileID string, sourceGeneration uint64, delimiter string, hasHeader bool, column int, op, value string, negate bool) (TransformResult, error) {
	opts, err := csvFilterOptions(delimiter, hasHeader, column, op, value, negate)
	if err != nil {
		return TransformResult{}, err
	}
	if err := s.csvTransformPreflight(fileID, sourceGeneration); err != nil {
		return TransformResult{}, err
	}
	dst, err := csvSaveDialog("Save filtered CSV as", "filtered.csv")
	if err != nil || dst == "" {
		return TransformResult{}, err
	}
	return s.withCSVTransformJob(fileID, sourceGeneration, "Filter CSV", func(ctx context.Context, file *session.File, progress func(int64, string)) (TransformResult, error) {
		expected, err := expectedCSVSource(ctx, file)
		if err != nil {
			return TransformResult{}, err
		}
		opts.ExpectedSource = expected
		opts.Progress = func(r int64) { progress(r, "rows read") }
		sum, err := csv.FilterRowsFile(ctx, file.Path, dst, opts)
		result := TransformResult{
			OutputPath:     dst,
			RecordsRead:    sum.RecordsRead,
			RecordsWritten: sum.RecordsWritten,
			Note:           "rows matching filter kept",
		}
		if err != nil {
			return transformResultAfterPublication(result, err)
		}
		return result, nil
	})
}

// CsvDedupeViaDialog writes a new CSV dropping duplicate rows — by a key column
// (keyColumn>=0) or the whole row (keyColumn<0). First occurrence wins.
func (s *FileService) CsvDedupeViaDialog(fileID string, sourceGeneration uint64, delimiter string, hasHeader bool, keyColumn int) (TransformResult, error) {
	parsedDelimiter, err := parseCSVDelimiter(delimiter)
	if err != nil {
		return TransformResult{}, err
	}
	if err := s.csvTransformPreflight(fileID, sourceGeneration); err != nil {
		return TransformResult{}, err
	}
	dst, err := csvSaveDialog("Save deduplicated CSV as", "deduped.csv")
	if err != nil || dst == "" {
		return TransformResult{}, err
	}
	return s.withCSVTransformJob(fileID, sourceGeneration, "Deduplicate CSV", func(ctx context.Context, file *session.File, progress func(int64, string)) (TransformResult, error) {
		expected, err := expectedCSVSource(ctx, file)
		if err != nil {
			return TransformResult{}, err
		}
		sum, err := csv.DedupeRowsFile(ctx, file.Path, dst, csv.DedupeOptions{
			Delimiter:      parsedDelimiter,
			HasHeader:      hasHeader,
			KeyColumn:      keyColumn,
			ExpectedSource: expected,
			Progress:       func(r int64) { progress(r, "rows read") },
		})
		dropped := sum.RecordsRead - sum.RecordsWritten
		result := TransformResult{
			OutputPath:     dst,
			RecordsRead:    sum.RecordsRead,
			RecordsWritten: sum.RecordsWritten,
			Note:           fmt.Sprintf("%d duplicate rows removed", dropped),
		}
		if err != nil {
			return transformResultAfterPublication(result, err)
		}
		return result, nil
	})
}

// CsvSampleViaDialog writes a new CSV keeping every Nth data row (header kept),
// for shrinking a huge dump to a representative slice.
func (s *FileService) CsvSampleViaDialog(fileID string, sourceGeneration uint64, delimiter string, hasHeader bool, everyN int) (TransformResult, error) {
	if everyN < 0 {
		return TransformResult{}, fmt.Errorf("%w: %d", ErrCSVSampleIntervalInvalid, everyN)
	}
	parsedDelimiter, err := parseCSVDelimiter(delimiter)
	if err != nil {
		return TransformResult{}, err
	}
	if err := s.csvTransformPreflight(fileID, sourceGeneration); err != nil {
		return TransformResult{}, err
	}
	if everyN == 0 {
		everyN = 10
	}
	dst, err := csvSaveDialog("Save sampled CSV as", "sampled.csv")
	if err != nil || dst == "" {
		return TransformResult{}, err
	}
	return s.withCSVTransformJob(fileID, sourceGeneration, "Sample CSV", func(ctx context.Context, file *session.File, progress func(int64, string)) (TransformResult, error) {
		expected, err := expectedCSVSource(ctx, file)
		if err != nil {
			return TransformResult{}, err
		}
		sum, err := csv.SampleRowsFile(ctx, file.Path, dst, csv.SampleOptions{
			Delimiter:      parsedDelimiter,
			HasHeader:      hasHeader,
			EveryN:         everyN,
			ExpectedSource: expected,
			Progress:       func(r int64) { progress(r, "rows read") },
		})
		result := TransformResult{
			OutputPath:     dst,
			RecordsRead:    sum.RecordsRead,
			RecordsWritten: sum.RecordsWritten,
			Note:           fmt.Sprintf("kept every %dth row", everyN),
		}
		if err != nil {
			return transformResultAfterPublication(result, err)
		}
		return result, nil
	})
}

// CsvExportJSONLViaDialog streams the CSV to newline-delimited JSON.
func (s *FileService) CsvExportJSONLViaDialog(fileID string, sourceGeneration uint64, delimiter string, hasHeader, numberKeys bool) (TransformResult, error) {
	parsedDelimiter, err := parseCSVDelimiter(delimiter)
	if err != nil {
		return TransformResult{}, err
	}
	if err := s.csvTransformPreflight(fileID, sourceGeneration); err != nil {
		return TransformResult{}, err
	}
	dst, err := csvSaveDialog("Export JSON Lines as", "export.jsonl")
	if err != nil || dst == "" {
		return TransformResult{}, err
	}
	return s.withCSVTransformJob(fileID, sourceGeneration, "Export JSONL", func(ctx context.Context, file *session.File, progress func(int64, string)) (TransformResult, error) {
		expected, err := expectedCSVSource(ctx, file)
		if err != nil {
			return TransformResult{}, err
		}
		sum, err := csv.ExportJSONLFile(ctx, file.Path, dst, csv.JSONLOptions{
			Delimiter:      parsedDelimiter,
			HasHeader:      hasHeader,
			NumberKeys:     numberKeys,
			ExpectedSource: expected,
			Progress:       func(r int64) { progress(r, "rows read") },
		})
		result := TransformResult{OutputPath: dst, RecordsRead: sum.RecordsRead, RecordsWritten: sum.RecordsWritten, Note: "JSON Lines"}
		if err != nil {
			return transformResultAfterPublication(result, err)
		}
		return result, nil
	})
}

// CsvExportSQLiteViaDialog is disabled until the SQLite driver can write to an
// operation-owned atomic handle without mutable scratch or journal pathnames.
// Return before opening a save dialog or touching any destination path.
func (s *FileService) CsvExportSQLiteViaDialog(fileID string, sourceGeneration uint64, delimiter string, hasHeader bool, tableName string, typedCells bool) (TransformResult, error) {
	return TransformResult{}, csv.ErrSQLiteExportSecurePublicationUnavailable
}

// CsvExportXLSXViaDialog is disabled until the streaming XLSX writer supports
// operation-owned scratch handles. Return before a dialog or filesystem access.
func (s *FileService) CsvExportXLSXViaDialog(fileID string, sourceGeneration uint64, delimiter string, hasHeader bool, sheetName string, typedCells bool) (TransformResult, error) {
	return TransformResult{}, csv.ErrXLSXExportSecureScratchUnavailable
}

// CsvMarkdownPreview returns the current preview as a Markdown table (for copy).
func (s *FileService) CsvMarkdownPreview(fileID, delimiter string, hasHeader bool, maxRows int) (CsvTextPreviewResult, error) {
	parsedDelimiter, err := parseCSVDelimiter(delimiter)
	if err != nil {
		return CsvTextPreviewResult{}, err
	}
	maxRows, err = normalizeCSVPreviewRows(maxRows)
	if err != nil {
		return CsvTextPreviewResult{}, err
	}
	sample, err := s.csvSample(fileID)
	if err != nil {
		return CsvTextPreviewResult{}, err
	}
	data, _, err := prepareCSVSample(context.Background(), sample, parsedDelimiter)
	if err != nil {
		return CsvTextPreviewResult{}, err
	}
	rep, err := csv.PreviewRowsContext(context.Background(), bytes.NewReader(data), csv.PreviewOptions{
		Delimiter: parsedDelimiter,
		HasHeader: hasHeader,
		MaxBytes:  decodedSampleLimit(data),
		MaxRows:   maxRows,
	})
	if err != nil {
		return CsvTextPreviewResult{}, err
	}
	return CsvTextPreviewResult{Generation: sample.generation, Text: csv.MarkdownPreview(rep.Header, rep.Rows)}, nil
}

func normalizeCSVPreviewRows(requested int) (int, error) {
	if requested < 0 {
		return 0, fmt.Errorf("CSV preview row limit %d is negative", requested)
	}
	if requested == 0 {
		return csvPreviewDefaultRows, nil
	}
	if requested > csvPreviewMaxRows {
		return 0, fmt.Errorf("CSV preview row limit %d exceeds maximum %d", requested, csvPreviewMaxRows)
	}
	return requested, nil
}

// CsvToSQLConfigPreview returns a short sample of the SQL for a full config.
func (s *FileService) CsvToSQLConfigPreview(fileID string, cfg CsvSqlConfig) (CsvTextPreviewResult, error) {
	opts, err := csvSqlOptions(cfg)
	if err != nil {
		return CsvTextPreviewResult{}, err
	}
	sample, err := s.csvSample(fileID)
	if err != nil {
		return CsvTextPreviewResult{}, err
	}
	data, _, err := prepareCSVSample(context.Background(), sample, opts.Delimiter)
	if err != nil {
		return CsvTextPreviewResult{}, err
	}
	rep, err := csv.PreviewSQLConversionContext(context.Background(), bytes.NewReader(data), csv.SQLPreviewOptions{
		SQLConvertOptions: opts,
		MaxBytes:          decodedSampleLimit(data),
		MaxRows:           20,
	})
	if err != nil {
		return CsvTextPreviewResult{}, err
	}
	return CsvTextPreviewResult{Generation: sample.generation, Text: rep.SQL}, nil
}

// CsvToSQLConfigViaDialog streams the whole CSV to a .sql file using a full
// column-mapping config (select/rename/type, insert mode, batch, null, policy).
func (s *FileService) CsvToSQLConfigViaDialog(fileID string, sourceGeneration uint64, cfg CsvSqlConfig) (TransformResult, error) {
	opts, err := csvSqlOptions(cfg)
	if err != nil {
		return TransformResult{}, err
	}
	if err := s.csvTransformPreflight(fileID, sourceGeneration); err != nil {
		return TransformResult{}, err
	}
	dst, err := csvSaveDialog("Save SQL as", sqlTableName(cfg.TableName)+".sql")
	if err != nil || dst == "" {
		return TransformResult{}, err
	}
	return s.withCSVTransformJob(fileID, sourceGeneration, "Convert CSV to SQL", func(ctx context.Context, file *session.File, _ func(int64, string)) (TransformResult, error) {
		expected, err := expectedCSVSource(ctx, file)
		if err != nil {
			return TransformResult{}, err
		}
		opts.ExpectedSource = expected
		sum, err := csv.ConvertToSQLFile(ctx, file.Path, dst, opts)
		note := fmt.Sprintf("table %q · %d rows", sum.TableName, sum.RowsWritten)
		if sum.SkippedRows > 0 || sum.SanitizedRows > 0 {
			note += fmt.Sprintf(" (%d skipped, %d sanitized)", sum.SkippedRows, sum.SanitizedRows)
		}
		result := TransformResult{OutputPath: dst, RecordsRead: sum.RecordsRead, RecordsWritten: sum.RowsWritten, Note: note}
		if err != nil {
			return transformResultAfterPublication(result, err)
		}
		return result, nil
	})
}
