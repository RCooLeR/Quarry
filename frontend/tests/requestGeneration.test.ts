import { describe, expect, it } from "vitest";
import { RequestGenerationGate } from "../src/requestGeneration";

interface Deferred<T> {
  promise: Promise<T>;
  resolve: (value: T) => void;
}

function deferred<T>(): Deferred<T> {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((done) => { resolve = done; });
  return { promise, resolve };
}

describe("RequestGenerationGate", () => {
  it("accepts only the newest response on an active request channel", () => {
    const gate = new RequestGenerationGate();
    const first = gate.beginActive("search", "file-a");
    const second = gate.beginActive("search", "file-a");

    expect(second.generation).toBeGreaterThan(first.generation);
    expect(gate.isActive(first, "file-a", "file-a")).toBe(false);
    expect(gate.isActive(second, "file-a", "file-a")).toBe(true);
  });

  it("rejects a response when either active or editor ownership differs", () => {
    const gate = new RequestGenerationGate();
    const request = gate.beginActive("goto", "file-a");

    expect(gate.isActive(request, "file-b", "file-a")).toBe(false);
    expect(gate.isActive(request, "file-a", "file-b")).toBe(false);
    expect(gate.isActive(request, "file-a", "file-a")).toBe(true);
  });

  it("uses an active epoch so an A to B to A switch cannot revive A's old response", () => {
    const gate = new RequestGenerationGate();
    const oldA = gate.beginActive("diff", "file-a");

    gate.invalidateActive(); // A -> B
    gate.invalidateActive(); // B -> A

    expect(gate.isLatest(oldA)).toBe(true);
    expect(gate.isActive(oldA, "file-a", "file-a")).toBe(false);
    expect(gate.isActive(gate.beginActive("diff", "file-a"), "file-a", "file-a")).toBe(true);
  });

  it("orders file-scoped responses independently for each session", () => {
    const gate = new RequestGenerationGate();
    const firstA = gate.beginFile("staging", "file-a");
    const onlyB = gate.beginFile("staging", "file-b");
    const secondA = gate.beginFile("staging", "file-a");

    expect(gate.isLatest(firstA)).toBe(false);
    expect(gate.isLatest(secondA)).toBe(true);
    expect(gate.isLatest(onlyB)).toBe(true);
  });

  it("invalidates and releases file-scoped state when a session closes", () => {
    const gate = new RequestGenerationGate();
    const request = gate.beginFile("staging", "file-a");

    gate.forgetFile("staging", "file-a");

    expect(gate.isLatest(request)).toBe(false);
  });

  it("invalidates captured child-component callbacks on a file transition", () => {
    const gate = new RequestGenerationGate();
    const toolsForA = gate.captureActive("file-a");

    expect(gate.isActiveContext(toolsForA, "file-a", "file-a")).toBe(true);
    gate.invalidateActive();
    expect(gate.isActiveContext(toolsForA, "file-a", "file-a")).toBe(false);
  });

  it("can explicitly supersede a pending search after query options change", () => {
    const gate = new RequestGenerationGate();
    const request = gate.beginActive("search", "file-a");

    gate.invalidateChannel("search");

    expect(gate.isActive(request, "file-a", "file-a")).toBe(false);
  });

  for (const [label, channel] of [
    ["search", "search"],
    ["diff", "diff"],
    ["go-to", "goto"],
    ["save", "editor-operation"],
  ] as const) {
    it(`ignores an out-of-order file A ${label} result after file B becomes active`, async () => {
      const gate = new RequestGenerationGate();
      let activeFileId = "file-a";
      let editorFileId = "file-a";
      const applied: string[] = [];
      const responseA = deferred<string>();
      const requestA = gate.beginActive(channel, "file-a");
      const applyA = responseA.promise.then((value) => {
        if (gate.isActive(requestA, activeFileId, editorFileId)) applied.push(value);
      });

      gate.invalidateActive();
      activeFileId = "file-b";
      editorFileId = "file-b";
      const responseB = deferred<string>();
      const requestB = gate.beginActive(channel, "file-b");
      const applyB = responseB.promise.then((value) => {
        if (gate.isActive(requestB, activeFileId, editorFileId)) applied.push(value);
      });

      responseB.resolve("B");
      await applyB;
      responseA.resolve("A");
      await applyA;

      expect(applied).toEqual(["B"]);
    });
  }
});
