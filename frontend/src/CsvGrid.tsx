import { useCallback, useEffect, useRef, useState } from "react";
import { FileService } from "../bindings/github.com/quarry/quarry-wails3";
import { normalizeCsvGrid, normalizeCsvInspect } from "./bridgePayloads";
import type { NormalizedCsvGridResult } from "./bridgePayloads";
import { detectedCsvDialect, fallbackCsvDialect, overrideCsvDialect } from "./csvDialect";
import type { CsvDialect } from "./csvDialect";

const GRID_BYTES = 128 * 1024;
const EDGE_PX = 240;
const CELL_PREVIEW_CODE_UNITS = 256;
const GRID_ROW_HEIGHT = 29;
const GRID_COLUMN_WIDTH = 180;
const GRID_ROW_NUMBER_WIDTH = 72;
const GRID_ROW_OVERSCAN = 4;
const GRID_COLUMN_OVERSCAN = 2;
export const MAX_RENDERED_GRID_ROWS = 64;
export const MAX_RENDERED_GRID_COLUMNS = 24;
// A grid response is already capped at GRID_BYTES. Keep the inspector bounded
// even if a malformed or future backend response violates that contract.
const CELL_INSPECT_CODE_UNITS = GRID_BYTES;

export interface CsvGridRenderWindow {
  rowStart: number;
  rowEnd: number;
  columnStart: number;
  columnEnd: number;
}

interface CsvGridViewport {
  scrollTop: number;
  scrollLeft: number;
  width: number;
  height: number;
}

function boundedWindow(
  count: number,
  scroll: number,
  viewport: number,
  itemSize: number,
  overscan: number,
  maximum: number,
): [number, number] {
  if (!Number.isFinite(count) || count <= 0) return [0, 0];
  const safeCount = Math.floor(count);
  const safeScroll = Number.isFinite(scroll) ? Math.max(0, scroll) : 0;
  const safeViewport = Number.isFinite(viewport) ? Math.max(itemSize, viewport) : itemSize;
  const visible = Math.max(1, Math.ceil(safeViewport / itemSize));
  const length = Math.min(safeCount, maximum, visible + overscan * 2);
  const firstVisible = Math.floor(safeScroll / itemSize);
  const start = Math.min(Math.max(0, firstVisible - overscan), Math.max(0, safeCount - length));
  return [start, Math.min(safeCount, start + length)];
}

/**
 * Computes the only rows and columns that may be materialised in the DOM.
 * Backend windows remain independently bounded; these caps protect the WebView
 * from rectangular amplification of an otherwise small CSV sample.
 */
export function csvGridRenderWindow(
  rowCount: number,
  columnCount: number,
  viewport: CsvGridViewport,
): CsvGridRenderWindow {
  const [rowStart, rowEnd] = boundedWindow(
    rowCount,
    viewport.scrollTop,
    viewport.height,
    GRID_ROW_HEIGHT,
    GRID_ROW_OVERSCAN,
    MAX_RENDERED_GRID_ROWS,
  );
  const [columnStart, columnEnd] = boundedWindow(
    columnCount,
    viewport.scrollLeft,
    viewport.width,
    GRID_COLUMN_WIDTH,
    GRID_COLUMN_OVERSCAN,
    MAX_RENDERED_GRID_COLUMNS,
  );
  return { rowStart, rowEnd, columnStart, columnEnd };
}

export interface CsvGridFocusCell {
  /** Zero is the header row; data rows are one-based within the current sample. */
  row: number;
  /** Zero-based data-column index. The noninteractive row-number column is excluded. */
  column: number;
}

export type CsvGridFocusNavigationKey =
  | "ArrowLeft"
  | "ArrowRight"
  | "ArrowUp"
  | "ArrowDown"
  | "Home"
  | "End"
  | "PageUp"
  | "PageDown";

function boundedInteger(value: number, minimum: number, maximum: number): number {
  const integer = Number.isFinite(value) ? Math.trunc(value) : minimum;
  return Math.min(maximum, Math.max(minimum, integer));
}

/**
 * Applies the WAI-ARIA grid keyboard model inside one bounded backend sample.
 * Page movement is capped by the caller-provided visible-row count and never
 * requests another file window; the explicit sample controls retain ownership
 * of parser-cursor navigation.
 */
export function csvGridFocusDestination(
  current: CsvGridFocusCell,
  key: string,
  dataRowCount: number,
  columnCount: number,
  pageRows: number,
  wholeGrid: boolean,
): CsvGridFocusCell | null {
  const lastRow = Math.max(0, Number.isFinite(dataRowCount) ? Math.floor(dataRowCount) : 0);
  const lastColumn = Math.max(0, (Number.isFinite(columnCount) ? Math.floor(columnCount) : 1) - 1);
  const row = boundedInteger(current.row, 0, lastRow);
  const column = boundedInteger(current.column, 0, lastColumn);
  const page = boundedInteger(pageRows, 1, Math.max(1, lastRow));

  switch (key as CsvGridFocusNavigationKey) {
    case "ArrowLeft":
      return { row, column: Math.max(0, column - 1) };
    case "ArrowRight":
      return { row, column: Math.min(lastColumn, column + 1) };
    case "ArrowUp":
      return { row: Math.max(0, row - 1), column };
    case "ArrowDown":
      return { row: Math.min(lastRow, row + 1), column };
    case "Home":
      return wholeGrid ? { row: 0, column: 0 } : { row, column: 0 };
    case "End":
      return wholeGrid ? { row: lastRow, column: lastColumn } : { row, column: lastColumn };
    case "PageUp":
      return { row: Math.max(0, row - page), column };
    case "PageDown":
      return { row: Math.min(lastRow, row + page), column };
    default:
      return null;
  }
}

interface InspectedCell {
  rowLabel: string;
  column: number;
  value: string;
  truncated: boolean;
}

export type CsvGridNavigation = "reset" | "forward" | "back";

export const MAX_CSV_GRID_CURSOR_HISTORY = 4096;

/**
 * Fixed-capacity stack of parser-confirmed CSV window starts.
 *
 * `head` identifies the oldest retained cursor and `size` identifies the
 * active stack depth. Entries beyond `size` are intentionally ignored after
 * back-navigation, so every operation remains O(1) and storage never grows
 * with the source file.
 */
export interface CsvGridCursorHistory {
  values: number[];
  head: number;
  size: number;
  truncated: boolean;
}

export function createCsvGridCursorHistory(): CsvGridCursorHistory {
  return { values: [], head: 0, size: 0, truncated: false };
}

export function updateCsvGridCursorHistory(
  history: CsvGridCursorHistory,
  navigation: CsvGridNavigation,
  currentStart: number,
): CsvGridCursorHistory {
  if (navigation === "reset") {
    history.values.length = 0;
    history.head = 0;
    history.size = 0;
    history.truncated = false;
    return history;
  }

  if (navigation === "back") {
    if (history.size > 0) history.size--;
    return history;
  }

  if (history.size < MAX_CSV_GRID_CURSOR_HISTORY) {
    const tail = (history.head + history.size) % MAX_CSV_GRID_CURSOR_HISTORY;
    history.values[tail] = currentStart;
    history.size++;
    return history;
  }

  history.values[history.head] = currentStart;
  history.head = (history.head + 1) % MAX_CSV_GRID_CURSOR_HISTORY;
  history.truncated = true;
  return history;
}

export function previousCsvGridCursor(history: CsvGridCursorHistory): number | undefined {
  if (history.size === 0) return undefined;
  const tail = (history.head + history.size - 1) % MAX_CSV_GRID_CURSOR_HISTORY;
  return history.values[tail];
}

export function csvGridCursorHistoryExhausted(history: CsvGridCursorHistory, atBof: boolean): boolean {
  return !atBof && history.truncated && history.size === 0;
}

function prefixWithoutSplitSurrogate(value: string, limit: number): string {
  if (value.length <= limit) return value;
  let end = limit;
  const last = value.charCodeAt(end - 1);
  if (last >= 0xd800 && last <= 0xdbff) end--;
  return value.slice(0, end);
}

function cellPreview(value: string): string {
  if (value.length <= CELL_PREVIEW_CODE_UNITS) return value;
  return `${prefixWithoutSplitSurrogate(value, CELL_PREVIEW_CODE_UNITS)}…`;
}

function cellTitle(value: string): string {
  return value.length <= CELL_PREVIEW_CODE_UNITS
    ? value
    : "Cell preview is truncated. Activate the cell to inspect its bounded value.";
}

function boundedCell(value: string): Pick<InspectedCell, "value" | "truncated"> {
  return {
    value: prefixWithoutSplitSurrogate(value, CELL_INSPECT_CODE_UNITS),
    truncated: value.length > CELL_INSPECT_CODE_UNITS,
  };
}

interface Props {
  fileId: string;
  detected?: string;
  dialect?: CsvDialect;
  onDialectChange?: (dialect: CsvDialect) => void;
  onError: (s: string) => void;
}

export default function CsvGrid({ fileId, detected = "csv", dialect, onDialectChange, onError }: Props) {
  const scrollRef = useRef<HTMLDivElement>(null);
  const inspectorRef = useRef<HTMLTextAreaElement>(null);
  const initialDialect = dialect ?? fallbackCsvDialect(detected);
  const [delim, setDelim] = useState(initialDialect.delimiter);
  const [useHeader, setUseHeader] = useState(initialDialect.hasHeader);
  const [headerCells, setHeaderCells] = useState<string[]>([]);
  const [rows, setRows] = useState<string[][]>([]);
  const [startRow, setStartRow] = useState(1);
  const [activeCell, setActiveCell] = useState<CsvGridFocusCell>({ row: 1, column: 0 });
  const [inspected, setInspected] = useState<InspectedCell | null>(null);
  const [viewport, setViewport] = useState<CsvGridViewport>({
    scrollTop: 0,
    scrollLeft: 0,
    width: 1024,
    height: 600,
  });
  const win = useRef({ startByte: 0, nextByte: 0, atBof: true, atEof: false });
  // CSV cursors are parser-confirmed logical-record boundaries. Keep the exact
  // forward history instead of deriving a previous page by byte subtraction,
  // which can land inside a quoted multiline record.
  const previousStarts = useRef<CsvGridCursorHistory>(createCsvGridCursorHistory());
  const lastScrollTop = useRef(0);
  const pendingGridFocus = useRef(false);
  const busy = useRef(false);
  const headerRef = useRef(false); // current useHeader for async callbacks
  const loadRequest = useRef(0);
  const sourceGeneration = useRef<number | null>(null);
  const dialectChangeRef = useRef(onDialectChange);
  dialectChangeRef.current = onDialectChange;

  const syncViewport = useCallback((element: HTMLDivElement) => {
    const next: CsvGridViewport = {
      scrollTop: Math.max(0, element.scrollTop),
      scrollLeft: Math.max(0, element.scrollLeft),
      width: element.clientWidth > 0 ? element.clientWidth : 1024,
      height: element.clientHeight > 0 ? element.clientHeight : 600,
    };
    setViewport((current) => (
      current.scrollTop === next.scrollTop
      && current.scrollLeft === next.scrollLeft
      && current.width === next.width
      && current.height === next.height
        ? current
        : next
    ));
  }, []);

  const apply = useCallback((g: NormalizedCsvGridResult, anchor: "top" | "bottom") => {
    setInspected(null);
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
    setActiveCell({ row: dataRows.length > 0 ? 1 : 0, column: 0 });
    // The request has completed. Programmatic scroll events remain harmless
    // because lastScrollTop is updated before the element is repositioned.
    busy.current = false;
    requestAnimationFrame(() => {
      const el = scrollRef.current;
      if (el) {
        el.scrollTop = anchor === "bottom" ? el.scrollHeight : 0;
        lastScrollTop.current = el.scrollTop;
        syncViewport(el);
      }
    });
  }, [syncViewport]);

  useEffect(() => {
    const element = scrollRef.current;
    if (!element) return;
    const measure = () => syncViewport(element);
    measure();
    window.addEventListener("resize", measure);
    const observer = typeof ResizeObserver === "undefined" ? null : new ResizeObserver(measure);
    observer?.observe(element);
    return () => {
      observer?.disconnect();
      window.removeEventListener("resize", measure);
    };
  }, [syncViewport]);

  useEffect(() => {
    if (!inspected) return;
    inspectorRef.current?.focus();
    inspectorRef.current?.select();
  }, [inspected]);

  const inspectCell = (
    cell: CsvGridFocusCell,
    rowLabel: string,
    column: number,
    value: string,
  ) => {
    setActiveCell(cell);
    setInspected({ rowLabel, column, ...boundedCell(value) });
  };

  const copyInspected = async () => {
    if (!inspected) return;
    if (!navigator.clipboard?.writeText) {
      onError("Clipboard access is unavailable. Select the cell value and copy it with the keyboard.");
      return;
    }
    try {
      await navigator.clipboard.writeText(inspected.value);
    } catch (error: unknown) {
      const message = error instanceof Error ? error.message : String(error);
      onError(`Could not copy the inspected cell: ${message}`);
    }
  };

  const loadAt = useCallback(
    async (
      startByte: number,
      anchor: "top" | "bottom",
      d: string,
      navigation: CsvGridNavigation = "reset",
      expectedGeneration?: number,
    ) => {
      const request = ++loadRequest.current;
      const requestFileId = fileId;
      try {
        const g = normalizeCsvGrid(await FileService.GetCsvGrid(fileId, d, Math.max(0, startByte), GRID_BYTES));
        if (request !== loadRequest.current) return;
        if (g.fileId !== "" && g.fileId !== requestFileId) {
          throw new Error(`CSV grid response file mismatch: expected ${requestFileId}, received ${g.fileId}`);
        }
        if (!Number.isSafeInteger(g.generation) || g.generation <= 0) {
          throw new Error("CSV grid response is missing a valid source generation. Reload the grid.");
        }
        if (
          (expectedGeneration != null && expectedGeneration !== g.generation)
          || (sourceGeneration.current != null && sourceGeneration.current !== g.generation)
        ) {
          onError("CSV grid response belongs to a stale source generation. Reload the grid from the beginning.");
          busy.current = false;
          return;
        }
        sourceGeneration.current = g.generation;
        updateCsvGridCursorHistory(previousStarts.current, navigation, win.current.startByte);
        apply(g, anchor);
      } catch (e: any) {
        if (request !== loadRequest.current) return;
        onError(String(e?.message ?? e));
        busy.current = false;
      }
    },
    [fileId, apply, onError],
  );

  useEffect(() => {
    let cancelled = false;
    loadRequest.current++;
    sourceGeneration.current = null;
    busy.current = true;
    (async () => {
      try {
        if (dialect) {
          setDelim(dialect.delimiter);
          setUseHeader(dialect.hasHeader);
          headerRef.current = dialect.hasHeader;
          await loadAt(0, "top", dialect.delimiter);
          return;
        }
        const ins = normalizeCsvInspect(await FileService.CsvInspect(fileId));
        if (cancelled) return;
        if (!Number.isSafeInteger(ins.generation) || ins.generation <= 0) {
          throw new Error("CSV inspection is missing a valid source generation. Reload the grid.");
        }
        const discovered = detectedCsvDialect(detected, ins.delimiter, ins.hasHeader);
        setDelim(discovered.delimiter);
        setUseHeader(discovered.hasHeader);
        headerRef.current = discovered.hasHeader;
        dialectChangeRef.current?.(discovered);
        await loadAt(0, "top", discovered.delimiter, "reset", ins.generation);
      } catch (e: any) {
        if (!cancelled) { onError(String(e?.message ?? e)); busy.current = false; }
      }
    })();
    return () => { cancelled = true; loadRequest.current++; };
  }, [fileId, detected, dialect?.delimiter, dialect?.hasHeader, loadAt, onError]);

  const toggleHeader = (on: boolean) => {
    setUseHeader(on);
    headerRef.current = on;
    dialectChangeRef.current?.(overrideCsvDialect(delim, on));
    if (!on) setHeaderCells([]);
    busy.current = true;
    void loadAt(0, "top", delim); // re-derive from the top
  };

  const onScroll = () => {
    const el = scrollRef.current;
    if (!el) return;
    syncViewport(el);
    const previousTop = lastScrollTop.current;
    const currentTop = el.scrollTop;
    lastScrollTop.current = currentTop;
    if (busy.current || currentTop === previousTop) return;
    if (currentTop > previousTop && currentTop + el.clientHeight >= el.scrollHeight - EDGE_PX) {
      if (!win.current.atEof) { busy.current = true; void loadAt(win.current.nextByte, "top", delim, "forward"); }
    } else if (currentTop < previousTop && currentTop <= EDGE_PX) {
      const previous = previousCsvGridCursor(previousStarts.current);
      if (!win.current.atBof && previous != null) { busy.current = true; void loadAt(previous, "bottom", delim, "back"); }
    }
  };

  const loadPreviousWindow = () => {
    if (busy.current) return;
    const previous = previousCsvGridCursor(previousStarts.current);
    if (win.current.atBof || previous == null) return;
    busy.current = true;
    void loadAt(previous, "bottom", delim, "back");
  };

  const loadFirstWindow = () => {
    if (busy.current || win.current.atBof) return;
    busy.current = true;
    void loadAt(0, "top", delim, "reset");
  };

  const loadNextWindow = () => {
    if (busy.current || win.current.atEof) return;
    busy.current = true;
    void loadAt(win.current.nextByte, "top", delim, "forward");
  };

  const colCount = Math.max(headerCells.length, ...rows.map((r) => r.length), 1);
  const headers = Array.from({ length: colCount }, (_, i) => (useHeader && headerCells[i] != null ? headerCells[i] : `col ${i + 1}`));
  const renderWindow = csvGridRenderWindow(rows.length, colCount, viewport);
  const visibleHeaders = headers.slice(renderWindow.columnStart, renderWindow.columnEnd);
  const visibleRows = rows.slice(renderWindow.rowStart, renderWindow.rowEnd);
  const leftColumns = renderWindow.columnStart;
  const rightColumns = colCount - renderWindow.columnEnd;
  const topRows = renderWindow.rowStart;
  const bottomRows = rows.length - renderWindow.rowEnd;
  const earlierHistoryUnavailable = csvGridCursorHistoryExhausted(previousStarts.current, win.current.atBof);
  const visibleRowLabel = rows.length === 0
    ? "No data rows in this bounded sample"
    : `Rows ${startRow + renderWindow.rowStart}–${startRow + renderWindow.rowEnd - 1} of ${rows.length}`;
  const visibleColumnLabel = `columns ${renderWindow.columnStart + 1}–${renderWindow.columnEnd} of ${colCount}`;

  // Manual scrolling may virtualize the formerly active cell out of the DOM.
  // In that case, keep one (and only one) rendered cell in the page tab order;
  // focusing it synchronizes activeCell through its onFocus handler.
  const rovingColumn = boundedInteger(
    activeCell.column,
    renderWindow.columnStart,
    Math.max(renderWindow.columnStart, renderWindow.columnEnd - 1),
  );
  const rovingRow = rows.length === 0
    ? 0
    : activeCell.row <= 0
      ? 0
      : boundedInteger(activeCell.row, renderWindow.rowStart + 1, renderWindow.rowEnd);
  const rovingCell: CsvGridFocusCell = { row: rovingRow, column: rovingColumn };
  const pageRows = boundedInteger(
    Math.floor(viewport.height / GRID_ROW_HEIGHT),
    1,
    MAX_RENDERED_GRID_ROWS,
  );

  const revealGridCell = useCallback((cell: CsvGridFocusCell) => {
    const element = scrollRef.current;
    if (!element) return;

    const viewHeight = element.clientHeight > 0 ? element.clientHeight : viewport.height;
    const viewWidth = element.clientWidth > 0 ? element.clientWidth : viewport.width;
    let nextTop = element.scrollTop;
    let nextLeft = element.scrollLeft;

    if (cell.row === 0) {
      nextTop = 0;
    } else {
      const cellTop = (cell.row - 1) * GRID_ROW_HEIGHT;
      const cellBottom = cellTop + GRID_ROW_HEIGHT;
      if (cellTop < nextTop) nextTop = cellTop;
      else if (cellBottom > nextTop + viewHeight) nextTop = Math.max(0, cellBottom - viewHeight);
    }

    const cellLeft = GRID_ROW_NUMBER_WIDTH + cell.column * GRID_COLUMN_WIDTH;
    const cellRight = cellLeft + GRID_COLUMN_WIDTH;
    if (cellLeft < nextLeft + GRID_ROW_NUMBER_WIDTH) {
      nextLeft = Math.max(0, cellLeft - GRID_ROW_NUMBER_WIDTH);
    } else if (cellRight > nextLeft + viewWidth) {
      nextLeft = Math.max(0, cellRight - viewWidth);
    }

    element.scrollTop = nextTop;
    element.scrollLeft = nextLeft;
    // Do not let this focus-management scroll masquerade as a request for the
    // next or previous parser-cursor window.
    lastScrollTop.current = nextTop;
    syncViewport(element);
  }, [syncViewport, viewport.height, viewport.width]);

  const handleGridCellKeyDown = (
    event: React.KeyboardEvent<HTMLTableCellElement>,
    cell: CsvGridFocusCell,
    rowLabel: string,
    column: number,
    value: string,
  ) => {
    if (event.key === "Enter" || event.key === " ") {
      event.preventDefault();
      inspectCell(cell, rowLabel, column, value);
      return;
    }

    const destination = csvGridFocusDestination(
      cell,
      event.key,
      rows.length,
      colCount,
      pageRows,
      event.ctrlKey || event.metaKey,
    );
    if (!destination) return;
    event.preventDefault();
    if (destination.row === cell.row && destination.column === cell.column) return;

    pendingGridFocus.current = true;
    setActiveCell(destination);
    revealGridCell(destination);
  };

  const closeInspector = () => {
    pendingGridFocus.current = true;
    setInspected(null);
  };

  useEffect(() => {
    if (!pendingGridFocus.current || inspected) return;
    const target = scrollRef.current?.querySelector<HTMLElement>(
      `[data-grid-row="${rovingCell.row}"][data-grid-column="${rovingCell.column}"]`,
    );
    if (!target) return;
    pendingGridFocus.current = false;
    if (activeCell.row !== rovingCell.row || activeCell.column !== rovingCell.column) {
      setActiveCell(rovingCell);
    }
    target.focus({ preventScroll: true });
  }, [activeCell.column, activeCell.row, inspected, rovingCell.column, rovingCell.row]);

  return (
    <div className="q-gridwrap">
      <div className="q-gridbar">
        <span className="q-thint">Separator: {delim === "\t" ? "tab" : JSON.stringify(delim)}</span>
        <label className="q-check">
          <input type="checkbox" checked={useHeader} onChange={(e) => toggleHeader(e.target.checked)} /> First row is a header
        </label>
        <button
          className="q-btn q-grid-window-button"
          type="button"
          disabled={win.current.atBof}
          onClick={loadFirstWindow}
        >
          First sample
        </button>
        <button
          className="q-btn q-grid-window-button"
          type="button"
          disabled={win.current.atBof || previousCsvGridCursor(previousStarts.current) == null}
          onClick={loadPreviousWindow}
        >
          Previous sample
        </button>
        <button
          className="q-btn q-grid-window-button"
          type="button"
          disabled={win.current.atEof}
          onClick={loadNextWindow}
        >
          Next sample
        </button>
        <span id="q-grid-window-status" className="q-thint" role="status" aria-live="polite">
          Bounded sample · {visibleRowLabel}; {visibleColumnLabel}
        </span>
        {earlierHistoryUnavailable && (
          <span className="q-thint" role="status" aria-live="polite">
            Earlier sample history is unavailable; use First sample.
          </span>
        )}
        {inspected && (
          <div role="region" aria-label={`Cell ${inspected.rowLabel}, column ${inspected.column} inspector`}>
            <label className="q-tlabel" htmlFor="q-grid-cell-inspector">
              {inspected.rowLabel}, column {inspected.column}
            </label>
            <textarea
              id="q-grid-cell-inspector"
              ref={inspectorRef}
              className="q-select"
              aria-label="Full bounded cell value"
              readOnly
              rows={2}
              value={inspected.value}
            />
            {inspected.truncated && (
              <span className="q-thint" role="status">
                Value exceeded the safety limit; inspection and copy are limited to {CELL_INSPECT_CODE_UNITS} UTF-16 code units.
              </span>
            )}
            <button className="q-btn" type="button" onClick={() => void copyInspected()}>Copy cell</button>
            <button className="q-btn" type="button" onClick={closeInspector}>Close inspector</button>
          </div>
        )}
      </div>
      <div className="q-grid" ref={scrollRef} onScroll={onScroll}>
        <table
          className="q-grid-table"
          role="grid"
          aria-rowcount={rows.length + 1}
          aria-colcount={colCount + 1}
          aria-describedby="q-grid-window-status"
          style={{ width: GRID_ROW_NUMBER_WIDTH + colCount * GRID_COLUMN_WIDTH }}
        >
          <caption className="q-sr-only">
            Virtualized bounded CSV sample. Use arrow, Home, End, Page Up, and Page Down keys to move; press Enter or Space to inspect a cell.
          </caption>
          <colgroup>
            <col style={{ width: GRID_ROW_NUMBER_WIDTH }} />
            <col span={colCount} style={{ width: GRID_COLUMN_WIDTH }} />
          </colgroup>
          <thead>
            <tr aria-rowindex={1}>
              <th className="q-grid-rownum" scope="col" aria-colindex={1}>#</th>
              {leftColumns > 0 && (
                <th className="q-grid-virtual-spacer" colSpan={leftColumns} aria-hidden="true" />
              )}
              {visibleHeaders.map((header, visibleIndex) => {
                const columnIndex = renderWindow.columnStart + visibleIndex;
                return (
                  <th
                    key={columnIndex}
                    scope="col"
                    aria-colindex={columnIndex + 2}
                    data-grid-row={0}
                    data-grid-column={columnIndex}
                    tabIndex={rovingCell.row === 0 && rovingCell.column === columnIndex ? 0 : -1}
                    title={cellTitle(header)}
                    aria-label={`Inspect header, column ${columnIndex + 1}`}
                    onFocus={() => setActiveCell({ row: 0, column: columnIndex })}
                    onClick={() => inspectCell({ row: 0, column: columnIndex }, "Header", columnIndex + 1, header)}
                    onKeyDown={(event) => handleGridCellKeyDown(
                      event,
                      { row: 0, column: columnIndex },
                      "Header",
                      columnIndex + 1,
                      header,
                    )}
                  >
                    {cellPreview(header)}
                  </th>
                );
              })}
              {rightColumns > 0 && (
                <th className="q-grid-virtual-spacer" colSpan={rightColumns} aria-hidden="true" />
              )}
            </tr>
          </thead>
          <tbody>
            {rows.length === 0 && (
              <tr>
                <td className="q-grid-empty" colSpan={headers.length + 1}>No data rows in this window.</td>
              </tr>
            )}
            {topRows > 0 && (
              <tr className="q-grid-spacer-row" aria-hidden="true">
                <td colSpan={colCount + 1} style={{ height: topRows * GRID_ROW_HEIGHT }} />
              </tr>
            )}
            {visibleRows.map((row, visibleRowIndex) => {
              const rowIndex = renderWindow.rowStart + visibleRowIndex;
              const rowLabel = `Row ${startRow + rowIndex}`;
              return (
                <tr key={rowIndex} aria-rowindex={rowIndex + 2} style={{ height: GRID_ROW_HEIGHT }}>
                  <th className="q-grid-rownum" scope="row">{startRow + rowIndex}</th>
                  {leftColumns > 0 && (
                    <td className="q-grid-virtual-spacer" colSpan={leftColumns} aria-hidden="true" />
                  )}
                  {visibleHeaders.map((_, visibleColumnIndex) => {
                    const columnIndex = renderWindow.columnStart + visibleColumnIndex;
                    const value = row[columnIndex] ?? "";
                  return (
                    <td
                      key={columnIndex}
                      aria-colindex={columnIndex + 2}
                      data-grid-row={rowIndex + 1}
                      data-grid-column={columnIndex}
                      tabIndex={rovingCell.row === rowIndex + 1 && rovingCell.column === columnIndex ? 0 : -1}
                      title={cellTitle(value)}
                      aria-label={`Inspect ${rowLabel.toLowerCase()}, column ${columnIndex + 1}`}
                      onFocus={() => setActiveCell({ row: rowIndex + 1, column: columnIndex })}
                      onClick={() => inspectCell(
                        { row: rowIndex + 1, column: columnIndex },
                        rowLabel,
                        columnIndex + 1,
                        value,
                      )}
                      onKeyDown={(event) => handleGridCellKeyDown(
                        event,
                        { row: rowIndex + 1, column: columnIndex },
                        rowLabel,
                        columnIndex + 1,
                        value,
                      )}
                    >
                      {cellPreview(value)}
                    </td>
                  );
                })}
                  {rightColumns > 0 && (
                    <td className="q-grid-virtual-spacer" colSpan={rightColumns} aria-hidden="true" />
                  )}
                </tr>
              );
            })}
            {bottomRows > 0 && (
              <tr className="q-grid-spacer-row" aria-hidden="true">
                <td colSpan={colCount + 1} style={{ height: bottomRows * GRID_ROW_HEIGHT }} />
              </tr>
            )}
          </tbody>
        </table>
      </div>
    </div>
  );
}
