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

export default function MenuBar({ menus }: Props) {
  const [open, setOpen] = useState<number | null>(null);
  const ref = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (open === null) return;
    const onDown = (e: MouseEvent) => {
      if (ref.current && !ref.current.contains(e.target as Node)) setOpen(null);
    };
    const onEsc = (e: KeyboardEvent) => {
      if (e.key === "Escape") setOpen(null);
    };
    window.addEventListener("mousedown", onDown);
    window.addEventListener("keydown", onEsc);
    return () => {
      window.removeEventListener("mousedown", onDown);
      window.removeEventListener("keydown", onEsc);
    };
  }, [open]);

  return (
    <div className="q-menubar" ref={ref}>
      {menus.map((menu, mi) => (
        <div key={menu.label} className="q-menu">
          <button
            className={"q-menu-label" + (open === mi ? " q-menu-label-open" : "")}
            onClick={() => setOpen((o) => (o === mi ? null : mi))}
            onMouseEnter={() => setOpen((o) => (o === null ? o : mi))}
          >
            {menu.label}
          </button>
          {open === mi && (
            <div className="q-menu-pop">
              {menu.items.map((it, ii) =>
                it.separator ? (
                  <div key={ii} className="q-menu-sep" />
                ) : (
                  <button
                    key={ii}
                    className="q-menu-item"
                    disabled={it.disabled}
                    onClick={() => {
                      setOpen(null);
                      it.onClick?.();
                    }}
                  >
                    <span className="q-menu-check">{it.checked ? "✓" : ""}</span>
                    <span className="q-menu-text">{it.label}</span>
                    {it.shortcut && <span className="q-menu-sc">{it.shortcut}</span>}
                  </button>
                ),
              )}
            </div>
          )}
        </div>
      ))}
    </div>
  );
}
