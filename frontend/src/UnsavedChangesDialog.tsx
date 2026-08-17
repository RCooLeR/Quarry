import { useEffect, useRef } from "react";
import type { CloseChoice, CloseDecisionContext } from "./closeSafety";

interface Props {
  context: CloseDecisionContext;
  onChoose: (choice: CloseChoice) => void;
}

const FOCUSABLE =
  'button:not([disabled]):not([tabindex="-1"]), input:not([disabled]):not([tabindex="-1"]), [href], [tabindex]:not([tabindex="-1"])';

function baseName(path: string): string {
  const i = Math.max(path.lastIndexOf("/"), path.lastIndexOf("\\"));
  return i >= 0 ? path.slice(i + 1) : path;
}

export default function UnsavedChangesDialog({ context, onChoose }: Props) {
  const dialogRef = useRef<HTMLElement>(null);
  const cancelRef = useRef<HTMLButtonElement>(null);
  const onChooseRef = useRef(onChoose);
  const restoreFocusRef = useRef<HTMLElement | null>(
    typeof document !== "undefined" && document.activeElement instanceof HTMLElement
      ? document.activeElement
      : null,
  );
  const { target, staging } = context;

  useEffect(() => {
    onChooseRef.current = onChoose;
  }, [onChoose]);

  useEffect(() => {
    const dialog = dialogRef.current;
    cancelRef.current?.focus();

    const onKey = (event: KeyboardEvent) => {
      if (event.key === "Escape") {
        event.preventDefault();
        event.stopImmediatePropagation();
        onChooseRef.current("cancel");
        return;
      }
      if (event.key !== "Tab") return;

      const focusable = Array.from(dialog?.querySelectorAll<HTMLElement>(FOCUSABLE) ?? []);
      if (focusable.length === 0) {
        event.preventDefault();
        dialog?.focus();
        return;
      }
      const first = focusable[0];
      const last = focusable[focusable.length - 1];
      if (event.shiftKey && (document.activeElement === first || !dialog?.contains(document.activeElement))) {
        event.preventDefault();
        last.focus();
      } else if (!event.shiftKey && (document.activeElement === last || !dialog?.contains(document.activeElement))) {
        event.preventDefault();
        first.focus();
      }
    };

    const onFocus = (event: FocusEvent) => {
      if (event.target instanceof Node && !dialog?.contains(event.target)) {
        cancelRef.current?.focus();
      }
    };

    window.addEventListener("keydown", onKey, true);
    window.addEventListener("focusin", onFocus, true);
    return () => {
      window.removeEventListener("keydown", onKey, true);
      window.removeEventListener("focusin", onFocus, true);
      const previous = restoreFocusRef.current;
      const active = document.activeElement;
      if (
        previous?.isConnected &&
        (active === document.body || (active instanceof Node && dialog?.contains(active)))
      ) {
        previous.focus();
      }
    };
  }, []);

  const applicationDetail =
    target.scope === "application" && target.position != null && target.total != null
      ? `Reviewing file ${target.position} of ${target.total} before application exit.`
      : "This tab has pending edits.";

  return (
    <div className="q-close-backdrop" role="presentation">
      <section
        ref={dialogRef}
        className="q-close-dialog"
        role="dialog"
        aria-modal="true"
        aria-labelledby="q-close-title"
        aria-describedby="q-close-description q-close-note"
        tabIndex={-1}
      >
        <h2 id="q-close-title">Save changes to {baseName(target.path)}?</h2>
        <p id="q-close-description">
          {applicationDetail} {staging.editCount} staged {staging.editCount === 1 ? "edit" : "edits"} will be lost if discarded.
        </p>
        <p className="q-close-path" title={target.path}>{target.path}</p>
        <p id="q-close-note" className="q-close-note">The source file will not be overwritten. Save copy writes the edited content to a path you choose.</p>
        <div className="q-close-actions">
          <button type="button" className="q-btn q-btn-primary" onClick={() => onChoose("save-copy")}>Save copy…</button>
          <button type="button" className="q-btn q-btn-danger" onClick={() => onChoose("discard")}>Discard</button>
          <button type="button" ref={cancelRef} className="q-btn" onClick={() => onChoose("cancel")}>Cancel</button>
        </div>
      </section>
    </div>
  );
}
