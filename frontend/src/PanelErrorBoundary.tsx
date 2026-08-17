import { Component, createRef, useLayoutEffect, useRef } from "react";
import type { ErrorInfo, ReactNode } from "react";

interface Props {
  children: ReactNode;
  panelName: string;
  resetKey: string;
  onClose: () => void;
  /** Recreate any cached lazy module before the boundary clears its error. */
  onRetry?: () => void;
  onError?: (message: string) => void;
  modal?: boolean;
  returnFocusTo?: HTMLElement | null;
}

interface State {
  error: Error | null;
}

function errorMessage(error: Error): string {
  const message = error.message.trim();
  return (message || "Unknown rendering error").slice(0, 500);
}

const MODAL_FOCUSABLE =
  'button:not([disabled]):not([tabindex="-1"]), input:not([disabled]):not([tabindex="-1"]), [href], [tabindex]:not([tabindex="-1"])';

function FailureContents({ error, panelName, onReset, onClose }: {
  error: Error;
  panelName: string;
  onReset: () => void;
  onClose: () => void;
}) {
  return (
    <>
      <h2>{panelName} could not be displayed</h2>
      <p>
        Quarry stopped this panel after an unexpected response or rendering failure. The open source file was not modified.
      </p>
      <pre>{errorMessage(error)}</pre>
      <div className="q-panel-failure-actions">
        <button className="q-btn q-btn-primary" type="button" onClick={onReset}>Try panel again</button>
        <button className="q-btn" type="button" onClick={onClose}>Close panel</button>
      </div>
    </>
  );
}

function ModalPanelFailure({ error, panelName, onReset, onClose, returnFocusTo }: {
  error: Error;
  panelName: string;
  onReset: () => void;
  onClose: () => void;
  returnFocusTo?: HTMLElement | null;
}) {
  const dialogRef = useRef<HTMLElement>(null);
  const onCloseRef = useRef(onClose);
  const restoreFocusRef = useRef(returnFocusTo ?? null);
  onCloseRef.current = onClose;

  useLayoutEffect(() => {
    const dialog = dialogRef.current;
    dialog?.focus({ preventScroll: true });

    const focusable = (): HTMLElement[] => Array.from(
      dialog?.querySelectorAll<HTMLElement>(MODAL_FOCUSABLE) ?? [],
    );
    const onKey = (event: KeyboardEvent) => {
      // Run before App's workbench shortcuts so a failed lazy modal cannot
      // accidentally open a second modal or operate on the inert editor.
      event.stopImmediatePropagation();
      if (event.key === "Escape") {
        event.preventDefault();
        onCloseRef.current();
        return;
      }
      if ((event.ctrlKey || event.metaKey) && event.key.toLowerCase() === "p") {
        event.preventDefault();
        return;
      }
      if (event.key === "F1") {
        event.preventDefault();
        return;
      }
      if (event.key !== "Tab") return;

      const actions = focusable();
      if (actions.length === 0) {
        event.preventDefault();
        dialog?.focus({ preventScroll: true });
        return;
      }
      const first = actions[0];
      const last = actions[actions.length - 1];
      const active = document.activeElement;
      if (!dialog?.contains(active) || active === dialog) {
        event.preventDefault();
        (event.shiftKey ? last : first).focus({ preventScroll: true });
      } else if (event.shiftKey && active === first) {
        event.preventDefault();
        last.focus({ preventScroll: true });
      } else if (!event.shiftKey && active === last) {
        event.preventDefault();
        first.focus({ preventScroll: true });
      }
    };
    const onFocus = (event: FocusEvent) => {
      if (event.target instanceof Node && !dialog?.contains(event.target)) {
        dialog?.focus({ preventScroll: true });
      }
    };
    window.addEventListener("keydown", onKey, true);
    window.addEventListener("focusin", onFocus, true);
    return () => {
      window.removeEventListener("keydown", onKey, true);
      window.removeEventListener("focusin", onFocus, true);
      const previous = restoreFocusRef.current;
      const restore = () => {
        const active = document.activeElement;
        if (
          previous?.isConnected
          && (active === document.body || (active instanceof Node && dialog?.contains(active)))
        ) {
          previous.focus({ preventScroll: true });
        }
      };
      restore();
      queueMicrotask(restore);
    };
  }, []);

  return (
    <div className="q-close-backdrop" role="presentation">
      <section
        ref={dialogRef}
        className="q-close-dialog q-panel-failure q-panel-failure-modal"
        role="dialog"
        aria-modal="true"
        aria-label={`${panelName} display failure`}
        tabIndex={-1}
        onKeyDown={(event) => event.stopPropagation()}
      >
        <FailureContents error={error} panelName={panelName} onReset={onReset} onClose={onClose} />
      </section>
    </div>
  );
}

export default class PanelErrorBoundary extends Component<Props, State> {
  state: State = { error: null };
  private readonly failureRef = createRef<HTMLElement>();

  static getDerivedStateFromError(error: Error): State {
    return { error };
  }

  componentDidCatch(error: Error, info: ErrorInfo): void {
    console.error(`${this.props.panelName} rendering failed`, error, info.componentStack);
    this.props.onError?.(`${this.props.panelName} could not be displayed: ${errorMessage(error)}`);
    if (!this.props.modal) this.failureRef.current?.focus();
  }

  componentDidUpdate(previous: Props): void {
    if (previous.resetKey !== this.props.resetKey && this.state.error) {
      this.setState({ error: null });
    }
  }

  private reset = (): void => {
    // React.lazy caches both fulfilled and rejected loader promises on the
    // lazy component object. App supplies onRetry to replace that object, so a
    // transient chunk failure is not immediately re-thrown from the same
    // cached rejection when this boundary renders its children again.
    this.props.onRetry?.();
    this.setState({ error: null });
  };

  render(): ReactNode {
    if (!this.state.error) return this.props.children;

    if (this.props.modal) {
      return (
        <ModalPanelFailure
          error={this.state.error}
          panelName={this.props.panelName}
          onReset={this.reset}
          onClose={this.props.onClose}
          returnFocusTo={this.props.returnFocusTo}
        />
      );
    }

    return (
      <section
        ref={this.failureRef}
        className="q-panel-failure"
        role="alert"
        aria-atomic="true"
        aria-label={`${this.props.panelName} display failure`}
        tabIndex={-1}
      >
        <FailureContents
          error={this.state.error}
          panelName={this.props.panelName}
          onReset={this.reset}
          onClose={this.props.onClose}
        />
      </section>
    );
  }
}
