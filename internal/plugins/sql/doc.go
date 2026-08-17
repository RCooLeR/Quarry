// Package sql provides SQL language support and dump-oriented tooling.
//
// It includes syntax highlighting, schema/table analysis, reshape, and
// extraction helpers. Generic byte/regex replacement and cleanup presets are
// disabled because they cannot preserve structured or length-encoded values
// such as PHP-serialized WordPress data. Enabled dump transforms either copy
// analyzed regions or preserve accepted INSERT tuple payload bytes exactly;
// ambiguous syntax fails before output publication. The tools are designed for
// large database dumps, so they prefer fixed-state streaming scans and bounded
// metadata summaries over whole-file parsing.
package sql
