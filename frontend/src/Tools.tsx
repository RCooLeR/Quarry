import { useEffect, useMemo, useRef, useState } from "react";
import { FileService } from "../bindings/github.com/quarry/quarry-wails3";
import type {
  CsvRedactColumn,
  CsvSqlColumnConfig,
  CsvSqlConfig,
  TransformResult,
} from "../bindings/github.com/quarry/quarry-wails3/models.js";
import {
  normalizeCsvInspect,
  normalizeCsvPreview,
  normalizeCsvProfile,
  normalizeCsvSchema,
  normalizeCsvTextPreview,
  normalizeSqlLint,
  normalizeSqlSchemaDiff,
  normalizeSqlSummary,
} from "./bridgePayloads";
import type {
  NormalizedCsvColumn,
  NormalizedCsvInspectResult,
  NormalizedCsvPreviewResult,
  NormalizedCsvProfileResult,
  NormalizedSqlLintResult,
  NormalizedSqlSchemaDiffResult,
  NormalizedSqlSummaryResult,
} from "./bridgePayloads";
import { isSQLAnalysisJob, jobProgressLabel } from "./jobState";
import type { ActiveJobState } from "./jobState";
import type { NotificationOwner } from "./notificationState";
import {
  detectedCsvDialect,
  fallbackCsvDialect,
  isValidCsvDelimiter,
  mergeCsvDetection,
  overrideCsvDialect,
} from "./csvDialect";
import type { CsvDialect } from "./csvDialect";
import {
  boundedUtf8Text,
  SQL_FIND_INPUT_MAX_BYTES,
  SQL_REPLACEMENT_INPUT_MAX_BYTES,
} from "./textInputLimits";
import { ToolsRequestEpochGate } from "./toolsRequestEpoch";

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

const SQL_TABLE_PAGE_SIZE = 200;
const SQL_DIFF_NAME_LIMIT = 200;
const SQL_DIFF_CHANGED_LIMIT = 100;
const SQL_DIFF_DETAIL_LIMIT = 100;
export const TOOLS_CSV_COLUMN_DOM_LIMIT = 256;
export const TOOLS_CSV_WARNING_DOM_LIMIT = 64;
export const TOOLS_CSV_IDENTIFIER_INPUT_LIMIT = 256;
export const TOOLS_CSV_VALUE_INPUT_LIMIT = 64 * 1024;
export const TOOLS_CSV_NULL_TOKEN_LIMIT = 1024;

function boundedToolsInput(value: string, limit: number): string {
  const bounded = value.slice(0, limit);
  if (bounded.length === 0) return bounded;
  const last = bounded.charCodeAt(bounded.length - 1);
  return last >= 0xD800 && last <= 0xDBFF ? bounded.slice(0, -1) : bounded;
}

export function parseToolsCsvNullValues(value: string): { values: string[]; overflow: boolean } {
  const segments = value.split(",", TOOLS_CSV_NULL_TOKEN_LIMIT + 1);
  return {
    values: segments.slice(0, TOOLS_CSV_NULL_TOKEN_LIMIT).map((segment) => segment.trim()).filter(Boolean),
    overflow: segments.length > TOOLS_CSV_NULL_TOKEN_LIMIT,
  };
}

function boundedNames(values: string[], limit = SQL_DIFF_NAME_LIMIT): string {
  const visible = values.slice(0, limit);
  const remainder = values.length - visible.length;
  return visible.join(", ") + (remainder > 0 ? ` … and ${remainder} more` : "");
}

function boundedItemNames(values: Array<{ name: string }>, limit = SQL_DIFF_NAME_LIMIT): string {
  const visible = values.slice(0, limit).map((value) => value.name);
  const remainder = values.length - visible.length;
  return visible.join(", ") + (remainder > 0 ? ` … and ${remainder} more` : "");
}

function fmtBytes(n: number): string {
  if (!n) return "—";
  if (n < 1024) return `${n} B`;
  const u = ["KB", "MB", "GB", "TB"];
  let v = n / 1024, i = 0;
  while (v >= 1024 && i < u.length - 1) { v /= 1024; i++; }
  return `${v.toFixed(v < 10 ? 1 : 0)} ${u[i]}`;
}

function requireCsvSourceGeneration(generation: number | null | undefined, operation: string): number {
  if (!Number.isSafeInteger(generation) || (generation ?? 0) <= 0) {
    throw new Error(`${operation} is not bound to a valid CSV source generation. Refresh the CSV preview and try again.`);
  }
  return generation as number;
}

interface OtherFile { id: string; name: string; detected: string; }

interface Props {
  fileId: string;
  detected: string;
  dialect?: CsvDialect;
  onDialectChange?: (dialect: CsvDialect) => void;
  analysis: NormalizedSqlSummaryResult | null;
  otherFiles: OtherFile[];
  onAnalyze: (fileId: string) => Promise<NormalizedSqlSummaryResult>;
  activeJob: ActiveJobState | null;
  onCancelJob: (jobId: string) => Promise<void> | void;
  onOperationStart: (operation: string, jobCandidate?: boolean) => NotificationOwner;
  onOperationComplete: (owner: NotificationOwner) => void;
  /** Reserve shared workbench transitions while a transform/dialog owns this file. */
  onWorkbenchBusyChange?: (busy: boolean) => void;
  onNotice: (owner: NotificationOwner, message: string, details?: string) => void;
  onError: (owner: NotificationOwner, message: string, details?: string) => void;
  onClose: () => void;
}

export default function Tools({
  fileId,
  detected,
  dialect,
  onDialectChange,
  analysis,
  otherFiles,
  onAnalyze,
  activeJob,
  onCancelJob,
  onOperationStart,
  onOperationComplete,
  onWorkbenchBusyChange,
  onNotice,
  onError,
  onClose,
}: Props) {
  const d = detected.toLowerCase();
  const isCsv = d === "csv" || d === "tsv";
  const isSql = d === "sql";
  const initialDialect = dialect ?? fallbackCsvDialect(detected);
  const requestGateRef = useRef<ToolsRequestEpochGate | null>(null);
  if (requestGateRef.current == null) requestGateRef.current = new ToolsRequestEpochGate(fileId, d);
  const requestGate = requestGateRef.current;
  const contextChanged = requestGate.syncContext(fileId, d);
  const contextKey = `${fileId}\u0000${d}`;
  const csvStateContextRef = useRef("");
  const sqlDiffContextRef = useRef("");
  const sqlDiffAnalysisRef = useRef<NormalizedSqlSummaryResult | null>(null);
  const analysisRef = useRef(analysis);
  analysisRef.current = analysis;
  if (contextChanged) {
    csvStateContextRef.current = "";
    sqlDiffContextRef.current = "";
    sqlDiffAnalysisRef.current = null;
  }

  // CSV state
  const [delim, setDelim] = useState(initialDialect.delimiter);
  const [hasHeader, setHasHeader] = useState(initialDialect.hasHeader);
  const [inspect, setInspect] = useState<NormalizedCsvInspectResult | null>(null);
  const [schema, setSchema] = useState<NormalizedCsvColumn[]>([]);
  const [schemaWarnings, setSchemaWarnings] = useState<string[]>([]);
  const [preview, setPreview] = useState<NormalizedCsvPreviewResult | null>(null);
  const [csvSourceGeneration, setCsvSourceGeneration] = useState<number | null>(null);
  const [cols, setCols] = useState<CsvSqlColumnConfig[]>([]);
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
  const [profile, setProfile] = useState<NormalizedCsvProfileResult | null>(null);
  const [lint, setLint] = useState<NormalizedSqlLintResult | null>(null);
  // filter / dedupe / sample
  const [filterCol, setFilterCol] = useState(0);
  const [filterOp, setFilterOp] = useState("contains");
  const [filterVal, setFilterVal] = useState("");
  const [filterNeg, setFilterNeg] = useState(false);
  const [dedupeKey, setDedupeKey] = useState(-1);
  const [sampleEvery, setSampleEvery] = useState(10);
  const [jsonlNumbers, setJsonlNumbers] = useState(true);

  // SQL state (analysis is lifted to App; shared with palette + X-ray)
  const sqlSummary = analysis;
  const [sampleRows, setSampleRows] = useState(100);
  const [reshapeMode, setReshapeMode] = useState("single");
  const [reshapeBatch, setReshapeBatch] = useState(100);
  const [find, setFind] = useState("");
  const [repl, setRepl] = useState("");
  const [sqlFindInputLimited, setSqlFindInputLimited] = useState(false);
  const [sqlReplacementInputLimited, setSqlReplacementInputLimited] = useState(false);
  const [sqlReplaceCaseInsensitive, setSqlReplaceCaseInsensitive] = useState(false);
  const [sqlReplaceWholeWord, setSqlReplaceWholeWord] = useState(false);
  const [diffTarget, setDiffTarget] = useState("");
  const [diff, setDiff] = useState<NormalizedSqlSchemaDiffResult | null>(null);
  const [diffPending, setDiffPending] = useState(false);
  const [sqlTableFilter, setSqlTableFilter] = useState("");
  const [sqlTablePage, setSqlTablePage] = useState(0);

  const [busy, setBusy] = useState(false);
  const [analysisPending, setAnalysisPending] = useState(false);
  const [csvRefreshPending, setCsvRefreshPending] = useState(false);
  const csvRefreshGeneration = useRef(0);
  const dialectRef = useRef(initialDialect);
  const csvSourceGenerationRef = useRef<number | null>(null);
  const csvSqlConfigurationKeyRef = useRef("");
  const diffPendingRequestRef = useRef<number | null>(null);
  const transformBusyRef = useRef(false);
  const workbenchBusyChangeRef = useRef(onWorkbenchBusyChange);
  workbenchBusyChangeRef.current = onWorkbenchBusyChange;
  const activeOperationOwnersRef = useRef(new Map<string, NotificationOwner>());
  const operationCompleteRef = useRef(onOperationComplete);
  operationCompleteRef.current = onOperationComplete;
  const beginOperation = (operation: string, jobCandidate?: boolean): NotificationOwner => {
    const owner = onOperationStart(operation, jobCandidate);
    activeOperationOwnersRef.current.set(owner.operationId, owner);
    return owner;
  };
  const completeOperation = (owner: NotificationOwner): void => {
    if (!activeOperationOwnersRef.current.delete(owner.operationId)) return;
    operationCompleteRef.current(owner);
  };
  const parsedNullValues = parseToolsCsvNullValues(nullValue);
  const analysisJob = isSQLAnalysisJob(activeJob, fileId) ? activeJob : null;
  const csvStateCurrent = csvStateContextRef.current === contextKey;
  const sqlDiffStateCurrent = sqlDiffContextRef.current === contextKey && sqlDiffAnalysisRef.current === analysis;
  const visibleDiff = sqlDiffStateCurrent ? diff : null;
  const currentDiffPending = sqlDiffStateCurrent && diffPending;
  const currentCsvRefreshPending = isCsv && csvStateCurrent && csvRefreshPending;
  const toolBusy = busy || analysisPending || currentCsvRefreshPending || currentDiffPending || activeJob != null;
  const csvTransformDisabled = toolBusy || !csvStateCurrent || csvSourceGeneration == null;
  const csvSqlTransformDisabled = csvTransformDisabled || parsedNullValues.overflow;
  const sqlReplaceInputInfo = [
    sqlFindInputLimited ? "SQL find text is limited to 64 KiB of UTF-8." : "",
    sqlReplacementInputLimited ? "SQL replacement text is limited to 256 KiB of UTF-8." : "",
  ].filter(Boolean).join(" ");
  const filteredSqlTables = useMemo(() => {
    const tables = sqlSummary?.tables ?? [];
    const query = sqlTableFilter.toLocaleLowerCase();
    return query ? tables.filter((table) => table.name.toLocaleLowerCase().includes(query)) : tables;
  }, [sqlSummary, sqlTableFilter]);
  const sqlTablePageCount = Math.max(1, Math.ceil(filteredSqlTables.length / SQL_TABLE_PAGE_SIZE));
  const visibleSqlTablePage = Math.min(sqlTablePage, sqlTablePageCount - 1);
  const visibleSqlTables = filteredSqlTables.slice(
    visibleSqlTablePage * SQL_TABLE_PAGE_SIZE,
    (visibleSqlTablePage + 1) * SQL_TABLE_PAGE_SIZE,
  );

  useEffect(() => {
    requestGate.mount();
    return () => {
      requestGate.unmount();
      const owners = Array.from(activeOperationOwnersRef.current.values());
      activeOperationOwnersRef.current.clear();
      owners.forEach((owner) => operationCompleteRef.current(owner));
    };
  }, [requestGate]);

  useEffect(() => { setSqlTablePage(0); }, [fileId, sqlSummary, sqlTableFilter]);

  useEffect(() => {
    requestGate.invalidateChannel("sql-schema-diff");
    requestGate.invalidateChannel("sql-lint");
    requestGate.invalidateChannel("sql-analyze");
    setDiffTarget("");
    setDiff(null);
    sqlDiffContextRef.current = "";
    sqlDiffAnalysisRef.current = null;
    diffPendingRequestRef.current = null;
    setDiffPending(false);
    setLint(null);
    setAnalysisPending(false);
  }, [fileId, d, requestGate]);

  useEffect(() => {
    requestGate.invalidateChannel("sql-schema-diff");
    requestGate.invalidateChannel("sql-lint");
    setDiff(null);
    sqlDiffContextRef.current = "";
    sqlDiffAnalysisRef.current = null;
    diffPendingRequestRef.current = null;
    setDiffPending(false);
  }, [analysis, requestGate]);

  const clearCsvSchemaDependentState = () => {
    setSchema([]);
    setSchemaWarnings([]);
    setPreview(null);
    setCols([]);
    setDropCol(0);
    setSqlPreviewText("");
    setRedactModes({});
    setProfile(null);
    setFilterCol(0);
    setFilterOp("contains");
    setFilterVal("");
    setFilterNeg(false);
    setDedupeKey(-1);
    csvSourceGenerationRef.current = null;
    setCsvSourceGeneration(null);
  };

  const refreshCsv = async (
    dl: string,
    h: boolean,
    existingOwner?: NotificationOwner,
    expectedSourceGeneration?: number,
  ) => {
    const owner = existingOwner ?? beginOperation("Refresh CSV schema and preview");
    csvStateContextRef.current = contextKey;
    requestGate.advanceCsvConfiguration();
    const request = requestGate.begin("csv-schema-preview", { csvConfiguration: true });
    const generation = ++csvRefreshGeneration.current;
    const requestFileId = fileId;
    setCsvRefreshPending(true);
    clearCsvSchemaDependentState();
    try {
      // Treat schema and preview as one configuration snapshot. Neither result
      // is committed until both calls and both payload validations succeed.
      const [schemaPayload, previewPayload] = await Promise.all([
        FileService.CsvSchema(requestFileId, dl, h),
        FileService.CsvPreview(requestFileId, dl, h, 12),
      ]);
      if (!requestGate.isCurrent(request) || csvRefreshGeneration.current !== generation) return;
      const sc = normalizeCsvSchema(schemaPayload);
      const pv = normalizeCsvPreview(previewPayload);
      const schemaGeneration = requireCsvSourceGeneration(sc.generation, "CSV schema");
      const previewGeneration = requireCsvSourceGeneration(pv.generation, "CSV preview");
      if (schemaGeneration !== previewGeneration) {
        throw new Error(`CSV schema generation ${schemaGeneration} does not match preview generation ${previewGeneration}. Refresh and try again.`);
      }
      if (expectedSourceGeneration != null && schemaGeneration !== expectedSourceGeneration) {
        throw new Error(`CSV source changed from inspected generation ${expectedSourceGeneration} to ${schemaGeneration}. Refresh and try again.`);
      }
      setSchema(sc.columns);
      setSchemaWarnings(sc.warnings);
      setPreview(pv);
      setCols(sc.columns.map((c, i) => ({ source: i, name: c.name, type: c.sqlType || "TEXT", include: true })));
      setDropCol(0);
      csvSourceGenerationRef.current = schemaGeneration;
      setCsvSourceGeneration(schemaGeneration);
    } catch (e: any) {
      if (requestGate.isCurrent(request) && csvRefreshGeneration.current === generation) {
        onError(owner, "Unable to refresh CSV schema and preview", String(e?.message ?? e));
      }
    } finally {
      if (requestGate.isCurrent(request) && csvRefreshGeneration.current === generation) setCsvRefreshPending(false);
      if (!existingOwner) completeOperation(owner);
    }
  };

  useEffect(() => {
    csvStateContextRef.current = contextKey;
    requestGate.advanceCsvConfiguration();
    const request = requestGate.begin("csv-inspect");
    ++csvRefreshGeneration.current;
    dialectRef.current = initialDialect;
    setDelim(initialDialect.delimiter);
    setHasHeader(initialDialect.hasHeader);
    setInspect(null);
    clearCsvSchemaDependentState();
    setCsvRefreshPending(isCsv);
    if (!isCsv) return () => {
      requestGate.invalidateChannel("csv-inspect");
      csvRefreshGeneration.current++;
    };
    let cancelled = false;
    const owner = beginOperation("Inspect CSV");
    (async () => {
      try {
        const payload = await FileService.CsvInspect(fileId);
        if (cancelled || !requestGate.isCurrent(request)) return;
        const r = normalizeCsvInspect(payload);
        const inspectedGeneration = requireCsvSourceGeneration(r.generation, "CSV inspection");
        setInspect(r);
        const detectedDialect = detectedCsvDialect(detected, r.delimiter, r.hasHeader);
        const effectiveDialect = mergeCsvDetection(dialectRef.current, detectedDialect);
        dialectRef.current = effectiveDialect;
        setDelim(effectiveDialect.delimiter);
        setHasHeader(effectiveDialect.hasHeader);
        onDialectChange?.(effectiveDialect);
        await refreshCsv(effectiveDialect.delimiter, effectiveDialect.hasHeader, owner, inspectedGeneration);
      } catch (e: any) {
        if (!cancelled && requestGate.isCurrent(request)) {
          onError(owner, "Unable to inspect CSV", String(e?.message ?? e));
        }
      } finally {
        if (!cancelled && requestGate.isCurrent(request)) setCsvRefreshPending(false);
        completeOperation(owner);
      }
    })();
    return () => {
      cancelled = true;
      requestGate.invalidateChannel("csv-inspect");
      csvRefreshGeneration.current++;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [fileId, detected]);

  useEffect(() => {
    if (!isCsv || !dialect) return;
    if (
      dialect.delimiter === dialectRef.current.delimiter
      && dialect.hasHeader === dialectRef.current.hasHeader
      && dialect.origin === dialectRef.current.origin
    ) return;
    const configurationChanged = dialect.delimiter !== dialectRef.current.delimiter
      || dialect.hasHeader !== dialectRef.current.hasHeader;
    dialectRef.current = dialect;
    setDelim(dialect.delimiter);
    setHasHeader(dialect.hasHeader);
    if (configurationChanged) {
      requestGate.invalidateChannel("csv-inspect");
      void refreshCsv(dialect.delimiter, dialect.hasHeader);
    }
    // Primitive dialect fields intentionally control propagation; object
    // identity is not a reason to rescan a bounded preview.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [fileId, isCsv, dialect?.delimiter, dialect?.hasHeader, dialect?.origin]);

  const onDelimChange = async (dl: string) => {
    const next = overrideCsvDialect(dl, hasHeader);
    requestGate.invalidateChannel("csv-inspect");
    dialectRef.current = next;
    setDelim(dl);
    onDialectChange?.(next);
    await refreshCsv(dl, hasHeader);
  };
  const onHeaderChange = async (h: boolean) => {
    const next = overrideCsvDialect(delim, h);
    requestGate.invalidateChannel("csv-inspect");
    dialectRef.current = next;
    setHasHeader(h);
    onDialectChange?.(next);
    await refreshCsv(delim, h);
  };

  const setCol = (i: number, patch: Partial<CsvSqlColumnConfig>) =>
    setCols((prev) => prev.map((c, j) => (j === i ? { ...c, ...patch } : c)));

  const buildConfig = (): CsvSqlConfig => ({
    delimiter: delim,
    hasHeader,
    tableName,
    columns: cols,
    includeCreate,
    insertMode,
    batchSize,
    nullValues: parsedNullValues.values,
    onInvalid,
  });
  const csvSqlConfigurationKey = JSON.stringify({ config: buildConfig(), nullValueOverflow: parsedNullValues.overflow });
  csvSqlConfigurationKeyRef.current = csvSqlConfigurationKey;

  useEffect(() => {
    requestGate.invalidateChannel("csv-sql-preview");
    setSqlPreviewText("");
  }, [csvSqlConfigurationKey, requestGate]);

  const invalidateCsvForSourceChange = () => {
    requestGate.advanceCsvConfiguration();
    ++csvRefreshGeneration.current;
    setInspect(null);
    clearCsvSchemaDependentState();
    setCsvRefreshPending(false);
  };

  const run = async (operation: string, fn: () => Promise<TransformResult>) => {
    // Button disabled state is rendered asynchronously. This ref closes the
    // same-tick double-click gap before two native dialogs/jobs can start.
    if (transformBusyRef.current || activeJob != null) return;
    transformBusyRef.current = true;
    const request = requestGate.begin("transform");
    const owner = beginOperation(operation, true);
    setBusy(true);
    workbenchBusyChangeRef.current?.(true);
    try {
      const r = await fn();
      if (requestGate.isCurrent(request) && r && r.outputPath) {
        onNotice(owner, `Wrote ${r.outputPath}`, r.note);
      }
    } catch (e: any) {
      if (requestGate.isCurrent(request)) {
        onError(owner, `${operation} failed`, String(e?.message ?? e));
      }
    } finally {
      completeOperation(owner);
      transformBusyRef.current = false;
      setBusy(false);
      workbenchBusyChangeRef.current?.(false);
    }
  };

  // CSV transforms
  const doDrop = () =>
    run("Drop CSV column", async () => {
      const keep = schema.map((_, i) => i).filter((i) => i !== dropCol);
      return FileService.CsvProjectViaDialog(fileId, requireCsvSourceGeneration(csvSourceGeneration, "CSV projection"), delim, keep);
    });
  const doAdd = () =>
    run("Add CSV column", async () => FileService.CsvAddColumnViaDialog(fileId, requireCsvSourceGeneration(csvSourceGeneration, "CSV add-column transform"), delim, schema.length, addVal));
  const doConvert = () =>
    run("Convert CSV to SQL", async () => FileService.CsvToSQLConfigViaDialog(fileId, requireCsvSourceGeneration(csvSourceGeneration, "CSV-to-SQL conversion"), buildConfig()));
  const redactCols: CsvRedactColumn[] = Object.entries(redactModes).filter(([, m]) => m && m !== "off").map(([i, m]) => ({ index: Number(i), mode: m }));
  const doRedact = () =>
    run("Redact CSV columns", async () => FileService.CsvRedactViaDialog(fileId, requireCsvSourceGeneration(csvSourceGeneration, "CSV redaction"), delim, hasHeader, redactCols, redactFixed));
  const doSqlPreview = async () => {
    const owner = beginOperation("Preview CSV as SQL");
    const sourceGeneration = csvSourceGenerationRef.current;
    const request = requestGate.begin("csv-sql-preview", {
      csvConfiguration: true,
      ...(sourceGeneration == null ? {} : { sourceGeneration }),
    });
    const configurationKey = csvSqlConfigurationKey;
    setSqlPreviewText("");
    try {
      const generation = requireCsvSourceGeneration(sourceGeneration, "CSV-to-SQL preview");
      const payload = await FileService.CsvToSQLConfigPreview(fileId, buildConfig());
      if (!requestGate.isCurrent(request, csvSourceGenerationRef.current) || configurationKey !== csvSqlConfigurationKeyRef.current) return;
      const result = normalizeCsvTextPreview(payload);
      if (result.generation !== generation) {
        const message = `CSV source changed from preview generation ${generation} to ${result.generation}. Refresh and try again.`;
        invalidateCsvForSourceChange();
        onError(owner, "Unable to preview CSV as SQL", message);
        return;
      }
      setSqlPreviewText(result.text);
    } catch (e: any) {
      if (requestGate.isCurrent(request, csvSourceGenerationRef.current) && configurationKey === csvSqlConfigurationKeyRef.current) {
        onError(owner, "Unable to preview CSV as SQL", String(e?.message ?? e));
      }
    } finally {
      completeOperation(owner);
    }
  };
  const doProfile = async () => {
    const owner = beginOperation("Profile CSV");
    const sourceGeneration = csvSourceGenerationRef.current;
    const request = requestGate.begin("csv-profile", {
      csvConfiguration: true,
      ...(sourceGeneration == null ? {} : { sourceGeneration }),
    });
    setProfile(null);
    try {
      const generation = requireCsvSourceGeneration(sourceGeneration, "CSV profile");
      const payload = await FileService.CsvProfile(fileId, delim, hasHeader);
      if (!requestGate.isCurrent(request, csvSourceGenerationRef.current)) return;
      const result = normalizeCsvProfile(payload);
      if (result.generation !== generation) {
        const message = `CSV source changed from preview generation ${generation} to ${result.generation}. Refresh and try again.`;
        invalidateCsvForSourceChange();
        onError(owner, "Unable to profile CSV", message);
        return;
      }
      setProfile(result);
    } catch (e: any) {
      if (requestGate.isCurrent(request, csvSourceGenerationRef.current)) {
        onError(owner, "Unable to profile CSV", String(e?.message ?? e));
      }
    } finally {
      completeOperation(owner);
    }
  };
  const needsValue = !(filterOp === "empty" || filterOp === "nonempty");
  const doFilter = () =>
    run("Filter CSV rows", async () => FileService.CsvFilterViaDialog(fileId, requireCsvSourceGeneration(csvSourceGeneration, "CSV filter"), delim, hasHeader, filterCol, filterOp, filterVal, filterNeg));
  const doDedupe = () =>
    run("Deduplicate CSV rows", async () => FileService.CsvDedupeViaDialog(fileId, requireCsvSourceGeneration(csvSourceGeneration, "CSV deduplication"), delim, hasHeader, dedupeKey));
  const doSampleCsv = () =>
    run("Sample CSV rows", async () => FileService.CsvSampleViaDialog(fileId, requireCsvSourceGeneration(csvSourceGeneration, "CSV sampling"), delim, hasHeader, sampleEvery));
  const doExportJsonl = () =>
    run("Export CSV as JSONL", async () => FileService.CsvExportJSONLViaDialog(fileId, requireCsvSourceGeneration(csvSourceGeneration, "CSV JSONL export"), delim, hasHeader, jsonlNumbers));
  const doCopyMarkdown = async () => {
    const owner = beginOperation("Copy CSV preview as Markdown");
    const sourceGeneration = csvSourceGenerationRef.current;
    const request = requestGate.begin("csv-markdown-preview", {
      csvConfiguration: true,
      ...(sourceGeneration == null ? {} : { sourceGeneration }),
    });
    try {
      const generation = requireCsvSourceGeneration(sourceGeneration, "CSV Markdown preview");
      const payload = await FileService.CsvMarkdownPreview(fileId, delim, hasHeader, 50);
      if (!requestGate.isCurrent(request, csvSourceGenerationRef.current)) return;
      const result = normalizeCsvTextPreview(payload);
      if (result.generation !== generation) {
        const message = `CSV source changed from preview generation ${generation} to ${result.generation}. Refresh and try again.`;
        invalidateCsvForSourceChange();
        onError(owner, "Unable to copy CSV preview as Markdown", message);
        return;
      }
      await navigator.clipboard.writeText(result.text);
      if (requestGate.isCurrent(request, csvSourceGenerationRef.current)) {
        onNotice(owner, "Copied preview as Markdown");
      }
    } catch (e: any) {
      if (requestGate.isCurrent(request, csvSourceGenerationRef.current)) {
        onError(owner, "Unable to copy CSV preview as Markdown", String(e?.message ?? e));
      }
    } finally {
      completeOperation(owner);
    }
  };

  // SQL tools
  const doAnalyze = async () => {
    if (analysisPending || activeJob != null) return;
    const request = requestGate.begin("sql-analyze");
    const owner = beginOperation("Analyze SQL dump", true);
    setAnalysisPending(true);
    try {
      const payload = await onAnalyze(fileId);
      if (!requestGate.isCurrent(request)) return;
      const s = normalizeSqlSummary(payload);
      onNotice(owner, `Found ${s.tables.length} tables`);
    } catch (e: any) {
      if (requestGate.isCurrent(request)) {
        onError(owner, "Unable to analyze SQL dump", String(e?.message ?? e));
      }
    } finally {
      completeOperation(owner);
      if (requestGate.isCurrent(request)) setAnalysisPending(false);
    }
  };
  const doExtract = (name: string) => run(`Extract SQL table ${name}`, async () => FileService.SqlExtractTableViaDialog(fileId, name));
  const doSchema = (name: string) => run(`Extract SQL schema ${name}`, async () => FileService.SqlExtractSchemaViaDialog(fileId, name));
  const doData = (name: string) => run(`Extract SQL data ${name}`, async () => FileService.SqlExtractDataViaDialog(fileId, name));
  const doSplit = () => run("Split SQL dump by table", async () => FileService.SqlSplitByTableViaDialog(fileId));
  const doSample = () => run("Create SQL sample fixture", async () => FileService.SqlSampleFixtureViaDialog(fileId, sampleRows));
  const doLint = async () => {
    const requestAnalysis = analysisRef.current;
    const request = requestGate.begin("sql-lint");
    const owner = beginOperation("Lint SQL dump");
    setLint(null);
    try {
      const payload = await FileService.SqlLint(fileId);
      if (!requestGate.isCurrent(request) || analysisRef.current !== requestAnalysis) return;
      setLint(normalizeSqlLint(payload));
    } catch (e: any) {
      if (requestGate.isCurrent(request) && analysisRef.current === requestAnalysis) {
        onError(owner, "Unable to lint SQL dump", String(e?.message ?? e));
      }
    } finally {
      completeOperation(owner);
    }
  };
  const doReplace = () => run(
    "Replace SQL text",
    async () => FileService.SqlReplaceViaDialog(fileId, find, repl, false, sqlReplaceCaseInsensitive, sqlReplaceWholeWord),
  );
  const updateSqlFind = (value: string) => {
    const bounded = boundedUtf8Text(value, SQL_FIND_INPUT_MAX_BYTES);
    setFind(bounded.value);
    setSqlFindInputLimited(bounded.truncated);
  };
  const updateSqlReplacement = (value: string) => {
    const bounded = boundedUtf8Text(value, SQL_REPLACEMENT_INPUT_MAX_BYTES);
    setRepl(bounded.value);
    setSqlReplacementInputLimited(bounded.truncated);
  };
  const doReshape = () => run("Reshape SQL inserts", async () => FileService.SqlReshapeInsertsViaDialog(fileId, reshapeMode, reshapeBatch));
  const doDiff = async () => {
    if (!diffTarget) return;
    const target = diffTarget;
    const request = requestGate.begin("sql-schema-diff");
    sqlDiffContextRef.current = contextKey;
    sqlDiffAnalysisRef.current = analysis;
    diffPendingRequestRef.current = request.requestEpoch;
    const owner = beginOperation("Compare SQL schemas");
    setDiff(null);
    setDiffPending(true);
    try {
      const payload = await FileService.SqlSchemaDiff(fileId, target);
      if (!requestGate.isCurrent(request)) return;
      const r = normalizeSqlSchemaDiff(payload);
      setDiff(r);
      const changes = r.addedTables.length + r.removedTables.length + r.changedTables.length;
      onNotice(owner, changes === 0 ? "Schemas are identical" : `${changes} table changes`);
    } catch (e: any) {
      if (requestGate.isCurrent(request)) {
        onError(owner, "Unable to compare SQL schemas", String(e?.message ?? e));
      }
    } finally {
      completeOperation(owner);
      if (diffPendingRequestRef.current === request.requestEpoch) {
        diffPendingRequestRef.current = null;
        setDiffPending(false);
      }
    }
  };
  const sqlOthers = otherFiles.filter((o) => ["sql"].includes(o.detected.toLowerCase()));

  useEffect(() => {
    if (!diffTarget || sqlOthers.some((file) => file.id === diffTarget)) return;
    requestGate.invalidateChannel("sql-schema-diff");
    setDiffTarget("");
    setDiff(null);
    sqlDiffContextRef.current = "";
    sqlDiffAnalysisRef.current = null;
    diffPendingRequestRef.current = null;
    setDiffPending(false);
  }, [diffTarget, otherFiles, requestGate]);

  const onDiffTargetChange = (target: string) => {
    requestGate.invalidateChannel("sql-schema-diff");
    setDiffTarget(target);
    setDiff(null);
  };

  const activeInspect = csvStateCurrent ? inspect : null;
  const activeSchema = csvStateCurrent ? schema : [];
  const activePreview = csvStateCurrent ? preview : null;
  const activeCols = csvStateCurrent ? cols : [];
  const activeProfile = csvStateCurrent ? profile : null;
  const candidateDelimiters = (activeInspect?.candidates ?? [])
    .filter((candidate) => isValidCsvDelimiter(candidate.delimiter))
    .map((candidate) => ({
      value: candidate.delimiter,
      label: `${candidate.name || JSON.stringify(candidate.delimiter)} - ${candidate.columns} cols, score ${candidate.score.toFixed(2)}`,
    }));
  const delims = [...candidateDelimiters, ...DELIMS]
    .filter((option, index, all) => all.findIndex((other) => other.value === option.value) === index);
  if (!delims.some((option) => option.value === delim)) {
    delims.unshift({ value: delim, label: `Configured (${JSON.stringify(delim)})` });
  }
  const csvWarnings = Array.from(new Set([
    ...(activeInspect?.warnings ?? []),
    ...(csvStateCurrent ? schemaWarnings : []),
    ...(activePreview?.warnings ?? []),
  ]));
  const renderedCsvWarnings = csvWarnings.slice(0, TOOLS_CSV_WARNING_DOM_LIMIT);
  const csvWarningsOmitted = csvWarnings.length - renderedCsvWarnings.length;
  const typeOptions = (t: string) => (SQL_TYPES.includes(t) ? SQL_TYPES : [t, ...SQL_TYPES]);
  const selectedCount = activeCols.filter((c) => c.include).length;
  const renderedSchema = activeSchema.slice(0, TOOLS_CSV_COLUMN_DOM_LIMIT);
  const renderedCols = activeCols.slice(0, TOOLS_CSV_COLUMN_DOM_LIMIT);
  const schemaColumnsOmitted = Math.max(activeSchema.length, activeCols.length) - Math.max(renderedSchema.length, renderedCols.length);
  const previewColumnCount = activePreview == null
    ? 0
    : Math.max(activePreview.header.length, activePreview.rows.reduce((largest, row) => Math.max(largest, row.length), 0));
  const previewColumnsOmitted = Math.max(0, previewColumnCount - TOOLS_CSV_COLUMN_DOM_LIMIT);
  const profileColumnsOmitted = Math.max(0, (activeProfile?.columns.length ?? 0) - TOOLS_CSV_COLUMN_DOM_LIMIT);

  return (
    <aside className="q-tools" aria-labelledby="q-tools-title">
      <div className="q-tools-head">
        <h2 id="q-tools-title" className="q-tools-title">
          {isCsv ? "CSV tools" : isSql ? "SQL tools" : "Data tools unavailable"}
        </h2>
        <button type="button" className="q-icon" aria-label="Close data tools" title="Close data tools" disabled={toolBusy} onClick={onClose}>×</button>
      </div>
      <div className="q-tools-body">
        {isCsv ? (
          <>
            <label className="q-tlabel" htmlFor="q-csv-separator">Separator</label>
            <div className="q-trow">
              <select id="q-csv-separator" className="q-select" value={delim} onChange={(e) => void onDelimChange(e.target.value)}>
                {delims.map((x) => (<option key={x.value} value={x.value}>{x.label}</option>))}
              </select>
              {activeInspect && (
                <span className="q-thint">
                  {dialectRef.current.origin === "override" ? "explicit override" : "auto-detected"}; detector ranked {activeInspect.delimiterName} ({activeInspect.confidence}), {activeInspect.columns} cols
                </span>
              )}
            </div>
            <label className="q-check">
              <input type="checkbox" checked={hasHeader} onChange={(e) => void onHeaderChange(e.target.checked)} /> First row is a header
            </label>
            {currentCsvRefreshPending && <div className="q-thint" role="status" aria-live="polite">Refreshing schema and preview…</div>}
            {activeInspect && activeInspect.candidates.length > 0 && (
              <div className="q-thint" aria-label="Ranked separator candidates">
                Ranked candidates: {activeInspect.candidates.map((candidate) => (
                  `${candidate.name || JSON.stringify(candidate.delimiter)} (${candidate.columns} cols, ${candidate.score.toFixed(2)})`
                )).join("; ")}
              </div>
            )}
            {csvWarnings.length > 0 && (
              <div className="q-csv-warnings" role="status" aria-live="polite">
                <strong>Bounded-sample warnings</strong>
                <ul>{renderedCsvWarnings.map((warning) => <li key={warning}>{warning}</li>)}</ul>
                {csvWarningsOmitted > 0 && (
                  <div className="q-thint">
                    Showing the first {TOOLS_CSV_WARNING_DOM_LIMIT} warnings; {csvWarningsOmitted} additional warnings are omitted from this bounded view.
                  </div>
                )}
              </div>
            )}

            <div className="q-tsection">CSV → SQL</div>
            <label className="q-tlabel" htmlFor="q-csv-table-name">Table name</label>
            <input id="q-csv-table-name" className="q-select" placeholder="table name" maxLength={TOOLS_CSV_IDENTIFIER_INPUT_LIMIT} value={tableName} onChange={(e) => setTableName(boundedToolsInput(e.target.value, TOOLS_CSV_IDENTIFIER_INPUT_LIMIT))} />

            <div id="q-csv-columns-label" className="q-tlabel">Columns — include · rename · type ({selectedCount}/{activeCols.length})</div>
            <div className="q-colcfg" role="group" aria-labelledby="q-csv-columns-label">
              {renderedCols.map((c, i) => (
                <div className={"q-colrow" + (c.include ? "" : " q-colrow-off")} key={i}>
                  <input type="checkbox" aria-label={`Include column ${i + 1}: ${c.name || `column ${i + 1}`}`} checked={c.include} onChange={(e) => setCol(i, { include: e.target.checked })} title="Include this column" />
                  <input className="q-colname" aria-label={`Output name for column ${i + 1}`} maxLength={TOOLS_CSV_IDENTIFIER_INPUT_LIMIT} value={c.name} onChange={(e) => setCol(i, { name: boundedToolsInput(e.target.value, TOOLS_CSV_IDENTIFIER_INPUT_LIMIT) })} placeholder={`col${i + 1}`} />
                  <select className="q-coltype" aria-label={`SQL type for column ${i + 1}: ${c.name || `column ${i + 1}`}`} value={c.type} onChange={(e) => setCol(i, { type: e.target.value })} disabled={!includeCreate}>
                    {typeOptions(c.type).map((t) => (<option key={t} value={t}>{t}</option>))}
                  </select>
                </div>
              ))}
              {activeCols.length === 0 && <div className="q-thint">No columns detected.</div>}
            </div>
            {schemaColumnsOmitted > 0 && (
              <div className="q-thint">Only the first {TOOLS_CSV_COLUMN_DOM_LIMIT} columns are materialized in Tools controls; {schemaColumnsOmitted} additional columns are omitted from this bounded view. Exports retain their default configuration.</div>
            )}

            <div className="q-trow">
              <label className="q-tlabel q-tlabel-inline" htmlFor="q-csv-insert-mode">Insert mode</label>
              <select id="q-csv-insert-mode" className="q-select" value={insertMode} onChange={(e) => setInsertMode(e.target.value)}>
                <option value="insert">INSERT INTO</option>
                <option value="ignore">INSERT IGNORE</option>
                <option value="replace">REPLACE INTO</option>
              </select>
            </div>
            <div className="q-trow">
              <label className="q-check"><input type="checkbox" checked={includeCreate} onChange={(e) => setIncludeCreate(e.target.checked)} /> CREATE TABLE</label>
              <label className="q-tlabel q-tlabel-inline" htmlFor="q-csv-batch-size">Rows per INSERT</label>
              <input id="q-csv-batch-size" className="q-num" type="number" min={1} max={10000} value={batchSize} onChange={(e) => setBatchSize(Math.min(10000, Math.max(1, Number(e.target.value) || 1)))} />
            </div>
            <div className="q-trow">
              <label className="q-tlabel q-tlabel-inline" htmlFor="q-csv-null-values">NULL tokens</label>
              <input id="q-csv-null-values" className="q-select" placeholder="NULL,\\N (comma-separated)" maxLength={TOOLS_CSV_VALUE_INPUT_LIMIT} aria-invalid={parsedNullValues.overflow || undefined} aria-describedby={parsedNullValues.overflow ? "q-csv-null-values-error" : undefined} value={nullValue} onChange={(e) => setNullValue(boundedToolsInput(e.target.value, TOOLS_CSV_VALUE_INPUT_LIMIT))} />
            </div>
            <div className="q-trow">
              <label className="q-tlabel q-tlabel-inline" htmlFor="q-csv-invalid-bytes">Invalid bytes</label>
              <select id="q-csv-invalid-bytes" className="q-select" value={onInvalid} onChange={(e) => setOnInvalid(e.target.value)}>
                <option value="fail">Fail on invalid byte</option>
                <option value="skip-row">Skip offending row</option>
                <option value="replace">Replace with �</option>
              </select>
            </div>
            <div className="q-trow">
              <button className="q-btn" disabled={csvSqlTransformDisabled} onClick={() => void doSqlPreview()}>Preview SQL</button>
              <button className="q-btn q-btn-primary" disabled={csvSqlTransformDisabled || selectedCount === 0} onClick={doConvert}>Convert → .sql</button>
            </div>
            {parsedNullValues.overflow && <div id="q-csv-null-values-error" className="q-thint" role="alert">NULL tokens exceed the {TOOLS_CSV_NULL_TOKEN_LIMIT}-item configuration limit.</div>}
            {sqlPreviewText && <pre className="q-tpre">{sqlPreviewText}</pre>}

            <div className="q-tsection">Column transforms (new CSV)</div>
            {activePreview && activePreview.rows.length > 0 && (
              <div className="q-tprev">
                <table>
                  <caption className="q-sr-only">Bounded CSV preview</caption>
                  {activePreview.header.length > 0 && (<thead><tr>{activePreview.header.slice(0, TOOLS_CSV_COLUMN_DOM_LIMIT).map((h, i) => <th scope="col" key={i}>{h}</th>)}</tr></thead>)}
                  <tbody>{activePreview.rows.slice(0, 6).map((r, i) => (<tr key={i}>{r.slice(0, TOOLS_CSV_COLUMN_DOM_LIMIT).map((cc, j) => <td key={j}>{cc}</td>)}</tr>))}</tbody>
                </table>
              </div>
            )}
            {previewColumnsOmitted > 0 && (
              <div className="q-thint">Preview materializes the first {TOOLS_CSV_COLUMN_DOM_LIMIT} columns; {previewColumnsOmitted} additional columns are omitted.</div>
            )}
            {activePreview && activePreview.rows.length === 0 && (
              <div className="q-thint">No preview data rows.</div>
            )}
            <div className="q-trow">
              <select className="q-select" aria-label="Column to drop" value={dropCol} onChange={(e) => setDropCol(Number(e.target.value))}>
                {renderedSchema.map((c, i) => (<option key={i} value={i}>{c.name}</option>))}
              </select>
              <button className="q-btn" disabled={csvTransformDisabled || activeSchema.length === 0} onClick={doDrop}>Drop column</button>
            </div>
            <div className="q-trow">
              <input className="q-select" aria-label="Constant column value" placeholder="constant value" maxLength={TOOLS_CSV_VALUE_INPUT_LIMIT} value={addVal} onChange={(e) => setAddVal(boundedToolsInput(e.target.value, TOOLS_CSV_VALUE_INPUT_LIMIT))} />
              <button className="q-btn" disabled={csvTransformDisabled} onClick={doAdd}>Add column</button>
            </div>
            {hasHeader && <div className="q-thint">The constant value is also written as the new header cell.</div>}

            <div className="q-tsection">Mask / pseudonymize → new CSV</div>
            <div className="q-colcfg" role="group" aria-label="Redaction mode by column">
              {renderedSchema.map((c, i) => (
                <div className="q-colrow" key={i}>
                  <span className="q-colname" style={{ border: "none", background: "transparent" }}>{c.name}</span>
                  <select className="q-coltype" aria-label={`Redaction mode for column ${i + 1}: ${c.name || `column ${i + 1}`}`} value={redactModes[i] ?? "off"} onChange={(e) => setRedactModes((p) => ({ ...p, [i]: e.target.value }))}>
                    <option value="off">keep</option>
                    <option value="null">blank</option>
                    <option value="fixed">fixed</option>
                    <option value="hash">keyed pseudonym (new key per export)</option>
                    <option value="email">email a***@…</option>
                  </select>
                </div>
              ))}
            </div>
            <div className="q-trow">
              <label className="q-tlabel q-tlabel-inline" htmlFor="q-csv-redaction-fixed">Fixed replacement</label>
              <input id="q-csv-redaction-fixed" className="q-select" maxLength={TOOLS_CSV_VALUE_INPUT_LIMIT} value={redactFixed} onChange={(e) => setRedactFixed(boundedToolsInput(e.target.value, TOOLS_CSV_VALUE_INPUT_LIMIT))} />
              <button className="q-btn q-btn-primary" disabled={csvTransformDisabled || redactCols.length === 0} onClick={doRedact}>Create masked copy →</button>
            </div>

            <div className="q-tsection">Filter / dedupe / sample → new CSV</div>
            <div className="q-trow">
              <select className="q-select" aria-label="Filter column" value={filterCol} onChange={(e) => setFilterCol(Number(e.target.value))}>
                {renderedSchema.map((c, i) => (<option key={i} value={i}>{c.name}</option>))}
              </select>
              <select className="q-select" aria-label="Filter operation" value={filterOp} onChange={(e) => setFilterOp(e.target.value)}>
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
              {needsValue && <input className="q-select" aria-label="Filter comparison value" placeholder="value" maxLength={TOOLS_CSV_VALUE_INPUT_LIMIT} value={filterVal} onChange={(e) => setFilterVal(boundedToolsInput(e.target.value, TOOLS_CSV_VALUE_INPUT_LIMIT))} />}
              <label className="q-check"><input type="checkbox" checked={filterNeg} onChange={(e) => setFilterNeg(e.target.checked)} /> invert</label>
              <button className="q-btn q-btn-primary" disabled={csvTransformDisabled || activeSchema.length === 0} onClick={doFilter}>Filter rows →</button>
            </div>
            <div className="q-trow">
              <label className="q-tlabel q-tlabel-inline" htmlFor="q-csv-dedupe-key">Dedupe by</label>
              <select id="q-csv-dedupe-key" className="q-select" value={dedupeKey} onChange={(e) => setDedupeKey(Number(e.target.value))}>
                <option value={-1}>whole row</option>
                {renderedSchema.map((c, i) => (<option key={i} value={i}>{c.name}</option>))}
              </select>
              <button className="q-btn" disabled={csvTransformDisabled} onClick={doDedupe}>Dedupe →</button>
            </div>
            <div className="q-trow">
              <label className="q-tlabel q-tlabel-inline" htmlFor="q-csv-sample-interval">Keep every Nth row</label>
              <input id="q-csv-sample-interval" className="q-num" type="number" min={2} value={sampleEvery} onChange={(e) => setSampleEvery(Math.max(2, Number(e.target.value) || 2))} />
              <button className="q-btn" disabled={csvTransformDisabled} onClick={doSampleCsv}>Sample →</button>
            </div>

            <div className="q-tsection">Export → JSONL</div>
            <div className="q-trow">
              <label className="q-check">
                <input type="checkbox" checked={jsonlNumbers} onChange={(e) => setJsonlNumbers(e.target.checked)} /> Type unambiguous numeric values
              </label>
              <button className="q-btn" disabled={csvTransformDisabled} onClick={doExportJsonl} title="Newline-delimited JSON, one object per row">JSONL</button>
            </div>
            <div className="q-trow">
              <button className="q-btn" disabled={csvTransformDisabled} onClick={() => void doCopyMarkdown()} title="Copy the preview as a Markdown table">Copy preview as Markdown</button>
            </div>

            <div className="q-tsection">Profile (sampled)</div>
            <button className="q-btn" disabled={csvTransformDisabled} onClick={() => void doProfile()}>Profile columns</button>
            {activeProfile && (
              <div className="q-profile">
                <div className="q-thint">{activeProfile.recordsScanned} rows scanned{activeProfile.truncated ? " (sample)" : ""}{activeProfile.raggedRows > 0 ? ` · ${activeProfile.raggedRows} ragged rows` : ""}</div>
                {activeProfile.columns.length === 0 && <div className="q-thint">No profile columns returned.</div>}
                {activeProfile.columns.slice(0, TOOLS_CSV_COLUMN_DOM_LIMIT).map((c, i) => {
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
                {profileColumnsOmitted > 0 && <div className="q-thint">Profile materializes the first {TOOLS_CSV_COLUMN_DOM_LIMIT} columns; {profileColumnsOmitted} additional columns are omitted.</div>}
              </div>
            )}
          </>
        ) : isSql ? (
          <>
            <button className="q-btn q-btn-primary" disabled={toolBusy} onClick={() => void doAnalyze()}>
              {analysisJob || analysisPending ? "Analyzing…" : sqlSummary ? "Re-analyze dump" : "Analyze dump"}
            </button>
            <div className="q-thint" role="note">
              Exports contain only analyzed CREATE/INSERT/REPLACE regions. Session preamble, ALTER/DROP, triggers, and other DML are omitted; add any required SQL before re-import.
            </div>
            {analysisPending && !analysisJob && <div className="q-analysis-progress">Starting analysis…</div>}
            {analysisJob && (
              <div className="q-analysis-progress" role="status" aria-live="polite">
                <span>{jobProgressLabel(analysisJob)}{analysisJob.note ? ` · ${analysisJob.note}` : ""}</span>
                <button className="q-btn" onClick={() => void onCancelJob(analysisJob.id)}>Cancel analysis</button>
              </div>
            )}
            {sqlSummary && (
              <>
                <div className="q-thint">
                  CREATE {sqlSummary.createTables} · INSERT {sqlSummary.insertTables} · DEFINER {sqlSummary.definerCount}
                </div>
                <div className="q-trow">
                  <button className="q-btn" disabled={toolBusy} onClick={doSplit} title="Write one .sql file per table into a folder">Split by table → folder</button>
                  <button className="q-btn" disabled={toolBusy} onClick={() => doSchema("")} title="Export the whole-dump schema (DDL only)">Whole schema</button>
                </div>
                <div className="q-trow">
                  <button className="q-btn q-btn-primary" disabled={toolBusy} onClick={doSample} title="Small shareable dump: each table's DDL + first N rows">Dev fixture →</button>
                  <label className="q-tlabel q-tlabel-inline" htmlFor="q-sql-sample-rows">Rows per table</label>
                  <input id="q-sql-sample-rows" className="q-num" type="number" min={1} value={sampleRows} onChange={(e) => setSampleRows(Math.max(1, Number(e.target.value) || 1))} />
                </div>
                <div className="q-trow">
                  <button className="q-btn" disabled={toolBusy} onClick={() => void doLint()} title="Report dump issues (empty tables, DEFINER, mixed charsets, largest tables)">Lint dump</button>
                </div>
                {lint && (
                  <div className="q-profile">
                    {lint.findings.length === 0 && <div className="q-thint">No lint findings.</div>}
                    {lint.findings.map((fdg, i) => (
                      <div className={"q-lint q-lint-" + fdg.severity} key={i}>
                        <span className="q-lint-t">{fdg.title}</span>
                        {fdg.detail && <span className="q-thint">{fdg.detail}</span>}
                      </div>
                    ))}
                  </div>
                )}
                <div id="q-sql-tables-label" className="q-tlabel">Tables ({sqlSummary.tables.length}) — extract / schema / data</div>
                <div className="q-trow">
                  <input
                    className="q-select"
                    type="search"
                    maxLength={256}
                    aria-label="Filter analyzed SQL tables"
                    placeholder="Filter tables"
                    value={sqlTableFilter}
                    onChange={(event) => setSqlTableFilter(event.target.value)}
                  />
                  <span className="q-thint">{filteredSqlTables.length} matching</span>
                </div>
                <div className="q-ttables" role="list" aria-labelledby="q-sql-tables-label">
                  {filteredSqlTables.length === 0 && (
                    <div className="q-thint" role="listitem">{sqlSummary.tables.length === 0 ? "No tables found in the analyzed dump." : "No tables match the current filter."}</div>
                  )}
                  {visibleSqlTables.map((t) => (
                    <div className="q-ttable" role="listitem" key={`${t.name}:${t.createOffset}:${t.insertOffset}`}>
                      <span className="q-tcol-n" title={t.name}>{t.name}</span>
                      <span className="q-tcol-sz">{fmtBytes(t.bytes)}</span>
                      <span className="q-tbtns">
                        <button type="button" className="q-icon" aria-label={`Extract analyzed regions for table ${t.name}`} disabled={toolBusy} title="Extract analyzed CREATE/INSERT/REPLACE regions" onClick={() => doExtract(t.name)}>⤓</button>
                        <button type="button" className="q-icon" aria-label={`Export schema for table ${t.name}`} disabled={toolBusy} title="Schema only (DDL)" onClick={() => doSchema(t.name)}>S</button>
                        <button type="button" className="q-icon" aria-label={`Export data for table ${t.name}`} disabled={toolBusy || t.insertOffset < 0} title="Data only (INSERTs)" onClick={() => doData(t.name)}>D</button>
                      </span>
                    </div>
                  ))}
                </div>
                {filteredSqlTables.length > SQL_TABLE_PAGE_SIZE && (
                  <div className="q-trow" aria-label="SQL table result pages">
                    <button type="button" className="q-btn" disabled={visibleSqlTablePage === 0} onClick={() => setSqlTablePage((page) => Math.max(0, page - 1))}>Previous tables</button>
                    <span className="q-thint">Page {visibleSqlTablePage + 1} of {sqlTablePageCount}</span>
                    <button type="button" className="q-btn" disabled={visibleSqlTablePage + 1 >= sqlTablePageCount} onClick={() => setSqlTablePage((page) => Math.min(sqlTablePageCount - 1, page + 1))}>Next tables</button>
                  </div>
                )}
              </>
            )}

            <div className="q-tsection">Find / replace → new file</div>
            <input
              className="q-select"
              aria-label="Find SQL text"
              aria-describedby={sqlReplaceInputInfo ? "q-sql-replace-input-limit" : undefined}
              placeholder="find text"
              value={find}
              maxLength={SQL_FIND_INPUT_MAX_BYTES}
              onChange={(event) => updateSqlFind(event.target.value)}
            />
            <input
              className="q-select"
              aria-label="Replace SQL text with"
              aria-describedby={sqlReplaceInputInfo ? "q-sql-replace-input-limit" : undefined}
              placeholder="replace with"
              value={repl}
              maxLength={SQL_REPLACEMENT_INPUT_MAX_BYTES}
              onChange={(event) => updateSqlReplacement(event.target.value)}
            />
            {sqlReplaceInputInfo && (
              <div id="q-sql-replace-input-limit" className="q-thint" role="status" aria-live="polite">
                {sqlReplaceInputInfo}
              </div>
            )}
            <div className="q-trow">
              <label className="q-check">
                <input type="checkbox" checked={sqlReplaceCaseInsensitive} onChange={(event) => setSqlReplaceCaseInsensitive(event.target.checked)} /> ignore case
              </label>
              <label className="q-check">
                <input type="checkbox" checked={sqlReplaceWholeWord} onChange={(event) => setSqlReplaceWholeWord(event.target.checked)} /> whole word
              </label>
              <button className="q-btn q-btn-primary" disabled={toolBusy || !find} onClick={doReplace}>Replace → file</button>
            </div>
            <div className="q-thint" role="note">
              Plain replacement updates SQL string values only, then recalculates PHP/WordPress serialized byte lengths before writing the copy. Comments, identifiers, and routine bodies are left unchanged; regex replacement remains unavailable for serialized dumps.
            </div>

            <div className="q-tsection">Reshape INSERTs → new file</div>
            <div className="q-trow">
              <select className="q-select" aria-label="INSERT reshape mode" value={reshapeMode} onChange={(e) => setReshapeMode(e.target.value)}>
                <option value="single">Explode → one row per INSERT</option>
                <option value="multi">Batch → extended INSERTs</option>
              </select>
              {reshapeMode === "multi" && (
                <>
                  <label className="q-tlabel q-tlabel-inline" htmlFor="q-sql-reshape-batch">Rows per INSERT</label>
                  <input id="q-sql-reshape-batch" className="q-num" type="number" min={1} value={reshapeBatch} onChange={(e) => setReshapeBatch(Math.max(1, Number(e.target.value) || 1))} />
                </>
              )}
            </div>
            <div className="q-trow">
              <button className="q-btn q-btn-primary" disabled={toolBusy} onClick={doReshape}>Reshape → file</button>
              <span className="q-thint">Changes statement grouping. Use only when statement-level triggers, atomicity, rollback, and database side effects are acceptable.</span>
            </div>

            <div className="q-tsection">Schema diff (vs another open dump)</div>
            {sqlOthers.length === 0 ? (
              <div className="q-thint">Open a second .sql dump to compare.</div>
            ) : (
              <>
                <div className="q-trow">
                  <select className="q-select" aria-label="Dump to compare" value={diffTarget} onChange={(e) => onDiffTargetChange(e.target.value)}>
                    <option value="">choose a dump…</option>
                    {sqlOthers.map((o) => (<option key={o.id} value={o.id}>{o.name}</option>))}
                  </select>
                  <button className="q-btn q-btn-primary" disabled={toolBusy || !diffTarget} onClick={() => void doDiff()}>Diff schemas</button>
                </div>
                <div className="q-thint">Both dumps must be analyzed first (Analyze dump).</div>
                {visibleDiff && (
                  <div className="q-profile">
                    <div className="q-thint">{visibleDiff.fileA} → {visibleDiff.fileB} · {visibleDiff.unchangedCount} unchanged</div>
                    {visibleDiff.addedTables.length > 0 && <div className="q-lint q-lint-info"><span className="q-lint-t">+ {visibleDiff.addedTables.length} tables</span><span className="q-thint">{boundedNames(visibleDiff.addedTables)}</span></div>}
                    {visibleDiff.removedTables.length > 0 && <div className="q-lint q-lint-warn"><span className="q-lint-t">− {visibleDiff.removedTables.length} tables</span><span className="q-thint">{boundedNames(visibleDiff.removedTables)}</span></div>}
                    {visibleDiff.changedTables.slice(0, SQL_DIFF_CHANGED_LIMIT).map((t, i) => (
                      <div className="q-prof" key={i}>
                        <div className="q-prof-h"><span className="q-tcol-n">{t.name}</span></div>
                        {t.addedColumns.length > 0 && <div className="q-thint">+ {boundedItemNames(t.addedColumns)}</div>}
                        {t.removedColumns.length > 0 && <div className="q-thint">− {boundedItemNames(t.removedColumns)}</div>}
                        {t.changedColumns.slice(0, SQL_DIFF_DETAIL_LIMIT).map((c, j) => (<div className="q-thint" key={j}>~ {c.name}: {c.old} → {c.new}</div>))}
                        {t.changedColumns.length > SQL_DIFF_DETAIL_LIMIT && <div className="q-thint">{t.changedColumns.length - SQL_DIFF_DETAIL_LIMIT} more changed columns</div>}
                        {t.status === "unknown" && <div className="q-thint">Comparison incomplete{t.reason ? `: ${t.reason}` : "."}</div>}
                        {t.identityChanged && <div className="q-thint">Identity: {t.identityChanged.old} → {t.identityChanged.new}</div>}
                        {t.columnOrderChanged && <div className="q-thint">Column order changed: {boundedNames(t.oldColumnOrder)} → {boundedNames(t.newColumnOrder)}</div>}
                        {t.addedConstraints.length > 0 && <div className="q-thint">+ constraints: {boundedNames(t.addedConstraints)}</div>}
                        {t.removedConstraints.length > 0 && <div className="q-thint">− constraints: {boundedNames(t.removedConstraints)}</div>}
                        {t.addedIndexes.length > 0 && <div className="q-thint">+ indexes: {boundedNames(t.addedIndexes)}</div>}
                        {t.removedIndexes.length > 0 && <div className="q-thint">− indexes: {boundedNames(t.removedIndexes)}</div>}
                        {t.changedOptions.slice(0, SQL_DIFF_DETAIL_LIMIT).map((option, j) => (
                          <div className="q-thint" key={`option-${j}`}>~ {option.name}: {option.old} → {option.new}</div>
                        ))}
                        {t.changedOptions.length > SQL_DIFF_DETAIL_LIMIT && <div className="q-thint">{t.changedOptions.length - SQL_DIFF_DETAIL_LIMIT} more changed table options</div>}
                      </div>
                    ))}
                    {visibleDiff.changedTables.length > SQL_DIFF_CHANGED_LIMIT && (
                      <div className="q-thint">{visibleDiff.changedTables.length - SQL_DIFF_CHANGED_LIMIT} additional changed tables are not rendered in this bounded view.</div>
                    )}
                    {visibleDiff.addedTables.length === 0 && visibleDiff.removedTables.length === 0 && visibleDiff.changedTables.length === 0 && (
                      <div className="q-thint">No structural differences.</div>
                    )}
                  </div>
                )}
              </>
            )}
          </>
        ) : (
          <div className="q-tools-unsupported" role="status">
            Data tools are available only for files detected as CSV, TSV, or SQL. No operation can run for this file type.
          </div>
        )}
      </div>
    </aside>
  );
}
