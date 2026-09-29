package csv

import (
	"context"
	"errors"
)

// XLSXOptions configures a CSV to Excel (.xlsx) export.
type XLSXOptions struct {
	Delimiter  rune
	HasHeader  bool
	SheetName  string
	TypedCells bool // write numeric cells as numbers, not text
	Progress   func(records int64)
}

// XLSXSummary reports an .xlsx export, including whether the row cap truncated.
type XLSXSummary struct {
	RecordsRead    int64
	RecordsWritten int64
	Truncated      bool
}

// ErrXLSXExportSecureScratchUnavailable is returned before Quarry opens the
// source or creates output. Excelize's memory-bounded stream writer spills to
// mutable named temporary files and removes them later by pathname; Quarry
// cannot prove those names still identify operation-owned objects at cleanup.
var ErrXLSXExportSecureScratchUnavailable = errors.New(
	"excel export is temporarily unavailable because the streaming writer cannot securely own and clean up its scratch files",
)

// ExportXLSXFile is conservatively disabled until its streaming dependency can
// accept operation-owned scratch handles. It performs no filesystem operations.
func ExportXLSXFile(context.Context, string, string, XLSXOptions) (XLSXSummary, error) {
	return XLSXSummary{}, ErrXLSXExportSecureScratchUnavailable
}
