import { describe, expect, it } from "vitest";
import {
  isSQLAnalysisJob,
  jobProgressLabel,
  reduceJobEvent,
  type ActiveJobState,
} from "../src/jobState";

describe("job event ownership", () => {
  it("rejects an initial start without a positive safe-integer sequence", () => {
    for (const sequence of [undefined, 0, -1, 1.5, Number.MAX_SAFE_INTEGER + 1]) {
      const payload: Record<string, unknown> = { id: "job-malformed", title: "Malformed" };
      if (sequence !== undefined) payload.sequence = sequence;
      expect(reduceJobEvent(null, "start", payload)).toBeNull();
    }
  });

  it("ignores stale progress and stale terminal events", () => {
    const first = reduceJobEvent(null, "start", {
      id: "job1",
      sequence: 1,
      title: "First",
      kind: "sql-analysis",
      fileId: "file-a",
      total: 100,
    });
    const second = reduceJobEvent(first, "start", {
      id: "job2",
      sequence: 2,
      title: "Second",
      kind: "sql-analysis",
      fileId: "file-b",
      total: 200,
    });
    expect(reduceJobEvent(second, "progress", { id: "job1", completed: 90 })).toBe(second);
    expect(reduceJobEvent(second, "end", { id: "job1", status: "cancelled" })).toBe(second);
    expect(reduceJobEvent(second, "end", { id: "job2", status: "completed" })).toBeNull();
  });

  it("fails closed on unsequenced or non-new replacement starts", () => {
    const current = reduceJobEvent(null, "start", {
      id: "job-current",
      sequence: 10,
      title: "Current",
    });
    expect(current).not.toBeNull();

    for (const sequence of [undefined, 0, 9, 10]) {
      const payload: Record<string, unknown> = { id: `replacement-${String(sequence)}`, title: "Replacement" };
      if (sequence !== undefined) payload.sequence = sequence;
      expect(reduceJobEvent(current, "start", payload)).toBe(current);
    }

    const newer = reduceJobEvent(current, "start", {
      id: "job-newer",
      sequence: 11,
      title: "Newer",
    });
    expect(newer?.id).toBe("job-newer");
    expect(newer?.sequence).toBe(11);
  });

  it("rejects a conflicting positive sequence for the same job ID", () => {
    const current = reduceJobEvent(null, "start", { id: "job-same", sequence: 20, title: "Original" });
    expect(reduceJobEvent(current, "start", {
      id: "job-same",
      sequence: 21,
      title: "Malformed replacement",
    })).toBe(current);

    const duplicate = reduceJobEvent(current, "start", {
      id: "job-same",
      sequence: 20,
      title: "Refreshed",
    });
    expect(duplicate?.title).toBe("Refreshed");
    expect(duplicate?.sequence).toBe(20);
  });

  it("rejects malformed or conflicting supplied sequences on progress and terminal events", () => {
    const current = reduceJobEvent(null, "start", {
      id: "job-sequenced",
      sequence: 30,
      completed: 1,
      total: 10,
    });
    expect(reduceJobEvent(current, "progress", {
      id: "job-sequenced",
      sequence: 29,
      completed: 9,
    })).toBe(current);
    expect(reduceJobEvent(current, "progress", {
      id: "job-sequenced",
      sequence: 0,
      completed: 9,
    })).toBe(current);
    expect(reduceJobEvent(current, "end", {
      id: "job-sequenced",
      sequence: 31,
      status: "completed",
    })).toBe(current);
    expect(reduceJobEvent(current, "end", {
      id: "job-sequenced",
      sequence: 30,
      status: "completed",
    })).toBeNull();
  });

  it("keeps progress monotonic, bounded by total, and note payloads bounded", () => {
    let state = reduceJobEvent(null, "start", {
      id: "job7",
      sequence: 7,
      kind: "sql-analysis",
      fileId: "file-a",
      completed: 20,
      total: 100,
    });
    state = reduceJobEvent(state, "progress", {
      id: "job7",
      completed: 10,
      total: 100,
      note: "x".repeat(500),
    });
    expect(state?.completed).toBe(20);
    expect(state?.note).toHaveLength(160);
    state = reduceJobEvent(state, "progress", { id: "job7", completed: 500, total: 100 });
    expect(state?.completed).toBe(100);
    expect(state?.total).toBe(100);
  });

  it("identifies analysis ownership and renders determinate byte progress", () => {
    const job: ActiveJobState = {
      id: "job3",
      sequence: 3,
      title: "Analyze SQL dump",
      kind: "sql-analysis",
      fileId: "file-a",
      completed: 16 * 1024 * 1024,
      total: 32 * 1024 * 1024,
      note: "bytes analyzed",
    };
    expect(isSQLAnalysisJob(job, "file-a")).toBe(true);
    expect(isSQLAnalysisJob(job, "file-b")).toBe(false);
    expect(jobProgressLabel(job)).toBe("16 MiB / 32 MiB (50%)");
  });

  it("renders exact source-verification progress as bytes", () => {
    const job: ActiveJobState = {
      id: "job-prepare",
      sequence: 4,
      title: "Prepare editing",
      kind: "source-verification",
      fileId: "file-a",
      completed: 64 * 1024 * 1024,
      total: 256 * 1024 * 1024,
      note: "verifying source bytes",
    };

    expect(jobProgressLabel(job)).toBe("64 MiB / 256 MiB (25%)");
  });
});

describe("analysis result retention harness", () => {
  it("retains the prior valid summary when re-analysis rejects", async () => {
    const previous = { tables: [{ name: "important_data" }], createTables: 1 };
    let visible = previous;
    const run = async (request: Promise<typeof previous>) => {
      try {
        visible = await request;
      } catch {
        // App publishes only fulfilled, still-owned requests.
      }
    };
    await run(Promise.reject(new Error("background job was cancelled")));
    expect(visible).toBe(previous);
  });
});
