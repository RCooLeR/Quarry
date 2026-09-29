package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	sqlanalyze "github.com/quarry/quarry-wails3/internal/plugins/sql/analyze"
	sqlschemadiff "github.com/quarry/quarry-wails3/internal/plugins/sql/schemadiff"
	"github.com/quarry/quarry-wails3/internal/session"
)

func TestSqlSchemaDiffCarriesFullSemanticChangesThroughService(t *testing.T) {
	svc := NewFileService()
	oldID := openAndAnalyzeSchemaDiffFixture(t, svc, "old.sql", `CREATE TABLE users (
 id int NOT NULL DEFAULT 'ABC',
 name varchar(20),
 PRIMARY KEY(id)
) ENGINE=InnoDB;`)
	newID := openAndAnalyzeSchemaDiffFixture(t, svc, "new.sql", `CREATE TABLE users (
 name varchar(40),
 id int NOT NULL DEFAULT 'abc',
 UNIQUE KEY uq_name(name)
) ENGINE=MyISAM;`)

	result, err := svc.SqlSchemaDiff(oldID, newID)
	if err != nil {
		t.Fatal(err)
	}
	if result.UnchangedCount != 0 || len(result.ChangedTables) != 1 {
		t.Fatalf("service diff = %#v, want one changed table", result)
	}
	change := result.ChangedTables[0]
	if change.Status != sqlschemadiff.DiffChanged || len(change.ChangedColumns) != 2 || !change.ColumnOrderChanged ||
		len(change.RemovedConstraints) != 1 || len(change.AddedIndexes) != 1 || len(change.ChangedOptions) != 1 {
		t.Fatalf("service lost semantic dimensions: %#v", change)
	}
}

func TestSqlSchemaDiffSurfacesUnsupportedDDLAsUnknownNotUnchanged(t *testing.T) {
	svc := NewFileService()
	unsupported := "CREATE TABLE t (id int MASKED WITH (FUNCTION='mask')) ENGINE=InnoDB;"
	oldID := openAndAnalyzeSchemaDiffFixture(t, svc, "old-unknown.sql", unsupported)
	newID := openAndAnalyzeSchemaDiffFixture(t, svc, "new-unknown.sql", unsupported)

	result, err := svc.SqlSchemaDiff(oldID, newID)
	if err != nil {
		t.Fatal(err)
	}
	if result.UnchangedCount != 0 || len(result.ChangedTables) != 1 {
		t.Fatalf("unsupported service diff = %#v, want explicit unknown table", result)
	}
	if result.ChangedTables[0].Status != sqlschemadiff.DiffUnknown || result.ChangedTables[0].Reason == "" {
		t.Fatalf("unsupported DDL was not surfaced as unknown: %#v", result.ChangedTables[0])
	}
}

func TestSqlSchemaDiffSameFileUsesOneLeaseAndReturnsUnchanged(t *testing.T) {
	svc := NewFileService()
	fileID := openAndAnalyzeSchemaDiffFixture(t, svc, "same.sql", `CREATE TABLE users (id int PRIMARY KEY);`)
	leaseCount := 0
	restore := installServiceLeaseHook(func(operation string, hookedFileID string, _ session.FileSnapshot) {
		if operation == "sql-schema-diff" && hookedFileID == fileID {
			leaseCount++
		}
	})
	defer restore()

	result, err := svc.SqlSchemaDiff(fileID, fileID)
	if err != nil {
		t.Fatal(err)
	}
	if leaseCount != 1 {
		t.Fatalf("same-file schema diff acquired %d leases, want exactly one", leaseCount)
	}
	if result.UnchangedCount != 1 || len(result.AddedTables) != 0 || len(result.RemovedTables) != 0 || len(result.ChangedTables) != 0 {
		t.Fatalf("same-file schema diff = %#v, want one unchanged table", result)
	}
	assertNoSchemaDiffOwnership(t, svc, fileID)
}

func TestSqlSchemaDiffAcquiresBothArgumentOrdersCanonically(t *testing.T) {
	svc := NewFileService()
	fileA := openAndAnalyzeSchemaDiffFixture(t, svc, "a.sql", `CREATE TABLE a (id int);`)
	fileB := openAndAnalyzeSchemaDiffFixture(t, svc, "b.sql", `CREATE TABLE b (id int);`)
	want := []string{fileA, fileB}
	sort.Strings(want)

	for _, args := range [][2]string{{fileA, fileB}, {fileB, fileA}} {
		var acquired []string
		restore := installServiceLeaseHook(func(operation string, fileID string, _ session.FileSnapshot) {
			if operation == "sql-schema-diff" {
				acquired = append(acquired, fileID)
			}
		})
		result, err := svc.SqlSchemaDiff(args[0], args[1])
		restore()
		if err != nil {
			t.Fatalf("SqlSchemaDiff(%q, %q): %v", args[0], args[1], err)
		}
		if len(acquired) != 2 || acquired[0] != want[0] || acquired[1] != want[1] {
			t.Fatalf("SqlSchemaDiff(%q, %q) lease order = %v, want %v", args[0], args[1], acquired, want)
		}
		if result.FileA == result.FileB {
			t.Fatalf("SqlSchemaDiff(%q, %q) lost argument mapping: %#v", args[0], args[1], result)
		}
	}
}

func TestSqlSchemaDiffRejectsAggregateBudgetsBeforeAnyDDLRead(t *testing.T) {
	t.Run("input", func(t *testing.T) {
		svc := NewFileService()
		data := append([]byte("CREATE TABLE t (id int);"), bytes.Repeat([]byte{' '}, sqlschemadiff.MaxStatementBytes+128)...)
		fileID := openSchemaDiffFixture(t, svc, "input-budget.sql", data)
		tables := make([]sqlanalyze.Table, 33)
		for i := range tables {
			tables[i] = sqlanalyze.Table{
				Name:         fmt.Sprintf("t%d", i),
				CreateOffset: 0,
				InsertOffset: -1,
				Regions: []sqlanalyze.Region{{
					Kind:        sqlanalyze.RegionCreate,
					StartOffset: 0,
					EndOffset:   int64(len(data)),
				}},
			}
		}
		installSchemaDiffSummary(t, svc, fileID, sqlanalyze.Summary{Tables: tables, CreateTables: len(tables)})

		reads := 0
		restore := installSQLSchemaDiffReadHook(func(context.Context, string, int64, int64) error {
			reads++
			return nil
		})
		defer restore()
		if _, err := svc.SqlSchemaDiff(fileID, fileID); !errors.Is(err, ErrSQLSchemaDiffBudget) {
			t.Fatalf("SqlSchemaDiff error = %v, want ErrSQLSchemaDiffBudget", err)
		}
		if reads != 0 {
			t.Fatalf("over-budget plan performed %d DDL reads, want zero", reads)
		}
		assertNoSchemaDiffOwnership(t, svc, fileID)
	})

	t.Run("retained state", func(t *testing.T) {
		svc := NewFileService()
		fileID := openSchemaDiffFixture(t, svc, "retained-budget.sql", []byte("x"))
		name := strings.Repeat("n", 8<<10)
		tables := make([]sqlanalyze.Table, sqlschemadiff.MaxDiffTableCount)
		for i := range tables {
			tables[i] = sqlanalyze.Table{Name: name, CreateOffset: -1, InsertOffset: 0}
		}
		installSchemaDiffSummary(t, svc, fileID, sqlanalyze.Summary{Tables: tables, InsertTables: len(tables)})

		reads := 0
		restore := installSQLSchemaDiffReadHook(func(context.Context, string, int64, int64) error {
			reads++
			return nil
		})
		defer restore()
		if _, err := svc.SqlSchemaDiff(fileID, fileID); !errors.Is(err, ErrSQLSchemaDiffBudget) {
			t.Fatalf("SqlSchemaDiff error = %v, want retained-memory budget rejection", err)
		}
		if reads != 0 {
			t.Fatalf("retained-state rejection performed %d DDL reads, want zero", reads)
		}
		assertNoSchemaDiffOwnership(t, svc, fileID)
	})
}

func TestSqlSchemaDiffCapsEveryStatementReadAtParserLimitPlusOne(t *testing.T) {
	svc := NewFileService()
	data := append([]byte("CREATE TABLE t (id int);"), bytes.Repeat([]byte{' '}, sqlschemadiff.MaxStatementBytes+128)...)
	fileID := openSchemaDiffFixture(t, svc, "statement-cap.sql", data)
	installSchemaDiffSummary(t, svc, fileID, sqlanalyze.Summary{
		Tables: []sqlanalyze.Table{{
			Name:         "t",
			CreateOffset: 0,
			InsertOffset: -1,
			Regions: []sqlanalyze.Region{{
				Kind:        sqlanalyze.RegionCreate,
				StartOffset: 0,
				EndOffset:   int64(len(data)),
			}},
		}},
		CreateTables: 1,
	})

	requested := make([]int64, 0, 1)
	restore := installSQLSchemaDiffReadHook(func(_ context.Context, hookedFileID string, start, end int64) error {
		if hookedFileID != fileID {
			t.Fatalf("read hook file = %q, want %q", hookedFileID, fileID)
		}
		requested = append(requested, end-start)
		return nil
	})
	defer restore()
	result, err := svc.SqlSchemaDiff(fileID, fileID)
	if err != nil {
		t.Fatal(err)
	}
	if len(requested) != 1 || requested[0] != int64(sqlschemadiff.MaxStatementBytes+1) {
		t.Fatalf("statement reads = %v, want exactly [%d]", requested, sqlschemadiff.MaxStatementBytes+1)
	}
	if result.UnchangedCount != 0 || len(result.ChangedTables) != 1 || result.ChangedTables[0].Status != sqlschemadiff.DiffUnknown {
		t.Fatalf("over-limit DDL result = %#v, want explicit unknown", result)
	}
}

func TestSqlSchemaDiffCancelJobReleasesJobForegroundAndLeaseOwnership(t *testing.T) {
	svc := NewFileService()
	fileID := openAndAnalyzeSchemaDiffFixture(t, svc, "cancel.sql", `CREATE TABLE t (id int);`)
	entered := make(chan struct{})
	var once sync.Once
	restore := installSQLSchemaDiffReadHook(func(ctx context.Context, _ string, _, _ int64) error {
		once.Do(func() { close(entered) })
		<-ctx.Done()
		return ctx.Err()
	})
	t.Cleanup(restore)

	done := make(chan error, 1)
	go func() {
		_, err := svc.SqlSchemaDiff(fileID, fileID)
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("schema diff did not reach its bounded read")
	}
	jobID := activeJobIDForTest(t, svc)
	if err := svc.CancelJob(jobID); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, ErrJobCancelled) || !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled diff error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled schema diff did not return")
	}
	restore()
	assertNoSchemaDiffOwnership(t, svc, fileID)
	lease, err := svc.reg.AcquireExclusive(fileID)
	if err != nil {
		t.Fatalf("schema diff leaked a registry lease: %v", err)
	}
	lease.Release()
	if _, err := svc.SqlSchemaDiff(fileID, fileID); err != nil {
		t.Fatalf("schema diff after cancellation: %v", err)
	}
}

func TestSqlSchemaDiffLifecycleCancelsEitherInput(t *testing.T) {
	for _, operation := range []string{"close", "refresh"} {
		for _, targetIndex := range []int{0, 1} {
			operation, targetIndex := operation, targetIndex
			t.Run(fmt.Sprintf("%s-input-%d", operation, targetIndex+1), func(t *testing.T) {
				svc := NewFileService()
				fileIDs := []string{
					openAndAnalyzeSchemaDiffFixture(t, svc, "old.sql", `CREATE TABLE t (id int);`),
					openAndAnalyzeSchemaDiffFixture(t, svc, "new.sql", `CREATE TABLE t (id bigint);`),
				}
				entered := make(chan struct{})
				var once sync.Once
				restore := installSQLSchemaDiffReadHook(func(ctx context.Context, _ string, _, _ int64) error {
					once.Do(func() { close(entered) })
					<-ctx.Done()
					return ctx.Err()
				})
				defer restore()

				diffDone := make(chan error, 1)
				go func() {
					_, err := svc.SqlSchemaDiff(fileIDs[0], fileIDs[1])
					diffDone <- err
				}()
				select {
				case <-entered:
				case <-time.After(5 * time.Second):
					t.Fatal("schema diff did not reach its bounded read")
				}

				lifecycleDone := make(chan error, 1)
				go func() {
					if operation == "close" {
						lifecycleDone <- svc.CloseFile(fileIDs[targetIndex])
						return
					}
					_, err := svc.RefreshFile(fileIDs[targetIndex])
					lifecycleDone <- err
				}()
				select {
				case err := <-diffDone:
					if !errors.Is(err, ErrJobCancelled) || !errors.Is(err, context.Canceled) {
						t.Fatalf("lifecycle-canceled diff error = %v", err)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("schema diff was not canceled by input lifecycle")
				}
				select {
				case err := <-lifecycleDone:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("lifecycle operation did not drain schema diff")
				}
				assertNoSchemaDiffOwnership(t, svc, fileIDs...)
			})
		}
	}
}

func openAndAnalyzeSchemaDiffFixture(t *testing.T, svc *FileService, name string, ddl string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(ddl), 0o600); err != nil {
		t.Fatal(err)
	}
	meta, err := svc.OpenFile(path)
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
	if len(result.Tables) != 1 {
		t.Fatalf("analysis tables = %#v, want one", result.Tables)
	}
	return meta.FileID
}

func openSchemaDiffFixture(t *testing.T, svc *FileService, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	meta, err := svc.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, ok := svc.reg.Get(meta.FileID); ok {
			_ = svc.CloseFile(meta.FileID)
		}
	})
	return meta.FileID
}

func installSchemaDiffSummary(t *testing.T, svc *FileService, fileID string, summary sqlanalyze.Summary) {
	t.Helper()
	lease, current, err := svc.acquireReadFile(fileID)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := lease.Snapshot()
	entry := sqlAnalysisEntry{
		Summary:    summary,
		FileID:     fileID,
		Path:       snapshot.Path,
		Document:   current.Doc,
		Generation: snapshot.Generation,
		Source:     current.Doc.OriginalFileState(),
		Options:    sqlAnalysisKey(sqlanalyze.Options{}),
	}
	lease.Release()
	svc.sqlMu.Lock()
	svc.sqlUseSeq++
	entry.LastUsed = svc.sqlUseSeq
	svc.sqlSummary[fileID] = entry
	svc.sqlMu.Unlock()
}

func assertNoSchemaDiffOwnership(t *testing.T, svc *FileService, fileIDs ...string) {
	t.Helper()
	manager := svc.jobs()
	manager.mu.Lock()
	active := manager.active
	manager.mu.Unlock()
	if active != nil {
		t.Fatalf("schema diff retained active job %q", active.id)
	}
	svc.foregroundMu.Lock()
	defer svc.foregroundMu.Unlock()
	if svc.foregroundRunCount != 0 {
		t.Fatalf("schema diff retained %d foreground runs", svc.foregroundRunCount)
	}
	for _, fileID := range fileIDs {
		if len(svc.foregroundRuns[fileID]) != 0 {
			t.Fatalf("schema diff retained foreground ownership for %q", fileID)
		}
	}
}
