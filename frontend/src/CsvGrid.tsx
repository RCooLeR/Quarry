import { useCallback, useEffect, useRef, useState } from "react";
import { FileService } from "../bindings/github.com/quarry/quarry-wails3";

interface CsvInspectResult { delimiter: string; hasHeader: boolean; columns: number; }
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
  const [useHeader, setUseHeader] = useState(false);
  const [headerCells, setHeaderCells] = useState<string[]>([]);
  const [rows, setRows] = useState<string[][]>([]);
  const [startRow, setStartRow] = useState(1);
  const win = useRef({ startByte: 0, nextByte: 0, atBof: true, atEof: false });
  const busy = useRef(false);
  const headerRef = useRef(false); // current useHeader for async callbacks

  const apply = useCallback((g: CsvGridResult, anchor: "top" | "bottom") => {
    win.current = { startByte: g.startByte, nextByte: g.nextByte, atBof: g.atBof, atEof: g.atEof };
    let dataRows = g.rows;
    let base = g.startRow > 0 ? g.startRow : 1;
    if (headerRef.current && g.startByte === 0 && g.rows.length > 0) {
      setHeaderCells(g.rows[0]);
      dataRows = g.rows.slice(1);
      base += 1;
    }
    setRows(dataRows);
    setStartRow(base);
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

  useEffect(() => {
    let cancelled = false;
    busy.current = true;
    (async () => {
      try {
        const ins = (await FileService.CsvInspect(fileId)) as CsvInspectResult;
        if (cancelled) return;
        const d = ins.delimiter || ",";
        setDelim(d);
        setUseHeader(ins.hasHeader);
        headerRef.current = ins.hasHeader;
        await loadAt(0, "top", d);
      } catch (e: any) {
        if (!cancelled) { onError(String(e?.message ?? e)); busy.current = false; }
      }
    })();
    return () => { cancelled = true; };
  }, [fileId, loadAt, onError]);

  const toggleHeader = (on: boolean) => {
    setUseHeader(on);
    headerRef.current = on;
    if (!on) setHeaderCells([]);
    busy.current = true;
    void loadAt(0, "top", delim); // re-derive from the top
  };

  const onScroll = () => {
    const el = scrollRef.current;
    if (!el || busy.current) return;
    if (el.scrollTop + el.clientHeight >= el.scrollHeight - EDGE_PX) {
      if (!win.current.atEof) { busy.current = true; void loadAt(win.current.nextByte, "top", delim); }
    } else if (el.scrollTop <= EDGE_PX) {
      if (!win.current.atBof) { busy.current = true; void loadAt(Math.max(0, win.current.startByte - GRID_BYTES), "bottom", delim); }
    }
  };

  const colCount = Math.max(headerCells.length, ...rows.map((r) => r.length), 1);
  const headers = Array.from({ length: colCount }, (_, i) => (useHeader && headerCells[i] != null ? headerCells[i] : `col ${i + 1}`));

  return (
    <div className="q-gridwrap">
      <div className="q-gridbar">
        <label className="q-check">
          <input type="checkbox" checked={useHeader} onChange={(e) => toggleHeader(e.target.checked)} /> First row is a header
        </label>
      </div>
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
    </div>
  );
}
