import { describe, expect, it } from "vitest";
import { normalizeSqlSummary } from "../src/bridgePayloads";
import { cacheSqlAnalysis, MAX_SQL_ANALYSIS_CACHE_ENTRIES } from "../src/sqlAnalysisCache";

describe("SQL analysis cache", () => {
  it("retains only the newest bounded summaries and refreshes recency", () => {
    let cache = {};
    for (let index = 0; index < MAX_SQL_ANALYSIS_CACHE_ENTRIES; index++) {
      cache = cacheSqlAnalysis(cache, `file-${index}`, normalizeSqlSummary({ tables: [{ name: `t${index}` }] }));
    }
    cache = cacheSqlAnalysis(cache, "file-0", normalizeSqlSummary({ tables: [{ name: "newest" }] }));
    cache = cacheSqlAnalysis(cache, "file-extra", normalizeSqlSummary({ tables: [] }));

    expect(Object.keys(cache)).toHaveLength(MAX_SQL_ANALYSIS_CACHE_ENTRIES);
    expect(cache["file-0"].tables[0].name).toBe("newest");
    expect(cache["file-1"]).toBeUndefined();
    expect(cache["file-extra"]).toBeDefined();
  });
});
