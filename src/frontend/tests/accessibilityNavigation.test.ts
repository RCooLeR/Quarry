import { describe, expect, it, vi } from "vitest";
import {
  modalBackgroundAttributes,
  navigateActionList,
  primaryShortcut,
} from "../src/accessibilityNavigation";

function keyEvent(currentTarget: HTMLButtonElement, key: string) {
  return {
    currentTarget,
    key,
    preventDefault: vi.fn(),
  } as unknown as React.KeyboardEvent<HTMLButtonElement>;
}

describe("App accessibility helpers", () => {
  it("uses a platform-neutral primary-modifier label", () => {
    expect(primaryShortcut("f")).toBe("Ctrl/Cmd+F");
  });

  it("only marks the workbench inert and hidden while a modal is active", () => {
    expect(modalBackgroundAttributes(false)).toEqual({});
    expect(modalBackgroundAttributes(true)).toEqual({ inert: true, "aria-hidden": true });
  });

  it("moves focus through bounded action lists with arrows, Home, and End", () => {
    const list = document.createElement("div");
    list.dataset.actionList = "";
    const first = document.createElement("button");
    const second = document.createElement("button");
    const third = document.createElement("button");
    const unrelated = document.createElement("button");
    for (const button of [first, second, third]) button.dataset.listAction = "true";
    list.append(first, unrelated, second, third);
    document.body.append(list);

    second.focus();
    const down = keyEvent(second, "ArrowDown");
    navigateActionList(down);
    expect(down.preventDefault).toHaveBeenCalledOnce();
    expect(document.activeElement).toBe(third);

    navigateActionList(keyEvent(third, "ArrowDown"));
    expect(document.activeElement).toBe(first);
    navigateActionList(keyEvent(first, "End"));
    expect(document.activeElement).toBe(third);
    navigateActionList(keyEvent(third, "Home"));
    expect(document.activeElement).toBe(first);
    navigateActionList(keyEvent(first, "ArrowUp"));
    expect(document.activeElement).toBe(third);

    const ignored = keyEvent(third, "Enter");
    navigateActionList(ignored);
    expect(ignored.preventDefault).not.toHaveBeenCalled();
    list.remove();
  });
});
