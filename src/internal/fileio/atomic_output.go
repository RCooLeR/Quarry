package fileio

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

var (
	ErrSourceAlias                   = errors.New("output aliases a source file")
	ErrOutputNotOpen                 = errors.New("atomic output is not open")
	ErrOutputAlreadyDone             = errors.New("atomic output is already published")
	ErrSecureAtomicOutputUnavailable = errors.New("secure handle-bound atomic output is unavailable")
	ErrInvalidExactPath              = errors.New("output path spelling is not exact")
	ErrOutputPathDrift               = errors.New("output path changed during publication")
)

// PublicationBoundary serializes the irreversible final-name publication
// point with an operation owner's cancellation decision. The boundary must
// call publish exactly once when publication is still permitted, or return a
// cancellation/error without calling it. AtomicOutput installs no boundary by
// default; service jobs attach one to their context so lower-level streaming
// packages do not need to depend on the service layer.
type PublicationBoundary func(publish func() error) error

type publicationBoundaryContextKey struct{}

// WithPublicationBoundary returns a context whose AtomicOutput commits enter
// boundary for the final platform publication step. Derived contexts retain
// the boundary through the ordinary context value chain.
func WithPublicationBoundary(ctx context.Context, boundary PublicationBoundary) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if boundary == nil {
		return ctx
	}
	// An outer operation owner already supplied the authoritative decision.
	// Nested engines may propagate that context but cannot replace its safety
	// boundary with a less restrictive callback.
	if existing, _ := ctx.Value(publicationBoundaryContextKey{}).(PublicationBoundary); existing != nil {
		return ctx
	}
	return context.WithValue(ctx, publicationBoundaryContextKey{}, boundary)
}

func publishWithinBoundary(ctx context.Context, publish func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	boundary, _ := ctx.Value(publicationBoundaryContextKey{}).(PublicationBoundary)
	if boundary == nil {
		return publish()
	}
	return boundary(publish)
}

// afterAtomicOutputPublish is a test seam at the narrow point between
// handle-relative publication and exact-path identity verification.
var afterAtomicOutputPublish = func() {}

// InvalidExactPathError reports a path that would change if filepath.Clean
// were applied. Atomic output deliberately rejects such paths instead of
// silently publishing to a different filesystem object. Whitespace is never
// trimmed. Spellings that the host cannot round-trip exactly fail closed (for
// example ordinary Windows components ending in a dot or space).
type InvalidExactPathError struct {
	Path      string
	CleanPath string
	Reason    string
}

func (e *InvalidExactPathError) Error() string {
	if e == nil {
		return ErrInvalidExactPath.Error()
	}
	if e.Reason != "" {
		return fmt.Sprintf("%v %q: %s", ErrInvalidExactPath, e.Path, e.Reason)
	}
	return fmt.Sprintf("%v %q (clean spelling would be %q)", ErrInvalidExactPath, e.Path, e.CleanPath)
}

func (e *InvalidExactPathError) Unwrap() error { return ErrInvalidExactPath }

// OutputPathDriftError means the retained parent/object no longer matches the
// caller's exact final spelling. RolledBack is true only when the just-published
// handle-owned entry was removed and the retained parent was synced before the
// operation returned.
type OutputPathDriftError struct {
	Path       string
	RolledBack bool
	Detail     string
}

func (e *OutputPathDriftError) Error() string {
	if e == nil {
		return ErrOutputPathDrift.Error()
	}
	state := "handle-owned publication rollback could not be confirmed"
	if e.RolledBack {
		state = "handle-owned publication was removed"
	}
	if e.Detail == "" {
		return fmt.Sprintf("%v at %q; %s", ErrOutputPathDrift, e.Path, state)
	}
	return fmt.Sprintf("%v at %q (%s); %s", ErrOutputPathDrift, e.Path, e.Detail, state)
}

func (e *OutputPathDriftError) Unwrap() error { return ErrOutputPathDrift }

// PublicationError reports a failure after the complete output became visible
// at its final path. Callers must not describe this as an ordinary no-output
// failure or blindly retry under a different assumption.
type PublicationError struct {
	FinalPath         string
	Durable           bool
	LocationUncertain bool
	Err               error
}

func (e *PublicationError) Error() string {
	if e.LocationUncertain {
		return fmt.Sprintf("publication state for requested path %q is inconsistent; a handle-owned artifact may remain at an unconfirmed location: %v", e.FinalPath, e.Err)
	}
	state := "publication durability is unconfirmed"
	if e.Durable {
		state = "publication completed, but finalization failed"
	}
	return fmt.Sprintf("output exists at %q; %s: %v", e.FinalPath, state, e.Err)
}

func (e *PublicationError) Unwrap() error { return e.Err }

// AtomicOutput streams to an operation-owned temporary object and publishes
// the complete file under its final name through a handle-bound, atomic,
// no-clobber platform primitive. Linux uses an anonymous O_TMPFILE descriptor;
// Windows renames the opened file by handle. Unsupported filesystems/platforms
// fail safely before exposing a final-name partial.
type AtomicOutput struct {
	finalPath string
	tempPath  string
	file      *os.File
	platform  atomicOutputPlatformState
	published bool
}

// OpenAtomicOutput validates that finalPath differs from every source path,
// refuses an existing destination, and creates a private platform temp.
func OpenAtomicOutput(finalPath string, sourcePaths []string, mode os.FileMode) (_ *AtomicOutput, retErr error) {
	if finalPath == "" {
		return nil, errors.New("output path is required")
	}
	if err := validateExactFinalPath(finalPath); err != nil {
		return nil, err
	}
	for _, sourcePath := range sourcePaths {
		same, err := SamePath(sourcePath, finalPath)
		if err != nil {
			return nil, err
		}
		if same {
			return nil, fmt.Errorf("%w: %s", ErrSourceAlias, finalPath)
		}
	}
	if _, err := os.Lstat(finalPath); err == nil {
		return nil, fmt.Errorf("%w: %s", ErrExists, finalPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	mode = mode.Perm()
	if mode == 0 {
		mode = 0o600
	}
	file, tempPath, platform, err := openAtomicOutputTemp(finalPath, mode)
	if err != nil {
		return nil, err
	}
	out := &AtomicOutput{
		finalPath: finalPath,
		tempPath:  tempPath,
		file:      file,
		platform:  platform,
	}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, out.Cleanup())
		}
	}()
	if err := file.Chmod(mode); err != nil {
		return nil, err
	}
	return out, nil
}

// ValidateExactOutputPath rejects a final path if lexical cleaning would
// change the caller's spelling. Callers that derive sibling paths must invoke
// this before filepath.Dir/Base/Join can erase that evidence.
func ValidateExactOutputPath(path string) error {
	clean := filepath.Clean(path)
	if clean != path {
		return &InvalidExactPathError{Path: path, CleanPath: clean}
	}
	_, base := filepath.Split(path)
	if base == "" || base == "." || base == ".." {
		return &InvalidExactPathError{
			Path:      path,
			CleanPath: clean,
			Reason:    "path must name a file with a non-dot final component",
		}
	}
	return validatePlatformExactPath(path, clean, false)
}

func validateExactFinalPath(path string) error { return ValidateExactOutputPath(path) }

// ValidateExactDirectoryPath applies the same no-rewrite contract to a parent
// directory. Dot-only directories are rejected because appending a child with
// filepath.Join would otherwise erase the caller's selected spelling.
func ValidateExactDirectoryPath(path string) error {
	clean := filepath.Clean(path)
	if path == "" || clean != path {
		return &InvalidExactPathError{Path: path, CleanPath: clean}
	}
	if path == "." || path == ".." {
		return &InvalidExactPathError{
			Path:      path,
			CleanPath: clean,
			Reason:    "directory must not be a dot-only spelling",
		}
	}
	return validatePlatformExactPath(path, clean, true)
}

// ExactChildPath constructs one direct child without filepath.Join cleaning
// the selected parent. The child name must already be one filename component.
func ExactChildPath(dir string, base string) (string, error) {
	if err := ValidateExactDirectoryPath(dir); err != nil {
		return "", err
	}
	baseDir, baseName := filepath.Split(base)
	if base == "" || baseDir != "" || baseName != base || base == "." || base == ".." || filepath.VolumeName(base) != "" {
		return "", &InvalidExactPathError{
			Path:      base,
			CleanPath: filepath.Clean(base),
			Reason:    "child name must be one non-dot filename component",
		}
	}
	child := dir
	if child[len(child)-1] != byte(filepath.Separator) {
		child += string(filepath.Separator)
	}
	child += base
	if err := ValidateExactOutputPath(child); err != nil {
		return "", err
	}
	return child, nil
}

func (o *AtomicOutput) Path() string {
	if o == nil {
		return ""
	}
	return o.finalPath
}

// TempPath is non-empty only on platforms whose secure primitive requires a
// named temporary entry. Linux's anonymous O_TMPFILE has no pathname.
func (o *AtomicOutput) TempPath() string {
	if o == nil {
		return ""
	}
	return o.tempPath
}

func (o *AtomicOutput) Write(p []byte) (int, error) {
	if o == nil || o.file == nil || o.published {
		return 0, ErrOutputNotOpen
	}
	return o.file.Write(p)
}

// Sync flushes bytes written so far without publishing the output. It allows
// existing streaming pipelines that require an io.Writer plus Sync contract to
// use AtomicOutput while the retained handle remains available for Commit.
func (o *AtomicOutput) Sync() error {
	if o == nil || o.file == nil || o.published {
		return ErrOutputNotOpen
	}
	return o.file.Sync()
}

func (o *AtomicOutput) Commit() error {
	return o.CommitContext(context.Background())
}

// CommitContext syncs the complete data and checks cancellation before the
// platform's atomic publication point. Cancellation that arrives after that
// point cannot revoke an already-published final file and publication wins.
func (o *AtomicOutput) CommitContext(ctx context.Context) error {
	return o.commitContext(ctx, nil)
}

// CommitContextValidated syncs the complete output, invokes validate, checks
// cancellation again, and then immediately enters the platform publication
// primitive. It is intended for transforms whose source generation must be
// revalidated after the potentially slow output fsync. Validation and rename
// affect different filesystem objects and therefore cannot be one atomic
// operation; this API deliberately narrows, but cannot eliminate, that race.
func (o *AtomicOutput) CommitContextValidated(ctx context.Context, validate func(context.Context) error) error {
	if validate == nil {
		return errors.New("atomic output publication validator is required")
	}
	return o.commitContext(ctx, validate)
}

func (o *AtomicOutput) commitContext(ctx context.Context, validate func(context.Context) error) error {
	if o == nil || o.published {
		return ErrOutputAlreadyDone
	}
	if o.file == nil {
		return ErrOutputNotOpen
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := o.file.Sync(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if validate != nil {
		if err := validate(ctx); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	return publishWithinBoundary(ctx, func() error {
		return publishAtomicOutput(ctx, o)
	})
}

// Cleanup closes and deletes only the temporary object bound to the retained
// operation handle. It never unlinks an arbitrary mutable pathname.
func (o *AtomicOutput) Cleanup() error {
	if o == nil {
		return nil
	}
	return cleanupAtomicOutput(o)
}
