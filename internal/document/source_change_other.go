//go:build !windows

package document

import (
	"os"
	"reflect"
)

// sourceChangeTokenForFile extracts the inode status-change time without
// binding the package to one operating system's syscall.Stat_t layout. Common
// Unix targets expose either Ctim/Ctimespec or Ctime plus Ctimensec.
func sourceChangeTokenForFile(file *os.File) (sourceChangeToken, error) {
	if file == nil {
		return sourceChangeToken{}, nil
	}
	info, err := file.Stat()
	if err != nil {
		return sourceChangeToken{}, err
	}
	value := reflect.ValueOf(info.Sys())
	for value.IsValid() && (value.Kind() == reflect.Pointer || value.Kind() == reflect.Interface) {
		if value.IsNil() {
			return sourceChangeToken{}, nil
		}
		value = value.Elem()
	}
	if !value.IsValid() || value.Kind() != reflect.Struct {
		return sourceChangeToken{}, nil
	}
	for _, name := range []string{"Ctim", "Ctimespec"} {
		if seconds, nanos, ok := reflectedTimespec(value.FieldByName(name)); ok {
			return sourceChangeToken{available: true, strong: true, kind: 2, a: seconds, b: nanos}, nil
		}
	}
	seconds, secondsOK := reflectedUint(value.FieldByName("Ctime"))
	nanos, nanosOK := reflectedUint(value.FieldByName("Ctimensec"))
	if secondsOK && nanosOK {
		return sourceChangeToken{available: true, strong: true, kind: 2, a: seconds, b: nanos}, nil
	}
	return sourceChangeToken{}, nil
}

func reflectedTimespec(value reflect.Value) (uint64, uint64, bool) {
	for value.IsValid() && value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return 0, 0, false
		}
		value = value.Elem()
	}
	if !value.IsValid() || value.Kind() != reflect.Struct {
		return 0, 0, false
	}
	seconds, secondsOK := reflectedUint(value.FieldByName("Sec"))
	nanos, nanosOK := reflectedUint(value.FieldByName("Nsec"))
	return seconds, nanos, secondsOK && nanosOK
}

func reflectedUint(value reflect.Value) (uint64, bool) {
	if !value.IsValid() {
		return 0, false
	}
	switch value.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return uint64(value.Int()), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return value.Uint(), true
	default:
		return 0, false
	}
}
