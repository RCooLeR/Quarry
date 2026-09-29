package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSqlReplaceViaDialogRecountsWordPressSerializedStringLengths(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "wordpress.sql")
	outputPath := filepath.Join(dir, "replaced.sql")
	source := []byte("INSERT INTO `wp_options` VALUES (1,'siteurl','a:2:{s:3:\"url\";s:15:\"http://old.test\";s:5:\"emoji\";s:4:\"\xf0\x9f\x98\x80\";}');\n")
	want := []byte("INSERT INTO `wp_options` VALUES (1,'siteurl','a:2:{s:3:\"url\";s:18:\"http://new.example\";s:5:\"emoji\";s:4:\"\xf0\x9f\x98\x80\";}');\n")
	if err := os.WriteFile(sourcePath, source, 0o600); err != nil {
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

	result, err := svc.SqlReplaceViaDialog(meta.FileID, "old.test", "new.example", false, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if result.OutputPath != outputPath || result.RecordsWritten != 1 {
		t.Fatalf("result = %#v", result)
	}
	assertFileBytes(t, sourcePath, source)
	assertFileBytes(t, outputPath, want)
}

func TestSqlReplaceViaDialogHandlesOrdinarySQLWithoutAnalysis(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "plain.sql")
	outputPath := filepath.Join(dir, "plain-replaced.sql")
	source := []byte("UPDATE settings SET value='http://old.test', name=`old_prefix_options`;\n")
	want := []byte("UPDATE settings SET value='http://new.example', name=`old_prefix_options`;\n")
	if err := os.WriteFile(sourcePath, source, 0o600); err != nil {
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

	result, err := svc.SqlReplaceViaDialog(meta.FileID, "old.test", "new.example", false, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if result.RecordsWritten != 1 || !strings.Contains(result.Note, "serialized string lengths recalculated") {
		t.Fatalf("result = %#v", result)
	}
	assertFileBytes(t, sourcePath, source)
	assertFileBytes(t, outputPath, want)
}

func TestSqlReplaceViaDialogDoesNotRewriteCommentsOrIdentifiers(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "plain.sql")
	outputPath := filepath.Join(dir, "plain-replaced.sql")
	source := []byte("-- http://old.test in comment\nCREATE TABLE old_test (url text);\nINSERT INTO old_test VALUES ('http://old.test');\n")
	want := []byte("-- http://old.test in comment\nCREATE TABLE old_test (url text);\nINSERT INTO old_test VALUES ('http://new.example');\n")
	if err := os.WriteFile(sourcePath, source, 0o600); err != nil {
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

	result, err := svc.SqlReplaceViaDialog(meta.FileID, "old.test", "new.example", false, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if result.RecordsWritten != 1 {
		t.Fatalf("result = %#v", result)
	}
	assertFileBytes(t, sourcePath, source)
	assertFileBytes(t, outputPath, want)
}

func TestSqlReplaceViaDialogRejectsRegexBeforeDialog(t *testing.T) {
	svc := NewFileService()
	dialogCalls := 0
	previousDialog := sqlSaveDialog
	sqlSaveDialog = func(string, string) (string, error) {
		dialogCalls++
		return "unexpected.sql", nil
	}
	t.Cleanup(func() { sqlSaveDialog = previousDialog })

	_, err := svc.SqlReplaceViaDialog("unknown-file", "old[.]test", "new.example", true, false, false)
	if !errors.Is(err, ErrSQLRegexReplaceUnsupported) {
		t.Fatalf("error = %v, want ErrSQLRegexReplaceUnsupported", err)
	}
	if dialogCalls != 0 {
		t.Fatalf("regex rejection opened %d save dialog(s), want 0", dialogCalls)
	}
}

func TestSqlReplaceViaDialogRejectsMalformedSerializedValueWithoutPublishing(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "bad.sql")
	outputPath := filepath.Join(dir, "bad-replaced.sql")
	source := []byte("INSERT INTO `wp_options` VALUES ('s:99:\"http://old.test');\n")
	if err := os.WriteFile(sourcePath, source, 0o600); err != nil {
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

	_, err = svc.SqlReplaceViaDialog(meta.FileID, "old.test", "new.example", false, false, false)
	if err == nil || !strings.Contains(err.Error(), "unsupported or malformed PHP/WordPress serialized data") {
		t.Fatalf("error = %v, want malformed serialized rejection", err)
	}
	assertFileBytes(t, sourcePath, source)
	assertNoPublishedOrTempOutput(t, outputPath)
}

func TestSqlReplaceViaDialogRejectsRoutineContextWithoutPublishing(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "routine.sql")
	outputPath := filepath.Join(dir, "routine-replaced.sql")
	source := []byte("DELIMITER $$\nCREATE PROCEDURE p()\nBEGIN\nSELECT 'http://old.test';\nEND$$\nDELIMITER ;\n")
	if err := os.WriteFile(sourcePath, source, 0o600); err != nil {
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

	_, err = svc.SqlReplaceViaDialog(meta.FileID, "old.test", "new.example", false, false, false)
	if err == nil || !strings.Contains(err.Error(), "unsupported SQL dump context") {
		t.Fatalf("error = %v, want unsupported SQL context rejection", err)
	}
	assertFileBytes(t, sourcePath, source)
	assertNoPublishedOrTempOutput(t, outputPath)
}
