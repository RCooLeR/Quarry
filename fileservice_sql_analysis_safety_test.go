package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sqlanalyze "github.com/quarry/quarry-wails3/internal/plugins/sql/analyze"
	"github.com/quarry/quarry-wails3/internal/sourceio"
)

func TestSqlAnalyzeRejectsDelimiterDumpWithoutCacheDialogsOrOutputs(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "routines.sql")
	outputPath := filepath.Join(dir, "must-not-exist.sql")
	splitDir := filepath.Join(dir, "must-stay-empty")
	if err := os.Mkdir(splitDir, 0o700); err != nil {
		t.Fatal(err)
	}
	source := "CREATE TABLE before_routine (id int);\r\n" +
		"\xef\xbb\xbf-not-a-bom\r\n" + // a mid-file BOM is ordinary source data
		strings.Repeat(" ", 80) + "DELIMITER $$\r\n" +
		"CREATE PROCEDURE p()\r\nBEGIN\r\n" +
		"  INSERT INTO routine_phantom VALUES (1);\r\nEND$$\r\nDELIMITER ;\r\n"
	if err := os.WriteFile(sourcePath, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}

	svc := NewFileService()
	meta, err := svc.OpenFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.CloseFile(meta.FileID) })

	result, err := svc.SqlAnalyze(meta.FileID)
	if !errors.Is(err, sqlanalyze.ErrUnsupportedDelimiter) {
		t.Fatalf("SqlAnalyze error = %v, want ErrUnsupportedDelimiter", err)
	}
	if len(result.Tables) != 0 || result.CreateTables != 0 || result.InsertTables != 0 || result.DefinerCount != 0 || result.Header {
		t.Fatalf("unsupported analysis returned partial bridge result: %#v", result)
	}
	if got := sqlCacheEntryCount(svc); got != 0 {
		t.Fatalf("unsupported analysis cached %d summary entries, want 0", got)
	}

	dialogCalls := 0
	installSQLGenerationDialogs(t,
		func(string, string) (string, error) {
			dialogCalls++
			return outputPath, nil
		},
		func(string) (string, error) {
			dialogCalls++
			return splitDir, nil
		},
	)
	consumers := []struct {
		name string
		call func() error
	}{
		{name: "table extract", call: func() error { _, err := svc.SqlExtractTableViaDialog(meta.FileID, "routine_phantom"); return err }},
		{name: "split", call: func() error { _, err := svc.SqlSplitByTableViaDialog(meta.FileID); return err }},
		{name: "schema", call: func() error { _, err := svc.SqlExtractSchemaViaDialog(meta.FileID, ""); return err }},
		{name: "data", call: func() error { _, err := svc.SqlExtractDataViaDialog(meta.FileID, "routine_phantom"); return err }},
		{name: "fixture", call: func() error { _, err := svc.SqlSampleFixtureViaDialog(meta.FileID, 1); return err }},
		{name: "lint", call: func() error { _, err := svc.SqlLint(meta.FileID); return err }},
	}
	for _, consumer := range consumers {
		if err := consumer.call(); !errors.Is(err, ErrSQLAnalysisRequired) {
			t.Errorf("%s error = %v, want ErrSQLAnalysisRequired", consumer.name, err)
		}
	}
	if dialogCalls != 0 {
		t.Fatalf("cache consumers opened %d dialog(s), want 0", dialogCalls)
	}
	if _, err := os.Lstat(outputPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unsupported cached consumer created output: %v", err)
	}
	entries, err := os.ReadDir(splitDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("unsupported split created outputs: %v", entries)
	}
}

func TestSqlAnalyzeBacktickTextCannotCreateExtractablePhantom(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "quoted-identifiers.sql")
	outputPath := filepath.Join(dir, "phantom.sql")
	source := "CREATE TABLE `real` (\n" +
		"  `column INSERT INTO phantom VALUES (1)` int,\n" +
		"  `escaped `` REPLACE INTO doubled_phantom VALUES (2)` int\n" +
		");\nINSERT INTO `real` VALUES (1, 2);\n"
	if err := os.WriteFile(sourcePath, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	svc := NewFileService()
	meta, err := svc.OpenFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.CloseFile(meta.FileID) })

	result, err := svc.SqlAnalyze(meta.FileID)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Tables) != 1 || result.Tables[0].Name != "real" {
		t.Fatalf("SqlAnalyze returned phantom tables: %#v", result.Tables)
	}
	dialogCalls := 0
	installSQLGenerationDialogs(t, func(string, string) (string, error) {
		dialogCalls++
		return outputPath, nil
	}, nil)
	if _, err := svc.SqlExtractTableViaDialog(meta.FileID, "phantom"); err == nil {
		t.Fatal("phantom table extraction unexpectedly succeeded")
	}
	if dialogCalls != 0 {
		t.Fatalf("phantom extraction opened %d dialog(s), want 0", dialogCalls)
	}
	if _, err := os.Lstat(outputPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("phantom extraction created output: %v", err)
	}
}

func TestSqlAnalyzeRejectsCompoundTriggerWithoutCachingInnerRegions(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "trigger.sql")
	outputPath := filepath.Join(dir, "must-not-exist.sql")
	source := "CREATE TRIGGER trg AFTER INSERT ON src BEGIN\n" +
		" INSERT INTO audit VALUES (1);\n INSERT INTO audit2 VALUES (2);\nEND;\n"
	if err := os.WriteFile(sourcePath, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	svc := NewFileService()
	meta, err := svc.OpenFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.CloseFile(meta.FileID) })

	result, err := svc.SqlAnalyze(meta.FileID)
	if !errors.Is(err, sqlanalyze.ErrUnsupportedCompoundStatement) {
		t.Fatalf("SqlAnalyze error = %v, want ErrUnsupportedCompoundStatement", err)
	}
	if len(result.Tables) != 0 || sqlCacheEntryCount(svc) != 0 {
		t.Fatalf("compound analysis escaped partial/cache state: result=%#v cache=%d", result, sqlCacheEntryCount(svc))
	}
	dialogCalls := 0
	installSQLGenerationDialogs(t, func(string, string) (string, error) {
		dialogCalls++
		return outputPath, nil
	}, nil)
	if _, err := svc.SqlExtractTableViaDialog(meta.FileID, "audit2"); !errors.Is(err, ErrSQLAnalysisRequired) {
		t.Fatalf("inner trigger extraction error = %v, want ErrSQLAnalysisRequired", err)
	}
	if dialogCalls != 0 {
		t.Fatalf("inner trigger extraction opened %d dialog(s), want 0", dialogCalls)
	}
	if _, err := os.Lstat(outputPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("inner trigger extraction created output: %v", err)
	}
}

func TestSQLAnalysisVerifiedReaderRejectsSameSizeRestoredMtimeRewrite(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "transient-rewrite.sql")
	const sourceBytes = 2*1024*1024 + 4096
	content := []byte("CREATE TABLE safe_table (id int);\n")
	content = append(content, []byte(strings.Repeat(" ", sourceBytes-len(content)))...)
	if err := os.WriteFile(sourcePath, content, 0o600); err != nil {
		t.Fatal(err)
	}
	originalInfo, err := os.Stat(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	svc := NewFileService()
	meta, err := svc.OpenFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.CloseFile(meta.FileID) })
	lease, _, err := svc.acquireReadFile(meta.FileID)
	if err != nil {
		t.Fatal(err)
	}
	hasStrongGeneration := lease.Snapshot().Doc.HasMutationGeneration()
	lease.Release()
	if !hasStrongGeneration {
		_, err := svc.sqlAnalyzeWithOptions(context.Background(), meta.FileID, sqlanalyze.Options{ChunkSize: 64 * 1024})
		if !errors.Is(err, sourceio.ErrMutationGenerationMissing) {
			t.Fatalf("weak-token analysis error = %v, want ErrMutationGenerationMissing", err)
		}
		assertSQLCacheReleased(t, svc, meta.FileID)
		return
	}

	mutated := false
	var mutationErr error
	poison := []byte("INSERT INTO poisoned_offsets VALUES (1);")
	_, err = svc.sqlAnalyzeWithOptions(context.Background(), meta.FileID, sqlanalyze.Options{
		ChunkSize: 64 * 1024,
		Progress: func(p sqlanalyze.Progress) {
			if mutated || p.BytesProcessed == 0 {
				return
			}
			mutated = true
			file, openErr := os.OpenFile(sourcePath, os.O_WRONLY, 0)
			if openErr != nil {
				mutationErr = openErr
				return
			}
			_, writeErr := file.WriteAt(poison, 1024*1024+128)
			mutationErr = errors.Join(writeErr, file.Sync(), file.Close())
			if mutationErr == nil {
				mutationErr = os.Chtimes(sourcePath, originalInfo.ModTime(), originalInfo.ModTime())
			}
		},
	})
	if mutationErr != nil {
		t.Fatalf("same-size rewrite: %v", mutationErr)
	}
	if !mutated {
		t.Fatal("analysis did not reach the deterministic mutation boundary")
	}
	if !errors.Is(err, sourceio.ErrSourceChanged) || !errors.Is(err, ErrSQLAnalysisStale) {
		t.Fatalf("analysis error = %v, want stale verified-source rejection", err)
	}
	if info, statErr := os.Stat(sourcePath); statErr != nil || info.Size() != originalInfo.Size() || !info.ModTime().Equal(originalInfo.ModTime()) {
		t.Fatalf("rewrite did not preserve visible size/mtime: info=%v err=%v", info, statErr)
	}
	assertSQLCacheReleased(t, svc, meta.FileID)

	dialogCalls := 0
	outputPath := filepath.Join(dir, "poisoned.sql")
	installSQLGenerationDialogs(t, func(string, string) (string, error) {
		dialogCalls++
		return outputPath, nil
	}, nil)
	if _, err := svc.SqlExtractTableViaDialog(meta.FileID, "poisoned_offsets"); err == nil {
		t.Fatal("poisoned offsets became extractable")
	}
	if dialogCalls != 0 {
		t.Fatalf("poisoned cache opened %d dialog(s), want 0", dialogCalls)
	}
	if _, err := os.Lstat(outputPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("poisoned cache created output: %v", err)
	}

	// Restore source contents so cleanup exercises an ordinary unchanged file;
	// the failed analysis must remain uncached regardless.
	if err := os.WriteFile(sourcePath, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(sourcePath, originalInfo.ModTime(), originalInfo.ModTime()); err != nil {
		t.Fatal(err)
	}
}
