//go:build windows

package document

import (
	"encoding/binary"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// fileBasicInfo matches FILE_BASIC_INFO. ChangeTime is maintained separately
// from LastWriteTime by Windows, so restoring the visible mtime does not restore
// this mutation generation.
type fileBasicInfo struct {
	CreationTime   int64
	LastAccessTime int64
	LastWriteTime  int64
	ChangeTime     int64
	FileAttributes uint32
	_              uint32
}

type readFileUSNData struct {
	MinMajorVersion uint16
	MaxMajorVersion uint16
}

const (
	usnRecordCommonHeaderBytes = 8
	usnRecordV2USNOffset       = 24
	usnRecordV3USNOffset       = 40
	maxFileUSNRecordBytes      = 64 * 1024
)

func sourceChangeTokenForFile(file *os.File) (sourceChangeToken, error) {
	if file == nil {
		return sourceChangeToken{}, fmt.Errorf("source change token: file is nil")
	}
	var info fileBasicInfo
	if err := windows.GetFileInformationByHandleEx(
		windows.Handle(file.Fd()),
		windows.FileBasicInfo,
		(*byte)(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		return sourceChangeToken{}, fmt.Errorf("source change token: %w", err)
	}
	// ChangeTime is useful as a cheap signal, but it is timestamp based and can
	// collide for two rapid mutations. Prefer the per-file USN when the volume's
	// change journal supports it. Artifact-producing transforms fail closed via
	// HasMutationGeneration when only the weaker timestamp is available.
	token := sourceChangeToken{available: true, kind: 1, a: uint64(info.ChangeTime)}
	usn, err := readFileUSN(windows.Handle(file.Fd()))
	if err != nil {
		return token, nil
	}
	token.strong = true
	token.kind = 3
	token.b = usn
	return token, nil
}

func readFileUSN(handle windows.Handle) (uint64, error) {
	input := readFileUSNData{MinMajorVersion: 2, MaxMajorVersion: 3}
	output := make([]byte, maxFileUSNRecordBytes)
	var returned uint32
	err := windows.DeviceIoControl(
		handle,
		windows.FSCTL_READ_FILE_USN_DATA,
		(*byte)(unsafe.Pointer(&input)),
		uint32(unsafe.Sizeof(input)),
		&output[0],
		uint32(len(output)),
		&returned,
		nil,
	)
	if err != nil {
		return 0, fmt.Errorf("read source file USN: %w", err)
	}
	return parseFileUSNRecord(output[:returned])
}

func parseFileUSNRecord(record []byte) (uint64, error) {
	if len(record) < usnRecordCommonHeaderBytes {
		return 0, fmt.Errorf("source file USN record is too short: %d bytes", len(record))
	}
	recordLength := int(binary.LittleEndian.Uint32(record[0:4]))
	if recordLength < usnRecordCommonHeaderBytes || recordLength > len(record) {
		return 0, fmt.Errorf("source file USN record length %d is invalid for %d bytes", recordLength, len(record))
	}
	major := binary.LittleEndian.Uint16(record[4:6])
	var offset int
	switch major {
	case 2:
		offset = usnRecordV2USNOffset
	case 3:
		offset = usnRecordV3USNOffset
	default:
		return 0, fmt.Errorf("unsupported source file USN record version %d", major)
	}
	if offset+8 > recordLength {
		return 0, fmt.Errorf("source file USN record version %d is truncated", major)
	}
	return binary.LittleEndian.Uint64(record[offset : offset+8]), nil
}
