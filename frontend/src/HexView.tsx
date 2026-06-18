import { useCallback, useEffect, useRef, useState } from "react";
import { FileService } from "../bindings/github.com/quarry/quarry-wails3";

interface HexLineData { offset: number; hex: string; ascii: string; }
interface HexWindowData { startByte: number; nextByte: number; lines: HexLineData[]; atBof: boolean; atEof: boolean; }

const HEX_BYTES = 64 * 1024;
const EDGE_PX = 240;

interface Props {
  fileId: string;
  onError: (s: string) => void;
}

export default function HexView({ fileId, onError }: Props) {
  const scrollRef = useRef<HTMLDivElement>(null);
  const [lines, setLines] = useState<HexLineData[]>([]);
  const win = useRef({ startByte: 0, nextByte: 0, atBof: true, atEof: false });
  const busy = useRef(false);

  const apply = useCallback((w: HexWindowData, anchor: "top" | "bottom") => {
    win.current = { startByte: w.startByte, nextByte: w.nextByte, atBof: w.atBof, atEof: w.atEof };
    setLines(w.lines);
    requestAnimationFrame(() => {
      const el = scrollRef.current;
      if (el) el.scrollTop = anchor === "bottom" ? el.scrollHeight : 0;
      setTimeout(() => { busy.current = false; }, 80);
    });
  }, []);

  const loadAt = useCallback(
    async (startByte: number, anchor: "top" | "bottom") => {
      try {
        const w = (await FileService.GetHexWindow(fileId, Math.max(0, startByte), HEX_BYTES)) as HexWindowData;
        apply(w, anchor);
      } catch (e: any) {
        onError(String(e?.message ?? e));
        busy.current = false;
      }
    },
    [fileId, apply, onError],
  );

  useEffect(() => {
    busy.current = true;
    void loadAt(0, "top");
  }, [fileId, loadAt]);

  const onScroll = () => {
    const el = scrollRef.current;
    if (!el || busy.current) return;
    if (el.scrollTop + el.clientHeight >= el.scrollHeight - EDGE_PX) {
      if (!win.current.atEof) { busy.current = true; void loadAt(win.current.nextByte, "top"); }
    } else if (el.scrollTop <= EDGE_PX) {
      if (!win.current.atBof) { busy.current = true; void loadAt(Math.max(0, win.current.startByte - HEX_BYTES), "bottom"); }
    }
  };

  return (
    <div className="q-hex" ref={scrollRef} onScroll={onScroll}>
      {lines.map((l, i) => (
        <div className="q-hex-row" key={i}>
          <span className="q-hex-off">{l.offset.toString(16).padStart(8, "0")}</span>
          <span className="q-hex-bytes">{l.hex}</span>
          <span className="q-hex-ascii">{l.ascii}</span>
        </div>
      ))}
    </div>
  );
}
