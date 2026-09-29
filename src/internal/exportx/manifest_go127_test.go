package exportx

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"testing"
)

func TestWriteManifestJSONPreservesV1WireFormat(t *testing.T) {
	tests := []struct {
		name  string
		value any
	}{
		{
			name: "export summary",
			value: &Summary{
				SourcePath:        `<source & input>`,
				OutputPath:        "output.json",
				ManifestPath:      "output.manifest.json",
				StartOffset:       1,
				EndOffset:         9,
				BytesWritten:      8,
				Mode:              "byte-range",
				ChecksumAlgorithm: "sha256",
				SHA256:            "abc123",
			},
		},
		{
			name: "split summary with nil collections",
			value: &SplitSummary{
				SourcePath:   "source.txt",
				BasePath:     "part.txt",
				Mode:         "split-size",
				ManifestPath: "part.manifest.json",
				Complete:     true,
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			want, err := json.MarshalIndent(test.value, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			want = append(want, '\n')

			var got bytes.Buffer
			if err := writeManifestJSON(&got, test.value); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got.Bytes(), want) {
				t.Fatalf("streamed manifest changed v1 wire format\ngot:  %s\nwant: %s", got.Bytes(), want)
			}
		})
	}
}

func BenchmarkWriteSplitManifestJSON(b *testing.B) {
	const parts = int(maxSplitParts)
	summary := SplitSummary{
		SourcePath:        "source.txt",
		BasePath:          "export.txt",
		Mode:              "split-size",
		ManifestPath:      "export.quarry-split-manifest.json",
		BytesWritten:      int64(parts) * 1024,
		Parts:             parts,
		BytesPerPart:      1024,
		ChecksumAlgorithm: "sha256",
		Complete:          true,
		Outputs:           make([]string, parts),
		OutputChecksums:   make([]OutputChecksum, parts),
	}
	for i := range parts {
		path := fmt.Sprintf("export.part%04d.txt", i+1)
		summary.Outputs[i] = path
		summary.OutputChecksums[i] = OutputChecksum{Path: path, SHA256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}
	}
	encoded, err := json.MarshalIndent(&summary, "", "  ")
	if err != nil {
		b.Fatal(err)
	}
	encodedBytes := int64(len(encoded) + 1)

	b.Run("streaming-v2-v1-semantics", func(b *testing.B) {
		b.SetBytes(encodedBytes)
		b.ReportAllocs()
		for b.Loop() {
			if err := writeManifestJSON(io.Discard, &summary); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("buffered-v1", func(b *testing.B) {
		b.SetBytes(encodedBytes)
		b.ReportAllocs()
		for b.Loop() {
			data, err := json.MarshalIndent(&summary, "", "  ")
			if err != nil {
				b.Fatal(err)
			}
			data = append(data, '\n')
			if _, err := io.Discard.Write(data); err != nil {
				b.Fatal(err)
			}
		}
	})
}
