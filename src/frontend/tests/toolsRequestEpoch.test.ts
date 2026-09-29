import { describe, expect, it } from "vitest";
import { ToolsRequestEpochGate } from "../src/toolsRequestEpoch";

describe("ToolsRequestEpochGate", () => {
  it("rejects requests after unmount and does not revive them after remount", () => {
    const gate = new ToolsRequestEpochGate("file-a", "csv");
    const request = gate.begin("profile", { csvConfiguration: true, sourceGeneration: 4 });

    gate.unmount();
    expect(gate.isCurrent(request, 4)).toBe(false);
    gate.mount();
    expect(gate.isCurrent(request, 4)).toBe(false);
  });

  it("binds requests to file/type context, including an A to B to A transition", () => {
    const gate = new ToolsRequestEpochGate("file-a", "csv");
    const oldA = gate.begin("schema");

    gate.syncContext("file-b", "csv");
    gate.syncContext("file-a", "csv");

    expect(gate.isCurrent(oldA)).toBe(false);
    expect(gate.isCurrent(gate.begin("schema"))).toBe(true);
  });

  it("rejects CSV work after either configuration or source generation changes", () => {
    const gate = new ToolsRequestEpochGate("file-a", "csv");
    const configured = gate.begin("profile", { csvConfiguration: true, sourceGeneration: 7 });

    expect(gate.isCurrent(configured, 8)).toBe(false);
    gate.advanceCsvConfiguration();
    expect(gate.isCurrent(configured, 7)).toBe(false);
  });

  it("orders each request channel and supports explicit target invalidation", () => {
    const gate = new ToolsRequestEpochGate("sql-a", "sql");
    const first = gate.begin("schema-diff");
    const second = gate.begin("schema-diff");

    expect(gate.isCurrent(first)).toBe(false);
    expect(gate.isCurrent(second)).toBe(true);
    gate.invalidateChannel("schema-diff");
    expect(gate.isCurrent(second)).toBe(false);
  });
});
