import { useCallback, useEffect, useRef, useState } from "react";
import { FileService } from "../bindings/github.com/quarry/quarry-wails3";

interface HexLineData { offset: number; hex: string; ascii: string; }
interface HexWindowData { startByte: number; nextByte: number; lines: HexLineData[]; atBof: boolean; atEof: boolean; }

const HEX_BYTES = 64 * 1024;
const EDGE_PX = 240;
export const MAX_HEX_WINDOW_LINES = HEX_BYTES / 16;
const MAX_HEX_TEXT_CODE_UNITS = 64;

function record(value: unknown): Record<string, unknown> {
  if (typeof value !== "object" || value == null || Array.isArray(value)) {
    throw new Error("invalid hex-window response");
  }
  return value as Record<string, unknown>;
}

function safeOffset(value: unknown, field: string): number {
  if (typeof value !== "number" || !Number.isSafeInteger(value) || value < 0) {
    throw new Error(`invalid hex-window ${field}`);
  }
  return value;
}

function boundedString(value: unknown, field: string, maximum: number): string {
  if (typeof value !== "string" || value.length > maximum) {
    throw new Error(`invalid or oversized hex-window ${field}`);
  }
  return value;
}

/** Reject malformed bridge payloads before they can amplify into thousands of DOM rows. */
export function normalizeHexWindow(value: unknown): HexWindowData {
  const payload = record(value);
  if (!Array.isArray(payload.lines) || payload.lines.length > MAX_HEX_WINDOW_LINES) {
    throw new Error(`hex-window response exceeds ${MAX_HEX_WINDOW_LINES} rows`);
  }
  const startByte = safeOffset(payload.startByte, "startByte");
  const nextByte = safeOffset(payload.nextByte, "nextByte");
  if (nextByte < startByte || typeof payload.atBof !== "boolean" || typeof payload.atEof !== "boolean") {
    throw new Error("invalid hex-window range or edge flags");
  }
  return {
    startByte,
    nextByte,
    atBof: payload.atBof,
    atEof: payload.atEof,
    lines: payload.lines.map((value, index) => {
      const line = record(value);
      return {
        offset: safeOffset(line.offset, `lines[${index}].offset`),
        hex: boundedString(line.hex, `lines[${index}].hex`, MAX_HEX_TEXT_CODE_UNITS),
        ascii: boundedString(line.ascii, `lines[${index}].ascii`, 16),
      };
    }),
  };
}

interface Props {
  fileId: string;
  onError: (s: string) => void;
}

export default function HexView({ fileId, onError }: Props) {
  const scrollRef = useRef<HTMLDivElement>(null);
  const [lines, setLines] = useState<HexLineData[]>([]);
  const win = useRef({ startByte: 0, nextByte: 0, atBof: true, atEof: false });
  const busy = useRef(false);
  const loadRequest = useRef(0);

  const apply = useCallback((w: HexWindowData, anchor: "top" | "bottom", request: number) => {
    if (request !== loadRequest.current) return;
    win.current = { startByte: w.startByte, nextByte: w.nextByte, atBof: w.atBof, atEof: w.atEof };
    setLines(w.lines);
    requestAnimationFrame(() => {
      if (request !== loadRequest.current) return;
      const el = scrollRef.current;
      if (el) el.scrollTop = anchor === "bottom" ? el.scrollHeight : 0;
      setTimeout(() => {
        if (request === loadRequest.current) busy.current = false;
      }, 80);
    });
  }, []);

  const loadAt = useCallback(
    async (startByte: number, anchor: "top" | "bottom") => {
      const request = ++loadRequest.current;
      try {
        const w = normalizeHexWindow(await FileService.GetHexWindow(fileId, Math.max(0, startByte), HEX_BYTES));
        apply(w, anchor, request);
      } catch (e: any) {
        if (request !== loadRequest.current) return;
        onError(String(e?.message ?? e));
        busy.current = false;
      }
    },
    [fileId, apply, onError],
  );

  useEffect(() => {
    busy.current = true;
    void loadAt(0, "top");
    return () => { loadRequest.current++; };
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

  const onWheel = (event: React.WheelEvent<HTMLDivElement>) => {
    const el = scrollRef.current;
    if (!el || busy.current) return;
    // A partial final window can fit entirely in the viewport. Wheeling then
    // produces no scroll event, so handle attempts to cross a pinned edge.
    if (event.deltaY < 0 && el.scrollTop <= 0 && !win.current.atBof) {
      busy.current = true;
      void loadAt(Math.max(0, win.current.startByte - HEX_BYTES), "bottom");
    } else if (event.deltaY > 0 && el.scrollTop + el.clientHeight >= el.scrollHeight - 1 && !win.current.atEof) {
      busy.current = true;
      void loadAt(win.current.nextByte, "top");
    }
  };

  return (
    <div className="q-hex" ref={scrollRef} onScroll={onScroll} onWheel={onWheel}>
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
