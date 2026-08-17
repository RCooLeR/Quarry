package main

import (
	"errors"
	"math"
	"strings"
	"testing"

	sqlanalyze "github.com/quarry/quarry-wails3/internal/plugins/sql/analyze"
	sqlextract "github.com/quarry/quarry-wails3/internal/plugins/sql/extract"
)

func TestSQLExtractRPCsBoundTableNameBeforeServiceOrDialog(t *testing.T) {
	previousDialog := sqlSaveDialog
	dialogCalls := 0
	sqlSaveDialog = func(string, string) (string, error) {
		dialogCalls++
		return "unused", nil
	}
	t.Cleanup(func() { sqlSaveDialog = previousDialog })

	tableName := strings.Repeat("x", sqlextract.MaxTableSelectionBytes+1)
	var service *FileService
	checks := []struct {
		name string
		call func() error
	}{
		{"table", func() error { _, err := service.SqlExtractTableViaDialog("f1", tableName); return err }},
		{"schema", func() error { _, err := service.SqlExtractSchemaViaDialog("f1", tableName); return err }},
		{"data", func() error { _, err := service.SqlExtractDataViaDialog("f1", tableName); return err }},
	}
	for _, check := range checks {
		if err := check.call(); !errors.Is(err, sqlextract.ErrTableNameTooLong) {
			t.Fatalf("%s error = %v, want %v", check.name, err, sqlextract.ErrTableNameTooLong)
		}
	}
	if dialogCalls != 0 {
		t.Fatalf("oversized table names opened save dialog %d times", dialogCalls)
	}
}

func TestSQLTableSelectionAllowsLargestQualifiedAnalyzerIdentity(t *testing.T) {
	component := strings.Repeat("`", sqlanalyze.MaxIdentifierBytes)
	display := "`" + strings.ReplaceAll(component, "`", "``") + "`.`" + strings.ReplaceAll(component, "`", "``") + "`"
	if len(display) != sqlextract.MaxTableSelectionBytes {
		t.Fatalf("qualified display length = %d, want %d", len(display), sqlextract.MaxTableSelectionBytes)
	}
	if err := validateSQLTableSelection(display, false); err != nil {
		t.Fatalf("largest analyzer identity rejected: %v", err)
	}
}

func TestSQLReshapeValidatesBridgeValuesBeforeDialog(t *testing.T) {
	previousDialog := sqlSaveDialog
	dialogCalls := 0
	sqlSaveDialog = func(string, string) (string, error) {
		dialogCalls++
		return "unused", nil
	}
	t.Cleanup(func() { sqlSaveDialog = previousDialog })

	var service *FileService
	if _, err := service.SqlReshapeInsertsViaDialog("missing", "single", 10); !errors.Is(err, ErrInvalidFileID) {
		t.Fatalf("invalid file id error = %v, want %v", err, ErrInvalidFileID)
	}
	if _, err := service.SqlReshapeInsertsViaDialog("f1", strings.Repeat("x", maxRPCEnumBytes+1), 10); !errors.Is(err, ErrRPCValueTooLong) {
		t.Fatalf("oversized mode error = %v, want %v", err, ErrRPCValueTooLong)
	}
	if _, err := service.SqlReshapeInsertsViaDialog("f1", "future", 10); !errors.Is(err, ErrSQLReshapeModeInvalid) {
		t.Fatalf("invalid mode error = %v, want %v", err, ErrSQLReshapeModeInvalid)
	}
	if _, err := service.SqlReshapeInsertsViaDialog("f1", "single", -1); err == nil || !strings.Contains(err.Error(), "negative") {
		t.Fatalf("negative batch size error = %v", err)
	}
	if _, err := service.SqlReshapeInsertsViaDialog("f1", "single", math.MaxInt); err == nil || !strings.Contains(err.Error(), "exceeds limit") {
		t.Fatalf("oversized batch size error = %v", err)
	}
	if dialogCalls != 0 {
		t.Fatalf("invalid reshape request opened save dialog %d times", dialogCalls)
	}
}

func TestSQLReshapeRejectsUnknownValidFileIDBeforeDialog(t *testing.T) {
	previousDialog := sqlSaveDialog
	dialogCalls := 0
	sqlSaveDialog = func(string, string) (string, error) {
		dialogCalls++
		return "unused", nil
	}
	t.Cleanup(func() { sqlSaveDialog = previousDialog })

	service := NewFileService()
	if _, err := service.SqlReshapeInsertsViaDialog("f999", "single", 10); err == nil || !strings.Contains(err.Error(), "unknown file id") {
		t.Fatalf("unknown file error = %v", err)
	}
	if dialogCalls != 0 {
		t.Fatalf("unknown file opened save dialog %d times", dialogCalls)
	}
}
