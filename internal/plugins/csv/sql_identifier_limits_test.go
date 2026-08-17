package csv

import (
	"strings"
	"testing"
)

func TestSQLIdentifierLimitsAcceptMaximumAndRejectNextByte(t *testing.T) {
	maximum := strings.Repeat("😀", MaxSQLIdentifierRunes)
	if len(maximum) != MaxSQLIdentifierBytes {
		t.Fatalf("test identifier is %d bytes, want %d", len(maximum), MaxSQLIdentifierBytes)
	}
	if err := ValidateSQLConvertOptions(SQLConvertOptions{TableName: maximum}); err != nil {
		t.Fatalf("maximum identifier rejected: %v", err)
	}

	tooLarge := maximum + "x"
	if err := ValidateSQLConvertOptions(SQLConvertOptions{TableName: tooLarge}); err == nil || !strings.Contains(err.Error(), "byte") {
		t.Fatalf("maximum+1-byte identifier error = %v", err)
	}
}

func TestSQLIdentifierLimitsRejectCharacterOverflow(t *testing.T) {
	tooManyRunes := strings.Repeat("x", MaxSQLIdentifierRunes+1)
	if err := ValidateSQLConvertOptions(SQLConvertOptions{TableName: tooManyRunes}); err == nil || !strings.Contains(err.Error(), "characters") {
		t.Fatalf("character-overflow error = %v", err)
	}
}

func TestSQLIdentifierLimitsRejectInvalidUTF8(t *testing.T) {
	invalidTable := string([]byte{'t', 0xff})
	if err := ValidateSQLConvertOptions(SQLConvertOptions{TableName: invalidTable}); err == nil || !strings.Contains(err.Error(), "invalid UTF-8") {
		t.Fatalf("invalid UTF-8 table-name error = %v", err)
	}
	invalidColumn := string([]byte{'c', 0xff})
	if err := ValidateSQLConvertOptions(SQLConvertOptions{TableName: "valid", Columns: []string{invalidColumn}}); err == nil || !strings.Contains(err.Error(), "invalid UTF-8") {
		t.Fatalf("invalid UTF-8 column-name error = %v", err)
	}
}
