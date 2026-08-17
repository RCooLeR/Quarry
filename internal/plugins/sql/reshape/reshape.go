// Package reshape rewrites the statement layout of a conservative subset of
// SQL INSERT ... VALUES statements. Tuple payload bytes are preserved exactly:
// an extended INSERT can be exploded into one statement per row, or compatible
// single-row INSERTs can be batched. Statement grouping itself is not claimed
// to be semantically equivalent because database-side triggers, atomicity, and
// rollback behavior may depend on it. Accepted non-target SQL is copied through
// byte-for-byte; ambiguous client commands, procedural bodies, raw-data modes,
// and unsupported dialect constructs make the operation fail without publishing
// an output file.
package reshape

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/quarry/quarry-wails3/internal/fileio"
	sqldelimiter "github.com/quarry/quarry-wails3/internal/plugins/sql/delimiter"
	"github.com/quarry/quarry-wails3/internal/sourceio"
)

// Mode selects the reshape direction.
type Mode string

const (
	// ModeSingleRow explodes extended INSERTs into one row per statement.
	ModeSingleRow Mode = "single"
	// ModeMultiRow batches consecutive same-prefix single-row INSERTs.
	ModeMultiRow Mode = "multi"
)

// Options configures a reshape run.
type Options struct {
	Mode           Mode
	BatchSize      int // ModeMultiRow: max rows per output INSERT (default 100)
	ExpectedSource *sourceio.Expectation
	Progress       func(Summary)
}

// Summary reports what a reshape produced.
type Summary struct {
	StatementsRead    int64
	InsertsRewritten  int64
	RowsSeen          int64
	StatementsWritten int64
}

// maxStatementBytes caps how much of a single statement we buffer before giving
// up and streaming it through verbatim. A normal mysqldump extended INSERT is
// well under this; the cap only guards against a pathological single statement.
const maxStatementBytes = 64 << 20

const (
	MaxBatchRows  = 10_000
	maxBatchBytes = 8 << 20
)

var (
	ErrUnsupportedInsert = errors.New("INSERT form is not safe to reshape")
	// ErrUnsupportedDelimiter is returned for mysql-client DELIMITER scripts,
	// whose statement boundaries cannot be inferred from ordinary semicolons.
	ErrUnsupportedDelimiter = sqldelimiter.ErrUnsupported
	// ErrUnsupportedCompoundStatement prevents routine bodies and client-owned
	// raw-data sections from being mistaken for top-level INSERT statements.
	ErrUnsupportedCompoundStatement = errors.New("compound SQL body or client-managed raw-data payload is not safe to reshape")
	// ErrUnsupportedLexicalConstruct rejects dialect quoting/comment syntax the
	// intentionally small reshape lexer cannot prove safe.
	ErrUnsupportedLexicalConstruct = errors.New("unsupported SQL dialect quoting or comment construct")
)

// ReshapeInsertsFile streams src to dst, reshaping INSERT row layout per opts.
// The source is never modified.
func ReshapeInsertsFile(ctx context.Context, srcPath, dstPath string, opts Options) (summary Summary, retErr error) {
	if opts.Mode != ModeSingleRow && opts.Mode != ModeMultiRow {
		return Summary{}, fmt.Errorf("unsupported reshape mode %q", opts.Mode)
	}
	if opts.BatchSize <= 0 {
		opts.BatchSize = 100
	}
	if opts.BatchSize > MaxBatchRows {
		return Summary{}, fmt.Errorf("reshape batch size %d exceeds limit %d", opts.BatchSize, MaxBatchRows)
	}
	in, err := sourceio.OpenContext(ctx, srcPath, opts.ExpectedSource)
	if err != nil {
		return Summary{}, err
	}
	defer func() {
		retErr = errors.Join(retErr, in.Close())
	}()
	out, err := fileio.OpenAtomicOutput(dstPath, []string{srcPath}, 0o600)
	if err != nil {
		return Summary{}, err
	}
	bw := bufio.NewWriterSize(out, 1<<20)
	defer func() {
		if err := out.Cleanup(); err != nil {
			retErr = errors.Join(retErr, err)
		}
	}()

	summary, err = reshapeStream(ctx, bufio.NewReaderSize(in, 1<<20), bw, opts)
	if err != nil {
		return summary, err
	}
	if err := bw.Flush(); err != nil {
		return summary, err
	}
	if err := ctxErr(ctx); err != nil {
		return summary, err
	}
	if err := out.CommitContextValidated(ctx, in.ValidateContext); err != nil {
		return summary, err
	}
	return summary, nil
}

// out is a write sink that remembers the last byte written, so a reshaped
// statement can guarantee it starts on a fresh line (otherwise a verbatim
// statement ending in ';' would run into the next reshaped INSERT).
type out struct {
	w    *bufio.Writer
	last byte
}

func (o *out) str(s string) error {
	if len(s) == 0 {
		return nil
	}
	if _, err := o.w.WriteString(s); err != nil {
		return err
	}
	o.last = s[len(s)-1]
	return nil
}

func (o *out) bytes(p []byte) error {
	if len(p) == 0 {
		return nil
	}
	if _, err := o.w.Write(p); err != nil {
		return err
	}
	o.last = p[len(p)-1]
	return nil
}

func (o *out) ensureNL() error {
	if o.last != '\n' {
		return o.str("\n")
	}
	return nil
}

// batcher coalesces single-row INSERTs that share a prefix into extended ones.
type batcher struct {
	o         *out
	batchSize int
	prefix    string
	tuples    []string
	bytes     int
	sum       *Summary
}

func (b *batcher) flush() error {
	if len(b.tuples) == 0 {
		return nil
	}
	if err := b.o.ensureNL(); err != nil {
		return err
	}
	if err := b.o.str(b.prefix); err != nil {
		return err
	}
	if err := b.o.str(strings.Join(b.tuples, ",")); err != nil {
		return err
	}
	if err := b.o.str(";\n"); err != nil {
		return err
	}
	b.sum.StatementsWritten++
	b.sum.InsertsRewritten++
	b.tuples = b.tuples[:0]
	b.prefix = ""
	b.bytes = 0
	return nil
}

func (b *batcher) add(prefix string, tuples []string) error {
	for _, t := range tuples {
		if len(prefix)+len(t) > maxBatchBytes {
			return fmt.Errorf("one reshaped INSERT row exceeds %d-byte batch memory limit", maxBatchBytes)
		}
		needed := len(t)
		if b.prefix == "" {
			needed += len(prefix)
		} else {
			needed++ // tuple separator
		}
		if b.prefix != "" && (b.prefix != prefix || len(b.tuples) >= b.batchSize || b.bytes+needed > maxBatchBytes) {
			if err := b.flush(); err != nil {
				return err
			}
			needed = len(prefix) + len(t)
		}
		b.prefix = prefix
		b.tuples = append(b.tuples, t)
		b.bytes += needed
	}
	return nil
}

func reshapeStream(ctx context.Context, r *bufio.Reader, w *bufio.Writer, opts Options) (Summary, error) {
	var sum Summary
	o := &out{w: w, last: '\n'}
	bat := &batcher{o: o, batchSize: opts.BatchSize, sum: &sum}
	var stmt bytes.Buffer
	lex := lexer{}
	overflow := false // current statement exceeded the buffer cap; pass through
	safety := reshapeSafetyGuard{}

	emitVerbatim := func(p []byte) error {
		if opts.Mode == ModeMultiRow {
			if err := bat.flush(); err != nil {
				return err
			}
		}
		return o.bytes(p)
	}

	flushStatement := func() error {
		defer stmt.Reset()
		if stmt.Len() == 0 {
			return nil
		}
		sum.StatementsRead++
		defer func() {
			if opts.Progress != nil {
				opts.Progress(sum)
			}
		}()
		parsed, isInsert, err := parseInsert(stmt.Bytes())
		if err != nil {
			return fmt.Errorf("statement %d: %w", sum.StatementsRead, err)
		}
		if !isInsert {
			return emitVerbatim(stmt.Bytes())
		}
		sum.RowsSeen += int64(len(parsed.tuples))
		switch opts.Mode {
		case ModeMultiRow:
			if len(bytes.TrimSpace(parsed.leading)) > 0 {
				if err := bat.flush(); err != nil {
					return err
				}
				if err := o.bytes(parsed.leading); err != nil {
					return err
				}
			}
			return bat.add(parsed.prefix, parsed.tuples)
		default: // ModeSingleRow
			if err := o.bytes(parsed.leading); err != nil {
				return err
			}
			for _, t := range parsed.tuples {
				if err := o.ensureNL(); err != nil {
					return err
				}
				if err := o.str(parsed.prefix); err != nil {
					return err
				}
				if err := o.str(t); err != nil {
					return err
				}
				if err := o.str(";\n"); err != nil {
					return err
				}
				sum.StatementsWritten++
			}
			sum.InsertsRewritten++
			return nil
		}
	}

	buf := make([]byte, 256*1024)
	for {
		if err := ctxErr(ctx); err != nil {
			return sum, err
		}
		n, err := r.Read(buf)
		for i := 0; i < n; i++ {
			c := buf[i]
			if guardErr := safety.Step(c); guardErr != nil {
				return sum, guardErr
			}
			atTop := lex.step(c)
			if overflow {
				if err := o.bytes(buf[i : i+1]); err != nil {
					return sum, err
				}
				if atTop && c == ';' {
					overflow = false
					sum.StatementsRead++
				}
				continue
			}
			stmt.WriteByte(c)
			if atTop && c == ';' {
				if ferr := flushStatement(); ferr != nil {
					return sum, ferr
				}
			} else if stmt.Len() >= maxStatementBytes {
				// A non-INSERT statement may be passed through verbatim. A target
				// INSERT that exceeds the parser budget must fail the operation;
				// silently skipping it would produce a partially reshaped dump. If
				// the first live token is not resolved yet (for example, the cap is
				// reached inside a leading comment), fail closed as well: later bytes
				// could reveal an INSERT that we would otherwise stream past.
				switch classifyStatementPrefix(stmt.Bytes()) {
				case statementPrefixInsert:
					return sum, fmt.Errorf("%w: statement exceeds %d-byte parser limit", ErrUnsupportedInsert, maxStatementBytes)
				case statementPrefixUnknown:
					return sum, fmt.Errorf("%w: statement prefix is unresolved at the %d-byte parser limit", ErrUnsupportedInsert, maxStatementBytes)
				}
				if werr := emitVerbatim(stmt.Bytes()); werr != nil {
					return sum, werr
				}
				stmt.Reset()
				overflow = true
			}
		}
		if err == io.EOF {
			if guardErr := safety.Finish(); guardErr != nil {
				return sum, guardErr
			}
			break
		}
		if err != nil {
			return sum, err
		}
	}
	// Trailing bytes with no terminating ';'.
	if stmt.Len() > 0 {
		if ferr := flushStatement(); ferr != nil {
			return sum, ferr
		}
	}
	if opts.Mode == ModeMultiRow {
		if err := bat.flush(); err != nil {
			return sum, err
		}
	}
	return sum, nil
}

type statementPrefixClass uint8

const (
	statementPrefixUnknown statementPrefixClass = iota
	statementPrefixInsert
	statementPrefixOther
)

// classifyStatementPrefix determines whether the first live SQL token is
// INSERT without reading beyond the bounded statement prefix. Unknown is
// intentionally distinct from non-INSERT: a prefix ending in whitespace, a
// line comment, an unterminated block comment, or a partial trivia opener may
// still reveal INSERT when more bytes arrive.
func classifyStatementPrefix(statement []byte) statementPrefixClass {
	index, resolved := scanLeadingTrivia(statement)
	if !resolved {
		return statementPrefixUnknown
	}
	rest := statement[index:]
	const keyword = "insert"
	limit := len(rest)
	if limit > len(keyword) {
		limit = len(keyword)
	}
	for offset := 0; offset < limit; offset++ {
		if lower(rest[offset]) != keyword[offset] {
			return statementPrefixOther
		}
	}
	if len(rest) < len(keyword) {
		return statementPrefixUnknown
	}
	if len(rest) == len(keyword) || !isWordByte(rest[len(keyword)]) {
		return statementPrefixInsert
	}
	return statementPrefixOther
}

// lexer tracks SQL string/comment/identifier state so statement and tuple
// boundaries are only recognized at the top level. step returns true when, after
// consuming c, the lexer is back at normal (non-string, non-comment) state — i.e.
// c was a structural byte that counts toward statement/tuple parsing.
type lexer struct {
	inSingle   bool
	inDouble   bool
	inBacktick bool
	inLine     bool
	inBlock    bool
	esc        bool
	prevStar   bool
	prevDash   bool
	prevSlash  bool
}

func (l *lexer) inLiteral() bool {
	return l.inSingle || l.inDouble || l.inBacktick || l.inLine || l.inBlock
}

func (l *lexer) step(c byte) bool {
	switch {
	case l.inLine:
		if c == '\n' || c == '\r' {
			l.inLine = false
		}
		return false
	case l.inBlock:
		if c == '/' && l.prevStar {
			l.inBlock = false
			l.prevStar = false
			return false
		}
		l.prevStar = c == '*'
		return false
	case l.inSingle:
		if l.esc {
			l.esc = false
		} else if c == '\\' {
			l.esc = true
		} else if c == '\'' {
			l.inSingle = false
		}
		return false
	case l.inDouble:
		if l.esc {
			l.esc = false
		} else if c == '\\' {
			l.esc = true
		} else if c == '"' {
			l.inDouble = false
		}
		return false
	case l.inBacktick:
		if c == '`' {
			l.inBacktick = false
		}
		return false
	}
	// normal state
	l.prevStar = false
	switch c {
	case '\'':
		l.inSingle = true
		l.prevDash, l.prevSlash = false, false
		return false
	case '"':
		l.inDouble = true
		l.prevDash, l.prevSlash = false, false
		return false
	case '`':
		l.inBacktick = true
		l.prevDash, l.prevSlash = false, false
		return false
	case '#':
		l.inLine = true
		l.prevDash, l.prevSlash = false, false
		return false
	case '-':
		if l.prevDash {
			l.inLine = true
			l.prevDash = false
			return false
		}
		l.prevDash = true
		l.prevSlash = false
		return true
	case '*':
		if l.prevSlash {
			l.inBlock = true
			l.prevSlash = false
			l.prevDash = false
			return false
		}
		l.prevDash, l.prevSlash = false, false
		return true
	case '/':
		l.prevSlash = true
		l.prevDash = false
		return true
	default:
		l.prevDash, l.prevSlash = false, false
		return true
	}
}

func ctxErr(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

type parsedInsert struct {
	leading []byte
	prefix  string
	tuples  []string
}

// parseInsert accepts only the grammar this transform can preserve exactly:
// INSERT ... VALUES <tuple>(,<tuple>)*;. Non-INSERT statements are returned as
// isInsert=false. Every unsupported or malformed INSERT fails the whole
// operation so no semantically incomplete final output can be published.
func parseInsert(stmt []byte) (parsed parsedInsert, isInsert bool, err error) {
	// Skip leading whitespace AND comments. mysqldump prefixes each table's data
	// with a "-- Dumping data for table `x`" comment block that gets buffered
	// together with the following INSERT. Ordinary provenance comments are
	// preserved once before the rewritten rows; executable/optimizer comments
	// in this position are rejected because duplicating or moving them is not
	// proven semantics-preserving.
	triviaEnd := skipLeadingTrivia(stmt)
	rest := stmt[triviaEnd:]
	if !matchKeyword(rest, 0, "insert") {
		return parsedInsert{}, false, nil
	}
	if containsExecutableSQLComment(stmt) {
		return parsedInsert{}, true, fmt.Errorf("%w: executable or optimizer comment in target INSERT", ErrUnsupportedInsert)
	}
	// Locate the VALUES keyword at top level.
	vi := findValues(rest)
	if vi < 0 {
		return parsedInsert{}, true, fmt.Errorf("%w: expected a top-level VALUES tuple list", ErrUnsupportedInsert)
	}
	if containsTopLevelKeyword(rest[:vi], "overwrite") {
		return parsedInsert{}, true, fmt.Errorf("%w: INSERT OVERWRITE is not row-additive", ErrUnsupportedInsert)
	}
	// prefix = "INSERT ... VALUES " with a single normalized trailing space.
	pre := string(rest[:vi])
	pre = strings.TrimRight(pre, " \t\r\n")
	parsed.prefix = pre + " "
	parsed.leading = append([]byte(nil), stmt[:triviaEnd]...)

	parsed.tuples, err = splitTuplesStrict(rest[vi:])
	if err != nil {
		return parsedInsert{}, true, fmt.Errorf("%w: %v", ErrUnsupportedInsert, err)
	}
	return parsed, true, nil
}

// containsExecutableSQLComment recognizes MySQL executable comments and
// optimizer hints only when their opener is live SQL. Bytes inside quoted
// values or identifiers are payload and must not be mistaken for directives.
func containsExecutableSQLComment(statement []byte) bool {
	lex := lexer{}
	for index := 0; index+2 < len(statement); index++ {
		if !lex.inLiteral() && statement[index] == '/' && statement[index+1] == '*' {
			marker := statement[index+2]
			if marker == '!' || marker == '+' ||
				((marker == 'm' || marker == 'M') && index+3 < len(statement) && statement[index+3] == '!') {
				return true
			}
		}
		lex.step(statement[index])
	}
	return false
}

func containsTopLevelKeyword(statement []byte, keyword string) bool {
	lex := lexer{}
	for index := 0; index < len(statement); index++ {
		top := lex.step(statement[index])
		if top && !lex.inLiteral() && matchKeyword(statement, index, keyword) {
			return true
		}
	}
	return false
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\r' || c == '\n' || c == '\f' || c == '\v'
}

// scanLeadingTrivia returns the index of the first byte that is not leading
// whitespace, a '--' or '#' line comment, or a '/* */' block comment. resolved
// is false when the bounded input ends before a first live token is known.
func scanLeadingTrivia(b []byte) (index int, resolved bool) {
	i := 0
	if len(b) < 3 && len(b) > 0 && b[0] == 0xef {
		if len(b) == 1 || b[1] == 0xbb {
			return len(b), false
		}
	}
	if len(b) >= 3 && b[0] == 0xef && b[1] == 0xbb && b[2] == 0xbf {
		i = 3
	}
	for i < len(b) {
		c := b[i]
		switch {
		case isSpace(c):
			i++
		case c == '#':
			for i < len(b) && b[i] != '\n' && b[i] != '\r' {
				i++
			}
			if i == len(b) {
				return i, false
			}
		case c == '-' && i+1 < len(b) && b[i+1] == '-':
			i += 2
			for i < len(b) && b[i] != '\n' && b[i] != '\r' {
				i++
			}
			if i == len(b) {
				return i, false
			}
		case c == '/' && i+1 < len(b) && b[i+1] == '*':
			i += 2
			for i+1 < len(b) && !(b[i] == '*' && b[i+1] == '/') {
				i++
			}
			if i+1 < len(b) {
				i += 2 // consume the closing */
			} else {
				return len(b), false
			}
		case (c == '-' || c == '/') && i+1 == len(b):
			return i, false
		default:
			return i, true
		}
	}
	return i, false
}

// skipLeadingTrivia is the complete-input form used by statement parsing.
// Input containing only trivia has no live token, so its full length is
// returned whether the final line/block comment is terminated or not.
func skipLeadingTrivia(b []byte) int {
	index, _ := scanLeadingTrivia(b)
	return index
}

func lower(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + 32
	}
	return c
}

// findValues returns the index just AFTER the top-level VALUES (or VALUE)
// keyword in an INSERT statement, or -1. It uses a lexer so a column named
// `values` (backtick-quoted) or the word inside a string isn't matched.
func findValues(b []byte) int {
	lex := lexer{}
	for i := 0; i < len(b); i++ {
		top := lex.step(b[i])
		if !top || lex.inLiteral() {
			continue
		}
		// match VALUES at a word boundary
		if matchKeyword(b, i, "values") {
			return i + len("values")
		}
		if matchKeyword(b, i, "value") {
			return i + len("value")
		}
	}
	return -1
}

// matchKeyword reports whether b[i:] begins with word (case-insensitive) at a
// word boundary on both sides.
func matchKeyword(b []byte, i int, word string) bool {
	if i+len(word) > len(b) {
		return false
	}
	if i > 0 && isWordByte(b[i-1]) {
		return false
	}
	for j := 0; j < len(word); j++ {
		if lower(b[i+j]) != word[j] {
			return false
		}
	}
	after := i + len(word)
	if after < len(b) && isWordByte(b[after]) {
		return false
	}
	return true
}

func isWordByte(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
		c == '_' || c == '$' || c >= 0x80
}

// splitTuplesStrict accepts only whitespace-separated tuples joined by commas,
// followed by a semicolon and optional whitespace. It rejects every trailing
// clause rather than guessing whether that clause may be duplicated safely.
func splitTuplesStrict(b []byte) ([]string, error) {
	skipSpace := func(i int) int {
		for i < len(b) && isSpace(b[i]) {
			i++
		}
		return i
	}

	var tuples []string
	i := skipSpace(0)
	for {
		if i >= len(b) || b[i] != '(' {
			return nil, fmt.Errorf("expected value tuple at byte %d", i)
		}
		start := i
		depth := 0
		lex := lexer{}
		closed := false
		for i < len(b) {
			top := lex.step(b[i])
			if top && !lex.inLiteral() {
				switch b[i] {
				case '(':
					depth++
				case ')':
					if depth == 0 {
						return nil, fmt.Errorf("unexpected closing parenthesis at byte %d", i)
					}
					depth--
					if depth == 0 {
						i++
						tuples = append(tuples, string(b[start:i]))
						closed = true
					}
				}
			}
			if closed {
				break
			}
			i++
		}
		if !closed || lex.inLiteral() {
			return nil, errors.New("unterminated value tuple")
		}

		i = skipSpace(i)
		if i >= len(b) {
			return nil, errors.New("missing statement terminator")
		}
		switch b[i] {
		case ',':
			i = skipSpace(i + 1)
			continue
		case ';':
			i = skipSpace(i + 1)
			if i != len(b) {
				return nil, fmt.Errorf("unsupported trailing tokens at byte %d", i)
			}
			return tuples, nil
		default:
			return nil, fmt.Errorf("unsupported trailing clause or malformed tuple separator at byte %d", i)
		}
	}
}

// FormatNote returns a short human description of a reshape result.
func FormatNote(mode Mode, sum Summary) string {
	switch mode {
	case ModeMultiRow:
		return fmt.Sprintf("%d rows batched into %d INSERTs", sum.RowsSeen, sum.InsertsRewritten)
	default:
		return fmt.Sprintf("%d extended INSERTs → %d single-row INSERTs", sum.InsertsRewritten, sum.StatementsWritten)
	}
}
