import { describe, expect, it } from "vitest";
import {
  INFO_EXPIRY_MS,
  initialNotificationState,
  notificationDetailsText,
  notificationReducer,
} from "../src/notificationState";
import type { NotificationOwner, NotificationState } from "../src/notificationState";

function owner(sequence: number, operationId = `operation-${sequence}`, fileId?: string): NotificationOwner {
  return {
    operationId,
    sequence,
    operation: `Operation ${sequence}`,
    ...(fileId ? { fileId, path: `  /data/${fileId}  ` } : {}),
  };
}

function reserve(state: NotificationState, value: NotificationOwner): NotificationState {
  return notificationReducer(state, { type: "reserve", owner: value });
}

function publish(
  state: NotificationState,
  value: NotificationOwner,
  severity: "info" | "success" | "warning" | "error",
  message: string,
  timestamp = 1_000,
): NotificationState {
  return notificationReducer(state, {
    type: "publish",
    owner: value,
    input: { severity, message, timestamp },
  });
}

describe("operation notification ownership", () => {
  it("rejects stale sequences and a different operation claiming the latest sequence", () => {
    const first = owner(1);
    const second = owner(2);
    let state = reserve(initialNotificationState, first);
    state = publish(state, first, "info", "first started");
    state = reserve(state, second);

    const afterStale = publish(state, first, "error", "late first failure");
    expect(afterStale).toBe(state);

    const impostor = { ...second, operationId: "different-operation" };
    const afterImpostor = publish(state, impostor, "error", "wrong owner");
    expect(afterImpostor).toBe(state);

    state = publish(state, second, "success", "second completed");
    expect(state.current?.message).toBe("second completed");
    expect(state.current?.operationId).toBe(second.operationId);
  });

  it("lets the current owner update repeated errors without adding an expiry", () => {
    const current = owner(4);
    let state = reserve(initialNotificationState, current);
    state = publish(state, current, "error", "first failure", 100);
    state = publish(state, current, "error", "more specific failure", 200);

    expect(state.current).toMatchObject({
      severity: "error",
      message: "more specific failure",
      timestamp: 200,
    });
    expect(state.current?.expiresAt).toBeUndefined();
  });

  it("makes dismissal final for that exact owner while a new retry can publish", () => {
    const failed = owner(5);
    let state = reserve(initialNotificationState, failed);
    state = publish(state, failed, "error", "failed");
    state = notificationReducer(state, {
      type: "dismiss",
      operationId: failed.operationId,
      sequence: failed.sequence,
    });
    expect(state.current).toBeNull();
    expect(publish(state, failed, "error", "late repeat")).toBe(state);

    const retry = owner(6);
    state = reserve(state, retry);
    state = publish(state, retry, "success", "retry completed");
    expect(state.current?.message).toBe("retry completed");
  });

  it("expires info once, prevents resurrection, and permits a fresh terminal owner", () => {
    const started = owner(7);
    let state = reserve(initialNotificationState, started);
    state = publish(state, started, "info", "started", 5_000);
    expect(state.current?.expiresAt).toBe(5_000 + INFO_EXPIRY_MS);

    const tooEarly = notificationReducer(state, {
      type: "expire",
      operationId: started.operationId,
      sequence: started.sequence,
      now: 5_000 + INFO_EXPIRY_MS - 1,
    });
    expect(tooEarly).toBe(state);

    state = notificationReducer(state, {
      type: "expire",
      operationId: started.operationId,
      sequence: started.sequence,
      now: 5_000 + INFO_EXPIRY_MS,
    });
    expect(state.current).toBeNull();
    expect(publish(state, started, "success", "late completion")).toBe(state);

    const terminal = owner(8, "job-terminal-8");
    state = reserve(state, terminal);
    state = publish(state, terminal, "success", "completed");
    expect(state.current?.message).toBe("completed");
  });

  it("clears a file-scoped notification on file switch and rejects its late update", () => {
    const fileA = owner(9, "file-a-operation", "file-a");
    let state = reserve(initialNotificationState, fileA);
    state = publish(state, fileA, "warning", "retrying");
    state = notificationReducer(state, { type: "active-file", fileId: "file-b" });
    expect(state.current).toBeNull();
    expect(publish(state, fileA, "error", "late file A failure")).toBe(state);
  });

  it("suppresses a reserved file owner when the file switches before first publish", () => {
    const fileA = owner(11, "pending-file-a", "file-a");
    let state = reserve(initialNotificationState, fileA);
    state = notificationReducer(state, { type: "active-file", fileId: "file-b" });
    expect(state.current).toBeNull();
    expect(publish(state, fileA, "error", "late pending failure")).toBe(state);
  });

  it("bounds diagnostic fields, removes NULs, and preserves exact path whitespace", () => {
    const value: NotificationOwner = {
      operationId: `  operation\0${"x".repeat(300)}  `,
      sequence: 10,
      operation: `Inspect\0${"o".repeat(200)}`,
      fileId: `file-${"f".repeat(300)}`,
      path: `  /tmp/${"p".repeat(2_000)}  `,
    };
    let state = reserve(initialNotificationState, value);
    state = notificationReducer(state, {
      type: "publish",
      owner: value,
      input: {
        severity: "error",
        message: `bad\0${"m".repeat(2_000)}`,
        details: `detail\0${"d".repeat(8_000)}`,
        timestamp: 0,
      },
    });

    const current = state.current!;
    expect(current.operationId.length).toBeLessThanOrEqual(160);
    expect(current.operation.length).toBeLessThanOrEqual(100);
    expect(current.fileId?.length).toBeLessThanOrEqual(160);
    expect(current.path?.length).toBeLessThanOrEqual(1_024);
    expect(current.path?.startsWith("  /tmp/")).toBe(true);
    expect(current.message.length).toBeLessThanOrEqual(600);
    expect(current.details?.length).toBeLessThanOrEqual(4_096);
    expect(notificationDetailsText(current)).not.toContain("\0");
  });

  it("clamps hostile finite timestamps so copied diagnostics cannot throw", () => {
    const value = owner(12);
    let state = reserve(initialNotificationState, value);
    state = notificationReducer(state, {
      type: "publish",
      owner: value,
      input: { severity: "error", message: "bad timestamp", timestamp: Number.MAX_VALUE },
    });
    expect(state.current?.timestamp).toBe(8_640_000_000_000_000);
    expect(() => notificationDetailsText(state.current!)).not.toThrow();
    expect(notificationDetailsText(state.current!)).toContain("+275760-09-13T00:00:00.000Z");
  });
});
