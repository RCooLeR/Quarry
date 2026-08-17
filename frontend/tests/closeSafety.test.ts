import { describe, expect, it, vi } from "vitest";
import type { StagingStateData } from "../src/editor/QuarryEditor";
import {
  closeBackendSession,
  resolveApplicationClose,
  resolveTabForClose,
  type CloseSafetyDependencies,
  type CloseTarget,
} from "../src/closeSafety";

const clean: StagingStateData = {
  editCount: 0,
  originalSize: 10,
  editedSize: 10,
  netDelta: 0,
  lengthPreserving: true,
  inPlaceEligible: false,
};

const staged: StagingStateData = { ...clean, editCount: 1 };

function target(overrides: Partial<CloseTarget> = {}): CloseTarget {
  return {
    fileId: "file-a",
    path: "C:\\data\\a.txt",
    dirty: false,
    editorAttached: true,
    scope: "tab",
    ...overrides,
  };
}

function dependencies(overrides: Partial<CloseSafetyDependencies> = {}): CloseSafetyDependencies {
  return {
    flushEditor: vi.fn().mockResolvedValue(undefined),
    getStaging: vi.fn().mockResolvedValue(staged),
    choose: vi.fn().mockResolvedValue("cancel"),
    saveCopy: vi.fn().mockResolvedValue({ mode: "copy", outputPath: "C:\\out\\a.txt", bytesWritten: 10 }),
    makeEditorReadOnly: vi.fn().mockResolvedValue(undefined),
    discard: vi.fn().mockResolvedValue(clean),
    onStaging: vi.fn(),
    ...overrides,
  };
}

describe("resolveTabForClose", () => {
  it("drains a debounce-pending active edit before declaring the tab clean", async () => {
    const deps = dependencies({ getStaging: vi.fn().mockResolvedValue(clean) });

    await expect(resolveTabForClose(target({ dirty: true }), deps)).resolves.toEqual({ outcome: "clean" });
    expect(deps.flushEditor).toHaveBeenCalledOnce();
    expect(deps.getStaging).toHaveBeenCalledWith("file-a");
    expect(deps.choose).not.toHaveBeenCalled();
  });

  it("fails closed on flush failure without inspecting or closing over the session", async () => {
    const failure = new Error("stage rejected");
    const deps = dependencies({ flushEditor: vi.fn().mockRejectedValue(failure) });

    await expect(resolveTabForClose(target({ dirty: true }), deps)).rejects.toBe(failure);
    expect(deps.getStaging).not.toHaveBeenCalled();
    expect(deps.choose).not.toHaveBeenCalled();
    expect(deps.saveCopy).not.toHaveBeenCalled();
    expect(deps.discard).not.toHaveBeenCalled();
  });

  it("refuses an impossible inactive dirty state", async () => {
    const deps = dependencies();

    await expect(resolveTabForClose(target({ dirty: true, editorAttached: false }), deps))
      .rejects.toThrow("cannot close inactive dirty session file-a");
    expect(deps.flushEditor).not.toHaveBeenCalled();
    expect(deps.getStaging).not.toHaveBeenCalled();
  });

  it("treats native Save copy cancellation as close cancellation", async () => {
    const deps = dependencies({
      choose: vi.fn().mockResolvedValue("save-copy"),
      saveCopy: vi.fn().mockResolvedValue({ mode: "", outputPath: "", bytesWritten: 0 }),
    });

    await expect(resolveTabForClose(target(), deps)).resolves.toEqual({ outcome: "cancelled" });
    expect(deps.flushEditor).toHaveBeenCalledTimes(2);
    expect(deps.discard).not.toHaveBeenCalled();
  });

  it("preserves staging when the second pre-save flush fails", async () => {
    const failure = new Error("second flush failed");
    const deps = dependencies({
      flushEditor: vi.fn()
        .mockResolvedValueOnce(undefined)
        .mockRejectedValueOnce(failure),
      choose: vi.fn().mockResolvedValue("save-copy"),
    });

    await expect(resolveTabForClose(target(), deps)).rejects.toBe(failure);
    expect(deps.flushEditor).toHaveBeenCalledTimes(2);
    expect(deps.saveCopy).not.toHaveBeenCalled();
    expect(deps.discard).not.toHaveBeenCalled();
    expect(deps.makeEditorReadOnly).not.toHaveBeenCalled();
    expect(deps.onStaging).toHaveBeenCalledTimes(1);
    expect(deps.onStaging).toHaveBeenLastCalledWith("file-a", staged);
  });

  it("requires a completed copy before resolving a save decision", async () => {
    const order: string[] = [];
    const deps = dependencies({
      choose: vi.fn().mockResolvedValue("save-copy"),
      saveCopy: vi.fn(async () => {
        order.push("save");
        return { mode: "copy", outputPath: "C:\\out\\a.txt", bytesWritten: 10 };
      }),
      makeEditorReadOnly: vi.fn(async () => {
        order.push("read-only");
      }),
      discard: vi.fn(async () => {
        order.push("discard");
        return clean;
      }),
    });

    const result = await resolveTabForClose(target(), deps);
    expect(result.outcome).toBe("saved");
    expect(deps.saveCopy).toHaveBeenCalledWith("file-a");
    expect(deps.discard).toHaveBeenCalledWith("file-a");
    expect(deps.onStaging).toHaveBeenCalledWith("file-a", clean);
    await closeBackendSession(
      "file-a",
      vi.fn(async () => { order.push("backend-close"); }),
      vi.fn(() => { order.push("remove-tab"); }),
    );
    expect(order).toEqual(["save", "read-only", "discard", "backend-close", "remove-tab"]);
  });

  it("preserves staging when Save copy fails", async () => {
    const failure = new Error("copy failed");
    const deps = dependencies({
      choose: vi.fn().mockResolvedValue("save-copy"),
      saveCopy: vi.fn().mockRejectedValue(failure),
    });

    await expect(resolveTabForClose(target(), deps)).rejects.toBe(failure);
    expect(deps.discard).not.toHaveBeenCalled();
    expect(deps.makeEditorReadOnly).not.toHaveBeenCalled();
    expect(deps.onStaging).toHaveBeenCalledTimes(1);
    expect(deps.onStaging).toHaveBeenLastCalledWith("file-a", staged);
  });

  it("clears only the saved inactive session without changing another editor", async () => {
    const deps = dependencies({ choose: vi.fn().mockResolvedValue("save-copy") });

    await expect(resolveTabForClose(target({
      fileId: "file-b",
      path: "C:\\data\\b.txt",
      editorAttached: false,
    }), deps)).resolves.toMatchObject({ outcome: "saved" });
    expect(deps.saveCopy).toHaveBeenCalledWith("file-b");
    expect(deps.discard).toHaveBeenCalledWith("file-b");
    expect(deps.onStaging).toHaveBeenLastCalledWith("file-b", clean);
    expect(deps.makeEditorReadOnly).not.toHaveBeenCalled();
  });

  it("keeps the tab open when post-save staging cannot be cleared", async () => {
    const deps = dependencies({
      choose: vi.fn().mockResolvedValue("save-copy"),
      discard: vi.fn().mockResolvedValue(staged),
    });

    await expect(resolveTabForClose(target(), deps))
      .rejects.toThrow("discard after save did not clear staged edits for file-a");
    expect(deps.makeEditorReadOnly).toHaveBeenCalledOnce();
  });

  it("preserves backend staging when the editor cannot become read-only after Save copy", async () => {
    const readOnlyFailure = new Error("read-only reload failed");
    const order: string[] = [];
    const deps = dependencies({
      choose: vi.fn().mockResolvedValue("save-copy"),
      saveCopy: vi.fn(async () => {
        order.push("save");
        return { mode: "copy", outputPath: "C:\\out\\a.txt", bytesWritten: 10 };
      }),
      makeEditorReadOnly: vi.fn(async () => {
        order.push("read-only");
        throw readOnlyFailure;
      }),
      discard: vi.fn(async () => {
        order.push("discard");
        return clean;
      }),
    });

    await expect(resolveTabForClose(target(), deps)).rejects.toBe(readOnlyFailure);
    expect(order).toEqual(["save", "read-only"]);
    expect(deps.discard).not.toHaveBeenCalled();
    expect(deps.onStaging).toHaveBeenLastCalledWith("file-a", staged);
  });

  it("makes the attached editor read-only before an explicit discard", async () => {
    const order: string[] = [];
    const deps = dependencies({
      choose: vi.fn().mockResolvedValue("discard"),
      makeEditorReadOnly: vi.fn(async () => {
        order.push("read-only");
      }),
      discard: vi.fn(async () => {
        order.push("discard");
        return clean;
      }),
    });

    await expect(resolveTabForClose(target(), deps)).resolves.toEqual({ outcome: "discarded" });
    expect(order).toEqual(["read-only", "discard"]);
    expect(deps.onStaging).toHaveBeenLastCalledWith("file-a", clean);
  });

  it("does not clear staging when the attached editor cannot become read-only", async () => {
    const readOnlyFailure = new Error("read-only reload failed");
    const deps = dependencies({
      choose: vi.fn().mockResolvedValue("discard"),
      makeEditorReadOnly: vi.fn().mockRejectedValue(readOnlyFailure),
    });

    await expect(resolveTabForClose(target(), deps)).rejects.toBe(readOnlyFailure);
    expect(deps.discard).not.toHaveBeenCalled();
    expect(deps.onStaging).toHaveBeenLastCalledWith("file-a", staged);
  });

  it("does not change an inactive editor before discard", async () => {
    const deps = dependencies({ choose: vi.fn().mockResolvedValue("discard") });

    await expect(resolveTabForClose(target({ editorAttached: false }), deps)).resolves.toEqual({ outcome: "discarded" });
    expect(deps.flushEditor).not.toHaveBeenCalled();
    expect(deps.makeEditorReadOnly).not.toHaveBeenCalled();
  });
});

describe("closeBackendSession", () => {
  it("removes the tab only after backend close succeeds", async () => {
    const order: string[] = [];
    await closeBackendSession(
      "file-a",
      vi.fn(async () => { order.push("backend"); }),
      vi.fn(() => { order.push("remove"); }),
    );
    expect(order).toEqual(["backend", "remove"]);
  });

  it("keeps the tab when backend close fails", async () => {
    const failure = new Error("close failed");
    const remove = vi.fn();
    await expect(closeBackendSession("file-a", vi.fn().mockRejectedValue(failure), remove)).rejects.toBe(failure);
    expect(remove).not.toHaveBeenCalled();
  });
});

describe("resolveApplicationClose", () => {
  it("resolves multiple dirty tabs serially before approval", async () => {
    const calls: string[] = [];
    const result = await resolveApplicationClose(["a", "b", "c"], async (id, position, total) => {
      calls.push(`${id}:${position}/${total}`);
      return { outcome: id === "a" ? "saved" : "discarded", ...(id === "a" ? { save: { mode: "copy", outputPath: "out", bytesWritten: 1 } } : {}) } as Awaited<ReturnType<typeof resolveTabForClose>>;
    });

    expect(result).toBe(true);
    expect(calls).toEqual(["a:1/3", "b:2/3", "c:3/3"]);
  });

  it("stops at Cancel and leaves later tabs unresolved", async () => {
    const resolve = vi.fn(async (id: string) => ({ outcome: id === "b" ? "cancelled" : "clean" }) as Awaited<ReturnType<typeof resolveTabForClose>>);

    await expect(resolveApplicationClose(["a", "b", "c"], resolve)).resolves.toBe(false);
    expect(resolve).toHaveBeenCalledTimes(2);
    expect(resolve).not.toHaveBeenCalledWith("c", expect.anything(), expect.anything());
  });

  it("leaves an earlier discarded editor read-only when a later tab cancels application close", async () => {
    const order: string[] = [];
    let editorReadOnly = false;
    const first = dependencies({
      choose: vi.fn().mockResolvedValue("discard"),
      makeEditorReadOnly: vi.fn(async () => {
        editorReadOnly = true;
        order.push("read-only:a");
      }),
      discard: vi.fn(async () => {
        expect(editorReadOnly).toBe(true);
        order.push("discard:a");
        return clean;
      }),
    });
    const second = dependencies({ choose: vi.fn().mockResolvedValue("cancel") });

    const result = await resolveApplicationClose(["a", "b"], async (fileId) => (
      fileId === "a"
        ? resolveTabForClose(target({ fileId, scope: "application" }), first)
        : resolveTabForClose(target({ fileId, editorAttached: false, scope: "application" }), second)
    ));

    expect(result).toBe(false);
    expect(editorReadOnly).toBe(true);
    expect(order).toEqual(["read-only:a", "discard:a"]);
    expect(second.discard).not.toHaveBeenCalled();
  });
});
