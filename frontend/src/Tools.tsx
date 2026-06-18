import { useEffect, useState } from "react";
import { FileService } from "../bindings/github.com/quarry/quarry-wails3";

interface CsvDelimiterOption { delimiter: string; name: string; columns: number; score: number; }
interface CsvInspectResult { delimiter: string; delimiterName: string; confidence: string; columns: number; hasHeader: boolean; candidates: CsvDelimiterOption[]; warnings: string[]; }
interface CsvColumn { name: string; sqlType: string; nonNull: number; null: number; samples: string[]; }
interface CsvSchemaResult { columns: CsvColumn[]; hasHeader: boolean; warnings: string[]; }
interface CsvPreviewResult { header: string[]; rows: string[][]; warnings: string[]; }
interface SqlTable { name: string; createOffset: number; insertOffset: number; }
interface SqlSummaryResult { tables: SqlTable[]; createTables: number; insertTables: number; definerCount: number; header: boolean; }
interface TransformResult { outputPath: string; recordsRead: number; recordsWritten: number; note: string; }

const DELIMS: { value: string; label: string }[] = [
  { value: ",", label: "Comma  ," },
  { value: "\t", label: "Tab  \\t" },
  { value: ";", label: "Semicolon  ;" },
  { value: "|", label: "Pipe  |" },
  { value: " ", label: "Space" },
];

function presetArgLabels(name: string): string[] {
  const n = name.toLowerCase();
  if (n.includes("database")) return ["old database name", "new database name"];
  if (n.includes("charset")) return ["old charset", "new charset", "old collation (opt)", "new collation (opt)"];
  return [];
}

interface Props {
  fileId: string;
  detected: string;
  onNotice: (s: string) => void;
  onError: (s: string) => void;
  onClose: () => void;
}

export default function Tools({ fileId, detected, onNotice, onError, onClose }: Props) {
  const d = detected.toLowerCase();
  const isCsv = d === "csv" || d === "tsv";

  // CSV state
  const [delim, setDelim] = useState(",");
  const [hasHeader, setHasHeader] = useState(true);
  const [inspect, setInspect] = useState<CsvInspectResult | null>(null);
  const [schema, setSchema] = useState<CsvColumn[]>([]);
  const [preview, setPreview] = useState<CsvPreviewResult | null>(null);
  const [dropCol, setDropCol] = useState(0);
  const [addVal, setAddVal] = useState("");
  const [tableName, setTableName] = useState("imported");
  const [includeCreate, setIncludeCreate] = useState(true);
  const [sqlPreviewText, setSqlPreviewText] = useState("");

  // SQL state
  const [sqlSummary, setSqlSummary] = useState<SqlSummaryResult | null>(null);
  const [find, setFind] = useState("");
  const [repl, setRepl] = useState("");
  const [regex, setRegex] = useState(false);
  const [ci, setCi] = useState(false);
  const [presets, setPresets] = useState<string[]>([]);
  const [preset, setPreset] = useState("");
  const [pa, setPa] = useState<string[]>(["", "", "", ""]);

  const [busy, setBusy] = useState(false);

  // Detect CSV delimiter when the panel opens for a CSV file.
  useEffect(() => {
    setInspect(null); setSchema([]); setPreview(null); setSqlSummary(null); setSqlPreviewText("");
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

  const refreshCsv = async (d: string, h: boolean) => {
    try {
      const sc = (await FileService.CsvSchema(fileId, d, h)) as CsvSchemaResult;
      setSchema(sc.columns);
      if (dropCol >= sc.columns.length) setDropCol(0);
      const pv = (await FileService.CsvPreview(fileId, d, h, 12)) as CsvPreviewResult;
      setPreview(pv);
    } catch (e: any) {
      onError(String(e?.message ?? e));
    }
  };

  const onDelimChange = async (d: string) => { setDelim(d); await refreshCsv(d, hasHeader); };
  const onHeaderChange = async (h: boolean) => { setHasHeader(h); await refreshCsv(delim, h); };

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

  const doDrop = () =>
    run(async () => {
      const keep = schema.map((_, i) => i).filter((i) => i !== dropCol);
      return (await FileService.CsvProjectViaDialog(fileId, delim, keep)) as TransformResult;
    });

  const doAdd = () =>
    run(async () => (await FileService.CsvAddColumnViaDialog(fileId, delim, schema.length, addVal)) as TransformResult);

  const doConvert = () =>
    run(async () => (await FileService.CsvToSQLViaDialog(fileId, delim, tableName, hasHeader, includeCreate)) as TransformResult);

  const doSqlPreview = async () => {
    try {
      const sql = (await FileService.CsvToSQLPreview(fileId, delim, tableName, hasHeader, includeCreate)) as string;
      setSqlPreviewText(sql);
    } catch (e: any) {
      onError(String(e?.message ?? e));
    }
  };

  const doAnalyze = async () => {
    setBusy(true);
    onNotice("Analyzing dump…");
    try {
      const s = (await FileService.SqlAnalyze(fileId)) as SqlSummaryResult;
      setSqlSummary(s);
      onNotice(`Found ${s.tables.length} tables`);
    } catch (e: any) {
      onError(String(e?.message ?? e));
    } finally {
      setBusy(false);
    }
  };

  const doExtract = (name: string) =>
    run(async () => (await FileService.SqlExtractTableViaDialog(fileId, name)) as TransformResult);

  const doReplace = () =>
    run(async () => (await FileService.SqlReplaceViaDialog(fileId, find, repl, regex, ci, false)) as TransformResult);

  const doPreset = () =>
    run(async () => (await FileService.SqlApplyPresetViaDialog(fileId, preset, pa[0], pa[1], pa[2], pa[3])) as TransformResult);

  // ensure detected delimiter appears in the dropdown
  const delims = DELIMS.some((d) => d.value === delim) ? DELIMS : [{ value: delim, label: `Detected (${JSON.stringify(delim)})` }, ...DELIMS];

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
                {delims.map((d) => (<option key={d.value} value={d.value}>{d.label}</option>))}
              </select>
              {inspect && (
                <span className="q-thint">
                  detected {inspect.delimiterName} ({inspect.confidence}), {inspect.columns} cols
                </span>
              )}
            </div>
            <label className="q-check">
              <input type="checkbox" checked={hasHeader} onChange={(e) => void onHeaderChange(e.target.checked)} /> First row is a header
            </label>

            <label className="q-tlabel">Columns ({schema.length})</label>
            <div className="q-tcols">
              {schema.map((c, i) => (
                <div className="q-tcol" key={i}><span className="q-tcol-n">{c.name}</span><span className="q-tcol-t">{c.sqlType}</span></div>
              ))}
            </div>

            {preview && preview.rows.length > 0 && (
              <>
                <label className="q-tlabel">Preview</label>
                <div className="q-tprev">
                  <table>
                    {preview.header.length > 0 && (<thead><tr>{preview.header.map((h, i) => <th key={i}>{h}</th>)}</tr></thead>)}
                    <tbody>{preview.rows.slice(0, 8).map((r, i) => (<tr key={i}>{r.map((c, j) => <td key={j}>{c}</td>)}</tr>))}</tbody>
                  </table>
                </div>
              </>
            )}

            <label className="q-tlabel">Transforms (write a new file)</label>
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
            <div className="q-trow">
              <input className="q-select" placeholder="table name" value={tableName} onChange={(e) => setTableName(e.target.value)} />
              <label className="q-check"><input type="checkbox" checked={includeCreate} onChange={(e) => setIncludeCreate(e.target.checked)} /> CREATE</label>
            </div>
            <div className="q-trow">
              <button className="q-btn" disabled={busy} onClick={() => void doSqlPreview()}>Preview SQL</button>
              <button className="q-btn q-btn-primary" disabled={busy} onClick={doConvert}>Convert → .sql</button>
            </div>
            {sqlPreviewText && <pre className="q-tpre">{sqlPreviewText}</pre>}
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
                <label className="q-tlabel">Tables ({sqlSummary.tables.length})</label>
                <div className="q-ttables">
                  {sqlSummary.tables.map((t, i) => (
                    <div className="q-ttable" key={i}>
                      <span className="q-tcol-n">{t.name}</span>
                      <span className="q-tcol-t">0x{t.createOffset.toString(16)}</span>
                      <button className="q-icon" disabled={busy} onClick={() => doExtract(t.name)}>Extract</button>
                    </div>
                  ))}
                </div>
              </>
            )}
            <label className="q-tlabel">Find / replace → new file</label>
            <input className="q-select" placeholder="find (e.g. `old_prefix_)" value={find} onChange={(e) => setFind(e.target.value)} />
            <input className="q-select" placeholder="replace with (e.g. `new_prefix_)" value={repl} onChange={(e) => setRepl(e.target.value)} />
            <div className="q-trow">
              <label className="q-check"><input type="checkbox" checked={regex} onChange={(e) => setRegex(e.target.checked)} /> regex</label>
              <label className="q-check"><input type="checkbox" checked={ci} onChange={(e) => setCi(e.target.checked)} /> ignore case</label>
              <button className="q-btn q-btn-primary" disabled={busy || !find} onClick={doReplace}>Replace → file</button>
            </div>

            <label className="q-tlabel">Cleanup presets → new file</label>
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
          </>
        )}
      </div>
    </div>
  );
}
