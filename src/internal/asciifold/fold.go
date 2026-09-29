package asciifold

// Fold returns a byte-stable ASCII lowercase copy of b.
//
// It deliberately does not apply Unicode case folding: some Unicode folds change
// byte length, which makes byte-offset search and replacement unsafe.
func Fold(b []byte) []byte {
	out := make([]byte, len(b))
	for i, c := range b {
		out[i] = Lower(c)
	}
	return out
}

// Lower returns c folded to lowercase for ASCII A-Z only.
func Lower(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + ('a' - 'A')
	}
	return c
}

// IndexFolded returns the first byte offset of foldedNeedle in haystack using
// ASCII-only case folding. foldedNeedle must already be folded with Fold.
func IndexFolded(haystack []byte, foldedNeedle []byte) int {
	if len(foldedNeedle) == 0 {
		return 0
	}
	if len(foldedNeedle) > len(haystack) {
		return -1
	}
	first := foldedNeedle[0]
	limit := len(haystack) - len(foldedNeedle)
	work := 0
	for i := 0; i <= limit; i++ {
		if Lower(haystack[i]) != first {
			continue
		}
		matched := matchingFoldedPrefix(haystack[i:i+len(foldedNeedle)], foldedNeedle)
		if matched == len(foldedNeedle) {
			return i
		}
		work += matched
		if work > 64+4*(i+1) {
			if next := indexFoldedLinear(haystack[i+1:], foldedNeedle, false); next >= 0 {
				return i + 1 + next
			}
			return -1
		}
	}
	return -1
}

// LastIndexFolded returns the last byte offset of foldedNeedle in haystack
// using ASCII-only case folding. foldedNeedle must already be folded with Fold.
func LastIndexFolded(haystack []byte, foldedNeedle []byte) int {
	if len(foldedNeedle) == 0 {
		return len(haystack)
	}
	if len(foldedNeedle) > len(haystack) {
		return -1
	}
	first := foldedNeedle[0]
	limit := len(haystack) - len(foldedNeedle)
	work := 0
	for i := limit; i >= 0; i-- {
		if Lower(haystack[i]) != first {
			continue
		}
		matched := matchingFoldedPrefix(haystack[i:i+len(foldedNeedle)], foldedNeedle)
		if matched == len(foldedNeedle) {
			return i
		}
		work += matched
		if work > 64+4*(limit-i+1) {
			return indexFoldedLinear(haystack[:i+len(foldedNeedle)-1], foldedNeedle, true)
		}
	}
	return -1
}

func matchingFoldedPrefix(haystack []byte, foldedNeedle []byte) int {
	for i, c := range foldedNeedle {
		if Lower(haystack[i]) != c {
			return i
		}
	}
	return len(foldedNeedle)
}

// indexFoldedLinear uses Knuth-Morris-Pratt after repeated prefix comparisons
// exceed a linear work budget. Ordinary searches remain allocation-free, while
// repetitive source bytes cannot cause work proportional to source * pattern.
// The fallback table is bounded by the pattern, never by the source size.
func indexFoldedLinear(haystack []byte, needle []byte, last bool) int {
	failure := make([]int, len(needle))
	for i, prefix := 1, 0; i < len(needle); i++ {
		for prefix > 0 && needle[i] != needle[prefix] {
			prefix = failure[prefix-1]
		}
		if needle[i] == needle[prefix] {
			prefix++
		}
		failure[i] = prefix
	}

	result := -1
	matched := 0
	for i, raw := range haystack {
		c := Lower(raw)
		for matched > 0 && c != needle[matched] {
			matched = failure[matched-1]
		}
		if c == needle[matched] {
			matched++
		}
		if matched == len(needle) {
			result = i + 1 - len(needle)
			if !last {
				return result
			}
			matched = failure[matched-1]
		}
	}
	return result
}
