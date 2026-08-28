package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"

	"github.com/quarry/quarry-wails3/internal/document"
	"github.com/quarry/quarry-wails3/internal/fileio"
	sqlanalyze "github.com/quarry/quarry-wails3/internal/plugins/sql/analyze"
	sqlextract "github.com/quarry/quarry-wails3/internal/plugins/sql/extract"
	sqlreshape "github.com/quarry/quarry-wails3/internal/plugins/sql/reshape"
	sqlschemadiff "github.com/quarry/quarry-wails3/internal/plugins/sql/schemadiff"
	"github.com/quarry/quarry-wails3/internal/replace"
	"github.com/quarry/quarry-wails3/internal/session"
	"github.com/quarry/quarry-wails3/internal/sourceio"
	"github.com/quarry/quarry-wails3/internal/units"
)

var (
	ErrSQLCleanupPresetsDisabled  = errors.New("SQL cleanup presets are disabled until token-aware transformations are available; generic SQL replacement is also disabled until structured and serialized values can be preserved")
	ErrSQLRegexReplaceUnsupported = errors.New("regex SQL replacement is unavailable for serialization-aware replacement; use plain find/replace so PHP/WordPress byte lengths can be recalculated")
	ErrSQLAnalysisRequired        = errors.New("analyze the dump first")
	ErrSQLAnalysisStale           = errors.New("SQL analysis is stale; analyze the current dump again")
	ErrSQLAnalysisOptionsMismatch = errors.New("SQL analysis options do not match; analyze the dump again with the required options")
	ErrSQLSchemaDiffBudget        = errors.New("SQL schema diff exceeds its bounded input or retained-memory budget")
	ErrSQLReshapeModeInvalid      = errors.New("unsupported reshape mode")
	ErrSQLSampleRowsInvalid       = errors.New("SQL fixture rows per table must not be negative")
)

// sqlAnalysisAfterDialogError distinguishes a cache that was never populated
// from one invalidated while an export dialog was open. Once preflight has
// succeeded, a missing cache means the approved generation is no longer
// available and callers must not silently treat the operation as a first use.
func sqlAnalysisAfterDialogError(err error) error {
	if errors.Is(err, ErrSQLAnalysisRequired) {
		return errors.Join(ErrSQLAnalysisStale, err)
	}
	return err
}

var (
	sqlSaveDialog = saveDialog
	sqlDirDialog  = dirDialog
)

var sqlSchemaDiffReadHook struct {
	sync.RWMutex
	fn func(context.Context, string, int64, int64) error
}

func notifySQLSchemaDiffRead(ctx context.Context, fileID string, start, end int64) error {
	sqlSchemaDiffReadHook.RLock()
	hook := sqlSchemaDiffReadHook.fn
	sqlSchemaDiffReadHook.RUnlock()
	if hook == nil {
		return nil
	}
	return hook(ctx, fileID, start, end)
}

func installSQLSchemaDiffReadHook(hook func(context.Context, string, int64, int64) error) func() {
	sqlSchemaDiffReadHook.Lock()
	previous := sqlSchemaDiffReadHook.fn
	sqlSchemaDiffReadHook.fn = hook
	sqlSchemaDiffReadHook.Unlock()
	return func() {
		sqlSchemaDiffReadHook.Lock()
		sqlSchemaDiffReadHook.fn = previous
		sqlSchemaDiffReadHook.Unlock()
	}
}

const defaultSQLAnalysisChunkSize = 16 * 1024 * 1024

type sqlAnalysisOptionsKey struct {
	ChunkSize int
}

type sqlAnalysisEntry struct {
	Summary    sqlanalyze.Summary
	FileID     string
	Path       string
	Document   *document.FileDocument
	Generation uint64
	Source     document.FileState
	Options    sqlAnalysisOptionsKey
	LastUsed   uint64
}

// sqlAnalysisOwner is the immutable identity approved before a native output
// dialog opens. It deliberately excludes the potentially large Summary while
// retaining every field that binds cached byte offsets to one analyzed source
// generation and analyzer configuration.
type sqlAnalysisOwner struct {
	FileID     string
	Path       string
	Document   *document.FileDocument
	Generation uint64
	Source     document.FileState
	Options    sqlAnalysisOptionsKey
}

func ownerOfSQLAnalysis(entry sqlAnalysisEntry) sqlAnalysisOwner {
	return sqlAnalysisOwner{
		FileID:     entry.FileID,
		Path:       entry.Path,
		Document:   entry.Document,
		Generation: entry.Generation,
		Source:     entry.Source,
		Options:    entry.Options,
	}
}

func (owner sqlAnalysisOwner) matches(entry sqlAnalysisEntry) bool {
	return owner.FileID == entry.FileID &&
		owner.Path == entry.Path &&
		owner.Document == entry.Document &&
		owner.Generation == entry.Generation &&
		owner.Options == entry.Options &&
		owner.Source.Equal(entry.Source)
}

// Retaining a full 10,000-table analysis for every open tab would make total
// memory scale without bound. Older summaries are safe to evict because every
// consumer already requires a current cached generation and can ask the user
// to analyze again.
const maxCachedSQLSummaries = 4

type sqlAnalysisRun struct {
	ID         uint64
	Generation uint64
	Options    sqlAnalysisOptionsKey
	Cancel     context.CancelFunc
}

type sqlAnalysisLease struct {
	file      session.FileSnapshot
	entry     sqlAnalysisEntry
	operation *session.Lease
}

func (l sqlAnalysisLease) Release() {
	if l.operation != nil {
		l.operation.Release()
	}
}

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

// SqlAnalyze captures an exact verification fingerprint, then streams analysis
// through a bounded verified reader and caches the result for later extraction.
// The extra full pass is intentional: cached byte offsets must never describe a
// transient source generation.
func (s *FileService) SqlAnalyze(fileID string) (SqlSummaryResult, error) {
	return runServiceJob(s, jobSpec{
		Title:  "Analyze SQL dump",
		Kind:   jobKindSQLAnalysis,
		FileID: fileID,
	}, func(ctx context.Context, progress func(int64, int64, string)) (SqlSummaryResult, error) {
		return s.sqlAnalyzeWithOptions(ctx, fileID, sqlanalyze.Options{
			Progress: func(p sqlanalyze.Progress) {
				progress(p.BytesProcessed, p.BytesTotal, "bytes analyzed")
			},
		})
	})
}

func (s *FileService) sqlAnalyzeWithOptions(ctx context.Context, fileID string, opts sqlanalyze.Options) (SqlSummaryResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	operationLease, _, err := s.acquireReadFileContext(ctx, fileID)
	if err != nil {
		return SqlSummaryResult{}, err
	}
	defer operationLease.Release()
	file := operationLease.Snapshot()
	notifyServiceLease("sql-analyze", fileID, operationLease)
	if err := s.validateSQLFileGeneration(file); err != nil {
		s.invalidateSQLAnalysisGeneration(fileID, file.Generation)
		return SqlSummaryResult{}, err
	}
	options := sqlAnalysisKey(opts)
	opts.ChunkSize = options.ChunkSize
	analysisCtx, cancel := context.WithCancel(ctx)

	s.sqlMu.Lock()
	if s.sqlSummary == nil {
		s.sqlSummary = make(map[string]sqlAnalysisEntry)
	}
	if s.sqlRuns == nil {
		s.sqlRuns = make(map[string]sqlAnalysisRun)
	}
	previous := s.sqlRuns[fileID]
	s.sqlRunSeq++
	runID := s.sqlRunSeq
	s.sqlRuns[fileID] = sqlAnalysisRun{
		ID:         runID,
		Generation: file.Generation,
		Options:    options,
		Cancel:     cancel,
	}
	s.sqlMu.Unlock()
	if previous.Cancel != nil {
		previous.Cancel()
	}
	defer func() {
		cancel()
		s.sqlMu.Lock()
		if current, exists := s.sqlRuns[fileID]; exists && current.ID == runID {
			delete(s.sqlRuns, fileID)
		}
		s.sqlMu.Unlock()
	}()
	// Capture one exact source generation, then let the analyzer see bytes only
	// through block verification against that capture. The first pass is an
	// intentional safety cost: raw ReaderAt calls could otherwise consume a
	// transient same-size rewrite and cache poisoned extraction offsets even if
	// the pathname and visible mtime were restored before the final check.
	expected, err := sourceio.ExpectDocumentContext(analysisCtx, file.Doc)
	if err != nil {
		if generationErr := s.validateSQLFileGeneration(file); generationErr != nil {
			s.invalidateSQLAnalysisGeneration(fileID, file.Generation)
			return SqlSummaryResult{}, errors.Join(err, generationErr)
		}
		return SqlSummaryResult{}, errors.Join(ErrSQLAnalysisStale, err)
	}
	verifiedSource, err := sourceio.NewVerifiedDocumentReader(analysisCtx, expected, file.Doc)
	if err != nil {
		if generationErr := s.validateSQLFileGeneration(file); generationErr != nil {
			s.invalidateSQLAnalysisGeneration(fileID, file.Generation)
			return SqlSummaryResult{}, errors.Join(err, generationErr)
		}
		return SqlSummaryResult{}, errors.Join(ErrSQLAnalysisStale, err)
	}
	// Reuse the document's already-open descriptor (it satisfies ReaderAtSize)
	// instead of AnalyzeFile reopening the path. The verified view retains one
	// source block at a time and never returns bytes whose captured digest does
	// not match, preserving bounded memory and exact cache ownership.
	summary, err := sqlanalyze.Analyze(analysisCtx, verifiedSource, opts)
	if err != nil {
		if generationErr := s.validateSQLFileGeneration(file); generationErr != nil {
			s.invalidateSQLAnalysisGeneration(fileID, file.Generation)
			return SqlSummaryResult{}, errors.Join(ErrSQLAnalysisStale, err, generationErr)
		}
		return SqlSummaryResult{}, err
	}
	if err := s.validateSQLFileGeneration(file); err != nil {
		s.invalidateSQLAnalysisGeneration(fileID, file.Generation)
		return SqlSummaryResult{}, err
	}
	if err := analysisCtx.Err(); err != nil {
		return SqlSummaryResult{}, err
	}
	entry := sqlAnalysisEntry{
		Summary:    summary,
		FileID:     fileID,
		Path:       file.Path,
		Document:   file.Doc,
		Generation: file.Generation,
		Source:     file.Doc.OriginalFileState(),
		Options:    options,
	}
	// Per-table byte size is the sum of exact owned statement regions.
	// PlanTableRanges errors when no offsets are known, so guard it.
	sizeByName := map[string]int64{}
	if ranges, perr := sqlextract.PlanTableRanges(summary, file.Doc.Size(), sqlextract.PlanOptions{}); perr == nil {
		for _, r := range ranges {
			sizeByName[r.Name] = r.Bytes
		}
	}

	tables := make([]SqlTable, 0, len(summary.Tables))
	for _, t := range summary.Tables {
		tables = append(tables, SqlTable{Name: t.Name, CreateOffset: t.CreateOffset, InsertOffset: t.InsertOffset, Bytes: sizeByName[t.Name]})
	}
	result := SqlSummaryResult{
		Tables:       tables,
		CreateTables: summary.CreateTables,
		InsertTables: summary.InsertTables,
		DefinerCount: summary.DefinerCount,
		Header:       summary.MysqldumpHeader,
	}
	// Result construction remains private until the final ownership check. A
	// failed or canceled re-analysis therefore leaves the previous valid cache
	// entry intact and never exposes a partial replacement.
	if err := s.validateSQLFileGeneration(file); err != nil {
		s.invalidateSQLAnalysisGeneration(fileID, file.Generation)
		return SqlSummaryResult{}, err
	}
	if err := analysisCtx.Err(); err != nil {
		return SqlSummaryResult{}, err
	}

	publish := func() bool {
		s.sqlMu.Lock()
		defer s.sqlMu.Unlock()
		currentRun, ok := s.sqlRuns[fileID]
		if !ok || currentRun.ID != runID || currentRun.Generation != file.Generation || currentRun.Options != options || analysisCtx.Err() != nil {
			return false
		}
		s.sqlUseSeq++
		entry.LastUsed = s.sqlUseSeq
		s.sqlSummary[fileID] = entry
		s.evictSQLSummariesLocked(fileID)
		return true
	}
	published := false
	if jobID, ok := serviceJobID(analysisCtx); ok {
		published = s.jobs().commit(jobID, publish)
	} else {
		published = publish()
	}
	if !published {
		if err := analysisCtx.Err(); err != nil {
			return SqlSummaryResult{}, err
		}
		return SqlSummaryResult{}, ErrSQLAnalysisStale
	}
	return result, nil
}

// SqlExtractTableViaDialog writes one table's exact analyzed
// CREATE/INSERT/REPLACE regions to a chosen output file (streamed; never
// materializes the whole dump). Surrounding session and other SQL is omitted.
func (s *FileService) SqlExtractTableViaDialog(fileID string, tableName string) (TransformResult, error) {
	if err := validateSQLTableSelection(tableName, false); err != nil {
		return TransformResult{}, err
	}
	lease, finishPreflight, err := s.sqlSummaryForPreflight(fileID)
	if err != nil {
		return TransformResult{}, err
	}
	_, err = sqlextract.PlanExtractTable(lease.entry.Summary, lease.file.Doc.Size(), tableName, sqlextract.PlanOptions{})
	if err != nil {
		finishPreflight()
		return TransformResult{}, err
	}
	if err := s.validateSQLAnalysisLease(lease); err != nil {
		finishPreflight()
		return TransformResult{}, err
	}
	approved := ownerOfSQLAnalysis(lease.entry)
	finishPreflight()
	dst, err := sqlSaveDialog("Extract table to", safeFileName(tableName)+".sql")
	if err != nil || dst == "" {
		return TransformResult{}, err
	}
	return s.withFileJob(fileID, "Extract SQL table", func(ctx context.Context, progress func(int64, string)) (TransformResult, error) {
		current, err := s.sqlSummaryAfterDialog(ctx, fileID, approved)
		if err != nil {
			return TransformResult{}, sqlAnalysisAfterDialogError(err)
		}
		defer current.Release()
		rng, err := sqlextract.PlanExtractTable(current.entry.Summary, current.file.Doc.Size(), tableName, sqlextract.PlanOptions{})
		if err != nil {
			return TransformResult{}, err
		}
		written, err := s.exportRangesToPathValidated(ctx, current.file.Doc, dst, sqlextract.ByteRanges(rng), progress, func(validateCtx context.Context) error {
			if err := validateCtx.Err(); err != nil {
				return err
			}
			return s.validateSQLAnalysisLease(current)
		})
		result := TransformResult{OutputPath: dst, RecordsWritten: 1, Note: fmt.Sprintf("%s — analyzed regions only — %s", tableName, fmtByteCount(written))}
		if err != nil {
			return transformResultAfterPublication(result, err)
		}
		if err := s.validateSQLAnalysisLease(current); err != nil {
			return result, &fileio.PublicationError{FinalPath: dst, Durable: true, Err: err}
		}
		return result, nil
	})
}

// SqlLintFinding is one issue found in a dump.
type SqlLintFinding struct {
	Severity string `json:"severity"` // info | warn
	Title    string `json:"title"`
	Detail   string `json:"detail"`
}

// SqlLintResult is the dump-linter output (derived from the cached analysis).
type SqlLintResult struct {
	Findings []SqlLintFinding `json:"findings"`
}

// SqlLint reports dump issues from the cached analysis: empty tables, DEFINER
// usage, mixed charsets/collations, and the largest tables. No new file scan.
func (s *FileService) SqlLint(fileID string) (SqlLintResult, error) {
	operationLease, lease, err := s.acquireSQLSummaryLease(fileID)
	if err != nil {
		return SqlLintResult{}, err
	}
	defer operationLease.Release()
	summary := lease.entry.Summary
	var out []SqlLintFinding

	empty := make([]string, 0)
	for _, t := range summary.Tables {
		if t.CreateOffset >= 0 && t.InsertOffset < 0 {
			empty = append(empty, t.Name)
		}
	}
	if len(empty) > 0 {
		out = append(out, SqlLintFinding{Severity: "info", Title: fmt.Sprintf("%d empty tables", len(empty)), Detail: previewList(empty)})
	}
	if summary.DefinerCount > 0 {
		out = append(out, SqlLintFinding{Severity: "warn", Title: fmt.Sprintf("%d DEFINER clauses", summary.DefinerCount), Detail: "Re-import may fail unless the definer user exists. Cleanup presets are unavailable; review this with a SQL- and serialization-aware migration tool."})
	}
	if len(summary.Charsets) > 1 {
		out = append(out, SqlLintFinding{Severity: "warn", Title: "mixed charsets", Detail: mapKeys(summary.Charsets)})
	}
	if len(summary.Collations) > 1 {
		out = append(out, SqlLintFinding{Severity: "info", Title: "mixed collations", Detail: mapKeys(summary.Collations)})
	}
	if ranges, perr := sqlextract.PlanTableRanges(summary, lease.file.Doc.Size(), sqlextract.PlanOptions{}); perr == nil {
		largest := append([]sqlextract.TableRange(nil), ranges...)
		sort.Slice(largest, func(i, j int) bool { return largest[i].Bytes > largest[j].Bytes })
		parts := make([]string, 0, 3)
		for i := 0; i < len(largest) && i < 3; i++ {
			parts = append(parts, fmt.Sprintf("%s (%s)", largest[i].Name, fmtByteCount(largest[i].Bytes)))
		}
		if len(parts) > 0 {
			out = append(out, SqlLintFinding{Severity: "info", Title: "largest tables", Detail: strings.Join(parts, ", ")})
		}
	}
	if len(out) == 0 {
		out = append(out, SqlLintFinding{Severity: "info", Title: "no issues found", Detail: ""})
	}
	if err := s.validateSQLAnalysisLease(lease); err != nil {
		return SqlLintResult{}, err
	}
	return SqlLintResult{Findings: out}, nil
}

func previewList(names []string) string {
	if len(names) > 8 {
		return strings.Join(names[:8], ", ") + fmt.Sprintf(", … (+%d)", len(names)-8)
	}
	return strings.Join(names, ", ")
}

func mapKeys(m map[string]int) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return previewList(keys)
}

// SqlSplitByTableViaDialog writes one .sql file per table into a chosen folder.
func (s *FileService) SqlSplitByTableViaDialog(fileID string) (TransformResult, error) {
	lease, finishPreflight, err := s.sqlSummaryForPreflight(fileID)
	if err != nil {
		return TransformResult{}, err
	}
	if err := s.validateSQLAnalysisLease(lease); err != nil {
		finishPreflight()
		return TransformResult{}, err
	}
	approved := ownerOfSQLAnalysis(lease.entry)
	finishPreflight()
	dir, err := sqlDirDialog("Choose a folder for the per-table files")
	if err != nil || dir == "" {
		return TransformResult{}, err
	}
	return s.withFileJob(fileID, "Split SQL by table", func(ctx context.Context, progress func(int64, string)) (TransformResult, error) {
		current, err := s.sqlSummaryAfterDialog(ctx, fileID, approved)
		if err != nil {
			return TransformResult{}, sqlAnalysisAfterDialogError(err)
		}
		defer current.Release()
		sum, err := sqlextract.SplitByTable(ctx, current.file.Doc, current.file.Path, current.entry.Summary,
			sqlextract.WriteOptions{
				OutputDir: dir,
				Progress: func(done, total int64, outputs int) {
					if progress != nil {
						progress(done, fmt.Sprintf("%d of %d bytes; %d table outputs", done, total, outputs))
					}
				},
				ValidateSource: func(validateCtx context.Context) error {
					if err := validateCtx.Err(); err != nil {
						return err
					}
					return s.validateSQLAnalysisLease(current)
				},
			})
		result, resultErr := sqlSplitTransformResult(dir, sum, err)
		if resultErr != nil {
			return result, resultErr
		}
		if err := s.validateSQLAnalysisLease(current); err != nil {
			return result, fmt.Errorf("split outputs may already exist after the source session changed: %w", err)
		}
		return result, nil
	})
}

func sqlSplitTransformResult(dir string, summary sqlextract.WriteSummary, operationErr error) (TransformResult, error) {
	if operationErr != nil && len(summary.Outputs) == 0 {
		return TransformResult{}, operationErr
	}
	result := TransformResult{
		OutputPath:     dir,
		RecordsWritten: int64(len(summary.Outputs)),
		Note:           fmt.Sprintf("%d tables · analyzed regions only · %s total", len(summary.Outputs), fmtByteCount(summary.BytesWritten)),
	}
	if operationErr == nil {
		return result, nil
	}
	if summary.Complete {
		result.Note += "; completion manifest published with a finalization warning"
	} else {
		result.Note += "; complete table outputs retained from an incomplete split"
	}
	return result, operationErr
}

// SqlExtractSchemaViaDialog writes exact analyzed CREATE regions with no INSERT
// data. An empty tableName selects every discovered table; it does not infer or
// include unrecognized, non-table, or surrounding dump SQL.
func (s *FileService) SqlExtractSchemaViaDialog(fileID, tableName string) (TransformResult, error) {
	if err := validateSQLTableSelection(tableName, true); err != nil {
		return TransformResult{}, err
	}
	lease, finishPreflight, err := s.sqlSummaryForPreflight(fileID)
	if err != nil {
		return TransformResult{}, err
	}
	_, defName, err := planSchemaExtraction(lease.entry.Summary, lease.file.Doc.Size(), tableName)
	approved := ownerOfSQLAnalysis(lease.entry)
	finishPreflight()
	if err != nil {
		return TransformResult{}, err
	}
	dst, err := sqlSaveDialog("Save schema as", defName)
	if err != nil || dst == "" {
		return TransformResult{}, err
	}
	return s.withFileJob(fileID, "Extract SQL schema", func(ctx context.Context, progress func(int64, string)) (TransformResult, error) {
		current, err := s.sqlSummaryAfterDialog(ctx, fileID, approved)
		if err != nil {
			return TransformResult{}, sqlAnalysisAfterDialogError(err)
		}
		defer current.Release()
		regions, _, err := planSchemaExtraction(current.entry.Summary, current.file.Doc.Size(), tableName)
		if err != nil {
			return TransformResult{}, err
		}
		written, err := s.exportRangesToPathValidated(ctx, current.file.Doc, dst, regions, progress, func(validateCtx context.Context) error {
			if err := validateCtx.Err(); err != nil {
				return err
			}
			return s.validateSQLAnalysisLease(current)
		})
		result := TransformResult{OutputPath: dst, RecordsWritten: int64(len(regions)), Note: "CREATE regions only — " + fmtByteCount(written)}
		if err != nil {
			return transformResultAfterPublication(result, err)
		}
		if err := s.validateSQLAnalysisLease(current); err != nil {
			return result, &fileio.PublicationError{FinalPath: dst, Durable: true, Err: err}
		}
		return result, nil
	})
}

// SqlExtractDataViaDialog writes exact analyzed INSERT/REPLACE regions for a
// table (no DDL or surrounding dump SQL).
func (s *FileService) SqlExtractDataViaDialog(fileID, tableName string) (TransformResult, error) {
	if err := validateSQLTableSelection(tableName, false); err != nil {
		return TransformResult{}, err
	}
	lease, finishPreflight, err := s.sqlSummaryForPreflight(fileID)
	if err != nil {
		return TransformResult{}, err
	}
	_, err = planDataExtraction(lease.entry.Summary, lease.file.Doc.Size(), tableName)
	approved := ownerOfSQLAnalysis(lease.entry)
	finishPreflight()
	if err != nil {
		return TransformResult{}, err
	}
	dst, err := sqlSaveDialog("Save data as", safeFileName(tableName)+".data.sql")
	if err != nil || dst == "" {
		return TransformResult{}, err
	}
	return s.withFileJob(fileID, "Extract SQL data", func(ctx context.Context, progress func(int64, string)) (TransformResult, error) {
		current, err := s.sqlSummaryAfterDialog(ctx, fileID, approved)
		if err != nil {
			return TransformResult{}, sqlAnalysisAfterDialogError(err)
		}
		defer current.Release()
		regions, err := planDataExtraction(current.entry.Summary, current.file.Doc.Size(), tableName)
		if err != nil {
			return TransformResult{}, err
		}
		written, err := s.exportRangesToPathValidated(ctx, current.file.Doc, dst, regions, progress, func(validateCtx context.Context) error {
			if err := validateCtx.Err(); err != nil {
				return err
			}
			return s.validateSQLAnalysisLease(current)
		})
		result := TransformResult{OutputPath: dst, RecordsWritten: int64(len(regions)), Note: "INSERT/REPLACE regions only — " + fmtByteCount(written)}
		if err != nil {
			return transformResultAfterPublication(result, err)
		}
		if err := s.validateSQLAnalysisLease(current); err != nil {
			return result, &fileio.PublicationError{FinalPath: dst, Durable: true, Err: err}
		}
		return result, nil
	})
}

func sqlAnalysisKey(opts sqlanalyze.Options) sqlAnalysisOptionsKey {
	// Progress is observational and does not change discovered offsets. Every
	// option that can affect analysis semantics must be added to this comparable
	// key when introduced.
	chunkSize := opts.ChunkSize
	if chunkSize <= 0 {
		chunkSize = defaultSQLAnalysisChunkSize
	}
	return sqlAnalysisOptionsKey{ChunkSize: chunkSize}
}

func (s *FileService) evictSQLSummariesLocked(keepFileID string) {
	for len(s.sqlSummary) > maxCachedSQLSummaries {
		oldestID := ""
		var oldestUse uint64
		for fileID, entry := range s.sqlSummary {
			if fileID == keepFileID {
				continue
			}
			if oldestID == "" || entry.LastUsed < oldestUse {
				oldestID = fileID
				oldestUse = entry.LastUsed
			}
		}
		if oldestID == "" {
			return
		}
		delete(s.sqlSummary, oldestID)
	}
}

// invalidateSQLAnalysis releases cached summary memory and cancels any analysis
// still producing offsets for this file ID. It is safe to call repeatedly.
func (s *FileService) invalidateSQLAnalysis(fileID string) {
	if s == nil {
		return
	}
	s.sqlMu.Lock()
	delete(s.sqlSummary, fileID)
	run := s.sqlRuns[fileID]
	delete(s.sqlRuns, fileID)
	s.sqlMu.Unlock()
	if run.Cancel != nil {
		run.Cancel()
	}
}

// invalidateSQLAnalysisGeneration cannot clear a newer session's cache/run
// when a stale request notices drift after refresh. Whole-ID invalidation is
// reserved for explicit close/refresh ownership transitions.
func (s *FileService) invalidateSQLAnalysisGeneration(fileID string, generation uint64) {
	if s == nil {
		return
	}
	var cancel context.CancelFunc
	s.sqlMu.Lock()
	if entry, ok := s.sqlSummary[fileID]; ok && entry.Generation == generation {
		delete(s.sqlSummary, fileID)
	}
	if run, ok := s.sqlRuns[fileID]; ok && run.Generation == generation {
		cancel = run.Cancel
		delete(s.sqlRuns, fileID)
	}
	s.sqlMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (s *FileService) validateSQLFileGeneration(file session.FileSnapshot) error {
	if !s.reg.IsCurrent(file) {
		return fmt.Errorf("%w: file session %q is closed", ErrSQLAnalysisStale, file.ID)
	}
	if file.Doc == nil || !file.Doc.HasMutationGeneration() {
		return errors.Join(ErrSQLAnalysisStale, sourceio.ErrSourceChanged, sourceio.ErrMutationGenerationMissing)
	}
	if err := validateDocumentSourceForOutput(file.Doc); err != nil {
		return errors.Join(ErrSQLAnalysisStale, err)
	}
	return nil
}

func sameSQLAnalysisOwner(a, b sqlAnalysisEntry) bool {
	return ownerOfSQLAnalysis(a).matches(b)
}

func (s *FileService) validateSQLAnalysisLease(lease sqlAnalysisLease) error {
	if lease.file.ID != lease.entry.FileID ||
		lease.file.Path != lease.entry.Path ||
		lease.file.Doc != lease.entry.Document ||
		lease.file.Generation != lease.entry.Generation {
		s.invalidateSQLAnalysisGeneration(lease.entry.FileID, lease.entry.Generation)
		return fmt.Errorf("%w: cached offsets belong to another file session generation", ErrSQLAnalysisStale)
	}
	if err := s.validateSQLFileGeneration(lease.file); err != nil {
		s.invalidateSQLAnalysisGeneration(lease.entry.FileID, lease.entry.Generation)
		return err
	}
	if !lease.file.Doc.OriginalFileState().Equal(lease.entry.Source) {
		s.invalidateSQLAnalysisGeneration(lease.entry.FileID, lease.entry.Generation)
		return fmt.Errorf("%w: analyzed source state no longer matches the open document", ErrSQLAnalysisStale)
	}
	s.sqlMu.Lock()
	cached, have := s.sqlSummary[lease.entry.FileID]
	current := have && sameSQLAnalysisOwner(cached, lease.entry)
	s.sqlMu.Unlock()
	if !current {
		return ErrSQLAnalysisStale
	}
	return nil
}

// sqlSummaryFor returns an analysis lease that is valid only while its file ID,
// document generation, source identity/state, and normalized analyzer options
// all remain unchanged.
func (s *FileService) sqlSummaryFor(fileID string) (sqlAnalysisLease, error) {
	return s.sqlSummaryForContext(context.Background(), fileID)
}

// sqlSummaryForPreflight owns the validation-only cached-summary lease under a
// cancellable foreground registration and a hard wait timeout. Call cleanup
// before opening the native dialog; the post-dialog job reacquires and
// revalidates the exact summary generation.
func (s *FileService) sqlSummaryForPreflight(fileID string) (sqlAnalysisLease, func(), error) {
	ctx, finish, err := s.beginFilePreflight(fileID)
	if err != nil {
		return sqlAnalysisLease{}, nil, err
	}
	lease, err := s.sqlSummaryForContext(ctx, fileID)
	if err != nil {
		finish()
		return sqlAnalysisLease{}, nil, err
	}
	cleanup := func() {
		lease.Release()
		finish()
	}
	return lease, cleanup, nil
}

func (s *FileService) sqlSummaryForContext(ctx context.Context, fileID string) (sqlAnalysisLease, error) {
	return s.sqlSummaryForOptionsContext(ctx, fileID, sqlanalyze.Options{})
}

// sqlSummaryAfterDialog reacquires the cached analysis and then proves it is
// the exact owner approved before the native dialog. A refresh followed by a
// fast re-analysis is still a different generation and must not silently turn
// an approved old-source export into an unreviewed new-source export.
func (s *FileService) sqlSummaryAfterDialog(ctx context.Context, fileID string, approved sqlAnalysisOwner) (sqlAnalysisLease, error) {
	current, err := s.sqlSummaryForContext(ctx, fileID)
	if err != nil {
		return sqlAnalysisLease{}, sqlAnalysisAfterDialogError(err)
	}
	if !approved.matches(current.entry) {
		current.Release()
		return sqlAnalysisLease{}, fmt.Errorf("%w: analyzed source generation changed while the output dialog was open", ErrSQLAnalysisStale)
	}
	return current, nil
}

// acquireSQLSummaryLease keeps the document generation alive for every use of
// cached byte offsets. Callers must Release the returned Registry lease.
func (s *FileService) acquireSQLSummaryLease(fileID string) (*session.Lease, sqlAnalysisLease, error) {
	analysis, err := s.sqlSummaryFor(fileID)
	if err != nil {
		return nil, sqlAnalysisLease{}, err
	}
	return analysis.operation, analysis, nil
}

func (s *FileService) sqlSummaryForOptions(fileID string, opts sqlanalyze.Options) (sqlAnalysisLease, error) {
	return s.sqlSummaryForOptionsContext(context.Background(), fileID, opts)
}

func (s *FileService) sqlSummaryForOptionsContext(ctx context.Context, fileID string, opts sqlanalyze.Options) (sqlAnalysisLease, error) {
	operationLease, _, err := s.acquireReadFileContext(ctx, fileID)
	if err != nil {
		return sqlAnalysisLease{}, err
	}
	file := operationLease.Snapshot()
	s.sqlMu.Lock()
	entry, have := s.sqlSummary[fileID]
	if have {
		s.sqlUseSeq++
		entry.LastUsed = s.sqlUseSeq
		s.sqlSummary[fileID] = entry
	}
	s.sqlMu.Unlock()
	if !have {
		operationLease.Release()
		return sqlAnalysisLease{}, ErrSQLAnalysisRequired
	}
	if entry.Options != sqlAnalysisKey(opts) {
		operationLease.Release()
		return sqlAnalysisLease{}, ErrSQLAnalysisOptionsMismatch
	}
	lease := sqlAnalysisLease{file: file, entry: entry, operation: operationLease}
	if err := s.validateSQLAnalysisLease(lease); err != nil {
		operationLease.Release()
		return sqlAnalysisLease{}, err
	}
	return lease, nil
}

func findRange(ranges []sqlextract.TableRange, name string) (sqlextract.TableRange, bool) {
	for _, r := range ranges {
		if r.Name == name {
			return r, true
		}
	}
	return sqlextract.TableRange{}, false
}

// schemaRegions returns every DDL block owned by one qualified table identity.
func schemaRegions(r sqlextract.TableRange) [][2]int64 {
	if len(r.Regions) > 0 {
		regions := make([][2]int64, 0, len(r.Regions))
		for _, region := range r.Regions {
			if region.Kind == sqlanalyze.RegionCreate {
				regions = append(regions, [2]int64{region.StartOffset, region.EndOffset})
			}
		}
		return regions
	}
	if r.CreateOffset < r.StartOffset || r.CreateOffset < 0 || r.CreateOffset >= r.EndOffset {
		return nil
	}
	start := r.CreateOffset
	end := r.EndOffset
	if r.InsertOffset > start && r.InsertOffset <= r.EndOffset {
		end = r.InsertOffset
	}
	if end <= start {
		return nil
	}
	return [][2]int64{{start, end}}
}

// dataRegions returns every INSERT/REPLACE block owned by one qualified table.
func dataRegions(r sqlextract.TableRange) [][2]int64 {
	if len(r.Regions) > 0 {
		regions := make([][2]int64, 0, len(r.Regions))
		for _, region := range r.Regions {
			if region.Kind == sqlanalyze.RegionInsert || region.Kind == sqlanalyze.RegionReplace {
				regions = append(regions, [2]int64{region.StartOffset, region.EndOffset})
			}
		}
		return regions
	}
	if r.InsertOffset < r.StartOffset || r.InsertOffset < 0 || r.InsertOffset >= r.EndOffset {
		return nil
	}
	return [][2]int64{{r.InsertOffset, r.EndOffset}}
}

func planSchemaExtraction(summary sqlanalyze.Summary, sourceSize int64, tableName string) ([][2]int64, string, error) {
	ranges, err := sqlextract.PlanTableRanges(summary, sourceSize, sqlextract.PlanOptions{})
	if err != nil {
		return nil, "", err
	}
	regions := make([][2]int64, 0, len(ranges)+1)
	defaultName := "schema.sql"
	if strings.TrimSpace(tableName) == "" {
		schemaCount := 0
		if len(ranges) > 0 && ranges[0].StartOffset > 0 {
			regions = append(regions, [2]int64{0, ranges[0].StartOffset})
		}
		for _, tableRange := range ranges {
			owned := schemaRegions(tableRange)
			regions = append(regions, owned...)
			schemaCount += len(owned)
		}
		if schemaCount == 0 {
			return nil, "", errors.New("no CREATE TABLE schema was discovered")
		}
		sort.SliceStable(regions, func(i, j int) bool { return regions[i][0] < regions[j][0] })
		return regions, defaultName, nil
	}
	tableRange, ok := findRange(ranges, tableName)
	if !ok {
		return nil, "", fmt.Errorf("table %q was not discovered", tableName)
	}
	owned := schemaRegions(tableRange)
	if len(owned) == 0 {
		return nil, "", fmt.Errorf("table %q has no CREATE TABLE schema", tableName)
	}
	return append(regions, owned...), safeFileName(tableName) + ".schema.sql", nil
}

func planDataExtraction(summary sqlanalyze.Summary, sourceSize int64, tableName string) ([][2]int64, error) {
	ranges, err := sqlextract.PlanTableRanges(summary, sourceSize, sqlextract.PlanOptions{})
	if err != nil {
		return nil, err
	}
	tableRange, ok := findRange(ranges, tableName)
	if !ok {
		return nil, fmt.Errorf("table %q was not discovered", tableName)
	}
	regions := dataRegions(tableRange)
	if len(regions) == 0 {
		return nil, fmt.Errorf("table %q has no INSERT or REPLACE data", tableName)
	}
	return regions, nil
}

// transformResultAfterPublication keeps output evidence only when the
// publication error proves that the complete output is visible at the exact
// requested path. An uncertain or different location cannot support that
// claim in the service result.
func transformResultAfterPublication(result TransformResult, err error) (TransformResult, error) {
	if err == nil {
		return result, nil
	}
	var publication *fileio.PublicationError
	if errors.As(err, &publication) &&
		!publication.LocationUncertain &&
		publication.FinalPath == result.OutputPath {
		if result.Note != "" {
			result.Note += "; "
		}
		result.Note += "output published with a finalization warning"
		return result, err
	}
	return TransformResult{}, err
}

// exportRangesToPath streams the given byte ranges into an operation-owned
// sibling temp and publishes only the complete, synced output. It refuses any
// destination that aliases any source currently open in Quarry.
func (s *FileService) exportRangesToPath(ctx context.Context, doc *document.FileDocument, dst string, ranges [][2]int64, progress func(int64, string)) (total int64, retErr error) {
	return s.exportRangesToPathValidated(ctx, doc, dst, ranges, progress, nil)
}

func (s *FileService) exportRangesToPathValidated(ctx context.Context, doc *document.FileDocument, dst string, ranges [][2]int64, progress func(int64, string), validate func(context.Context) error) (total int64, retErr error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if doc == nil {
		return 0, errors.New("document is required")
	}
	out, err := fileio.OpenAtomicOutput(dst, s.reg.Paths(), 0o600)
	if err != nil {
		return 0, err
	}
	defer func() {
		if err := out.Cleanup(); err != nil {
			retErr = errors.Join(retErr, err)
		}
	}()
	expected, err := sourceio.ExpectDocumentContext(ctx, doc)
	if err != nil {
		return 0, err
	}
	verified, err := sourceio.NewVerifiedDocumentReader(ctx, expected, doc)
	if err != nil {
		return 0, err
	}

	bw := bufio.NewWriterSize(out, 1<<20)
	buf := make([]byte, 1<<20)
	for _, rg := range ranges {
		n, err := copyRangeContext(ctx, bw, verified, rg[0], rg[1], buf)
		total += n
		if err != nil {
			return total, err
		}
		if progress != nil {
			progress(total, "bytes written")
		}
	}
	if err := bw.Flush(); err != nil {
		return total, err
	}
	if err := ctx.Err(); err != nil {
		return total, err
	}
	if err := out.CommitContextValidated(ctx, func(validateCtx context.Context) error {
		if validate != nil {
			if err := validate(validateCtx); err != nil {
				return err
			}
			if err := validateCtx.Err(); err != nil {
				return err
			}
		}
		return expected.ValidateDocumentContext(validateCtx, doc)
	}); err != nil {
		return total, err
	}
	return total, nil
}

// SqlSampleFixtureViaDialog writes a small "dev fixture" dump: each table's DDL
// plus only its first rowsPerTable INSERT rows. Turns a prod dump into a tiny,
// shareable seed without ever materialising the whole file.
func (s *FileService) SqlSampleFixtureViaDialog(fileID string, rowsPerTable int) (TransformResult, error) {
	if rowsPerTable < 0 {
		return TransformResult{}, fmt.Errorf("%w: %d", ErrSQLSampleRowsInvalid, rowsPerTable)
	}
	lease, finishPreflight, err := s.sqlSummaryForPreflight(fileID)
	if err != nil {
		return TransformResult{}, err
	}
	if rowsPerTable == 0 {
		rowsPerTable = 100
	}
	if rowsPerTable > maxFixtureRowsPerTable {
		finishPreflight()
		return TransformResult{}, fmt.Errorf("rows per table %d exceeds limit %d", rowsPerTable, maxFixtureRowsPerTable)
	}
	_, err = sqlextract.PlanTableRanges(lease.entry.Summary, lease.file.Doc.Size(), sqlextract.PlanOptions{})
	if err != nil {
		finishPreflight()
		return TransformResult{}, err
	}
	if err := s.validateSQLAnalysisLease(lease); err != nil {
		finishPreflight()
		return TransformResult{}, err
	}
	approved := ownerOfSQLAnalysis(lease.entry)
	finishPreflight()
	dst, err := sqlSaveDialog("Save dev fixture as", "fixture.sql")
	if err != nil || dst == "" {
		return TransformResult{}, err
	}

	return s.withFileJob(fileID, "Sample SQL fixture", func(ctx context.Context, progress func(int64, string)) (TransformResult, error) {
		current, err := s.sqlSummaryAfterDialog(ctx, fileID, approved)
		if err != nil {
			return TransformResult{}, sqlAnalysisAfterDialogError(err)
		}
		defer current.Release()
		ranges, err := sqlextract.PlanTableRanges(current.entry.Summary, current.file.Doc.Size(), sqlextract.PlanOptions{})
		if err != nil {
			return TransformResult{}, err
		}
		result, err := s.sqlSampleFixtureToPathValidated(ctx, current.file.Doc, ranges, rowsPerTable, dst, progress, func(validateCtx context.Context) error {
			if err := validateCtx.Err(); err != nil {
				return err
			}
			return s.validateSQLAnalysisLease(current)
		})
		if err != nil {
			return result, err
		}
		if err := s.validateSQLAnalysisLease(current); err != nil {
			return result, &fileio.PublicationError{FinalPath: dst, Durable: true, Err: err}
		}
		return result, nil
	})
}

func (s *FileService) sqlSampleFixtureToPath(ctx context.Context, doc *document.FileDocument, ranges []sqlextract.TableRange, rowsPerTable int, dst string, progress func(int64, string)) (result TransformResult, retErr error) {
	return s.sqlSampleFixtureToPathValidated(ctx, doc, ranges, rowsPerTable, dst, progress, nil)
}

func (s *FileService) sqlSampleFixtureToPathValidated(ctx context.Context, doc *document.FileDocument, ranges []sqlextract.TableRange, rowsPerTable int, dst string, progress func(int64, string), validate func(context.Context) error) (result TransformResult, retErr error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if doc == nil {
		return TransformResult{}, errors.New("document is required")
	}
	out, err := fileio.OpenAtomicOutput(dst, s.reg.Paths(), 0o600)
	if err != nil {
		return TransformResult{}, err
	}
	defer func() {
		if err := out.Cleanup(); err != nil {
			retErr = errors.Join(retErr, err)
		}
	}()
	expected, err := sourceio.ExpectDocumentContext(ctx, doc)
	if err != nil {
		return TransformResult{}, err
	}
	verified, err := sourceio.NewVerifiedDocumentReader(ctx, expected, doc)
	if err != nil {
		return TransformResult{}, err
	}

	bw := bufio.NewWriterSize(out, 1<<20)
	buf := make([]byte, 1<<20)
	var size int64
	writeRange := func(start, end int64) error {
		n, err := copyRangeContext(ctx, bw, verified, start, end, buf)
		size += n
		return err
	}

	// Leading preamble (SET NAMES / charset) so the fixture re-imports cleanly.
	if len(ranges) > 0 && ranges[0].StartOffset > 0 {
		if err := writeRange(0, ranges[0].StartOffset); err != nil {
			return TransformResult{}, err
		}
	}
	for i, r := range ranges {
		if err := ctx.Err(); err != nil {
			return TransformResult{}, err
		}
		schemas := schemaRegions(r)
		if len(schemas) > 1 {
			return TransformResult{}, fmt.Errorf("table %q has multiple CREATE blocks; fixture sampling cannot preserve their execution order safely", r.Name)
		}
		if len(schemas) == 1 {
			if err := writeRange(schemas[0][0], schemas[0][1]); err != nil {
				return TransformResult{}, err
			}
		}
		remaining := rowsPerTable
		for _, dat := range dataRegions(r) {
			if remaining <= 0 {
				break
			}
			if len(r.Regions) > 0 {
				for _, owned := range r.Regions {
					if owned.StartOffset == dat[0] && owned.EndOffset == dat[1] && owned.Kind == sqlanalyze.RegionReplace {
						return TransformResult{}, fmt.Errorf("table %q uses REPLACE; bounded fixture row sampling supports strict INSERT ... VALUES only", r.Name)
					}
				}
			}
			sample, sampledRows, err := sampleInsertRowsContextCount(ctx, verified, dat[0], dat[1], remaining)
			if err != nil {
				return TransformResult{}, err
			}
			n, err := bw.Write(sample)
			size += int64(n)
			if err != nil {
				return TransformResult{}, err
			}
			if n != len(sample) {
				return TransformResult{}, io.ErrShortWrite
			}
			remaining -= sampledRows
		}
		if progress != nil {
			progress(int64(i+1), "tables sampled")
		}
	}
	if err := bw.Flush(); err != nil {
		return TransformResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return TransformResult{}, err
	}
	result = TransformResult{
		OutputPath:     dst,
		RecordsWritten: int64(len(ranges)),
		Note:           fmt.Sprintf("%d tables · ≤%d rows each · %s", len(ranges), rowsPerTable, fmtByteCount(size)),
	}
	if err := out.CommitContextValidated(ctx, func(validateCtx context.Context) error {
		if validate != nil {
			if err := validate(validateCtx); err != nil {
				return err
			}
			if err := validateCtx.Err(); err != nil {
				return err
			}
		}
		return expected.ValidateDocumentContext(validateCtx, doc)
	}); err != nil {
		return transformResultAfterPublication(result, err)
	}
	return result, nil
}

// copyRangeContext streams doc[start:end) with bounded memory and observes
// cancellation between each read/write chunk.
func copyRangeContext(ctx context.Context, dst io.Writer, doc document.ReaderAtSize, start, end int64, buf []byte) (int64, error) {
	if doc == nil {
		return 0, errors.New("document is required")
	}
	if start < 0 || end < start || end > doc.Size() {
		return 0, fmt.Errorf("invalid copy range [%d,%d) for document size %d", start, end, doc.Size())
	}
	if end == start {
		return 0, nil
	}
	if len(buf) == 0 {
		return 0, errors.New("copy buffer is required")
	}
	reader := io.NewSectionReader(doc, start, end-start)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		n, readErr := reader.Read(buf)
		if n > 0 {
			written, writeErr := dst.Write(buf[:n])
			total += int64(written)
			if writeErr != nil {
				return total, writeErr
			}
			if written != n {
				return total, io.ErrShortWrite
			}
		}
		if errors.Is(readErr, io.EOF) {
			if total != end-start {
				return total, io.ErrUnexpectedEOF
			}
			return total, nil
		}
		if readErr != nil {
			return total, readErr
		}
		if n == 0 {
			return total, io.ErrNoProgress
		}
	}
}

// sampleInsertRows returns the prefix of an INSERT region doc[start:end) that
// contains the first maxRows value tuples, terminated as valid SQL. It counts
// top-level "(...)" tuples, respecting single-quoted strings with backslash and
// doubled-quote escapes, and reads the region in chunks so it stops early.
func sampleInsertRows(r io.ReaderAt, start, end int64, maxRows int) ([]byte, error) {
	return sampleInsertRowsContext(context.Background(), r, start, end, maxRows)
}

const (
	maxFixtureSampleBytes  = 8 << 20
	maxFixtureRowsPerTable = 10_000
)

func sampleInsertRowsContext(ctx context.Context, r io.ReaderAt, start, end int64, maxRows int) ([]byte, error) {
	sample, _, err := sampleInsertRowsContextCount(ctx, r, start, end, maxRows)
	return sample, err
}

func sampleInsertRowsContextCount(ctx context.Context, r io.ReaderAt, start, end int64, maxRows int) ([]byte, int, error) {
	if start < 0 || end < start {
		return nil, 0, fmt.Errorf("invalid INSERT sample range [%d,%d)", start, end)
	}
	if maxRows <= 0 || maxRows > maxFixtureRowsPerTable {
		return nil, 0, fmt.Errorf("sample row limit %d is outside 1..%d", maxRows, maxFixtureRowsPerTable)
	}
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	length := end - start
	readLength := length
	truncated := false
	if readLength > maxFixtureSampleBytes {
		readLength = maxFixtureSampleBytes
		truncated = true
	}
	data := make([]byte, int(readLength))
	n, err := r.ReadAt(data, start)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, 0, err
	}
	if n != len(data) {
		return nil, 0, io.ErrUnexpectedEOF
	}
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	sample, rows, satisfied, err := sqlreshape.SampleInsertRows(data, maxRows)
	if err != nil {
		if truncated && errors.Is(err, sqlreshape.ErrIncompleteStatement) {
			return nil, 0, fmt.Errorf("sampled SQL row prefix exceeds %d-byte memory limit", maxFixtureSampleBytes)
		}
		return nil, 0, err
	}
	if truncated && !satisfied {
		return nil, 0, fmt.Errorf("sampled SQL row prefix exceeds %d-byte memory limit", maxFixtureSampleBytes)
	}
	if rows == 0 {
		return nil, 0, errors.New("INSERT sample contains no complete VALUES tuples")
	}
	return sample, rows, nil
}

// SqlReplaceViaDialog streams a serialization-aware plain replacement to a new
// SQL file. SQL literals are decoded before replacement. Native PHP/WordPress
// serialized values are parsed and their byte lengths are recalculated before
// the literal is re-escaped. Unsupported serialized values fail before publish.
func (s *FileService) SqlReplaceViaDialog(fileID, find, replaceWith string, regex, caseInsensitive, wholeWord bool) (TransformResult, error) {
	if regex {
		return TransformResult{}, ErrSQLRegexReplaceUnsupported
	}
	if err := validateRPCFileID(fileID); err != nil {
		return TransformResult{}, err
	}
	if strings.TrimSpace(find) == "" {
		return TransformResult{}, errors.New("enter text to find")
	}
	preflight, _, finishPreflight, err := s.acquireReadFilePreflight(fileID)
	if err != nil {
		return TransformResult{}, err
	}
	generation := preflight.Snapshot().Generation
	finishPreflight()
	dst, err := sqlSaveDialog("Save replaced SQL copy as", "replaced.sql")
	if err != nil || strings.TrimSpace(dst) == "" {
		return TransformResult{}, err
	}
	return s.withFileJob(fileID, "Replace SQL text", func(ctx context.Context, progress func(int64, string)) (TransformResult, error) {
		operationLease, current, err := s.acquireReadFileContext(ctx, fileID)
		if err != nil {
			return TransformResult{}, err
		}
		defer operationLease.Release()
		if operationLease.Snapshot().Generation != generation {
			return TransformResult{}, fmt.Errorf("%w: file was refreshed while the replace dialog was open", sourceio.ErrSourceChanged)
		}
		expected, err := sourceio.ExpectDocumentContext(ctx, current.Doc)
		if err != nil {
			return TransformResult{}, err
		}
		sum, err := replace.ReplaceSQLPlainFileAtomic(ctx, current.Path, dst, []byte(find), []byte(replaceWith),
			replace.FileOptions{
				ExpectedSource: expected,
			},
			replace.BatchOptions{
				CaseInsensitive: caseInsensitive,
				WholeWord:       wholeWord,
				Progress: func(p replace.Progress) {
					if progress != nil {
						progress(p.BytesProcessed, fmt.Sprintf("%d replacements", p.Matches))
					}
				},
			})
		result := TransformResult{
			OutputPath:     dst,
			RecordsWritten: sum.Matches,
			Note:           fmt.Sprintf("%d replacements; serialized string lengths recalculated where applicable", sum.Matches),
		}
		if err != nil {
			return transformResultAfterPublication(result, err)
		}
		return result, nil
	})
}

// SqlReshapeInsertsViaDialog rewrites INSERT row layout, streaming to a new file.
// mode "single" explodes extended INSERTs into one row each (good for line diffs);
// mode "multi" batches consecutive single-row INSERTs (good for fast re-import).
func (s *FileService) SqlReshapeInsertsViaDialog(fileID, mode string, batchSize int) (TransformResult, error) {
	if err := validateRPCFileID(fileID); err != nil {
		return TransformResult{}, err
	}
	if err := validateRPCEnum(mode); err != nil {
		return TransformResult{}, err
	}
	var m sqlreshape.Mode
	var defName string
	switch mode {
	case "single":
		m = sqlreshape.ModeSingleRow
		defName = "single-row.sql"
	case "multi":
		m = sqlreshape.ModeMultiRow
		defName = "batched.sql"
	default:
		return TransformResult{}, ErrSQLReshapeModeInvalid
	}
	if batchSize > sqlreshape.MaxBatchRows {
		return TransformResult{}, fmt.Errorf("reshape batch size %d exceeds limit %d", batchSize, sqlreshape.MaxBatchRows)
	}
	if batchSize < 0 {
		return TransformResult{}, fmt.Errorf("reshape batch size %d is negative", batchSize)
	}
	preflight, _, finishPreflight, err := s.acquireReadFilePreflight(fileID)
	if err != nil {
		return TransformResult{}, err
	}
	defer finishPreflight()
	generation := preflight.Snapshot().Generation
	finishPreflight()
	dst, err := sqlSaveDialog("Save reshaped SQL as", defName)
	if err != nil || dst == "" {
		return TransformResult{}, err
	}
	return s.withFileJob(fileID, "Reshape SQL INSERTs", func(ctx context.Context, progress func(int64, string)) (TransformResult, error) {
		operationLease, current, err := s.acquireReadFileContext(ctx, fileID)
		if err != nil {
			return TransformResult{}, err
		}
		defer operationLease.Release()
		if operationLease.Snapshot().Generation != generation {
			return TransformResult{}, fmt.Errorf("%w: file was refreshed while the reshape dialog was open", sourceio.ErrSourceChanged)
		}
		expected, err := sourceio.ExpectDocumentContext(ctx, current.Doc)
		if err != nil {
			return TransformResult{}, err
		}
		sum, err := sqlreshape.ReshapeInsertsFile(ctx, current.Path, dst, sqlreshape.Options{
			Mode:           m,
			BatchSize:      batchSize,
			ExpectedSource: expected,
		})
		if progress != nil {
			progress(sum.RowsSeen, "rows reshaped")
		}
		result := TransformResult{
			OutputPath:     dst,
			RecordsRead:    sum.StatementsRead,
			RecordsWritten: sum.StatementsWritten,
			Note:           sqlreshape.FormatNote(m, sum),
		}
		if err != nil {
			return transformResultAfterPublication(result, err)
		}
		return result, nil
	})
}

func validateSQLTableSelection(tableName string, allowEmpty bool) error {
	// Bound raw bridge input before TrimSpace, cached-summary lookup, planning,
	// error formatting, or Unicode filename normalization.
	if len(tableName) > sqlextract.MaxTableSelectionBytes {
		return sqlextract.ErrTableNameTooLong
	}
	if !allowEmpty && strings.TrimSpace(tableName) == "" {
		return errors.New("table name is required")
	}
	return nil
}

// SqlSchemaDiffResult is the structural diff between two dumps.
type SqlSchemaDiffResult struct {
	FileA          string                    `json:"fileA"`
	FileB          string                    `json:"fileB"`
	AddedTables    []string                  `json:"addedTables"`
	RemovedTables  []string                  `json:"removedTables"`
	ChangedTables  []sqlschemadiff.TableDiff `json:"changedTables"`
	UnchangedCount int                       `json:"unchangedCount"`
}

// SqlSchemaDiff compares the table/column structure of two analyzed dumps
// (A = baseline, B = new). Both must be analyzed first.
func (s *FileService) SqlSchemaDiff(fileIDA, fileIDB string) (SqlSchemaDiffResult, error) {
	return runServiceJob(s, jobSpec{
		Title:  "Compare SQL schemas",
		Kind:   "sql-schema-diff",
		FileID: fileIDA,
	}, func(jobCtx context.Context, progress func(int64, int64, string)) (SqlSchemaDiffResult, error) {
		ctx, finish, err := s.beginForegroundRun(jobCtx, fileIDA, fileIDB)
		if err != nil {
			return SqlSchemaDiffResult{}, err
		}
		defer finish()
		return s.sqlSchemaDiff(ctx, fileIDA, fileIDB, progress)
	})
}

func (s *FileService) sqlSchemaDiff(ctx context.Context, fileIDA, fileIDB string, progress func(int64, int64, string)) (SqlSchemaDiffResult, error) {
	if fileIDA == fileIDB {
		file, err := s.sqlSummaryForContext(ctx, fileIDA)
		if err != nil {
			return SqlSchemaDiffResult{}, err
		}
		defer file.Release()
		notifyServiceLease("sql-schema-diff", fileIDA, file.operation)
		plan, err := planSQLSchemaReads(ctx, file)
		if err != nil {
			return SqlSchemaDiffResult{}, err
		}
		budget, err := newSQLSchemaDiffBudget(plan)
		if err != nil {
			return SqlSchemaDiffResult{}, err
		}
		if progress != nil {
			progress(0, plan.inputBytes, "schema bytes planned")
		}
		tables, err := s.parsedSchemaFromLease(ctx, file, plan, budget, progress)
		if err != nil {
			return SqlSchemaDiffResult{}, err
		}
		if err := s.validateSQLAnalysisLease(file); err != nil {
			return SqlSchemaDiffResult{}, err
		}
		result, err := makeSQLSchemaDiffResult(ctx, file, file, tables, tables)
		if err != nil {
			return SqlSchemaDiffResult{}, err
		}
		if err := s.validateSQLAnalysisLease(file); err != nil {
			return SqlSchemaDiffResult{}, err
		}
		if err := s.commitSQLSchemaDiff(ctx); err != nil {
			return SqlSchemaDiffResult{}, err
		}
		return result, nil
	}

	// Every two-file comparison acquires read leases in one stable order. This
	// is required because registry writers have priority: opposing Diff(A,B)
	// and Diff(B,A) requests must never each retain one reader while waiting
	// behind a queued writer for the other file.
	firstID, secondID := fileIDA, fileIDB
	if secondID < firstID {
		firstID, secondID = secondID, firstID
	}
	first, err := s.sqlSummaryForContext(ctx, firstID)
	if err != nil {
		return SqlSchemaDiffResult{}, err
	}
	defer first.Release()
	notifyServiceLease("sql-schema-diff", firstID, first.operation)
	second, err := s.sqlSummaryForContext(ctx, secondID)
	if err != nil {
		return SqlSchemaDiffResult{}, err
	}
	defer second.Release()
	notifyServiceLease("sql-schema-diff", secondID, second.operation)

	fa, fb := first, second
	if fileIDA != firstID {
		fa, fb = second, first
	}
	planA, err := planSQLSchemaReads(ctx, fa)
	if err != nil {
		return SqlSchemaDiffResult{}, err
	}
	planB, err := planSQLSchemaReads(ctx, fb)
	if err != nil {
		return SqlSchemaDiffResult{}, err
	}
	budget, err := newSQLSchemaDiffBudget(planA, planB)
	if err != nil {
		return SqlSchemaDiffResult{}, err
	}
	if progress != nil {
		progress(0, planA.inputBytes+planB.inputBytes, "schema bytes planned")
	}
	ta, err := s.parsedSchemaFromLease(ctx, fa, planA, budget, progress)
	if err != nil {
		return SqlSchemaDiffResult{}, err
	}
	tb, err := s.parsedSchemaFromLease(ctx, fb, planB, budget, progress)
	if err != nil {
		return SqlSchemaDiffResult{}, err
	}
	if err := s.validateSQLAnalysisLease(fa); err != nil {
		return SqlSchemaDiffResult{}, err
	}
	if err := s.validateSQLAnalysisLease(fb); err != nil {
		return SqlSchemaDiffResult{}, err
	}
	result, err := makeSQLSchemaDiffResult(ctx, fa, fb, ta, tb)
	if err != nil {
		return SqlSchemaDiffResult{}, err
	}
	if err := s.validateSQLAnalysisLease(fa); err != nil {
		return SqlSchemaDiffResult{}, err
	}
	if err := s.validateSQLAnalysisLease(fb); err != nil {
		return SqlSchemaDiffResult{}, err
	}
	if err := s.commitSQLSchemaDiff(ctx); err != nil {
		return SqlSchemaDiffResult{}, err
	}
	return result, nil
}

func (s *FileService) commitSQLSchemaDiff(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	jobID, ok := serviceJobID(ctx)
	if !ok {
		return nil
	}
	if !s.jobs().commit(jobID, func() bool { return ctx.Err() == nil }) {
		if err := ctx.Err(); err != nil {
			return err
		}
		return ErrJobCancelled
	}
	return ctx.Err()
}

func makeSQLSchemaDiffResult(ctx context.Context, fa, fb sqlAnalysisLease, ta, tb []sqlschemadiff.Table) (SqlSchemaDiffResult, error) {
	res, err := sqlschemadiff.DiffContext(ctx, ta, tb)
	if err != nil {
		return SqlSchemaDiffResult{}, err
	}
	return SqlSchemaDiffResult{
		FileA:          baseName(fa.file.Path),
		FileB:          baseName(fb.file.Path),
		AddedTables:    res.AddedTables,
		RemovedTables:  res.RemovedTables,
		ChangedTables:  res.ChangedTables,
		UnchangedCount: res.UnchangedCount,
	}, nil
}

const maxSQLSchemaStatementReadBytes = int64(sqlschemadiff.MaxStatementBytes) + 1

type sqlSchemaRead struct {
	name   string
	start  int64
	end    int64
	hasDDL bool
}

type sqlSchemaReadPlan struct {
	reads      []sqlSchemaRead
	inputBytes int64
}

type sqlSchemaDiffBudget struct {
	inputRemaining    int64
	retainedRemaining int64
	inputUsed         int64
	totalInput        int64
}

// planSQLSchemaReads computes every bounded read before any DDL bytes are
// touched. Aggregate admission can therefore reject a hostile 10,000-table
// summary without first doing terabytes of individually capped reads.
func planSQLSchemaReads(ctx context.Context, lease sqlAnalysisLease) (sqlSchemaReadPlan, error) {
	if err := ctx.Err(); err != nil {
		return sqlSchemaReadPlan{}, err
	}
	ranges, err := sqlextract.PlanTableRanges(lease.entry.Summary, lease.file.Doc.Size(), sqlextract.PlanOptions{})
	if err != nil {
		return sqlSchemaReadPlan{}, err
	}
	if len(ranges) > sqlschemadiff.MaxDiffTableCount {
		return sqlSchemaReadPlan{}, fmt.Errorf("%w: table count %d exceeds per-side limit %d", ErrSQLSchemaDiffBudget, len(ranges), sqlschemadiff.MaxDiffTableCount)
	}
	plan := sqlSchemaReadPlan{reads: make([]sqlSchemaRead, 0, len(ranges))}
	for index, tableRange := range ranges {
		if index%64 == 0 {
			if err := ctx.Err(); err != nil {
				return sqlSchemaReadPlan{}, err
			}
		}
		read := sqlSchemaRead{name: tableRange.Name}
		if tableRange.CreateOffset >= 0 {
			schemas := schemaRegions(tableRange)
			if len(schemas) == 1 {
				region := schemas[0]
				if region[0] < 0 || region[1] <= region[0] {
					return sqlSchemaReadPlan{}, fmt.Errorf("invalid CREATE TABLE region [%d,%d) for table %q", region[0], region[1], tableRange.Name)
				}
				read.start, read.end, read.hasDDL = region[0], region[1], true
				if read.end-read.start > maxSQLSchemaStatementReadBytes {
					read.end = read.start + maxSQLSchemaStatementReadBytes
				}
				readBytes := read.end - read.start
				if plan.inputBytes > int64(sqlschemadiff.MaxDiffDefinitionBytes)-readBytes {
					plan.inputBytes = int64(sqlschemadiff.MaxDiffDefinitionBytes) + 1
				} else {
					plan.inputBytes += readBytes
				}
			}
		}
		plan.reads = append(plan.reads, read)
	}
	if err := ctx.Err(); err != nil {
		return sqlSchemaReadPlan{}, err
	}
	return plan, nil
}

func newSQLSchemaDiffBudget(plans ...sqlSchemaReadPlan) (*sqlSchemaDiffBudget, error) {
	inputRemaining := int64(sqlschemadiff.MaxDiffDefinitionBytes)
	retainedRemaining := int64(sqlschemadiff.MaxDiffDefinitionBytes)
	var totalInput int64
	for _, plan := range plans {
		if plan.inputBytes < 0 || plan.inputBytes > inputRemaining {
			return nil, fmt.Errorf("%w: planned CREATE TABLE input exceeds %d bytes", ErrSQLSchemaDiffBudget, sqlschemadiff.MaxDiffDefinitionBytes)
		}
		inputRemaining -= plan.inputBytes
		totalInput += plan.inputBytes
		for _, read := range plan.reads {
			minimum := sqlschemadiff.TableRetainedBytes(sqlschemadiff.Table{Name: read.name})
			if minimum < 0 || minimum > retainedRemaining {
				return nil, fmt.Errorf("%w: minimum retained schema state exceeds %d bytes", ErrSQLSchemaDiffBudget, sqlschemadiff.MaxDiffDefinitionBytes)
			}
			retainedRemaining -= minimum
		}
	}
	return &sqlSchemaDiffBudget{
		inputRemaining:    int64(sqlschemadiff.MaxDiffDefinitionBytes),
		retainedRemaining: int64(sqlschemadiff.MaxDiffDefinitionBytes),
		totalInput:        totalInput,
	}, nil
}

func (b *sqlSchemaDiffBudget) reserveInput(bytes int64) error {
	if b == nil || bytes < 0 || bytes > b.inputRemaining {
		return fmt.Errorf("%w: aggregate CREATE TABLE input exceeds %d bytes", ErrSQLSchemaDiffBudget, sqlschemadiff.MaxDiffDefinitionBytes)
	}
	b.inputRemaining -= bytes
	b.inputUsed += bytes
	return nil
}

func (b *sqlSchemaDiffBudget) reserveRetained(bytes int64) error {
	if b == nil || bytes < 0 || bytes > b.retainedRemaining {
		return fmt.Errorf("%w: aggregate retained schema state exceeds %d bytes", ErrSQLSchemaDiffBudget, sqlschemadiff.MaxDiffDefinitionBytes)
	}
	b.retainedRemaining -= bytes
	return nil
}

// parsedSchemaFromLease reads each table's CREATE TABLE DDL from a dump and
// parses its columns using an already-retained cached-analysis lease. It must
// never reacquire a registry lease: SqlSchemaDiff owns canonical acquisition.
func (s *FileService) parsedSchemaFromLease(ctx context.Context, lease sqlAnalysisLease, plan sqlSchemaReadPlan, budget *sqlSchemaDiffBudget, progress func(int64, int64, string)) ([]sqlschemadiff.Table, error) {
	tables := make([]sqlschemadiff.Table, 0, len(plan.reads))
	for index, read := range plan.reads {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		table := sqlschemadiff.Table{Name: read.name}
		// No CREATE TABLE → schema is unknown; emit an empty-column table rather
		// than parsing INSERT data rows as if they were column definitions.
		if read.hasDDL {
			readBytes := read.end - read.start
			if readBytes < 0 || readBytes > maxSQLSchemaStatementReadBytes {
				return nil, fmt.Errorf("invalid bounded schema read [%d,%d)", read.start, read.end)
			}
			if err := budget.reserveInput(readBytes); err != nil {
				return nil, err
			}
			if err := notifySQLSchemaDiffRead(ctx, lease.file.ID, read.start, read.end); err != nil {
				return nil, err
			}
			ddl, err := lease.file.Doc.ReadRangeWithLimit(read.start, read.end, maxSQLSchemaStatementReadBytes)
			if err != nil {
				return nil, err
			}
			if int64(len(ddl)) > maxSQLSchemaStatementReadBytes {
				return nil, fmt.Errorf("schema read exceeded %d-byte statement cap", maxSQLSchemaStatementReadBytes)
			}
			parsed, err := sqlschemadiff.ParseTableContext(ctx, ddl)
			if err != nil {
				return nil, err
			}
			table.Definition = parsed
			if progress != nil {
				progress(budget.inputUsed, budget.totalInput, fmt.Sprintf("%d schema definitions parsed", index+1))
			}
		}
		if err := budget.reserveRetained(sqlschemadiff.TableRetainedBytes(table)); err != nil {
			return nil, err
		}
		tables = append(tables, table)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := s.validateSQLAnalysisLease(lease); err != nil {
		return nil, err
	}
	return tables, nil
}

func baseName(p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

// SqlListPresets exposes no names while SQL cleanup presets remain disabled.
func (s *FileService) SqlListPresets() []string {
	return []string{}
}

// SqlApplyPresetViaDialog is retained as a bridge-compatibility boundary while
// cleanup presets are disabled. It returns the same fail-closed error before
// file lookup, dialog selection, job creation, or filesystem work.
func (s *FileService) SqlApplyPresetViaDialog(fileID, name, a1, a2, a3, a4 string) (TransformResult, error) {
	return TransformResult{}, ErrSQLCleanupPresetsDisabled
}

func validateDocumentSourceForOutput(doc *document.FileDocument) error {
	if doc == nil {
		return errors.New("document is required")
	}
	if err := doc.ValidateUnchanged(); err != nil {
		return err
	}
	current, sameOpenedFile, err := doc.CurrentPathIdentity()
	if err != nil {
		return err
	}
	if !sameOpenedFile || !current.Equal(doc.OriginalFileState()) {
		return fmt.Errorf("%w: source pathname or generation changed", document.ErrSourceChanged)
	}
	return nil
}

func safeFileName(name string) string {
	return sqlextract.SafeFilenameComponent(name)
}

func fmtByteCount(n int64) string {
	return units.FormatBytes(n)
}
