package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/quarry/quarry-wails3/internal/document"
	"github.com/quarry/quarry-wails3/internal/fileio"
	sqlextract "github.com/quarry/quarry-wails3/internal/plugins/sql/extract"
	sqlreshape "github.com/quarry/quarry-wails3/internal/plugins/sql/reshape"
	"github.com/quarry/quarry-wails3/internal/sourceio"
)

func TestExportRangesToPathPublishesOnlyCompleteOutput(t *testing.T) {
	source := []byte("alpha\nbeta\ngamma\n")
	path := writeTempFile(t, "source.sql", source)
	svc := NewFileService()
	meta, err := svc.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.CloseFile(meta.FileID)
	f, _ := svc.reg.Get(meta.FileID)
	dst := filepath.Join(t.TempDir(), "ranges.sql")

	written, err := svc.exportRangesToPath(context.Background(), f.Doc, dst, [][2]int64{{0, 6}, {11, 17}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if written != 12 {
		t.Fatalf("written = %d, want 12", written)
	}
	if got, err := os.ReadFile(dst); err != nil || string(got) != "alpha\ngamma\n" {
		t.Fatalf("output = %q, err %v", got, err)
	}
}

func TestExportRangesToPathRejectsSourceAliasesAndExistingDestination(t *testing.T) {
	source := []byte("source bytes")
	path := writeTempFile(t, "source.sql", source)
	svc := NewFileService()
	meta, err := svc.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.CloseFile(meta.FileID)
	f, _ := svc.reg.Get(meta.FileID)

	aliases := []struct {
		name string
		path func(t *testing.T) string
	}{
		{name: "same path", path: func(*testing.T) string { return path }},
		{name: "hard link", path: func(t *testing.T) string {
			alias := filepath.Join(filepath.Dir(path), "source-hardlink.sql")
			if err := os.Link(path, alias); err != nil {
				t.Skipf("hard links unavailable: %v", err)
			}
			return alias
		}},
		{name: "symlink", path: func(t *testing.T) string {
			alias := filepath.Join(filepath.Dir(path), "source-symlink.sql")
			if err := os.Symlink(path, alias); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			return alias
		}},
	}
	for _, tt := range aliases {
		t.Run(tt.name, func(t *testing.T) {
			_, err := svc.exportRangesToPath(context.Background(), f.Doc, tt.path(t), [][2]int64{{0, int64(len(source))}}, nil)
			if !errors.Is(err, fileio.ErrSourceAlias) {
				t.Fatalf("error = %v, want ErrSourceAlias", err)
			}
			if got, readErr := os.ReadFile(path); readErr != nil || string(got) != string(source) {
				t.Fatalf("source = %q, err %v", got, readErr)
			}
		})
	}

	dst := filepath.Join(t.TempDir(), "existing.sql")
	if err := os.WriteFile(dst, []byte("sentinel"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.exportRangesToPath(context.Background(), f.Doc, dst, [][2]int64{{0, int64(len(source))}}, nil); !errors.Is(err, fileio.ErrExists) {
		t.Fatalf("error = %v, want ErrExists", err)
	}
	if got, err := os.ReadFile(dst); err != nil || string(got) != "sentinel" {
		t.Fatalf("destination = %q, err %v", got, err)
	}
}

func TestExportRangesToPathCancellationAndReadFailureLeaveNoArtifact(t *testing.T) {
	path := writeTempFile(t, "source.sql", []byte("source bytes"))
	svc := NewFileService()
	meta, err := svc.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	f, _ := svc.reg.Get(meta.FileID)
	dir := t.TempDir()
	dst := filepath.Join(dir, "cancelled.sql")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := svc.exportRangesToPath(ctx, f.Doc, dst, [][2]int64{{0, f.Doc.Size()}}, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error = %v, want context.Canceled", err)
	}
	assertNoPublishedOrTempOutput(t, dst)

	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}
	dst = filepath.Join(dir, "read-failed.sql")
	if _, err := svc.exportRangesToPath(context.Background(), f.Doc, dst, [][2]int64{{0, f.Doc.Size()}}, nil); !errors.Is(err, document.ErrSourceChanged) {
		t.Fatalf("truncated source error = %v, want ErrSourceChanged", err)
	}
	assertNoPublishedOrTempOutput(t, dst)
	_ = svc.CloseFile(meta.FileID)
}

func TestExportRangesToPathValidatedFailureLeavesNoArtifact(t *testing.T) {
	path := writeTempFile(t, "source.sql", []byte("source bytes"))
	svc := NewFileService()
	meta, err := svc.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.CloseFile(meta.FileID)
	f, _ := svc.reg.Get(meta.FileID)
	dst := filepath.Join(t.TempDir(), "rejected.sql")
	validationErr := errors.New("analysis generation changed")

	_, err = svc.exportRangesToPathValidated(context.Background(), f.Doc, dst, [][2]int64{{0, f.Doc.Size()}}, nil, func(context.Context) error {
		if _, statErr := os.Lstat(dst); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("final output visible before validation: %v", statErr)
		}
		return validationErr
	})
	if !errors.Is(err, validationErr) {
		t.Fatalf("error = %v, want validation failure", err)
	}
	assertNoPublishedOrTempOutput(t, dst)
}

func TestSQLTransformResultPreservesOnlyKnownPublishedOutput(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "published.sql")
	base := TransformResult{OutputPath: dst, RecordsWritten: 3, Note: "3 ranges — 12 B"}
	sentinel := errors.New("directory sync failed")

	t.Run("known requested output", func(t *testing.T) {
		publication := &fileio.PublicationError{FinalPath: dst, Durable: false, Err: sentinel}
		result, err := transformResultAfterPublication(base, publication)
		if !errors.Is(err, sentinel) {
			t.Fatalf("err = %v, want publication warning", err)
		}
		if result.OutputPath != dst || result.RecordsWritten != base.RecordsWritten {
			t.Fatalf("result = %#v, want known published output evidence", result)
		}
		if !strings.Contains(result.Note, "finalization warning") {
			t.Fatalf("note = %q, want publication warning", result.Note)
		}
	})

	t.Run("uncertain location", func(t *testing.T) {
		publication := &fileio.PublicationError{FinalPath: dst, LocationUncertain: true, Err: sentinel}
		result, err := transformResultAfterPublication(base, publication)
		if !errors.Is(err, sentinel) {
			t.Fatalf("err = %v, want publication warning", err)
		}
		if result != (TransformResult{}) {
			t.Fatalf("result = %#v, want no claim for uncertain location", result)
		}
	})

	t.Run("different published path", func(t *testing.T) {
		publication := &fileio.PublicationError{FinalPath: dst + ".other", Durable: true, Err: sentinel}
		result, err := transformResultAfterPublication(base, publication)
		if !errors.Is(err, sentinel) {
			t.Fatalf("err = %v, want publication warning", err)
		}
		if result != (TransformResult{}) {
			t.Fatalf("result = %#v, want no evidence for a different path", result)
		}
	})
}

func TestSQLReshapeServiceRejectsRawCopyPayloadWithoutPublication(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "copy.sql")
	outputPath := filepath.Join(dir, "must-not-exist.sql")
	source := "INSERT INTO safe VALUES (0);\n" +
		"COPY imported(value) FROM STDIN;\n" +
		"INSERT INTO payload VALUES (1),(2);\n" +
		"\\.\n"
	if err := os.WriteFile(sourcePath, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	svc := NewFileService()
	meta, err := svc.OpenFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.CloseFile(meta.FileID) })

	previousDialog := sqlSaveDialog
	sqlSaveDialog = func(string, string) (string, error) { return outputPath, nil }
	t.Cleanup(func() { sqlSaveDialog = previousDialog })

	result, err := svc.SqlReshapeInsertsViaDialog(meta.FileID, "single", 100)
	if !errors.Is(err, sqlreshape.ErrUnsupportedCompoundStatement) {
		t.Fatalf("error = %v, want %v", err, sqlreshape.ErrUnsupportedCompoundStatement)
	}
	if result != (TransformResult{}) {
		t.Fatalf("result = %#v, want no output evidence", result)
	}
	assertNoPublishedOrTempOutput(t, outputPath)
	if got, readErr := os.ReadFile(sourcePath); readErr != nil || string(got) != source {
		t.Fatalf("source = %q, err %v", got, readErr)
	}
}

func TestSQLSplitTransformResultPreservesRetainedOutputsOnError(t *testing.T) {
	dir := t.TempDir()
	sentinel := errors.New("split stopped")
	retained := sqlextract.WriteSummary{
		Outputs: []sqlextract.TableRange{
			{Name: "users", OutputPath: filepath.Join(dir, "users.sql")},
			{Name: "orders", OutputPath: filepath.Join(dir, "orders.sql")},
		},
		BytesWritten: 42,
	}

	result, err := sqlSplitTransformResult(dir, retained, sentinel)
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want split failure", err)
	}
	if result.OutputPath != dir || result.RecordsWritten != 2 {
		t.Fatalf("result = %#v, want retained output evidence", result)
	}
	if !strings.Contains(result.Note, "retained") {
		t.Fatalf("note = %q, want retained-output status", result.Note)
	}

	retained.Complete = true
	result, err = sqlSplitTransformResult(dir, retained, sentinel)
	if !errors.Is(err, sentinel) || result.OutputPath != dir || result.RecordsWritten != 2 {
		t.Fatalf("complete publication result = %#v, err %v", result, err)
	}
	if !strings.Contains(result.Note, "completion manifest published") {
		t.Fatalf("note = %q, want completion-manifest warning", result.Note)
	}

	result, err = sqlSplitTransformResult(dir, sqlextract.WriteSummary{}, sentinel)
	if !errors.Is(err, sentinel) || result != (TransformResult{}) {
		t.Fatalf("empty split result = %#v, err %v; want zero result", result, err)
	}
}

func TestExportRangesRejectsSameSizeRewriteWithRestoredMtimeBeforePublication(t *testing.T) {
	original := []byte("CREATE TABLE t(id INT);\n")
	changed := []byte("CREATE TABLE x(id INT);\n")
	if len(changed) != len(original) {
		t.Fatalf("fixture sizes differ: %d != %d", len(changed), len(original))
	}
	path := writeTempFile(t, "source.sql", original)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	svc := NewFileService()
	meta, err := svc.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.CloseFile(meta.FileID)
	f, _ := svc.reg.Get(meta.FileID)
	dst := filepath.Join(t.TempDir(), "rejected.sql")

	_, err = svc.exportRangesToPathValidated(context.Background(), f.Doc, dst, [][2]int64{{0, f.Doc.Size()}}, nil, func(context.Context) error {
		if err := os.WriteFile(path, changed, 0o600); err != nil {
			return err
		}
		return os.Chtimes(path, info.ModTime(), info.ModTime())
	})
	if !errors.Is(err, sourceio.ErrSourceChanged) {
		t.Fatalf("error = %v, want exact source-generation rejection", err)
	}
	assertNoPublishedOrTempOutput(t, dst)
	gotInfo, statErr := os.Stat(path)
	if statErr != nil {
		t.Fatal(statErr)
	}
	if delta := gotInfo.ModTime().Sub(info.ModTime()); delta < -time.Nanosecond || delta > time.Nanosecond {
		t.Fatalf("source mtime was not restored: got %s want %s", gotInfo.ModTime(), info.ModTime())
	}
}

func TestSqlSampleFixtureToPathUsesSafePublication(t *testing.T) {
	source := "SET NAMES utf8;\nCREATE TABLE t (id INT);\nINSERT INTO t VALUES (1),(2),(3);\n"
	path := writeTempFile(t, "source.sql", []byte(source))
	svc := NewFileService()
	meta, err := svc.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.CloseFile(meta.FileID)
	f, _ := svc.reg.Get(meta.FileID)
	create := int64(strings.Index(source, "CREATE TABLE"))
	insert := int64(strings.Index(source, "INSERT INTO"))
	ranges := []sqlextract.TableRange{{
		Name:         "t",
		StartOffset:  create,
		EndOffset:    int64(len(source)),
		CreateOffset: create,
		InsertOffset: insert,
	}}
	dst := filepath.Join(t.TempDir(), "fixture.sql")

	result, err := svc.sqlSampleFixtureToPath(context.Background(), f.Doc, ranges, 2, dst, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.RecordsWritten != 1 || result.OutputPath != dst {
		t.Fatalf("result = %+v", result)
	}
	want := "SET NAMES utf8;\nCREATE TABLE t (id INT);\nINSERT INTO t VALUES (1),(2);\n"
	if got, err := os.ReadFile(dst); err != nil || string(got) != want {
		t.Fatalf("fixture = %q, err %v, want %q", got, err, want)
	}
}

func TestSqlSampleFixtureToPathValidatedFailureLeavesNoArtifact(t *testing.T) {
	source := "CREATE TABLE t (id INT);\nINSERT INTO t VALUES (1),(2);\n"
	path := writeTempFile(t, "source.sql", []byte(source))
	svc := NewFileService()
	meta, err := svc.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.CloseFile(meta.FileID)
	f, _ := svc.reg.Get(meta.FileID)
	insert := int64(strings.Index(source, "INSERT INTO"))
	ranges := []sqlextract.TableRange{{
		Name:         "t",
		StartOffset:  0,
		EndOffset:    int64(len(source)),
		CreateOffset: 0,
		InsertOffset: insert,
	}}
	dst := filepath.Join(t.TempDir(), "rejected-fixture.sql")
	validationErr := errors.New("analysis generation changed")

	_, err = svc.sqlSampleFixtureToPathValidated(context.Background(), f.Doc, ranges, 1, dst, nil, func(context.Context) error {
		if _, statErr := os.Lstat(dst); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("final fixture visible before validation: %v", statErr)
		}
		return validationErr
	})
	if !errors.Is(err, validationErr) {
		t.Fatalf("error = %v, want validation failure", err)
	}
	assertNoPublishedOrTempOutput(t, dst)
}

func TestValidateDocumentSourceForOutputDetectsStateAndIdentityChanges(t *testing.T) {
	t.Run("unchanged", func(t *testing.T) {
		path := writeTempFile(t, "source.sql", []byte("SELECT 1;\n"))
		doc, err := document.OpenFile(path)
		if err != nil {
			t.Fatal(err)
		}
		defer doc.Close()
		if err := validateDocumentSourceForOutput(doc); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("truncated", func(t *testing.T) {
		path := writeTempFile(t, "source.sql", []byte("SELECT 1;\n"))
		doc, err := document.OpenFile(path)
		if err != nil {
			t.Fatal(err)
		}
		defer doc.Close()
		if err := os.Truncate(path, 0); err != nil {
			t.Fatal(err)
		}
		if err := validateDocumentSourceForOutput(doc); !errors.Is(err, document.ErrSourceChanged) {
			t.Fatalf("error = %v, want ErrSourceChanged", err)
		}
	})

	t.Run("same metadata replacement", func(t *testing.T) {
		path := writeTempFile(t, "source.sql", []byte("SELECT 1;\n"))
		doc, err := document.OpenFile(path)
		if err != nil {
			t.Fatal(err)
		}
		defer doc.Close()
		state := doc.OriginalFileState()
		moved := path + ".moved"
		if err := os.Rename(path, moved); err != nil {
			t.Skipf("cannot replace an open file on this filesystem: %v", err)
		}
		if err := os.WriteFile(path, []byte("SELECT 2;\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, state.ModTime, state.ModTime); err != nil {
			t.Fatal(err)
		}
		if err := validateDocumentSourceForOutput(doc); !errors.Is(err, document.ErrSourceChanged) {
			t.Fatalf("error = %v, want ErrSourceChanged", err)
		}
	})
}

func TestSQLCleanupPresetsFailDisabledBeforeOutputSelection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "source.sql")
	source := []byte("INSERT INTO t VALUES ('DEFINER=`kept`');\n")
	if err := os.WriteFile(path, source, 0o600); err != nil {
		t.Fatal(err)
	}
	svc := NewFileService()
	meta, err := svc.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.CloseFile(meta.FileID)

	if got := svc.SqlListPresets(); len(got) != 0 {
		t.Fatalf("advertised presets = %v, want none", got)
	}
	previousDialog := sqlSaveDialog
	dialogCalls := 0
	sqlSaveDialog = func(message, defaultName string) (string, error) {
		dialogCalls++
		return filepath.Join(dir, "must-not-exist.sql"), nil
	}
	t.Cleanup(func() { sqlSaveDialog = previousDialog })
	result, err := svc.SqlApplyPresetViaDialog(meta.FileID, "remove-definers", "", "", "", "")
	if !errors.Is(err, ErrSQLCleanupPresetsDisabled) {
		t.Fatalf("error = %v, want ErrSQLCleanupPresetsDisabled", err)
	}
	if result.OutputPath != "" || result.RecordsWritten != 0 {
		t.Fatalf("result = %#v, want zero unpublished result", result)
	}
	if _, err := svc.SqlApplyPresetViaDialog("missing-file", "remove-definers", "", "", "", ""); !errors.Is(err, ErrSQLCleanupPresetsDisabled) {
		t.Fatalf("invalid-file error = %v, want stable ErrSQLCleanupPresetsDisabled", err)
	}
	if dialogCalls != 0 {
		t.Fatalf("disabled preset opened the save dialog %d times", dialogCalls)
	}
	if got, readErr := os.ReadFile(path); readErr != nil || string(got) != string(source) {
		t.Fatalf("source = %q, err %v", got, readErr)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(path) {
		t.Fatalf("disabled preset created artifacts: %v", entries)
	}
}

func assertNoPublishedOrTempOutput(t *testing.T, dst string) {
	t.Helper()
	if _, err := os.Stat(dst); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unexpected final output %q: %v", dst, err)
	}
	temps, err := filepath.Glob(filepath.Join(filepath.Dir(dst), "."+filepath.Base(dst)+".quarry-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(temps) != 0 {
		t.Fatalf("temporary outputs remain for %q: %v", dst, temps)
	}
}
