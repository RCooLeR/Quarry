import type { StagingStateData } from "./editor/QuarryEditor";

export type CloseChoice = "save-copy" | "discard" | "cancel";
export type CloseScope = "tab" | "application";

export interface CloseTarget {
  fileId: string;
  path: string;
  dirty: boolean;
  editorAttached: boolean;
  scope: CloseScope;
  position?: number;
  total?: number;
}

export interface CloseSaveResult {
  mode: string;
  outputPath: string;
  bytesWritten: number;
}

export interface CloseDecisionContext {
  target: CloseTarget;
  staging: StagingStateData;
}

export interface CloseSafetyDependencies {
  flushEditor: () => Promise<void>;
  getStaging: (fileId: string) => Promise<StagingStateData>;
  choose: (context: CloseDecisionContext) => Promise<CloseChoice>;
  saveCopy: (fileId: string) => Promise<CloseSaveResult>;
  makeEditorReadOnly: () => Promise<void>;
  discard: (fileId: string) => Promise<StagingStateData>;
  onStaging: (fileId: string, staging: StagingStateData) => void;
}

export type CloseResolution =
  | { outcome: "clean" }
  | { outcome: "saved"; save: CloseSaveResult }
  | { outcome: "discarded" }
  | { outcome: "cancelled" };

/**
 * Resolves one tab's edit state before its session may be removed. The caller
 * still owns the final backend CloseFile call, which must also complete before
 * removing the tab from UI state.
 */
export async function resolveTabForClose(
  target: CloseTarget,
  deps: CloseSafetyDependencies,
): Promise<CloseResolution> {
  if (target.editorAttached) {
    // Cancels/drains a debounce-pending edit. A rejection deliberately aborts
    // before staging inspection, save, discard, or backend close.
    await deps.flushEditor();
  } else if (target.dirty) {
    // Pending text can only live in the one attached editor. If App state ever
    // claims otherwise, do not guess which document owns that text.
    throw new Error(`cannot close inactive dirty session ${target.fileId}`);
  }

  const staging = await deps.getStaging(target.fileId);
  deps.onStaging(target.fileId, staging);
  if (staging.editCount === 0) return { outcome: "clean" };

  const choice = await deps.choose({ target, staging });
  if (choice === "cancel") return { outcome: "cancelled" };

  if (choice === "save-copy") {
    // The modal owns focus, but drain once more before opening the native save
    // dialog so a queued editor update cannot be omitted from the copy.
    if (target.editorAttached) await deps.flushEditor();
    const save = await deps.saveCopy(target.fileId);
    if (save.mode !== "copy" || !save.outputPath) {
      // Native save-dialog cancellation is cancellation of the close as well.
      return { outcome: "cancelled" };
    }
    // SaveCopy deliberately keeps the staged session intact for ordinary Save
    // copy commands. In a confirmed close flow, however, retaining those edits
    // would make the backend CloseFile guard reject the close. Clear only this
    // successfully saved session. Make the editor read-only *before* clearing
    // backend staging. Besides pairing visible raw bytes with a valid backend
    // state, this closes the input race where text typed while DiscardEdits was
    // in flight would try to stage against a session that no longer exists.
    if (target.editorAttached) await deps.makeEditorReadOnly();
    const discarded = await deps.discard(target.fileId);
    if (discarded.editCount !== 0) {
      throw new Error(`discard after save did not clear staged edits for ${target.fileId}`);
    }
    deps.onStaging(target.fileId, discarded);
    return { outcome: "saved", save };
  }

  if (target.editorAttached) await deps.makeEditorReadOnly();
  const discarded = await deps.discard(target.fileId);
  if (discarded.editCount !== 0) {
    throw new Error(`discard did not clear staged edits for ${target.fileId}`);
  }
  deps.onStaging(target.fileId, discarded);
  return { outcome: "discarded" };
}

/** Keeps the UI/session owner intact unless the backend release succeeds. */
export async function closeBackendSession(
  fileId: string,
  closeFile: (fileId: string) => Promise<void>,
  removeTab: () => void,
): Promise<void> {
  await closeFile(fileId);
  removeTab();
}

/** Resolves application-exit decisions serially so native save dialogs cannot overlap. */
export async function resolveApplicationClose<T>(
  targets: readonly T[],
  resolve: (target: T, position: number, total: number) => Promise<CloseResolution>,
): Promise<boolean> {
  for (let i = 0; i < targets.length; i++) {
    const result = await resolve(targets[i], i + 1, targets.length);
    if (result.outcome === "cancelled") return false;
  }
  return true;
}
