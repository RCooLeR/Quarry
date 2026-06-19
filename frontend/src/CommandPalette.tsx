import { useEffect, useMemo, useRef, useState } from "react";

export interface Command {
  id: string;
  label: string;
  hint?: string;
  group?: string;
  run: () => void;
}

interface Props {
  commands: Command[];
  onClose: () => void;
}

// Subsequence fuzzy score; -1 if not all query chars match in order.
function score(q: string, text: string): number {
  if (!q) return 0;
  const t = text.toLowerCase();
  let qi = 0;
  let s = 0;
  let streak = 0;
  for (let ti = 0; ti < t.length && qi < q.length; ti++) {
    if (t[ti] === q[qi]) {
      streak++;
      s += 1 + streak + (ti === 0 ? 4 : 0);
      qi++;
    } else {
      streak = 0;
    }
  }
  return qi === q.length ? s : -1;
}

export default function CommandPalette({ commands, onClose }: Props) {
  const [query, setQuery] = useState("");
  const [sel, setSel] = useState(0);
  const listRef = useRef<HTMLDivElement>(null);

  const results = useMemo(() => {
    const q = query.trim().toLowerCase();
    return commands
      .map((c) => ({ c, s: score(q, c.label + " " + (c.hint ?? "") + " " + (c.group ?? "")) }))
      .filter((r) => r.s >= 0)
      .sort((a, b) => b.s - a.s)
      .slice(0, 60)
      .map((r) => r.c);
  }, [commands, query]);

  useEffect(() => {
    setSel(0);
  }, [query]);

  useEffect(() => {
    const el = listRef.current?.querySelector<HTMLElement>(`[data-i="${sel}"]`);
    el?.scrollIntoView({ block: "nearest" });
  }, [sel]);

  const choose = (c?: Command) => {
    if (!c) return;
    onClose();
    c.run();
  };

  return (
    <div className="q-cmd-backdrop" onMouseDown={onClose}>
      <div className="q-cmd" onMouseDown={(e) => e.stopPropagation()}>
        <input
          className="q-cmd-input"
          autoFocus
          spellCheck={false}
          placeholder="Type a command, or a table name…"
          value={query}
          onChange={(e) => setQuery(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "ArrowDown") { e.preventDefault(); setSel((i) => Math.min(i + 1, results.length - 1)); }
            else if (e.key === "ArrowUp") { e.preventDefault(); setSel((i) => Math.max(i - 1, 0)); }
            else if (e.key === "Enter") { e.preventDefault(); choose(results[sel]); }
            else if (e.key === "Escape") { e.preventDefault(); onClose(); }
          }}
        />
        <div className="q-cmd-list" ref={listRef}>
          {results.length === 0 && <div className="q-cmd-empty">No matching commands</div>}
          {results.map((c, i) => (
            <div
              key={c.id}
              data-i={i}
              className={"q-cmd-item" + (i === sel ? " q-cmd-item-sel" : "")}
              onMouseEnter={() => setSel(i)}
              onMouseDown={(e) => { e.preventDefault(); choose(c); }}
            >
              {c.group && <span className="q-cmd-group">{c.group}</span>}
              <span className="q-cmd-label">{c.label}</span>
              {c.hint && <span className="q-cmd-hint">{c.hint}</span>}
            </div>
          ))}
        </div>
      </div>
    </div>
  );
}
