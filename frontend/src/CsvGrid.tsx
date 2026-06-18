import { useCallback, useEffect, useRef, useState } from "react";
import { FileService } from "../bindings/github.com/quarry/quarry-wails3";

interface CsvInspectResult { delimiter: string; hasHeader: boolean; columns: number; }
interface CsvColumn { name: string; sqlType: string; }
interface CsvSchemaResult { columns: CsvColumn[]; }
interface CsvGridResult {
  startByte: number;
  nextByte: number;
  startRow: number;
  rows: string[][];
  columns: number;
  atBof: boolean;
  atEof: boolean;
}

const GRID_BYTES = 128 * 1024;
const EDGE_PX = 240;

interface Props {
  fileId: string;
  onError: (s: string) => void;
}

export default function CsvGrid({ fileId, onError }: Props) {
  const scrollRef = useRef<HTMLDivElement>(null);
  const [delim, setDelim] = useState(",");
  const [cols, setCols] = useState<string[]>([]);
  const [rows, setRows] = useState<string[][]>([]);
  const [startRow, setStartRow] = useState(1);
  const win = useRef({ startByte: 0, nextByte: 0, atBof: true, atEof: false });
  const busy = useRef(false);

  const apply = useCallback((g: CsvGridResult, anchor: "top" | "bottom") => {
    win.current = { startByte: g.startByte, nextByte: g.nextByte, atBof: g.atBof, atEof: g.atEof };
    setRows(g.rows);
    setStartRow(g.startRow > 0 ? g.startRow : 1);
    setCols((prev) => {
      const need = Math.max(prev.length, g.columns);
      if (prev.length >= need) return prev;
      const next = prev.slice();
      for (let i = prev.length; i < need; i++) next.push(`col ${i + 1}`);
      return next;
    });
    requestAnimationFrame(() => {
      const el = scrollRef.current;
      if (el) el.scrollTop = anchor === "bottom" ? el.scrollHeight : 0;
      setTimeout(() => { busy.current = false; }, 80);
    });
  }, []);

  const loadAt = useCallback(
    async (startByte: number, anchor: "top" | "bottom", d: string) => {
      try {
        const g = (await FileService.GetCsvGrid(fileId, d, Math.max(0, startByte), GRID_BYTES)) as CsvGridResult;
        apply(g, anchor);
      } catch (e: any) {
        onError(String(e?.message ?? e));
        busy.current = false;
      }
    },
    [fileId, apply, onError],
  );

  // initial: detect delimiter + column names, then load the first window
  useEffect(() => {
    let cancelled = false;
    busy.current = true;
    (async () => {
      try {
        const ins = (await FileService.CsvInspect(fileId)) as CsvInspectResult;
        if (cancelled) return;
        const d = ins.delimiter || ",";
        setDelim(d);
        try {
          const sc = (await FileService.CsvSchema(fileId, d, ins.hasHeader)) as CsvSchemaResult;
          if (!cancelled) setCols(sc.columns.map((c) => c.name));
        } catch { /* fall back to generic labels */ }
        await loadAt(0, "top", d);
      } catch (e: any) {
        if (!cancelled) {
          onError(String(e?.message ?? e));
          busy.current = false;
        }
      }
    })();
    return () => { cancelled = true; };
  }, [fileId, loadAt, onError]);

  const onScroll = () => {
    const el = scrollRef.current;
    if (!el || busy.current) return;
    if (el.scrollTop + el.clientHeight >= el.scrollHeight - EDGE_PX) {
      if (!win.current.atEof) { busy.current = true; void loadAt(win.current.nextByte, "top", delim); }
    } else if (el.scrollTop <= EDGE_PX) {
      if (!win.current.atBof) { busy.current = true; void loadAt(Math.max(0, win.current.startByte - GRID_BYTES), "bottom", delim); }
    }
  };

  const colCount = Math.max(cols.length, ...rows.map((r) => r.length), 1);
  const headers = Array.from({ length: colCount }, (_, i) => cols[i] ?? `col ${i + 1}`);

  return (
    <div className="q-grid" ref={scrollRef} onScroll={onScroll}>
      <table className="q-grid-table">
        <thead>
          <tr>
            <th className="q-grid-rownum">#</th>
            {headers.map((h, i) => (<th key={i}>{h}</th>))}
          </tr>
        </thead>
        <tbody>
          {rows.map((r, i) => (
            <tr key={i}>
              <td className="q-grid-rownum">{startRow + i}</td>
              {headers.map((_, c) => (<td key={c}>{r[c] ?? ""}</td>))}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
