package manualedit

import (
	"errors"
	"fmt"
	"math"

	"github.com/quarry/quarry-wails3/internal/sourceio"
)

const (
	DefaultMaxLiveInsertedBytes int64 = 16 * 1024 * 1024
	DefaultMaxHistoryBytes      int64 = 48 * 1024 * 1024
	DefaultMaxTransientBytes    int64 = 192 * 1024 * 1024
	DefaultMaxEditCount               = 4096
	DefaultMaxHistoryDepth            = 4096
	DefaultMaxPieceCount              = 8193

	// These deliberately overestimate Go's current structure sizes. The limits
	// are safety accounting, not a promise about one compiler's heap layout.
	pieceAccountingBytes     int64 = 32
	rangeAccountingBytes     int64 = 24
	historyAccountingBytes   int64 = 96
	sliceAccountingBytes     int64 = 24
	sourceCompareBufferBytes       = 64 * 1024
	sourceWriteBufferBytes         = 1024 * 1024
	// Covers the fixed Session/source-binding/expectation values that are not
	// already represented by the byte slices and conservative collection-entry
	// accounting below. This is safety accounting, not a runtime-size claim.
	defaultRetainedFixedAccountingBytes int64 = 4 * 1024
)

var (
	ErrEditCountLimit           = errors.New("active edit count exceeds the session limit")
	ErrHistoryDepthLimit        = errors.New("retained undo history exceeds the session depth limit")
	ErrHistoryBytesLimit        = errors.New("retained undo history exceeds the session memory limit")
	ErrTransientMemoryLimit     = errors.New("edit rebuild would exceed the transient memory limit")
	ErrInvalidSessionLimits     = errors.New("manual-edit session limits are invalid")
	ErrExpectedBytesRequired    = errors.New("a source-bound edit requires exact expected old bytes")
	ErrExpectedBytesMismatch    = errors.New("staged edit no longer matches its expected old bytes")
	ErrSessionRevisionExhausted = errors.New("manual-edit session revision is exhausted")
)

// Limits caps every cumulative in-memory dimension of a manual-edit session.
// Callers that override one value should start with DefaultLimits and change
// only the desired field; zero is invalid so a partially initialized policy
// cannot silently remove a bound.
type Limits struct {
	MaxEditTextBytes     int64
	MaxLiveInsertedBytes int64
	MaxHistoryBytes      int64
	MaxTransientBytes    int64
	MaxEditCount         int
	MaxHistoryDepth      int
	MaxPieceCount        int
}

func DefaultLimits() Limits {
	return Limits{
		MaxEditTextBytes:     DefaultMaxInsertedBytes,
		MaxLiveInsertedBytes: DefaultMaxLiveInsertedBytes,
		MaxHistoryBytes:      DefaultMaxHistoryBytes,
		MaxTransientBytes:    DefaultMaxTransientBytes,
		MaxEditCount:         DefaultMaxEditCount,
		MaxHistoryDepth:      DefaultMaxHistoryDepth,
		MaxPieceCount:        DefaultMaxPieceCount,
	}
}

// DefaultRetainedMemoryUpperBound reports the conservative retained-byte
// accounting ceiling for one default source-bound session at rest. Transient
// rebuild and verification buffers are governed separately by
// DefaultMaxTransientBytes.
func DefaultRetainedMemoryUpperBound() int64 {
	verificationMapBytes, _ := sourceio.MaximumVerificationMemoryBounds()
	return DefaultMaxHistoryBytes +
		DefaultMaxLiveInsertedBytes +
		int64(DefaultMaxPieceCount)*pieceAccountingBytes +
		int64(DefaultMaxEditCount)*rangeAccountingBytes +
		verificationMapBytes +
		defaultRetainedFixedAccountingBytes
}

func validateLimits(limits Limits) error {
	if limits.MaxEditTextBytes <= 0 ||
		limits.MaxLiveInsertedBytes <= 0 ||
		limits.MaxHistoryBytes <= 0 ||
		limits.MaxTransientBytes <= 0 ||
		limits.MaxEditCount <= 0 ||
		limits.MaxHistoryDepth <= 0 ||
		limits.MaxPieceCount <= 0 {
		return ErrInvalidSessionLimits
	}
	if limits.MaxEditTextBytes > int64(maxInt()) || limits.MaxLiveInsertedBytes > int64(maxInt()) {
		return fmt.Errorf("%w: byte limit exceeds addressable memory", ErrInvalidSessionLimits)
	}
	return nil
}

func legacyLimits(maxInsertedBytes int64) Limits {
	limits := DefaultLimits()
	if maxInsertedBytes > 0 {
		limits.MaxEditTextBytes = maxInsertedBytes
		limits.MaxLiveInsertedBytes = maxInsertedBytes
		// Preserve small test/application policies as true total-memory policies.
		// The minimum fixed accounting still makes malformed tiny policies fail
		// with a controlled typed error rather than allocating without a bound.
		if maxInsertedBytes <= math.MaxInt64/4 {
			candidate := maxInsertedBytes * 4
			if candidate > historyAccountingBytes {
				limits.MaxHistoryBytes = candidate
				if candidate <= math.MaxInt64/2 {
					limits.MaxTransientBytes = candidate * 2
				}
			}
		}
	}
	return limits
}

func checkedMemorySum(values ...int64) (int64, error) {
	var total int64
	for _, value := range values {
		if value < 0 || value > math.MaxInt64-total {
			return 0, ErrTransientMemoryLimit
		}
		total += value
	}
	return total, nil
}
