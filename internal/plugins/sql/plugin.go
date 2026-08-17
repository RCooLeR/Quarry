package sql

import (
	"context"

	"github.com/quarry/quarry-wails3/internal/highlight"
	"github.com/quarry/quarry-wails3/internal/plugins"
	analyzepkg "github.com/quarry/quarry-wails3/internal/plugins/sql/analyze"
	extractpkg "github.com/quarry/quarry-wails3/internal/plugins/sql/extract"
	sqlhighlight "github.com/quarry/quarry-wails3/internal/plugins/sql/highlight"
)

type ReaderAtSize = analyzepkg.ReaderAtSize
type AnalyzeOptions = analyzepkg.Options
type AnalyzeProgress = analyzepkg.Progress
type Summary = analyzepkg.Summary
type Table = analyzepkg.Table
type ExtractPlanOptions = extractpkg.PlanOptions
type ExtractTableRange = extractpkg.TableRange
type ExtractManifestPreview = extractpkg.ManifestPreview
type ExtractWriteOptions = extractpkg.WriteOptions
type ExtractWriteSummary = extractpkg.WriteSummary

type TokenKind = highlight.TokenKind
type Token = highlight.Token

const (
	TokenText       = highlight.TokenText
	TokenKeyword    = highlight.TokenKeyword
	TokenString     = highlight.TokenString
	TokenComment    = highlight.TokenComment
	TokenNumber     = highlight.TokenNumber
	TokenIdentifier = highlight.TokenIdentifier
	TokenOperator   = highlight.TokenOperator
)

type Runtime interface {
	plugins.RuntimePlugin
	Analyze(context.Context, ReaderAtSize, AnalyzeOptions) (Summary, error)
	AnalyzeFile(context.Context, string, AnalyzeOptions) (Summary, error)
	HighlightVisible(text string) []Token
	SplitByTablePreview(Summary, int64, ExtractPlanOptions) (ExtractManifestPreview, error)
	ExtractTablePreview(Summary, int64, string, ExtractPlanOptions) (ExtractManifestPreview, error)
	SplitByTable(context.Context, ReaderAtSize, string, Summary, ExtractWriteOptions) (ExtractWriteSummary, error)
	ExtractTable(context.Context, ReaderAtSize, string, Summary, string, ExtractWriteOptions) (ExtractWriteSummary, error)
}

type BuiltIn struct{}

func RuntimePlugin() BuiltIn {
	return BuiltIn{}
}

func (BuiltIn) Descriptor() plugins.Descriptor {
	return Plugin()
}

func (BuiltIn) Analyze(ctx context.Context, r ReaderAtSize, opts AnalyzeOptions) (Summary, error) {
	return Analyze(ctx, r, opts)
}

func (BuiltIn) AnalyzeFile(ctx context.Context, path string, opts AnalyzeOptions) (Summary, error) {
	return AnalyzeFile(ctx, path, opts)
}

func (BuiltIn) HighlightVisible(text string) []Token {
	return HighlightVisible(text)
}

func (BuiltIn) SplitByTablePreview(summary Summary, sourceSize int64, opts ExtractPlanOptions) (ExtractManifestPreview, error) {
	return SplitByTablePreview(summary, sourceSize, opts)
}

func (BuiltIn) ExtractTablePreview(summary Summary, sourceSize int64, tableName string, opts ExtractPlanOptions) (ExtractManifestPreview, error) {
	return ExtractTablePreview(summary, sourceSize, tableName, opts)
}

func (BuiltIn) SplitByTable(ctx context.Context, doc ReaderAtSize, sourcePath string, summary Summary, opts ExtractWriteOptions) (ExtractWriteSummary, error) {
	return SplitByTable(ctx, doc, sourcePath, summary, opts)
}

func (BuiltIn) ExtractTable(ctx context.Context, doc ReaderAtSize, sourcePath string, summary Summary, tableName string, opts ExtractWriteOptions) (ExtractWriteSummary, error) {
	return ExtractTable(ctx, doc, sourcePath, summary, tableName, opts)
}

func Plugin() plugins.Descriptor {
	return plugins.Descriptor{
		ID:           "sql",
		DisplayName:  "SQL Dumps",
		Category:     "data",
		Description:  "SQL dump highlighting, analysis, navigation, reshape, and table extraction/splitting tools.",
		FilePatterns: plugins.SQLPatterns(),
		Capabilities: []plugins.Capability{
			plugins.CapabilitySyntax,
			plugins.CapabilityAnalyze,
			plugins.CapabilityTransform,
			plugins.CapabilityNavigate,
			plugins.CapabilityExtract,
		},
		Operations: []plugins.OperationCapability{
			{
				ID: "highlight-visible", Capability: plugins.CapabilitySyntax,
				Processing: plugins.ProcessingBoundedWindow, Memory: plugins.MemoryBounded,
				MaxInputBytes: 32 << 20,
				Notes:         []string{"Highlighting consumes only the bounded visible window supplied by the editor."},
			},
			{
				ID: "analyze-dump", Capability: plugins.CapabilityAnalyze,
				Processing: plugins.ProcessingStreaming, Memory: plugins.MemoryMetadataProportional,
				Cancellable: true,
				Notes:       []string{"Input scanning is streaming; retained table and statement metadata grows with dump cardinality."},
			},
			{
				ID: "navigate-tables", Capability: plugins.CapabilityNavigate,
				Processing: plugins.ProcessingStreaming, Memory: plugins.MemoryMetadataProportional,
				Cancellable: true,
				Notes:       []string{"Navigation uses analyzer metadata and inherits its cardinality-proportional retention."},
			},
			{
				ID: "reshape-inserts", Capability: plugins.CapabilityTransform,
				Processing: plugins.ProcessingStreaming, Memory: plugins.MemoryBatchProportional,
				Cancellable: true, AtomicOutput: true,
				Notes: []string{"Reshape memory grows with the configured tuple batch and statement size."},
			},
			{
				ID: "extract-tables", Capability: plugins.CapabilityExtract,
				Processing: plugins.ProcessingStreaming, Memory: plugins.MemoryStatementProportional,
				Cancellable: true, AtomicOutput: true,
				Notes: []string{"Table writes stream source ranges, while statement and manifest metadata remain input-dependent."},
			},
			{
				ID: "fixture-sample", Capability: plugins.CapabilityExtract,
				Processing: plugins.ProcessingMaterialized, Memory: plugins.MemoryInputProportional,
				Cancellable: true,
				Notes:       []string{"Fixture sampling returns a buffer and must not be treated as huge-file safe without a separate hard sample cap."},
			},
		},
		Modes: []plugins.Mode{plugins.ModeInteractive, plugins.ModeStreaming},
		Notes: []string{
			"SQL analyzer, visible SQL highlighting, reshape, split/extract manifest previews, and streaming SQL table writes are routed through this built-in plugin adapter.",
			"Cleanup presets are not advertised because byte-level replacement is not safe for structural SQL transformations.",
		},
	}
}

func Analyze(ctx context.Context, r ReaderAtSize, opts AnalyzeOptions) (Summary, error) {
	return analyzepkg.Analyze(ctx, r, opts)
}

func AnalyzeFile(ctx context.Context, path string, opts AnalyzeOptions) (Summary, error) {
	return analyzepkg.AnalyzeFile(ctx, path, opts)
}

func HighlightVisible(text string) []Token {
	return sqlhighlight.SQLVisible(text)
}

func SplitByTablePreview(summary Summary, sourceSize int64, opts ExtractPlanOptions) (ExtractManifestPreview, error) {
	return extractpkg.SplitByTablePreview(summary, sourceSize, opts)
}

func ExtractTablePreview(summary Summary, sourceSize int64, tableName string, opts ExtractPlanOptions) (ExtractManifestPreview, error) {
	return extractpkg.ExtractTablePreview(summary, sourceSize, tableName, opts)
}

func SplitByTable(ctx context.Context, doc ReaderAtSize, sourcePath string, summary Summary, opts ExtractWriteOptions) (ExtractWriteSummary, error) {
	return extractpkg.SplitByTable(ctx, doc, sourcePath, summary, opts)
}

func ExtractTable(ctx context.Context, doc ReaderAtSize, sourcePath string, summary Summary, tableName string, opts ExtractWriteOptions) (ExtractWriteSummary, error) {
	return extractpkg.ExtractTable(ctx, doc, sourcePath, summary, tableName, opts)
}
