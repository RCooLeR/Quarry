package document

// sourceChangeToken is an OS-provided mutation generation for an opened file.
// It supplements size and modification time because both of those values can
// be restored after a same-object rewrite. Tokens are deliberately private:
// callers can ask FileDocument to validate its retained generation, but cannot
// manufacture a weaker comparison.
type sourceChangeToken struct {
	available bool
	strong    bool
	kind      uint8
	a         uint64
	b         uint64
}
