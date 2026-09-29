package main

import (
	"errors"
	"testing"
)

func TestSampleParametersRejectNegativesBeforeLookupOrDialog(t *testing.T) {
	previousCSVDialog := csvSaveDialog
	previousSQLDialog := sqlSaveDialog
	dialogCalls := 0
	csvSaveDialog = func(string, string) (string, error) {
		dialogCalls++
		return "unexpected.csv", nil
	}
	sqlSaveDialog = func(string, string) (string, error) {
		dialogCalls++
		return "unexpected.sql", nil
	}
	t.Cleanup(func() {
		csvSaveDialog = previousCSVDialog
		sqlSaveDialog = previousSQLDialog
	})

	service := NewFileService()
	if _, err := service.CsvSampleViaDialog("f1", 1, ",", true, -1); !errors.Is(err, ErrCSVSampleIntervalInvalid) {
		t.Fatalf("negative CSV sample error = %v, want ErrCSVSampleIntervalInvalid", err)
	}
	if _, err := service.SqlSampleFixtureViaDialog("f1", -1); !errors.Is(err, ErrSQLSampleRowsInvalid) {
		t.Fatalf("negative SQL sample error = %v, want ErrSQLSampleRowsInvalid", err)
	}
	if dialogCalls != 0 {
		t.Fatalf("negative sample parameters opened %d dialogs", dialogCalls)
	}

	if _, err := service.CsvSampleViaDialog("f1", 1, ",", true, 0); errors.Is(err, ErrCSVSampleIntervalInvalid) {
		t.Fatalf("zero CSV sample interval rejected instead of using default: %v", err)
	}
	if _, err := service.SqlSampleFixtureViaDialog("f1", 0); errors.Is(err, ErrSQLSampleRowsInvalid) {
		t.Fatalf("zero SQL sample rows rejected instead of using default: %v", err)
	}
}
