import React from "react";
import { createRoot } from "react-dom/client";
import { act } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import CommandPalette, { COMMAND_PALETTE_QUERY_MAX_BYTES, type Command } from "../src/CommandPalette";
import Help from "../src/Help";
import Sidebar, { type SidebarTab } from "../src/Sidebar";
import UnsavedChangesDialog from "../src/UnsavedChangesDialog";
import XRay from "../src/XRay";
import type { CloseDecisionContext } from "../src/closeSafety";

describe("workbench accessibility widgets", () => {
  let host: HTMLDivElement;
  let trigger: HTMLButtonElement;
  let root: ReturnType<typeof createRoot>;
  let mounted: boolean;

  beforeEach(() => {
    trigger = document.createElement("button");
    trigger.textContent = "Open overlay";
    document.body.append(trigger);
    trigger.focus();
    host = document.createElement("div");
    document.body.append(host);
    root = createRoot(host);
    mounted = true;
  });

  afterEach(() => {
    if (mounted) act(() => root.unmount());
    host.remove();
    trigger.remove();
  });

  const unmount = () => {
    act(() => root.unmount());
    mounted = false;
  };

  it("exposes the command palette as a modal combobox and keeps active-descendant focus in the input", () => {
    const runFirst = vi.fn();
    const runSecond = vi.fn();
    const onClose = vi.fn();
    const commands: Command[] = [
      { id: "open", label: "Open file", group: "File", run: runFirst },
      { id: "help", label: "Show help", hint: "F1", group: "Help", run: runSecond },
    ];

    act(() => root.render(React.createElement(CommandPalette, { commands, onClose })));

    const dialog = host.querySelector<HTMLElement>("[role=dialog]");
    const input = host.querySelector<HTMLInputElement>("[role=combobox]");
    const options = Array.from(host.querySelectorAll<HTMLButtonElement>("button[role=option]"));
    expect(dialog?.getAttribute("aria-modal")).toBe("true");
    expect(input?.getAttribute("aria-controls")).toBe("q-command-results");
    expect(input?.getAttribute("aria-activedescendant")).toBe("q-command-option-0");
    expect(input?.maxLength).toBe(COMMAND_PALETTE_QUERY_MAX_BYTES);
    expect(document.activeElement).toBe(input);
    expect(options).toHaveLength(2);
    expect(options[0].getAttribute("aria-selected")).toBe("true");

    const valueSetter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, "value")?.set;
    act(() => {
      valueSetter?.call(input, "🙂".repeat((COMMAND_PALETTE_QUERY_MAX_BYTES / 4) + 1));
      input?.dispatchEvent(new Event("input", { bubbles: true }));
    });
    expect(new TextEncoder().encode(input?.value ?? "")).toHaveLength(COMMAND_PALETTE_QUERY_MAX_BYTES);
    act(() => {
      valueSetter?.call(input, "");
      input?.dispatchEvent(new Event("input", { bubbles: true }));
    });

    const workbenchShortcut = vi.fn();
    window.addEventListener("keydown", workbenchShortcut);
    act(() => input?.dispatchEvent(new KeyboardEvent("keydown", { key: "w", ctrlKey: true, bubbles: true, cancelable: true })));
    window.removeEventListener("keydown", workbenchShortcut);
    expect(workbenchShortcut).not.toHaveBeenCalled();

    act(() => input?.dispatchEvent(new KeyboardEvent("keydown", { key: "ArrowDown", bubbles: true, cancelable: true })));
    expect(input?.getAttribute("aria-activedescendant")).toBe("q-command-option-1");
    expect(document.activeElement).toBe(input);

    act(() => input?.dispatchEvent(new KeyboardEvent("keydown", { key: "Enter", bubbles: true, cancelable: true })));
    expect(onClose).toHaveBeenCalledOnce();
    expect(runSecond).toHaveBeenCalledOnce();
    expect(runFirst).not.toHaveBeenCalled();

    const tab = new KeyboardEvent("keydown", { key: "Tab", bubbles: true, cancelable: true });
    act(() => input?.dispatchEvent(tab));
    expect(tab.defaultPrevented).toBe(true);
    expect(document.activeElement).toBe(input);

    unmount();
    expect(document.activeElement).toBe(trigger);
  });

  it("exposes disabled command reasons and never invokes unavailable commands", () => {
    const unavailable = vi.fn();
    const onClose = vi.fn();
    const commands: Command[] = [{
      id: "edit",
      label: "Turn editing on",
      enabled: false,
      disabledReason: "This file cannot be edited safely",
      run: unavailable,
    }];

    act(() => root.render(React.createElement(CommandPalette, { commands, onClose })));
    const input = host.querySelector<HTMLInputElement>("[role=combobox]");
    const option = host.querySelector<HTMLButtonElement>("button[role=option]");
    expect(option?.getAttribute("aria-disabled")).toBe("true");
    expect(option?.textContent).toContain("This file cannot be edited safely");

    act(() => input?.dispatchEvent(new KeyboardEvent("keydown", { key: "Enter", bubbles: true, cancelable: true })));
    act(() => option?.click());
    expect(unavailable).not.toHaveBeenCalled();
    expect(onClose).not.toHaveBeenCalled();
  });

  it("implements Help as a labelled tabbed dialog with roving topic focus and restoration", () => {
    const onClose = vi.fn();
    act(() => root.render(React.createElement(Help, { onClose })));

    const dialog = host.querySelector<HTMLElement>("[role=dialog]");
    const tabs = Array.from(host.querySelectorAll<HTMLButtonElement>("[role=tab]"));
    expect(dialog?.getAttribute("aria-labelledby")).toBe("q-help-title");
    expect(tabs).toHaveLength(7);
    expect(document.activeElement).toBe(tabs[0]);
    expect(tabs[0].getAttribute("aria-selected")).toBe("true");

    const workbenchShortcut = vi.fn();
    window.addEventListener("keydown", workbenchShortcut);
    act(() => tabs[0].dispatchEvent(new KeyboardEvent("keydown", { key: "w", ctrlKey: true, bubbles: true, cancelable: true })));
    window.removeEventListener("keydown", workbenchShortcut);
    expect(workbenchShortcut).not.toHaveBeenCalled();

    act(() => tabs[0].dispatchEvent(new KeyboardEvent("keydown", { key: "ArrowDown", bubbles: true, cancelable: true })));
    expect(document.activeElement).toBe(tabs[1]);
    expect(tabs[1].getAttribute("aria-selected")).toBe("true");
    expect(host.querySelector("[role=tabpanel]")?.getAttribute("aria-labelledby")).toBe("q-help-tab-shortcuts");

    act(() => tabs[1].dispatchEvent(new KeyboardEvent("keydown", { key: "End", bubbles: true, cancelable: true })));
    expect(document.activeElement).toBe(tabs[tabs.length - 1]);
    act(() => tabs[tabs.length - 1].dispatchEvent(new KeyboardEvent("keydown", { key: "Home", bubbles: true, cancelable: true })));
    expect(document.activeElement).toBe(tabs[0]);

    act(() => tabs[0].dispatchEvent(new KeyboardEvent("keydown", { key: "Escape", bubbles: true, cancelable: true })));
    expect(onClose).toHaveBeenCalledOnce();
    unmount();
    expect(document.activeElement).toBe(trigger);
  });

  it("uses native file-tab and close buttons with vertical tablist navigation", () => {
    const onActivate = vi.fn();
    const onClose = vi.fn();
    const onToggle = vi.fn();
    const meta = (fileId: string, path: string, detected: string): SidebarTab => ({
      fileId,
      pendingEdits: fileId === "a",
      meta: {
        fileId,
        path,
        size: 2048,
        encoding: "UTF-8",
        detected,
        binary: false,
        editable: true,
      },
    });
    const tabs = [meta("a", "C:\\data\\alpha.sql", "sql"), meta("b", "C:\\data\\beta.csv", "csv")];

    act(() => root.render(React.createElement(Sidebar, {
      tabs,
      activeId: "a",
      collapsed: false,
      onActivate,
      onClose,
      onToggle,
    })));

    const fileTabs = Array.from(host.querySelectorAll<HTMLButtonElement>(".q-sb-activate"));
    const close = host.querySelector<HTMLButtonElement>('button[aria-label="Close alpha.sql"]');
    expect(host.querySelector('[role="list"]')?.getAttribute("aria-label")).toBe("Open files");
    expect(fileTabs[0].getAttribute("aria-current")).toBe("page");
    expect(fileTabs[0].getAttribute("aria-label")).toContain("pending edits");
    expect(fileTabs[0].tabIndex).toBe(0);
    expect(fileTabs[1].tabIndex).toBe(-1);
    expect(close?.tagName).toBe("BUTTON");

    fileTabs[0].focus();
    act(() => fileTabs[0].dispatchEvent(new KeyboardEvent("keydown", { key: "ArrowDown", bubbles: true, cancelable: true })));
    expect(onActivate).toHaveBeenLastCalledWith("b");
    expect(document.activeElement).toBe(fileTabs[1]);

    act(() => close?.click());
    expect(onClose).toHaveBeenCalledWith("a", expect.anything());
  });

  it("exposes the file map as a bounded vertical slider with pointer and keyboard seeking", () => {
    const onSeek = vi.fn();
    act(() => root.render(React.createElement(XRay, {
      size: 1000,
      regions: [{ start: 400, end: 500, name: "orders" }],
      vpStart: 100,
      vpEnd: 200,
      onSeek,
    })));

    const slider = host.querySelector<HTMLButtonElement>('button[role="slider"]');
    expect(slider?.getAttribute("aria-orientation")).toBe("vertical");
    expect(slider?.getAttribute("aria-valuenow")).toBe("100");
    expect(slider?.getAttribute("aria-valuemax")).toBe("999");
    const regionDetailsId = slider?.getAttribute("aria-describedby");
    const regionDetails = regionDetailsId ? document.getElementById(regionDetailsId) : null;
    expect(regionDetails?.textContent).toContain("Analyzed file regions:");
    expect(regionDetails?.textContent).toContain("orders: byte range 400 up to but not including 500");

    act(() => slider?.dispatchEvent(new KeyboardEvent("keydown", { key: "ArrowDown", bubbles: true, cancelable: true })));
    expect(onSeek).toHaveBeenLastCalledWith(110);
    act(() => slider?.dispatchEvent(new KeyboardEvent("keydown", { key: "End", bubbles: true, cancelable: true })));
    expect(onSeek).toHaveBeenLastCalledWith(999);

    const region = host.querySelector<HTMLElement>("[data-region-start]");
    expect(region?.getAttribute("aria-hidden")).toBe("true");
    act(() => region?.dispatchEvent(new MouseEvent("click", { bubbles: true, cancelable: true, detail: 1 })));
    expect(onSeek).toHaveBeenLastCalledWith(400);
  });

  it("traps the unsaved-changes decision, cancels on Escape, and restores its invoker", () => {
    const onChoose = vi.fn();
    const context: CloseDecisionContext = {
      target: {
        fileId: "file-a",
        path: "C:\\data\\important.sql",
        dirty: true,
        editorAttached: true,
        scope: "tab",
      },
      staging: {
        editCount: 2,
        originalSize: 10,
        editedSize: 12,
        netDelta: 2,
        lengthPreserving: false,
        inPlaceEligible: false,
      },
    };

    act(() => root.render(React.createElement(UnsavedChangesDialog, { context, onChoose })));

    const dialog = host.querySelector<HTMLElement>("[role=dialog]");
    const buttons = Array.from(dialog?.querySelectorAll<HTMLButtonElement>("button") ?? []);
    expect(dialog?.getAttribute("aria-modal")).toBe("true");
    expect(dialog?.getAttribute("aria-describedby")).toContain("q-close-note");
    expect(document.activeElement).toBe(buttons[2]);

    const tab = new KeyboardEvent("keydown", { key: "Tab", bubbles: true, cancelable: true });
    act(() => buttons[2].dispatchEvent(tab));
    expect(tab.defaultPrevented).toBe(true);
    expect(document.activeElement).toBe(buttons[0]);

    const shiftTab = new KeyboardEvent("keydown", { key: "Tab", shiftKey: true, bubbles: true, cancelable: true });
    act(() => buttons[0].dispatchEvent(shiftTab));
    expect(shiftTab.defaultPrevented).toBe(true);
    expect(document.activeElement).toBe(buttons[2]);

    act(() => window.dispatchEvent(new KeyboardEvent("keydown", { key: "Escape", bubbles: true, cancelable: true })));
    expect(onChoose).toHaveBeenCalledWith("cancel");
    unmount();
    expect(document.activeElement).toBe(trigger);
  });
});
