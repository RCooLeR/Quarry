// Package replace contains dormant, package-private streaming replacement
// implementations retained for tests and future structured-transform work.
//
// Replacements never patch the source file in place while scanning. The package
// writes a new file, tracks lifecycle evidence in manifests, supports recovery,
// and verifies the source has not changed before any optional final swap.
//
// The implementation is chunked because files may be tens or hundreds of
// gigabytes. Boundary carry buffers let matches cross chunk edges without
// loading the whole source into memory.
//
// No mutation entry point is exported to the rest of the application. File-
// level replacement also fails closed when a constant-memory source scan
// recognizes native PHP/WordPress serialization. Generic dump-byte rewriting
// cannot safely maintain the decoded byte lengths embedded in that format.
package replace
