package reshape

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/quarry/quarry-wails3/internal/document"
	sqldelimiter "github.com/quarry/quarry-wails3/internal/plugins/sql/delimiter"
	"github.com/quarry/quarry-wails3/internal/sourceio"
)

func run(t *testing.T, in string, opts Options) (string, Summary) {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "in.sql")
	dst := filepath.Join(dir, "out.sql")
	if err := os.WriteFile(src, []byte(in), 0o666); err != nil {
		t.Fatal(err)
	}
	sum, err := ReshapeInsertsFile(context.Background(), src, dst, opts)
	if err != nil {
		t.Fatalf("reshape: %v", err)
	}
	b, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	return string(b), sum
}

func runFailsWithoutOutput(t *testing.T, in string, opts Options) error {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "in.sql")
	dst := filepath.Join(dir, "out.sql")
	if err := os.WriteFile(src, []byte(in), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := ReshapeInsertsFile(context.Background(), src, dst, opts)
	if err == nil {
		t.Fatal("reshape unexpectedly succeeded")
	}
	if _, statErr := os.Stat(dst); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("failed reshape published output: %v", statErr)
	}
	temps, globErr := filepath.Glob(filepath.Join(dir, ".out.sql.quarry-*"))
	if globErr != nil {
		t.Fatal(globErr)
	}
	if len(temps) != 0 {
		t.Fatalf("failed reshape left temporary outputs: %v", temps)
	}
	if got, readErr := os.ReadFile(src); readErr != nil || string(got) != in {
		t.Fatalf("failed reshape changed source: got %q, err %v", got, readErr)
	}
	return err
}

func TestReshapeRejectsSourcePathReplacementDuringStreaming(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "source.sql")
	held := filepath.Join(dir, "streamed-generation.sql")
	dst := filepath.Join(dir, "output.sql")
	original := []byte("INSERT INTO t VALUES (1),(2);\nINSERT INTO t VALUES (3),(4);\n")
	substitute := []byte("INSERT INTO t VALUES (99);\n")
	if err := os.WriteFile(src, original, 0o600); err != nil {
		t.Fatal(err)
	}
	originalInfo, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := document.OpenFile(src)
	if err != nil {
		t.Fatal(err)
	}
	defer doc.Close()
	expected, err := sourceio.ExpectDocument(doc)
	if err != nil {
		t.Fatal(err)
	}
	mutated := false
	pathReplaced := false
	var mutationErr error
	_, err = ReshapeInsertsFile(context.Background(), src, dst, Options{
		Mode: ModeSingleRow, ExpectedSource: expected,
		Progress: func(Summary) {
			if mutated {
				return
			}
			mutated = true
			if renameErr := os.Rename(src, held); renameErr != nil {
				changed := originalInfo.ModTime().Add(2 * time.Second)
				mutationErr = os.Chtimes(src, changed, changed)
				return
			}
			pathReplaced = true
			mutationErr = os.WriteFile(src, substitute, 0o600)
		},
	})
	if mutationErr != nil {
		t.Fatalf("replace source during reshape: %v", mutationErr)
	}
	if !mutated {
		t.Fatal("reshape did not reach the streaming mutation point")
	}
	if !errors.Is(err, sourceio.ErrSourceChanged) {
		t.Fatalf("error = %v, want source-generation rejection", err)
	}
	if _, statErr := os.Stat(dst); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("failed reshape published output: %v", statErr)
	}
	if pathReplaced {
		if got, readErr := os.ReadFile(held); readErr != nil || string(got) != string(original) {
			t.Fatalf("streamed generation changed: got %q, err %v", got, readErr)
		}
		if got, readErr := os.ReadFile(src); readErr != nil || string(got) != string(substitute) {
			t.Fatalf("replacement path changed: got %q, err %v", got, readErr)
		}
	} else if got, readErr := os.ReadFile(src); readErr != nil || string(got) != string(original) {
		t.Fatalf("metadata-mutated source content changed: got %q, err %v", got, readErr)
	}
}

func TestExplodeExtendedInsert(t *testing.T) {
	in := "INSERT INTO `t` (`a`,`b`) VALUES (1,'x'),(2,'y'),(3,'z');\n"
	out, sum := run(t, in, Options{Mode: ModeSingleRow})
	lines := nonEmptyLines(out)
	if len(lines) != 3 {
		t.Fatalf("want 3 single-row inserts, got %d:\n%s", len(lines), out)
	}
	for i, l := range lines {
		if !strings.HasPrefix(l, "INSERT INTO `t` (`a`,`b`) VALUES (") {
			t.Fatalf("line %d wrong prefix: %q", i, l)
		}
		if !strings.HasSuffix(l, ");") {
			t.Fatalf("line %d not terminated: %q", i, l)
		}
	}
	if sum.RowsSeen != 3 || sum.StatementsWritten != 3 || sum.InsertsRewritten != 1 {
		t.Fatalf("summary off: %+v", sum)
	}
}

func TestExplodePreservesUTF8QualifiedIdentifierBytes(t *testing.T) {
	prefix := "INSERT INTO \u0431\u0430\u0437\u0430.\u043f\u043e\u043b\u044c\u0437\u043e\u0432\u0430\u0442\u0435\u043b\u0438 (`\u0437\u043d\u0430\u0447\u0435\u043d\u0438\u0435`) VALUES "
	first := "(1,'caf\u00e9')"
	second := "(2,'\u03bb')"
	out, summary := run(t, prefix+first+","+second+";\n", Options{Mode: ModeSingleRow})
	if strings.Count(out, prefix) != 2 || !strings.Contains(out, prefix+first+";") || !strings.Contains(out, prefix+second+";") {
		t.Fatalf("UTF-8 identifier or tuple bytes changed:\n%s", out)
	}
	if summary.RowsSeen != 2 || summary.InsertsRewritten != 1 {
		t.Fatalf("summary = %+v", summary)
	}
}

func TestExplodeRespectsQuotedParens(t *testing.T) {
	// A value containing "),(" must not be split as a tuple boundary.
	in := "INSERT INTO `t` VALUES ('a),(b'),('c');\n"
	out, _ := run(t, in, Options{Mode: ModeSingleRow})
	lines := nonEmptyLines(out)
	if len(lines) != 2 {
		t.Fatalf("want 2 rows, got %d:\n%s", len(lines), out)
	}
	if !strings.Contains(lines[0], "'a),(b'") {
		t.Fatalf("quoted parens were split: %q", lines[0])
	}
}

func TestExplodePreservesNestedTupleExpressions(t *testing.T) {
	in := "INSERT INTO `t` VALUES (COALESCE((SELECT 1),2),'a),(b'),(3,'c');\n"
	out, sum := run(t, in, Options{Mode: ModeSingleRow})
	if strings.Count(out, "INSERT INTO `t` VALUES (") != 2 {
		t.Fatalf("nested tuples were not preserved:\n%s", out)
	}
	if !strings.Contains(out, "COALESCE((SELECT 1),2)") || sum.RowsSeen != 2 {
		t.Fatalf("nested tuple expression changed: summary=%+v\n%s", sum, out)
	}
}

func TestTrailingClausesAndMalformedTupleListsFailWithoutOutput(t *testing.T) {
	tests := []struct {
		name string
		sql  string
	}{
		{name: "on duplicate with function", sql: "INSERT INTO t VALUES (1) ON DUPLICATE KEY UPDATE n=func(2);"},
		{name: "on duplicate plain", sql: "INSERT INTO t VALUES (1) ON DUPLICATE KEY UPDATE n=2;"},
		{name: "unsupported after safe statement", sql: "INSERT INTO t VALUES (1),(2);\nINSERT INTO t VALUES (3) ON DUPLICATE KEY UPDATE n=4;"},
		{name: "returning", sql: "INSERT INTO t VALUES (1) RETURNING id;"},
		{name: "row alias", sql: "INSERT INTO t VALUES (1) AS new;"},
		{name: "missing comma", sql: "INSERT INTO t VALUES (1) (2);"},
		{name: "missing tuple after comma", sql: "INSERT INTO t VALUES (1),;"},
		{name: "trailing comment", sql: "INSERT INTO t VALUES (1) /* keep */;"},
		{name: "missing terminator", sql: "INSERT INTO t VALUES (1)"},
		{name: "insert select", sql: "INSERT INTO t SELECT 1;"},
		{name: "executable leading comment", sql: "/*!99999 SET @x=1 */ INSERT INTO t VALUES (1),(2);"},
		{name: "optimizer prefix hint", sql: "INSERT /*+ SET_VAR(foreign_key_checks=0) */ INTO t VALUES (1),(2);"},
		{name: "executable body comment", sql: "INSERT INTO t VALUES (1,/*!99999 2 */),(3,4);"},
		{name: "optimizer body comment", sql: "INSERT INTO t VALUES (1,/*+ BKA(t) */2),(3,4);"},
		{name: "MariaDB executable comment", sql: "INSERT /*M! LOW_PRIORITY */ INTO t VALUES (1),(2);"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := runFailsWithoutOutput(t, tt.sql, Options{Mode: ModeSingleRow})
			if !errors.Is(err, ErrUnsupportedInsert) {
				t.Fatalf("error = %v, want ErrUnsupportedInsert", err)
			}
		})
	}
}

func TestReshapePreservesWordPressSerializedTuplePayloadBytes(t *testing.T) {
	tupleA := `(1,'a:3:{s:4:"semi";s:4:"a;b;";s:5:"shape";s:3:"),(";s:5:"quote";s:8:"O\'Reilly";}')`
	tupleB := `(2,'O:8:"stdClass":2:{s:5:"value";s:5:"x),(;";s:5:"slash";s:3:"a\\b";}')`
	prefix := "INSERT INTO `wp_options` (`option_id`,`option_value`) VALUES "

	t.Run("explode", func(t *testing.T) {
		input := prefix + tupleA + "," + tupleB + ";\n"
		output, summary := run(t, input, Options{Mode: ModeSingleRow})
		want := prefix + tupleA + ";\n" + prefix + tupleB + ";\n\n"
		if output != want {
			t.Fatalf("serialized tuple bytes changed\n got: %q\nwant: %q", output, want)
		}
		if summary.RowsSeen != 2 || summary.InsertsRewritten != 1 {
			t.Fatalf("summary = %+v", summary)
		}
	})

	t.Run("batch", func(t *testing.T) {
		input := prefix + tupleA + ";\n" + prefix + tupleB + ";\n"
		output, summary := run(t, input, Options{Mode: ModeMultiRow, BatchSize: 10})
		wantPrefix := prefix + tupleA + "," + tupleB + ";\n"
		if !strings.HasPrefix(output, wantPrefix) || strings.TrimPrefix(output, wantPrefix) != "\n" {
			t.Fatalf("serialized tuple bytes changed\n got: %q\nwant prefix: %q", output, wantPrefix)
		}
		if summary.RowsSeen != 2 || summary.InsertsRewritten != 1 {
			t.Fatalf("summary = %+v", summary)
		}
	})
}

func TestUnsupportedStructuredSQLFailsAfterPriorValidInsertWithoutPublication(t *testing.T) {
	tests := []struct {
		name string
		sql  string
		want error
	}{
		{
			name: "COPY FROM STDIN payload containing insert",
			sql:  "COPY imported(value) FROM STDIN;\nINSERT INTO payload VALUES (1),(2);\n\\.\n",
			want: ErrUnsupportedCompoundStatement,
		},
		{
			name: "PostgreSQL dollar quoted function",
			sql:  "CREATE OR REPLACE FUNCTION f() RETURNS void AS $body$ BEGIN; INSERT INTO audit VALUES (1),(2); END; $body$ LANGUAGE plpgsql;",
			want: ErrUnsupportedCompoundStatement,
		},
		{
			name: "anonymous dollar quoted block",
			sql:  "DO $$ BEGIN; INSERT INTO audit VALUES (1),(2); END; $$;",
			want: ErrUnsupportedCompoundStatement,
		},
		{
			name: "Unicode dollar quoted block",
			sql:  "DO $тег$ BEGIN; INSERT INTO audit VALUES (1),(2); END; $тег$;",
			want: ErrUnsupportedCompoundStatement,
		},
		{
			name: "compound trigger",
			sql:  "CREATE TRIGGER trg AFTER INSERT ON src BEGIN INSERT INTO audit VALUES (1),(2); END;",
			want: ErrUnsupportedCompoundStatement,
		},
		{
			name: "SQL Server CREATE PROC alias",
			sql:  "CREATE PROC p AS BEGIN SET NOCOUNT ON; INSERT INTO audit VALUES (1),(2); END;",
			want: ErrUnsupportedCompoundStatement,
		},
		{
			name: "HANA DO block",
			sql:  "DO BEGIN DECLARE n INTEGER; INSERT INTO audit VALUES (1),(2); END;",
			want: ErrUnsupportedCompoundStatement,
		},
		{
			name: "Firebird execute block",
			sql:  "EXECUTE BLOCK AS BEGIN POST_EVENT 'x'; INSERT INTO audit VALUES (1),(2); END;",
			want: ErrUnsupportedCompoundStatement,
		},
		{
			name: "Firebird recreate procedure",
			sql:  "RECREATE PROCEDURE p AS BEGIN INSERT INTO audit VALUES (1),(2); END;",
			want: ErrUnsupportedCompoundStatement,
		},
		{
			name: "Teradata replace procedure",
			sql:  "REPLACE PROCEDURE p() BEGIN INSERT INTO audit VALUES (1),(2); END;",
			want: ErrUnsupportedCompoundStatement,
		},
		{
			name: "compound macro",
			sql:  "CREATE MACRO m AS (INSERT INTO audit VALUES (1),(2););",
			want: ErrUnsupportedCompoundStatement,
		},
		{
			name: "Snowflake task scripting block",
			sql: "CREATE TASK scheduled_load SCHEDULE='USING CRON 0 * * * * UTC' AS BEGIN SELECT 1; " +
				"INSERT INTO audit VALUES (1),(2); END;",
			want: ErrUnsupportedCompoundStatement,
		},
		{
			name: "MySQL routine with unquoted definer account",
			sql: "CREATE DEFINER=user@localhost PROCEDURE p() BEGIN SELECT 1; " +
				"INSERT INTO audit VALUES (1),(2); END;",
			want: ErrUnsupportedCompoundStatement,
		},
		{
			name: "alter procedure",
			sql:  "ALTER PROCEDURE p AS BEGIN INSERT INTO audit VALUES (1),(2); END;",
			want: ErrUnsupportedCompoundStatement,
		},
		{
			name: "create event",
			sql:  "CREATE EVENT ev DO BEGIN INSERT INTO audit VALUES (1),(2); END;",
			want: ErrUnsupportedCompoundStatement,
		},
		{
			name: "create rule",
			sql:  "CREATE RULE r AS ON INSERT TO src DO ALSO INSERT INTO audit VALUES (1),(2);",
			want: ErrUnsupportedCompoundStatement,
		},
		{
			name: "create package",
			sql:  "CREATE PACKAGE p AS PROCEDURE run; END p;",
			want: ErrUnsupportedCompoundStatement,
		},
		{
			name: "create module",
			sql:  "CREATE MODULE m LANGUAGE SQL BEGIN INSERT INTO audit VALUES (1),(2); END;",
			want: ErrUnsupportedCompoundStatement,
		},
		{
			name: "Oracle type body",
			sql:  "CREATE TYPE BODY widget AS MEMBER PROCEDURE p IS BEGIN INSERT INTO audit VALUES (1),(2); END; END;",
			want: ErrUnsupportedCompoundStatement,
		},
		{
			name: "anonymous begin",
			sql:  "BEGIN INSERT INTO audit VALUES (1),(2); END;",
			want: ErrUnsupportedCompoundStatement,
		},
		{
			name: "anonymous declare",
			sql:  "DECLARE n INTEGER := 1; BEGIN INSERT INTO audit VALUES (1),(2); END;",
			want: ErrUnsupportedCompoundStatement,
		},
		{
			name: "bracket identifier",
			sql:  "CREATE TABLE [name; INSERT INTO phantom VALUES (1),(2);] (id int);",
			want: ErrUnsupportedLexicalConstruct,
		},
		{
			name: "Oracle q quote",
			sql:  "INSERT INTO retained VALUES (q'[text; INSERT INTO phantom VALUES (1),(2);]');",
			want: ErrUnsupportedLexicalConstruct,
		},
		{
			name: "nested block comment",
			sql:  "/* outer /* inner */ text; INSERT INTO phantom VALUES (1),(2); */",
			want: ErrUnsupportedLexicalConstruct,
		},
		{
			name: "overlapping unterminated block comment",
			sql:  "/*/ ; INSERT INTO phantom VALUES (1),(2);",
			want: ErrUnsupportedLexicalConstruct,
		},
		{
			name: "unterminated string",
			sql:  "INSERT INTO retained VALUES ('unterminated; INSERT INTO phantom VALUES (1),(2);",
			want: ErrUnsupportedLexicalConstruct,
		},
		{
			name: "dash comment without whitespace",
			sql:  "--comment; INSERT INTO phantom VALUES (1),(2);",
			want: ErrUnsupportedLexicalConstruct,
		},
		{
			name: "inline hash operator",
			sql:  "SELECT payload #> path; INSERT INTO phantom VALUES (1),(2);",
			want: ErrUnsupportedLexicalConstruct,
		},
		{
			name: "SQL Server GO batch",
			sql:  "SET NOCOUNT ON\nGO\nCREATE TRIGGER trg ON src AFTER INSERT AS INSERT INTO audit VALUES (1),(2);",
			want: ErrUnsupportedCompoundStatement,
		},
		{
			name: "SQL Server GO batch with comment",
			sql:  "SET NOCOUNT ON\nGO -- next batch\nCREATE TRIGGER trg ON src AFTER INSERT AS INSERT INTO audit VALUES (1),(2);",
			want: ErrUnsupportedCompoundStatement,
		},
		{
			name: "SQL Server GO count with comment",
			sql:  "SET NOCOUNT ON\nGO 2 -- repeat\nCREATE TRIGGER trg ON src AFTER INSERT AS INSERT INTO audit VALUES (1),(2);",
			want: ErrUnsupportedCompoundStatement,
		},
		{
			name: "SQL Plus slash batch",
			sql:  "SET DEFINE OFF\n/\nCREATE TRIGGER trg AFTER INSERT ON src BEGIN INSERT INTO audit VALUES (1),(2); END;",
			want: ErrUnsupportedCompoundStatement,
		},
		{
			name: "Firebird SET TERM",
			sql:  "SET TERM ^ ;\nEXECUTE BLOCK AS BEGIN INSERT INTO audit VALUES (1),(2); END^\nSET TERM ; ^",
			want: ErrUnsupportedDelimiter,
		},
		{
			name: "SQL Plus PROMPT text",
			sql:  "PROMPT heading; INSERT INTO phantom VALUES (8),(9);\nBEGIN NULL; INSERT INTO audit VALUES (1),(2); END;",
			want: ErrUnsupportedCompoundStatement,
		},
		{
			name: "SQL Plus REM text",
			sql:  "REM heading; INSERT INTO phantom VALUES (8),(9);\nBEGIN NULL; INSERT INTO audit VALUES (1),(2); END;",
			want: ErrUnsupportedCompoundStatement,
		},
		{
			name: "SQL Plus REMARK text",
			sql:  "REMARK heading; INSERT INTO phantom VALUES (8),(9);\nBEGIN NULL; INSERT INTO audit VALUES (1),(2); END;",
			want: ErrUnsupportedCompoundStatement,
		},
		{
			name: "unrecognized newline client command",
			sql:  "SPOOL output.log\nINSERT INTO audit VALUES (1),(2);",
			want: ErrUnsupportedCompoundStatement,
		},
		{
			name: "SQL Plus SPOOL same-line semicolon payload",
			sql:  "SPOOL output.log; INSERT INTO audit VALUES (1),(2);",
			want: ErrUnsupportedCompoundStatement,
		},
		{
			name: "mysql SOURCE same-line semicolon payload",
			sql:  "SOURCE nested.sql; INSERT INTO audit VALUES (1),(2);",
			want: ErrUnsupportedCompoundStatement,
		},
		{
			name: "SQL Plus at-script same-line semicolon payload",
			sql:  "@@nested.sql; INSERT INTO audit VALUES (1),(2);",
			want: ErrUnsupportedCompoundStatement,
		},
		{
			name: "mysql help shorthand same-line semicolon payload",
			sql:  "? contents; INSERT INTO audit VALUES (1),(2);",
			want: ErrUnsupportedCompoundStatement,
		},
		{
			name: "unknown client same-line semicolon payload",
			sql:  "CLIENTX options; INSERT INTO audit VALUES (1),(2);",
			want: ErrUnsupportedCompoundStatement,
		},
		{
			name: "leading comment before unknown client same-line payload",
			sql:  "/**/ CLIENTX options; INSERT INTO audit VALUES (1),(2);",
			want: ErrUnsupportedCompoundStatement,
		},
		{
			name: "unknown client ambiguity stays sticky on physical line",
			sql:  "CLIENTX options; SELECT 1; INSERT INTO audit VALUES (1),(2);",
			want: ErrUnsupportedCompoundStatement,
		},
		{
			name: "ambiguous SET client or server same-line payload",
			sql:  "SET NAMES utf8mb4; INSERT INTO audit VALUES (1),(2);",
			want: ErrUnsupportedCompoundStatement,
		},
		{
			name: "SQL Plus SHUTDOWN same-line payload",
			sql:  "SHUTDOWN; INSERT INTO audit VALUES (1),(2);",
			want: ErrUnsupportedCompoundStatement,
		},
		{
			name: "client file and shell commands",
			sql: "HOST echo setup; INSERT INTO audit VALUES (1),(2);\n" +
				"SYSTEM echo setup; TEE output.log; PAGER less; INPUT; OUTPUT;",
			want: ErrUnsupportedCompoundStatement,
		},
		{
			name: "unrecognized client command before Snowflake task",
			sql: "SPOOL output.log\nCREATE TASK scheduled_load AS BEGIN SELECT 1; " +
				"INSERT INTO audit VALUES (1),(2); END;",
			want: ErrUnsupportedCompoundStatement,
		},
		{
			name: "psql meta command",
			sql:  "SET client_encoding = 'UTF8'\n\\copy imported FROM STDIN\nINSERT INTO payload VALUES (1),(2);",
			want: ErrUnsupportedCompoundStatement,
		},
		{
			name: "SQLCMD colon command before compound body",
			sql:  ":setvar Database app\nCREATE TRIGGER trg ON src AFTER INSERT AS BEGIN SELECT 1; INSERT INTO phantom VALUES (1),(2); END;",
			want: ErrUnsupportedCompoundStatement,
		},
		{
			name: "SQLite dot command before compound body",
			sql:  ".read setup.sql\nCREATE TRIGGER trg AFTER INSERT ON src BEGIN SELECT 1; INSERT INTO phantom VALUES (1),(2); END;",
			want: ErrUnsupportedCompoundStatement,
		},
		{
			name: "SQLCMD shell command before compound body",
			sql: "!! echo setup\nCREATE TRIGGER trg ON src AFTER INSERT AS BEGIN SELECT 1; " +
				"INSERT INTO phantom VALUES (1),(2); END;",
			want: ErrUnsupportedCompoundStatement,
		},
		{
			name: "SnowSQL command before task body",
			sql: "!set output_format=csv\nCREATE TASK scheduled_load AS BEGIN SELECT 1; " +
				"INSERT INTO phantom VALUES (1),(2); END;",
			want: ErrUnsupportedCompoundStatement,
		},
		{
			name: "Oracle block label",
			sql:  "<<load_rows>>\nBEGIN INSERT INTO audit VALUES (1),(2); END;",
			want: ErrUnsupportedLexicalConstruct,
		},
		{
			name: "non-additive INSERT OVERWRITE",
			sql:  "INSERT OVERWRITE TABLE retained VALUES (1),(2);",
			want: ErrUnsupportedInsert,
		},
	}

	for _, mode := range []Mode{ModeSingleRow, ModeMultiRow} {
		for _, test := range tests {
			t.Run(string(mode)+"/"+test.name, func(t *testing.T) {
				input := "INSERT INTO safe VALUES (0);\n" + test.sql
				err := runFailsWithoutOutput(t, input, Options{Mode: mode, BatchSize: 10})
				if !errors.Is(err, test.want) {
					t.Fatalf("error = %v, want %v", err, test.want)
				}
			})
		}
	}
}

func TestReshapeSafetyGuardBytewiseSeamsAndSupportedSyntax(t *testing.T) {
	supported := "# line-leading MySQL comment\n" +
		"CREATE TYPE mood AS ENUM ('sad', 'ok');\n" +
		"ALTER TYPE mood ADD VALUE 'great';\n" +
		"BEGIN;\n" +
		"INSERT INTO foo$bar$baz VALUES ('it''s; ),('),(2);\n" +
		"COMMIT;\n" +
		"BEGIN TRANSACTION;\n" +
		"INSERT INTO \"quo\"\"ted\" VALUES (3),(4);\n" +
		"COMMIT;\n" +
		"/**/ INSERT INTO `tick``name` VALUES (5),(6);\n"
	supported += "INSERT INTO decimals VALUES (\n.5\n),(1);\n"
	supported += "SELECT\n!0;\n"
	supported += "SELECT\nhost, system, input, output, source, spool, @variable, ?\nFROM retained;\n"
	supported += "SELECT fooé$tag$ FROM t;\n"
	supported += "INSERT INTO strings VALUES ('\nGO\nPROMPT text\nSPOOL file\nSOURCE file\nHOST command\nSYSTEM command\nTEE file\nPAGER less\nINPUT\nOUTPUT\n@file\n@@file\n/\n\\copy data\n:setvar x y\n.read file\n!! echo payload\n!set payload=true\n'),(2);\n"
	supported += "/*\nGO\nPROMPT text\nSPOOL file\nSOURCE file\nHOST command\nSYSTEM command\nTEE file\nPAGER less\nINPUT\nOUTPUT\n@file\n@@file\n/\n\\copy data\n:setvar x y\n.read file\n!! echo payload\n!set payload=true\n*/ SELECT 1;\n"
	var guard reshapeSafetyGuard
	for index := range supported {
		if err := guard.Step(supported[index]); err != nil {
			t.Fatalf("byte %d: %v", index, err)
		}
	}
	if err := guard.Finish(); err != nil {
		t.Fatal(err)
	}

	for _, input := range []string{
		"DO $tag$ BEGIN; INSERT INTO x VALUES (1); END; $tag$;",
		"/* outer /* nested */",
		"INSERT INTO [x; INSERT INTO y VALUES (1);] VALUES (1);",
		"INSERT INTO x VALUES (q'[a; INSERT INTO y VALUES (1);]');",
	} {
		guard = reshapeSafetyGuard{}
		var err error
		for index := range input {
			if err = guard.Step(input[index]); err != nil {
				break
			}
		}
		if err == nil {
			err = guard.Finish()
		}
		if err == nil {
			t.Fatalf("bytewise guard accepted unsafe input %q", input)
		}
	}
}

func TestReshapeSafetyGuardRejectsAmbiguousNewlineStatementStarters(t *testing.T) {
	starters := []string{
		"INSERT", "CREATE", "ALTER", "RECREATE", "REPLACE", "BEGIN", "DECLARE", "DO", "EXECUTE",
		"COPY", "IF", "LOOP", "WHILE", "FOR", "REPEAT",
	}
	for _, starter := range starters {
		t.Run(strings.ToLower(starter), func(t *testing.T) {
			input := "CLIENTX output.log\n" + starter + " next_token;"
			var guard reshapeSafetyGuard
			var err error
			for index := range input {
				if err = guard.Step(input[index]); err != nil {
					break
				}
			}
			if err == nil {
				err = guard.Finish()
			}
			if !errors.Is(err, ErrUnsupportedCompoundStatement) {
				t.Fatalf("error = %v, want ErrUnsupportedCompoundStatement", err)
			}
		})
	}

	// Prefix matches require a complete word, not just a risky-looking byte
	// prefix. Keeping this near miss guards the fixed token overflow/boundary
	// behavior used for arbitrarily long physical lines.
	input := "CLIENTX output.log\nCREATED next_token;"
	var guard reshapeSafetyGuard
	for index := range input {
		if err := guard.Step(input[index]); err != nil {
			t.Fatalf("near miss rejected at byte %d: %v", index, err)
		}
	}
	if err := guard.Finish(); err != nil {
		t.Fatalf("near miss rejected at EOF: %v", err)
	}
}

func TestReshapeSafetyGuardRejectsAmbiguousSameLineStatementStarters(t *testing.T) {
	starters := []string{
		"INSERT", "CREATE", "ALTER", "RECREATE", "REPLACE", "BEGIN", "DECLARE", "DO", "EXECUTE",
		"COPY", "IF", "LOOP", "WHILE", "FOR", "REPEAT",
	}
	for _, starter := range starters {
		t.Run(strings.ToLower(starter), func(t *testing.T) {
			input := strings.Repeat("client_command_name", 4) + " options; SELECT 1; " + starter + " next_token;"
			var guard reshapeSafetyGuard
			var err error
			for index := range input {
				if err = guard.Step(input[index]); err != nil {
					break
				}
			}
			if err == nil {
				err = guard.Finish()
			}
			if !errors.Is(err, ErrUnsupportedCompoundStatement) {
				t.Fatalf("error = %v, want ErrUnsupportedCompoundStatement", err)
			}
		})
	}
}

func TestReshapeSafetyGuardRejectsLineLeadingFileAndShellCommands(t *testing.T) {
	commands := []string{
		"SPOOL output.log", "SOURCE nested.sql", "HOST echo setup", "SYSTEM echo setup", "TEE output.log",
		"PAGER less", "INPUT", "OUTPUT", "@nested.sql", "@@nested.sql", "? contents",
	}
	for _, command := range commands {
		t.Run(strings.Fields(command)[0], func(t *testing.T) {
			input := command + "; INSERT INTO phantom VALUES (1),(2);"
			var guard reshapeSafetyGuard
			var err error
			for index := range input {
				if err = guard.Step(input[index]); err != nil {
					break
				}
			}
			if err == nil {
				err = guard.Finish()
			}
			if !errors.Is(err, ErrUnsupportedCompoundStatement) {
				t.Fatalf("error = %v, want ErrUnsupportedCompoundStatement", err)
			}
		})
	}
}

func TestReshapePreservesCommonMultilineStatementPrefixes(t *testing.T) {
	input := "CREATE\nTABLE retained (id integer);\n" +
		"ALTER\nTABLE retained ADD COLUMN value integer;\n" +
		"BEGIN\nTRANSACTION;\n" +
		"INSERT\nINTO retained VALUES (1),(2);\n" +
		"COMMIT;\n"
	for _, mode := range []Mode{ModeSingleRow, ModeMultiRow} {
		t.Run(string(mode), func(t *testing.T) {
			output, summary := run(t, input, Options{Mode: mode, BatchSize: 10})
			if summary.RowsSeen != 2 || summary.InsertsRewritten != 1 {
				t.Fatalf("summary = %+v", summary)
			}
			if !strings.Contains(output, "CREATE\nTABLE retained") ||
				!strings.Contains(output, "ALTER\nTABLE retained") ||
				!strings.Contains(output, "BEGIN\nTRANSACTION") {
				t.Fatalf("safe multiline statements changed: %q", output)
			}
		})
	}
}

func TestReshapePreservesRecognizedSQLSameLineAndAmbiguousTokensAcrossNewlines(t *testing.T) {
	input := "/**/ SELECT 1; CREATE TABLE retained (id integer); INSERT INTO retained VALUES (1),(2);\n" +
		"SET NAMES utf8mb4;\n" +
		"START TRANSACTION;\n" +
		"USE app;\n" +
		"SHOW TABLES;\n" +
		"INSERT INTO retained VALUES (3),(4);\n" +
		"COMMIT;\n"
	for _, mode := range []Mode{ModeSingleRow, ModeMultiRow} {
		t.Run(string(mode), func(t *testing.T) {
			output, summary := run(t, input, Options{Mode: mode, BatchSize: 10})
			if summary.RowsSeen != 4 || summary.InsertsRewritten != 2 {
				t.Fatalf("summary = %+v", summary)
			}
			for _, preserved := range []string{"SELECT 1;", "CREATE TABLE retained", "SET NAMES utf8mb4;", "START TRANSACTION;", "USE app;", "SHOW TABLES;"} {
				if !strings.Contains(output, preserved) {
					t.Fatalf("missing preserved SQL %q in %q", preserved, output)
				}
			}
		})
	}
}

func TestClassifyStatementPrefixFailsClosedAtBoundedSeams(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  statementPrefixClass
	}{
		{name: "empty", input: "", want: statementPrefixUnknown},
		{name: "whitespace", input: " \r\n", want: statementPrefixUnknown},
		{name: "partial keyword", input: "/* complete */ ins", want: statementPrefixUnknown},
		{name: "unterminated block comment", input: "/* comment", want: statementPrefixUnknown},
		{name: "line comment without newline", input: "-- comment", want: statementPrefixUnknown},
		{name: "hash comment without newline", input: "# comment", want: statementPrefixUnknown},
		{name: "partial dash opener", input: "-", want: statementPrefixUnknown},
		{name: "partial slash opener", input: "/", want: statementPrefixUnknown},
		{name: "partial byte order mark", input: string([]byte{0xef, 0xbb}), want: statementPrefixUnknown},
		{name: "comment then insert", input: "/* complete */ INSERT ", want: statementPrefixInsert},
		{name: "byte order mark then insert", input: string([]byte{0xef, 0xbb, 0xbf}) + "INSERT ", want: statementPrefixInsert},
		{name: "insert exact", input: "INSERT", want: statementPrefixInsert},
		{name: "identifier beginning insert", input: "inserted", want: statementPrefixOther},
		{name: "dollar identifier beginning insert", input: "insert$column", want: statementPrefixOther},
		{name: "unicode identifier beginning insert", input: "insert\u00e9", want: statementPrefixOther},
		{name: "known other statement", input: "/* complete */ CREATE TABLE", want: statementPrefixOther},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := classifyStatementPrefix([]byte(test.input)); got != test.want {
				t.Fatalf("classification = %d, want %d", got, test.want)
			}
		})
	}
}

func TestReshapeHandlesBOMAndLineCommentEndingsWithoutSkippingInserts(t *testing.T) {
	for _, test := range []struct {
		name  string
		input string
	}{
		{
			name:  "UTF-8 BOM",
			input: string([]byte{0xef, 0xbb, 0xbf}) + "INSERT INTO t VALUES (1),(2);\nINSERT INTO t VALUES (3),(4);\n",
		},
		{
			name:  "CR-only comments",
			input: "-- first\rINSERT INTO t VALUES (1),(2);\r# second\rINSERT INTO t VALUES (3),(4);\r",
		},
		{
			name:  "CRLF comments",
			input: "-- first\r\nINSERT INTO t VALUES (1),(2);\r\n# second\r\nINSERT INTO t VALUES (3),(4);\r\n",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			output, summary := run(t, test.input, Options{Mode: ModeSingleRow})
			if summary.InsertsRewritten != 2 || summary.RowsSeen != 4 ||
				strings.Count(output, "INSERT INTO t VALUES (") != 4 {
				t.Fatalf("inserts were skipped: summary=%+v output=%q", summary, output)
			}
		})
	}
}

func TestReshapeAcceptsSupportedKeywordBoundariesAndQuotedCommentMarkers(t *testing.T) {
	input := "CREATE TYPE mood AS ENUM ('sad', 'ok');\n" +
		"ALTER TYPE mood ADD VALUE 'great';\n" +
		"BEGIN TRANSACTION;\n" +
		"INSERT INTO foo$bar$baz VALUES (1,'/*! payload */'),(2,'/*+ payload */'),(3,'/*M! payload */');\n" +
		"COMMIT;\n"
	output, summary := run(t, input, Options{Mode: ModeSingleRow})
	if summary.InsertsRewritten != 1 || summary.RowsSeen != 3 ||
		strings.Count(output, "INSERT INTO foo$bar$baz VALUES (") != 3 {
		t.Fatalf("supported syntax was not reshaped: summary=%+v output=%q", summary, output)
	}
	for _, payload := range []string{"'/*! payload */'", "'/*+ payload */'", "'/*M! payload */'"} {
		if strings.Count(output, payload) != 1 {
			t.Fatalf("quoted payload %q changed in %q", payload, output)
		}
	}
}

func TestInvalidModeAndOversizedBatchFailBeforeOutput(t *testing.T) {
	if err := runFailsWithoutOutput(t, "INSERT INTO t VALUES (1);", Options{Mode: Mode("future")}); err == nil {
		t.Fatal("invalid mode unexpectedly succeeded")
	}
	if err := runFailsWithoutOutput(t, "INSERT INTO t VALUES (1);", Options{Mode: ModeMultiRow, BatchSize: MaxBatchRows + 1}); err == nil {
		t.Fatal("oversized batch unexpectedly succeeded")
	}
}

func TestCustomDelimiterRoutineFailsWithoutOutput(t *testing.T) {
	sql := "DELIMITER $$\nCREATE PROCEDURE p()\nBEGIN\n  INSERT INTO t VALUES (1);\nEND$$\nDELIMITER ;\n"
	err := runFailsWithoutOutput(t, sql, Options{Mode: ModeSingleRow})
	if !errors.Is(err, ErrUnsupportedDelimiter) {
		t.Fatalf("error = %v, want ErrUnsupportedDelimiter", err)
	}
}

func TestDelimiterDirectiveDetectorStreaming(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		chunks []int
		want   bool
	}{
		{
			name:   "arbitrary indentation and keyword seams",
			input:  strings.Repeat(" \t", 80) + "DeLiMiTeR $$\r\n",
			chunks: []int{31, 1, 7, 2, 64, 3, 1},
			want:   true,
		},
		{
			name:   "BOM and directive split bytewise",
			input:  "\xef\xbb\xbf\tDELIMITER //\r\n",
			chunks: []int{1},
			want:   true,
		},
		{
			name:   "CRLF resets rejected line",
			input:  "SELECT 'not a directive';\r\n\tDELIMITER ;;\r\n",
			chunks: []int{28, 1, 1, 4},
			want:   true,
		},
		{
			name:   "exact keyword at EOF",
			input:  "delimiter",
			chunks: []int{3, 2},
			want:   true,
		},
		{
			name:   "identifier is not directive",
			input:  "delimiter_value = 1;\n",
			chunks: []int{9, 1},
			want:   false,
		},
		{
			name:   "keyword away from line prefix",
			input:  "SELECT delimiter FROM settings;\n",
			chunks: []int{1},
			want:   false,
		},
		{
			name:   "BOM only recognized at BOF",
			input:  "\r\n\xef\xbb\xbfDELIMITER $$\n",
			chunks: []int{2, 1, 1, 1, 9},
			want:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var detector sqldelimiter.Detector
			matched := false
			at := 0
			chunkAt := 0
			for at < len(tt.input) && !matched {
				size := len(tt.input) - at
				if len(tt.chunks) > 0 {
					size = tt.chunks[chunkAt%len(tt.chunks)]
					chunkAt++
					if size > len(tt.input)-at {
						size = len(tt.input) - at
					}
				}
				for _, c := range []byte(tt.input[at : at+size]) {
					if detector.Step(c) {
						matched = true
						break
					}
				}
				at += size
			}
			matched = matched || detector.Finish()
			if matched != tt.want {
				t.Fatalf("matched = %v, want %v", matched, tt.want)
			}
		})
	}
}

func TestIndentedBOMCustomDelimiterRoutineFailsWithoutPublication(t *testing.T) {
	tests := []struct {
		name   string
		prefix string
		eol    string
	}{
		{name: "more than probe width", prefix: strings.Repeat(" ", 96), eol: "\n"},
		{name: "BOM tabs and CRLF", prefix: "\xef\xbb\xbf" + strings.Repeat("\t", 40), eol: "\r\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sql := tt.prefix + "DELIMITER $$" + tt.eol +
				"CREATE PROCEDURE p()" + tt.eol +
				"BEGIN" + tt.eol +
				"  INSERT INTO routine_log VALUES ('internal semicolon');" + tt.eol +
				"END$$" + tt.eol +
				"DELIMITER ;" + tt.eol +
				"INSERT INTO exported_rows VALUES (1),(2);" + tt.eol
			err := runFailsWithoutOutput(t, sql, Options{Mode: ModeSingleRow})
			if !errors.Is(err, ErrUnsupportedDelimiter) {
				t.Fatalf("error = %v, want ErrUnsupportedDelimiter", err)
			}
		})
	}
}

func TestExistingDestinationIsPreserved(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "in.sql")
	dst := filepath.Join(dir, "out.sql")
	if err := os.WriteFile(src, []byte("INSERT INTO t VALUES (1),(2);"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("sentinel"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReshapeInsertsFile(context.Background(), src, dst, Options{Mode: ModeSingleRow}); err == nil {
		t.Fatal("existing destination was accepted")
	}
	if got, err := os.ReadFile(dst); err != nil || string(got) != "sentinel" {
		t.Fatalf("destination = %q, err %v", got, err)
	}
}

func TestBatchSingleRowInserts(t *testing.T) {
	in := "INSERT INTO `t` VALUES (1);\nINSERT INTO `t` VALUES (2);\nINSERT INTO `t` VALUES (3);\n"
	out, sum := run(t, in, Options{Mode: ModeMultiRow, BatchSize: 2})
	lines := nonEmptyLines(out)
	// batch size 2 → (1),(2) then (3)
	if len(lines) != 2 {
		t.Fatalf("want 2 batched inserts, got %d:\n%s", len(lines), out)
	}
	if !strings.Contains(lines[0], "VALUES (1),(2)") {
		t.Fatalf("first batch wrong: %q", lines[0])
	}
	if !strings.Contains(lines[1], "VALUES (3)") {
		t.Fatalf("second batch wrong: %q", lines[1])
	}
	if sum.RowsSeen != 3 {
		t.Fatalf("rows seen = %d, want 3", sum.RowsSeen)
	}
}

func TestNonInsertCopiedVerbatim(t *testing.T) {
	in := "CREATE TABLE `t` (`a` int);\n/*!40000 SET x=1 */;\nINSERT INTO `t` VALUES (1),(2);\n"
	out, _ := run(t, in, Options{Mode: ModeSingleRow})
	if !strings.Contains(out, "CREATE TABLE `t` (`a` int);") {
		t.Fatalf("CREATE not preserved:\n%s", out)
	}
	if !strings.Contains(out, "/*!40000 SET x=1 */;") {
		t.Fatalf("conditional comment not preserved:\n%s", out)
	}
	if strings.Count(out, "INSERT INTO `t` VALUES (") != 2 {
		t.Fatalf("insert not exploded:\n%s", out)
	}
}

func TestExplodeAfterLeadingComment(t *testing.T) {
	// mysqldump prefixes each table's data with a comment block + blank line.
	in := "--\n-- Dumping data for table `users`\n--\n\nINSERT INTO `users` VALUES (1,'a'),(2,'b'),(3,'c');\n"
	out, sum := run(t, in, Options{Mode: ModeSingleRow})
	if !strings.HasPrefix(out, "--\n-- Dumping data for table `users`\n--\n\n") {
		t.Fatalf("leading provenance comments were not preserved:\n%s", out)
	}
	if sum.InsertsRewritten != 1 || sum.RowsSeen != 3 {
		t.Fatalf("comment-prefixed INSERT not reshaped: %+v", sum)
	}
	if strings.Count(out, "INSERT INTO `users` VALUES (") != 3 {
		t.Fatalf("want 3 reshaped rows:\n%s", out)
	}
}

func TestHashAndBlockCommentBeforeInsert(t *testing.T) {
	for _, in := range []string{
		"# a hash comment\nINSERT INTO `t` VALUES (1),(2);\n",
		"/* block\n comment */ INSERT INTO `t` VALUES (1),(2);\n",
	} {
		out, sum := run(t, in, Options{Mode: ModeSingleRow})
		if sum.InsertsRewritten != 1 || strings.Count(out, "INSERT INTO `t` VALUES (") != 2 {
			t.Fatalf("comment form not reshaped for %q ->\n%s", in, out)
		}
	}
}

func TestNoMergeAfterVerbatimStatement(t *testing.T) {
	// A verbatim statement ends in ';' with no newline; the following reshaped
	// INSERT must still start on its own line.
	in := "LOCK TABLES `t` WRITE;\nINSERT INTO `t` VALUES (1),(2);\n"
	out, _ := run(t, in, Options{Mode: ModeSingleRow})
	if strings.Contains(out, ";INSERT") {
		t.Fatalf("verbatim statement merged with reshaped INSERT:\n%s", out)
	}
	if strings.Count(out, "INSERT INTO `t` VALUES (") != 2 {
		t.Fatalf("insert not exploded:\n%s", out)
	}
}

func TestRejectsSamePath(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "x.sql")
	if err := os.WriteFile(src, []byte("INSERT INTO t VALUES (1);"), 0o666); err != nil {
		t.Fatal(err)
	}
	_, err := ReshapeInsertsFile(context.Background(), src, src, Options{Mode: ModeSingleRow})
	if err == nil {
		t.Fatal("expected error for same path")
	}
}

func nonEmptyLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}
