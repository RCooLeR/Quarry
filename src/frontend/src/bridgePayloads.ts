import type {
  CsvColumn,
  CsvDelimiterOption,
  CsvGridResult,
  CsvInspectResult,
  CsvPreviewResult,
  CsvProfileResult,
  CsvSchemaResult,
  CsvTextPreviewResult,
  SqlLintFinding,
  SqlLintResult,
  SqlSchemaDiffResult,
  SqlSummaryResult,
  SqlTable,
} from "../bindings/github.com/quarry/quarry-wails3/models.js";
import type { ColumnProfile, ValueCount } from "../bindings/github.com/quarry/quarry-wails3/internal/plugins/csv/models.js";
import { DiffStatus } from "../bindings/github.com/quarry/quarry-wails3/internal/plugins/sql/schemadiff/models.js";
import type {
  Column,
  ColumnChange,
  IdentityChange,
  OptionChange,
  TableDiff,
} from "../bindings/github.com/quarry/quarry-wails3/internal/plugins/sql/schemadiff/models.js";

type PayloadObject = Record<string, unknown>;

const MAX_SQL_TABLES = 10_000;
const MAX_SQL_COLUMNS = 4_096;
const MAX_SQL_SCHEMA_ENTRIES = 8_192;
const MAX_SQL_TABLE_OPTIONS = 256;
const MAX_SQL_LINT_FINDINGS = 8;
const MAX_CSV_GRID_ROWS = 500;
const MAX_CSV_GRID_COLUMNS = 256;
const MAX_CSV_GRID_CELLS = 10_000;
// These mirror the backend parser/sample contracts. Keep structured CSV
// payloads bounded before copying them into React state even if a future or
// malformed bridge response violates the server-side limits.
const MAX_CSV_FIELDS = 1_024;
const MAX_CSV_PREVIEW_ROWS = 500;
const MAX_CSV_PREVIEW_CELLS = MAX_CSV_FIELDS * MAX_CSV_PREVIEW_ROWS;
const MAX_CSV_WARNINGS = 4_096;
const MAX_CSV_DELIMITER_CANDIDATES = 16;
const MAX_CSV_SCHEMA_SAMPLES = 3;
const MAX_CSV_PROFILE_TOP_VALUES = 10;

/**
 * Wails accurately models nil Go slices as nullable arrays. Components should
 * not have to repeat null checks for every render, so bridge responses are
 * converted once into these non-null, recursively normalized view models.
 */
export type NormalizedCsvColumn = Omit<CsvColumn, "samples"> & { samples: string[] };
export type NormalizedCsvGridResult = Omit<CsvGridResult, "rows"> & { rows: string[][] };
export type NormalizedCsvInspectResult = Omit<CsvInspectResult, "candidates" | "warnings"> & {
  candidates: CsvDelimiterOption[];
  warnings: string[];
};
export type NormalizedCsvSchemaResult = Omit<CsvSchemaResult, "columns" | "warnings"> & {
  columns: NormalizedCsvColumn[];
  warnings: string[];
};
export type NormalizedCsvPreviewResult = Omit<CsvPreviewResult, "header" | "rows" | "warnings"> & {
  header: string[];
  rows: string[][];
  warnings: string[];
};
export type NormalizedColumnProfile = Omit<ColumnProfile, "top"> & { top: ValueCount[] };
export type NormalizedCsvProfileResult = Omit<CsvProfileResult, "columns"> & {
  columns: NormalizedColumnProfile[];
};
export type NormalizedCsvTextPreviewResult = CsvTextPreviewResult;
export type NormalizedSqlSummaryResult = Omit<SqlSummaryResult, "tables"> & { tables: SqlTable[] };
export type NormalizedSqlLintResult = Omit<SqlLintResult, "findings"> & { findings: SqlLintFinding[] };
export type NormalizedTableDiff = Omit<
  TableDiff,
  | "addedColumns"
  | "removedColumns"
  | "changedColumns"
  | "oldColumnOrder"
  | "newColumnOrder"
  | "addedConstraints"
  | "removedConstraints"
  | "addedIndexes"
  | "removedIndexes"
  | "changedOptions"
> & {
  addedColumns: Column[];
  removedColumns: Column[];
  changedColumns: ColumnChange[];
  oldColumnOrder: string[];
  newColumnOrder: string[];
  addedConstraints: string[];
  removedConstraints: string[];
  addedIndexes: string[];
  removedIndexes: string[];
  changedOptions: OptionChange[];
};
export type NormalizedSqlSchemaDiffResult = Omit<
  SqlSchemaDiffResult,
  "addedTables" | "removedTables" | "changedTables"
> & {
  addedTables: string[];
  removedTables: string[];
  changedTables: NormalizedTableDiff[];
};

export class BridgePayloadError extends Error {
  readonly field: string;

  constructor(field: string, expected: string) {
    super(`Invalid backend response at ${field}: expected ${expected}`);
    this.name = "BridgePayloadError";
    this.field = field;
  }
}

function objectValue(value: unknown, field: string): PayloadObject {
  if (value == null) return {};
  if (typeof value !== "object" || Array.isArray(value)) {
    throw new BridgePayloadError(field, "an object");
  }
  return value as PayloadObject;
}

function arrayValue(value: unknown, field: string): unknown[] {
  if (value == null) return [];
  if (!Array.isArray(value)) throw new BridgePayloadError(field, "an array or null");
  return value;
}

function boundedArrayValue(value: unknown, field: string, maximum: number): unknown[] {
  const result = arrayValue(value, field);
  if (result.length > maximum) {
    throw new BridgePayloadError(field, `an array with at most ${maximum} entries`);
  }
  return result;
}

function stringValue(value: unknown, field: string, fallback = ""): string {
  if (value == null) return fallback;
  if (typeof value !== "string") throw new BridgePayloadError(field, "a string");
  return value;
}

function optionalString(value: unknown, field: string): string | undefined {
  if (value === undefined) return undefined;
  if (typeof value !== "string") throw new BridgePayloadError(field, "a string or undefined");
  return value;
}

function numberValue(value: unknown, field: string, fallback = 0): number {
  if (value == null) return fallback;
  if (typeof value !== "number" || !Number.isFinite(value)) {
    throw new BridgePayloadError(field, "a finite number");
  }
  return value;
}

function sourceGenerationValue(value: unknown, field: string): number {
  const generation = numberValue(value, field);
  if (!Number.isSafeInteger(generation) || generation < 0) {
    throw new BridgePayloadError(field, "a non-negative safe integer");
  }
  return generation;
}

function booleanValue(value: unknown, field: string, fallback = false): boolean {
  if (value == null) return fallback;
  if (typeof value !== "boolean") throw new BridgePayloadError(field, "a boolean");
  return value;
}

function boundedStringArray(value: unknown, field: string, maximum: number): string[] {
  return boundedArrayValue(value, field, maximum).map((item, index) =>
    stringValue(item, `${field}[${index}]`),
  );
}

function boundedStringRows(
  value: unknown,
  field: string,
  maximumRows: number,
  maximumColumns: number,
  maximumCells: number,
): string[][] {
  const rawRows = boundedArrayValue(value, field, maximumRows);
  let cells = 0;
  return rawRows.map((row, rowIndex) => {
    const normalized = boundedStringArray(row, `${field}[${rowIndex}]`, maximumColumns);
    cells += normalized.length;
    if (cells > maximumCells) {
      throw new BridgePayloadError(field, `at most ${maximumCells} cells`);
    }
    return normalized;
  });
}

function normalizeCsvDelimiterOption(value: unknown, field: string): CsvDelimiterOption {
  const item = objectValue(value, field);
  return {
    delimiter: stringValue(item.delimiter, `${field}.delimiter`),
    name: stringValue(item.name, `${field}.name`),
    columns: numberValue(item.columns, `${field}.columns`),
    score: numberValue(item.score, `${field}.score`),
  };
}

function normalizeCsvColumn(value: unknown, field: string): NormalizedCsvColumn {
  const item = objectValue(value, field);
  return {
    name: stringValue(item.name, `${field}.name`),
    sqlType: stringValue(item.sqlType, `${field}.sqlType`),
    nonNull: numberValue(item.nonNull, `${field}.nonNull`),
    null: numberValue(item.null, `${field}.null`),
    samples: boundedStringArray(item.samples, `${field}.samples`, MAX_CSV_SCHEMA_SAMPLES),
  };
}

function normalizeValueCount(value: unknown, field: string): ValueCount {
  const item = objectValue(value, field);
  return {
    value: stringValue(item.value, `${field}.value`),
    count: numberValue(item.count, `${field}.count`),
  };
}

function normalizeColumnProfile(value: unknown, field: string): NormalizedColumnProfile {
  const item = objectValue(value, field);
  return {
    name: stringValue(item.name, `${field}.name`),
    sqlType: stringValue(item.sqlType, `${field}.sqlType`),
    nonNull: numberValue(item.nonNull, `${field}.nonNull`),
    null: numberValue(item.null, `${field}.null`),
    distinct: numberValue(item.distinct, `${field}.distinct`),
    distinctCapped: booleanValue(item.distinctCapped, `${field}.distinctCapped`),
    min: stringValue(item.min, `${field}.min`),
    max: stringValue(item.max, `${field}.max`),
    top: boundedArrayValue(item.top, `${field}.top`, MAX_CSV_PROFILE_TOP_VALUES).map((entry, index) =>
      normalizeValueCount(entry, `${field}.top[${index}]`),
    ),
  };
}

function normalizeSqlTable(value: unknown, field: string): SqlTable {
  const item = objectValue(value, field);
  return {
    name: stringValue(item.name, `${field}.name`),
    createOffset: numberValue(item.createOffset, `${field}.createOffset`),
    insertOffset: numberValue(item.insertOffset, `${field}.insertOffset`),
    bytes: numberValue(item.bytes, `${field}.bytes`),
  };
}

function normalizeLintFinding(value: unknown, field: string): SqlLintFinding {
  const item = objectValue(value, field);
  return {
    severity: stringValue(item.severity, `${field}.severity`),
    title: stringValue(item.title, `${field}.title`),
    detail: stringValue(item.detail, `${field}.detail`),
  };
}

function normalizeColumn(value: unknown, field: string): Column {
  const item = objectValue(value, field);
  return {
    name: stringValue(item.name, `${field}.name`),
    definition: stringValue(item.definition, `${field}.definition`),
  };
}

function normalizeColumnChange(value: unknown, field: string): ColumnChange {
  const item = objectValue(value, field);
  return {
    name: stringValue(item.name, `${field}.name`),
    old: stringValue(item.old, `${field}.old`),
    new: stringValue(item.new, `${field}.new`),
  };
}

function normalizeOptionChange(value: unknown, field: string): OptionChange {
  const item = objectValue(value, field);
  return {
    name: stringValue(item.name, `${field}.name`),
    old: stringValue(item.old, `${field}.old`),
    new: stringValue(item.new, `${field}.new`),
  };
}

function normalizeIdentityChange(value: unknown, field: string): IdentityChange | null | undefined {
  if (value === undefined) return undefined;
  if (value === null) return null;
  const item = objectValue(value, field);
  return {
    old: stringValue(item.old, `${field}.old`),
    new: stringValue(item.new, `${field}.new`),
  };
}

function normalizeDiffStatus(value: unknown, field: string): TableDiff["status"] {
  if (value == null || value === DiffStatus.$zero) return DiffStatus.$zero;
  if (value === DiffStatus.DiffChanged) return DiffStatus.DiffChanged;
  if (value === DiffStatus.DiffUnknown) return DiffStatus.DiffUnknown;
  throw new BridgePayloadError(field, '"changed", "unknown", or empty');
}

function normalizeTableDiff(value: unknown, field: string): NormalizedTableDiff {
  const item = objectValue(value, field);
  return {
    name: stringValue(item.name, `${field}.name`),
    status: normalizeDiffStatus(item.status, `${field}.status`),
    reason: optionalString(item.reason, `${field}.reason`),
    identityChanged: normalizeIdentityChange(item.identityChanged, `${field}.identityChanged`),
    addedColumns: boundedArrayValue(item.addedColumns, `${field}.addedColumns`, MAX_SQL_COLUMNS).map((entry, index) =>
      normalizeColumn(entry, `${field}.addedColumns[${index}]`),
    ),
    removedColumns: boundedArrayValue(item.removedColumns, `${field}.removedColumns`, MAX_SQL_COLUMNS).map((entry, index) =>
      normalizeColumn(entry, `${field}.removedColumns[${index}]`),
    ),
    changedColumns: boundedArrayValue(item.changedColumns, `${field}.changedColumns`, MAX_SQL_COLUMNS).map((entry, index) =>
      normalizeColumnChange(entry, `${field}.changedColumns[${index}]`),
    ),
    columnOrderChanged: booleanValue(item.columnOrderChanged, `${field}.columnOrderChanged`),
    oldColumnOrder: boundedStringArray(item.oldColumnOrder, `${field}.oldColumnOrder`, MAX_SQL_COLUMNS),
    newColumnOrder: boundedStringArray(item.newColumnOrder, `${field}.newColumnOrder`, MAX_SQL_COLUMNS),
    addedConstraints: boundedStringArray(item.addedConstraints, `${field}.addedConstraints`, MAX_SQL_SCHEMA_ENTRIES),
    removedConstraints: boundedStringArray(item.removedConstraints, `${field}.removedConstraints`, MAX_SQL_SCHEMA_ENTRIES),
    addedIndexes: boundedStringArray(item.addedIndexes, `${field}.addedIndexes`, MAX_SQL_SCHEMA_ENTRIES),
    removedIndexes: boundedStringArray(item.removedIndexes, `${field}.removedIndexes`, MAX_SQL_SCHEMA_ENTRIES),
    changedOptions: boundedArrayValue(item.changedOptions, `${field}.changedOptions`, MAX_SQL_TABLE_OPTIONS).map((entry, index) =>
      normalizeOptionChange(entry, `${field}.changedOptions[${index}]`),
    ),
  };
}

export function normalizeCsvGrid(value: unknown): NormalizedCsvGridResult {
  const result = objectValue(value, "CsvGridResult");
  return {
    fileId: stringValue(result.fileId, "CsvGridResult.fileId"),
    generation: sourceGenerationValue(result.generation, "CsvGridResult.generation"),
    encoding: stringValue(result.encoding, "CsvGridResult.encoding"),
    startByte: numberValue(result.startByte, "CsvGridResult.startByte"),
    nextByte: numberValue(result.nextByte, "CsvGridResult.nextByte"),
    startRow: numberValue(result.startRow, "CsvGridResult.startRow"),
    rows: boundedStringRows(
      result.rows,
      "CsvGridResult.rows",
      MAX_CSV_GRID_ROWS,
      MAX_CSV_GRID_COLUMNS,
      MAX_CSV_GRID_CELLS,
    ),
    columns: numberValue(result.columns, "CsvGridResult.columns"),
    sourceBytes: numberValue(result.sourceBytes, "CsvGridResult.sourceBytes"),
    decodedBytes: numberValue(result.decodedBytes, "CsvGridResult.decodedBytes"),
    atBof: booleanValue(result.atBof, "CsvGridResult.atBof"),
    atEof: booleanValue(result.atEof, "CsvGridResult.atEof"),
  };
}

export function normalizeCsvInspect(value: unknown): NormalizedCsvInspectResult {
  const result = objectValue(value, "CsvInspectResult");
  return {
    generation: sourceGenerationValue(result.generation, "CsvInspectResult.generation"),
    delimiter: stringValue(result.delimiter, "CsvInspectResult.delimiter"),
    delimiterName: stringValue(result.delimiterName, "CsvInspectResult.delimiterName"),
    confidence: stringValue(result.confidence, "CsvInspectResult.confidence"),
    columns: numberValue(result.columns, "CsvInspectResult.columns"),
    hasHeader: booleanValue(result.hasHeader, "CsvInspectResult.hasHeader"),
    candidates: boundedArrayValue(result.candidates, "CsvInspectResult.candidates", MAX_CSV_DELIMITER_CANDIDATES).map((entry, index) =>
      normalizeCsvDelimiterOption(entry, `CsvInspectResult.candidates[${index}]`),
    ),
    warnings: boundedStringArray(result.warnings, "CsvInspectResult.warnings", MAX_CSV_WARNINGS),
  };
}

export function normalizeCsvSchema(value: unknown): NormalizedCsvSchemaResult {
  const result = objectValue(value, "CsvSchemaResult");
  return {
    generation: sourceGenerationValue(result.generation, "CsvSchemaResult.generation"),
    columns: boundedArrayValue(result.columns, "CsvSchemaResult.columns", MAX_CSV_FIELDS).map((entry, index) =>
      normalizeCsvColumn(entry, `CsvSchemaResult.columns[${index}]`),
    ),
    hasHeader: booleanValue(result.hasHeader, "CsvSchemaResult.hasHeader"),
    warnings: boundedStringArray(result.warnings, "CsvSchemaResult.warnings", MAX_CSV_WARNINGS),
  };
}

export function normalizeCsvPreview(value: unknown): NormalizedCsvPreviewResult {
  const result = objectValue(value, "CsvPreviewResult");
  return {
    generation: sourceGenerationValue(result.generation, "CsvPreviewResult.generation"),
    header: boundedStringArray(result.header, "CsvPreviewResult.header", MAX_CSV_FIELDS),
    rows: boundedStringRows(
      result.rows,
      "CsvPreviewResult.rows",
      MAX_CSV_PREVIEW_ROWS,
      MAX_CSV_FIELDS,
      MAX_CSV_PREVIEW_CELLS,
    ),
    warnings: boundedStringArray(result.warnings, "CsvPreviewResult.warnings", MAX_CSV_WARNINGS),
  };
}

export function normalizeCsvProfile(value: unknown): NormalizedCsvProfileResult {
  const result = objectValue(value, "CsvProfileResult");
  return {
    generation: sourceGenerationValue(result.generation, "CsvProfileResult.generation"),
    columns: boundedArrayValue(result.columns, "CsvProfileResult.columns", MAX_CSV_FIELDS).map((entry, index) =>
      normalizeColumnProfile(entry, `CsvProfileResult.columns[${index}]`),
    ),
    recordsScanned: numberValue(result.recordsScanned, "CsvProfileResult.recordsScanned"),
    raggedRows: numberValue(result.raggedRows, "CsvProfileResult.raggedRows"),
    truncated: booleanValue(result.truncated, "CsvProfileResult.truncated"),
  };
}

export function normalizeCsvTextPreview(value: unknown): NormalizedCsvTextPreviewResult {
  const result = objectValue(value, "CsvTextPreviewResult");
  return {
    generation: sourceGenerationValue(result.generation, "CsvTextPreviewResult.generation"),
    text: stringValue(result.text, "CsvTextPreviewResult.text"),
  };
}

export function normalizeSqlSummary(value: unknown): NormalizedSqlSummaryResult {
  const result = objectValue(value, "SqlSummaryResult");
  return {
    tables: boundedArrayValue(result.tables, "SqlSummaryResult.tables", MAX_SQL_TABLES).map((entry, index) =>
      normalizeSqlTable(entry, `SqlSummaryResult.tables[${index}]`),
    ),
    createTables: numberValue(result.createTables, "SqlSummaryResult.createTables"),
    insertTables: numberValue(result.insertTables, "SqlSummaryResult.insertTables"),
    definerCount: numberValue(result.definerCount, "SqlSummaryResult.definerCount"),
    header: booleanValue(result.header, "SqlSummaryResult.header"),
  };
}

export function normalizeSqlLint(value: unknown): NormalizedSqlLintResult {
	const result = objectValue(value, "SqlLintResult");
	return {
		findings: boundedArrayValue(result.findings, "SqlLintResult.findings", MAX_SQL_LINT_FINDINGS).map((entry, index) =>
			normalizeLintFinding(entry, `SqlLintResult.findings[${index}]`),
		),
	};
}

export function normalizeSqlSchemaDiff(value: unknown): NormalizedSqlSchemaDiffResult {
  const result = objectValue(value, "SqlSchemaDiffResult");
  return {
    fileA: stringValue(result.fileA, "SqlSchemaDiffResult.fileA"),
    fileB: stringValue(result.fileB, "SqlSchemaDiffResult.fileB"),
    addedTables: boundedStringArray(result.addedTables, "SqlSchemaDiffResult.addedTables", MAX_SQL_TABLES),
    removedTables: boundedStringArray(result.removedTables, "SqlSchemaDiffResult.removedTables", MAX_SQL_TABLES),
    changedTables: boundedArrayValue(result.changedTables, "SqlSchemaDiffResult.changedTables", MAX_SQL_TABLES).map((entry, index) =>
      normalizeTableDiff(entry, `SqlSchemaDiffResult.changedTables[${index}]`),
    ),
    unchangedCount: numberValue(result.unchangedCount, "SqlSchemaDiffResult.unchangedCount"),
  };
}
