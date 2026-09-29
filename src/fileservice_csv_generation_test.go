package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/quarry/quarry-wails3/internal/sourceio"
)

func TestCSVAnalysisResultsCarryExactSessionGeneration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source.csv")
	if err := os.WriteFile(path, []byte("id,name\n1,Ada\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	service := NewFileService()
	meta, err := service.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.CloseFile(meta.FileID) })

	inspect, err := service.CsvInspect(meta.FileID)
	if err != nil {
		t.Fatal(err)
	}
	if inspect.Generation == 0 {
		t.Fatal("CSV inspection returned an empty generation")
	}
	schema, err := service.CsvSchema(meta.FileID, ",", true)
	if err != nil {
		t.Fatal(err)
	}
	preview, err := service.CsvPreview(meta.FileID, ",", true, 10)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := service.CsvProfile(meta.FileID, ",", true)
	if err != nil {
		t.Fatal(err)
	}
	grid, err := service.GetCsvGrid(meta.FileID, ",", 0, csvGridDefaultRawBytes)
	if err != nil {
		t.Fatal(err)
	}
	markdown, err := service.CsvMarkdownPreview(meta.FileID, ",", true, 10)
	if err != nil {
		t.Fatal(err)
	}
	configuredSQL, err := service.CsvToSQLConfigPreview(meta.FileID, CsvSqlConfig{
		Delimiter: ",", HasHeader: true, TableName: "records", InsertMode: "insert",
		BatchSize: 100, OnInvalid: "fail",
		Columns: []CsvSqlColumnConfig{{Source: 0, Name: "id", Type: "TEXT", Include: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	legacySQL, err := service.CsvToSQLPreview(meta.FileID, ",", "records", true, false)
	if err != nil {
		t.Fatal(err)
	}

	for name, generation := range map[string]uint64{
		"schema": schema.Generation, "preview": preview.Generation,
		"profile": profile.Generation, "grid": grid.Generation,
		"markdown": markdown.Generation, "configured SQL": configuredSQL.Generation,
		"SQL": legacySQL.Generation,
	} {
		if generation != inspect.Generation {
			t.Errorf("%s generation = %d, want inspected generation %d", name, generation, inspect.Generation)
		}
	}

	if _, err := service.RefreshFile(meta.FileID); err != nil {
		t.Fatal(err)
	}
	refreshed, err := service.CsvInspect(meta.FileID)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.Generation == inspect.Generation {
		t.Fatalf("refresh retained CSV generation %d", refreshed.Generation)
	}
}

func TestCSVArtifactTransformsRequireGenerationBeforeDialog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source.csv")
	if err := os.WriteFile(path, []byte("id,name\n1,Ada\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	service := NewFileService()
	meta, err := service.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.CloseFile(meta.FileID) })

	previousDialog := csvSaveDialog
	dialogCalls := 0
	csvSaveDialog = func(string, string) (string, error) {
		dialogCalls++
		return filepath.Join(t.TempDir(), "unexpected.csv"), nil
	}
	t.Cleanup(func() { csvSaveDialog = previousDialog })

	for _, test := range csvArtifactTransformCases() {
		if _, err := test.run(service, meta.FileID, 0); !errors.Is(err, ErrCSVSourceGenerationRequired) {
			t.Errorf("%s missing-generation error = %v, want ErrCSVSourceGenerationRequired", test.name, err)
		}
	}
	inspect, err := service.CsvInspect(meta.FileID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.RefreshFile(meta.FileID); err != nil {
		t.Fatal(err)
	}
	for _, test := range csvArtifactTransformCases() {
		if _, err := test.run(service, meta.FileID, inspect.Generation); !errors.Is(err, sourceio.ErrSourceChanged) {
			t.Errorf("%s stale-generation error = %v, want sourceio.ErrSourceChanged", test.name, err)
		}
	}
	if dialogCalls != 0 {
		t.Fatalf("save dialog calls = %d, want 0 for invalid generations", dialogCalls)
	}
}

func TestCSVArtifactTransformsRejectRefreshDuringDialog(t *testing.T) {
	for _, test := range csvArtifactTransformCases() {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			source := filepath.Join(dir, "source.csv")
			destination := filepath.Join(dir, "output")
			sentinel := []byte("existing destination must remain unchanged")
			if err := os.WriteFile(source, []byte("id,value\n1,original\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(destination, sentinel, 0o600); err != nil {
				t.Fatal(err)
			}

			service := NewFileService()
			meta, err := service.OpenFile(source)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = service.CloseFile(meta.FileID) })
			inspect, err := service.CsvInspect(meta.FileID)
			if err != nil {
				t.Fatal(err)
			}

			previousDialog := csvSaveDialog
			dialogCalls := 0
			csvSaveDialog = func(string, string) (string, error) {
				dialogCalls++
				if _, err := service.RefreshFile(meta.FileID); err != nil {
					return "", err
				}
				return destination, nil
			}
			t.Cleanup(func() { csvSaveDialog = previousDialog })

			if _, err := test.run(service, meta.FileID, inspect.Generation); !errors.Is(err, sourceio.ErrSourceChanged) {
				t.Fatalf("error = %v, want source-generation rejection", err)
			}
			if dialogCalls != 1 {
				t.Fatalf("save dialog calls = %d, want 1", dialogCalls)
			}
			assertFileBytes(t, destination, sentinel)
		})
	}
}
