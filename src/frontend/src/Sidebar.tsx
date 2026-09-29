import { useRef } from "react";
import type { FileMetaData } from "./editor/QuarryEditor";

export interface SidebarTab {
  fileId: string;
  meta: FileMetaData;
  pendingEdits?: boolean;
}

interface Props {
  tabs: SidebarTab[];
  activeId: string | null;
  collapsed: boolean;
  onActivate: (fileId: string) => void;
  onClose: (fileId: string, e: React.MouseEvent) => void;
  onToggle: () => void;
}

function baseName(p: string): string {
  const i = Math.max(p.lastIndexOf("/"), p.lastIndexOf("\\"));
  return i >= 0 ? p.slice(i + 1) : p;
}

function fmtBytes(n: number): string {
  if (n < 1024) return `${n} B`;
  const units = ["KB", "MB", "GB", "TB"];
  let v = n / 1024;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v.toFixed(v < 10 ? 1 : 0)} ${units[i]}`;
}

function typeBadge(detected: string): string {
  const d = (detected || "").toUpperCase();
  return d || "TXT";
}

export default function Sidebar({ tabs, activeId, collapsed, onActivate, onClose, onToggle }: Props) {
  const tabRefs = useRef<Array<HTMLButtonElement | null>>([]);
  const activeIndex = tabs.findIndex((tab) => tab.fileId === activeId);
  const tabbableIndex = activeIndex >= 0 ? activeIndex : 0;

  const activateAt = (index: number) => {
    if (tabs.length === 0) return;
    const next = (index + tabs.length) % tabs.length;
    onActivate(tabs[next].fileId);
    tabRefs.current[next]?.focus();
  };

  return (
    <aside className={"q-sidebar" + (collapsed ? " q-sidebar-collapsed" : "")} aria-label="Open files">
      <div className="q-sb-head">
        <button
          type="button"
          className="q-sb-toggle"
          title={collapsed ? "Expand (Ctrl/Cmd+B)" : "Collapse (Ctrl/Cmd+B)"}
          aria-label={collapsed ? "Show open-file details" : "Hide open-file details"}
          aria-controls="q-open-files"
          aria-expanded={!collapsed}
          onClick={onToggle}
        >
          <span aria-hidden="true">{collapsed ? "»" : "«"}</span>
        </button>
        {!collapsed && <span className="q-sb-title">Open files ({tabs.length})</span>}
      </div>
      <div
        id="q-open-files"
        className="q-sb-list"
        role="list"
        aria-label="Open files"
      >
        {tabs.map((t, index) => {
          const active = t.fileId === activeId;
          const name = baseName(t.meta.path);
          const fileType = typeBadge(t.meta.detected);
          const size = fmtBytes(t.meta.size);
          const label = `${name}, ${fileType}, ${size}${t.pendingEdits ? ", pending edits" : ""}`;
          return (
            <div
              key={t.fileId}
              role="listitem"
              className={"q-sb-item" + (active ? " q-sb-item-active" : "")}
            >
              <button
                ref={(element) => { tabRefs.current[index] = element; }}
                type="button"
                data-file-id={t.fileId}
                aria-current={active ? "page" : undefined}
                aria-label={label}
                tabIndex={index === tabbableIndex ? 0 : -1}
                className="q-sb-activate"
                title={t.meta.path}
                onClick={() => onActivate(t.fileId)}
                onKeyDown={(event) => {
                  if (event.key === "ArrowDown" || event.key === "ArrowRight") {
                    event.preventDefault();
                    activateAt(index + 1);
                  } else if (event.key === "ArrowUp" || event.key === "ArrowLeft") {
                    event.preventDefault();
                    activateAt(index - 1);
                  } else if (event.key === "Home") {
                    event.preventDefault();
                    activateAt(0);
                  } else if (event.key === "End") {
                    event.preventDefault();
                    activateAt(tabs.length - 1);
                  }
                }}
              >
                <span className={"q-sb-badge q-badge-" + (t.meta.detected || "txt").toLowerCase()} aria-hidden="true">
                  {collapsed ? fileType.slice(0, 1) : fileType}
                </span>
                {!collapsed && (
                  <>
                    <span className="q-sb-name">{name}</span>
                    {t.pendingEdits && <span className="q-sb-unsaved" title="Pending edits" aria-hidden="true">●</span>}
                    <span className="q-sb-size">{size}</span>
                  </>
                )}
              </button>
              {!collapsed && (
                <button
                  type="button"
                  data-file-id={t.fileId}
                  className="q-sb-close"
                  title={`Close ${name}`}
                  aria-label={`Close ${name}`}
                  onClick={(event) => onClose(t.fileId, event)}
                >
                  <span aria-hidden="true">×</span>
                </button>
              )}
            </div>
          );
        })}
        {tabs.length === 0 && !collapsed && <div className="q-sb-empty">No files open</div>}
      </div>
    </aside>
  );
}
