import type { FileMetaData } from "./editor/QuarryEditor";

export interface SidebarTab {
  fileId: string;
  meta: FileMetaData;
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
  return (
    <aside className={"q-sidebar" + (collapsed ? " q-sidebar-collapsed" : "")}>
      <div className="q-sb-head">
        <button className="q-sb-toggle" title={collapsed ? "Expand (Ctrl+B)" : "Collapse (Ctrl+B)"} onClick={onToggle}>
          {collapsed ? "»" : "«"}
        </button>
        {!collapsed && <span className="q-sb-title">Open files ({tabs.length})</span>}
      </div>
      <div className="q-sb-list">
        {tabs.map((t) => {
          const active = t.fileId === activeId;
          return (
            <div
              key={t.fileId}
              className={"q-sb-item" + (active ? " q-sb-item-active" : "")}
              title={t.meta.path}
              onClick={() => onActivate(t.fileId)}
            >
              <span className={"q-sb-badge q-badge-" + (t.meta.detected || "txt").toLowerCase()}>
                {collapsed ? typeBadge(t.meta.detected).slice(0, 1) : typeBadge(t.meta.detected)}
              </span>
              {!collapsed && (
                <>
                  <span className="q-sb-name">{baseName(t.meta.path)}</span>
                  <span className="q-sb-size">{fmtBytes(t.meta.size)}</span>
                  <span className="q-sb-close" title="Close" onClick={(e) => onClose(t.fileId, e)}>
                    ×
                  </span>
                </>
              )}
            </div>
          );
        })}
        {tabs.length === 0 && !collapsed && <div className="q-sb-empty">No files open</div>}
      </div>
    </aside>
  );
}
