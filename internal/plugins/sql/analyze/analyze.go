package analyze

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
)

type ReaderAtSize interface {
	io.ReaderAt
	Size() int64
}

type Progress struct {
	BytesProcessed int64
	BytesTotal     int64
}

type Table struct {
	Name         string
	CreateOffset int64
	InsertOffset int64
}

type Summary struct {
	Tables          []Table
	CreateTables    int
	InsertTables    int
	DefinerCount    int
	Charsets        map[string]int
	Collations      map[string]int
	MysqldumpHeader bool
}

type Options struct {
	ChunkSize int
	Progress  func(Progress)
}

var (
	createTableRe = regexp.MustCompile("(?i)CREATE(?:\\s+DEFINER\\s*=\\s*`?[^`\\s]+`?@`?[^`\\s]+`?)?\\s+TABLE(?:\\s+IF\\s+NOT\\s+EXISTS)?\\s+`?([a-zA-Z0-9_.$-]+)`?")
	definerRe     = regexp.MustCompile("(?i)DEFINER\\s*=\\s*`?[^`\\s]+`?@`?[^`\\s]+`?")
	charsetRe     = regexp.MustCompile("(?i)(?:DEFAULT\\s+)?(?:CHARSET|CHARACTER\\s+SET)\\s*=?\\s*`?([a-zA-Z0-9_]+)`?")
	collationRe   = regexp.MustCompile("(?i)COLLATE\\s*=?\\s*`?([a-zA-Z0-9_]+)`?")
	mysqldumpRe   = regexp.MustCompile("(?i)mysqldump|mysql\\s+dump")
)

const analyzerCarrySize = 16 * 1024

func AnalyzeFile(ctx context.Context, path string, opts Options) (Summary, error) {
	f, err := os.Open(path)
	if err != nil {
		return Summary{}, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return Summary{}, err
	}
	return Analyze(ctx, fileReaderAtSize{File: f, size: st.Size()}, opts)
}

type fileReaderAtSize struct {
	*os.File
	size int64
}

func (f fileReaderAtSize) Size() int64 {
	return f.size
}

func Analyze(ctx context.Context, r ReaderAtSize, opts Options) (Summary, error) {
	if opts.ChunkSize <= 0 {
		opts.ChunkSize = 16 * 1024 * 1024
	}
	buf := make([]byte, opts.ChunkSize)
	carry := make([]byte, 0, analyzerCarrySize)
	window := make([]byte, 0, opts.ChunkSize+analyzerCarrySize)
	processed := int64(0)
	total := r.Size()

	summary := Summary{
		Charsets:   make(map[string]int),
		Collations: make(map[string]int),
	}
	tables := make(map[string]*Table)

	for processed < total {
		select {
		case <-ctx.Done():
			return Summary{}, ctx.Err()
		default:
		}

		want := opts.ChunkSize
		if remaining := total - processed; remaining < int64(want) {
			want = int(remaining)
		}
		n, err := r.ReadAt(buf[:want], processed)
		if err != nil && !errors.Is(err, io.EOF) {
			return Summary{}, err
		}
		if n == 0 {
			break
		}

		window = window[:0]
		window = append(window, carry...)
		window = append(window, buf[:n]...)
		windowStart := processed - int64(len(carry))
		processLimit := len(window) - analyzerCarrySize
		if errors.Is(err, io.EOF) {
			processLimit = len(window)
		}
		if processLimit < 0 {
			processLimit = 0
		}

		processWindow(&summary, tables, window, windowStart, processLimit)

		if processLimit < len(window) {
			carry = append(carry[:0], window[processLimit:]...)
		} else {
			carry = carry[:0]
		}
		processed += int64(n)
		if opts.Progress != nil {
			opts.Progress(Progress{BytesProcessed: processed, BytesTotal: total})
		}
	}

	if len(carry) > 0 {
		processWindow(&summary, tables, carry, total-int64(len(carry)), len(carry))
	}

	for _, table := range tables {
		summary.Tables = append(summary.Tables, *table)
		if table.CreateOffset >= 0 {
			summary.CreateTables++
		}
		if table.InsertOffset >= 0 {
			summary.InsertTables++
		}
	}
	sort.Slice(summary.Tables, func(i, j int) bool {
		left := tablePrimaryOffset(summary.Tables[i])
		right := tablePrimaryOffset(summary.Tables[j])
		if left != right {
			return left < right
		}
		return summary.Tables[i].Name < summary.Tables[j].Name
	})
	return summary, nil
}

func processWindow(summary *Summary, tables map[string]*Table, window []byte, windowStart int64, processLimit int) {
	scan := maskSQLLiteralsAndComments(window)
	if processLimit > len(scan) {
		processLimit = len(scan)
	}
	if processLimit < 0 {
		processLimit = 0
	}
	if !summary.MysqldumpHeader && containsFold(window, "mysql") {
		if match := mysqldumpRe.FindIndex(window); matchStartsBeforeLimit(match, processLimit) {
			summary.MysqldumpHeader = true
		}
	}

	// INSERT INTO is the hot path on data dumps: a fast ASCII fold scan + a
	// hand-rolled parse instead of a case-insensitive RE2 scan (which dominated
	// CPU profiling). Equivalent to insertIntoRe over the masked window.
	for pos := 0; pos < processLimit; {
		k := indexFold(scan, "insert", pos)
		if k < 0 || k >= processLimit {
			break
		}
		ns, ne, end, ok := parseInsertInto(scan, k)
		if ok {
			table := ensureTable(tables, normalizeIdentifier(window[ns:ne]))
			if table.InsertOffset < 0 {
				table.InsertOffset = windowStart + int64(k)
			}
			pos = end
		} else {
			pos = k + 1
		}
	}

	// Rarer statements: only run the precise regex when its keyword is present
	// in the window (a cheap fold scan), so INSERT-only windows skip them all.
	if containsFold(scan, "create") {
		for _, match := range createTableRe.FindAllSubmatchIndex(scan, -1) {
			if !matchStartsBeforeLimit(match, processLimit) {
				continue
			}
			name := normalizeIdentifier(window[match[2]:match[3]])
			offset := windowStart + int64(match[0])
			table := ensureTable(tables, name)
			if table.CreateOffset < 0 {
				table.CreateOffset = offset
			}
		}
	}
	if containsFold(scan, "definer") {
		for _, match := range definerRe.FindAllIndex(scan, -1) {
			if matchStartsBeforeLimit(match, processLimit) {
				summary.DefinerCount++
			}
		}
	}
	if containsFold(scan, "char") { // CHARSET or CHARACTER SET
		for _, match := range charsetRe.FindAllSubmatchIndex(scan, -1) {
			if !matchStartsBeforeLimit(match, processLimit) {
				continue
			}
			name := normalizeStatsName(window[match[2]:match[3]])
			if name != "" {
				summary.Charsets[name]++
			}
		}
	}
	if containsFold(scan, "collate") {
		for _, match := range collationRe.FindAllSubmatchIndex(scan, -1) {
			if !matchStartsBeforeLimit(match, processLimit) {
				continue
			}
			name := normalizeStatsName(window[match[2]:match[3]])
			if name != "" {
				summary.Collations[name]++
			}
		}
	}
}

func asciiLower(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + 32
	}
	return c
}

// indexFold returns the index >= from of the first case-insensitive ASCII match
// of word (which must be lowercase) in b, or -1.
func indexFold(b []byte, word string, from int) int {
	m := len(word)
	if m == 0 {
		return from
	}
	c0 := word[0]
	last := len(b) - m
	for i := from; i <= last; i++ {
		if asciiLower(b[i]) != c0 {
			continue
		}
		j := 1
		for ; j < m; j++ {
			if asciiLower(b[i+j]) != word[j] {
				break
			}
		}
		if j == m {
			return i
		}
	}
	return -1
}

func containsFold(b []byte, word string) bool { return indexFold(b, word, 0) >= 0 }

func matchFold(b []byte, pos int, word string) bool {
	if pos+len(word) > len(b) {
		return false
	}
	for j := 0; j < len(word); j++ {
		if asciiLower(b[pos+j]) != word[j] {
			return false
		}
	}
	return true
}

func skipSQLSpace(b []byte, pos int) int {
	for pos < len(b) {
		switch b[pos] {
		case ' ', '\t', '\r', '\n', '\f', '\v':
			pos++
		default:
			return pos
		}
	}
	return pos
}

func isSQLIdentByte(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
		c == '_' || c == '.' || c == '$' || c == '-'
}

// parseInsertInto parses "INSERT\s+INTO\s+`?name`?" starting at b[k] (where b[k:]
// already folds to "insert"). It returns the name byte range [ns,ne), the end
// position, and whether it matched — equivalent to insertIntoRe.
func parseInsertInto(b []byte, k int) (ns, ne, end int, ok bool) {
	p := k + len("insert")
	q := skipSQLSpace(b, p)
	if q == p {
		return 0, 0, 0, false
	}
	p = q
	if !matchFold(b, p, "into") {
		return 0, 0, 0, false
	}
	p += len("into")
	q = skipSQLSpace(b, p)
	if q == p {
		return 0, 0, 0, false
	}
	p = q
	if p < len(b) && b[p] == '`' {
		p++
	}
	ns = p
	for p < len(b) && isSQLIdentByte(b[p]) {
		p++
	}
	ne = p
	if ne == ns {
		return 0, 0, 0, false
	}
	if p < len(b) && b[p] == '`' {
		p++
	}
	return ns, ne, p, true
}

func matchStartsBeforeLimit(match []int, processLimit int) bool {
	return len(match) >= 2 && match[0] >= 0 && match[0] < processLimit
}

func maskSQLLiteralsAndComments(in []byte) []byte {
	out := append([]byte(nil), in...)
	for i := 0; i < len(out); {
		switch out[i] {
		case '\'':
			i = maskSQLQuoted(in, out, i, '\'')
		case '"':
			i = maskSQLQuoted(in, out, i, '"')
		case '#':
			i = maskSQLLineComment(out, i)
		case '-':
			if i+1 < len(out) && out[i+1] == '-' && (i+2 >= len(out) || out[i+2] == ' ' || out[i+2] == '\t' || out[i+2] == '\r' || out[i+2] == '\n') {
				i = maskSQLLineComment(out, i)
				continue
			}
			i++
		case '/':
			if i+1 < len(out) && out[i+1] == '*' {
				i = maskSQLBlockComment(in, out, i)
				continue
			}
			i++
		default:
			i++
		}
	}
	return out
}

func maskSQLQuoted(in []byte, out []byte, start int, quote byte) int {
	out[start] = ' '
	for i := start + 1; i < len(out); i++ {
		if in[i] != '\n' && in[i] != '\r' {
			out[i] = ' '
		}
		if in[i] == quote {
			if isEscapedSQLQuote(in, i) {
				continue
			}
			if i+1 < len(out) && in[i+1] == quote {
				out[i+1] = ' '
				i++
				continue
			}
			return i + 1
		}
	}
	return len(out)
}

func maskSQLLineComment(out []byte, start int) int {
	for i := start; i < len(out); i++ {
		if out[i] == '\n' || out[i] == '\r' {
			return i
		}
		out[i] = ' '
	}
	return len(out)
}

func maskSQLBlockComment(in []byte, out []byte, start int) int {
	for i := start; i < len(out); i++ {
		end := i > start && in[i-1] == '*' && in[i] == '/'
		if in[i] != '\n' && in[i] != '\r' {
			out[i] = ' '
		}
		if end {
			return i + 1
		}
	}
	return len(out)
}

func isEscapedSQLQuote(in []byte, index int) bool {
	slashes := 0
	for i := index - 1; i >= 0 && in[i] == '\\'; i-- {
		slashes++
	}
	return slashes%2 == 1
}

func ensureTable(tables map[string]*Table, name string) *Table {
	table, ok := tables[name]
	if ok {
		return table
	}
	table = &Table{Name: name, CreateOffset: -1, InsertOffset: -1}
	tables[name] = table
	return table
}

func normalizeIdentifier(value []byte) string {
	trimmed := bytes.TrimSpace(value)
	trimmed = bytes.Trim(trimmed, "`")
	return strings.TrimSpace(string(trimmed))
}

func normalizeStatsName(value []byte) string {
	return strings.ToLower(normalizeIdentifier(value))
}

func tablePrimaryOffset(table Table) int64 {
	switch {
	case table.CreateOffset >= 0:
		return table.CreateOffset
	case table.InsertOffset >= 0:
		return table.InsertOffset
	default:
		return 1<<62 - 1
	}
}
