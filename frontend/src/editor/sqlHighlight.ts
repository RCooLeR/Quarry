// Lightweight SQL syntax highlighting for the windowed editor.
//
// This deliberately does NOT use a full grammar/parser. The editor shows a
// synthetic window whose long lines are COLLAPSED and truncated with "⋯", so a
// real parser would frequently see a string/comment cut mid-token and bleed that
// colour across the rest of the window. Instead we tokenise each VISIBLE line
// independently with a single regex, which keeps highlighting bounded to the
// viewport and prevents a truncated line from affecting the next one.

import { RangeSetBuilder } from "@codemirror/state";
import { Decoration, DecorationSet, EditorView, ViewPlugin, ViewUpdate } from "@codemirror/view";

// A pragmatic MySQL/ANSI keyword set — enough to make dumps readable.
const KEYWORDS = new Set(
  (
    "ADD ALTER AND AS ASC AUTO_INCREMENT BEGIN BETWEEN BIGINT BINARY BLOB BOOLEAN BY " +
    "CASCADE CASE CHANGE CHAR CHARACTER CHARSET COLLATE COLUMN COMMIT CONSTRAINT CREATE " +
    "CROSS CURRENT_TIMESTAMP DATABASE DATABASES DATE DATETIME DECIMAL DEFAULT DEFINER " +
    "DELETE DESC DISTINCT DOUBLE DROP ELSE ENGINE ENUM EXISTS FALSE FLOAT FOREIGN FROM " +
    "FULL FULLTEXT FUNCTION GRANT GROUP HAVING IF IGNORE IN INDEX INNER INSERT INT INTEGER " +
    "INTO IS JOIN KEY KEYS LEFT LIKE LIMIT LOCK LONGBLOB LONGTEXT MEDIUMBLOB MEDIUMINT " +
    "MEDIUMTEXT MODIFY NOT NULL ON OR ORDER OUTER PRIMARY PROCEDURE REFERENCES RENAME " +
    "REPLACE RIGHT SELECT SET SMALLINT START TABLE TABLES TEMPORARY TEXT THEN TIME TIMESTAMP " +
    "TINYINT TINYTEXT TO TRANSACTION TRIGGER TRUE TRUNCATE UNION UNIQUE UNLOCK UNSIGNED " +
    "UPDATE USE USING VALUES VARBINARY VARCHAR VIEW WHEN WHERE WITH ZEROFILL"
  ).split(" "),
);

// One pass per line. Groups, in priority order:
// 1 comment  2 single-quote string  3 double-quote string  4 backtick identifier
// 5 number   6 word (keyword candidate)
const TOKEN =
  /(--[ \t][^\n]*|--$|#[^\n]*|\/\*[\s\S]*?\*\/|\/\*[^\n]*)|('(?:\\.|''|[^'\\])*'?)|("(?:\\.|""|[^"\\])*"?)|(`(?:``|[^`])*`?)|(\b\d[\d.]*\b)|([A-Za-z_][A-Za-z0-9_$]*)/g;

const DECOS = {
  comment: Decoration.mark({ class: "q-sql-comment" }),
  string: Decoration.mark({ class: "q-sql-string" }),
  ident: Decoration.mark({ class: "q-sql-ident" }),
  number: Decoration.mark({ class: "q-sql-number" }),
  keyword: Decoration.mark({ class: "q-sql-keyword" }),
};

function buildDecorations(view: EditorView): DecorationSet {
  const builder = new RangeSetBuilder<Decoration>();
  for (const { from, to } of view.visibleRanges) {
    let pos = from;
    while (pos <= to) {
      const line = view.state.doc.lineAt(pos);
      const text = line.text;
      TOKEN.lastIndex = 0;
      let m: RegExpExecArray | null;
      while ((m = TOKEN.exec(text)) !== null) {
        let deco: Decoration | null = null;
        if (m[1] !== undefined) deco = DECOS.comment;
        else if (m[2] !== undefined || m[3] !== undefined) deco = DECOS.string;
        else if (m[4] !== undefined) deco = DECOS.ident;
        else if (m[5] !== undefined) deco = DECOS.number;
        else if (m[6] !== undefined && KEYWORDS.has(m[6].toUpperCase())) deco = DECOS.keyword;
        if (deco) {
          const start = line.from + m.index;
          builder.add(start, start + m[0].length, deco);
        }
        if (m[0].length === 0) TOKEN.lastIndex++; // guard against zero-width matches
      }
      pos = line.to + 1;
    }
  }
  return builder.finish();
}

const sqlHighlightPlugin = ViewPlugin.fromClass(
  class {
    decorations: DecorationSet;
    constructor(view: EditorView) {
      this.decorations = buildDecorations(view);
    }
    update(u: ViewUpdate) {
      if (u.docChanged || u.viewportChanged) {
        this.decorations = buildDecorations(u.view);
      }
    }
  },
  { decorations: (v) => v.decorations },
);

const sqlHighlightTheme = EditorView.theme({
  ".q-sql-comment": { color: "#6a9955", fontStyle: "italic" },
  ".q-sql-string": { color: "#ce9178" },
  ".q-sql-ident": { color: "#4ec9b0" }, // backtick-quoted identifiers
  ".q-sql-number": { color: "#b5cea8" },
  ".q-sql-keyword": { color: "#569cd6" },
});

/** SQL highlighting extension (display-only, per visible line). */
export const sqlHighlight = [sqlHighlightPlugin, sqlHighlightTheme];
