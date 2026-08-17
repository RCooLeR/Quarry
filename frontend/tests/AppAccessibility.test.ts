import React from "react";
import { createRoot } from "react-dom/client";
import { act } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const runtime = vi.hoisted(() => ({
  on: vi.fn(() => () => {}),
}));

const backend = vi.hoisted(() => ({
  BeginSearchRequest: vi.fn(),
  CancelSearch: vi.fn(),
  GetStagingState: vi.fn(),
  OpenViaDialog: vi.fn(),
  SearchAllRequest: vi.fn(),
}));

const editorCalls = vi.hoisted(() => ({
  showMatch: vi.fn(),
}));
const helpHarness = vi.hoisted(() => ({ fail: false }));

vi.mock("@wailsio/runtime", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@wailsio/runtime")>();
  return { ...actual, Events: { ...actual.Events, On: runtime.on } };
});

vi.mock("../bindings/github.com/quarry/quarry-wails3", () => ({
  AppLifecycle: {
    ApproveClose: vi.fn(),
    CancelClose: vi.fn(),
  },
  FileService: backend,
}));

vi.mock("../src/editor/QuarryEditor", () => ({
  QuarryEditor: class {
    private callbacks: { onStatus: (status: unknown) => void };

    constructor(_host: HTMLElement, callbacks: { onStatus: (status: unknown) => void }) {
      this.callbacks = callbacks;
    }

    async attach(fileId: string, _detected: string, _path: string, startByte: number): Promise<void> {
      this.callbacks.onStatus({
        fileId,
        startByte,
        positionByte: startByte + 128,
        endByte: startByte + 256,
        firstLine: 4,
        lastLine: 8,
        atBof: false,
        atEof: false,
        approx: false,
      });
    }

    async showMatch(...args: unknown[]): Promise<void> {
      editorCalls.showMatch(...args);
    }

    destroy(): void {}
    setTheme(): void {}
    setShowByteOffsets(): void {}
  },
}));

vi.mock("../src/Help", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../src/Help")>();
  const ReactModule = await import("react");
  return {
    ...actual,
    default: (props: { onClose: () => void; returnFocusTo?: HTMLElement | null }) => {
      if (helpHarness.fail) throw new Error("help chunk render failed");
      return ReactModule.createElement(actual.default, props);
    },
  };
});

import App, { LoadingHelpDialog } from "../src/App";

async function settle(): Promise<void> {
  for (let i = 0; i < 8; i++) await Promise.resolve();
}

async function waitForElement<T extends Element>(
  container: ParentNode,
  selector: string,
  attempts = 20,
): Promise<T | null> {
  for (let i = 0; i < attempts; i++) {
    const element = container.querySelector<T>(selector);
    if (element) return element;
    await act(async () => {
      await new Promise<void>((resolve) => setTimeout(resolve, 0));
      await settle();
    });
  }
  return container.querySelector<T>(selector);
}

describe("App modal accessibility integration", () => {
  let host: HTMLDivElement;
  let root: ReturnType<typeof createRoot>;

  beforeEach(() => {
    localStorage.clear();
    vi.clearAllMocks();
    helpHarness.fail = false;
    backend.GetStagingState.mockResolvedValue({
      editCount: 0,
      originalSize: 1024,
      editedSize: 1024,
      netDelta: 0,
      lengthPreserving: true,
      inPlaceEligible: false,
    });
    backend.OpenViaDialog.mockResolvedValue({
      fileId: "file-a",
      path: "C:\\data\\sample.sql",
      size: 1024,
      encoding: "UTF-8",
      detected: "sql",
      binary: false,
      editable: true,
    });
    backend.BeginSearchRequest.mockResolvedValue("search1");
    backend.CancelSearch.mockResolvedValue(true);
    backend.SearchAllRequest.mockResolvedValue({
      hits: [
        { offset: 10, length: 2, line: 2, preview: "first id" },
        { offset: 20, length: 2, line: 3, preview: "second id" },
      ],
      truncated: false,
    });
    runtime.on.mockClear();
    host = document.createElement("div");
    document.body.append(host);
    root = createRoot(host);
  });

  afterEach(() => {
    act(() => root.unmount());
    host.remove();
    localStorage.clear();
  });

  it("makes the workbench inert and hidden only while the command palette is modal", async () => {
    await act(async () => {
      root.render(React.createElement(App));
      await settle();
    });

    const workbench = host.querySelector<HTMLElement>(".q-workbench");
    expect(workbench?.hasAttribute("inert")).toBe(false);
    expect(workbench?.hasAttribute("aria-hidden")).toBe(false);

    await act(async () => {
      window.dispatchEvent(new KeyboardEvent("keydown", { key: "p", ctrlKey: true, bubbles: true, cancelable: true }));
      await settle();
    });

    const palette = host.querySelector<HTMLElement>('[role="dialog"][aria-modal="true"]');
    const input = host.querySelector<HTMLInputElement>('[role="combobox"]');
    expect(palette).not.toBeNull();
    expect(workbench?.hasAttribute("inert")).toBe(true);
    expect(workbench?.getAttribute("aria-hidden")).toBe("true");
    expect(host.textContent).toContain("Ctrl/Cmd+O");
    expect(document.activeElement).toBe(input);

    await act(async () => {
      input?.dispatchEvent(new KeyboardEvent("keydown", { key: "Escape", bubbles: true, cancelable: true }));
      await settle();
    });

    expect(host.querySelector('[role="dialog"]')).toBeNull();
    expect(workbench?.hasAttribute("inert")).toBe(false);
    expect(workbench?.hasAttribute("aria-hidden")).toBe(false);
  });

  it("keeps delayed Help loading modal, traps focus, and blocks modal-stacking shortcuts", async () => {
    let resolveHelp!: (module: { default: React.ComponentType }) => void;
    const DelayedHelp = React.lazy(() => new Promise((resolve) => { resolveHelp = resolve; }));
    const onClose = vi.fn();
    const returnFocusTo = document.createElement("button");
    returnFocusTo.textContent = "Help invoker";
    document.body.append(returnFocusTo);
    returnFocusTo.focus();

    await act(async () => {
      root.render(React.createElement(
        React.Suspense,
        { fallback: React.createElement(LoadingHelpDialog, { onClose, returnFocusTo }) },
        React.createElement(DelayedHelp),
      ));
      await settle();
    });

    const dialog = host.querySelector<HTMLElement>('[role="dialog"][aria-label="Loading help"]');
    expect(dialog).not.toBeNull();
    expect(document.activeElement).toBe(dialog);

    const workbenchShortcut = vi.fn();
    window.addEventListener("keydown", workbenchShortcut);
    const palette = new KeyboardEvent("keydown", {
      key: "p",
      ctrlKey: true,
      bubbles: true,
      cancelable: true,
    });
    act(() => dialog?.dispatchEvent(palette));
    window.removeEventListener("keydown", workbenchShortcut);
    expect(palette.defaultPrevented).toBe(true);
    expect(workbenchShortcut).not.toHaveBeenCalled();
    expect(host.querySelectorAll('[role="dialog"]')).toHaveLength(1);

    for (const shiftKey of [false, true]) {
      const tab = new KeyboardEvent("keydown", {
        key: "Tab",
        shiftKey,
        bubbles: true,
        cancelable: true,
      });
      act(() => dialog?.dispatchEvent(tab));
      expect(tab.defaultPrevented).toBe(true);
      expect(document.activeElement).toBe(dialog);
    }

    const escape = new KeyboardEvent("keydown", { key: "Escape", bubbles: true, cancelable: true });
    act(() => dialog?.dispatchEvent(escape));
    expect(escape.defaultPrevented).toBe(true);
    expect(onClose).toHaveBeenCalledOnce();

    act(() => root.render(React.createElement("div", null, "Help closed")));
    expect(document.activeElement).toBe(returnFocusTo);

    await act(async () => {
      resolveHelp({ default: () => React.createElement("div", null, "Loaded help") });
      await settle();
    });
    expect(host.querySelector('[aria-label="Loading help"]')).toBeNull();
    returnFocusTo.remove();
  });

  it("contains a failed Help render as a modal and restores its App-level invoker", async () => {
    const consoleError = vi.spyOn(console, "error").mockImplementation(() => {});
    const preventWindowError = (event: ErrorEvent) => event.preventDefault();
    window.addEventListener("error", preventWindowError);
    helpHarness.fail = true;
    try {
      await act(async () => {
        root.render(React.createElement(App));
        await settle();
      });

      const invoker = host.querySelector<HTMLButtonElement>(".q-top .q-btn-primary");
      invoker?.focus();
      await act(async () => {
        window.dispatchEvent(new KeyboardEvent("keydown", { key: "F1", bubbles: true, cancelable: true }));
        await settle();
      });

      const workbench = host.querySelector<HTMLElement>(".q-workbench");
      const dialog = await waitForElement<HTMLElement>(host, '[role="dialog"][aria-label="Help display failure"]');
      expect(dialog, host.innerHTML).not.toBeNull();
      expect(host.querySelectorAll('[role="dialog"]')).toHaveLength(1);
      expect(dialog?.textContent).toContain("help chunk render failed");
      expect(workbench?.getAttribute("aria-hidden")).toBe("true");
      expect(workbench?.hasAttribute("inert")).toBe(true);
      expect(document.activeElement).toBe(dialog);

      await act(async () => {
        dialog?.dispatchEvent(new KeyboardEvent("keydown", { key: "Escape", bubbles: true, cancelable: true }));
        await settle();
      });
      expect(host.querySelector('[role="dialog"]')).toBeNull();
      expect(workbench?.hasAttribute("inert")).toBe(false);
      expect(document.activeElement).toBe(invoker);
    } finally {
      helpHarness.fail = false;
      window.removeEventListener("error", preventWindowError);
      consoleError.mockRestore();
    }
  });

  it("restores menu focus after menu-launched palette and lazy Help dialogs close", async () => {
    await act(async () => {
      root.render(React.createElement(App));
      await settle();
    });

    const menuTrigger = (label: string) => Array.from(
      host.querySelectorAll<HTMLButtonElement>('[aria-haspopup="menu"]'),
    ).find((button) => button.textContent === label)!;
    const openMenuItem = async (trigger: HTMLButtonElement, label: string) => {
      await act(async () => {
        trigger.click();
        await settle();
      });
      const item = Array.from(host.querySelectorAll<HTMLButtonElement>(".q-menu-item"))
        .find((button) => button.textContent?.includes(label));
      expect(item).toBeDefined();
      await act(async () => {
        item?.click();
        await settle();
      });
    };

    const viewTrigger = menuTrigger("View");
    await openMenuItem(viewTrigger, "Command palette");
    const paletteInput = host.querySelector<HTMLInputElement>('[role="combobox"]');
    expect(document.activeElement).toBe(paletteInput);

    await act(async () => {
      paletteInput?.dispatchEvent(new KeyboardEvent("keydown", { key: "Escape", bubbles: true, cancelable: true }));
      await settle();
    });
    expect(host.querySelector('[role="dialog"]')).toBeNull();
    expect(document.activeElement).toBe(viewTrigger);

    const helpTrigger = menuTrigger("Help");
    await openMenuItem(helpTrigger, "Help & shortcuts");
    const helpTab = await waitForElement<HTMLButtonElement>(host, "#q-help-tab-overview");
    expect(helpTab).not.toBeNull();
    expect(document.activeElement).toBe(helpTab);

    await act(async () => {
      helpTab?.dispatchEvent(new KeyboardEvent("keydown", { key: "Escape", bubbles: true, cancelable: true }));
      await settle();
    });
    expect(host.querySelector('[role="dialog"]')).toBeNull();
    expect(document.activeElement).toBe(helpTrigger);
  });

  it("renders search and bookmark navigation as named keyboard-operable actions", async () => {
    await act(async () => {
      root.render(React.createElement(App));
      await settle();
    });

    const open = host.querySelector<HTMLButtonElement>(".q-top .q-btn-primary");
    await act(async () => {
      open?.click();
      await settle();
    });

    await act(async () => {
      window.dispatchEvent(new KeyboardEvent("keydown", { key: "f", ctrlKey: true, bubbles: true, cancelable: true }));
      await settle();
    });
    const search = host.querySelector<HTMLInputElement>('input[aria-label="Find text"]');
    expect(search).not.toBeNull();
    const toggles = Array.from(host.querySelectorAll<HTMLButtonElement>(".q-find .q-toggle"));
    expect(toggles.map((toggle) => toggle.getAttribute("aria-label"))).toEqual([
      "Match case",
      "Match whole words",
      "Use regular expression",
    ]);
    expect(toggles.every((toggle) => toggle.getAttribute("aria-pressed") === "false")).toBe(true);
    act(() => toggles[2].click());
    expect(toggles[2].getAttribute("aria-pressed")).toBe("true");

    await act(async () => {
      const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, "value")?.set;
      setter?.call(search, "id");
      search?.dispatchEvent(new Event("input", { bubbles: true }));
      await settle();
    });
    const listAll = host.querySelector<HTMLButtonElement>('button[aria-label="List up to 1,000 matches"]');
    await act(async () => {
      listAll?.click();
      await settle();
    });

    const matches = Array.from(host.querySelectorAll<HTMLButtonElement>(".q-result-button"));
    expect(matches).toHaveLength(2);
    expect(matches[0].getAttribute("aria-label")).toContain("Go to match on line 2");
    matches[0].focus();
    act(() => matches[0].dispatchEvent(new KeyboardEvent("keydown", { key: "ArrowDown", bubbles: true, cancelable: true })));
    expect(document.activeElement).toBe(matches[1]);

    await act(async () => {
      window.dispatchEvent(new KeyboardEvent("keydown", { key: "p", ctrlKey: true, bubbles: true, cancelable: true }));
      await settle();
    });
    const addBookmark = Array.from(host.querySelectorAll<HTMLButtonElement>('button[role="option"]'))
      .find((option) => option.textContent?.includes("Add bookmark here"));
    await act(async () => {
      addBookmark?.click();
      await settle();
    });

    const bookmark = host.querySelector<HTMLButtonElement>(".q-bookmark-seek");
    expect(bookmark?.getAttribute("aria-label")).toContain("Go to bookmark line ~4");
    expect(host.querySelector('button[aria-label^="Remove bookmark"]')).not.toBeNull();
  });
});
