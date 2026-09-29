//go:build windows

package document

import (
	"encoding/binary"
	"testing"
)

func TestParseFileUSNRecordVersionsAndBounds(t *testing.T) {
	for _, test := range []struct {
		name   string
		major  uint16
		offset int
	}{
		{name: "v2", major: 2, offset: usnRecordV2USNOffset},
		{name: "v3", major: 3, offset: usnRecordV3USNOffset},
	} {
		t.Run(test.name, func(t *testing.T) {
			const want = uint64(0x1020304050607080)
			record := make([]byte, test.offset+8)
			binary.LittleEndian.PutUint32(record[0:4], uint32(len(record)))
			binary.LittleEndian.PutUint16(record[4:6], test.major)
			binary.LittleEndian.PutUint64(record[test.offset:test.offset+8], want)
			got, err := parseFileUSNRecord(record)
			if err != nil {
				t.Fatal(err)
			}
			if got != want {
				t.Fatalf("USN = %#x, want %#x", got, want)
			}
		})
	}

	for _, record := range [][]byte{
		nil,
		make([]byte, usnRecordCommonHeaderBytes),
		func() []byte {
			record := make([]byte, usnRecordV2USNOffset+8)
			binary.LittleEndian.PutUint32(record[0:4], uint32(len(record)+1))
			binary.LittleEndian.PutUint16(record[4:6], 2)
			return record
		}(),
		func() []byte {
			record := make([]byte, usnRecordCommonHeaderBytes)
			binary.LittleEndian.PutUint32(record[0:4], uint32(len(record)))
			binary.LittleEndian.PutUint16(record[4:6], 4)
			return record
		}(),
	} {
		if _, err := parseFileUSNRecord(record); err == nil {
			t.Fatalf("parseFileUSNRecord(%v) unexpectedly succeeded", record)
		}
	}
}
