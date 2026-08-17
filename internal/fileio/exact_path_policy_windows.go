//go:build windows

package fileio

import (
	"path/filepath"
	"strings"
)

// validatePlatformExactPath rejects Windows spellings whose filesystem meaning
// is not one ordinary file/directory at the exact requested pathname. In
// particular, a colon outside the volume prefix names an NTFS alternate data
// stream rather than a new file.
func validatePlatformExactPath(path string, clean string, directory bool) error {
	volume := filepath.VolumeName(path)
	if volume != "" && !filepath.IsAbs(path) {
		return invalidWindowsExactPath(path, clean, "drive-relative path spellings are not supported")
	}
	lowerPath := strings.ToLower(path)
	if strings.HasPrefix(lowerPath, `\\.\`) || strings.HasPrefix(lowerPath, `\??\`) {
		return invalidWindowsExactPath(path, clean, "Win32 device-namespace paths are not supported")
	}
	isExtended := strings.HasPrefix(lowerPath, `\\?\`)
	if isExtended && !isAllowedWindowsExtendedPath(lowerPath) {
		return invalidWindowsExactPath(path, clean, "extended device namespaces other than drive and UNC paths are not supported")
	}
	if strings.HasPrefix(lowerPath, `\\?\unc\`) {
		return validateWindowsExtendedUNCPath(path, clean, directory)
	}
	if reason := invalidWindowsUNCVolumeReason(volume, isExtended); reason != "" {
		return invalidWindowsExactPath(path, clean, reason)
	}

	remainder := path[len(volume):]
	if strings.Contains(remainder, ":") {
		return invalidWindowsExactPath(path, clean, "NTFS alternate-data-stream path spellings are not supported")
	}
	components := strings.Split(remainder, `\`)
	for _, component := range components {
		if component == "" {
			continue
		}
		if !isExtended && (strings.HasSuffix(component, ".") || strings.HasSuffix(component, " ")) {
			return invalidWindowsExactPath(path, clean, "ordinary Win32 components ending in a dot or space are not exact")
		}
		if strings.ContainsAny(component, `<>"|?*`) || containsWindowsControlCharacter(component) {
			return invalidWindowsExactPath(path, clean, "path contains a Win32-reserved character")
		}
		if isWindowsReservedComponent(component) {
			return invalidWindowsExactPath(path, clean, "reserved DOS device-name components are not supported")
		}
	}
	return nil
}

func validateWindowsExtendedUNCPath(path string, clean string, directory bool) error {
	const prefix = `\\?\UNC\`
	components := strings.Split(path[len(prefix):], `\`)
	if len(components) < 2 || components[0] == "" || components[1] == "" {
		return invalidWindowsExactPath(path, clean, "extended UNC paths require non-empty server and share components")
	}
	if !directory && len(components) < 3 {
		return invalidWindowsExactPath(path, clean, "an output path under extended UNC syntax must name a child of the share")
	}
	for index, component := range components {
		if component == "" || component == "." || component == ".." {
			return invalidWindowsExactPath(path, clean, "extended UNC path contains an empty or dot-only component")
		}
		if strings.ContainsAny(component, `:<>"|?*`) || containsWindowsControlCharacter(component) {
			return invalidWindowsExactPath(path, clean, "extended UNC path contains a Win32-reserved character")
		}
		if index >= 2 && isWindowsReservedComponent(component) {
			return invalidWindowsExactPath(path, clean, "reserved DOS device-name components are not supported")
		}
	}
	return nil
}

func invalidWindowsUNCVolumeReason(volume string, extended bool) string {
	prefix := `\\`
	lowerVolume := strings.ToLower(volume)
	if extended {
		prefix = `\\?\unc\`
		if !strings.HasPrefix(lowerVolume, prefix) {
			return ""
		}
	} else if !strings.HasPrefix(volume, prefix) {
		return ""
	}
	components := strings.Split(volume[len(prefix):], `\`)
	if len(components) != 2 || components[0] == "" || components[1] == "" {
		return "UNC paths must contain exact non-empty server and share components"
	}
	for _, component := range components {
		if strings.ContainsAny(component, `:<>"|?*`) || containsWindowsControlCharacter(component) {
			return "UNC server/share contains a Win32-reserved character"
		}
		if !extended && (strings.HasSuffix(component, ".") || strings.HasSuffix(component, " ")) {
			return "ordinary Win32 UNC server/share components ending in a dot or space are not exact"
		}
	}
	return ""
}

func isAllowedWindowsExtendedPath(lowerPath string) bool {
	if strings.HasPrefix(lowerPath, `\\?\unc\`) {
		return true
	}
	return len(lowerPath) >= 7 && strings.HasPrefix(lowerPath, `\\?\`) &&
		lowerPath[4] >= 'a' && lowerPath[4] <= 'z' && lowerPath[5] == ':' && lowerPath[6] == '\\'
}

func invalidWindowsExactPath(path string, clean string, reason string) error {
	return &InvalidExactPathError{Path: path, CleanPath: clean, Reason: reason}
}

func containsWindowsControlCharacter(component string) bool {
	for _, r := range component {
		if r >= 0 && r < 32 {
			return true
		}
	}
	return false
}

func isWindowsReservedComponent(component string) bool {
	// Win32 device parsing ignores an extension and spaces immediately before
	// it. Superscript 1/2/3 are also DOS-device digits for COM/LPT names.
	stem := component
	if dot := strings.IndexByte(stem, '.'); dot >= 0 {
		stem = stem[:dot]
	}
	stem = strings.TrimRight(stem, " ")
	stem = strings.ToUpper(stem)
	switch stem {
	case "CON", "PRN", "AUX", "NUL", "CLOCK$", "CONIN$", "CONOUT$":
		return true
	}
	if !strings.HasPrefix(stem, "COM") && !strings.HasPrefix(stem, "LPT") {
		return false
	}
	switch stem[3:] {
	case "1", "2", "3", "4", "5", "6", "7", "8", "9", "¹", "²", "³":
		return true
	default:
		return false
	}
}
