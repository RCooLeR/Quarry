import { useEffect, useRef, useState } from "react";

export interface MenuItem {
  label?: string;
  shortcut?: string;
  onClick?: () => void;
  disabled?: boolean;
  checked?: boolean;
  separator?: boolean;
}

export interface MenuDef {
  label: string;
  items: MenuItem[];
}

interface Props {
  menus: MenuDef[];
}

interface PendingItemFocus {
  menu: number;
  item: number;
}

const wrapIndex = (index: number, length: number) => ((index % length) + length) % length;

export default function MenuBar({ menus }: Props) {
  const [open, setOpen] = useState<number | null>(null);
  const ref = useRef<HTMLDivElement>(null);
  const menuButtons = useRef<Array<HTMLButtonElement | null>>([]);
  const itemButtons = useRef<Array<Array<HTMLButtonElement | null>>>([]);
  const pendingItemFocus = useRef<PendingItemFocus | null>(null);

  const enabledItems = (menuIndex: number) => menus[menuIndex]?.items
    .map((item, itemIndex) => ({ item, itemIndex }))
    .filter(({ item }) => !item.separator && !item.disabled)
    .map(({ itemIndex }) => itemIndex) ?? [];

  const focusTopMenu = (menuIndex: number, preserveOpen: boolean) => {
    if (menus.length === 0) return;
    const next = wrapIndex(menuIndex, menus.length);
    if (preserveOpen) setOpen(next);
    menuButtons.current[next]?.focus();
  };

  const openAndFocusItem = (menuIndex: number, edge: "first" | "last") => {
    if (menus.length === 0) return;
    const nextMenu = wrapIndex(menuIndex, menus.length);
    const enabled = enabledItems(nextMenu);
    if (enabled.length === 0) {
      setOpen(nextMenu);
      menuButtons.current[nextMenu]?.focus();
      return;
    }
    pendingItemFocus.current = {
      menu: nextMenu,
      item: edge === "first" ? enabled[0] : enabled[enabled.length - 1],
    };
    setOpen(nextMenu);
  };

  useEffect(() => {
    const pending = pendingItemFocus.current;
    if (pending == null || open !== pending.menu) return;
    pendingItemFocus.current = null;
    itemButtons.current[pending.menu]?.[pending.item]?.focus();
  }, [open]);

  useEffect(() => {
    if (open === null) return;
    const onDown = (e: MouseEvent) => {
      if (ref.current && !ref.current.contains(e.target as Node)) setOpen(null);
    };
    const onEsc = (e: KeyboardEvent) => {
      if (e.key !== "Escape") return;
      e.preventDefault();
      e.stopPropagation();
      const activeMenu = open;
      setOpen(null);
      menuButtons.current[activeMenu]?.focus();
    };
    window.addEventListener("mousedown", onDown);
    window.addEventListener("keydown", onEsc);
    return () => {
      window.removeEventListener("mousedown", onDown);
      window.removeEventListener("keydown", onEsc);
    };
  }, [open]);

  const onTopKeyDown = (event: React.KeyboardEvent<HTMLButtonElement>, menuIndex: number) => {
    switch (event.key) {
      case "ArrowRight":
        event.preventDefault();
        focusTopMenu(menuIndex + 1, open !== null);
        break;
      case "ArrowLeft":
        event.preventDefault();
        focusTopMenu(menuIndex - 1, open !== null);
        break;
      case "Home":
        event.preventDefault();
        focusTopMenu(0, open !== null);
        break;
      case "End":
        event.preventDefault();
        focusTopMenu(menus.length - 1, open !== null);
        break;
      case "ArrowDown":
      case "Enter":
      case " ":
        event.preventDefault();
        openAndFocusItem(menuIndex, "first");
        break;
      case "ArrowUp":
        event.preventDefault();
        openAndFocusItem(menuIndex, "last");
        break;
      case "Escape":
        if (open !== null) {
          event.preventDefault();
          setOpen(null);
        }
        break;
      default:
        break;
    }
  };

  const onItemKeyDown = (event: React.KeyboardEvent<HTMLButtonElement>, menuIndex: number, itemIndex: number) => {
    const enabled = enabledItems(menuIndex);
    const position = enabled.indexOf(itemIndex);
    switch (event.key) {
      case "ArrowDown":
      case "ArrowUp": {
        event.preventDefault();
        const delta = event.key === "ArrowDown" ? 1 : -1;
        const next = enabled[wrapIndex(position + delta, enabled.length)];
        itemButtons.current[menuIndex]?.[next]?.focus();
        break;
      }
      case "Home":
        event.preventDefault();
        itemButtons.current[menuIndex]?.[enabled[0]]?.focus();
        break;
      case "End":
        event.preventDefault();
        itemButtons.current[menuIndex]?.[enabled[enabled.length - 1]]?.focus();
        break;
      case "ArrowRight":
        event.preventDefault();
        openAndFocusItem(menuIndex + 1, "first");
        break;
      case "ArrowLeft":
        event.preventDefault();
        openAndFocusItem(menuIndex - 1, "first");
        break;
      case "Escape":
        event.preventDefault();
        setOpen(null);
        menuButtons.current[menuIndex]?.focus();
        break;
      case "Tab":
        setOpen(null);
        break;
      default:
        break;
    }
  };

  return (
    <div className="q-menubar" ref={ref} role="menubar" aria-label="Application menu">
      {menus.map((menu, mi) => {
        const menuId = `quarry-menu-${mi}`;
        const triggerId = `quarry-menu-trigger-${mi}`;
        return (
          <div key={menu.label} className="q-menu" role="none">
            <button
              ref={(node) => { menuButtons.current[mi] = node; }}
              id={triggerId}
              role="menuitem"
              aria-haspopup="menu"
              aria-expanded={open === mi}
              aria-controls={open === mi ? menuId : undefined}
              className={"q-menu-label" + (open === mi ? " q-menu-label-open" : "")}
              onKeyDown={(event) => onTopKeyDown(event, mi)}
              onClick={() => setOpen((current) => (current === mi ? null : mi))}
              onMouseEnter={() => setOpen((current) => (current === null ? current : mi))}
            >
              {menu.label}
            </button>
            {open === mi && (
              <div id={menuId} className="q-menu-pop" role="menu" aria-labelledby={triggerId}>
                {menu.items.map((it, ii) => {
                  if (it.separator) return <div key={ii} className="q-menu-sep" role="separator" />;
                  const checkable = it.checked !== undefined;
                  return (
                    <button
                      ref={(node) => {
                        if (!itemButtons.current[mi]) itemButtons.current[mi] = [];
                        itemButtons.current[mi][ii] = node;
                      }}
                      key={ii}
                      role={checkable ? "menuitemcheckbox" : "menuitem"}
                      aria-checked={checkable ? Boolean(it.checked) : undefined}
                      className="q-menu-item"
                      disabled={it.disabled}
                      onKeyDown={(event) => onItemKeyDown(event, mi, ii)}
                      onClick={() => {
                        setOpen(null);
                        // Menu items are removed when their action runs. Move
                        // focus to the persistent menu trigger first so a
                        // dialog opened by the action can record a connected,
                        // logical element to restore when it closes.
                        menuButtons.current[mi]?.focus();
                        it.onClick?.();
                      }}
                    >
                      <span className="q-menu-check" aria-hidden="true">{it.checked ? "✓" : ""}</span>
                      <span className="q-menu-text">{it.label}</span>
                      {it.shortcut && <span className="q-menu-sc" aria-hidden="true">{it.shortcut}</span>}
                    </button>
                  );
                })}
              </div>
            )}
          </div>
        );
      })}
    </div>
  );
}
