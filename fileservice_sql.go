package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/quarry/quarry-wails3/internal/document"
	"github.com/quarry/quarry-wails3/internal/exportx"
	sqlanalyze "github.com/quarry/quarry-wails3/internal/plugins/sql/analyze"
	sqlextract "github.com/quarry/quarry-wails3/internal/plugins/sql/extract"
	sqlpreset "github.com/quarry/quarry-wails3/internal/plugins/sql/preset"
	"github.com/quarry/quarry-wails3/internal/replace"
	"github.com/quarry/quarry-wails3/internal/session"
)

// SqlTable is one discovered table in a dump.
type SqlTable struct {
	Name         string `json:"name"`
	CreateOffset int64  `json:"createOffset"`
	InsertOffset int64  `json:"insertOffset"`
	Bytes        int64  `json:"bytes"` // approx byte size of the table's region in the dump
}

// SqlSummaryResult is the result of analysing a SQL dump.
type SqlSummaryResult struct {
	Tables       []SqlTable `json:"tables"`
	CreateTables int        `json:"createTables"`
	InsertTables int        `json:"insertTables"`
	DefinerCount int        `json:"definerCount"`
	Header       bool       `json:"header"`
}

// SqlAnalyze streams a single pass over the dump to discover tables and stats,
// caching the result for later extraction. This is a full pass — the UI should
// show progress for huge files.
func (s *FileService) SqlAnalyze(fileID string) (SqlSummaryResult, error) {
	f, ok := s.reg.Get(fileID)
	if !ok {
		return SqlSummaryResult{}, fmt.Errorf("unknown file id %q", fileID)
	}
	// Reuse the document's already-open descriptor (it satisfies ReaderAtSize)
	// instead of AnalyzeFile reopening the path. NOTE: this still reads the whole
	// file once; fusing the analyze pass with line indexing to avoid the second
	// full read is a separate, larger change.
	summary, err := sqlanalyze.Analyze(context.Background(), f.Doc, sqlanalyze.Options{})
	if err != nil {
		return SqlSummaryResult{}, err
	}
	s.sqlMu.Lock()
	s.sqlSummary[fileID] = summary
	s.sqlMu.Unlock()

	// Per-table byte size, from contiguous ranges (start of this table → start of
	// the next). PlanTableRanges errors when no offsets are known, so guard it.
	sizeByName := map[string]int64{}
	if ranges, perr := sqlextract.PlanTableRanges(summary, f.Doc.Size(), sqlextract.PlanOptions{}); perr == nil {
		for _, r := range ranges {
			sizeByName[r.Name] = r.Bytes
		}
	}

	tables := make([]SqlTable, 0, len(summary.Tables))
	for _, t := range summary.Tables {
		tables = append(tables, SqlTable{Name: t.Name, CreateOffset: t.CreateOffset, InsertOffset: t.InsertOffset, Bytes: sizeByName[t.Name]})
	}
	return SqlSummaryResult{
		Tables:       tables,
		CreateTables: summary.CreateTables,
		InsertTables: summary.InsertTables,
		DefinerCount: summary.DefinerCount,
		Header:       summary.MysqldumpHeader,
	}, nil
}

// SqlExtractTableViaDialog writes one table's byte range to a chosen output
// file (streamed; never materializes the whole dump).
func (s *FileService) SqlExtractTableViaDialog(fileID string, tableName string) (TransformResult, error) {
	f, ok := s.reg.Get(fileID)
	if !ok {
		return TransformResult{}, fmt.Errorf("unknown file id %q", fileID)
	}
	s.sqlMu.Lock()
	summary, have := s.sqlSummary[fileID]
	s.sqlMu.Unlock()
	if !have {
		return TransformResult{}, errors.New("analyze the dump first")
	}
	rng, err := sqlextract.PlanExtractTable(summary, f.Doc.Size(), tableName, sqlextract.PlanOptions{})
	if err != nil {
		return TransformResult{}, err
	}
	dst, err := saveDialog("Extract table to", safeFileName(tableName)+".sql")
	if err != nil || strings.TrimSpace(dst) == "" {
		return TransformResult{}, err
	}
	sum, err := exportx.ExportByteRange(context.Background(), f.Doc, f.Path, dst, rng.StartOffset, rng.EndOffset, exportx.Options{})
	if err != nil {
		return TransformResult{}, err
	}
	return TransformResult{
		OutputPath:     dst,
		RecordsWritten: 1,
		Note:           fmt.Sprintf("%s — %s", tableName, fmtByteCount(sum.BytesWritten)),
	}, nil
}

// SqlSplitByTableViaDialog writes one .sql file per table into a chosen folder.
func (s *FileService) SqlSplitByTableViaDialog(fileID string) (TransformResult, error) {
	f, summary, err := s.sqlSummaryFor(fileID)
	if err != nil {
		return TransformResult{}, err
	}
	dir, err := dirDialog("Choose a folder for the per-table files")
	if err != nil || strings.TrimSpace(dir) == "" {
		return TransformResult{}, err
	}
	sum, err := sqlextract.SplitByTable(context.Background(), f.Doc, f.Path, summary,
		sqlextract.WriteOptions{PlanOptions: sqlextract.PlanOptions{OutputDir: dir}})
	if err != nil {
		return TransformResult{}, err
	}
	return TransformResult{
		OutputPath:     dir,
		RecordsWritten: int64(len(sum.Outputs)),
		Note:           fmt.Sprintf("%d tables · %s total", len(sum.Outputs), fmtByteCount(sum.BytesWritten)),
	}, nil
}

// SqlExtractSchemaViaDialog writes just the DDL (CREATE TABLE / structure) with
// no INSERT data. An empty tableName extracts the whole dump's schema.
func (s *FileService) SqlExtractSchemaViaDialog(fileID, tableName string) (TransformResult, error) {
	f, summary, err := s.sqlSummaryFor(fileID)
	if err != nil {
		return TransformResult{}, err
	}
	ranges, err := sqlextract.PlanTableRanges(summary, f.Doc.Size(), sqlextract.PlanOptions{})
	if err != nil {
		return TransformResult{}, err
	}

	var regions [][2]int64
	defName := "schema.sql"
	if strings.TrimSpace(tableName) == "" {
		// Whole-dump schema: leading preamble (SET NAMES …) + each table's DDL.
		if len(ranges) > 0 && ranges[0].StartOffset > 0 {
			regions = append(regions, [2]int64{0, ranges[0].StartOffset})
		}
		for _, r := range ranges {
			regions = append(regions, schemaRegion(r))
		}
	} else {
		r, ok := findRange(ranges, tableName)
		if !ok {
			return TransformResult{}, fmt.Errorf("table %q was not discovered", tableName)
		}
		regions = append(regions, schemaRegion(r))
		defName = safeFileName(tableName) + ".schema.sql"
	}

	dst, err := saveDialog("Save schema as", defName)
	if err != nil || strings.TrimSpace(dst) == "" {
		return TransformResult{}, err
	}
	written, err := exportRanges(f.Doc, dst, regions)
	if err != nil {
		return TransformResult{}, err
	}
	return TransformResult{OutputPath: dst, RecordsWritten: int64(len(regions)), Note: "schema only — " + fmtByteCount(written)}, nil
}

// SqlExtractDataViaDialog writes just the INSERT rows for a table (no DDL).
func (s *FileService) SqlExtractDataViaDialog(fileID, tableName string) (TransformResult, error) {
	f, summary, err := s.sqlSummaryFor(fileID)
	if err != nil {
		return TransformResult{}, err
	}
	ranges, err := sqlextract.PlanTableRanges(summary, f.Doc.Size(), sqlextract.PlanOptions{})
	if err != nil {
		return TransformResult{}, err
	}
	r, ok := findRange(ranges, tableName)
	if !ok {
		return TransformResult{}, fmt.Errorf("table %q was not discovered", tableName)
	}
	region := dataRegion(r)
	if region[1] <= region[0] {
		return TransformResult{}, fmt.Errorf("table %q has no INSERT data", tableName)
	}
	dst, err := saveDialog("Save data as", safeFileName(tableName)+".data.sql")
	if err != nil || strings.TrimSpace(dst) == "" {
		return TransformResult{}, err
	}
	written, err := exportRanges(f.Doc, dst, [][2]int64{region})
	if err != nil {
		return TransformResult{}, err
	}
	return TransformResult{OutputPath: dst, RecordsWritten: 1, Note: "data only — " + fmtByteCount(written)}, nil
}

// sqlSummaryFor returns the file and its cached analysis, or an error to analyze first.
func (s *FileService) sqlSummaryFor(fileID string) (*session.File, sqlanalyze.Summary, error) {
	f, ok := s.reg.Get(fileID)
	if !ok {
		return nil, sqlanalyze.Summary{}, fmt.Errorf("unknown file id %q", fileID)
	}
	s.sqlMu.Lock()
	summary, have := s.sqlSummary[fileID]
	s.sqlMu.Unlock()
	if !have {
		return nil, sqlanalyze.Summary{}, errors.New("analyze the dump first")
	}
	return f, summary, nil
}

func findRange(ranges []sqlextract.TableRange, name string) (sqlextract.TableRange, bool) {
	for _, r := range ranges {
		if r.Name == name {
			return r, true
		}
	}
	return sqlextract.TableRange{}, false
}

// schemaRegion is a table's DDL byte range (CREATE … up to the first INSERT).
func schemaRegion(r sqlextract.TableRange) [2]int64 {
	start := r.StartOffset
	if r.CreateOffset >= 0 {
		start = r.CreateOffset
	}
	end := r.EndOffset
	if r.InsertOffset > start && r.InsertOffset <= r.EndOffset {
		end = r.InsertOffset
	}
	return [2]int64{start, end}
}

// dataRegion is a table's INSERT byte range (first INSERT to the next table).
func dataRegion(r sqlextract.TableRange) [2]int64 {
	start := r.StartOffset
	if r.InsertOffset >= start {
		start = r.InsertOffset
	}
	return [2]int64{start, r.EndOffset}
}

// exportRanges streams the given byte ranges of doc, in order, into one file.
func exportRanges(doc *document.FileDocument, dst string, ranges [][2]int64) (int64, error) {
	out, err := os.Create(dst)
	if err != nil {
		return 0, err
	}
	defer out.Close()
	bw := bufio.NewWriterSize(out, 1<<20)
	var total int64
	for _, rg := range ranges {
		if rg[1] <= rg[0] {
			continue
		}
		n, err := io.Copy(bw, io.NewSectionReader(doc, rg[0], rg[1]-rg[0]))
		total += n
		if err != nil {
			return total, err
		}
	}
	if err := bw.Flush(); err != nil {
		return total, err
	}
	return total, out.Sync()
}

// SqlReplaceViaDialog streams a find/replace (plain or regex) to a new file —
// also used for table-prefix renames. The source is never modified.
func (s *FileService) SqlReplaceViaDialog(fileID, find, replaceWith string, regex, caseInsensitive, wholeWord bool) (TransformResult, error) {
	f, ok := s.reg.Get(fileID)
	if !ok {
		return TransformResult{}, fmt.Errorf("unknown file id %q", fileID)
	}
	if strings.TrimSpace(find) == "" {
		return TransformResult{}, errors.New("enter text to find")
	}
	dst, err := saveDialog("Save replaced copy as", "replaced.sql")
	if err != nil || strings.TrimSpace(dst) == "" {
		return TransformResult{}, err
	}
	rules := []replace.BatchRule{{Name: "find-replace", Find: []byte(find), Replace: []byte(replaceWith)}}

	var sum replace.FileSummary
	if regex {
		sum, err = replace.ReplaceBatchRegexpFile(context.Background(), f.Path, dst, rules,
			replace.FileOptions{CaseInsensitive: caseInsensitive, WholeWord: wholeWord},
			replace.RegexOptions{CaseInsensitive: caseInsensitive})
	} else {
		sum, err = replace.ReplaceBatchPlainFile(context.Background(), f.Path, dst, rules,
			replace.FileOptions{CaseInsensitive: caseInsensitive, WholeWord: wholeWord},
			replace.BatchOptions{CaseInsensitive: caseInsensitive, WholeWord: wholeWord})
	}
	if err != nil {
		return TransformResult{}, err
	}
	return TransformResult{
		OutputPath:     dst,
		RecordsWritten: sum.Matches,
		Note:           fmt.Sprintf("%d replacements", sum.Matches),
	}, nil
}

// SqlListPresets returns the available SQL cleanup preset names.
func (s *FileService) SqlListPresets() []string {
	return append([]string(nil), sqlpreset.PresetNames...)
}

// SqlApplyPresetViaDialog runs a named cleanup preset, streaming the result to a
// chosen file. Presets that need arguments (e.g. change-database) take them via
// a1..a4; the source is never modified.
func (s *FileService) SqlApplyPresetViaDialog(fileID, name, a1, a2, a3, a4 string) (TransformResult, error) {
	f, ok := s.reg.Get(fileID)
	if !ok {
		return TransformResult{}, fmt.Errorf("unknown file id %q", fileID)
	}
	cfg, err := sqlpreset.Build(name, a1, a2, a3, a4)
	if err != nil {
		return TransformResult{}, err
	}
	dst, err := saveDialog("Save cleaned SQL as", "cleaned.sql")
	if err != nil || strings.TrimSpace(dst) == "" {
		return TransformResult{}, err
	}
	ci := !cfg.CaseSensitive
	var sum replace.FileSummary
	switch cfg.Mode {
	case sqlpreset.ModeRegex:
		rules := []replace.BatchRule{{Name: name, Find: []byte(cfg.Search), Replace: []byte(cfg.Replace)}}
		sum, err = replace.ReplaceBatchRegexpFile(context.Background(), f.Path, dst, rules,
			replace.FileOptions{CaseInsensitive: ci, WholeWord: cfg.WholeWord},
			replace.RegexOptions{CaseInsensitive: ci})
	case sqlpreset.ModeRegexBatch:
		sum, err = replace.ReplaceBatchRegexpFile(context.Background(), f.Path, dst, cfg.BatchRules,
			replace.FileOptions{CaseInsensitive: ci, WholeWord: cfg.WholeWord},
			replace.RegexOptions{CaseInsensitive: ci})
	case sqlpreset.ModeBatch:
		sum, err = replace.ReplaceBatchPlainFile(context.Background(), f.Path, dst, cfg.BatchRules,
			replace.FileOptions{CaseInsensitive: ci, WholeWord: cfg.WholeWord},
			replace.BatchOptions{CaseInsensitive: ci, WholeWord: cfg.WholeWord})
	default: // plain
		rules := []replace.BatchRule{{Name: name, Find: []byte(cfg.Search), Replace: []byte(cfg.Replace)}}
		sum, err = replace.ReplaceBatchPlainFile(context.Background(), f.Path, dst, rules,
			replace.FileOptions{CaseInsensitive: ci, WholeWord: cfg.WholeWord},
			replace.BatchOptions{CaseInsensitive: ci, WholeWord: cfg.WholeWord})
	}
	if err != nil {
		return TransformResult{}, err
	}
	return TransformResult{OutputPath: dst, RecordsWritten: sum.Matches, Note: cfg.Summary}, nil
}

func safeFileName(name string) string {
	name = strings.TrimSpace(name)
	repl := strings.NewReplacer("/", "_", "\\", "_", ":", "_", "*", "_", "?", "_", "\"", "_", "<", "_", ">", "_", "|", "_", "`", "")
	name = repl.Replace(name)
	if name == "" {
		return "table"
	}
	return name
}

func fmtByteCount(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGT"[exp])
}
