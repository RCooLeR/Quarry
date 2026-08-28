package extract

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestWriteManifestJSONPreservesV1WireFormat(t *testing.T) {
	summary := &WriteSummary{
		Operation:            "sql-split-by-table",
		SourcePath:           `<source & dump.sql>`,
		SourceSize:           128,
		ManifestPath:         "manifest.json",
		Outputs:              nil,
		Complete:             true,
		HeaderIncluded:       false,
		PublicationUncertain: false,
	}
	want, err := json.MarshalIndent(summary, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	want = append(want, '\n')

	var got bytes.Buffer
	if err := writeManifestJSON(&got, summary); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Bytes(), want) {
		t.Fatalf("streamed SQL manifest changed v1 wire format\ngot:  %s\nwant: %s", got.Bytes(), want)
	}
}
