import React, { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import CommandPalette from "../src/CommandPalette";
import Help from "../src/Help";
import MenuBar, { type MenuDef } from "../src/MenuBar";

let container: HTMLDivElement;
let root: Root;

const key = (element: Element, value: string) => {
  act(() => {
    element.dispatchEvent(new KeyboardEvent("keydown", { key: value, bubbles: true, cancelable: true }));
  });
};

describe("MenuBar keyboard and ARIA behavior", () => {
  beforeEach(() => {
    container = document.createElement("div");
    document.body.appendChild(container);
    root = createRoot(container);
  });

  afterEach(() => {
    act(() => root.unmount());
    container.remove();
  });

  it("opens with ArrowDown, skips disabled items, wraps, and restores trigger focus", () => {
    const menus: MenuDef[] = [{
      label: "File",
      items: [
        { label: "Open" },
        { label: "Disabled", disabled: true },
        { separator: true },
        { label: "Close" },
      ],
    }];
    act(() => root.render(React.createElement(MenuBar, { menus })));

    const menubar = container.querySelector('[role="menubar"]');
    const trigger = container.querySelector('[role="menuitem"][aria-haspopup="menu"]') as HTMLButtonElement;
    expect(menubar?.getAttribute("aria-label")).toBe("Application menu");
    expect(trigger.getAttribute("aria-expanded")).toBe("false");

    trigger.focus();
    key(trigger, "ArrowDown");
    expect(trigger.getAttribute("aria-expanded")).toBe("true");
    expect(document.activeElement?.textContent).toContain("Open");
    expect(container.querySelector('[role="separator"]')).not.toBeNull();

    key(document.activeElement!, "ArrowDown");
    expect(document.activeElement?.textContent).toContain("Close");
    key(document.activeElement!, "ArrowDown");
    expect(document.activeElement?.textContent).toContain("Open");
    key(document.activeElement!, "Escape");
    expect(document.activeElement).toBe(trigger);
    expect(trigger.getAttribute("aria-expanded")).toBe("false");
  });

  it("switches menus with arrows and exposes checked state", () => {
    const menus: MenuDef[] = [
      { label: "File", items: [{ label: "Open" }] },
      { label: "View", items: [{ label: "Wrap", checked: true }, { label: "Lines", checked: false }] },
    ];
    act(() => root.render(React.createElement(MenuBar, { menus })));
    const triggers = Array.from(container.querySelectorAll('[aria-haspopup="menu"]')) as HTMLButtonElement[];
    triggers[0].focus();
    key(triggers[0], "ArrowDown");
    key(document.activeElement!, "ArrowRight");

    expect(triggers[1].getAttribute("aria-expanded")).toBe("true");
    const checked = container.querySelector('[role="menuitemcheckbox"][aria-checked="true"]');
    const unchecked = container.querySelector('[role="menuitemcheckbox"][aria-checked="false"]');
    expect(checked?.textContent).toContain("Wrap");
    expect(unchecked?.textContent).toContain("Lines");
    expect(document.activeElement?.textContent).toContain("Wrap");
  });

  it("activates a focused item exactly once", () => {
    const action = vi.fn();
    const menus: MenuDef[] = [{ label: "File", items: [{ label: "Open", onClick: action }] }];
    act(() => root.render(React.createElement(MenuBar, { menus })));
    const trigger = container.querySelector('[aria-haspopup="menu"]') as HTMLButtonElement;
    trigger.focus();
    key(trigger, "Enter");
    act(() => (document.activeElement as HTMLButtonElement).click());
    expect(action).toHaveBeenCalledTimes(1);
    expect(trigger.getAttribute("aria-expanded")).toBe("false");
  });

  it("restores focus to the menu trigger after an invoked dialog closes", () => {
    const Harness = () => {
      const [helpOpen, setHelpOpen] = React.useState(false);
      const menus: MenuDef[] = [{
        label: "Help",
        items: [{ label: "Help & shortcuts…", onClick: () => setHelpOpen(true) }],
      }];
      return React.createElement(
        React.Fragment,
        null,
        React.createElement(MenuBar, { menus }),
        helpOpen ? React.createElement(Help, { onClose: () => setHelpOpen(false) }) : null,
      );
    };

    act(() => root.render(React.createElement(Harness)));
    const trigger = container.querySelector('[aria-haspopup="menu"]') as HTMLButtonElement;
    trigger.focus();
    key(trigger, "Enter");
    act(() => (document.activeElement as HTMLButtonElement).click());

    const dialog = container.querySelector<HTMLElement>('[role="dialog"]');
    const activeTopic = container.querySelector<HTMLButtonElement>('[role="tab"]');
    expect(dialog).not.toBeNull();
    expect(document.activeElement).toBe(activeTopic);

    key(activeTopic!, "Escape");
    expect(container.querySelector('[role="dialog"]')).toBeNull();
    expect(document.activeElement).toBe(trigger);
  });

  it("restores focus to the menu trigger after an invoked command palette closes", () => {
    const Harness = () => {
      const [paletteOpen, setPaletteOpen] = React.useState(false);
      const menus: MenuDef[] = [{
        label: "View",
        items: [{ label: "Command palette…", onClick: () => setPaletteOpen(true) }],
      }];
      return React.createElement(
        React.Fragment,
        null,
        React.createElement(MenuBar, { menus }),
        paletteOpen
          ? React.createElement(CommandPalette, {
            commands: [{ id: "noop", label: "No operation", run: vi.fn() }],
            onClose: () => setPaletteOpen(false),
          })
          : null,
      );
    };

    act(() => root.render(React.createElement(Harness)));
    const trigger = container.querySelector('[aria-haspopup="menu"]') as HTMLButtonElement;
    trigger.focus();
    key(trigger, "Enter");
    act(() => (document.activeElement as HTMLButtonElement).click());

    const input = container.querySelector<HTMLInputElement>('[role="combobox"]');
    expect(container.querySelector('[role="dialog"]')).not.toBeNull();
    expect(document.activeElement).toBe(input);

    key(input!, "Escape");
    expect(container.querySelector('[role="dialog"]')).toBeNull();
    expect(document.activeElement).toBe(trigger);
  });
});
