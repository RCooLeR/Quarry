package exportx

import (
	"context"
	"fmt"

	"github.com/quarry/quarry-wails3/internal/document"
	"github.com/quarry/quarry-wails3/internal/sourceio"
)

type retainedSourceValidator interface {
	ValidateUnchanged() error
}

// preparedExportSource is shared only by one sequential multi-output export.
// Its reader verifies every consumed block against one captured expectation;
// completionValidation performs the operation-level and full exact check once
// more before the completion manifest is published.
type preparedExportSource struct {
	reader               document.ReaderAtSize
	completionValidation func(context.Context) error
}

// prepareExactExportSource upgrades FileDocument exports to a mandatory exact
// block-verified snapshot. It is called only after the destination has passed
// alias/existence checks, preserving safe error precedence. Generic ReaderAt
// implementations retain the caller-supplied validator contract.
func prepareExactExportSource(ctx context.Context, doc document.ReaderAtSize, validate func(context.Context) error) (document.ReaderAtSize, func(context.Context) error, error) {
	retained, ok := doc.(*document.FileDocument)
	if !ok {
		return doc, validate, nil
	}
	expected, err := sourceio.ExpectDocumentContext(ctx, retained)
	if err != nil {
		return nil, nil, err
	}
	verified, err := sourceio.NewVerifiedDocumentReader(ctx, expected, retained)
	if err != nil {
		return nil, nil, err
	}
	exact := func(validateCtx context.Context) error {
		return expected.ValidateDocumentContext(validateCtx, retained)
	}
	return verified, composeExportSourceValidators(validate, exact), nil
}

func composeExportSourceValidators(first, last func(context.Context) error) func(context.Context) error {
	return func(ctx context.Context) error {
		if ctx == nil {
			ctx = context.Background()
		}
		if first != nil {
			if err := first(ctx); err != nil {
				return err
			}
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		return last(ctx)
	}
}

// validateExportSource composes an operation-level generation check with the
// retained document's own handle/generation check. The document check runs
// last so a callback cannot accidentally leave a changed document accepted.
func validateExportSource(ctx context.Context, doc document.ReaderAtSize, validate func(context.Context) error) error {
	if ctx == nil {
		ctx = context.Background()
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
	if retained, ok := doc.(retainedSourceValidator); ok {
		if err := retained.ValidateUnchanged(); err != nil {
			return fmt.Errorf("export source generation changed: %w", err)
		}
	}
	return ctx.Err()
}

func commitExportOutput(ctx context.Context, output *createdOutput, doc document.ReaderAtSize, validate func(context.Context) error) error {
	return output.CommitContextValidated(ctx, func(ctx context.Context) error {
		return validateExportSource(ctx, doc, validate)
	})
}
