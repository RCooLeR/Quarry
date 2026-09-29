import { describe, expect, it } from "vitest";
import {
  BridgePayloadError,
  normalizeCsvGrid,
  normalizeCsvInspect,
  normalizeCsvPreview,
  normalizeCsvProfile,
  normalizeCsvSchema,
  normalizeCsvTextPreview,
  normalizeSqlLint,
  normalizeSqlSchemaDiff,
  normalizeSqlSummary,
} from "../src/bridgePayloads";

describe("bridge payload normalization", () => {
  it.each([null, undefined])("turns a %s CSV grid response into a stable empty window", (payload) => {
    expect(normalizeCsvGrid(payload)).toEqual({
      fileId: "",
      generation: 0,
      encoding: "",
      startByte: 0,
      nextByte: 0,
      startRow: 0,
      rows: [],
      columns: 0,
      sourceBytes: 0,
      decodedBytes: 0,
      atBof: false,
      atEof: false,
    });
  });

  it.each([null, undefined])("normalizes a %s root response for every affected tool", (payload) => {
    expect(normalizeCsvInspect(payload).candidates).toEqual([]);
    expect(normalizeCsvSchema(payload).columns).toEqual([]);
    expect(normalizeCsvPreview(payload).rows).toEqual([]);
    expect(normalizeCsvProfile(payload).columns).toEqual([]);
    expect(normalizeSqlSummary(payload).tables).toEqual([]);
    expect(normalizeSqlLint(payload).findings).toEqual([]);
    expect(normalizeSqlSchemaDiff(payload).changedTables).toEqual([]);
  });

  it("normalizes empty and whitespace-only CSV inspection slices", () => {
    expect(normalizeCsvInspect({
      delimiter: "",
      columns: 0,
      hasHeader: false,
      candidates: null,
      warnings: null,
    })).toMatchObject({ candidates: [], warnings: [], columns: 0, hasHeader: false });
  });

  it("normalizes empty schema slices recursively", () => {
    expect(normalizeCsvSchema({ columns: null, warnings: undefined })).toEqual({
      generation: 0,
      columns: [],
      hasHeader: false,
      warnings: [],
    });

    expect(normalizeCsvSchema({
      columns: [{ name: "id", sqlType: "BIGINT", samples: null }],
      warnings: null,
    }).columns[0].samples).toEqual([]);
  });

  it("preserves a header-only preview while normalizing its nil data rows", () => {
    expect(normalizeCsvPreview({
      header: ["id", "name"],
      rows: null,
      warnings: null,
    })).toEqual({
      generation: 0,
      header: ["id", "name"],
      rows: [],
      warnings: [],
    });
  });

  it("preserves exact CSV generations across structured and text previews", () => {
    expect(normalizeCsvInspect({ generation: 17 }).generation).toBe(17);
    expect(normalizeCsvSchema({ generation: 17 }).generation).toBe(17);
    expect(normalizeCsvPreview({ generation: 17 }).generation).toBe(17);
    expect(normalizeCsvProfile({ generation: 17 }).generation).toBe(17);
    expect(normalizeCsvTextPreview({ generation: 17, text: "INSERT INTO records VALUES (1);" })).toEqual({
      generation: 17,
      text: "INSERT INTO records VALUES (1);",
    });
    expect(() => normalizeCsvPreview({ generation: 1.5 })).toThrow("CsvPreviewResult.generation");
    expect(() => normalizeCsvTextPreview({ generation: Number.MAX_SAFE_INTEGER + 1 })).toThrow("CsvTextPreviewResult.generation");
  });

  it("normalizes nested nil grid rows and profile top-value slices", () => {
    expect(normalizeCsvGrid({ rows: [null, ["a", "b"]] }).rows).toEqual([[], ["a", "b"]]);

    const profile = normalizeCsvProfile({
      columns: [{ name: "id", top: null }],
      recordsScanned: 0,
      raggedRows: 0,
      truncated: false,
    });
    expect(profile.columns).toHaveLength(1);
    expect(profile.columns[0].top).toEqual([]);
  });

  it("normalizes nullable SQL summary and lint slices", () => {
    expect(normalizeSqlSummary({ tables: null })).toMatchObject({
      tables: [],
      createTables: 0,
      insertTables: 0,
      definerCount: 0,
    });
    expect(normalizeSqlLint({ findings: null })).toEqual({ findings: [] });
  });

  it("normalizes an identical schema diff", () => {
    expect(normalizeSqlSchemaDiff({
      fileA: "a.sql",
      fileB: "b.sql",
      addedTables: null,
      removedTables: null,
      changedTables: null,
      unchangedCount: 4,
    })).toEqual({
      fileA: "a.sql",
      fileB: "b.sql",
      addedTables: [],
      removedTables: [],
      changedTables: [],
      unchangedCount: 4,
    });
  });

  it("normalizes every nested category in a partial table diff", () => {
    const result = normalizeSqlSchemaDiff({
      changedTables: [{
        name: "users",
        status: "changed",
        identityChanged: null,
        addedColumns: null,
        removedColumns: [{ name: "legacy", definition: "legacy TEXT" }],
        changedColumns: null,
        oldColumnOrder: null,
        newColumnOrder: undefined,
        addedConstraints: null,
        removedConstraints: null,
        addedIndexes: null,
        removedIndexes: null,
        changedOptions: null,
      }],
    });

    const table = result.changedTables[0];
    expect(table.removedColumns).toEqual([{ name: "legacy", definition: "legacy TEXT" }]);
    expect(table.identityChanged).toBeNull();
    expect(table.addedColumns).toEqual([]);
    expect(table.changedColumns).toEqual([]);
    expect(table.oldColumnOrder).toEqual([]);
    expect(table.newColumnOrder).toEqual([]);
    expect(table.addedConstraints).toEqual([]);
    expect(table.removedConstraints).toEqual([]);
    expect(table.addedIndexes).toEqual([]);
    expect(table.removedIndexes).toEqual([]);
    expect(table.changedOptions).toEqual([]);
  });

  it("rejects malformed arrays before they can enter component state", () => {
    const malformed: Array<[string, () => unknown, string]> = [
      ["grid", () => normalizeCsvGrid({ rows: "not-an-array" }), "CsvGridResult.rows"],
      ["inspection", () => normalizeCsvInspect({ candidates: {} }), "CsvInspectResult.candidates"],
      ["schema", () => normalizeCsvSchema({ columns: {} }), "CsvSchemaResult.columns"],
      ["preview", () => normalizeCsvPreview({ rows: "not-an-array" }), "CsvPreviewResult.rows"],
      ["profile", () => normalizeCsvProfile({ columns: {} }), "CsvProfileResult.columns"],
      ["SQL summary", () => normalizeSqlSummary({ tables: {} }), "SqlSummaryResult.tables"],
      ["SQL lint", () => normalizeSqlLint({ findings: {} }), "SqlLintResult.findings"],
      ["schema diff", () => normalizeSqlSchemaDiff({ changedTables: {} }), "SqlSchemaDiffResult.changedTables"],
    ];
    for (const [label, normalize, field] of malformed) {
      expect(normalize, label).toThrow(BridgePayloadError);
      expect(normalize, label).toThrow(`Invalid backend response at ${field}`);
    }
    expect(() => normalizeSqlSchemaDiff({ changedTables: [{ status: "invented" }] })).toThrow(
      "Invalid backend response at SqlSchemaDiffResult.changedTables[0].status",
    );
  });

  it("rejects SQL metadata above backend cardinality contracts before copying it into state", () => {
    expect(() => normalizeSqlSummary({ tables: Array.from({ length: 10_001 }, () => ({})) })).toThrow(
      "Invalid backend response at SqlSummaryResult.tables",
    );
    expect(() => normalizeSqlSchemaDiff({ addedTables: Array.from({ length: 10_001 }, () => "table") })).toThrow(
      "Invalid backend response at SqlSchemaDiffResult.addedTables",
    );
    expect(() => normalizeSqlSchemaDiff({
      changedTables: [{ addedColumns: Array.from({ length: 4_097 }, () => ({})) }],
    })).toThrow("Invalid backend response at SqlSchemaDiffResult.changedTables[0].addedColumns");
		expect(() => normalizeSqlLint({ findings: Array.from({ length: 9 }, () => ({})) })).toThrow(
			"Invalid backend response at SqlLintResult.findings",
		);
  });

  it("rejects CSV grid payloads above the bounded row, column, and cell contract", () => {
    expect(() => normalizeCsvGrid({ rows: Array.from({ length: 501 }, () => []) })).toThrow(
      "Invalid backend response at CsvGridResult.rows",
    );
    expect(() => normalizeCsvGrid({ rows: [Array.from({ length: 257 }, () => "cell")] })).toThrow(
      "Invalid backend response at CsvGridResult.rows[0]",
    );
    expect(() => normalizeCsvGrid({ rows: Array.from({ length: 100 }, () => Array.from({ length: 101 }, () => "cell")) })).toThrow(
      "Invalid backend response at CsvGridResult.rows",
    );
  });

  it("rejects structured CSV analysis payloads above parser and sample contracts", () => {
    expect(() => normalizeCsvInspect({ candidates: Array.from({ length: 17 }, () => ({})) })).toThrow(
      "Invalid backend response at CsvInspectResult.candidates",
    );
    expect(() => normalizeCsvSchema({ columns: Array.from({ length: 1_025 }, () => ({})) })).toThrow(
      "Invalid backend response at CsvSchemaResult.columns",
    );
    expect(() => normalizeCsvSchema({ columns: [{ samples: ["a", "b", "c", "d"] }] })).toThrow(
      "Invalid backend response at CsvSchemaResult.columns[0].samples",
    );
    expect(() => normalizeCsvPreview({ header: Array.from({ length: 1_025 }, () => "h") })).toThrow(
      "Invalid backend response at CsvPreviewResult.header",
    );
    expect(() => normalizeCsvPreview({ rows: Array.from({ length: 501 }, () => []) })).toThrow(
      "Invalid backend response at CsvPreviewResult.rows",
    );
    expect(() => normalizeCsvPreview({ rows: [Array.from({ length: 1_025 }, () => "cell")] })).toThrow(
      "Invalid backend response at CsvPreviewResult.rows[0]",
    );
    expect(() => normalizeCsvProfile({ columns: Array.from({ length: 1_025 }, () => ({})) })).toThrow(
      "Invalid backend response at CsvProfileResult.columns",
    );
    expect(() => normalizeCsvProfile({ columns: [{ top: Array.from({ length: 11 }, () => ({})) }] })).toThrow(
      "Invalid backend response at CsvProfileResult.columns[0].top",
    );
    expect(() => normalizeCsvPreview({ warnings: Array.from({ length: 4_097 }, () => "warning") })).toThrow(
      "Invalid backend response at CsvPreviewResult.warnings",
    );
  });
});
