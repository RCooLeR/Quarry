//go:build !windows

package inplace

import (
	"errors"
	"os"
	"reflect"
)

func sourceIdentityForFile(f *os.File) (sourceIdentity, error) {
	if f == nil {
		return sourceIdentity{}, errors.New("inplace: source handle is nil")
	}
	info, err := f.Stat()
	if err != nil {
		return sourceIdentity{}, err
	}
	value := reflect.ValueOf(info.Sys())
	if value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return sourceIdentity{}, errors.New("inplace: source identity is unavailable")
		}
		value = value.Elem()
	}
	if value.Kind() != reflect.Struct {
		return sourceIdentity{}, errors.New("inplace: source identity is unavailable")
	}
	device, ok := identityField(value.FieldByName("Dev"))
	if !ok {
		return sourceIdentity{}, errors.New("inplace: source device identity is unavailable")
	}
	inode, ok := identityField(value.FieldByName("Ino"))
	if !ok {
		return sourceIdentity{}, errors.New("inplace: source inode identity is unavailable")
	}
	return sourceIdentity{kind: identityKindUnix, a: device, b: inode}, nil
}

func identityField(value reflect.Value) (uint64, bool) {
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
