package csv

import (
	"context"
	"errors"
)

// SQLiteOptions configures a CSV to SQLite (.db) export.
type SQLiteOptions struct {
	Delimiter  rune
	HasHeader  bool
	TableName  string
	BatchSize  int
	TypedCells bool // bind numeric cells as INTEGER/REAL (else everything TEXT)
	Progress   func(records int64)
}

// ErrSQLiteExportSecurePublicationUnavailable is returned before Quarry opens
// the source or creates any destination/scratch path. The SQLite driver needs a
// mutable pathname and may create sibling journal files; Quarry cannot yet bind
// those objects to its retained atomic-output handle on every supported
// platform. Failing closed is safer than exposing predictable scratch names or
// deleting/replacing a path Quarry no longer owns.
var ErrSQLiteExportSecurePublicationUnavailable = errors.New(
	"SQLite export is temporarily unavailable because secure atomic publication is not supported by the SQLite writer",
)

// ExportSQLiteFile is conservatively disabled until the SQLite writer can use
// an operation-owned handle without reopening a mutable pathname or creating
// unowned journal aliases. It deliberately performs no filesystem operations.
func ExportSQLiteFile(context.Context, string, string, SQLiteOptions) (ExportSummary, error) {
	return ExportSummary{}, ErrSQLiteExportSecurePublicationUnavailable
}
