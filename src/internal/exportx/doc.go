// Package exportx streams selected text or file ranges to new output files.
//
// Export operations never overwrite the source file. They copy bounded byte or
// line ranges through optional decoders/encoders, can compute SHA-256 evidence,
// and write manifests that describe exactly what was produced.
//
// Raw range exports preserve the caller's byte coordinates exactly and may
// therefore contain partial encoded characters. Text range exports use a
// fail-closed policy: they reject rather than expand endpoints inside a BOM,
// rune, code unit, or surrogate pair, and validate the complete selected stream
// before publication. A matching leading source BOM is omitted when a text
// range starts at byte zero. UTF-16 text outputs receive one target BOM; UTF-8
// and Windows code-page outputs do not receive a generated BOM.
package exportx
