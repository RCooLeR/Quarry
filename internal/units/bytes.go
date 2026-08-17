package units

import (
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
)

const maxNumericInputLength = 128

// ParseLineNumber parses a 1-based line number.
func ParseLineNumber(input string) (int64, error) {
	line, err := strconv.ParseInt(strings.TrimSpace(input), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("expected a positive line number")
	}
	if line <= 0 {
		return 0, fmt.Errorf("line must be positive")
	}
	return line, nil
}

// FormatBytes formats byte counts for compact UI labels and progress text.
func FormatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for n/div >= unit && exp < 5 {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// FormatBytesUint formats unsigned byte counts, primarily for filesystem stats.
func FormatBytesUint(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for n/div >= unit && exp < 5 {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// ParseByteOffset parses a byte offset or percentage within a file size.
func ParseByteOffset(input string, size int64) (int64, error) {
	if size < 0 {
		return 0, errors.New("file size cannot be negative")
	}
	s := strings.TrimSpace(input)
	if s == "" {
		return 0, errors.New("empty offset")
	}
	if strings.HasSuffix(s, "%") {
		return parsePercent(s[:len(s)-1], size)
	}

	fields := strings.Fields(s)
	if len(fields) > 2 {
		return 0, fmt.Errorf("invalid offset %q", input)
	}

	number := fields[0]
	unit := ""
	if len(fields) == 2 {
		unit = fields[1]
	} else {
		number, unit = splitNumberUnit(number)
	}

	value, ok := parseDecimal(number)
	if !ok {
		return 0, fmt.Errorf("invalid offset %q", input)
	}
	if value.Sign() < 0 {
		return 0, errors.New("offset cannot be negative")
	}

	multiplier, err := byteMultiplier(unit)
	if err != nil {
		return 0, err
	}

	scaled := new(big.Rat).Mul(value, new(big.Rat).SetInt64(multiplier))
	offset := roundNonNegativeRat(scaled)
	if offset.Cmp(big.NewInt(size)) > 0 {
		return size, nil
	}
	return offset.Int64(), nil
}

// ParseByteSize parses a byte-size value such as "64 KB" or "1.5 MiB".
func ParseByteSize(input string) (int64, error) {
	s := strings.TrimSpace(input)
	if s == "" {
		return 0, errors.New("empty size")
	}
	if strings.HasSuffix(s, "%") {
		return 0, fmt.Errorf("percentages are not valid byte sizes")
	}

	fields := strings.Fields(s)
	if len(fields) > 2 {
		return 0, fmt.Errorf("invalid size %q", input)
	}

	number := fields[0]
	unit := ""
	if len(fields) == 2 {
		unit = fields[1]
	} else {
		number, unit = splitNumberUnit(number)
	}

	value, ok := parseDecimal(number)
	if !ok {
		return 0, fmt.Errorf("invalid size %q", input)
	}
	if value.Sign() <= 0 {
		return 0, errors.New("size must be positive")
	}

	multiplier, err := byteMultiplier(unit)
	if err != nil {
		return 0, err
	}

	scaled := new(big.Rat).Mul(value, new(big.Rat).SetInt64(multiplier))
	size := roundNonNegativeRat(scaled)
	if !size.IsInt64() {
		return 0, errors.New("size exceeds the supported byte range")
	}
	result := size.Int64()
	if result <= 0 {
		return 0, errors.New("size must be positive")
	}
	return result, nil
}

func parsePercent(input string, size int64) (int64, error) {
	if size < 0 {
		return 0, errors.New("file size cannot be negative")
	}
	value, ok := parseDecimal(strings.TrimSpace(input))
	if !ok {
		return 0, fmt.Errorf("invalid percentage %q", input+"%")
	}
	if value.Sign() < 0 {
		return 0, errors.New("percentage cannot be negative")
	}
	if value.Cmp(big.NewRat(100, 1)) > 0 {
		return 0, errors.New("percentage cannot exceed 100")
	}
	scaled := new(big.Rat).Mul(value, new(big.Rat).SetFrac(big.NewInt(size), big.NewInt(100)))
	return roundNonNegativeRat(scaled).Int64(), nil
}

func parseDecimal(input string) (*big.Rat, bool) {
	if input == "" || len(input) > maxNumericInputLength {
		return nil, false
	}
	value, ok := new(big.Rat).SetString(input)
	return value, ok
}

// roundNonNegativeRat implements math.Round's positive-value behavior without
// converting through float64: halves round away from zero.
func roundNonNegativeRat(value *big.Rat) *big.Int {
	numerator := new(big.Int).Set(value.Num())
	denominator := new(big.Int).Set(value.Denom())
	quotient, remainder := new(big.Int), new(big.Int)
	quotient.QuoRem(numerator, denominator, remainder)
	if new(big.Int).Lsh(remainder, 1).Cmp(denominator) >= 0 {
		quotient.Add(quotient, big.NewInt(1))
	}
	return quotient
}

func splitNumberUnit(input string) (string, string) {
	for i, r := range input {
		if (r < '0' || r > '9') && r != '.' {
			return strings.TrimSpace(input[:i]), strings.TrimSpace(input[i:])
		}
	}
	return input, ""
}

func byteMultiplier(unit string) (int64, error) {
	switch strings.ToLower(strings.TrimSpace(unit)) {
	case "", "b", "byte", "bytes":
		return 1, nil
	case "k", "kb", "kib":
		return 1024, nil
	case "m", "mb", "mib":
		return 1024 * 1024, nil
	case "g", "gb", "gib":
		return 1024 * 1024 * 1024, nil
	case "t", "tb", "tib":
		return 1024 * 1024 * 1024 * 1024, nil
	default:
		return 0, fmt.Errorf("unsupported byte unit %q", unit)
	}
}
