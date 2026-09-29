import React, { act } from "react";
import { createRoot } from "react-dom/client";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const fileService = vi.hoisted(() => ({ GetHexWindow: vi.fn() }));
vi.mock("../bindings/github.com/quarry/quarry-wails3", () => ({ FileService: fileService }));

import HexView from "../src/HexView";

describe("hex window navigation at pinned viewport edges", () => {
  let host: HTMLDivElement;
  let root: ReturnType<typeof createRoot>;

  beforeEach(() => {
    vi.resetAllMocks();
    vi.useFakeTimers();
    host = document.createElement("div");
    document.body.append(host);
    root = createRoot(host);
    fileService.GetHexWindow.mockImplementation((_fileId, startByte) => Promise.resolve({
      startByte,
      nextByte: startByte === 0 ? 65536 : 65552,
      atBof: startByte === 0,
      atEof: startByte !== 0,
      lines: [{ offset: startByte, hex: "41", ascii: "A" }],
    }));
  });

  afterEach(() => {
    act(() => root.unmount());
    host.remove();
    vi.useRealTimers();
  });

  async function render() {
    await act(async () => {
      root.render(React.createElement(HexView, { fileId: "file", onError: vi.fn() }));
    });
    await act(async () => { await vi.advanceTimersByTimeAsync(100); });
    const viewport = host.querySelector<HTMLDivElement>(".q-hex")!;
    Object.defineProperties(viewport, {
      clientHeight: { configurable: true, value: 600 },
      scrollHeight: { configurable: true, value: 600 },
    });
    return viewport;
  }

  async function wheel(viewport: HTMLDivElement, deltaY: number) {
    await act(async () => {
      viewport.dispatchEvent(new WheelEvent("wheel", { bubbles: true, deltaY }));
    });
    await act(async () => { await vi.advanceTimersByTimeAsync(100); });
  }

  it("can leave a partial final window without requiring a scroll event", async () => {
    const viewport = await render();
    await wheel(viewport, 100);
    expect(fileService.GetHexWindow).toHaveBeenLastCalledWith("file", 65536, 65536);
    expect(viewport.scrollTop).toBe(0);

    await wheel(viewport, -100);
    expect(fileService.GetHexWindow).toHaveBeenLastCalledWith("file", 0, 65536);
    expect(fileService.GetHexWindow).toHaveBeenCalledTimes(3);
  });

  it("does not request windows beyond BOF or EOF", async () => {
    const viewport = await render();
    await wheel(viewport, -100);
    expect(fileService.GetHexWindow).toHaveBeenCalledTimes(1);
    await wheel(viewport, 100);
    await wheel(viewport, 100);
    expect(fileService.GetHexWindow).toHaveBeenCalledTimes(2);
  });
});
