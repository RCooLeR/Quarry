package main

import "strings"

func plainSearchUnsupportedReason(encoding string, query string, caseSensitive bool, wholeWord bool) string {
	encoding = strings.TrimSpace(encoding)
	isUTF8 := encoding == "" || strings.EqualFold(encoding, "UTF-8")
	if wholeWord && !isUTF8 {
		return "Whole-word search is unavailable for " + encoding + " files because byte boundaries do not represent character boundaries. Convert the file to UTF-8 or turn off Whole word."
	}
	if !caseSensitive && !isASCIIText(query) {
		return "Case-insensitive plain search currently supports ASCII terms only. Turn on Match case for this term."
	}
	return ""
}

func plainSearchByteAlignment(encoding string) int {
	if strings.EqualFold(strings.TrimSpace(encoding), "UTF-16LE") || strings.EqualFold(strings.TrimSpace(encoding), "UTF-16BE") {
		return 2
	}
	return 1
}

func isASCIIText(value string) bool {
	for i := 0; i < len(value); i++ {
		if value[i] >= 0x80 {
			return false
		}
	}
	return true
}
