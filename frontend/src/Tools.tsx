import { useEffect, useState } from "react";
import { FileService } from "../bindings/github.com/quarry/quarry-wails3";

interface CsvDelimiterOption { delimiter: string; name: string; columns: number; score: number; }
interface CsvInspectResult { delimiter: string; delimiterName: string; confidence: string; columns: number; hasHeader: boolean; candidates: CsvDelimiterOption[]; warnings: string[]; }
interface CsvColumn { name: string; sqlType: string; nonNull: number; null: number; samples: string[]; }
interface CsvSchemaResult { columns: CsvColumn[]; hasHeader: boolean; warnings: string[]; }
interface CsvPreviewResult { header: string[]; rows: string[][]; warnings: string[]; }
interface SqlTable { name: string; createOffset: number; insertOffset: number; bytes: number; }
interface SqlSummaryResult { tables: SqlTable[]; createTables: number; insertTables: number; definerCount: number; header: boolean; }
interface TransformResult { outputPath: string; recordsRead: number; recordsWritten: number; note: string; }
interface ValueCount { value: string; count: number; }
interface ColumnProfile { name: string; sqlType: string; nonNull: number; null: number; distinct: number; distinctCapped: boolean; min: string; max: string; top: ValueCount[]; }
interface CsvProfileResult { columns: ColumnProfile[]; recordsScanned: number; raggedRows: number; truncated: boolean; }
interface SqlLintFinding { severity: string; title: string; detail: string; }
interface SqlLintResult { findings: SqlLintFinding[]; }

interface ColCfg { source: number; name: string; type: string; include: boolean; }

const DELIMS: { value: string; label: string }[] = [
  { value: ",", label: "Comma  ," },
  { value: "\t", label: "Tab  \\t" },
  { value: ";", label: "Semicolon  ;" },
  { value: "|", label: "Pipe  |" },
  { value: " ", label: "Space" },
];

const SQL_TYPES = [
  "BIGINT", "INT", "SMALLINT", "DOUBLE", "DECIMAL(10,2)", "BOOLEAN",
  "DATE", "DATETIME", "TIMESTAMP", "VARCHAR(255)", "TEXT", "LONGTEXT", "BLOB",
];

function fmtBytes(n: number): string {
  if (!n) return "—";
  if (n < 1024) return `${n} B`;
  const u = ["KB", "MB", "GB", "TB"];
  let v = n / 1024, i = 0;
  while (v >= 1024 && i < u.length - 1) { v /= 1024; i++; }
  return `${v.toFixed(v < 10 ? 1 : 0)} ${u[i]}`;
}

function presetArgLabels(name: string): string[] {
  const n = name.toLowerCase();
  if (n.includes("database")) return ["old database name", "new database name"];
  if (n.includes("charset")) return ["old charset", "new charset", "old collation (opt)", "new collation (opt)"];
  return [];
}

interface OtherFile { id: string; name: string; detected: string; }

interface ColumnChange { name: string; old: string; new: string; }
interface TableDiff { name: string; addedColumns: { name: string; definition: string }[]; removedColumns: { name: string; definition: string }[]; changedColumns: ColumnChange[]; }
interface SqlSchemaDiffResult { fileA: string; fileB: string; addedTables: string[]; removedTables: string[]; changedTables: TableDiff[]; unchangedCount: number; }

interface Props {
  fileId: string;
  detected: string;
  analysis: SqlSummaryResult | null;
  otherFiles: OtherFile[];
  onAnalyze: (fileId: string) => Promise<SqlSummaryResult>;
  onNotice: (s: string) => void;
  onError: (s: string) => void;
  onClose: () => void;
}

export default function Tools({ fileId, detected, analysis, otherFiles, onAnalyze, onNotice, onError, onClose }: Props) {
  const d = detected.toLowerCase();
  const isCsv = d === "csv" || d === "tsv";

  // CSV state
  const [delim, setDelim] = useState(",");
  const [hasHeader, setHasHeader] = useState(true);
  const [inspect, setInspect] = useState<CsvInspectResult | null>(null);
  const [schema, setSchema] = useState<CsvColumn[]>([]);
  const [preview, setPreview] = useState<CsvPreviewResult | null>(null);
  const [cols, setCols] = useState<ColCfg[]>([]);
  const [dropCol, setDropCol] = useState(0);
  const [addVal, setAddVal] = useState("");
  const [tableName, setTableName] = useState("imported");
  const [includeCreate, setIncludeCreate] = useState(true);
  const [insertMode, setInsertMode] = useState("insert");
  const [batchSize, setBatchSize] = useState(500);
  const [nullValue, setNullValue] = useState("NULL");
  const [onInvalid, setOnInvalid] = useState("fail");
  const [sqlPreviewText, setSqlPreviewText] = useState("");
  const [redactModes, setRedactModes] = useState<Record<number, string>>({});
  const [redactFixed, setRedactFixed] = useState("REDACTED");
  const [profile, setProfile] = useState<CsvProfileResult | null>(null);
  const [lint, setLint] = useState<SqlLintResult | null>(null);
  // filter / dedupe / sample
  const [filterCol, setFilterCol] = useState(0);
  const [filterOp, setFilterOp] = useState("contains");
  const [filterVal, setFilterVal] = useState("");
  const [filterNeg, setFilterNeg] = useState(false);
  const [dedupeKey, setDedupeKey] = useState(-1);
  const [sampleEvery, setSampleEvery] = useState(10);

  // SQL state (analysis is lifted to App; shared with palette + X-ray)
  const sqlSummary = analysis;
  const [find, setFind] = useState("");
  const [repl, setRepl] = useState("");
  const [regex, setRegex] = useState(false);
  const [ci, setCi] = useState(false);
  const [presets, setPresets] = useState<string[]>([]);
  const [preset, setPreset] = useState("");
  const [pa, setPa] = useState<string[]>(["", "", "", ""]);
  const [sampleRows, setSampleRows] = useState(100);
  const [reshapeMode, setReshapeMode] = useState("single");
  const [reshapeBatch, setReshapeBatch] = useState(100);
  const [diffTarget, setDiffTarget] = useState("");
  const [diff, setDiff] = useState<SqlSchemaDiffResult | null>(null);

  const [busy, setBusy] = useState(false);

  useEffect(() => {
    setInspect(null); setSchema([]); setPreview(null); setCols([]); setSqlPreviewText("");
    if (!isCsv) {
      (async () => {
        try {
          const p = (await FileService.SqlListPresets()) as string[];
          setPresets(p);
          if (p.length) setPreset(p[0]);
        } catch { /* ignore */ }
      })();
      return;
    }
    let cancelled = false;
    (async () => {
      try {
        const r = (await FileService.CsvInspect(fileId)) as CsvInspectResult;
        if (cancelled) return;
        setInspect(r);
        if (r.delimiter) setDelim(r.delimiter);
        setHasHeader(r.hasHeader);
        await refreshCsv(r.delimiter || ",", r.hasHeader);
      } catch (e: any) {
        if (!cancelled) onError(String(e?.message ?? e));
      }
    })();
    return () => { cancelled = true; };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [fileId, detected]);

  const refreshCsv = async (dl: string, h: boolean) => {
    try {
      const sc = (await FileService.CsvSchema(fileId, dl, h)) as CsvSchemaResult;
      setSchema(sc.columns);
      setCols(sc.columns.map((c, i) => ({ source: i, name: c.name, type: c.sqlType || "TEXT", include: true })));
      if (dropCol >= sc.columns.length) setDropCol(0);
      const pv = (await FileService.CsvPreview(fileId, dl, h, 12)) as CsvPreviewResult;
      setPreview(pv);
    } catch (e: any) {
      onError(String(e?.message ?? e));
    }
  };

  const onDelimChange = async (dl: string) => { setDelim(dl); await refreshCsv(dl, hasHeader); };
  const onHeaderChange = async (h: boolean) => { setHasHeader(h); await refreshCsv(delim, h); };

  const setCol = (i: number, patch: Partial<ColCfg>) =>
    setCols((prev) => prev.map((c, j) => (j === i ? { ...c, ...patch } : c)));

  const buildConfig = () => ({
    delimiter: delim,
    hasHeader,
    tableName,
    columns: cols,
    includeCreate,
    insertMode,
    batchSize,
    nullValues: nullValue.split(",").map((s) => s.trim()).filter(Boolean),
    onInvalid,
  });

  const run = async (fn: () => Promise<TransformResult>) => {
    setBusy(true);
    try {
      const r = await fn();
      if (r && r.outputPath) onNotice(`Wrote ${r.outputPath} — ${r.note}`);
    } catch (e: any) {
      onError(String(e?.message ?? e));
    } finally {
      setBusy(false);
    }
  };

  // CSV transforms
  const doDrop = () =>
    run(async () => {
      const keep = schema.map((_, i) => i).filter((i) => i !== dropCol);
      return (await FileService.CsvProjectViaDialog(fileId, delim, keep)) as TransformResult;
    });
  const doAdd = () =>
    run(async () => (await FileService.CsvAddColumnViaDialog(fileId, delim, schema.length, addVal)) as TransformResult);
  const doConvert = () =>
    run(async () => (await FileService.CsvToSQLConfigViaDialog(fileId, buildConfig() as any)) as TransformResult);
  const redactCols = Object.entries(redactModes).filter(([, m]) => m && m !== "off").map(([i, m]) => ({ index: Number(i), mode: m }));
  const doRedact = () =>
    run(async () => (await FileService.CsvRedactViaDialog(fileId, delim, hasHeader, redactCols as any, redactFixed)) as TransformResult);
  const doSqlPreview = async () => {
    try {
      const sql = (await FileService.CsvToSQLConfigPreview(fileId, buildConfig() as any)) as string;
      setSqlPreviewText(sql);
    } catch (e: any) {
      onError(String(e?.message ?? e));
    }
  };
  const doProfile = async () => {
    try { setProfile((await FileService.CsvProfile(fileId, delim, hasHeader)) as CsvProfileResult); } catch (e: any) { onError(String(e?.message ?? e)); }
  };
  const needsValue = !(filterOp === "empty" || filterOp === "nonempty");
  const doFilter = () =>
    run(async () => (await FileService.CsvFilterViaDialog(fileId, delim, hasHeader, filterCol, filterOp, filterVal, filterNeg)) as TransformResult);
  const doDedupe = () =>
    run(async () => (await FileService.CsvDedupeViaDialog(fileId, delim, hasHeader, dedupeKey)) as TransformResult);
  const doSampleCsv = () =>
    run(async () => (await FileService.CsvSampleViaDialog(fileId, delim, hasHeader, sampleEvery)) as TransformResult);
  const doExportJsonl = () =>
    run(async () => (await FileService.CsvExportJSONLViaDialog(fileId, delim, hasHeader, true)) as TransformResult);
  const doExportSqlite = () =>
    run(async () => (await FileService.CsvExportSQLiteViaDialog(fileId, delim, hasHeader, tableName, true)) as TransformResult);
  const doExportXlsx = () =>
    run(async () => (await FileService.CsvExportXLSXViaDialog(fileId, delim, hasHeader, tableName, true)) as TransformResult);
  const doCopyMarkdown = async () => {
    try {
      const md = (await FileService.CsvMarkdownPreview(fileId, delim, hasHeader, 50)) as string;
      await navigator.clipboard.writeText(md);
      onNotice("Copied preview as Markdown");
    } catch (e: any) {
      onError(String(e?.message ?? e));
    }
  };

  // SQL tools
  const doAnalyze = async () => {
    setBusy(true);
    onNotice("Analyzing dump…");
    try {
      const s = await onAnalyze(fileId);
      onNotice(`Found ${s.tables.length} tables`);
    } catch (e: any) {
      onError(String(e?.message ?? e));
    } finally {
      setBusy(false);
    }
  };
  const doExtract = (name: string) => run(async () => (await FileService.SqlExtractTableViaDialog(fileId, name)) as TransformResult);
  const doSchema = (name: string) => run(async () => (await FileService.SqlExtractSchemaViaDialog(fileId, name)) as TransformResult);
  const doData = (name: string) => run(async () => (await FileService.SqlExtractDataViaDialog(fileId, name)) as TransformResult);
  const doSplit = () => run(async () => (await FileService.SqlSplitByTableViaDialog(fileId)) as TransformResult);
  const doSample = () => run(async () => (await FileService.SqlSampleFixtureViaDialog(fileId, sampleRows)) as TransformResult);
  const doLint = async () => {
    try { setLint((await FileService.SqlLint(fileId)) as SqlLintResult); } catch (e: any) { onError(String(e?.message ?? e)); }
  };
  const doReplace = () => run(async () => (await FileService.SqlReplaceViaDialog(fileId, find, repl, regex, ci, false)) as TransformResult);
  const doPreset = () => run(async () => (await FileService.SqlApplyPresetViaDialog(fileId, preset, pa[0], pa[1], pa[2], pa[3])) as TransformResult);
  const doReshape = () => run(async () => (await FileService.SqlReshapeInsertsViaDialog(fileId, reshapeMode, reshapeBatch)) as TransformResult);
  const doDiff = async () => {
    if (!diffTarget) return;
    setBusy(true);
    onNotice("Diffing schemas…");
    try {
      const r = (await FileService.SqlSchemaDiff(fileId, diffTarget)) as SqlSchemaDiffResult;
      setDiff(r);
      const changes = r.addedTables.length + r.removedTables.length + r.changedTables.length;
      onNotice(changes === 0 ? "Schemas are identical" : `${changes} table changes`);
    } catch (e: any) {
      onError(String(e?.message ?? e));
    } finally {
      setBusy(false);
    }
  };
  const sqlOthers = otherFiles.filter((o) => ["sql"].includes(o.detected.toLowerCase()));

  const delims = DELIMS.some((x) => x.value === delim) ? DELIMS : [{ value: delim, label: `Detected (${JSON.stringify(delim)})` }, ...DELIMS];
  const typeOptions = (t: string) => (SQL_TYPES.includes(t) ? SQL_TYPES : [t, ...SQL_TYPES]);
  const selectedCount = cols.filter((c) => c.include).length;

  return (
    <div className="q-tools">
      <div className="q-tools-head">
        <span>{isCsv ? "CSV tools" : "SQL tools"}</span>
        <button className="q-icon" onClick={onClose}>×</button>
      </div>
      <div className="q-tools-body">
        {isCsv ? (
          <>
            <label className="q-tlabel">Separator</label>
            <div className="q-trow">
              <select className="q-select" value={delim} onChange={(e) => void onDelimChange(e.target.value)}>
                {delims.map((x) => (<option key={x.value} value={x.value}>{x.label}</option>))}
              </select>
              {inspect && <span className="q-thint">detected {inspect.delimiterName} ({inspect.confidence}), {inspect.columns} cols</span>}
            </div>
            <label className="q-check">
              <input type="checkbox" checked={hasHeader} onChange={(e) => void onHeaderChange(e.target.checked)} /> First row is a header
            </label>

            <div className="q-tsection">CSV → SQL</div>
            <label className="q-tlabel">Table name</label>
            <input className="q-select" placeholder="table name" value={tableName} onChange={(e) => setTableName(e.target.value)} />

            <label className="q-tlabel">Columns — include · rename · type ({selectedCount}/{cols.length})</label>
            <div className="q-colcfg">
              {cols.map((c, i) => (
                <div className={"q-colrow" + (c.include ? "" : " q-colrow-off")} key={i}>
                  <input type="checkbox" checked={c.include} onChange={(e) => setCol(i, { include: e.target.checked })} title="Include this column" />
                  <input className="q-colname" value={c.name} onChange={(e) => setCol(i, { name: e.target.value })} placeholder={`col${i + 1}`} />
                  <select className="q-coltype" value={c.type} onChange={(e) => setCol(i, { type: e.target.value })} disabled={!includeCreate}>
                    {typeOptions(c.type).map((t) => (<option key={t} value={t}>{t}</option>))}
                  </select>
                </div>
              ))}
              {cols.length === 0 && <div className="q-thint">No columns detected.</div>}
            </div>

            <div className="q-trow">
              <label className="q-tlabel q-tlabel-inline">Insert mode</label>
              <select className="q-select" value={insertMode} onChange={(e) => setInsertMode(e.target.value)}>
                <option value="insert">INSERT INTO</option>
                <option value="ignore">INSERT IGNORE</option>
                <option value="replace">REPLACE INTO</option>
              </select>
            </div>
            <div className="q-trow">
              <label className="q-check"><input type="checkbox" checked={includeCreate} onChange={(e) => setIncludeCreate(e.target.checked)} /> CREATE TABLE</label>
              <label className="q-tlabel q-tlabel-inline">Batch</label>
              <input className="q-num" type="number" min={1} value={batchSize} onChange={(e) => setBatchSize(Math.max(1, Number(e.target.value) || 1))} />
            </div>
            <div className="q-trow">
              <label className="q-tlabel q-tlabel-inline">NULL =</label>
              <input className="q-select" placeholder="NULL,\\N (comma-separated)" value={nullValue} onChange={(e) => setNullValue(e.target.value)} />
            </div>
            <div className="q-trow">
              <label className="q-tlabel q-tlabel-inline">Bad bytes</label>
              <select className="q-select" value={onInvalid} onChange={(e) => setOnInvalid(e.target.value)}>
                <option value="fail">Fail on invalid byte</option>
                <option value="skip-row">Skip offending row</option>
                <option value="replace">Replace with �</option>
              </select>
            </div>
            <div className="q-trow">
              <button className="q-btn" disabled={busy} onClick={() => void doSqlPreview()}>Preview SQL</button>
              <button className="q-btn q-btn-primary" disabled={busy || selectedCount === 0} onClick={doConvert}>Convert → .sql</button>
            </div>
            {sqlPreviewText && <pre className="q-tpre">{sqlPreviewText}</pre>}

            <div className="q-tsection">Column transforms (new CSV)</div>
            {preview && preview.rows.length > 0 && (
              <div className="q-tprev">
                <table>
                  {preview.header.length > 0 && (<thead><tr>{preview.header.map((h, i) => <th key={i}>{h}</th>)}</tr></thead>)}
                  <tbody>{preview.rows.slice(0, 6).map((r, i) => (<tr key={i}>{r.map((cc, j) => <td key={j}>{cc}</td>)}</tr>))}</tbody>
                </table>
              </div>
            )}
            <div className="q-trow">
              <select className="q-select" value={dropCol} onChange={(e) => setDropCol(Number(e.target.value))}>
                {schema.map((c, i) => (<option key={i} value={i}>{c.name}</option>))}
              </select>
              <button className="q-btn" disabled={busy || schema.length === 0} onClick={doDrop}>Drop column</button>
            </div>
            <div className="q-trow">
              <input className="q-select" placeholder="constant value" value={addVal} onChange={(e) => setAddVal(e.target.value)} />
              <button className="q-btn" disabled={busy} onClick={doAdd}>Add column</button>
            </div>

            <div className="q-tsection">Redact / anonymize → new CSV</div>
            <div className="q-colcfg">
              {schema.map((c, i) => (
                <div className="q-colrow" key={i}>
                  <span className="q-colname" style={{ border: "none", background: "transparent" }}>{c.name}</span>
                  <select className="q-coltype" value={redactModes[i] ?? "off"} onChange={(e) => setRedactModes((p) => ({ ...p, [i]: e.target.value }))}>
                    <option value="off">keep</option>
                    <option value="null">blank</option>
                    <option value="fixed">fixed</option>
                    <option value="hash">hash</option>
                    <option value="email">email a***@…</option>
                  </select>
                </div>
              ))}
            </div>
            <div className="q-trow">
              <label className="q-tlabel q-tlabel-inline">fixed =</label>
              <input className="q-select" value={redactFixed} onChange={(e) => setRedactFixed(e.target.value)} />
              <button className="q-btn q-btn-primary" disabled={busy || redactCols.length === 0} onClick={doRedact}>Redact →</button>
            </div>

            <div className="q-tsection">Filter / dedupe / sample → new CSV</div>
            <div className="q-trow">
              <select className="q-select" value={filterCol} onChange={(e) => setFilterCol(Number(e.target.value))}>
                {schema.map((c, i) => (<option key={i} value={i}>{c.name}</option>))}
              </select>
              <select className="q-select" value={filterOp} onChange={(e) => setFilterOp(e.target.value)}>
                <option value="contains">contains</option>
                <option value="eq">equals</option>
                <option value="ne">not equals</option>
                <option value="gt">&gt; (number)</option>
                <option value="lt">&lt; (number)</option>
                <option value="empty">is empty</option>
                <option value="nonempty">is not empty</option>
              </select>
            </div>
            <div className="q-trow">
              {needsValue && <input className="q-select" placeholder="value" value={filterVal} onChange={(e) => setFilterVal(e.target.value)} />}
              <label className="q-check"><input type="checkbox" checked={filterNeg} onChange={(e) => setFilterNeg(e.target.checked)} /> invert</label>
              <button className="q-btn q-btn-primary" disabled={busy || schema.length === 0} onClick={doFilter}>Filter rows →</button>
            </div>
            <div className="q-trow">
              <label className="q-tlabel q-tlabel-inline">Dedupe by</label>
              <select className="q-select" value={dedupeKey} onChange={(e) => setDedupeKey(Number(e.target.value))}>
                <option value={-1}>whole row</option>
                {schema.map((c, i) => (<option key={i} value={i}>{c.name}</option>))}
              </select>
              <button className="q-btn" disabled={busy} onClick={doDedupe}>Dedupe →</button>
            </div>
            <div className="q-trow">
              <label className="q-tlabel q-tlabel-inline">Keep every</label>
              <input className="q-num" type="number" min={2} value={sampleEvery} onChange={(e) => setSampleEvery(Math.max(2, Number(e.target.value) || 2))} />
              <label className="q-tlabel q-tlabel-inline">th row</label>
              <button className="q-btn" disabled={busy} onClick={doSampleCsv}>Sample →</button>
            </div>

            <div className="q-tsection">Export → JSONL / SQLite / Excel</div>
            <div className="q-trow">
              <button className="q-btn" disabled={busy} onClick={doExportJsonl} title="Newline-delimited JSON, one object per row">JSONL</button>
              <button className="q-btn" disabled={busy} onClick={doExportSqlite} title="SQLite .db with one table">SQLite</button>
              <button className="q-btn" disabled={busy} onClick={doExportXlsx} title="Excel .xlsx (≤1,048,576 rows)">Excel</button>
            </div>
            <div className="q-trow">
              <button className="q-btn" disabled={busy} onClick={() => void doCopyMarkdown()} title="Copy the preview as a Markdown table">Copy preview as Markdown</button>
            </div>

            <div className="q-tsection">Profile (sampled)</div>
            <button className="q-btn" disabled={busy} onClick={() => void doProfile()}>Profile columns</button>
            {profile && (
              <div className="q-profile">
                <div className="q-thint">{profile.recordsScanned} rows scanned{profile.truncated ? " (sample)" : ""}{profile.raggedRows > 0 ? ` · ${profile.raggedRows} ragged rows` : ""}</div>
                {profile.columns.map((c, i) => {
                  const total = c.nonNull + c.null;
                  const nullPct = total > 0 ? Math.round((c.null / total) * 100) : 0;
                  return (
                    <div className="q-prof" key={i}>
                      <div className="q-prof-h"><span className="q-tcol-n">{c.name}</span><span className="q-tcol-t">{c.sqlType}</span></div>
                      <div className="q-thint">null {nullPct}% · distinct {c.distinct}{c.distinctCapped ? "+" : ""} · min {c.min || "—"} · max {c.max || "—"}</div>
                      {c.top.length > 0 && <div className="q-thint">top: {c.top.slice(0, 4).map((t) => `${t.value || "∅"}×${t.count}`).join(", ")}</div>}
                    </div>
                  );
                })}
              </div>
            )}
          </>
        ) : (
          <>
            <button className="q-btn q-btn-primary" disabled={busy} onClick={() => void doAnalyze()}>
              {busy ? "Analyzing…" : sqlSummary ? "Re-analyze dump" : "Analyze dump"}
            </button>
            {sqlSummary && (
              <>
                <div className="q-thint">
                  CREATE {sqlSummary.createTables} · INSERT {sqlSummary.insertTables} · DEFINER {sqlSummary.definerCount}
                </div>
                <div className="q-trow">
                  <button className="q-btn" disabled={busy} onClick={doSplit} title="Write one .sql file per table into a folder">Split by table → folder</button>
                  <button className="q-btn" disabled={busy} onClick={() => doSchema("")} title="Export the whole-dump schema (DDL only)">Whole schema</button>
                </div>
                <div className="q-trow">
                  <button className="q-btn q-btn-primary" disabled={busy} onClick={doSample} title="Small shareable dump: each table's DDL + first N rows">Dev fixture →</button>
                  <label className="q-tlabel q-tlabel-inline">rows/table</label>
                  <input className="q-num" type="number" min={1} value={sampleRows} onChange={(e) => setSampleRows(Math.max(1, Number(e.target.value) || 1))} />
                </div>
                <div className="q-trow">
                  <button className="q-btn" disabled={busy} onClick={() => void doLint()} title="Report dump issues (empty tables, DEFINER, mixed charsets, largest tables)">Lint dump</button>
                </div>
                {lint && (
                  <div className="q-profile">
                    {lint.findings.map((fdg, i) => (
                      <div className={"q-lint q-lint-" + fdg.severity} key={i}>
                        <span className="q-lint-t">{fdg.title}</span>
                        {fdg.detail && <span className="q-thint">{fdg.detail}</span>}
                      </div>
                    ))}
                  </div>
                )}
                <label className="q-tlabel">Tables ({sqlSummary.tables.length}) — extract / schema / data</label>
                <div className="q-ttables">
                  {sqlSummary.tables.map((t, i) => (
                    <div className="q-ttable" key={i}>
                      <span className="q-tcol-n" title={t.name}>{t.name}</span>
                      <span className="q-tcol-sz">{fmtBytes(t.bytes)}</span>
                      <span className="q-tbtns">
                        <button className="q-icon" disabled={busy} title="Extract whole table" onClick={() => doExtract(t.name)}>⤓</button>
                        <button className="q-icon" disabled={busy} title="Schema only (DDL)" onClick={() => doSchema(t.name)}>S</button>
                        <button className="q-icon" disabled={busy || t.insertOffset < 0} title="Data only (INSERTs)" onClick={() => doData(t.name)}>D</button>
                      </span>
                    </div>
                  ))}
                </div>
              </>
            )}

            <div className="q-tsection">Find / replace → new file</div>
            <input className="q-select" placeholder="find (e.g. `old_prefix_)" value={find} onChange={(e) => setFind(e.target.value)} />
            <input className="q-select" placeholder="replace with (e.g. `new_prefix_)" value={repl} onChange={(e) => setRepl(e.target.value)} />
            <div className="q-trow">
              <label className="q-check"><input type="checkbox" checked={regex} onChange={(e) => setRegex(e.target.checked)} /> regex</label>
              <label className="q-check"><input type="checkbox" checked={ci} onChange={(e) => setCi(e.target.checked)} /> ignore case</label>
              <button className="q-btn q-btn-primary" disabled={busy || !find} onClick={doReplace}>Replace → file</button>
            </div>

            <div className="q-tsection">Cleanup presets → new file</div>
            <select className="q-select" value={preset} onChange={(e) => setPreset(e.target.value)}>
              {presets.map((p) => (<option key={p} value={p}>{p}</option>))}
            </select>
            {presetArgLabels(preset).map((lbl, i) => (
              <input
                key={i}
                className="q-select"
                placeholder={lbl}
                value={pa[i]}
                onChange={(e) => setPa((prev) => { const c = [...prev]; c[i] = e.target.value; return c; })}
              />
            ))}
            <div className="q-trow">
              <button className="q-btn q-btn-primary" disabled={busy || !preset} onClick={doPreset}>Apply preset</button>
            </div>

            <div className="q-tsection">Reshape INSERTs → new file</div>
            <div className="q-trow">
              <select className="q-select" value={reshapeMode} onChange={(e) => setReshapeMode(e.target.value)}>
                <option value="single">Explode → one row per INSERT</option>
                <option value="multi">Batch → extended INSERTs</option>
              </select>
              {reshapeMode === "multi" && (
                <>
                  <label className="q-tlabel q-tlabel-inline">rows/INSERT</label>
                  <input className="q-num" type="number" min={1} value={reshapeBatch} onChange={(e) => setReshapeBatch(Math.max(1, Number(e.target.value) || 1))} />
                </>
              )}
            </div>
            <div className="q-trow">
              <button className="q-btn q-btn-primary" disabled={busy} onClick={doReshape}>Reshape → file</button>
              <span className="q-thint">{reshapeMode === "single" ? "one row per line — great for diffs" : "fewer, larger INSERTs — faster import"}</span>
            </div>

            <div className="q-tsection">Schema diff (vs another open dump)</div>
            {sqlOthers.length === 0 ? (
              <div className="q-thint">Open a second .sql dump to compare.</div>
            ) : (
              <>
                <div className="q-trow">
                  <select className="q-select" value={diffTarget} onChange={(e) => setDiffTarget(e.target.value)}>
                    <option value="">choose a dump…</option>
                    {sqlOthers.map((o) => (<option key={o.id} value={o.id}>{o.name}</option>))}
                  </select>
                  <button className="q-btn q-btn-primary" disabled={busy || !diffTarget} onClick={() => void doDiff()}>Diff schemas</button>
                </div>
                <div className="q-thint">Both dumps must be analyzed first (Analyze dump).</div>
                {diff && (
                  <div className="q-profile">
                    <div className="q-thint">{diff.fileA} → {diff.fileB} · {diff.unchangedCount} unchanged</div>
                    {diff.addedTables.length > 0 && <div className="q-lint q-lint-info"><span className="q-lint-t">+ {diff.addedTables.length} tables</span><span className="q-thint">{diff.addedTables.join(", ")}</span></div>}
                    {diff.removedTables.length > 0 && <div className="q-lint q-lint-warn"><span className="q-lint-t">− {diff.removedTables.length} tables</span><span className="q-thint">{diff.removedTables.join(", ")}</span></div>}
                    {diff.changedTables.map((t, i) => (
                      <div className="q-prof" key={i}>
                        <div className="q-prof-h"><span className="q-tcol-n">{t.name}</span></div>
                        {t.addedColumns.length > 0 && <div className="q-thint">+ {t.addedColumns.map((c) => c.name).join(", ")}</div>}
                        {t.removedColumns.length > 0 && <div className="q-thint">− {t.removedColumns.map((c) => c.name).join(", ")}</div>}
                        {t.changedColumns.map((c, j) => (<div className="q-thint" key={j}>~ {c.name}: {c.old} → {c.new}</div>))}
                      </div>
                    ))}
                    {diff.addedTables.length === 0 && diff.removedTables.length === 0 && diff.changedTables.length === 0 && (
                      <div className="q-thint">No structural differences.</div>
                    )}
                  </div>
                )}
              </>
            )}
          </>
        )}
      </div>
    </div>
  );
}
