// Package fileio contains filesystem helpers with explicit safety semantics.
//
// The package owns atomic writes, exclusive output creation, path comparisons,
// and overwrite recovery. A live writer can finish the checksummed,
// operation-bound transaction it just published. If that process is interrupted,
// later access classifies, preserves, and blocks on the journal pending an
// explicit recovery workflow; public and read-time recovery is inspection-only.
// Callers use these helpers instead of open-coded file writes so source files are
// not accidentally overwritten or partially updated.
// Operation-owned files are private by default: Windows creation supplies a
// protected current-user/LocalSystem DACL, while Unix-like callers default to
// mode 0600. Callers and overwrite targets may deliberately request or inherit
// broader POSIX modes. Windows creation fails closed if its descriptor cannot be
// constructed or applied; it never creates with inherited access and tightens
// permissions afterward.
package fileio
