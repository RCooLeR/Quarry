//go:build windows

package inplace

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

func sourceIdentityForFile(f *os.File) (sourceIdentity, error) {
	if f == nil {
		return sourceIdentity{}, fmt.Errorf("inplace: source handle is nil")
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(f.Fd()), &info); err != nil {
		return sourceIdentity{}, fmt.Errorf("inplace: read source identity: %w", err)
	}
	return sourceIdentity{
		kind: identityKindWindows,
		a:    uint64(info.VolumeSerialNumber),
		b:    uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow),
	}, nil
}
