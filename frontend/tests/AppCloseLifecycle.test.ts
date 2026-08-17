import React, { act } from "react";
import { createRoot } from "react-dom/client";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const runtime = vi.hoisted(() => ({
  listeners: new Map<string, (event?: unknown) => void>(),
  on: vi.fn((name: string, callback: (event?: unknown) => void) => {
    runtime.listeners.set(name, callback);
    return () => runtime.listeners.delete(name);
  }),
}));

const lifecycle = vi.hoisted(() => ({
  ApproveClose: vi.fn(),
  CancelClose: vi.fn(),
}));

const backend = vi.hoisted(() => ({
  DiscardEdits: vi.fn(),
  GetStagingState: vi.fn(),
  OpenFile: vi.fn(),
  OpenViaDialog: vi.fn(),
  PrepareEditSession: vi.fn(),
  ReleaseCleanEditSession: vi.fn(),
  SaveCopyViaDialog: vi.fn(),
}));

const editor = vi.hoisted(() => ({
  attach: vi.fn(),
  callbacks: null as null | { onMode: (enabled: boolean) => void },
  flush: vi.fn(),
  setEditMode: vi.fn(),
}));

vi.mock("@wailsio/runtime", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@wailsio/runtime")>();
  return { ...actual, Events: { ...actual.Events, On: runtime.on } };
});
vi.mock("../bindings/github.com/quarry/quarry-wails3", () => ({
  AppLifecycle: lifecycle,
  FileService: backend,
}));
vi.mock("../src/editor/QuarryEditor", () => ({
  QuarryEditor: class {
    constructor(_host: HTMLElement, callbacks: { onMode: (enabled: boolean) => void }) {
      editor.callbacks = callbacks;
    }

    async attach(...args: unknown[]): Promise<void> {
      await editor.attach(...args);
    }

    async flush(): Promise<void> {
      await editor.flush();
    }

    async refresh(): Promise<void> {}
    clear(): void {}
    destroy(): void {}
    setCsvDelimiter(): void {}
    async setEditMode(enabled: boolean): Promise<void> {
      await editor.setEditMode(enabled);
      editor.callbacks?.onMode(enabled);
    }
    setTheme(): void {}
    setShowByteOffsets(): void {}
  },
}));

import App from "../src/App";

const cleanStaging = {
  editCount: 0,
  originalSize: 1024,
  editedSize: 1024,
  netDelta: 0,
  lengthPreserving: true,
  inPlaceEligible: false,
};

const staged = {
  ...cleanStaging,
  editCount: 1,
  editedSize: 1032,
  netDelta: 8,
  lengthPreserving: false,
};

async function settle(): Promise<void> {
  for (let index = 0; index < 12; index++) await Promise.resolve();
}

describe("native application-close lifecycle integration", () => {
  let host: HTMLDivElement;
  let root: ReturnType<typeof createRoot>;

  beforeEach(() => {
    localStorage.clear();
    runtime.listeners.clear();
    vi.clearAllMocks();
    lifecycle.ApproveClose.mockResolvedValue(undefined);
    lifecycle.CancelClose.mockResolvedValue(undefined);
    backend.GetStagingState.mockResolvedValue(cleanStaging);
    backend.DiscardEdits.mockResolvedValue(cleanStaging);
    backend.PrepareEditSession.mockResolvedValue(cleanStaging);
    backend.ReleaseCleanEditSession.mockResolvedValue(cleanStaging);
    backend.SaveCopyViaDialog.mockResolvedValue({ mode: "", outputPath: "", bytesWritten: 0 });
    backend.OpenViaDialog.mockResolvedValue({
      fileId: "important",
      path: "C:\\data\\important.sql",
      size: 1024,
      encoding: "UTF-8",
      detected: "sql",
      binary: false,
      editable: true,
    });
    backend.OpenFile.mockImplementation(async (path: string) => ({
      fileId: "dropped",
      path,
      size: 1024,
      encoding: "UTF-8",
      detected: "sql",
      binary: false,
      editable: true,
    }));
    editor.attach.mockResolvedValue(undefined);
    editor.callbacks = null;
    editor.flush.mockResolvedValue(undefined);
    editor.setEditMode.mockResolvedValue(undefined);
    host = document.createElement("div");
    document.body.append(host);
    root = createRoot(host);
  });

  afterEach(() => {
    act(() => root.unmount());
    host.remove();
    localStorage.clear();
  });

  async function renderApp(): Promise<void> {
    await act(async () => {
      root.render(React.createElement(App));
      await settle();
    });
    expect(runtime.listeners.has("quarry:close-requested")).toBe(true);
  }

  async function requestNativeClose(): Promise<void> {
    await act(async () => {
      runtime.listeners.get("quarry:close-requested")?.();
      await settle();
    });
  }

  it("approves the already-blocked native request only after a clean review", async () => {
    await renderApp();
    await requestNativeClose();

    expect(lifecycle.ApproveClose).toHaveBeenCalledOnce();
    expect(lifecycle.CancelClose).not.toHaveBeenCalled();
  });

  it("validates native drop envelopes and reports all omitted entries once", async () => {
    await renderApp();
    expect(runtime.listeners.has("quarry:files-dropped")).toBe(true);

    await act(async () => {
      runtime.listeners.get("quarry:files-dropped")?.({
        data: {
          paths: ["C:\\data\\dropped.sql", 42, ""],
          omitted: 2,
        },
      });
      await settle();
    });

    expect(backend.OpenFile).toHaveBeenCalledOnce();
    expect(backend.OpenFile).toHaveBeenCalledWith("C:\\data\\dropped.sql");
    const notices = host.querySelectorAll<HTMLElement>(".q-notification");
    expect(notices).toHaveLength(1);
    expect(notices[0].textContent).toContain("4 dropped files were omitted");
  });

  it("cancels the native request and keeps a staged tab when the user cancels", async () => {
    backend.GetStagingState.mockResolvedValue(staged);
    await renderApp();

    await act(async () => {
      host.querySelector<HTMLButtonElement>(".q-top .q-btn-primary")?.click();
      await settle();
    });
    expect(editor.attach).toHaveBeenCalled();

    const originalFocus = host.querySelector<HTMLButtonElement>('button[aria-label="Edit mode"]');
    originalFocus?.focus();
    expect(document.activeElement).toBe(originalFocus);

    let resolveReview!: (value: typeof staged) => void;
    backend.GetStagingState.mockReturnValueOnce(new Promise((resolve) => {
      resolveReview = resolve;
    }));
    await requestNativeClose();
    const review = host.querySelector<HTMLElement>('.q-close-progress[role="dialog"]');
    expect(review).not.toBeNull();
    expect(document.activeElement).toBe(review);

    await act(async () => {
      resolveReview(staged);
      await settle();
    });
    const dialog = host.querySelector<HTMLElement>('.q-close-dialog[role="dialog"]');
    const cancel = Array.from(dialog?.querySelectorAll<HTMLButtonElement>("button") ?? [])
      .find((button) => button.textContent === "Cancel");
    expect(cancel).toBeDefined();

    await act(async () => {
      cancel?.click();
      await settle();
    });

    expect(editor.flush).toHaveBeenCalledOnce();
    expect(lifecycle.ApproveClose).not.toHaveBeenCalled();
    expect(lifecycle.CancelClose).toHaveBeenCalledOnce();
    expect(backend.DiscardEdits).not.toHaveBeenCalled();
    expect(host.querySelector('button[aria-label^="important.sql,"]')).not.toBeNull();
    expect(document.activeElement).toBe(originalFocus);
  });

  it("keeps an earlier discarded active editor read-only when a later tab cancels application close", async () => {
    backend.GetStagingState.mockResolvedValue(staged);
    backend.PrepareEditSession.mockResolvedValue(staged);
    await renderApp();

    await act(async () => {
      host.querySelector<HTMLButtonElement>(".q-top .q-btn-primary")?.click();
      await settle();
    });
    backend.OpenViaDialog.mockResolvedValueOnce({
      fileId: "second",
      path: "C:\\data\\second.sql",
      size: 2048,
      encoding: "UTF-8",
      detected: "sql",
      binary: false,
      editable: true,
    });
    await act(async () => {
      Array.from(host.querySelectorAll<HTMLButtonElement>(".q-top button"))
        .find((button) => button.textContent?.includes("Open file"))?.click();
      await settle();
      host.querySelector<HTMLButtonElement>('button[aria-label="Edit mode"]')?.click();
      await settle();
    });
    expect(host.querySelector<HTMLButtonElement>('button[aria-label="Edit mode"]')?.textContent).toBe("Editing");

    editor.setEditMode.mockClear();
    backend.DiscardEdits.mockClear();
    await requestNativeClose();
    let dialog = host.querySelector<HTMLElement>('.q-close-dialog[role="dialog"]');
    expect(dialog?.textContent).toContain("second.sql");
    const discard = Array.from(dialog?.querySelectorAll<HTMLButtonElement>("button") ?? [])
      .find((button) => button.textContent === "Discard");
    await act(async () => {
      discard?.click();
      await settle();
    });

    expect(editor.setEditMode).toHaveBeenCalledWith(false);
    expect(backend.DiscardEdits).toHaveBeenCalledWith("second");
    expect(editor.setEditMode.mock.invocationCallOrder[0])
      .toBeLessThan(backend.DiscardEdits.mock.invocationCallOrder[0]);
    dialog = host.querySelector<HTMLElement>('.q-close-dialog[role="dialog"]');
    expect(dialog?.textContent).toContain("important.sql");
    const cancel = Array.from(dialog?.querySelectorAll<HTMLButtonElement>("button") ?? [])
      .find((button) => button.textContent === "Cancel");
    await act(async () => {
      cancel?.click();
      await settle();
    });

    expect(lifecycle.CancelClose).toHaveBeenCalledOnce();
    expect(lifecycle.ApproveClose).not.toHaveBeenCalled();
    expect(host.querySelector<HTMLButtonElement>('button[aria-label="Edit mode"]')?.textContent).toBe("Edit");
    expect(host.querySelector('button[aria-label^="second.sql,"]')).not.toBeNull();
    expect(host.querySelector('button[aria-label^="important.sql,"]')).not.toBeNull();
  });

  it("does not enable the editor until managed source preparation succeeds", async () => {
    let resolvePreparation!: (value: typeof cleanStaging) => void;
    backend.PrepareEditSession.mockReturnValue(new Promise((resolve) => {
      resolvePreparation = resolve;
    }));
    await renderApp();

    await act(async () => {
      host.querySelector<HTMLButtonElement>(".q-top .q-btn-primary")?.click();
      await settle();
    });
    const edit = host.querySelector<HTMLButtonElement>('button[aria-label="Edit mode"]');
    expect(edit).not.toBeNull();

    act(() => edit?.click());
    await act(async () => { await settle(); });
    expect(backend.PrepareEditSession).toHaveBeenCalledWith("important");
    expect(editor.setEditMode).not.toHaveBeenCalled();

    await act(async () => {
      resolvePreparation(cleanStaging);
      await settle();
    });
    expect(editor.setEditMode).toHaveBeenCalledOnce();
    expect(editor.setEditMode).toHaveBeenCalledWith(true);
  });

  it("keeps editing disabled when managed source preparation is cancelled", async () => {
    backend.PrepareEditSession.mockRejectedValue(new Error("job cancelled"));
    await renderApp();

    await act(async () => {
      host.querySelector<HTMLButtonElement>(".q-top .q-btn-primary")?.click();
      await settle();
    });
    await act(async () => {
      host.querySelector<HTMLButtonElement>('button[aria-label="Edit mode"]')?.click();
      await settle();
    });

    expect(backend.PrepareEditSession).toHaveBeenCalledWith("important");
    expect(editor.setEditMode).not.toHaveBeenCalled();
  });

  it("makes the editor read-only before clearing an explicitly discarded session", async () => {
    backend.GetStagingState.mockResolvedValue(staged);
    backend.PrepareEditSession.mockResolvedValue(staged);
    await renderApp();

    await act(async () => {
      host.querySelector<HTMLButtonElement>(".q-top .q-btn-primary")?.click();
      await settle();
    });
    await act(async () => {
      host.querySelector<HTMLButtonElement>('button[aria-label="Edit mode"]')?.click();
      await settle();
    });
    expect(host.querySelector<HTMLButtonElement>('button[aria-label="Edit mode"]')?.textContent).toBe("Editing");

    editor.setEditMode.mockClear();
    backend.DiscardEdits.mockClear();
    const fileMenu = Array.from(host.querySelectorAll<HTMLButtonElement>('[aria-haspopup="menu"]'))
      .find((button) => button.textContent === "File");
    await act(async () => {
      fileMenu?.click();
      await settle();
    });
    const discard = Array.from(host.querySelectorAll<HTMLButtonElement>(".q-menu-item"))
      .find((button) => button.textContent?.includes("Discard edits"));
    expect(discard?.disabled).toBe(false);

    await act(async () => {
      discard?.click();
      await settle();
    });

    expect(editor.setEditMode).toHaveBeenCalledWith(false);
    expect(backend.DiscardEdits).toHaveBeenCalledWith("important");
    expect(editor.setEditMode.mock.invocationCallOrder[0])
      .toBeLessThan(backend.DiscardEdits.mock.invocationCallOrder[0]);
    expect(host.querySelector<HTMLButtonElement>('button[aria-label="Edit mode"]')?.textContent).toBe("Edit");
    expect(host.textContent).toContain("Edits discarded");
  });

  it("does not report success when discard returns a non-empty staging session", async () => {
    backend.GetStagingState.mockResolvedValue(staged);
    backend.PrepareEditSession.mockResolvedValue(staged);
    backend.DiscardEdits.mockResolvedValue(staged);
    await renderApp();

    await act(async () => {
      host.querySelector<HTMLButtonElement>(".q-top .q-btn-primary")?.click();
      await settle();
    });
    await act(async () => {
      host.querySelector<HTMLButtonElement>('button[aria-label="Edit mode"]')?.click();
      await settle();
    });
    const fileMenu = Array.from(host.querySelectorAll<HTMLButtonElement>('[aria-haspopup="menu"]'))
      .find((button) => button.textContent === "File");
    await act(async () => {
      fileMenu?.click();
      await settle();
    });
    const discard = Array.from(host.querySelectorAll<HTMLButtonElement>(".q-menu-item"))
      .find((button) => button.textContent?.includes("Discard edits"));
    await act(async () => {
      discard?.click();
      await settle();
    });

    expect(editor.setEditMode).toHaveBeenCalledWith(false);
    expect(backend.DiscardEdits).toHaveBeenCalledWith("important");
    expect(host.textContent).toContain("discard did not clear staged edits");
    expect(host.textContent).not.toContain("Edits discarded");
  });

  it("retains prepared-session ownership when a close-flow discard reports edits", async () => {
    backend.GetStagingState.mockResolvedValue(staged);
    backend.PrepareEditSession.mockResolvedValue(staged);
    backend.DiscardEdits.mockResolvedValue(staged);
    await renderApp();

    await act(async () => {
      host.querySelector<HTMLButtonElement>(".q-top .q-btn-primary")?.click();
      await settle();
    });
    await act(async () => {
      host.querySelector<HTMLButtonElement>('button[aria-label="Edit mode"]')?.click();
      await settle();
    });
    expect(backend.PrepareEditSession).toHaveBeenCalledWith("important");
    await requestNativeClose();
    const discard = Array.from(host.querySelectorAll<HTMLButtonElement>('.q-close-dialog[role="dialog"] button'))
      .find((button) => button.textContent === "Discard");
    await act(async () => {
      discard?.click();
      await settle();
    });
    expect(host.textContent).toContain("discard did not clear staged edits");

    backend.GetStagingState.mockClear();
    editor.attach.mockClear();
    backend.OpenViaDialog.mockResolvedValueOnce({
      fileId: "second",
      path: "C:\\data\\second.sql",
      size: 2048,
      encoding: "UTF-8",
      detected: "sql",
      binary: false,
      editable: true,
    });
    await act(async () => {
      Array.from(host.querySelectorAll<HTMLButtonElement>(".q-top button"))
        .find((button) => button.textContent?.includes("Open file"))?.click();
      await settle();
    });

    expect(backend.GetStagingState).toHaveBeenCalledWith("important");
    expect(backend.GetStagingState.mock.invocationCallOrder[0])
      .toBeLessThan(editor.attach.mock.invocationCallOrder[0]);
  });

  it("flushes, confirms clean staging, then releases preparation on edit-off", async () => {
    await renderApp();
    await act(async () => {
      host.querySelector<HTMLButtonElement>(".q-top .q-btn-primary")?.click();
      await settle();
    });
    await act(async () => {
      host.querySelector<HTMLButtonElement>('button[aria-label="Edit mode"]')?.click();
      await settle();
    });
    expect(editor.setEditMode).toHaveBeenLastCalledWith(true);

    editor.setEditMode.mockClear();
    backend.GetStagingState.mockClear();
    backend.ReleaseCleanEditSession.mockClear();
    await act(async () => {
      host.querySelector<HTMLButtonElement>('button[aria-label="Edit mode"]')?.click();
      await settle();
    });

    expect(editor.setEditMode).toHaveBeenCalledWith(false);
    expect(backend.GetStagingState).toHaveBeenCalledWith("important");
    expect(backend.ReleaseCleanEditSession).toHaveBeenCalledWith("important");
    const modeOrder = editor.setEditMode.mock.invocationCallOrder[0];
    const stagingOrder = backend.GetStagingState.mock.invocationCallOrder[0];
    const releaseOrder = backend.ReleaseCleanEditSession.mock.invocationCallOrder[0];
    expect(modeOrder).toBeLessThan(stagingOrder);
    expect(stagingOrder).toBeLessThan(releaseOrder);
  });

  it("stays read-only and surfaces a clean-session release failure", async () => {
    await renderApp();
    await act(async () => {
      host.querySelector<HTMLButtonElement>(".q-top .q-btn-primary")?.click();
      await settle();
    });
    await act(async () => {
      host.querySelector<HTMLButtonElement>('button[aria-label="Edit mode"]')?.click();
      await settle();
    });
    backend.ReleaseCleanEditSession.mockRejectedValue(new Error("release failed"));

    await act(async () => {
      host.querySelector<HTMLButtonElement>('button[aria-label="Edit mode"]')?.click();
      await settle();
    });

    expect(editor.setEditMode).toHaveBeenLastCalledWith(false);
    expect(backend.ReleaseCleanEditSession).toHaveBeenCalledWith("important");
    expect(host.querySelector<HTMLButtonElement>('button[aria-label="Edit mode"]')?.textContent).toBe("Edit");
    expect(host.textContent).toContain("Unable to change editor mode");
  });

  it("never releases a prepared session after edit-off reports staged data", async () => {
    await renderApp();
    await act(async () => {
      host.querySelector<HTMLButtonElement>(".q-top .q-btn-primary")?.click();
      await settle();
    });
    await act(async () => {
      host.querySelector<HTMLButtonElement>('button[aria-label="Edit mode"]')?.click();
      await settle();
    });
    backend.GetStagingState.mockResolvedValue(staged);
    backend.ReleaseCleanEditSession.mockClear();

    await act(async () => {
      host.querySelector<HTMLButtonElement>('button[aria-label="Edit mode"]')?.click();
      await settle();
    });

    expect(editor.setEditMode).toHaveBeenLastCalledWith(false);
    expect(backend.GetStagingState).toHaveBeenLastCalledWith("important");
    expect(backend.ReleaseCleanEditSession).not.toHaveBeenCalled();
  });

  it("releases a clean preparation before attaching another tab", async () => {
    await renderApp();
    await act(async () => {
      host.querySelector<HTMLButtonElement>(".q-top .q-btn-primary")?.click();
      await settle();
    });
    await act(async () => {
      host.querySelector<HTMLButtonElement>('button[aria-label="Edit mode"]')?.click();
      await settle();
    });

    backend.OpenViaDialog.mockResolvedValueOnce({
      fileId: "second",
      path: "C:\\data\\second.sql",
      size: 2048,
      encoding: "UTF-8",
      detected: "sql",
      binary: false,
      editable: true,
    });
    editor.setEditMode.mockClear();
    backend.GetStagingState.mockClear();
    backend.ReleaseCleanEditSession.mockClear();
    editor.attach.mockClear();
    await act(async () => {
      host.querySelector<HTMLButtonElement>(".q-top .q-btn-primary")?.click();
      await settle();
    });

    expect(editor.setEditMode).toHaveBeenCalledWith(false);
    expect(backend.GetStagingState).toHaveBeenCalledWith("important");
    expect(backend.ReleaseCleanEditSession).toHaveBeenCalledWith("important");
    expect(editor.attach).toHaveBeenCalledWith("second", "sql", "C:\\data\\second.sql", 0, "");
    expect(backend.ReleaseCleanEditSession.mock.invocationCallOrder[0])
      .toBeLessThan(editor.attach.mock.invocationCallOrder[0]);
  });
});
