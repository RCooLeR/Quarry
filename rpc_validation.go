package main

import "errors"

var (
	ErrInvalidFileID   = errors.New("invalid file id")
	ErrInvalidJobID    = errors.New("invalid background job id")
	ErrRPCPathTooLong  = errors.New("path exceeds the RPC byte limit")
	ErrRPCValueTooLong = errors.New("value exceeds the RPC byte limit")
)

const (
	maxRPCOpaqueSequenceDigits = 19
	maxRPCOpaqueSequenceValue  = "9223372036854775807"
	// Windows extended paths can approach 32,767 UTF-16 code units. Four UTF-8
	// bytes per unit keeps that legitimate platform range bounded without
	// reflecting or duplicating an arbitrary bridge-sized string.
	maxRPCPathBytes = 128 * 1024
	maxRPCEnumBytes = 64
)

func validateGeneratedRPCID(value, prefix string, invalid error) error {
	if len(value) <= len(prefix) || len(value) > len(prefix)+maxRPCOpaqueSequenceDigits || value[:len(prefix)] != prefix {
		return invalid
	}
	digits := value[len(prefix):]
	if digits[0] == '0' {
		return invalid
	}
	for i := 0; i < len(digits); i++ {
		if digits[i] < '0' || digits[i] > '9' {
			return invalid
		}
	}
	// Registry and job sequences are positive int64 values. The lexical check
	// is allocation-free because equal-width ASCII decimal strings sort in
	// numeric order.
	if len(digits) == len(maxRPCOpaqueSequenceValue) && digits > maxRPCOpaqueSequenceValue {
		return invalid
	}
	return nil
}

func validateRPCFileID(fileID string) error {
	return validateGeneratedRPCID(fileID, "f", ErrInvalidFileID)
}

func validateRPCJobID(jobID string) error {
	return validateGeneratedRPCID(jobID, "job", ErrInvalidJobID)
}

func validateRPCPath(path string) error {
	if len(path) > maxRPCPathBytes {
		return ErrRPCPathTooLong
	}
	return nil
}

func validateRPCEnum(value string) error {
	if len(value) > maxRPCEnumBytes {
		return ErrRPCValueTooLong
	}
	return nil
}
