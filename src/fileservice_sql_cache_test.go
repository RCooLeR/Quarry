package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	sqlanalyze "github.com/quarry/quarry-wails3/internal/plugins/sql/analyze"
)

func TestSQLAnalysisCacheEvictsLeastRecentlyUsedSummary(t *testing.T) {
	svc := NewFileService()
	ids := make([]string, 0, maxCachedSQLSummaries+1)
	for index := 0; index < maxCachedSQLSummaries+1; index++ {
		path := filepath.Join(t.TempDir(), fmt.Sprintf("dump-%d.sql", index))
		content := fmt.Sprintf("CREATE TABLE `table_%d` (`id` int);\n", index)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		meta, err := svc.OpenFile(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = svc.CloseFile(meta.FileID) })
		if _, err := svc.sqlAnalyzeWithOptions(context.Background(), meta.FileID, sqlanalyze.Options{ChunkSize: 1024}); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, meta.FileID)
	}

	if got := sqlCacheEntryCount(svc); got != maxCachedSQLSummaries {
		t.Fatalf("cache entries = %d, want hard cap %d", got, maxCachedSQLSummaries)
	}
	if _, err := svc.sqlSummaryForOptions(ids[0], sqlanalyze.Options{ChunkSize: 1024}); !errors.Is(err, ErrSQLAnalysisRequired) {
		t.Fatalf("oldest cache lookup error = %v, want ErrSQLAnalysisRequired", err)
	}
	for _, fileID := range ids[1:] {
		lease, err := svc.sqlSummaryForOptions(fileID, sqlanalyze.Options{ChunkSize: 1024})
		if err != nil {
			t.Fatalf("retained cache lookup %q: %v", fileID, err)
		}
		lease.Release()
	}
}
