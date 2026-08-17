package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/quarry/quarry-wails3/internal/document"
	sqlanalyze "github.com/quarry/quarry-wails3/internal/plugins/sql/analyze"
	"github.com/quarry/quarry-wails3/internal/session"
	"github.com/quarry/quarry-wails3/internal/sourceio"
)

const sqlGenerationFixture = "CREATE TABLE `alpha` (`id` int);\nINSERT INTO `alpha` VALUES (1),(2);\n"

func TestSQLAnalysisRefreshInvalidatesEveryCachedConsumerBeforeDialog(t *testing.T) {
	svc, meta, sourcePath := openAnalyzedSQLGenerationFile(t, sqlGenerationFixture)
	if got := sqlCacheEntryCount(svc); got != 1 {
		t.Fatalf("cache entries = %d, want 1", got)
	}
	if err := os.WriteFile(sourcePath, []byte(sqlGenerationFixture+"-- refreshed generation\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RefreshFile(meta.FileID); err != nil {
		t.Fatal(err)
	}
	assertSQLCacheReleased(t, svc, meta.FileID)

	dialogCalls := 0
	installSQLGenerationDialogs(t,
		func(string, string) (string, error) {
			dialogCalls++
			return "", errors.New("stale SQL consumer reached save dialog")
		},
		func(string) (string, error) {
			dialogCalls++
			return "", errors.New("stale SQL consumer reached directory dialog")
		},
	)

	consumers := []struct {
		name string
		call func() error
	}{
		{name: "lint", call: func() error { _, err := svc.SqlLint(meta.FileID); return err }},
		{name: "table extract", call: func() error { _, err := svc.SqlExtractTableViaDialog(meta.FileID, "alpha"); return err }},
		{name: "split", call: func() error { _, err := svc.SqlSplitByTableViaDialog(meta.FileID); return err }},
		{name: "schema extract", call: func() error { _, err := svc.SqlExtractSchemaViaDialog(meta.FileID, "alpha"); return err }},
		{name: "data extract", call: func() error { _, err := svc.SqlExtractDataViaDialog(meta.FileID, "alpha"); return err }},
		{name: "fixture", call: func() error { _, err := svc.SqlSampleFixtureViaDialog(meta.FileID, 1); return err }},
		{name: "schema diff", call: func() error { _, err := svc.SqlSchemaDiff(meta.FileID, meta.FileID); return err }},
	}
	for _, consumer := range consumers {
		t.Run(consumer.name, func(t *testing.T) {
			if err := consumer.call(); !errors.Is(err, ErrSQLAnalysisRequired) {
				t.Fatalf("error = %v, want ErrSQLAnalysisRequired", err)
			}
		})
	}
	if dialogCalls != 0 {
		t.Fatalf("stale consumers opened %d dialog(s), want 0", dialogCalls)
	}
}

func TestSQLAnalysisDetectsTruncateWithoutRefreshAndReleasesCache(t *testing.T) {
	svc, meta, sourcePath := openAnalyzedSQLGenerationFile(t, sqlGenerationFixture)
	if err := os.Truncate(sourcePath, int64(len(sqlGenerationFixture)/2)); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.SqlLint(meta.FileID); !errors.Is(err, ErrSQLAnalysisStale) || !errors.Is(err, document.ErrSourceChanged) {
		t.Fatalf("SqlLint error = %v, want stale analysis and ErrSourceChanged", err)
	}
	assertSQLCacheReleased(t, svc, meta.FileID)
	if _, err := svc.SqlLint(meta.FileID); !errors.Is(err, ErrSQLAnalysisRequired) {
		t.Fatalf("second SqlLint error = %v, want ErrSQLAnalysisRequired", err)
	}
}

func TestSQLAnalysisDetectsRenameRecreateWithSameSizeAndMtime(t *testing.T) {
	svc, meta, sourcePath := openAnalyzedSQLGenerationFile(t, sqlGenerationFixture)
	info, err := os.Stat(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	displacedPath := sourcePath + ".opened-generation"
	replacement := strings.Repeat("x", len(sqlGenerationFixture))
	if err := os.Rename(sourcePath, displacedPath); err != nil {
		t.Fatalf("rename opened SQL source: %v", err)
	}
	if err := os.WriteFile(sourcePath, []byte(replacement), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(sourcePath, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	recreated, err := os.Stat(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if recreated.Size() != info.Size() || !recreated.ModTime().Equal(info.ModTime()) {
		t.Fatalf("replacement state = size %d mtime %s, want %d / %s", recreated.Size(), recreated.ModTime(), info.Size(), info.ModTime())
	}

	if _, err := svc.SqlLint(meta.FileID); !errors.Is(err, ErrSQLAnalysisStale) || !errors.Is(err, document.ErrSourceChanged) {
		t.Fatalf("SqlLint error = %v, want identity-bound stale analysis", err)
	}
	assertSQLCacheReleased(t, svc, meta.FileID)
}

func TestSQLAnalysisCloseAndReopenReleaseCache(t *testing.T) {
	svc, meta, sourcePath := openAnalyzedSQLGenerationFile(t, sqlGenerationFixture)
	if err := svc.CloseFile(meta.FileID); err != nil {
		t.Fatal(err)
	}
	assertSQLCacheReleased(t, svc, meta.FileID)
	if _, err := svc.SqlLint(meta.FileID); err == nil || errors.Is(err, ErrSQLAnalysisRequired) {
		t.Fatalf("closed-session SqlLint error = %v, want unknown file ID", err)
	}

	reopened, err := svc.OpenFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.CloseFile(reopened.FileID) })
	if reopened.FileID == meta.FileID {
		t.Fatalf("reopened ID = %q, want a new session ID", reopened.FileID)
	}
	if _, err := svc.SqlLint(reopened.FileID); !errors.Is(err, ErrSQLAnalysisRequired) {
		t.Fatalf("new-session SqlLint error = %v, want ErrSQLAnalysisRequired", err)
	}
	if _, err := svc.SqlAnalyze(reopened.FileID); err != nil {
		t.Fatal(err)
	}
	if got := sqlCacheEntryCount(svc); got != 1 {
		t.Fatalf("cache entries after reanalysis = %d, want 1", got)
	}
}

func TestSQLAnalysisDirectSessionReplacementRejectsOldGeneration(t *testing.T) {
	svc, meta, _ := openAnalyzedSQLGenerationFile(t, sqlGenerationFixture)
	before, ok := svc.reg.Snapshot(meta.FileID)
	if !ok {
		t.Fatal("missing original session snapshot")
	}
	if _, err := svc.reg.Reopen(meta.FileID); err != nil {
		t.Fatal(err)
	}
	after, ok := svc.reg.Snapshot(meta.FileID)
	if !ok {
		t.Fatal("missing replacement session snapshot")
	}
	if after.Generation != before.Generation+1 || after.Doc == before.Doc {
		t.Fatalf("generation transition = %d/%p -> %d/%p", before.Generation, before.Doc, after.Generation, after.Doc)
	}

	if _, err := svc.SqlLint(meta.FileID); !errors.Is(err, ErrSQLAnalysisStale) {
		t.Fatalf("SqlLint error = %v, want ErrSQLAnalysisStale", err)
	}
	assertSQLCacheReleased(t, svc, meta.FileID)
}

func TestSQLStaleGenerationInvalidationCannotClearNewerCache(t *testing.T) {
	svc, meta, _ := openAnalyzedSQLGenerationFile(t, sqlGenerationFixture)
	before, ok := svc.reg.Snapshot(meta.FileID)
	if !ok {
		t.Fatal("missing original session snapshot")
	}
	if _, err := svc.reg.Reopen(meta.FileID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SqlAnalyze(meta.FileID); err != nil {
		t.Fatal(err)
	}
	after, ok := svc.reg.Snapshot(meta.FileID)
	if !ok || after.Generation == before.Generation {
		t.Fatalf("session generation did not advance: before=%d after=%d", before.Generation, after.Generation)
	}

	svc.invalidateSQLAnalysisGeneration(meta.FileID, before.Generation)
	lease, err := svc.sqlSummaryFor(meta.FileID)
	if err != nil {
		t.Fatalf("stale generation cleared newer cache: %v", err)
	}
	defer lease.Release()
	if lease.entry.Generation != after.Generation {
		t.Fatalf("cached generation = %d, want current %d", lease.entry.Generation, after.Generation)
	}
}

func TestSQLAnalysisOptionsArePartOfCacheOwnership(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "options.sql")
	if err := os.WriteFile(sourcePath, []byte(sqlGenerationFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	svc := NewFileService()
	meta, err := svc.OpenFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.CloseFile(meta.FileID) })

	const chunkSize = 64
	if _, err := svc.sqlAnalyzeWithOptions(context.Background(), meta.FileID, sqlanalyze.Options{ChunkSize: chunkSize}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.sqlSummaryFor(meta.FileID); !errors.Is(err, ErrSQLAnalysisOptionsMismatch) {
		t.Fatalf("default-options lookup error = %v, want ErrSQLAnalysisOptionsMismatch", err)
	}
	dialogCalls := 0
	installSQLGenerationDialogs(t, func(string, string) (string, error) {
		dialogCalls++
		return "", errors.New("option-mismatched extraction reached dialog")
	}, nil)
	if _, err := svc.SqlExtractTableViaDialog(meta.FileID, "alpha"); !errors.Is(err, ErrSQLAnalysisOptionsMismatch) {
		t.Fatalf("option-mismatched extract error = %v, want ErrSQLAnalysisOptionsMismatch", err)
	}
	if dialogCalls != 0 {
		t.Fatalf("option-mismatched extraction opened %d dialog(s), want 0", dialogCalls)
	}
	lease, err := svc.sqlSummaryForOptions(meta.FileID, sqlanalyze.Options{ChunkSize: chunkSize})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if lease.entry.Options.ChunkSize != chunkSize {
		t.Fatalf("cached chunk size = %d, want %d", lease.entry.Options.ChunkSize, chunkSize)
	}
	if got := sqlCacheEntryCount(svc); got != 1 {
		t.Fatalf("option mismatch released valid differently-keyed cache: entries=%d", got)
	}
}

func TestSQLExtractRevalidatesGenerationAfterDialogBeforeArtifact(t *testing.T) {
	svc, meta, _ := openAnalyzedSQLGenerationFile(t, sqlGenerationFixture)
	outputPath := filepath.Join(t.TempDir(), "must-not-exist.sql")
	dialogCalls := 0
	installSQLGenerationDialogs(t, func(string, string) (string, error) {
		dialogCalls++
		if _, err := svc.RefreshFile(meta.FileID); err != nil {
			t.Fatalf("refresh inside dialog seam: %v", err)
		}
		return outputPath, nil
	}, nil)

	if _, err := svc.SqlExtractTableViaDialog(meta.FileID, "alpha"); !errors.Is(err, ErrSQLAnalysisStale) {
		t.Fatalf("extract error = %v, want ErrSQLAnalysisStale", err)
	}
	if dialogCalls != 1 {
		t.Fatalf("dialog calls = %d, want 1", dialogCalls)
	}
	if _, err := os.Lstat(outputPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale extraction created an artifact: %v", err)
	}
	assertSQLCacheReleased(t, svc, meta.FileID)
}

func TestSQLDialogTransformsRejectRefreshAndReanalysisBeforeArtifact(t *testing.T) {
	tests := []struct {
		name      string
		directory bool
		call      func(*FileService, string) (TransformResult, error)
	}{
		{name: "table extract", call: func(svc *FileService, fileID string) (TransformResult, error) {
			return svc.SqlExtractTableViaDialog(fileID, "alpha")
		}},
		{name: "split", directory: true, call: func(svc *FileService, fileID string) (TransformResult, error) {
			return svc.SqlSplitByTableViaDialog(fileID)
		}},
		{name: "schema extract", call: func(svc *FileService, fileID string) (TransformResult, error) {
			return svc.SqlExtractSchemaViaDialog(fileID, "alpha")
		}},
		{name: "data extract", call: func(svc *FileService, fileID string) (TransformResult, error) {
			return svc.SqlExtractDataViaDialog(fileID, "alpha")
		}},
		{name: "sample fixture", call: func(svc *FileService, fileID string) (TransformResult, error) {
			return svc.SqlSampleFixtureViaDialog(fileID, 1)
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			svc, meta, _ := openAnalyzedSQLGenerationFile(t, sqlGenerationFixture)
			dir := t.TempDir()
			outputPath := filepath.Join(dir, "must-not-exist.sql")
			dialogCalls := 0
			refreshAndReanalyze := func() {
				dialogCalls++
				if _, err := svc.RefreshFile(meta.FileID); err != nil {
					t.Fatalf("refresh inside dialog seam: %v", err)
				}
				if _, err := svc.SqlAnalyze(meta.FileID); err != nil {
					t.Fatalf("reanalyze inside dialog seam: %v", err)
				}
			}
			installSQLGenerationDialogs(t,
				func(string, string) (string, error) {
					refreshAndReanalyze()
					return outputPath, nil
				},
				func(string) (string, error) {
					refreshAndReanalyze()
					return dir, nil
				},
			)

			if _, err := test.call(svc, meta.FileID); !errors.Is(err, ErrSQLAnalysisStale) {
				t.Fatalf("transform error = %v, want ErrSQLAnalysisStale", err)
			}
			if dialogCalls != 1 {
				t.Fatalf("dialog calls = %d, want 1", dialogCalls)
			}
			if test.directory {
				entries, err := os.ReadDir(dir)
				if err != nil {
					t.Fatal(err)
				}
				if len(entries) != 0 {
					t.Fatalf("stale split created artifacts: %v", entries)
				}
			} else if _, err := os.Lstat(outputPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("stale transform created an artifact: %v", err)
			}
		})
	}
}

func TestSQLReshapeRevalidatesGenerationAfterDialogBeforeArtifact(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.sql")
	outputPath := filepath.Join(dir, "must-not-exist.sql")
	if err := os.WriteFile(sourcePath, []byte(sqlGenerationFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	svc := NewFileService()
	meta, err := svc.OpenFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.CloseFile(meta.FileID) })

	dialogCalls := 0
	installSQLGenerationDialogs(t, func(string, string) (string, error) {
		dialogCalls++
		if _, refreshErr := svc.RefreshFile(meta.FileID); refreshErr != nil {
			t.Fatalf("refresh inside reshape dialog seam: %v", refreshErr)
		}
		return outputPath, nil
	}, nil)

	if _, err := svc.SqlReshapeInsertsViaDialog(meta.FileID, "single", 100); !errors.Is(err, sourceio.ErrSourceChanged) {
		t.Fatalf("reshape error = %v, want source generation change", err)
	}
	if dialogCalls != 1 {
		t.Fatalf("dialog calls = %d, want 1", dialogCalls)
	}
	if _, err := os.Lstat(outputPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale reshape created an artifact: %v", err)
	}
	if got, readErr := os.ReadFile(sourcePath); readErr != nil || string(got) != sqlGenerationFixture {
		t.Fatalf("source changed: %q, %v", got, readErr)
	}
}

func TestSQLAnalysisRefreshCancelsInFlightGenerationAndCannotRepopulateCache(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "concurrent.sql")
	content := strings.Repeat(sqlGenerationFixture, 128)
	if err := os.WriteFile(sourcePath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	svc := NewFileService()
	meta, err := svc.OpenFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, ok := svc.reg.Get(meta.FileID); ok {
			_ = svc.CloseFile(meta.FileID)
		}
	})

	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	errCh := make(chan error, 1)
	go func() {
		_, err := svc.sqlAnalyzeWithOptions(context.Background(), meta.FileID, sqlanalyze.Options{
			ChunkSize: 32,
			Progress: func(sqlanalyze.Progress) {
				once.Do(func() { close(started) })
				<-release
			},
		})
		errCh <- err
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("analysis did not reach deterministic progress boundary")
	}
	refreshCh := make(chan error, 1)
	go func() {
		_, err := svc.RefreshFile(meta.FileID)
		refreshCh <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		probe, _, probeErr := svc.acquireReadFile(meta.FileID)
		if errors.Is(probeErr, session.ErrTransitioning) {
			break
		}
		if probe != nil {
			probe.Release()
		}
		if probeErr != nil {
			t.Fatalf("probe transition: %v", probeErr)
		}
		if time.Now().After(deadline) {
			t.Fatal("refresh did not enter the transition barrier")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case err := <-refreshCh:
		t.Fatalf("refresh returned before the active analysis released its lease: %v", err)
	default:
	}
	close(release)
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) && !errors.Is(err, ErrSQLAnalysisStale) {
			t.Fatalf("analysis error = %v, want cancellation/stale generation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled analysis did not return")
	}
	select {
	case err := <-refreshCh:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("refresh did not finish after the analysis released its lease")
	}
	assertSQLCacheReleased(t, svc, meta.FileID)
}

func TestSQLAnalysisCloseCancelsInFlightGenerationAndReleasesRun(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "close-concurrent.sql")
	content := strings.Repeat(sqlGenerationFixture, 128)
	if err := os.WriteFile(sourcePath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	svc := NewFileService()
	meta, err := svc.OpenFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}

	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	errCh := make(chan error, 1)
	go func() {
		_, err := svc.sqlAnalyzeWithOptions(context.Background(), meta.FileID, sqlanalyze.Options{
			ChunkSize: 32,
			Progress: func(sqlanalyze.Progress) {
				once.Do(func() { close(started) })
				<-release
			},
		})
		errCh <- err
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("analysis did not reach deterministic progress boundary")
	}
	closeCh := make(chan error, 1)
	go func() { closeCh <- svc.CloseFile(meta.FileID) }()
	// Close establishes a transition before cancellation and keeps the ID
	// transition-owned until pre-existing leases drain. Removing the ID before
	// that dirty-state check would make a refused close unreachable.
	waitForTransition(t, svc, meta.FileID)
	select {
	case err := <-closeCh:
		t.Fatalf("close returned before the active analysis released its lease: %v", err)
	default:
	}
	close(release)
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) && !errors.Is(err, ErrSQLAnalysisStale) {
			t.Fatalf("analysis error = %v, want cancellation/stale generation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("close-canceled analysis did not return")
	}
	select {
	case err := <-closeCh:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("close did not finish after the analysis released its lease")
	}
	assertSQLCacheReleased(t, svc, meta.FileID)
}

func TestSQLAnalysisSourceDriftDuringScanCannotPopulateCache(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "drift-during-analysis.sql")
	content := strings.Repeat(sqlGenerationFixture, 128)
	if err := os.WriteFile(sourcePath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	svc := NewFileService()
	meta, err := svc.OpenFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.CloseFile(meta.FileID) })

	truncated := false
	var truncateErr error
	_, err = svc.sqlAnalyzeWithOptions(context.Background(), meta.FileID, sqlanalyze.Options{
		ChunkSize: 32,
		Progress: func(sqlanalyze.Progress) {
			if !truncated {
				truncated = true
				truncateErr = os.Truncate(sourcePath, 64)
			}
		},
	})
	if truncateErr != nil {
		t.Fatalf("truncate source during analysis: %v", truncateErr)
	}
	if !errors.Is(err, ErrSQLAnalysisStale) || !errors.Is(err, document.ErrSourceChanged) {
		t.Fatalf("analysis error = %v, want stale/source-changed failure", err)
	}
	assertSQLCacheReleased(t, svc, meta.FileID)
}

func TestSQLSourceStatePollingInvalidatesCachedAnalysis(t *testing.T) {
	svc, meta, sourcePath := openAnalyzedSQLGenerationFile(t, sqlGenerationFixture)
	if err := os.WriteFile(sourcePath, []byte(sqlGenerationFixture+"-- growth\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.FileSize(meta.FileID); err != nil {
		t.Fatal(err)
	}
	assertSQLCacheReleased(t, svc, meta.FileID)
}

func TestPublicSQLAnalysisJobEmitsBoundedProgressAndCancelsByID(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "job-progress.sql")
	writeLargeSQLJobFixture(t, sourcePath, defaultSQLAnalysisChunkSize+1024)
	svc := NewFileService()
	meta, err := svc.OpenFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, ok := svc.reg.Get(meta.FileID); ok {
			_ = svc.CloseFile(meta.FileID)
		}
	})

	var mu sync.Mutex
	events := make([]recordedJobEvent, 0, 4)
	var cancelOnce sync.Once
	var cancelErr error
	manager := svc.jobs()
	manager.mu.Lock()
	manager.emit = func(name string, value any) {
		data, _ := value.(map[string]any)
		mu.Lock()
		events = append(events, recordedJobEvent{name: name, data: data})
		mu.Unlock()
		if name == "quarry:job-progress" {
			cancelOnce.Do(func() {
				jobID, _ := data["id"].(string)
				cancelErr = svc.CancelJob(jobID)
			})
		}
	}
	manager.mu.Unlock()

	if _, err := svc.SqlAnalyze(meta.FileID); !errors.Is(err, ErrJobCancelled) || !errors.Is(err, context.Canceled) {
		t.Fatalf("SqlAnalyze error = %v, want ID-scoped cancellation", err)
	}
	if cancelErr != nil {
		t.Fatalf("CancelJob: %v", cancelErr)
	}
	assertSQLCacheReleased(t, svc, meta.FileID)
	manager.mu.Lock()
	active := manager.active
	manager.mu.Unlock()
	if active != nil {
		t.Fatal("canceled public SQL analysis retained active job ownership")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(events) < 3 || events[0].name != "quarry:job-start" || events[len(events)-1].name != "quarry:job-end" {
		t.Fatalf("job event order = %#v", events)
	}
	start := events[0].data
	if start["kind"] != jobKindSQLAnalysis || start["fileId"] != meta.FileID {
		t.Fatalf("start ownership = %#v", start)
	}
	if start["total"] != int64(0) {
		t.Fatalf("start total = %v, want 0 until analyzer progress establishes the source size", start["total"])
	}
	foundProgress := false
	for _, event := range events {
		if event.name != "quarry:job-progress" {
			continue
		}
		foundProgress = true
		completed, _ := event.data["completed"].(int64)
		total, _ := event.data["total"].(int64)
		if completed <= 0 || completed > total || total != meta.Size {
			t.Fatalf("progress payload = %#v", event.data)
		}
	}
	if !foundProgress {
		t.Fatal("SQL analysis emitted no progress event")
	}
	if status := events[len(events)-1].data["status"]; status != "cancelled" {
		t.Fatalf("terminal status = %v, want cancelled", status)
	}
}

func TestCancelledReanalysisRetainsPreviousValidSummary(t *testing.T) {
	svc, meta, _ := openAnalyzedSQLGenerationFile(t, strings.Repeat(sqlGenerationFixture, 16))
	ctx, cancel := context.WithCancel(context.Background())
	var once sync.Once
	_, err := svc.sqlAnalyzeWithOptions(ctx, meta.FileID, sqlanalyze.Options{
		ChunkSize: 32,
		Progress: func(sqlanalyze.Progress) {
			once.Do(cancel)
		},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("reanalysis error = %v, want context cancellation", err)
	}
	if got := sqlCacheEntryCount(svc); got != 1 {
		t.Fatalf("cache entries after failed reanalysis = %d, want prior valid entry", got)
	}
	_, err = svc.SqlLint(meta.FileID)
	if err != nil {
		t.Fatalf("prior summary was not usable after failed reanalysis: %v", err)
	}
}

func writeLargeSQLJobFixture(t *testing.T, path string, minimumBytes int) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	block := strings.Repeat(sqlGenerationFixture, 1024)
	written := 0
	for written < minimumBytes {
		n, writeErr := f.WriteString(block)
		written += n
		if writeErr != nil {
			_ = f.Close()
			t.Fatal(writeErr)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func openAnalyzedSQLGenerationFile(t *testing.T, content string) (*FileService, FileMeta, string) {
	t.Helper()
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.sql")
	if err := os.WriteFile(sourcePath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	svc := NewFileService()
	meta, err := svc.OpenFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, ok := svc.reg.Get(meta.FileID); ok {
			_ = svc.CloseFile(meta.FileID)
		}
	})
	result, err := svc.SqlAnalyze(meta.FileID)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Tables) == 0 || result.Tables[0].Name != "alpha" {
		t.Fatalf("analysis result = %+v, want alpha table", result)
	}
	return svc, meta, sourcePath
}

func installSQLGenerationDialogs(t *testing.T, save func(string, string) (string, error), dir func(string) (string, error)) {
	t.Helper()
	originalSave := sqlSaveDialog
	originalDir := sqlDirDialog
	if save != nil {
		sqlSaveDialog = save
	}
	if dir != nil {
		sqlDirDialog = dir
	}
	t.Cleanup(func() {
		sqlSaveDialog = originalSave
		sqlDirDialog = originalDir
	})
}

func sqlCacheEntryCount(svc *FileService) int {
	svc.sqlMu.Lock()
	defer svc.sqlMu.Unlock()
	return len(svc.sqlSummary)
}

func assertSQLCacheReleased(t *testing.T, svc *FileService, fileID string) {
	t.Helper()
	svc.sqlMu.Lock()
	defer svc.sqlMu.Unlock()
	if _, ok := svc.sqlSummary[fileID]; ok {
		t.Fatalf("cached SQL summary for %q was not released", fileID)
	}
	if _, ok := svc.sqlRuns[fileID]; ok {
		t.Fatalf("in-flight SQL analysis for %q was not released", fileID)
	}
}
