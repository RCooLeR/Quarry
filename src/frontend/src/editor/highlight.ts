// Unified, dependency-free syntax highlighting for the windowed editor.
//
// Like the SQL highlighter, everything here works PER VISIBLE LINE so the
// editor's collapsed/truncated long lines never bleed a string/comment colour
// into the rest of the window. Three modes:
//   - rainbow CSV  : each field coloured by its column index
//   - SQL          : the dedicated SQL highlighter (sqlHighlight.ts)
//   - generic code : comments / strings / numbers / keywords (+ json/yaml keys)
//     driven by a per-language profile chosen from the file extension.

import { RangeSetBuilder } from "@codemirror/state";
import { Decoration, DecorationSet, EditorView, ViewPlugin, ViewUpdate } from "@codemirror/view";
import { MAX_EDITOR_HIGHLIGHT_RANGES } from "./highlightLimits";
import { sqlHighlight } from "./sqlHighlight";

// ---------------------------------------------------------------------------
// Generic language highlighter
// ---------------------------------------------------------------------------

type LineKeys = "json" | "yaml" | "none";

interface LangProfile {
  lineComment: string[];
  block?: [string, string];
  strings: string[]; // quote characters
  keywords: Set<string>;
  keys: LineKeys; // colour `key:` / "key": as keys (json/yaml/configs)
}

const esc = (s: string) => s.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
const kw = (s: string) => new Set(s.split(/\s+/).filter(Boolean));

// Keyword sets (pragmatic — enough to make code readable).
const KW = {
  js: kw("abstract any as async await boolean break case catch class const continue debugger declare default delete do else enum export extends false finally for from function get if implements import in instanceof interface let new null of private protected public readonly return set static super switch this throw true try type typeof undefined var void while yield"),
  py: kw("and as assert async await break class continue def del elif else except False finally for from global if import in is lambda None nonlocal not or pass raise return True try while with yield self"),
  go: kw("break case chan const continue default defer else fallthrough for func go goto if import interface map package range return select struct switch type var nil true false iota string int int64 byte rune bool error"),
  rust: kw("as async await break const continue crate dyn else enum extern false fn for if impl in let loop match mod move mut pub ref return self Self static struct super trait true type unsafe use where while async"),
  java: kw("abstract assert boolean break byte case catch char class const continue default do double else enum extends final finally float for goto if implements import instanceof int interface long native new package private protected public return short static super switch synchronized this throw throws transient true false null try void volatile while var record"),
  c: kw("auto break case char const continue default do double else enum extern float for goto if inline int long register restrict return short signed sizeof static struct switch typedef union unsigned void volatile while bool true false NULL include define ifdef ifndef endif pragma"),
  cpp: kw("alignas alignof auto bool break case catch char class const constexpr continue decltype default delete do double else enum explicit export extern false float for friend goto if inline int long mutable namespace new noexcept nullptr operator private protected public return short signed sizeof static struct switch template this throw true try typedef typename union unsigned using virtual void volatile while"),
  cs: kw("abstract as base bool break byte case catch char checked class const continue decimal default delegate do double else enum event explicit extern false finally fixed float for foreach goto if implicit in int interface internal is lock long namespace new null object operator out override params private protected public readonly ref return sbyte sealed short sizeof static string struct switch this throw true try typeof uint ulong unchecked unsafe ushort using var virtual void volatile while async await yield"),
  php: kw("abstract and array as break callable case catch class clone const continue declare default do echo else elseif empty enddeclare endfor endforeach endif endswitch endwhile extends final finally fn for foreach function global goto if implements include include_once instanceof insteadof interface isset list namespace new or print private protected public require require_once return static switch throw trait try unset use var while xor yield true false null"),
  ruby: kw("alias and begin break case class def defined do else elsif end ensure false for if in module next nil not or redo rescue retry return self super then true unless until when while yield attr_accessor attr_reader attr_writer require require_relative puts"),
  shell: kw("if then else elif fi case esac for while until do done function in select time return break continue echo export local readonly declare set unset source alias"),
  kotlin: kw("abstract as break by catch class companion const continue crossinline data do dynamic else enum external false final finally for fun get if import in infix init inline inner interface internal is lateinit object open operator out override package private protected public reified return sealed set super suspend this throw true try typealias typeof val var vararg when where while null"),
  swift: kw("associatedtype class deinit enum extension fileprivate func import init inout internal let open operator private protocol public rethrows static struct subscript typealias var break case continue default defer do else fallthrough for guard if in repeat return switch where while as catch false is nil rethrows self super throw throws true try nil"),
  json: kw("true false null"),
  yaml: kw("true false null yes no on off"),
  toml: kw("true false"),
  css: kw("important inherit initial unset none auto"),
};

const C_FAMILY = { lineComment: ["//"], block: ["/*", "*/"] as [string, string], strings: ['"', "'", "`"] };
const HASH_FAMILY = { lineComment: ["#"], strings: ['"', "'"] };

const PROFILES: Record<string, LangProfile> = {
  js: { ...C_FAMILY, keywords: KW.js, keys: "none" },
  py: { ...HASH_FAMILY, keywords: KW.py, keys: "none" },
  go: { ...C_FAMILY, keywords: KW.go, keys: "none" },
  rust: { ...C_FAMILY, keywords: KW.rust, keys: "none" },
  java: { ...C_FAMILY, keywords: KW.java, keys: "none" },
  c: { ...C_FAMILY, keywords: KW.c, keys: "none" },
  cpp: { ...C_FAMILY, keywords: KW.cpp, keys: "none" },
  cs: { ...C_FAMILY, keywords: KW.cs, keys: "none" },
  php: { lineComment: ["//", "#"], block: ["/*", "*/"], strings: ['"', "'"], keywords: KW.php, keys: "none" },
  ruby: { ...HASH_FAMILY, keywords: KW.ruby, keys: "none" },
  shell: { ...HASH_FAMILY, keywords: KW.shell, keys: "none" },
  kotlin: { ...C_FAMILY, keywords: KW.kotlin, keys: "none" },
  swift: { ...C_FAMILY, keywords: KW.swift, keys: "none" },
  json: { lineComment: [], strings: ['"'], keywords: KW.json, keys: "json" },
  yaml: { lineComment: ["#"], strings: ['"', "'"], keywords: KW.yaml, keys: "yaml" },
  toml: { lineComment: ["#"], strings: ['"', "'"], keywords: KW.toml, keys: "yaml" },
  ini: { lineComment: [";", "#"], strings: ['"', "'"], keywords: new Set<string>(), keys: "yaml" },
  xml: { lineComment: [], block: ["<!--", "-->"], strings: ['"', "'"], keywords: new Set<string>(), keys: "none" },
  css: { lineComment: [], block: ["/*", "*/"], strings: ['"', "'"], keywords: KW.css, keys: "none" },
};

// Extension → profile key.
const EXT_TO_PROFILE: Record<string, string> = {
  js: "js", mjs: "js", cjs: "js", jsx: "js", ts: "js", tsx: "js", json: "json", jsonc: "json",
  py: "py", pyw: "py", go: "go", rs: "rust", java: "java",
  c: "c", h: "c", cpp: "cpp", cc: "cpp", cxx: "cpp", hpp: "cpp", hxx: "cpp",
  cs: "cs", php: "php", rb: "ruby", sh: "shell", bash: "shell", zsh: "shell",
  kt: "kotlin", kts: "kotlin", swift: "swift",
  yaml: "yaml", yml: "yaml", toml: "toml", ini: "ini", cfg: "ini", conf: "ini",
  xml: "xml", html: "xml", htm: "xml", svg: "xml", css: "css", scss: "css", less: "css",
};

const NUMBER = "\\b0[xX][0-9a-fA-F]+\\b|\\b\\d[\\d_]*(?:\\.\\d+)?(?:[eE][+-]?\\d+)?\\b";
const WORD = "[A-Za-z_$][A-Za-z0-9_$]*";
const NEVER = "((?!))"; // capturing group that can never match — keeps indices stable

function buildRegex(p: LangProfile): RegExp {
  const comments: string[] = [];
  if (p.block) comments.push(esc(p.block[0]) + "[\\s\\S]*?" + esc(p.block[1]), esc(p.block[0]) + "[^\\n]*");
  for (const lc of p.lineComment) comments.push(esc(lc) + "[^\\n]*");
  const strings: string[] = [];
  for (const q of p.strings) {
    const e = esc(q);
    strings.push(e + "(?:\\\\.|" + e + e + "|[^" + e + "\\\\])*" + e + "?");
  }
  const g = (arr: string[]) => (arr.length ? "(" + arr.join("|") + ")" : NEVER);
  return new RegExp([g(comments), g(strings), "(" + NUMBER + ")", "(" + WORD + ")"].join("|"), "g");
}

const HL = {
  comment: Decoration.mark({ class: "q-hl-comment" }),
  string: Decoration.mark({ class: "q-hl-string" }),
  number: Decoration.mark({ class: "q-hl-number" }),
  keyword: Decoration.mark({ class: "q-hl-keyword" }),
  key: Decoration.mark({ class: "q-hl-key" }),
};

function genericPlugin(profile: LangProfile) {
  const re = buildRegex(profile);
  const build = (view: EditorView): DecorationSet => {
    const b = new RangeSetBuilder<Decoration>();
    let remaining = MAX_EDITOR_HIGHLIGHT_RANGES;
    visible: for (const { from, to } of view.visibleRanges) {
      let pos = from;
      while (pos <= to) {
        const line = view.state.doc.lineAt(pos);
        const text = line.text;
        re.lastIndex = 0;
        let m: RegExpExecArray | null;
        while ((m = re.exec(text)) !== null) {
          let deco: Decoration | null = null;
          if (m[1] !== undefined) deco = HL.comment;
          else if (m[2] !== undefined) {
            deco = profile.keys !== "none" && /^\s*:/.test(text.slice(m.index + m[0].length)) ? HL.key : HL.string;
          } else if (m[3] !== undefined) deco = HL.number;
          else if (m[4] !== undefined) {
            if (profile.keywords.has(m[4]) || profile.keywords.has(m[4].toLowerCase())) deco = HL.keyword;
            else if (profile.keys !== "none" && /^\s*:/.test(text.slice(m.index + m[0].length))) deco = HL.key;
          }
          if (deco) {
            const start = line.from + m.index;
            b.add(start, start + m[0].length, deco);
            remaining--;
            if (remaining === 0) break visible;
          }
          if (m[0].length === 0) re.lastIndex++;
        }
        pos = line.to + 1;
      }
    }
    return b.finish();
  };
  return ViewPlugin.fromClass(
    class {
      decorations: DecorationSet;
      constructor(view: EditorView) {
        this.decorations = build(view);
      }
      update(u: ViewUpdate) {
        if (u.docChanged || u.viewportChanged) this.decorations = build(u.view);
      }
    },
    { decorations: (v) => v.decorations },
  );
}

export type EditorColorTheme = "dark" | "light";

// These backgrounds are shared with QuarryEditor so the contrast contract is
// explicit and testable. Every light-theme foreground below is at least 4.5:1
// against #fbfcfe; color is never the only indication of an editor selection.
export const EDITOR_SYNTAX_BACKGROUNDS = {
  dark: "#12161c",
  light: "#fbfcfe",
} as const;

export const EDITOR_SYNTAX_PALETTES = {
  dark: {
    generic: {
      comment: "#6a9955",
      string: "#ce9178",
      number: "#b5cea8",
      keyword: "#569cd6",
      key: "#4ec9b0",
    },
    sql: {
      comment: "#6a9955",
      string: "#ce9178",
      ident: "#4ec9b0",
      number: "#b5cea8",
      keyword: "#569cd6",
    },
    csv: ["#e06c75", "#d19a66", "#e5c07b", "#98c379", "#56b6c2", "#61afef", "#c678dd", "#b48ead"],
  },
  light: {
    generic: {
      comment: "#52606d",
      string: "#8a2c0d",
      number: "#4e5d00",
      keyword: "#174ea6",
      key: "#00695c",
    },
    sql: {
      comment: "#52606d",
      string: "#8a2c0d",
      ident: "#00695c",
      number: "#4e5d00",
      keyword: "#174ea6",
    },
    csv: ["#9f1239", "#9a3412", "#854d0e", "#3f6212", "#0f6b61", "#075985", "#6b21a8", "#7e2855"],
  },
} as const;

// ---------------------------------------------------------------------------
// Rainbow CSV
// ---------------------------------------------------------------------------

const CSV_COLORS = 8;
export const MAX_CSV_DELIMITER_DETECTION_CODE_UNITS = 64 * 1024;

export function detectCsvDelimiter(text: string): string {
  let commas = 0;
  let tabs = 0;
  let semicolons = 0;
  let pipes = 0;
  const scanLength = Math.min(text.length, MAX_CSV_DELIMITER_DETECTION_CODE_UNITS);
  for (let index = 0; index < scanLength; index++) {
    switch (text[index]) {
      case ",": commas++; break;
      case "\t": tabs++; break;
      case ";": semicolons++; break;
      case "|": pipes++; break;
    }
  }
  let best = ",";
  let bestCount = commas;
  if (tabs > bestCount) { best = "\t"; bestCount = tabs; }
  if (semicolons > bestCount) { best = ";"; bestCount = semicolons; }
  if (pipes > bestCount) best = "|";
  return best;
}

// Rainbow coloring is cosmetic and must never amplify one bounded editor
// window into hundreds of thousands of decorations. Normal CSV rows retain
// their full highlighting; only pathological, ultra-wide rows hit this cap.
export const MAX_CSV_HIGHLIGHT_RANGES = MAX_EDITOR_HIGHLIGHT_RANGES;

function visitCsvFieldRanges(
  text: string,
  delim: string,
  limit: number,
  visit: (start: number, end: number) => void,
): number {
  const boundedLimit = Math.max(0, Math.min(MAX_CSV_HIGHLIGHT_RANGES, Math.floor(limit)));
  if (boundedLimit === 0) return 0;
  let start = 0;
  let inQuotes = false;
  let visited = 0;
  const emit = (end: number): boolean => {
    visit(start, end);
    visited++;
    return visited >= boundedLimit;
  };
  for (let i = 0; i < text.length; i++) {
    const ch = text[i];
    if (ch === '"') {
      if (inQuotes && text[i + 1] === '"') {
        i++;
        continue;
      }
      inQuotes = !inQuotes;
    } else if (ch === delim && !inQuotes) {
      if (emit(i)) return visited;
      start = i + 1;
    }
  }
  emit(text.length);
  return visited;
}

// Split a CSV line into field ranges, respecting double-quoted fields. The
// returned collection is capped for callers outside the editor plugin too.
export function csvFieldRanges(
  text: string,
  delim: string,
  limit = MAX_CSV_HIGHLIGHT_RANGES,
): Array<[number, number]> {
  const ranges: Array<[number, number]> = [];
  visitCsvFieldRanges(text, delim, limit, (start, end) => ranges.push([start, end]));
  return ranges;
}

const CSV_MARKS = Array.from({ length: CSV_COLORS }, (_, i) => Decoration.mark({ class: "q-csv-col-" + i }));

function rainbowPlugin(configuredDelimiter: string) {
  // This closure belongs to one editor/file configuration. When no confirmed
  // dialect is available yet, detection is local to this extension instance;
  // it can never retain a separator from a previously attached file.
  let delim = configuredDelimiter;
  const build = (view: EditorView): DecorationSet => {
    const b = new RangeSetBuilder<Decoration>();
    let remaining = MAX_CSV_HIGHLIGHT_RANGES;
    visible: for (const { from, to } of view.visibleRanges) {
      let pos = from;
      while (pos <= to) {
        const line = view.state.doc.lineAt(pos);
        if (line.text.length > 0) {
          if (!delim) delim = detectCsvDelimiter(line.text);
          let col = 0;
          const visited = visitCsvFieldRanges(line.text, delim, remaining, (start, end) => {
            if (end > start) b.add(line.from + start, line.from + end, CSV_MARKS[col % CSV_COLORS]);
            col++;
          });
          remaining -= visited;
          if (remaining === 0) break visible;
        }
        pos = line.to + 1;
      }
    }
    return b.finish();
  };
  return ViewPlugin.fromClass(
    class {
      decorations: DecorationSet;
      constructor(view: EditorView) {
        this.decorations = build(view);
      }
      update(u: ViewUpdate) {
        if (u.docChanged || u.viewportChanged) this.decorations = build(u.view);
      }
    },
    { decorations: (v) => v.decorations },
  );
}

function rainbowCsv(delimiter: string) {
  return [rainbowPlugin(delimiter)];
}

function createSyntaxTheme(theme: EditorColorTheme) {
  const colors = EDITOR_SYNTAX_PALETTES[theme];
  return EditorView.theme({
    ".q-hl-comment": { color: colors.generic.comment, fontStyle: "italic" },
    ".q-hl-string": { color: colors.generic.string },
    ".q-hl-number": { color: colors.generic.number },
    ".q-hl-keyword": { color: colors.generic.keyword },
    ".q-hl-key": { color: colors.generic.key },
    ".q-sql-comment": { color: colors.sql.comment, fontStyle: "italic" },
    ".q-sql-string": { color: colors.sql.string },
    ".q-sql-ident": { color: colors.sql.ident },
    ".q-sql-number": { color: colors.sql.number },
    ".q-sql-keyword": { color: colors.sql.keyword },
    ...Object.fromEntries(colors.csv.map((color, index) => [`.q-csv-col-${index}`, { color }])),
  });
}

const syntaxThemes = {
  dark: createSyntaxTheme("dark"),
  light: createSyntaxTheme("light"),
};

/** Token colors live in the editor theme compartment, not the language one. */
export function syntaxThemeFor(theme: string) {
  return theme === "light" ? syntaxThemes.light : syntaxThemes.dark;
}

// ---------------------------------------------------------------------------
// Dispatch
// ---------------------------------------------------------------------------

function extOf(path: string): string {
  const base = path.replace(/[\\/]+$/, "");
  const dot = base.lastIndexOf(".");
  const slash = Math.max(base.lastIndexOf("/"), base.lastIndexOf("\\"));
  return dot > slash ? base.slice(dot + 1).toLowerCase() : "";
}

/** Returns fresh highlight extension(s) for one file and its confirmed dialect. */
export function highlightFor(detected: string, path: string, csvDelimiter = "") {
  const d = (detected || "").toLowerCase();
  const ext = extOf(path);
  if (d === "csv" || d === "tsv" || ext === "csv" || ext === "tsv") {
    const delimiter = Array.from(csvDelimiter).length === 1
      ? csvDelimiter
      : (d === "tsv" || ext === "tsv" ? "\t" : "");
    return rainbowCsv(delimiter);
  }
  if (d === "sql" || ext === "sql") return sqlHighlight;
  const key = EXT_TO_PROFILE[ext];
  if (key && PROFILES[key]) return [genericPlugin(PROFILES[key])];
  return [];
}
