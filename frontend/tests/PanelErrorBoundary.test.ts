import React from "react";
import { createRoot } from "react-dom/client";
import { act } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import PanelErrorBoundary from "../src/PanelErrorBoundary";

function Fault({ fail }: { fail: boolean }) {
  if (fail) throw new Error("deliberate panel failure");
  return React.createElement("div", { id: "healthy-panel" }, "Panel restored");
}

describe("PanelErrorBoundary", () => {
  let host: HTMLDivElement;
  let root: ReturnType<typeof createRoot>;
  let consoleError: ReturnType<typeof vi.spyOn>;
  let preventWindowError: (event: ErrorEvent) => void;

  beforeEach(() => {
    host = document.createElement("div");
    document.body.append(host);
    root = createRoot(host);
    consoleError = vi.spyOn(console, "error").mockImplementation(() => {});
    preventWindowError = (event) => event.preventDefault();
    window.addEventListener("error", preventWindowError);
  });

  afterEach(() => {
    act(() => root.unmount());
    host.remove();
    consoleError.mockRestore();
    window.removeEventListener("error", preventWindowError);
  });

  it("contains a panel crash, keeps the workbench usable, and supports reset", () => {
    let fail = true;
    const onClose = vi.fn();
    const onError = vi.fn();
    const render = () => {
      root.render(React.createElement(
        React.Fragment,
        null,
        React.createElement("button", { id: "workbench-action" }, "Editor remains available"),
        React.createElement(
          PanelErrorBoundary,
          { panelName: "CSV grid", resetKey: "file-a", onClose, onError },
          React.createElement(Fault, { fail }),
        ),
      ));
    };

    act(render);

    expect(host.querySelector("#workbench-action")?.textContent).toBe("Editor remains available");
    const alert = host.querySelector<HTMLElement>("[role=alert]");
    expect(alert?.textContent).toContain("CSV grid could not be displayed");
    expect(alert?.getAttribute("aria-atomic")).toBe("true");
    expect(alert?.getAttribute("aria-label")).toBe("CSV grid display failure");
    expect(document.activeElement).toBe(alert);
    expect(host.textContent).toContain("source file was not modified");
    expect(onError).toHaveBeenCalledWith(expect.stringContaining("deliberate panel failure"));

    fail = false;
    act(render);
    const reset = Array.from(host.querySelectorAll("button")).find((button) => button.textContent === "Try panel again");
    expect(reset).toBeDefined();
    act(() => reset?.dispatchEvent(new MouseEvent("click", { bubbles: true })));

    expect(host.querySelector("#healthy-panel")?.textContent).toBe("Panel restored");
    expect(host.querySelector("[role=alert]")).toBeNull();
    expect(onClose).not.toHaveBeenCalled();
  });

  it("offers an explicit close recovery action", () => {
    const onClose = vi.fn();
    act(() => {
      root.render(React.createElement(
        PanelErrorBoundary,
        { panelName: "Data tools", resetKey: "file-b", onClose },
        React.createElement(Fault, { fail: true }),
      ));
    });

    const close = Array.from(host.querySelectorAll("button")).find((button) => button.textContent === "Close panel");
    expect(close).toBeDefined();
    act(() => close?.dispatchEvent(new MouseEvent("click", { bubbles: true })));
    expect(onClose).toHaveBeenCalledOnce();
  });

  it("contains a lazy modal rejection and preserves modal focus and shortcuts", async () => {
    const RejectedHelp = React.lazy(async () => {
      throw new Error("help chunk unavailable");
    });
    const onClose = vi.fn();
    const workbenchShortcut = vi.fn();

    await act(async () => {
      root.render(React.createElement(
        React.Fragment,
        null,
        React.createElement("button", { id: "workbench-action" }, "Editor action"),
        React.createElement(
          PanelErrorBoundary,
          { panelName: "Help", resetKey: "help", onClose, modal: true },
          React.createElement(
            React.Suspense,
            { fallback: React.createElement("div", null, "Loading help") },
            React.createElement(RejectedHelp),
          ),
        ),
      ));
      await new Promise<void>((resolve) => setTimeout(resolve, 0));
    });

    const dialog = host.querySelector<HTMLElement>('[role="dialog"][aria-modal="true"]');
    expect(dialog?.getAttribute("aria-label")).toBe("Help display failure");
    expect(dialog?.textContent).toContain("help chunk unavailable");
    expect(dialog?.textContent).toContain("source file was not modified");
    expect(document.activeElement).toBe(dialog);

    const outside = host.querySelector<HTMLButtonElement>("#workbench-action");
    act(() => outside?.focus());
    expect(document.activeElement).toBe(dialog);

    const actions = dialog?.querySelectorAll<HTMLButtonElement>("button") ?? [];
    const tabIntoDialog = new KeyboardEvent("keydown", { key: "Tab", bubbles: true, cancelable: true });
    act(() => dialog?.dispatchEvent(tabIntoDialog));
    expect(tabIntoDialog.defaultPrevented).toBe(true);
    expect(document.activeElement).toBe(actions[0]);

    const wrapBack = new KeyboardEvent("keydown", { key: "Tab", shiftKey: true, bubbles: true, cancelable: true });
    act(() => actions[0]?.dispatchEvent(wrapBack));
    expect(wrapBack.defaultPrevented).toBe(true);
    expect(document.activeElement).toBe(actions[actions.length - 1]);

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

    const escape = new KeyboardEvent("keydown", { key: "Escape", bubbles: true, cancelable: true });
    act(() => dialog?.dispatchEvent(escape));
    expect(escape.defaultPrevented).toBe(true);
    expect(onClose).toHaveBeenCalledOnce();
  });

  it("recreates a rejected lazy component before retrying", async () => {
    let attempts = 0;
    const loader = vi.fn(async () => {
      attempts++;
      if (attempts <= 2) throw new Error(`temporary chunk failure ${attempts}`);
      return {
        default: () => React.createElement("div", { id: "retried-lazy-panel" }, "Lazy panel restored"),
      };
    });

    function RetryableLazyPanel() {
      const [epoch, setEpoch] = React.useState(0);
      const LazyPanel = React.useMemo(() => React.lazy(loader), [epoch]);
      return React.createElement(
        PanelErrorBoundary,
        {
          panelName: "Hex view",
          resetKey: "file-a",
          onClose: vi.fn(),
          onRetry: () => setEpoch((value) => value + 1),
        },
        React.createElement(
          React.Suspense,
          { fallback: React.createElement("div", null, "Loading panel") },
          React.createElement(LazyPanel),
        ),
      );
    }

    await act(async () => {
      root.render(React.createElement(RetryableLazyPanel));
      await new Promise<void>((resolve) => setTimeout(resolve, 0));
    });
    expect(host.textContent).toContain("temporary chunk failure 1");
    expect(loader).toHaveBeenCalledOnce();

    const retry = Array.from(host.querySelectorAll("button"))
      .find((button) => button.textContent === "Try panel again");
    await act(async () => {
      retry?.click();
      await new Promise<void>((resolve) => setTimeout(resolve, 0));
    });

    expect(loader).toHaveBeenCalledTimes(2);
    expect(host.querySelector("[role=alert]")?.textContent).toContain("temporary chunk failure 2");

    const retryAgain = Array.from(host.querySelectorAll("button"))
      .find((button) => button.textContent === "Try panel again");
    await act(async () => {
      retryAgain?.click();
      await new Promise<void>((resolve) => setTimeout(resolve, 0));
    });

    expect(loader).toHaveBeenCalledTimes(3);
    expect(host.querySelector("#retried-lazy-panel")?.textContent).toBe("Lazy panel restored");
    expect(host.querySelector("[role=alert]")).toBeNull();
  });
});
