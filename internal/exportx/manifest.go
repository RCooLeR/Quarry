package exportx

import (
	"context"
	jsonv1 "encoding/json"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/quarry/quarry-wails3/internal/fileio"
)

const defaultExportManifestSuffix = ".quarry-export-manifest.json"

func shouldWriteExportManifest(opts Options) bool {
	return opts.WriteManifest || opts.ManifestPath != ""
}

func exportManifestPathFor(outputPath string, override string) string {
	if override != "" {
		return override
	}
	return outputPath + defaultExportManifestSuffix
}

func ensureExportManifestAvailable(path string, protectedPaths ...string) error {
	if path == "" {
		return errors.New("export manifest path is required")
	}
	if err := fileio.ValidateExactOutputPath(path); err != nil {
		return err
	}
	for _, protectedPath := range protectedPaths {
		same, err := fileio.SamePath(path, protectedPath)
		if err != nil {
			return err
		}
		if same {
			return fmt.Errorf("%w: export manifest %s", fileio.ErrSourceAlias, path)
		}
	}
	return ensureManifestFinalAvailable("export", path)
}

func ensureManifestFinalAvailable(kind string, path string) error {
	if path == "" {
		return errors.New(kind + " manifest path is required")
	}
	if err := fileio.ValidateExactOutputPath(path); err != nil {
		return err
	}
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("%w: %s", fileio.ErrExists, path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func writeExportManifest(ctx context.Context, summary Summary, validate func(context.Context) error) (retErr error) {
	if summary.ManifestPath == "" {
		return errors.New("export manifest path is required")
	}
	manifest, err := fileio.OpenAtomicOutput(
		summary.ManifestPath,
		[]string{summary.SourcePath, summary.OutputPath},
		0o600,
	)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, manifest.Cleanup()) }()
	if err := writeManifestJSON(manifest, &summary); err != nil {
		return err
	}
	if validate != nil {
		return manifest.CommitContextValidated(ctx, validate)
	}
	return manifest.CommitContext(ctx)
}

// writeManifestJSON streams the indented document directly to its private
// atomic output. DefaultOptionsV1 keeps the established manifest wire format
// while avoiding a second, potentially large in-memory copy of the document.
func writeManifestJSON(out io.Writer, value any) error {
	if err := jsonv2.MarshalWrite(out, value, jsonv1.DefaultOptionsV1(), jsontext.WithIndent("  ")); err != nil {
		return err
	}
	if n, err := io.WriteString(out, "\n"); err != nil {
		return err
	} else if n != 1 {
		return io.ErrShortWrite
	}
	return nil
}

func exportManifestPublicationError(summary Summary, err error) error {
	return &fileio.PublicationError{
		FinalPath: summary.OutputPath,
		Durable:   true,
		Err:       fmt.Errorf("export output was published, but manifest %q failed: %w", summary.ManifestPath, err),
	}
}

func markUncertainPublication(summary *Summary, err error) {
	if summary == nil {
		return
	}
	var publication *fileio.PublicationError
	if errors.As(err, &publication) && publication.LocationUncertain {
		summary.PublicationUncertain = true
	}
}
