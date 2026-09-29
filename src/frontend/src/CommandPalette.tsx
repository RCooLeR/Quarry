import { useEffect, useMemo, useRef, useState } from "react";
import { boundedUtf8Text } from "./textInputLimits";

export interface Command {
  id: string;
  label: string;
  hint?: string;
  group?: string;
  enabled?: boolean;
  disabledReason?: string;
  run: () => void;
}

interface Props {
  commands: Command[];
  onClose: () => void;
  returnFocusTo?: HTMLElement | null;
}

const FOCUSABLE =
  'button:not([disabled]):not([tabindex="-1"]), input:not([disabled]):not([tabindex="-1"]), [href], [tabindex]:not([tabindex="-1"])';
export const COMMAND_PALETTE_QUERY_MAX_BYTES = 4 * 1024;

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

export default function CommandPalette({ commands, onClose, returnFocusTo }: Props) {
  const [query, setQuery] = useState("");
  const [sel, setSel] = useState(0);
  const listRef = useRef<HTMLDivElement>(null);
  const dialogRef = useRef<HTMLDivElement>(null);
  const inputRef = useRef<HTMLInputElement>(null);
  const restoreFocusRef = useRef<HTMLElement | null>(
    returnFocusTo ?? (typeof document !== "undefined" && document.activeElement instanceof HTMLElement
      ? document.activeElement
      : null),
  );

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
    el?.scrollIntoView?.({ block: "nearest" });
  }, [sel]);

  useEffect(() => {
    const dialog = dialogRef.current;
    inputRef.current?.focus();
    return () => {
      const previous = restoreFocusRef.current;
      const restore = () => {
        const active = document.activeElement;
        if (
          previous?.isConnected &&
          (active === document.body || (active instanceof Node && dialog?.contains(active)))
        ) {
          previous.focus({ preventScroll: true });
        }
      };
      restore();
      queueMicrotask(restore);
    };
  }, []);

  const choose = (c?: Command) => {
    if (!c || c.enabled === false) return;
    onClose();
    c.run();
  };

  const trapTab = (event: React.KeyboardEvent<HTMLDivElement>) => {
    const focusable = Array.from(dialogRef.current?.querySelectorAll<HTMLElement>(FOCUSABLE) ?? []);
    if (focusable.length === 0) {
      event.preventDefault();
      dialogRef.current?.focus();
      return;
    }
    const first = focusable[0];
    const last = focusable[focusable.length - 1];
    if (event.shiftKey && (document.activeElement === first || !dialogRef.current?.contains(document.activeElement))) {
      event.preventDefault();
      last.focus();
    } else if (!event.shiftKey && (document.activeElement === last || !dialogRef.current?.contains(document.activeElement))) {
      event.preventDefault();
      first.focus();
    }
  };

  const activeOptionId = results[sel] ? `q-command-option-${sel}` : undefined;
  const resultSummary = results.length === 1 ? "1 matching command" : `${results.length} matching commands`;

  return (
    <div
      className="q-cmd-backdrop"
      role="presentation"
      onMouseDown={(event) => {
        if (event.target === event.currentTarget) onClose();
      }}
    >
      <div
        ref={dialogRef}
        className="q-cmd"
        role="dialog"
        aria-modal="true"
        aria-labelledby="q-command-title"
        tabIndex={-1}
        onKeyDown={(event) => {
          // Do not let application shortcuts operate on the workbench behind
          // this modal. Input/list handling still runs before this bubble step.
          event.stopPropagation();
          if (event.key === "Tab") {
            trapTab(event);
          } else if (event.key === "Escape") {
            event.preventDefault();
            onClose();
          }
        }}
      >
        <h2 id="q-command-title" className="q-sr-only">Command palette</h2>
        <input
          ref={inputRef}
          className="q-cmd-input"
          spellCheck={false}
          placeholder="Type a command, or a table name…"
          role="combobox"
          aria-label="Search commands"
          aria-autocomplete="list"
          aria-controls="q-command-results"
          aria-expanded="true"
          aria-activedescendant={activeOptionId}
          aria-describedby="q-command-status"
          value={query}
          maxLength={COMMAND_PALETTE_QUERY_MAX_BYTES}
          onChange={(e) => setQuery(boundedUtf8Text(e.target.value, COMMAND_PALETTE_QUERY_MAX_BYTES).value)}
          onKeyDown={(e) => {
            if (e.key === "ArrowDown") {
              e.preventDefault();
              if (results.length > 0) setSel((i) => Math.min(i + 1, results.length - 1));
            } else if (e.key === "ArrowUp") {
              e.preventDefault();
              if (results.length > 0) setSel((i) => Math.max(i - 1, 0));
            } else if (e.key === "Enter") {
              e.preventDefault();
              choose(results[sel]);
            }
          }}
        />
        <span id="q-command-status" className="q-sr-only" role="status" aria-live="polite">
          {resultSummary}
        </span>
        <div id="q-command-results" className="q-cmd-list" ref={listRef} role="listbox" aria-label="Matching commands">
          {results.length === 0 && <div className="q-cmd-empty" role="status">No matching commands</div>}
          {results.map((c, i) => (
            <button
              type="button"
              key={c.id}
              id={`q-command-option-${i}`}
              data-i={i}
              role="option"
              aria-selected={i === sel}
              aria-disabled={c.enabled === false ? "true" : undefined}
              tabIndex={-1}
              className={"q-cmd-item" + (i === sel ? " q-cmd-item-sel" : "") + (c.enabled === false ? " q-cmd-item-disabled" : "")}
              onMouseEnter={() => setSel(i)}
              onMouseDown={(event) => event.preventDefault()}
              onClick={() => choose(c)}
            >
              {c.group && <span className="q-cmd-group">{c.group}</span>}
              <span className="q-cmd-label">{c.label}</span>
              {(c.enabled === false ? c.disabledReason ?? "Unavailable" : c.hint) && (
                <span className="q-cmd-hint">{c.enabled === false ? c.disabledReason ?? "Unavailable" : c.hint}</span>
              )}
            </button>
          ))}
        </div>
      </div>
    </div>
  );
}
