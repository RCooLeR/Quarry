import { lazy, Suspense, useCallback, useEffect, useLayoutEffect, useMemo, useReducer, useRef, useState } from "react";
import { Events } from "@wailsio/runtime";
import { AppLifecycle, FileService } from "../bindings/github.com/quarry/quarry-wails3";
import type { QuarryEditor, FileMetaData, StagingStateData, WindowStatus } from "./editor/QuarryEditor";
import { RequestGenerationGate } from "./requestGeneration";
import type { ActiveRequestToken, FileRequestToken } from "./requestGeneration";
import { isSQLAnalysisJob, jobProgressLabel, reduceJobEvent } from "./jobState";
import type { ActiveJobState, JobEventType } from "./jobState";
import { presentSearchPage } from "./searchPresentation";
import { closeBackendSession, resolveApplicationClose, resolveTabForClose } from "./closeSafety";
import type { CloseChoice, CloseDecisionContext, CloseScope } from "./closeSafety";
import UnsavedChangesDialog from "./UnsavedChangesDialog";
import PanelErrorBoundary from "./PanelErrorBoundary";
import { normalizeSqlSummary } from "./bridgePayloads";
import type { NormalizedSqlSummaryResult } from "./bridgePayloads";
import { cacheSqlAnalysis } from "./sqlAnalysisCache";
import Sidebar from "./Sidebar";
import MenuBar from "./MenuBar";
import type { MenuDef, MenuItem } from "./MenuBar";
import CommandPalette from "./CommandPalette";
import type { Command } from "./CommandPalette";
import XRay from "./XRay";
import type { XRayRegion } from "./XRay";
import { formatBuildInfo } from "./buildInfo";
import { modalBackgroundAttributes, navigateActionList, primaryShortcut } from "./accessibilityNavigation";
import {
  initialNotificationState,
  notificationDetailsText,
  notificationReducer,
} from "./notificationState";
import type {
  NotificationAction,
  NotificationInput,
  NotificationOwner,
} from "./notificationState";
import {
  hasSourcePathInput,
  isBoundedSourcePathInput,
  SOURCE_PATH_INPUT_MAX_CODE_UNITS,
} from "./sourcePathInput";
import {
  browserLocalStorage,
  clearWorkspacePathData,
  commitWorkspacePreferences,
  loadLocalSettings,
  nextRecentPaths,
  normalizeBookmarks,
  persistBookmarks,
  persistRecentPaths,
  persistSessionPaths,
  saveByteOffsets,
  saveTheme,
} from "./workspacePrivacy";
import type { QuarryTheme, StoredBookmarks, WorkspacePreferences } from "./workspacePrivacy";
import { mergeCsvDetection } from "./csvDialect";
import type { CsvDialect } from "./csvDialect";
import { normalizeSurfaceForFile } from "./activeSurface";
import { normalizeNativeDrop } from "./nativeDrop";
import { boundedPaletteTables, MAX_OPEN_FILE_SESSIONS } from "./productLimits";
import {
  boundedUtf8Text,
  SEARCH_PLAIN_INPUT_MAX_BYTES,
  SEARCH_REGEX_INPUT_MAX_BYTES,
} from "./textInputLimits";
import "./quarry.css";

const loadTools = () => import("./Tools");
const loadCsvGrid = () => import("./CsvGrid");
const loadHexView = () => import("./HexView");
const loadDiffView = () => import("./DiffView");
const loadHelp = () => import("./Help");

type LazyPanel = "tools" | "csv" | "hex" | "diff" | "help";
type LazyPanelEpochs = Record<LazyPanel, number>;

const initialLazyPanelEpochs: LazyPanelEpochs = {
  tools: 0,
  csv: 0,
  hex: 0,
  diff: 0,
  help: 0,
};

export function LoadingHelpDialog({ onClose, returnFocusTo }: {
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

    const onKey = (event: KeyboardEvent) => {
      // This listener runs in capture phase so the workbench's global
      // shortcuts cannot open a second modal while the lazy Help chunk waits.
      event.stopImmediatePropagation();
      if (event.key === "Escape") {
        event.preventDefault();
        onCloseRef.current();
      } else if (event.key === "Tab") {
        event.preventDefault();
        dialog?.focus({ preventScroll: true });
      } else if ((event.ctrlKey || event.metaKey) && event.key.toLowerCase() === "p") {
        event.preventDefault();
      } else if (event.key === "F1") {
        event.preventDefault();
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
    <div className="q-cmd-backdrop" role="presentation">
      <section
        ref={dialogRef}
        className="q-close-dialog q-close-progress"
        role="dialog"
        aria-modal="true"
        aria-label="Loading help"
        tabIndex={-1}
        onKeyDown={(event) => event.stopPropagation()}
      >
        <p role="status">Loading help…</p>
      </section>
    </div>
  );
}

interface Tab {
  fileId: string;
  meta: FileMetaData;
  startByte: number;
}

interface PendingTabCloseFocus {
  fileId: string;
  invoker: HTMLElement;
  outcome: "removed" | "retained";
  replacementFileId?: string;
}

interface StagedEditData {
  start: number;
  end: number;
  line: number;
  old: string;
  new: string;
}

interface TabEditState {
  dirty: boolean;
  staging: StagingStateData | null;
}

interface SearchContext {
  fileId: string;
  query: string;
  regex: boolean;
  caseSensitive: boolean;
  wholeWord: boolean;
}

interface SearchResultsSession {
  request: ActiveRequestToken;
  context: SearchContext;
}

interface ActiveInteractiveSearch {
  requestId: string;
  request: ActiveRequestToken;
}

interface LineResolution {
  offset: number;
  resolvedLine: number;
  exact: boolean;
  found: boolean;
  indexComplete: boolean;
  limited: boolean;
}
interface FileStateResult { size: number; modTimeNanos: number; sameOpenedFile: boolean; changedFromOpen: boolean; }
const MAX_XRAY_REGIONS = 512;
const GOTO_POSITION_INPUT_MAX_CODE_UNITS = 128;
const FileStateService = FileService as unknown as {
  FileState: (fileId: string) => Promise<FileStateResult>;
};
// PrepareEditSession is generated with the other bridge methods. Keep this
// narrow declaration next to the temporary service facets used by this file so
// frontend tests can mock only the capability under test.
const EditPreparationService = FileService as unknown as {
  PrepareEditSession: (fileId: string) => Promise<StagingStateData>;
  ReleaseCleanEditSession: (fileId: string) => Promise<StagingStateData>;
};

function fmtBytes(n: number): string {
  if (n < 1024) return `${n} B`;
  const units = ["KiB", "MiB", "GiB", "TiB"];
  let v = n / 1024;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v.toFixed(v < 10 ? 2 : 1)} ${units[i]}`;
}

function snippet(value: unknown): string {
  return String(value ?? "").replace(/\n/g, "⏎").slice(0, 120);
}

function searchInputLimitMessage(useRegex: boolean): string {
  return useRegex
    ? "Regular-expression search text is limited to 64 KiB of UTF-8."
    : "Search text is limited to 1 MiB of UTF-8.";
}

function App() {
  const hostRef = useRef<HTMLDivElement>(null);
  const editorRef = useRef<QuarryEditor | null>(null);
  const editorReadyRef = useRef<Promise<QuarryEditor> | null>(null);
  const [storage] = useState(() => browserLocalStorage());
  const [initialSettings] = useState(() => loadLocalSettings(storage));
  const [lazyPanelEpochs, retryLazyPanel] = useReducer(
    (current: LazyPanelEpochs, panel: LazyPanel): LazyPanelEpochs => ({
      ...current,
      [panel]: current[panel] + 1,
    }),
    initialLazyPanelEpochs,
  );
  // A new lazy component object owns a new loader promise. Merely remounting
  // the same React.lazy object cannot recover from a cached import rejection.
  const Tools = useMemo(() => lazy(loadTools), [lazyPanelEpochs.tools]);
  const CsvGrid = useMemo(() => lazy(loadCsvGrid), [lazyPanelEpochs.csv]);
  const HexView = useMemo(() => lazy(loadHexView), [lazyPanelEpochs.hex]);
  const DiffView = useMemo(() => lazy(loadDiffView), [lazyPanelEpochs.diff]);
  const Help = useMemo(() => lazy(loadHelp), [lazyPanelEpochs.help]);

  const [tabs, setTabs] = useState<Tab[]>([]);
  const [activeId, setActiveId] = useState<string | null>(null);
  const [status, setStatus] = useState<WindowStatus | null>(null);
  const [notificationState, dispatchNotification] = useReducer(notificationReducer, initialNotificationState);
  const [busy, setBusy] = useState(false);
  const [path, setPath] = useState("");
  const [pathInputInfo, setPathInputInfo] = useState("");

  const [searchOpen, setSearchOpen] = useState(false);
  const [gotoOpen, setGotoOpen] = useState(false);
  const [query, setQuery] = useState("");
  const [regex, setRegex] = useState(false);
  const [caseSensitive, setCaseSensitive] = useState(false);
  const [wholeWord, setWholeWord] = useState(false);
  const [searching, setSearching] = useState(false);
  const [activeSearchRequestId, setActiveSearchRequestId] = useState<string | null>(null);
  const [searchInfo, setSearchInfo] = useState("");
  const [results, setResults] = useState<{ offset: number; length: number; line: number; preview: string }[] | null>(null);
  const [resultsInfo, setResultsInfo] = useState("");
  const [gotoValue, setGotoValue] = useState("");
  const lastMatch = useRef<number | null>(null);

  const [editMode, setEditMode] = useState(false);
  const [tabEditStates, setTabEditStates] = useState<Record<string, TabEditState>>({});
  const [diffOpen, setDiffOpen] = useState(false);
  const [diffMode, setDiffMode] = useState<"list" | "side">("list");
  const [stagedEdits, setStagedEdits] = useState<StagedEditData[]>([]);
  const [toolsOpen, setToolsOpen] = useState(false);
  const [gridView, setGridView] = useState(false);
  const [hexView, setHexView] = useState(false);
  const [csvDialects, setCsvDialects] = useState<Record<string, CsvDialect>>({});
  // RefreshFile keeps the backend file ID stable while replacing every
  // generation-bound view of the source. Track that transition explicitly so
  // React panels cannot retain state merely because the file ID/type did not
  // change (for example, after a same-size external rewrite).
  const [sourceRefreshEpochs, setSourceRefreshEpochs] = useState<Record<string, number>>({});
  const [sidebarCollapsed, setSidebarCollapsed] = useState(false);
  const [paletteOpen, setPaletteOpen] = useState(false);
  const [helpOpen, setHelpOpen] = useState(false);
  const [analyses, setAnalyses] = useState<Record<string, NormalizedSqlSummaryResult>>({});
  const [xrayOn, setXrayOn] = useState(true);
  const [theme, setTheme] = useState<QuarryTheme>(initialSettings.theme);
  // Byte-offset gutter is opt-in: most users only need line numbers. "1" = shown.
  const [showByteOffsets, setShowByteOffsets] = useState<boolean>(initialSettings.showByteOffsets);
  const [workspacePreferences, setWorkspacePreferences] = useState<WorkspacePreferences>(initialSettings.workspace);
  const [recent, setRecent] = useState<string[]>(initialSettings.recentPaths);
  const restoredRef = useRef(false);
  const [bookmarks, setBookmarks] = useState<StoredBookmarks>(initialSettings.bookmarks);
  const [bookmarksOpen, setBookmarksOpen] = useState(false);
  const [following, setFollowing] = useState(false);
  // RefreshFile can replace the backend generation before the shared editor
  // has successfully loaded its new window. Keep that committed-but-unsynced
  // state outside the follow effect so turning follow off cannot discard it.
  const [pendingSourceSyncEpoch, bumpPendingSourceSyncEpoch] = useReducer((value: number) => value + 1, 0);
  const [job, setJob] = useState<ActiveJobState | null>(null);
  const [closeDecision, setCloseDecision] = useState<CloseDecisionContext | null>(null);
  const [closeReview, setCloseReview] = useState<CloseScope | null>(null);

  const activeIdRef = useRef<string | null>(null);
  const editorFileIdRef = useRef<string | null>(null);
  const tabsRef = useRef<Tab[]>([]);
  const tabEditStatesRef = useRef<Record<string, TabEditState>>({});
  const busyRef = useRef(false);
  const operationBusyGenerationRef = useRef(0);
  const editModeRef = useRef(false);
  const closeFlowRef = useRef(false);
  const tabTransitionRef = useRef(false);
  const diffOpenRef = useRef(false);
  const jobRef = useRef<ActiveJobState | null>(null);
  const jobEndWaitersRef = useRef(new Map<string, Set<() => void>>());
  const notificationRef = useRef(initialNotificationState);
  const notificationSequenceRef = useRef(0);
  const pendingJobOwnerRef = useRef<{ owner: NotificationOwner; timestamp: number } | null>(null);
  const jobNotificationOwnersRef = useRef(new Map<string, NotificationOwner>());
  const terminalOwnerByOperationRef = useRef(new Map<string, {
    owner: NotificationOwner;
    jobID: string;
    status: string;
  }>());
  const requestGateRef = useRef(new RequestGenerationGate());
  const resultsRequestRef = useRef<SearchResultsSession | null>(null);
  const activeSearchRef = useRef<ActiveInteractiveSearch | null>(null);
  const statusRef = useRef<WindowStatus | null>(null);
  const sourceRefreshEpochsRef = useRef<Record<string, number>>({});
  const pendingSourceRefreshMetaRef = useRef(new Map<string, FileMetaData>());
  const tabPositionsRef = useRef<Record<string, number>>({});
  const tabStatusesRef = useRef<Record<string, WindowStatus>>({});
  const preparedEditFilesRef = useRef<Set<string>>(new Set());
  const csvDialectsRef = useRef<Record<string, CsvDialect>>({});
  const previousSurfaceFileRef = useRef<string | null>(null);
  const searchConfigRef = useRef({ query, regex, caseSensitive, wholeWord });
  const closeDecisionResolverRef = useRef<((choice: CloseChoice) => void) | null>(null);
  const closeTabRef = useRef<(fileId: string) => Promise<void>>(async () => {});
  const requestApplicationCloseRef = useRef<() => Promise<void>>(async () => {});
  const pendingTabCloseFocusRef = useRef<PendingTabCloseFocus | null>(null);
  const pendingApplicationCloseFocusRef = useRef<HTMLElement | null>(null);
  const closeReviewDialogRef = useRef<HTMLElement | null>(null);
  const nativeDropQueueRef = useRef<Promise<void>>(Promise.resolve());
  const workspacePreferencesRef = useRef(workspacePreferences);
  const paletteReturnFocusRef = useRef<HTMLElement | null>(null);
  const helpReturnFocusRef = useRef<HTMLElement | null>(null);
  activeIdRef.current = activeId;
  tabsRef.current = tabs;
  busyRef.current = busy;
  editModeRef.current = editMode;
  diffOpenRef.current = diffOpen;
  jobRef.current = job;
  workspacePreferencesRef.current = workspacePreferences;
  searchConfigRef.current = { query, regex, caseSensitive, wholeWord };
  notificationRef.current = notificationState;

  const resolveModalInvoker = (): HTMLElement | null => {
    const active = document.activeElement;
    if (
      active instanceof HTMLElement
      && active !== document.body
      && !active.closest('[role="dialog"][aria-modal="true"]')
    ) {
      return active;
    }
    // When one modal launches another, preserve the original workbench
    // invoker instead of recording an element that is about to be detached.
    if (paletteReturnFocusRef.current?.isConnected) return paletteReturnFocusRef.current;
    if (helpReturnFocusRef.current?.isConnected) return helpReturnFocusRef.current;
    return null;
  };

  const openCommandPalette = () => {
    paletteReturnFocusRef.current = resolveModalInvoker();
    setPaletteOpen(true);
  };
  const closeCommandPalette = () => setPaletteOpen(false);
  const openHelp = () => {
    helpReturnFocusRef.current = resolveModalInvoker();
    setHelpOpen(true);
  };
  const closeHelp = () => setHelpOpen(false);

  useEffect(() => {
    if (paletteOpen) return;
    const previous = paletteReturnFocusRef.current;
    if (!previous) return;
    paletteReturnFocusRef.current = null;
    const restore = () => {
      if (previous.isConnected && document.activeElement === document.body) {
        previous.focus({ preventScroll: true });
      }
    };
    restore();
    queueMicrotask(restore);
  }, [paletteOpen]);

  useEffect(() => {
    if (helpOpen) return;
    const previous = helpReturnFocusRef.current;
    if (!previous) return;
    helpReturnFocusRef.current = null;
    const restore = () => {
      if (previous.isConnected && document.activeElement === document.body) {
        previous.focus({ preventScroll: true });
      }
    };
    restore();
    queueMicrotask(restore);
  }, [helpOpen]);

  useLayoutEffect(() => {
    if (closeReview !== null && closeDecision === null) {
      closeReviewDialogRef.current?.focus({ preventScroll: true });
      return;
    }
    if (closeReview !== null || closeDecision !== null) return;
    const applicationInvoker = pendingApplicationCloseFocusRef.current;
    if (applicationInvoker) {
      pendingApplicationCloseFocusRef.current = null;
      if (applicationInvoker.isConnected) applicationInvoker.focus({ preventScroll: true });
      return;
    }
    const pending = pendingTabCloseFocusRef.current;
    if (!pending) return;
    pendingTabCloseFocusRef.current = null;

    const app = (hostRef.current?.closest(".q-app") as HTMLElement | null) ?? document.body;
    const closeButtonFor = (fileId: string) => Array.from(
      app.querySelectorAll<HTMLButtonElement>("button.q-sb-close[data-file-id]"),
    ).find((button) => button.dataset.fileId === fileId);

    if (pending.outcome === "retained") {
      const target = pending.invoker.isConnected ? pending.invoker : closeButtonFor(pending.fileId);
      target?.focus({ preventScroll: true });
      return;
    }

    if (pending.replacementFileId) {
      const replacement = closeButtonFor(pending.replacementFileId);
      if (replacement) {
        replacement.focus({ preventScroll: true });
        return;
      }
    }

    if (tabs.length > 0) {
      const editorTarget = hostRef.current?.querySelector<HTMLElement>('.cm-content[contenteditable="true"]')
        ?? hostRef.current;
      editorTarget?.focus({ preventScroll: true });
      return;
    }
    app.querySelector<HTMLButtonElement>("button.q-empty-open")?.focus({ preventScroll: true });
  }, [closeDecision, closeReview, sidebarCollapsed, tabs]);

  const isActiveRequest = (request: ActiveRequestToken): boolean =>
    requestGateRef.current.isActive(request, activeIdRef.current, editorFileIdRef.current);

  const isActiveContext = (fileId: string, activeEpoch: number): boolean =>
    requestGateRef.current.isActiveContext(
      { fileId, activeEpoch },
      activeIdRef.current,
      editorFileIdRef.current,
    );

  const isOpenSessionResponse = (request: FileRequestToken): boolean =>
    requestGateRef.current.isLatest(request)
    && tabsRef.current.some((tab) => tab.fileId === request.fileId);

  const recordEditorStatus = (next: WindowStatus | null) => {
    if (!next) {
      statusRef.current = null;
      setStatus(null);
      return;
    }
    if (!tabsRef.current.some((tab) => tab.fileId === next.fileId)) return;
    const position = Number.isSafeInteger(next.positionByte) && next.positionByte >= 0
      ? next.positionByte
      : next.startByte;
    const ownedStatus = position === next.positionByte ? next : { ...next, positionByte: position };
    tabPositionsRef.current[next.fileId] = position;
    tabStatusesRef.current[next.fileId] = ownedStatus;
    if (activeIdRef.current === next.fileId && editorFileIdRef.current === next.fileId) {
      statusRef.current = ownedStatus;
      setStatus(ownedStatus);
    }
  };

  const persistOwnedPosition = () => {
    const owner = editorFileIdRef.current;
    const current = statusRef.current;
    if (owner && current?.fileId === owner) {
      tabPositionsRef.current[owner] = current.positionByte;
    }
  };

  const searchContextFor = (fileId: string): SearchContext => ({
    fileId,
    query: searchConfigRef.current.query,
    regex: searchConfigRef.current.regex,
    caseSensitive: searchConfigRef.current.caseSensitive,
    wholeWord: searchConfigRef.current.wholeWord,
  });

  const sameSearchContext = (left: SearchContext, right: SearchContext): boolean =>
    left.fileId === right.fileId
    && left.query === right.query
    && left.regex === right.regex
    && left.caseSensitive === right.caseSensitive
    && left.wholeWord === right.wholeWord;

  const isCurrentSearchContext = (context: SearchContext): boolean =>
    activeIdRef.current === context.fileId
    && editorFileIdRef.current === context.fileId
    && sameSearchContext(context, searchContextFor(context.fileId));

  const isCurrentResultsSession = (session: SearchResultsSession): boolean =>
    resultsRequestRef.current === session
    && isActiveRequest(session.request)
    && isCurrentSearchContext(session.context);

  const currentSearchOrigin = (fileId: string, fallback: number): number => {
    const current = statusRef.current;
    if (current?.fileId === fileId && editorFileIdRef.current === fileId) return current.positionByte;
    return tabPositionsRef.current[fileId] ?? fallback;
  };

  const applyNotificationAction = (action: NotificationAction): boolean => {
    const previous = notificationRef.current;
    const next = notificationReducer(previous, action);
    if (next === previous) return false;
    notificationRef.current = next;
    dispatchNotification(action);
    return true;
  };

  const beginOperation = (
    operation: string,
    options: { fileId?: string; path?: string; jobCandidate?: boolean } = {},
  ): NotificationOwner => {
    const sequence = ++notificationSequenceRef.current;
    const owner: NotificationOwner = {
      operationId: `ui-${sequence}`,
      sequence,
      operation,
      ...(options.fileId ? { fileId: options.fileId } : {}),
      ...(options.path ? { path: options.path } : {}),
    };
    applyNotificationAction({ type: "reserve", owner });
    if (options.jobCandidate) pendingJobOwnerRef.current = { owner, timestamp: Date.now() };
    else {
      const pending = pendingJobOwnerRef.current;
      if (pending && pending.owner.sequence < owner.sequence) pendingJobOwnerRef.current = null;
    }
    return owner;
  };

  const finishOperation = (owner: NotificationOwner) => {
    if (pendingJobOwnerRef.current?.owner.operationId === owner.operationId) {
      pendingJobOwnerRef.current = null;
    }
    terminalOwnerByOperationRef.current.delete(owner.operationId);
  };

  const publishOperation = (owner: NotificationOwner, input: NotificationInput): boolean => {
    if (
      owner.fileId
      && (activeIdRef.current !== owner.fileId || editorFileIdRef.current !== owner.fileId)
    ) return false;
    const terminal = terminalOwnerByOperationRef.current.get(owner.operationId);
    let publishOwner = owner;
    let publishInput = input;
    if (terminal) {
      if (terminal.status === "cancelled" && input.severity === "error") return false;
      if (terminal.status !== "completed" && input.severity === "success") return false;
      publishOwner = terminal.owner;
      const inputMessage = String(input.message ?? "").slice(0, 600);
      const inputDetails = input.details == null ? "" : String(input.details).slice(0, 4_096);
      const jobDetails = `Job ID: ${terminal.jobID}\nStatus: ${terminal.status}`;
      publishInput = input.severity === "success"
        ? {
            ...input,
            message: `${publishOwner.operation} completed — ${inputMessage}`,
            details: [jobDetails, inputDetails].filter(Boolean).join("\n"),
          }
        : {
            ...input,
            message: `${publishOwner.operation} failed`,
            details: [jobDetails, inputMessage, inputDetails].filter(Boolean).join("\n"),
          };
    }
    const accepted = applyNotificationAction({ type: "publish", owner: publishOwner, input: publishInput });
    const published = notificationRef.current.current;
    if (accepted && published?.operationId === publishOwner.operationId && published.severity === "error") {
      console.error("Quarry operation failed", {
        operation: published.operation,
        operationId: published.operationId,
        sequence: published.sequence,
        fileId: published.fileId ?? "",
        message: published.message,
        details: published.details ?? "",
      });
    }
    return accepted;
  };

  const notifyInstant = (
    operation: string,
    input: NotificationInput,
    options: { fileId?: string; path?: string } = {},
  ): NotificationOwner => {
    const owner = beginOperation(operation, options);
    publishOperation(owner, input);
    return owner;
  };

  useEffect(() => {
    const current = notificationState.current;
    if (current?.expiresAt == null) return;
    const delay = Math.max(0, current.expiresAt - Date.now());
    const timer = window.setTimeout(() => {
      applyNotificationAction({
        type: "expire",
        operationId: current.operationId,
        sequence: current.sequence,
        now: Date.now(),
      });
    }, delay);
    return () => window.clearTimeout(timer);
    // The action helper is ref-backed; current identity owns this timer.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [notificationState.current]);

  const copyNotificationDetails = async () => {
    const current = notificationRef.current.current;
    if (!current) return;
    try {
      await navigator.clipboard.writeText(notificationDetailsText(current));
    } catch (cause: unknown) {
      const message = cause instanceof Error ? cause.message : String(cause);
      console.error("Unable to copy bounded notification details", message.slice(0, 600));
    }
  };

  const cancelSearchRequest = (requestId: string) => {
    void FileService.CancelSearch(requestId).catch((cause: unknown) => {
      const message = cause instanceof Error ? cause.message : String(cause);
      console.error("Unable to cancel interactive search", message.slice(0, 600));
    });
  };

  const cancelBackendSearch = (): boolean => {
    const active = activeSearchRef.current;
    if (!active) return false;
    activeSearchRef.current = null;
    setActiveSearchRequestId(null);
    cancelSearchRequest(active.requestId);
    return true;
  };

  const invalidateSearch = () => {
    cancelBackendSearch();
    requestGateRef.current.invalidateChannel("search");
    resultsRequestRef.current = null;
    lastMatch.current = null;
    setSearching(false);
    setSearchInfo("");
    setResults(null);
    setResultsInfo("");
  };

  const updateSearchQuery = (value: string) => {
    const bounded = boundedUtf8Text(
      value,
      regex ? SEARCH_REGEX_INPUT_MAX_BYTES : SEARCH_PLAIN_INPUT_MAX_BYTES,
    );
    invalidateSearch();
    setQuery(bounded.value);
    if (bounded.truncated) setSearchInfo(searchInputLimitMessage(regex));
  };

  const toggleSearchRegex = () => {
    const nextRegex = !regex;
    const bounded = boundedUtf8Text(
      query,
      nextRegex ? SEARCH_REGEX_INPUT_MAX_BYTES : SEARCH_PLAIN_INPUT_MAX_BYTES,
    );
    invalidateSearch();
    setRegex(nextRegex);
    if (bounded.value !== query) setQuery(bounded.value);
    if (bounded.truncated) setSearchInfo(searchInputLimitMessage(nextRegex));
  };

  const invalidateActiveRequests = (clearDiff = true) => {
    cancelBackendSearch();
    requestGateRef.current.invalidateActive();
    resultsRequestRef.current = null;
    lastMatch.current = null;
    setSearching(false);
    setSearchInfo("");
    setResults(null);
    setResultsInfo("");
    if (clearDiff) setStagedEdits([]);
  };

  const invalidateRefreshedSource = (fileId: string) => {
    // If this is still the attached file, prevent any request started against
    // the pre-refresh backend generation from committing after RefreshFile.
    // A tab switch already invalidates that ownership, so do not disturb work
    // for a different active file when a late refresh finishes.
    if (activeIdRef.current === fileId && editorFileIdRef.current === fileId) {
      invalidateActiveRequests();
    }

    setAnalyses((previous) => {
      if (!(fileId in previous)) return previous;
      const next = { ...previous };
      delete next[fileId];
      return next;
    });

    // Detector-owned CSV configuration belongs to the replaced generation.
    // Preserve only an explicit user override; otherwise the remounted grid
    // and Tools panel must inspect the refreshed source again.
    if (csvDialectsRef.current[fileId]?.origin === "detected") {
      const nextDialects = { ...csvDialectsRef.current };
      delete nextDialects[fileId];
      csvDialectsRef.current = nextDialects;
      setCsvDialects(nextDialects);
    }

    const nextEpoch = (sourceRefreshEpochsRef.current[fileId] ?? 0) + 1;
    const nextEpochs = { ...sourceRefreshEpochsRef.current, [fileId]: nextEpoch };
    sourceRefreshEpochsRef.current = nextEpochs;
    setSourceRefreshEpochs(nextEpochs);
  };

  const rememberPendingSourceRefresh = (meta: FileMetaData) => {
    pendingSourceRefreshMetaRef.current.set(meta.fileId, meta);
    bumpPendingSourceSyncEpoch();
  };

  const completePendingSourceRefresh = (fileId: string, expected: FileMetaData) => {
    // A late renderer completion must not clear a newer committed replacement
    // for the same stable file ID.
    if (pendingSourceRefreshMetaRef.current.get(fileId) !== expected) return;
    pendingSourceRefreshMetaRef.current.delete(fileId);
    bumpPendingSourceSyncEpoch();
  };

  const setOperationBusy = (value: boolean): number => {
    // Close requests arrive through a native event and can race React's next
    // render. Keep the synchronous ref authoritative as soon as an operation
    // starts, including while a native file dialog is opening.
    const generation = ++operationBusyGenerationRef.current;
    busyRef.current = value;
    setBusy(value);
    return generation;
  };

  const releaseOwnedOperationBusy = (generation: number) => {
    // A stale background continuation must never clear a later operation's
    // reservation. Ordinary UI operations remain serialized by busyRef; this
    // token is for background follow work that began between two awaits.
    if (operationBusyGenerationRef.current === generation) setOperationBusy(false);
  };

  const cancelJob = useCallback(async (jobID: string) => {
    const current = jobRef.current;
    if (!current || current.id !== jobID) return;
    const belongsToActiveFile = !current.fileId
      || (activeIdRef.current === current.fileId && editorFileIdRef.current === current.fileId);
    let owner = jobNotificationOwnersRef.current.get(jobID);
    if (!owner && belongsToActiveFile) {
      const path = tabsRef.current.find((tab) => tab.fileId === current.fileId)?.meta.path;
      owner = beginOperation(`Cancel ${current.title}`, { fileId: current.fileId, path });
      jobNotificationOwnersRef.current.set(jobID, owner);
    }
    if (owner) {
      publishOperation(owner, {
        severity: "info",
        message: `Cancellation requested for ${current.title}`,
        details: `Job ID: ${jobID}`,
      });
    }
    try {
      await FileService.CancelJob(jobID);
    } catch (e: any) {
      // The backend rejects stale IDs and jobs that have already committed.
      // Keep a delayed click from affecting any newer job now in the ref.
      if (owner && jobRef.current?.id === jobID) {
        publishOperation(owner, {
          severity: "warning",
          message: "The job could not be cancelled because it is already finishing",
          details: `Job ID: ${jobID}\n${String(e?.message ?? e)}`,
        });
      }
    }
  }, []);

  const waitForJobEnd = useCallback((jobID: string): Promise<void> => {
    if (jobRef.current?.id !== jobID) return Promise.resolve();
    return new Promise<void>((resolve) => {
      const waiters = jobEndWaitersRef.current.get(jobID) ?? new Set<() => void>();
      waiters.add(resolve);
      jobEndWaitersRef.current.set(jobID, waiters);
      // Events execute serially in the browser, but recheck after registration
      // so a React state flush cannot leave a waiter behind for an ended job.
      if (jobRef.current?.id !== jobID) {
        waiters.delete(resolve);
        if (waiters.size === 0) jobEndWaitersRef.current.delete(jobID);
        resolve();
      }
    });
  }, []);

  const cancelJobAndWait = useCallback(async (jobID: string) => {
    const ended = waitForJobEnd(jobID);
    await cancelJob(jobID);
    await ended;
  }, [cancelJob, waitForJobEnd]);

  const updateTabEditState = (fileId: string, patch: Partial<TabEditState>) => {
    if (!fileId) return;
    const previous = tabEditStatesRef.current[fileId] ?? { dirty: false, staging: null };
    const next = {
      ...tabEditStatesRef.current,
      [fileId]: { ...previous, ...patch },
    };
    tabEditStatesRef.current = next;
    setTabEditStates(next);
  };

  const forgetTabEditState = (fileId: string) => {
    requestGateRef.current.forgetFile("staging", fileId);
    if (!(fileId in tabEditStatesRef.current)) return;
    const next = { ...tabEditStatesRef.current };
    delete next[fileId];
    tabEditStatesRef.current = next;
    setTabEditStates(next);
  };

  const updateCsvDialect = (fileId: string, proposed: CsvDialect) => {
    if (!fileId) return;
    const current = csvDialectsRef.current[fileId];
    const effective = proposed.origin === "detected"
      ? mergeCsvDetection(current, proposed)
      : proposed;
    if (
      current?.delimiter === effective.delimiter
      && current.hasHeader === effective.hasHeader
      && current.origin === effective.origin
    ) return;
    const next = { ...csvDialectsRef.current, [fileId]: effective };
    csvDialectsRef.current = next;
    setCsvDialects(next);
    if (activeIdRef.current === fileId && editorFileIdRef.current === fileId) {
      editorRef.current?.setCsvDelimiter?.(fileId, effective.delimiter);
    }
  };

  const forgetCsvDialect = (fileId: string) => {
    if (!(fileId in csvDialectsRef.current)) return;
    const next = { ...csvDialectsRef.current };
    delete next[fileId];
    csvDialectsRef.current = next;
    setCsvDialects(next);
  };

  useEffect(() => {
    if (!hostRef.current) return;
    let cancelled = false;
    let ed: QuarryEditor | null = null;
    const initialTheme = initialSettings.theme;
    const initialBytes = initialSettings.showByteOffsets;
    const host = hostRef.current;
    const ready = import("./editor/QuarryEditor").then(({ QuarryEditor: Editor }) => {
      if (cancelled) throw new Error("editor initialization was cancelled");
      const instance = new Editor(host, {
        onStatus: recordEditorStatus,
        onDirty: (value) => {
          const fileId = editorFileIdRef.current;
          if (fileId) updateTabEditState(fileId, { dirty: value });
        },
        onStaging: (value) => {
          const fileId = editorFileIdRef.current;
          if (fileId) updateTabEditState(fileId, { staging: value });
        },
        onMode: (on) => {
          editModeRef.current = on;
          setEditMode(on);
        },
        onError: (value) => {
          const fileId = editorFileIdRef.current ?? undefined;
          const activeTab = tabsRef.current.find((tab) => tab.fileId === fileId);
          notifyInstant("Editor", {
            severity: "error",
            message: String((value as { message?: unknown })?.message ?? value),
          }, { fileId, path: activeTab?.meta.path });
        },
      }, initialTheme, initialBytes);
      ed = instance;
      editorRef.current = instance;
      return instance;
    });
    editorReadyRef.current = ready;
    void ready.catch((cause: unknown) => {
      if (!cancelled) notifyInstant("Initialize editor", {
        severity: "error",
        message: "Unable to initialise the editor",
        details: cause instanceof Error ? cause.message : String(cause),
      });
    });
    const blockEditorInputDuringClose = (e: Event) => {
      if (!closeFlowRef.current) return;
      const dialog = document.querySelector(".q-close-dialog");
      if (dialog && e.target instanceof Node && dialog.contains(e.target)) return;
      e.preventDefault();
      e.stopImmediatePropagation();
    };
    const onKey = (e: KeyboardEvent) => {
      const mod = e.ctrlKey || e.metaKey;
      if (closeFlowRef.current && (mod || e.key === "F1")) {
        e.preventDefault();
        return;
      }
      if (mod && e.key.toLowerCase() === "o") {
        e.preventDefault();
        void openViaDialog();
      } else if (mod && e.key.toLowerCase() === "f") {
        e.preventDefault();
        if (!activeIdRef.current) return;
        requestGateRef.current.invalidateChannel("goto");
        setGotoOpen(false);
        setSearchOpen(true);
      } else if (mod && e.key.toLowerCase() === "g") {
        e.preventDefault();
        if (!activeIdRef.current) return;
        invalidateSearch();
        requestGateRef.current.invalidateChannel("goto");
        setGotoOpen(true);
      } else if (mod && e.key.toLowerCase() === "b") {
        e.preventDefault();
        setSidebarCollapsed((v) => !v);
      } else if (mod && e.key.toLowerCase() === "w") {
        if (activeIdRef.current) {
          e.preventDefault();
          void closeTabRef.current(activeIdRef.current);
        }
      } else if (mod && e.key.toLowerCase() === "p") {
        e.preventDefault();
        openCommandPalette();
      } else if (e.key === "F1") {
        e.preventDefault();
        openHelp();
      } else if (e.key === "Escape") {
        invalidateSearch();
        requestGateRef.current.invalidateChannel("goto");
        setSearchOpen(false);
        setGotoOpen(false);
        closeCommandPalette();
        closeHelp();
      }
    };
    window.addEventListener("keydown", blockEditorInputDuringClose, true);
    window.addEventListener("beforeinput", blockEditorInputDuringClose, true);
    window.addEventListener("keydown", onKey);
    return () => {
      cancelled = true;
      window.removeEventListener("keydown", blockEditorInputDuringClose, true);
      window.removeEventListener("beforeinput", blockEditorInputDuringClose, true);
      window.removeEventListener("keydown", onKey);
      ed?.destroy();
      editorRef.current = null;
      if (editorReadyRef.current === ready) editorReadyRef.current = null;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  // Apply the theme to the document root + editor whenever it changes.
  useEffect(() => {
    document.documentElement.dataset.theme = theme;
    saveTheme(storage, theme);
    editorRef.current?.setTheme(theme);
  }, [storage, theme]);

  const toggleTheme = () => setTheme((t) => (t === "light" ? "dark" : "light"));

  const showAbout = useCallback(async () => {
    const owner = beginOperation("Read build information");
    try {
      publishOperation(owner, { severity: "info", message: formatBuildInfo(await FileService.GetBuildInfo()) });
    } catch (cause: unknown) {
      const message = cause instanceof Error ? cause.message : String(cause);
      publishOperation(owner, {
        severity: "error",
        message: "Unable to read build information",
        details: message,
      });
    } finally {
      finishOperation(owner);
    }
  }, []);

  // Show/hide the per-line byte-offset gutter (persisted).
  useEffect(() => {
    saveByteOffsets(storage, showByteOffsets);
    editorRef.current?.setShowByteOffsets(showByteOffsets);
  }, [showByteOffsets, storage]);

  // Tail/follow uses a non-overlapping poll. Size, mtime, and retained-handle
  // identity detect append, truncate, and rename/recreate rotation. State is
  // advanced only after a successful refresh so transient failures retry.
  useEffect(() => {
    if (!following) return;
    const id = activeId;
    if (!id) return;
    let lastState: FileStateResult | null = null;
    let stop = false;
    let timer: number | null = null;
    let degraded = false;
    let owner: NotificationOwner | null = null;
    const nextOwner = () => beginOperation("Follow file", {
      fileId: id,
      path: tabsRef.current.find((tab) => tab.fileId === id)?.meta.path,
    });
    const schedule = () => {
      if (!stop) timer = window.setTimeout(() => void tick(), 1500);
    };
    const tick = async () => {
      if (stop) return;
      if (editModeRef.current || busyRef.current || closeFlowRef.current || tabTransitionRef.current) {
        schedule();
        return;
      }
      try {
        const state = await FileStateService.FileState(id);
        const tabSize = tabsRef.current.find((t) => t.fileId === id)?.meta.size ?? 0;
        const changed = pendingSourceRefreshMetaRef.current.has(id)
          || !state.sameOpenedFile
          || state.changedFromOpen
          || state.size !== (lastState?.size ?? tabSize)
          || (lastState != null && state.modTimeNanos !== lastState.modTimeNanos);
        if (changed) {
          // FileState awaited outside the global busy state. Recheck every
          // transition guard, then reserve synchronously before RefreshFile so
          // edit/tab/close operations cannot overlap the generation commit.
          if (
            stop
            || editModeRef.current
            || busyRef.current
            || closeFlowRef.current
            || tabTransitionRef.current
            || activeIdRef.current !== id
            || editorFileIdRef.current !== id
          ) return;
          const busyGeneration = setOperationBusy(true);
          try {
            let m = pendingSourceRefreshMetaRef.current.get(id) ?? null;
            if (m == null) {
              m = (await FileService.RefreshFile(id)) as FileMetaData;
              // A fulfilled RefreshFile is authoritative even when it reports
              // a bounded warning about closing the replaced session.
              // Invalidate generation-owned results before metadata,
              // navigation, or notice work so no old completion can commit.
              invalidateRefreshedSource(id);
              preparedEditFilesRef.current.delete(id);
              if (m.fileId !== id) {
                throw new Error(`refresh response file mismatch: expected ${id}, received ${m.fileId || "empty file ID"}`);
              }
              rememberPendingSourceRefresh(m);
              const currentTabs = tabsRef.current;
              if (currentTabs.some((tab) => tab.fileId === id)) {
                const nextTabs = currentTabs.map((tab) => (tab.fileId === id ? { ...tab, meta: m! } : tab));
                tabsRef.current = nextTabs;
                setTabs(nextTabs);
              }
              const refreshWarning = typeof m.refreshWarning === "string" ? m.refreshWarning.trim() : "";
              if (refreshWarning) {
                const warningOwner = nextOwner();
                publishOperation(warningOwner, {
                  severity: "warning",
                  message: "File refreshed with a session-cleanup warning",
                  details: refreshWarning,
                });
                finishOperation(warningOwner);
              }
            }
            const editor = editorRef.current;
            if (!editor || editModeRef.current || activeIdRef.current !== id || editorFileIdRef.current !== id) {
              throw new Error("refreshed source no longer owns the attached read-only editor");
            }
            await editor.refreshSource(m, csvDialectsRef.current[id]?.delimiter ?? "");
            completePendingSourceRefresh(id, m);
            if (stop) return;
            lastState = await FileStateService.FileState(id);
          } finally {
            releaseOwnedOperationBusy(busyGeneration);
          }

        } else {
          lastState = state;
        }
        if (degraded) {
          degraded = false;
          owner = nextOwner();
          publishOperation(owner, { severity: "success", message: "Live follow resumed" });
        }
      } catch (e: any) {
        if (!degraded) owner = nextOwner();
        degraded = true;
        if (!stop && owner) publishOperation(owner, {
          severity: "warning",
          message: "Live follow could not refresh; retrying",
          details: String(e?.message ?? e),
        });
      } finally {
        schedule();
      }
    };
    timer = window.setTimeout(() => void tick(), 0);
    return () => {
      stop = true;
      if (timer != null) window.clearTimeout(timer);
      if (owner) finishOperation(owner);
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [following, activeId]);

  // Stopping follow must not strand an editor on generation N after
  // RefreshFile already committed generation N+1. This small recovery loop is
  // independent of follow polling: it retries only the renderer attachment,
  // never RefreshFile, and retains ownership until the authoritative metadata
  // and tail window are paired again.
  useEffect(() => {
    if (following || !activeId || !pendingSourceRefreshMetaRef.current.has(activeId)) return;
    const id = activeId;
    let stop = false;
    let timer: number | null = null;
    let owner: NotificationOwner | null = null;
    const schedule = () => {
      if (!stop) timer = window.setTimeout(() => void synchronize(), 1500);
    };
    const synchronize = async () => {
      if (stop) return;
      const meta = pendingSourceRefreshMetaRef.current.get(id);
      if (!meta) return;
      if (
        busyRef.current
        || editModeRef.current
        || closeFlowRef.current
        || tabTransitionRef.current
        || activeIdRef.current !== id
        || editorFileIdRef.current !== id
      ) {
        schedule();
        return;
      }

      const busyGeneration = setOperationBusy(true);
      try {
        const editor = editorRef.current;
        if (!editor) throw new Error("the refreshed file no longer owns an editor");
        await editor.refreshSource(meta, csvDialectsRef.current[id]?.delimiter ?? "");
        completePendingSourceRefresh(id, meta);
        owner ??= beginOperation("Synchronize refreshed file", { fileId: id, path: meta.path });
        publishOperation(owner, {
          severity: "success",
          message: "Refreshed file view synchronized",
        });
        finishOperation(owner);
        owner = null;
      } catch (e: any) {
        if (!stop && pendingSourceRefreshMetaRef.current.get(id) === meta) {
          owner ??= beginOperation("Synchronize refreshed file", { fileId: id, path: meta.path });
          publishOperation(owner, {
            severity: "warning",
            message: "The refreshed file view is still synchronizing; retrying",
            details: String(e?.message ?? e),
          });
        }
      } finally {
        releaseOwnedOperationBusy(busyGeneration);
        if (pendingSourceRefreshMetaRef.current.has(id)) schedule();
      }
    };
    timer = window.setTimeout(() => void synchronize(), 0);
    return () => {
      stop = true;
      if (timer != null) window.clearTimeout(timer);
      if (owner) finishOperation(owner);
    };
    // The epoch makes a committed refresh that lands after follow cleanup
    // start this recovery worker without coupling it back into the poller.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [following, activeId, pendingSourceSyncEpoch]);

  // Following reloads from disk, so turn it off as soon as the user starts editing.
  useEffect(() => { if (editMode && following) setFollowing(false); }, [editMode]); // eslint-disable-line react-hooks/exhaustive-deps

  // Open a file by path (used by drag-drop and session restore).
  const openFilePath = async (p: string) => {
    if (!isBoundedSourcePathInput(p)) return;
    if (busyRef.current || closeFlowRef.current) return;
    const owner = beginOperation("Open file", { path: p });
    setOperationBusy(true);
    try {
      const meta = (await FileService.OpenFile(p)) as FileMetaData;
      await showMeta(meta);
    } catch (e: any) {
      publishOperation(owner, { severity: "error", message: "Unable to open file", details: String(e?.message ?? e) });
    } finally {
      finishOperation(owner);
      setOperationBusy(false);
    }
  };
  const openFileRef = useRef(openFilePath);
  openFileRef.current = openFilePath;

  // Native file drops (Wails) → open each dropped path.
  useEffect(() => {
    const off = Events.On("quarry:files-dropped", (e: any) => {
      // Apply the absolute product bound synchronously so a renderer-side
      // event cannot enqueue an unbounded array while earlier drops are open.
      const bounded = normalizeNativeDrop(e?.data, MAX_OPEN_FILE_SESSIONS);
      nativeDropQueueRef.current = nativeDropQueueRef.current.then(async () => {
        const remainingSlots = Math.max(0, MAX_OPEN_FILE_SESSIONS - tabsRef.current.length);
        const paths = bounded.paths.slice(0, remainingSlots);
        let omitted = Math.min(
          Number.MAX_SAFE_INTEGER,
          bounded.omitted + bounded.paths.length - paths.length,
        );
        if (closeFlowRef.current || tabTransitionRef.current || busyRef.current) {
          omitted = Math.min(Number.MAX_SAFE_INTEGER, omitted + paths.length);
        } else {
          for (const file of paths) await openFileRef.current(file);
        }
        if (omitted > 0) {
          notifyInstant("Open dropped files", {
            severity: "warning",
            message: `${omitted} dropped ${omitted === 1 ? "file was" : "files were"} omitted`,
            details: `Quarry accepts at most ${MAX_OPEN_FILE_SESSIONS} open files. Invalid entries, excess paths, and drops received while another transition is active are omitted.`,
          });
        }
      });
    });
    return () => { try { off(); } catch { /* ignore */ } };
  }, []);

  // The native Wails hooks have already cancelled the close by the time this
  // event arrives. The frontend must explicitly approve or cancel that one
  // pending request after resolving every dirty/staged tab.
  useEffect(() => {
    const off = Events.On("quarry:close-requested", () => {
      void requestApplicationCloseRef.current();
    });
    return () => { try { off(); } catch { /* ignore */ } };
  }, []);

  // Authoritative backend job lifecycle (progress toast + ID-scoped terminal notification).
  useEffect(() => {
    const apply = (type: JobEventType, payload: Record<string, unknown>) => {
      const current = jobRef.current;
      const next = reduceJobEvent(current, type, payload);
      if (next === current) return { accepted: false, current, next };
      jobRef.current = next;
      setJob(next);
      return { accepted: true, current, next };
    };
    const offStart = Events.On("quarry:job-start", (e: any) => {
      const payload = (e?.data ?? {}) as Record<string, unknown>;
      const applied = apply("start", payload);
      const next = applied.next;
      if (!applied.accepted || !next || applied.current?.id === next.id) return;

      const pending = pendingJobOwnerRef.current;
      const pendingIsFresh = pending != null
        && Date.now() - pending.timestamp <= 2_500
        && (!pending.owner.fileId || pending.owner.fileId === next.fileId);
      const path = tabsRef.current.find((tab) => tab.fileId === next.fileId)?.meta.path;
      const belongsToActiveFile = !next.fileId
        || (activeIdRef.current === next.fileId && editorFileIdRef.current === next.fileId);
      const owner = pendingIsFresh
        ? pending!.owner
        : belongsToActiveFile
          ? beginOperation(next.title, { fileId: next.fileId || undefined, path })
          : null;
      pendingJobOwnerRef.current = null;
      jobNotificationOwnersRef.current.clear();
      terminalOwnerByOperationRef.current.clear();
      if (owner) {
        jobNotificationOwnersRef.current.set(next.id, owner);
        publishOperation(owner, {
          severity: "info",
          message: `${next.title} started`,
          details: `Job ID: ${next.id}`,
        });
      }
    });
    const offProg = Events.On("quarry:job-progress", (e: any) => {
      apply("progress", e?.data ?? {});
    });
    const offEnd = Events.On("quarry:job-end", (e: any) => {
      const payload = (e?.data ?? {}) as Record<string, unknown>;
      const jobID = typeof payload.id === "string" ? payload.id.trim() : "";
      const before = jobRef.current;
      const applied = apply("end", payload);
      if (jobID && before?.id === jobID && applied.accepted && applied.next === null) {
        const startedOwner = jobNotificationOwnersRef.current.get(jobID);
        if (startedOwner) finishOperation(startedOwner);
        const status = typeof payload.status === "string" ? payload.status : "failed";
        const belongsToActiveFile = !before.fileId
          || (activeIdRef.current === before.fileId && editorFileIdRef.current === before.fileId);
        if (belongsToActiveFile) {
          const severity = status === "completed" ? "success" : status === "cancelled" ? "info" : "error";
          const terminal = status === "completed" ? "completed" : status === "cancelled" ? "cancelled" : "failed";
          const note = typeof payload.note === "string" ? payload.note : before.note;
          const terminalOwner = beginOperation(before.title, {
            fileId: before.fileId || undefined,
            path: tabsRef.current.find((tab) => tab.fileId === before.fileId)?.meta.path,
          });
          publishOperation(terminalOwner, {
            severity,
            message: `${before.title} ${terminal}`,
            details: [`Job ID: ${jobID}`, `Status: ${status}`, note ? `Note: ${note}` : ""].filter(Boolean).join("\n"),
          });
          if (startedOwner) {
            terminalOwnerByOperationRef.current.set(startedOwner.operationId, {
              owner: terminalOwner,
              jobID,
              status,
            });
          }
          finishOperation(terminalOwner);
        }
        jobNotificationOwnersRef.current.delete(jobID);
      }
      const waiters = jobEndWaitersRef.current.get(jobID);
      const terminalOwnershipAccepted = before?.id !== jobID || (applied.accepted && applied.next === null);
      if (waiters && terminalOwnershipAccepted) {
        jobEndWaitersRef.current.delete(jobID);
        for (const resolve of waiters) resolve();
      }
    });
    return () => { try { offStart(); offProg(); offEnd(); } catch { /* ignore */ } };
  }, []);

  const pushRecent = (p: string) => {
    if (!p) return;
    setRecent((prev) => {
      const next = nextRecentPaths(prev, p);
      persistRecentPaths(storage, workspacePreferencesRef.current, next);
      return next;
    });
  };

  const saveBookmarks = (next: StoredBookmarks) => {
    const normalized = normalizeBookmarks(next);
    setBookmarks(normalized);
    persistBookmarks(storage, workspacePreferencesRef.current, normalized);
  };
  const addBookmark = () => {
    const t = tabsRef.current.find((x) => x.fileId === activeIdRef.current);
    if (!t) return;
    const offset = status?.positionByte ?? status?.startByte ?? 0;
    const label = `line ~${status?.firstLine ?? "?"}`;
    const list = bookmarks[t.meta.path] ?? [];
    if (list.some((b) => b.offset === offset)) { setBookmarksOpen(true); return; }
    const next = { ...bookmarks, [t.meta.path]: [...list, { offset, label }].sort((a, b) => a.offset - b.offset) };
    saveBookmarks(next);
    setBookmarksOpen(true);
    notifyInstant("Add bookmark", {
      severity: "success",
      message: `Bookmarked 0x${offset.toString(16)}`,
    }, { fileId: t.fileId, path: t.meta.path });
  };
  const removeBookmark = (path: string, offset: number) => {
    const list = (bookmarks[path] ?? []).filter((b) => b.offset !== offset);
    const next = { ...bookmarks };
    if (list.length) next[path] = list; else delete next[path];
    saveBookmarks(next);
  };

  // Persist only an explicitly enabled, bounded open-file set.
  useEffect(() => {
    if (!restoredRef.current) return; // don't clobber saved session before restore
    const paths = tabs.map((t) => t.meta.path).filter(Boolean);
    persistSessionPaths(storage, workspacePreferences, paths);
  }, [storage, tabs, workspacePreferences]);

  // Restore only the validated, count-bounded opt-in snapshot loaded at mount.
  useEffect(() => {
    if (!initialSettings.workspace.rememberPaths || !initialSettings.workspace.restoreSession) {
      restoredRef.current = true;
      return;
    }
    let cancelled = false;
    const saved = initialSettings.sessionPaths;
    void (async () => {
      for (const p of saved) {
        if (cancelled) return;
        await openFileRef.current(p);
      }
      if (!cancelled) restoredRef.current = true;
    })();
    return () => { cancelled = true; };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const refreshStaging = async (fileId: string): Promise<StagingStateData | null> => {
    const request = requestGateRef.current.beginFile("staging", fileId);
    try {
      const s = (await FileService.GetStagingState(fileId)) as StagingStateData;
      if (!isOpenSessionResponse(request)) return null;
      updateTabEditState(fileId, { staging: s });
      if (diffOpenRef.current && activeIdRef.current === fileId && editorFileIdRef.current === fileId) {
        await loadDiff(fileId);
      }
      return s;
    } catch {
      return null;
    }
  };

  const requireStaging = async (fileId: string): Promise<StagingStateData> => {
    const request = requestGateRef.current.beginFile("staging", fileId);
    const s = (await FileService.GetStagingState(fileId)) as StagingStateData;
    if (isOpenSessionResponse(request)) updateTabEditState(fileId, { staging: s });
    return s;
  };

  const releasePreparedEditSessionIfClean = async (fileId: string): Promise<void> => {
    if (!preparedEditFilesRef.current.has(fileId)) return;
    // The caller must first make the attached editor read-only; that transition
    // drains debounce-pending text. Re-read authoritative staging afterward and
    // never ask the backend to release a session that owns edits.
    const staging = await requireStaging(fileId);
    if (staging.editCount !== 0) return;
    const released = await EditPreparationService.ReleaseCleanEditSession(fileId);
    if (released.editCount !== 0) {
      throw new Error(`clean edit-session release returned ${released.editCount} staged edits`);
    }
    preparedEditFilesRef.current.delete(fileId);
    updateTabEditState(fileId, { dirty: false, staging: released });
  };

  const loadDiff = async (fileId: string) => {
    const request = requestGateRef.current.beginActive("diff", fileId);
    try {
      const edits = (await FileService.GetStagedEdits(fileId)) as StagedEditData[] | null;
      if (isActiveRequest(request)) setStagedEdits(edits ?? []);
    } catch {
      if (isActiveRequest(request)) setStagedEdits([]);
    }
  };

  // Run (or re-run) SQL analysis and cache it per file; shared by the Tools
  // panel, the command palette (jump-to-table), and the file X-ray.
  const analyze = async (fileId: string): Promise<NormalizedSqlSummaryResult> => {
    if (activeIdRef.current !== fileId || editorFileIdRef.current !== fileId) {
      throw new Error("analysis request no longer belongs to the active file");
    }
    const request = requestGateRef.current.beginActive("analysis", fileId);
    const s = normalizeSqlSummary(await FileService.SqlAnalyze(fileId));
    if (isActiveRequest(request)) {
      setAnalyses((previous) => cacheSqlAnalysis(previous, fileId, s));
    }
    return s;
  };

  const activateAttachedTab = async (fileId: string) => {
    const ed = editorRef.current ?? await editorReadyRef.current;
    const t = tabsRef.current.find((tab) => tab.fileId === fileId);
    if (!ed || !t) return;
    if (editorFileIdRef.current === fileId && activeIdRef.current === fileId) return;

    const currentJob = jobRef.current;
    if (isSQLAnalysisJob(currentJob) && currentJob.fileId !== fileId) {
      await cancelJobAndWait(currentJob.id);
    }

    // Invalidate every active-file response before attach starts. Keeping the
    // old activeId until attach succeeds is intentional, but must not allow an
    // old search/diff/tool callback to act on the shared editor mid-transition.
    invalidateActiveRequests();

    // Status callbacks carry their owning file ID. Snapshot the currently
    // owned raw position before attach flushes or replaces the editor window.
    persistOwnedPosition();
    const restorePosition = tabPositionsRef.current[fileId] ?? t.startByte;

    const previousFileId = editorFileIdRef.current;
    if (previousFileId
        && previousFileId !== fileId
        && preparedEditFilesRef.current.has(previousFileId)) {
      // Release only a confirmed-clean prepared session before transferring the
      // shared editor. A flush, staging lookup, or release failure leaves the
      // previous tab active and read-only; attach never runs after that failure.
      if (editModeRef.current) await ed.setEditMode(false);
      else await ed.flush();
      if (editorFileIdRef.current !== previousFileId || activeIdRef.current !== previousFileId) {
        throw new Error("editor ownership changed while preparing a tab switch");
      }
      await releasePreparedEditSessionIfClean(previousFileId);
    }

    // attach() first drains the currently attached editor. Do not change the
    // callback ownership ref until that flush has completed successfully.
    await ed.attach(
      fileId,
      t.meta.detected,
      t.meta.path,
      restorePosition,
      csvDialectsRef.current[fileId]?.delimiter ?? "",
    );
    const pendingRefresh = pendingSourceRefreshMetaRef.current.get(fileId);
    if (pendingRefresh) completePendingSourceRefresh(fileId, pendingRefresh);
    const targetEditState = tabEditStatesRef.current[fileId];
    const targetHasEdits = (targetEditState?.dirty ?? false) || (targetEditState?.staging?.editCount ?? 0) > 0;
    const targetSurface = normalizeSurfaceForFile(t.meta.detected, targetHasEdits, {
      gridView,
      hexView,
      toolsOpen,
      diffOpen: diffOpenRef.current,
    }, true);
    setGridView(targetSurface.gridView);
    setHexView(targetSurface.hexView);
    setToolsOpen(targetSurface.toolsOpen);
    if (targetSurface.diffOpen !== diffOpenRef.current) {
      diffOpenRef.current = targetSurface.diffOpen;
      if (!targetSurface.diffOpen) requestGateRef.current.invalidateChannel("diff");
      setDiffOpen(targetSurface.diffOpen);
    }
    editorFileIdRef.current = fileId;
    setActiveId(fileId);
    activeIdRef.current = fileId;
    applyNotificationAction({ type: "active-file", fileId });
    const attachedStatus = tabStatusesRef.current[fileId] ?? null;
    statusRef.current = attachedStatus;
    setStatus(attachedStatus);
    lastMatch.current = null;
    updateTabEditState(fileId, tabEditStatesRef.current[fileId] ?? { dirty: false, staging: null });
    await requireStaging(fileId);
    if (diffOpenRef.current && activeIdRef.current === fileId && editorFileIdRef.current === fileId) {
      await loadDiff(fileId);
    }
  };

  const activate = async (fileId: string, allowBusy = false) => {
    if (closeFlowRef.current || tabTransitionRef.current || (busyRef.current && !allowBusy)) return;
    const owner = beginOperation("Switch tab", {
      path: tabsRef.current.find((tab) => tab.fileId === fileId)?.meta.path,
    });
    tabTransitionRef.current = true;
    try {
      await activateAttachedTab(fileId);
    } catch (e: any) {
      publishOperation(owner, { severity: "error", message: "Unable to switch tabs", details: String(e?.message ?? e) });
    } finally {
      finishOperation(owner);
      tabTransitionRef.current = false;
    }
  };

  const showMeta = async (meta: FileMetaData) => {
    if (!meta.fileId) return;
    if (tabsRef.current.some((t) => t.fileId === meta.fileId)) {
      await activate(meta.fileId, true);
      return;
    }
    if (closeFlowRef.current || tabTransitionRef.current) {
      throw new Error("another tab transition is still in progress");
    }
    tabTransitionRef.current = true;
    const tab: Tab = { fileId: meta.fileId, meta, startByte: 0 };
    try {
      tabPositionsRef.current[meta.fileId] = 0;
      setTabs((prev) => [...prev, tab]);
      tabsRef.current = [...tabsRef.current, tab];
      updateTabEditState(meta.fileId, { dirty: false, staging: null });
      if (meta.encodingRequiresConfirmation) {
        throw new Error(
          `Encoding detection is ambiguous (${meta.encoding || "unknown encoding"}). `
          + "Convert a copy to UTF-8 or another known encoding, then open that copy. The source file was not changed.",
        );
      }
      await activateAttachedTab(meta.fileId);
      if (activeIdRef.current !== meta.fileId || editorFileIdRef.current !== meta.fileId) {
        throw new Error(`file activation did not attach ${meta.path}`);
      }
      pushRecent(meta.path);
    } catch (activationError) {
      // attach() is transactional, so a rejected initial window leaves the
      // previous editor identity and bytes intact. Roll back only while that
      // previous owner is still authoritative; failures after a committed
      // activation must retain the tab rather than closing the editor's owner.
      if (activeIdRef.current !== meta.fileId && editorFileIdRef.current !== meta.fileId) {
        const remaining = tabsRef.current.filter((item) => item.fileId !== meta.fileId);
        tabsRef.current = remaining;
        setTabs(remaining);
        delete tabPositionsRef.current[meta.fileId];
        delete tabStatusesRef.current[meta.fileId];
        preparedEditFilesRef.current.delete(meta.fileId);
        forgetTabEditState(meta.fileId);
        forgetCsvDialect(meta.fileId);
        try {
          await FileService.CloseFile(meta.fileId);
        } catch (closeError: any) {
          throw new Error(
            `${String((activationError as any)?.message ?? activationError)} `
            + `The failed file session could not be released: ${String(closeError?.message ?? closeError)}`,
          );
        }
      }
      throw activationError;
    } finally {
      tabTransitionRef.current = false;
    }
  };

  const openViaDialog = async () => {
    if (busyRef.current || closeFlowRef.current) return;
    const owner = beginOperation("Open file");
    setOperationBusy(true);
    try {
      const meta = (await FileService.OpenViaDialog()) as FileMetaData;
      await showMeta(meta);
    } catch (e: any) {
      publishOperation(owner, { severity: "error", message: "Unable to open file", details: String(e?.message ?? e) });
    } finally {
      finishOperation(owner);
      setOperationBusy(false);
    }
  };

  const openPath = async () => {
    const p = path;
    if (!isBoundedSourcePathInput(p)) return;
    if (busyRef.current || closeFlowRef.current) return;
    const owner = beginOperation("Open file", { path: p });
    setOperationBusy(true);
    try {
      const meta = (await FileService.OpenFile(p)) as FileMetaData;
      await showMeta(meta);
      setPath("");
    } catch (e: any) {
      publishOperation(owner, { severity: "error", message: "Unable to open file", details: String(e?.message ?? e) });
    } finally {
      finishOperation(owner);
      setOperationBusy(false);
    }
  };

  const askCloseDecision = (context: CloseDecisionContext): Promise<CloseChoice> => {
    if (closeDecisionResolverRef.current) {
      return Promise.reject(new Error("another unsaved-changes decision is already open"));
    }
    setCloseDecision(context);
    return new Promise<CloseChoice>((resolve) => {
      closeDecisionResolverRef.current = resolve;
    });
  };

  const chooseCloseDecision = (choice: CloseChoice) => {
    const resolve = closeDecisionResolverRef.current;
    if (!resolve) return;
    closeDecisionResolverRef.current = null;
    setCloseDecision(null);
    resolve(choice);
  };

  const resolveCloseTarget = async (
    tab: Tab,
    scope: CloseScope,
    position?: number,
    total?: number,
  ) => {
    const editState = tabEditStatesRef.current[tab.fileId] ?? { dirty: false, staging: null };
    const editorAttached = editorFileIdRef.current === tab.fileId;
    return resolveTabForClose(
      {
        fileId: tab.fileId,
        path: tab.meta.path,
        dirty: editState.dirty,
        editorAttached,
        scope,
        position,
        total,
      },
      {
        flushEditor: async () => {
          const ed = editorRef.current;
          if (!ed || editorFileIdRef.current !== tab.fileId) {
            throw new Error(`editor is not attached to ${tab.meta.path}`);
          }
          await ed.flush();
        },
        getStaging: requireStaging,
        choose: askCloseDecision,
        saveCopy: async (fileId) => await FileService.SaveCopyViaDialog(fileId),
        makeEditorReadOnly: async () => {
          const ed = editorRef.current;
          if (!ed || editorFileIdRef.current !== tab.fileId) {
            throw new Error(`editor is not attached to ${tab.meta.path}`);
          }
          await ed.setEditMode(false);
        },
        discard: async (fileId) => {
          const discarded = await FileService.DiscardEdits(fileId) as StagingStateData;
          // Keep ownership when a malformed/failed backend discard reports
          // retained edits. resolveTabForClose will fail closed on that result,
          // and later transitions must still know a prepared session exists.
          if (discarded.editCount === 0) preparedEditFilesRef.current.delete(fileId);
          return discarded;
        },
        onStaging: (fileId, value) => {
          updateTabEditState(fileId, { dirty: false, staging: value });
          if (diffOpenRef.current && activeIdRef.current === fileId && editorFileIdRef.current === fileId) {
            void loadDiff(fileId);
          }
        },
      },
    );
  };

  const closeTab = async (fileId: string, e?: React.MouseEvent) => {
    const closeInvoker = e?.currentTarget instanceof HTMLElement ? e.currentTarget : null;
    e?.stopPropagation();
    if (closeFlowRef.current || tabTransitionRef.current || busyRef.current) {
      notifyInstant("Close tab", {
        severity: "warning",
        message: "Finish or cancel the current operation before closing a tab",
      });
      return;
    }
    const tabIndex = tabsRef.current.findIndex((item) => item.fileId === fileId);
    const tab = tabsRef.current[tabIndex];
    if (!tab) return;
    if (closeInvoker) {
      pendingTabCloseFocusRef.current = {
        fileId,
        invoker: closeInvoker,
        outcome: "retained",
      };
    }
    const owner = beginOperation("Close tab", { path: tab.meta.path });
    let tabRemoved = false;

    closeFlowRef.current = true;
    tabTransitionRef.current = true;
    busyRef.current = true;
    invalidateActiveRequests(false);
    setCloseReview("tab");
    setOperationBusy(true);
    try {
      const currentJob = jobRef.current;
      if (isSQLAnalysisJob(currentJob, fileId)) {
        await cancelJobAndWait(currentJob.id);
      }
      const resolution = await resolveCloseTarget(tab, "tab");
      if (resolution.outcome === "cancelled") {
        publishOperation(owner, { severity: "info", message: "Tab close cancelled; edits were kept" });
        return;
      }
      if (resolution.outcome === "saved") {
        publishOperation(owner, {
          severity: "success",
          message: `Saved copy → ${resolution.save.outputPath} (${fmtBytes(resolution.save.bytesWritten)})`,
        });
      }

      const wasActive = activeIdRef.current === fileId;
      const remaining = tabsRef.current.filter((item) => item.fileId !== fileId);
      const next = wasActive ? remaining[remaining.length - 1] : undefined;
      if (next) {
        // Move the editor before closing its old backend session. If activation
        // fails, CloseFile is never called and the original tab remains intact.
        await activateAttachedTab(next.fileId);
      }

      // Backend close is part of the transaction: UI removal happens only
      // after it succeeds. A rejection leaves this tab in tabsRef/state.
      await closeBackendSession(fileId, FileService.CloseFile, () => {
        preparedEditFilesRef.current.delete(fileId);
        setTabs(remaining);
        tabsRef.current = remaining;
        delete tabPositionsRef.current[fileId];
        delete tabStatusesRef.current[fileId];
        pendingSourceRefreshMetaRef.current.delete(fileId);
        forgetTabEditState(fileId);
        forgetCsvDialect(fileId);
        setAnalyses((previous) => {
          if (!(fileId in previous)) return previous;
          const nextAnalyses = { ...previous };
          delete nextAnalyses[fileId];
          return nextAnalyses;
        });
        if (fileId in sourceRefreshEpochsRef.current) {
          const nextEpochs = { ...sourceRefreshEpochsRef.current };
          delete nextEpochs[fileId];
          sourceRefreshEpochsRef.current = nextEpochs;
          setSourceRefreshEpochs(nextEpochs);
        }
      });
      tabRemoved = true;
      if (closeInvoker) {
        const replacement = remaining[Math.min(tabIndex, remaining.length - 1)];
        pendingTabCloseFocusRef.current = {
          fileId,
          invoker: closeInvoker,
          outcome: "removed",
          ...(replacement ? { replacementFileId: replacement.fileId } : {}),
        };
      }
      if (wasActive && !next) {
        invalidateActiveRequests();
        setActiveId(null);
        activeIdRef.current = null;
        editorFileIdRef.current = null;
        applyNotificationAction({ type: "active-file", fileId: null });
        editorRef.current?.clear();
        statusRef.current = null;
        setStatus(null);
      }
    } catch (e: any) {
      publishOperation(owner, {
        severity: "error",
        message: `Could not close ${tab.meta.path}`,
        details: String(e?.message ?? e),
      });
    } finally {
      if (closeInvoker && !tabRemoved) {
        pendingTabCloseFocusRef.current = {
          fileId,
          invoker: closeInvoker,
          outcome: "retained",
        };
      }
      finishOperation(owner);
      closeFlowRef.current = false;
      tabTransitionRef.current = false;
      busyRef.current = false;
      setCloseReview(null);
      setOperationBusy(false);
    }
  };
  closeTabRef.current = (fileId: string) => closeTab(fileId);

  const cancelPendingApplicationClose = async (owner?: NotificationOwner) => {
    try {
      await AppLifecycle.CancelClose();
    } catch (e: any) {
      const target = owner ?? beginOperation("Cancel application close");
      publishOperation(target, {
        severity: "error",
        message: "Application close remains blocked",
        details: String(e?.message ?? e),
      });
      finishOperation(target);
    }
  };

  const requestApplicationClose = async () => {
    if (closeFlowRef.current || tabTransitionRef.current || busyRef.current) {
      const owner = notifyInstant("Close application", {
        severity: "warning",
        message: "Application close cancelled because another operation is active",
      });
      await cancelPendingApplicationClose(owner);
      return;
    }
    const owner = beginOperation("Close application");
    const activeElement = document.activeElement;
    pendingApplicationCloseFocusRef.current = activeElement instanceof HTMLElement
      && activeElement !== document.body
      && !activeElement.closest('[role="dialog"][aria-modal="true"]')
      ? activeElement
      : null;

    closeFlowRef.current = true;
    tabTransitionRef.current = true;
    busyRef.current = true;
    invalidateActiveRequests(false);
    setCloseReview("application");
    setOperationBusy(true);
    let approved = false;
    try {
      const currentJob = jobRef.current;
      if (currentJob) {
        await cancelJobAndWait(currentJob.id);
      }
      const snapshot = [...tabsRef.current];
      const editorFileId = editorFileIdRef.current;
      const ordered = [
        ...snapshot.filter((tab) => tab.fileId === editorFileId),
        ...snapshot.filter((tab) => tab.fileId !== editorFileId),
      ];
      const resolved = await resolveApplicationClose(ordered, async (tab, position, total) => {
        return await resolveCloseTarget(tab, "application", position, total);
      });
      if (!resolved) {
        publishOperation(owner, { severity: "info", message: "Application close cancelled; unresolved edits were kept" });
        await cancelPendingApplicationClose(owner);
        return;
      }

      // Keep React's tab list intact so the existing session-restore list is
      // not erased during an orderly application exit.
      await AppLifecycle.ApproveClose();
      approved = true;
    } catch (e: any) {
      publishOperation(owner, {
        severity: "error",
        message: "Application close was blocked",
        details: String(e?.message ?? e),
      });
      await cancelPendingApplicationClose(owner);
    } finally {
      finishOperation(owner);
      if (!approved) {
        closeFlowRef.current = false;
        tabTransitionRef.current = false;
        busyRef.current = false;
        setCloseReview(null);
        setOperationBusy(false);
      } else {
        pendingApplicationCloseFocusRef.current = null;
      }
    }
  };
  requestApplicationCloseRef.current = requestApplicationClose;

  // ---- search / goto ----
  const runFind = async (dir: "next" | "prev") => {
    const id = activeIdRef.current;
    if (!id) return;
    const context = searchContextFor(id);
    if (!context.query.trim()) return;
    if (busyRef.current || closeFlowRef.current || tabTransitionRef.current) return;
    const tab = tabsRef.current.find((t) => t.fileId === id);
    if (!tab) return;
    const request = requestGateRef.current.beginActive("search", id);
    resultsRequestRef.current = null;
    setResults(null);
    setSearching(true);
    setSearchInfo("Searching…");
    let requestId = "";
    try {
      requestId = await FileService.BeginSearchRequest(id);
      if (!isActiveRequest(request) || !isCurrentSearchContext(context)) {
        cancelSearchRequest(requestId);
        return;
      }
      activeSearchRef.current = { requestId, request };
      setActiveSearchRequestId(requestId);
      let hit;
      const origin = currentSearchOrigin(id, tab.startByte);
      if (dir === "next") {
        const from = lastMatch.current != null ? lastMatch.current + 1 : origin;
        hit = await FileService.FindNextRequest(requestId, context.query, from, context.regex, context.caseSensitive, context.wholeWord);
      } else {
        const before = lastMatch.current != null ? lastMatch.current : origin;
        hit = await FileService.FindPrevRequest(requestId, context.query, before, context.regex, context.caseSensitive, context.wholeWord);
      }
      if (!isActiveRequest(request) || !isCurrentSearchContext(context)) return;
      if (hit.unsupported) {
        setSearchInfo(hit.message || "Search not supported for this file's encoding");
        return;
      }
      if (hit.timedOut) {
        setSearchInfo("Timed out — narrow the query");
        return;
      }
      if (!hit.found) {
        setSearchInfo(dir === "next" ? "No matches below" : "No matches above");
        return;
      }
      lastMatch.current = hit.offset;
      const ed = editorRef.current;
      if (!ed) return;
      await ed.showMatch(
        hit.offset,
        hit.length,
        context.query,
        context.regex,
        context.caseSensitive,
        () => isActiveRequest(request) && isCurrentSearchContext(context),
      );
      if (isActiveRequest(request) && isCurrentSearchContext(context)) {
        setSearchInfo(`0x${hit.offset.toString(16)} · line ~${hit.line}`);
      }
    } catch (e: any) {
      if (isActiveRequest(request) && isCurrentSearchContext(context)) setSearchInfo(String(e?.message ?? e));
    } finally {
      if (activeSearchRef.current?.requestId === requestId) {
        activeSearchRef.current = null;
        setActiveSearchRequestId(null);
      }
      if (isActiveRequest(request) && isCurrentSearchContext(context)) setSearching(false);
    }
  };

  const runSearchAll = async () => {
    const id = activeIdRef.current;
    if (!id) return;
    const context = searchContextFor(id);
    if (!context.query.trim()) return;
    if (busyRef.current || closeFlowRef.current || tabTransitionRef.current) return;
    const request = requestGateRef.current.beginActive("search", id);
    const session: SearchResultsSession = { request, context };
    resultsRequestRef.current = session;
    setSearching(true);
    setResultsInfo("Searching whole file…");
    setResults([]);
    let requestId = "";
    try {
      requestId = await FileService.BeginSearchRequest(id);
      if (!isCurrentResultsSession(session)) {
        cancelSearchRequest(requestId);
        return;
      }
      activeSearchRef.current = { requestId, request };
      setActiveSearchRequestId(requestId);
      const r = await FileService.SearchAllRequest(requestId, context.query, context.regex, context.caseSensitive, context.wholeWord, 1000);
      if (!isCurrentResultsSession(session)) return;
      if (r.unsupported) {
        resultsRequestRef.current = null;
        setResultsInfo(r.message || "Unsupported for this encoding");
        setResults(null);
        return;
      }
      const presentation = presentSearchPage<{ offset: number; length: number; line: number; preview: string }>(r);
      setResults(presentation.hits);
      setResultsInfo(presentation.info);
    } catch (e: any) {
      if (isCurrentResultsSession(session)) {
        resultsRequestRef.current = null;
        setResultsInfo(String(e?.message ?? e));
        setResults(null);
      }
    } finally {
      if (activeSearchRef.current?.requestId === requestId) {
        activeSearchRef.current = null;
        setActiveSearchRequestId(null);
      }
      if (isActiveRequest(request) && isCurrentSearchContext(context)) setSearching(false);
    }
  };

  const cancelActiveSearch = (requestId: string) => {
    const active = activeSearchRef.current;
    if (!active || active.requestId !== requestId) return;
    activeSearchRef.current = null;
    setActiveSearchRequestId(null);
    cancelSearchRequest(requestId);
    requestGateRef.current.invalidateChannel("search");
    resultsRequestRef.current = null;
    lastMatch.current = null;
    setSearching(false);
    setSearchInfo("Search cancelled");
    setResults(null);
    setResultsInfo("");
  };

  const harvest = async () => {
    const q = query;
    const id = activeIdRef.current;
    if (!q.trim() || !id) return;
    if (!regex) {
      setSearchInfo("Turn on regular-expression search before extracting matches");
      return;
    }
    if (busyRef.current || closeFlowRef.current || tabTransitionRef.current) return;
    const request = requestGateRef.current.beginActive("harvest", id);
    const activePath = tabsRef.current.find((tab) => tab.fileId === id)?.meta.path;
    const owner = beginOperation("Extract search matches", { fileId: id, path: activePath, jobCandidate: true });
    setOperationBusy(true);
    try {
      const r = await FileService.HarvestMatchesViaDialog(id, q, !caseSensitive);
      if (isActiveRequest(request) && r.outputPath) {
        publishOperation(owner, {
          severity: "success",
          message: `Extracted ${r.recordsWritten} matches → ${r.outputPath}`,
        });
      }
    } catch (e: any) {
      if (isActiveRequest(request)) publishOperation(owner, {
        severity: "error",
        message: "Unable to extract search matches",
        details: String(e?.message ?? e),
      });
    } finally {
      finishOperation(owner);
      setOperationBusy(false);
    }
  };

  const doGoto = async () => {
    const v = gotoValue.trim();
    const id = activeIdRef.current;
    if (!v || !id) return;
    if (busyRef.current || closeFlowRef.current || tabTransitionRef.current) return;
    const request = requestGateRef.current.beginActive("goto", id);
    const tab = tabsRef.current.find((t) => t.fileId === id);
    const owner = beginOperation("Go to position", { fileId: id, path: tab?.meta.path });
    try {
      let offset: number | null = null;
      let goToEnd = false;
      let retainGotoStatus = false;
      const pct = v.match(/^(\d{1,3}(?:\.\d+)?)\s*%$/);
      if (pct && tab) {
        const p = Number(pct[1]);
        if (!Number.isFinite(p) || p < 0 || p > 100) {
          setSearchInfo("Percentage must be between 0 and 100");
          return;
        }
        if (p === 100) goToEnd = true;
        else offset = Math.floor((tab.meta.size * p) / 100);
      } else if (/^0x[0-9a-f]+$/i.test(v)) {
        offset = Number.parseInt(v.slice(2), 16);
        if (!Number.isSafeInteger(offset)) {
          setSearchInfo("Byte offset is outside the exact supported range");
          return;
        }
      }
      else if (/^\d+$/.test(v)) {
        const line = Number(v);
        if (!Number.isSafeInteger(line) || line < 1) {
          setSearchInfo("Line number is outside the exact supported range");
          return;
        }
        const resolved = (await FileService.ResolveLine(id, line)) as unknown as LineResolution;
        if (!isActiveRequest(request)) return;
        if (!resolved.found) {
          if (resolved.limited) {
            setSearchInfo("Exact line lookup reached its bounded scan limit before a usable known position was available");
          } else {
            setSearchInfo(resolved.indexComplete ? "Line does not exist" : "Line index is not ready yet");
          }
          return;
        }
        offset = resolved.offset;
        if (!resolved.exact) {
          const resolvedPosition = Number.isSafeInteger(resolved.resolvedLine) && resolved.resolvedLine > 0
            ? `nearest known line ${resolved.resolvedLine}`
            : "the nearest known position";
          setSearchInfo(resolved.limited
            ? `Exact line lookup reached its bounded scan limit; using ${resolvedPosition} instead`
            : `Using ${resolvedPosition} while indexing continues`);
          retainGotoStatus = true;
        }
      }
      if (!isActiveRequest(request)) return;
      if (!goToEnd && (offset == null || Number.isNaN(offset))) {
        setSearchInfo("Enter a line number, 0xHEX offset, or NN%");
        return;
      }
      lastMatch.current = null;
      const ed = editorRef.current;
      if (!ed) return;
      if (goToEnd) await ed.gotoEnd();
      else await ed.gotoByte(offset!);
      if (isActiveRequest(request) && !retainGotoStatus) setGotoOpen(false);
    } catch (e: any) {
      if (isActiveRequest(request)) publishOperation(owner, {
        severity: "error",
        message: "Unable to go to the requested position",
        details: String(e?.message ?? e),
      });
    } finally {
      finishOperation(owner);
    }
  };

  const showListedMatch = async (result: { offset: number; length: number }) => {
    const session = resultsRequestRef.current;
    if (!session || !isCurrentResultsSession(session)) return;
    if (busyRef.current || closeFlowRef.current || tabTransitionRef.current) return;
    const ed = editorRef.current;
    if (!ed) return;
    lastMatch.current = result.offset;
    try {
      await ed.showMatch(
        result.offset,
        result.length,
        session.context.query,
        session.context.regex,
        session.context.caseSensitive,
        () => isCurrentResultsSession(session),
      );
    } catch (e: any) {
      if (isCurrentResultsSession(session)) setSearchInfo(String(e?.message ?? e));
    }
  };

  const seekActiveFile = async (fileId: string, offset: number) => {
    if (activeIdRef.current !== fileId || editorFileIdRef.current !== fileId) return;
    if (busyRef.current || closeFlowRef.current || tabTransitionRef.current) return;
    const request = requestGateRef.current.beginActive("seek", fileId);
    const owner = beginOperation("Go to byte offset", {
      fileId,
      path: tabsRef.current.find((tab) => tab.fileId === fileId)?.meta.path,
    });
    const ed = editorRef.current;
    if (!ed) {
      finishOperation(owner);
      return;
    }
    try {
      await ed.gotoByte(offset);
    } catch (e: any) {
      if (isActiveRequest(request)) publishOperation(owner, {
        severity: "error",
        message: "Unable to go to the requested byte offset",
        details: String(e?.message ?? e),
      });
    } finally {
      finishOperation(owner);
    }
  };

  // ---- editing / save ----
  const toggleEdit = async () => {
    const ed = editorRef.current;
    const id = activeIdRef.current;
    if (!ed || !id) return;
    if (busyRef.current || closeFlowRef.current || tabTransitionRef.current) return;
    const currentTab = tabsRef.current.find((tab) => tab.fileId === id);
    const turningOn = !editModeRef.current;
    if (turningOn && pendingSourceRefreshMetaRef.current.has(id)) {
      notifyInstant("Start editing", {
        severity: "warning",
        message: "Wait for the refreshed file view to finish synchronizing before editing",
      }, { fileId: id, path: currentTab?.meta.path });
      return;
    }
    if (turningOn && !currentTab?.meta.editable) {
      notifyInstant("Start editing", {
        severity: "error",
        message: "Editing is unavailable for this file encoding or line-ending format",
      }, { fileId: id, path: currentTab?.meta.path });
      return;
    }
    const request = requestGateRef.current.beginActive("editor-operation", id);
    const owner = beginOperation(turningOn ? "Start editing" : "Stop editing", {
      fileId: id,
      path: tabsRef.current.find((tab) => tab.fileId === id)?.meta.path,
      jobCandidate: turningOn,
    });
    setOperationBusy(true);
    try {
      if (turningOn) {
        // The initial exact source fingerprint is O(file size). Run it through
        // the managed backend job before requesting an editable window so the
        // existing progress/cancel surface owns the operation. A cancelled or
        // failed preparation installs no session and must never enable editing.
        const prepared = await EditPreparationService.PrepareEditSession(id);
        preparedEditFilesRef.current.add(id);
        if (!isActiveRequest(request)) return;
        updateTabEditState(id, { dirty: false, staging: prepared });
      }
      if (!isActiveRequest(request)) return;
      await ed.setEditMode(turningOn);
      if (!isActiveRequest(request)) return;
      if (turningOn) {
        // Editing always owns the visible editor surface; an editable
        // CodeMirror must not remain focusable behind a grid or hex overlay.
        setGridView(false);
        setHexView(false);
      }
      if (!turningOn) await releasePreparedEditSessionIfClean(id);
    } catch (e: any) {
      if (isActiveRequest(request)) publishOperation(owner, {
        severity: "error",
        message: "Unable to change editor mode",
        details: String(e?.message ?? e),
      });
    } finally {
      finishOperation(owner);
      setOperationBusy(false);
    }
  };

  const toggleDiff = async () => {
    const id = activeIdRef.current;
    if (busyRef.current || closeFlowRef.current || tabTransitionRef.current) return;
    const next = !diffOpenRef.current;
    setDiffOpen(next);
    diffOpenRef.current = next;
    if (!next) requestGateRef.current.invalidateChannel("diff");
    if (next && id) await loadDiff(id);
  };

  const closeDiff = () => {
    diffOpenRef.current = false;
    requestGateRef.current.invalidateChannel("diff");
    setDiffOpen(false);
  };

  const discardEdits = async () => {
    const ed = editorRef.current;
    const id = activeIdRef.current;
    if (!ed || !id) return;
    if (busyRef.current || closeFlowRef.current || tabTransitionRef.current) return;
    const request = requestGateRef.current.beginActive("editor-operation", id);
    const owner = beginOperation("Discard edits", {
      fileId: id,
      path: tabsRef.current.find((tab) => tab.fileId === id)?.meta.path,
    });
    setOperationBusy(true);
    try {
      // Drain the current draft and close the editable surface before backend
      // staging is cleared. Otherwise input arriving during DiscardEdits can be
      // stranded against the now-released prepared edit session.
      await ed.setEditMode(false);
      if (!isActiveRequest(request)) return;
      const s = (await FileService.DiscardEdits(id)) as StagingStateData;
      if (s.editCount !== 0) {
        throw new Error(`discard did not clear staged edits for ${id}`);
      }
      preparedEditFilesRef.current.delete(id);
      if (!isActiveRequest(request)) return;
      updateTabEditState(id, { dirty: false, staging: s });
      setStagedEdits([]);
      if (isActiveRequest(request)) publishOperation(owner, { severity: "success", message: "Edits discarded" });
    } catch (e: any) {
      if (isActiveRequest(request)) publishOperation(owner, {
        severity: "error",
        message: "Unable to discard edits",
        details: String(e?.message ?? e),
      });
    } finally {
      finishOperation(owner);
      setOperationBusy(false);
    }
  };

  const saveCopy = async () => {
    const ed = editorRef.current;
    const id = activeIdRef.current;
    if (!ed || !id) return;
    if (busyRef.current || closeFlowRef.current || tabTransitionRef.current) return;
    const request = requestGateRef.current.beginActive("editor-operation", id);
    const owner = beginOperation("Save copy", {
      fileId: id,
      path: tabsRef.current.find((tab) => tab.fileId === id)?.meta.path,
      jobCandidate: true,
    });
    setOperationBusy(true);
    try {
      await ed.flush();
      if (!isActiveRequest(request)) return;
      const r = await FileService.SaveCopyViaDialog(id);
      if (!isActiveRequest(request)) return;
      if (r.mode === "copy") {
        publishOperation(owner, {
          severity: "success",
          message: `Saved copy → ${r.outputPath} (${fmtBytes(r.bytesWritten)})`,
        });
        await refreshStaging(id);
      }
    } catch (e: any) {
      if (isActiveRequest(request)) publishOperation(owner, {
        severity: "error",
        message: "Unable to save a copy",
        details: String(e?.message ?? e),
      });
    } finally {
      finishOperation(owner);
      setOperationBusy(false);
    }
  };

  const hasFiles = tabs.length > 0;
  const activeTab = tabs.find((t) => t.fileId === activeId);
  const activeEditState = activeId ? tabEditStates[activeId] : undefined;
  const dirty = activeEditState?.dirty ?? false;
  const staging = activeEditState?.staging ?? null;
  const editCount = staging?.editCount ?? 0;
  const hasEdits = editCount > 0 || dirty;
  const activeSourceSyncPending = activeId != null && pendingSourceRefreshMetaRef.current.has(activeId);
  const detected = (activeTab?.meta.detected ?? "").toLowerCase();
  const isCsv = detected === "csv" || detected === "tsv";
  const isSql = detected === "sql";
  const activeViewContext = activeTab
    ? requestGateRef.current.captureActive(activeTab.fileId)
    : null;
  const activeViewFileId = activeViewContext?.fileId ?? null;
  const activeViewEpoch = activeViewContext?.activeEpoch ?? -1;
  const activeViewPath = activeTab?.meta.path;
  const activeViewIsCurrent = (): boolean =>
    activeViewContext != null
    && isActiveContext(activeViewContext.fileId, activeViewContext.activeEpoch);
  const activeViewStart = (operation: string, jobCandidate = false): NotificationOwner =>
    beginOperation(operation, {
      fileId: activeViewFileId ?? undefined,
      path: activeViewPath,
      jobCandidate,
    });
  const activeViewNotice = (owner: NotificationOwner, value: string, details?: string) => {
    if (activeViewIsCurrent() && owner.fileId === activeViewFileId) {
      publishOperation(owner, { severity: "success", message: value, details });
    }
    finishOperation(owner);
  };
  const activeViewOwnedError = (owner: NotificationOwner, value: string, details?: string) => {
    if (activeViewIsCurrent() && owner.fileId === activeViewFileId) {
      publishOperation(owner, { severity: "error", message: value, details });
    }
    finishOperation(owner);
  };
  const activeViewComplete = (owner: NotificationOwner) => finishOperation(owner);
  const activePanelError = useCallback((value: string) => {
    if (activeViewFileId != null && requestGateRef.current.isActiveContext(
      { fileId: activeViewFileId, activeEpoch: activeViewEpoch },
      activeIdRef.current,
      editorFileIdRef.current,
    )) {
      notifyInstant("Active file panel", { severity: "error", message: value }, {
        fileId: activeViewFileId,
        path: tabsRef.current.find((tab) => tab.fileId === activeViewFileId)?.meta.path,
      });
    }
  }, [activeViewEpoch, activeViewFileId]);

  useEffect(() => {
    const fileChanged = previousSurfaceFileRef.current !== activeId;
    previousSurfaceFileRef.current = activeId;
    const normalized = normalizeSurfaceForFile(detected, hasEdits, {
      gridView,
      hexView,
      toolsOpen,
      diffOpen,
    }, fileChanged);
    if (normalized.gridView !== gridView) setGridView(normalized.gridView);
    if (normalized.hexView !== hexView) setHexView(normalized.hexView);
    if (normalized.toolsOpen !== toolsOpen) setToolsOpen(normalized.toolsOpen);
    if (normalized.diffOpen !== diffOpen) {
      diffOpenRef.current = normalized.diffOpen;
      if (!normalized.diffOpen) requestGateRef.current.invalidateChannel("diff");
      setDiffOpen(normalized.diffOpen);
    }
    // Re-normalize only at a file/type/edit-capability boundary. Including the
    // view flags would immediately undo a user's valid same-file selection.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [activeId, detected, hasEdits]);

  const analyzeActiveView = async (fileId: string): Promise<NormalizedSqlSummaryResult> => {
    if (!activeViewContext || fileId !== activeViewContext.fileId || !activeViewIsCurrent()) {
      throw new Error("tool request no longer belongs to the active file");
    }
    return analyze(fileId);
  };
  const closeToolsPanel = () => {
    if (busyRef.current || closeFlowRef.current || tabTransitionRef.current) return;
    if (!activeViewIsCurrent()) return;
    const currentJob = jobRef.current;
    if (isSQLAnalysisJob(currentJob, activeViewFileId)) {
      void cancelJob(currentJob.id);
    }
    setToolsOpen(false);
  };
  const toggleToolsPanel = () => {
    if (toolsOpen) closeToolsPanel();
    else if (isCsv || isSql) setToolsOpen(true);
  };
  const openSearch = () => {
    requestGateRef.current.invalidateChannel("goto");
    setGotoOpen(false);
    setSearchOpen(true);
  };
  const openGoto = () => {
    invalidateSearch();
    requestGateRef.current.invalidateChannel("goto");
    setSearchOpen(false);
    setGotoOpen(true);
  };
  const closeSearch = () => {
    invalidateSearch();
    setSearchOpen(false);
  };
  const showText = () => { setHexView(false); setGridView(false); };
  const toggleFollowing = () => {
    if (following) {
      setFollowing(false);
      return;
    }
    if (!hasFiles || hasEdits) {
      notifyInstant("Follow tail", {
        severity: "warning",
        message: hasEdits ? "Save or discard edits before following the file tail" : "Open a file before enabling live follow",
      }, { fileId: activeId ?? undefined, path: activeTab?.meta.path });
      return;
    }
    setFollowing(true);
  };

  const toggleRememberWorkspacePaths = () => {
    const enabling = !workspacePreferences.rememberPaths;
    const requested: WorkspacePreferences = enabling
      ? { rememberPaths: true, restoreSession: false }
      : { rememberPaths: false, restoreSession: false };
    const result = commitWorkspacePreferences(storage, requested);
    workspacePreferencesRef.current = result.preferences;
    setWorkspacePreferences(result.preferences);

    if (!enabling) {
      // Clear path-bearing UI history without touching the live backend tabs.
      setRecent([]);
      setBookmarks(normalizeBookmarks(null));
      notifyInstant("Update workspace privacy", {
        severity: result.persisted ? "success" : "warning",
        message: result.persisted
          ? "Workspace path memory is off; remembered paths were cleared. Open tabs were kept."
          : "Workspace path memory is off for this run, but browser storage could not be fully cleared.",
      });
      return;
    }
    if (!result.preferences.rememberPaths) {
      notifyInstant("Update workspace privacy", {
        severity: "error",
        message: "Workspace path memory could not be enabled because the preference could not be stored",
      });
      return;
    }
    persistRecentPaths(storage, result.preferences, recent);
    persistBookmarks(storage, result.preferences, bookmarks);
    notifyInstant("Update workspace privacy", {
      severity: "success",
      message: "Workspace path memory is on. Current recent files and bookmarks may now be stored locally.",
    });
  };

  const toggleAutomaticSessionRestore = () => {
    if (!workspacePreferences.rememberPaths) return;
    const enabling = !workspacePreferences.restoreSession;
    const result = commitWorkspacePreferences(storage, {
      rememberPaths: true,
      restoreSession: enabling,
    });
    workspacePreferencesRef.current = result.preferences;
    setWorkspacePreferences(result.preferences);
    if (!result.preferences.rememberPaths) {
      setRecent([]);
      setBookmarks(normalizeBookmarks(null));
      notifyInstant("Update session restore", {
        severity: "error",
        message: "The session-restore preference could not be stored; workspace path memory was disabled and cleared",
      });
      return;
    }
    notifyInstant("Update session restore", {
      severity: "success",
      message: enabling
        ? "Automatic restore is on; up to 16 open paths will be remembered for the next launch."
        : "Automatic restore is off; the stored session path list was cleared.",
    });
  };

  const clearRememberedWorkspaceData = () => {
    const cleared = clearWorkspacePathData(storage);
    setRecent([]);
    setBookmarks(normalizeBookmarks(null));
    notifyInstant("Clear workspace data", {
      severity: cleared ? "success" : "warning",
      message: cleared
        ? "Recent files, stored session paths, and bookmarks were cleared. Open tabs were kept."
        : "Open tabs were kept, but browser storage could not be fully cleared.",
    });
  };

  const toolsMenuItems = (): MenuItem[] => {
    if (isCsv) {
      return [
        { label: "CSV → SQL converter…", onClick: () => setToolsOpen(true) },
        { label: "CSV column tools…", onClick: () => setToolsOpen(true) },
      ];
    }
    if (isSql) {
      return [
        { label: "Analyze & extract tables…", onClick: () => setToolsOpen(true) },
        { label: "Find & replace…", onClick: () => setToolsOpen(true) },
      ];
    }
    return [{ label: "No data tools for this file type", disabled: true }];
  };

  const menus: MenuDef[] = [
    {
      label: "File",
      items: [
        { label: "Open file…", shortcut: primaryShortcut("O"), onClick: () => void openViaDialog() },
        { label: "Close tab", shortcut: primaryShortcut("W"), disabled: !activeTab, onClick: () => activeId && void closeTab(activeId) },
        ...(recent.length > 0
          ? [
              { separator: true } as MenuItem,
              { label: workspacePreferences.rememberPaths ? "Recent files" : "Recent files (this session)", disabled: true } as MenuItem,
              ...recent.slice(0, 10).map((p) => ({
                label: "  " + p.replace(/^.*[\\/]/, ""),
                onClick: () => void openFilePath(p),
              })),
            ]
          : []),
        { separator: true },
        { label: "Save copy…", disabled: !hasEdits, onClick: () => void saveCopy() },
        { label: "Discard edits", disabled: !hasEdits, onClick: () => void discardEdits() },
        { separator: true },
        { label: "Remember workspace paths", checked: workspacePreferences.rememberPaths, onClick: toggleRememberWorkspacePaths },
        {
          label: "Restore remembered tabs on launch",
          checked: workspacePreferences.restoreSession,
          disabled: !workspacePreferences.rememberPaths,
          onClick: toggleAutomaticSessionRestore,
        },
        { label: "Clear recent, session & bookmarks", onClick: clearRememberedWorkspaceData },
      ],
    },
    {
      label: "Edit",
      items: [
        { label: "Find…", shortcut: primaryShortcut("F"), disabled: !hasFiles, onClick: openSearch },
        { label: "Go to line / offset…", shortcut: primaryShortcut("G"), disabled: !hasFiles, onClick: openGoto },
        { separator: true },
        { label: "Edit mode", checked: editMode, disabled: !activeTab?.meta.editable || activeSourceSyncPending, onClick: () => void toggleEdit() },
        { label: "Diff panel", checked: diffOpen, disabled: !hasEdits, onClick: () => void toggleDiff() },
      ],
    },
    {
      label: "View",
      items: [
        { label: "Sidebar", shortcut: primaryShortcut("B"), checked: !sidebarCollapsed, onClick: () => setSidebarCollapsed((v) => !v) },
        { label: "File map (X-ray)", checked: xrayOn, onClick: () => setXrayOn((v) => !v) },
        { label: "Bookmarks", disabled: !hasFiles, checked: bookmarksOpen, onClick: () => setBookmarksOpen((v) => !v) },
        { label: "Add bookmark here", disabled: !activeTab, onClick: addBookmark },
        { label: "Follow tail (live)", disabled: !following && (!hasFiles || hasEdits), checked: following, onClick: toggleFollowing },
        { label: "Command palette…", shortcut: primaryShortcut("P"), onClick: openCommandPalette },
        { label: "Byte offsets in gutter", checked: showByteOffsets, onClick: () => setShowByteOffsets((v) => !v) },
        { label: "Light theme", checked: theme === "light", onClick: toggleTheme },
        { separator: true },
        { label: "Plain text", checked: !hexView && !gridView, disabled: !hasFiles, onClick: showText },
        { label: "Hex view", checked: hexView, disabled: !hasFiles || editMode, onClick: () => { setGridView(false); setHexView((v) => !v); } },
        { label: "Grid view", checked: gridView, disabled: !isCsv || editMode, onClick: () => { setHexView(false); setGridView((v) => !v); } },
        { separator: true },
        { label: "Data tools panel", checked: toolsOpen, disabled: !(isCsv || isSql), onClick: toggleToolsPanel },
      ],
    },
    { label: "Tools", items: toolsMenuItems() },
    {
      label: "Help",
      items: [
        { label: "Help & shortcuts…", shortcut: "F1", onClick: openHelp },
        { separator: true },
        { label: "About Quarry", onClick: () => void showAbout() },
      ],
    },
  ];

  const commands: Command[] = [
    { id: "open", group: "File", label: "Open file…", hint: primaryShortcut("O"), run: () => void openViaDialog() },
    { id: "close", group: "File", label: "Close tab", hint: primaryShortcut("W"), enabled: Boolean(activeTab), disabledReason: "No file is open", run: () => activeId && void closeTab(activeId) },
    { id: "savecopy", group: "File", label: "Save copy…", enabled: hasEdits, disabledReason: "No edits to save", run: () => void saveCopy() },
    { id: "discard", group: "File", label: "Discard edits", enabled: hasEdits, disabledReason: "No edits to discard", run: () => void discardEdits() },
    { id: "find", group: "Edit", label: "Find…", hint: primaryShortcut("F"), enabled: hasFiles, disabledReason: "No file is open", run: openSearch },
    { id: "goto", group: "Edit", label: "Go to line / offset…", hint: primaryShortcut("G"), enabled: hasFiles, disabledReason: "No file is open", run: openGoto },
    { id: "edit", group: "Edit", label: editMode ? "Turn editing off" : "Turn editing on", enabled: editMode || (Boolean(activeTab?.meta.editable) && !activeSourceSyncPending), disabledReason: activeSourceSyncPending ? "The refreshed file view is still synchronizing" : "This file cannot be edited safely", run: () => void toggleEdit() },
    { id: "sidebar", group: "View", label: "Toggle sidebar", hint: primaryShortcut("B"), run: () => setSidebarCollapsed((v) => !v) },
    { id: "theme", group: "View", label: theme === "light" ? "Switch to dark theme" : "Switch to light theme", run: toggleTheme },
    { id: "byteoffsets", group: "View", label: showByteOffsets ? "Hide byte offsets in gutter" : "Show byte offsets in gutter", run: () => setShowByteOffsets((v) => !v) },
    { id: "bookmark", group: "View", label: "Add bookmark here", enabled: Boolean(activeTab), disabledReason: "No file is open", run: addBookmark },
    { id: "bookmarks", group: "View", label: "Toggle bookmarks panel", enabled: hasFiles, disabledReason: "No file is open", run: () => setBookmarksOpen((v) => !v) },
    { id: "follow", group: "View", label: following ? "Stop following tail" : "Follow tail (live)", enabled: following || (hasFiles && !hasEdits), disabledReason: hasEdits ? "Save or discard edits first" : "No file is open", run: toggleFollowing },
    { id: "text", group: "View", label: "Plain text view", enabled: hasFiles, disabledReason: "No file is open", run: showText },
    { id: "hex", group: "View", label: "Toggle hex view", enabled: hasFiles && !editMode, disabledReason: editMode ? "Turn editing off before changing the primary view" : "No file is open", run: () => { setGridView(false); setHexView((v) => !v); } },
    { id: "grid", group: "View", label: "Toggle grid view", enabled: isCsv && !editMode, disabledReason: editMode ? "Turn editing off before changing the primary view" : "Grid view requires a CSV or TSV file", run: () => { setHexView(false); setGridView((v) => !v); } },
    { id: "tools", group: "Tools", label: (isCsv || isSql) ? "Toggle data tools panel" : "Data tools (CSV/SQL only)", enabled: isCsv || isSql, disabledReason: "Data tools require a CSV, TSV, or SQL file", run: toggleToolsPanel },
    { id: "help", group: "Help", label: "Help & shortcuts", hint: "F1", run: openHelp },
  ];

  // File X-ray regions + table-jump commands, from cached analysis of the active file.
  const analysis = activeId ? analyses[activeId] : null;
  const activeSourceRefreshEpoch = activeTab
    ? sourceRefreshEpochs[activeTab.fileId] ?? 0
    : 0;
  const xrayRegions: XRayRegion[] = [];
  if (analysis && activeTab) {
    const sorted = analysis.tables
      .map((t) => ({ off: t.createOffset >= 0 ? t.createOffset : t.insertOffset, name: t.name, bytes: t.bytes }))
      .filter((t) => t.off >= 0)
      .sort((x, y) => x.off - y.off);
    const visibleRegions = sorted.length <= MAX_XRAY_REGIONS
      ? sorted
      : Array.from({ length: MAX_XRAY_REGIONS }, (_, index) => sorted[Math.floor((index * sorted.length) / MAX_XRAY_REGIONS)]);
    for (let i = 0; i < visibleRegions.length; i++) {
      const t = visibleRegions[i];
      const end = t.bytes > 0 ? t.off + t.bytes : visibleRegions[i + 1]?.off ?? activeTab.meta.size;
      xrayRegions.push({ start: t.off, end, name: t.name });
    }
  }
  const seekFileId = activeTab?.fileId ?? null;
  const seekTo = (byte: number) => {
    if (seekFileId) void seekActiveFile(seekFileId, byte);
  };
  if (analysis) {
    for (const t of boundedPaletteTables(analysis.tables)) {
      const off = t.createOffset >= 0 ? t.createOffset : t.insertOffset;
      if (off >= 0) {
        commands.push({ id: "tbl:" + t.name, group: "Table", label: "Jump to " + t.name, hint: fmtBytes(t.bytes), run: () => seekTo(off) });
      }
    }
  }

  const modalOpen = paletteOpen || helpOpen || closeReview !== null || closeDecision !== null;
  const notification = notificationState.current;

  return (
    <div className="q-app" data-file-drop-target>
      <div className="q-workbench" {...modalBackgroundAttributes(modalOpen)}>
      <header className="q-top">
        <div className="q-brand">
          <img className="q-brand-mark" src="/logos/symbol.png" alt="Quarry" />
          <span className="q-brand-name">Quarry</span>
        </div>
        <MenuBar menus={menus} />
        <div className="q-spacer" />
        {hasFiles && (
          <>
            <button className="q-btn" title={`Find (${primaryShortcut("F")})`} onClick={openSearch}>Find</button>
            {activeTab?.meta.editable && (
              <button
                className={"q-btn" + (editMode ? " q-btn-on" : "")}
                aria-label="Edit mode"
                aria-pressed={editMode}
                title="Toggle editing"
                disabled={activeSourceSyncPending}
                onClick={() => void toggleEdit()}
              >
                {editMode ? "Editing" : "Edit"}
              </button>
            )}
            {hasEdits && (
              <>
                <button
                  className={"q-btn" + (diffOpen ? " q-btn-on" : "")}
                  aria-label="Staged-edit diff panel"
                  aria-pressed={diffOpen}
                  onClick={() => void toggleDiff()}
                >
                  Diff ({editCount})
                </button>
                <button className="q-btn q-btn-primary" disabled={busy} title="Stream a full edited copy to a new file" onClick={() => void saveCopy()}>
                  Save copy…
                </button>
              </>
            )}
          </>
        )}
        <button className="q-btn q-btn-primary" onClick={() => void openViaDialog()} disabled={busy}>
          {busy ? "Working…" : "Open file"}
        </button>
      </header>

      {notification && (
        <section
          className={`q-notification q-notification-${notification.severity}`}
          role={notification.severity === "error" || notification.severity === "warning" ? "alert" : "status"}
          aria-live={notification.severity === "error" || notification.severity === "warning" ? "assertive" : "polite"}
          aria-atomic="true"
          aria-label={`${notification.operation} ${notification.severity} notification`}
        >
          <div className="q-notification-body">
            <div className="q-notification-context">
              <strong>{notification.operation}</strong>
              {notification.path && <span title={notification.path}>{notification.path}</span>}
            </div>
            <div className="q-notification-message">{notification.message}</div>
            {notification.details && (
              <details className="q-notification-details">
                <summary>Details</summary>
                <pre>{notification.details}</pre>
              </details>
            )}
          </div>
          <div className="q-notification-actions">
            <button type="button" className="q-btn" onClick={() => void copyNotificationDetails()}>
              Copy details
            </button>
            {notification.dismissible && (
              <button
                type="button"
                className="q-btn"
                onClick={() => applyNotificationAction({
                  type: "dismiss",
                  operationId: notification.operationId,
                  sequence: notification.sequence,
                })}
              >
                Dismiss
              </button>
            )}
          </div>
        </section>
      )}

      {job && (
        <div className="q-job" role="status" aria-live="polite">
          <span className="q-job-spin" />
          <span className="q-job-title">{job.title}…</span>
          <span className="q-job-count">{jobProgressLabel(job)}{job.note ? ` · ${job.note}` : ""}</span>
          <span className="q-spacer" />
          <button className="q-btn" onClick={() => void cancelJob(job.id)}>Cancel</button>
        </div>
      )}

      <div className="q-body">
        {hasFiles && (
          <Sidebar
            tabs={tabs.map((tab) => ({
              ...tab,
              pendingEdits:
                (tabEditStates[tab.fileId]?.dirty ?? false) ||
                (tabEditStates[tab.fileId]?.staging?.editCount ?? 0) > 0,
            }))}
            activeId={activeId}
            collapsed={sidebarCollapsed}
            onActivate={(id) => void activate(id)}
            onClose={(id, e) => void closeTab(id, e)}
            onToggle={() => setSidebarCollapsed((v) => !v)}
          />
        )}
        <div className="q-stage">
          <div
            className={"q-editor" + (xrayOn && hasFiles ? " q-editor-xray" : "")}
            ref={hostRef}
            tabIndex={-1}
          />

          {xrayOn && activeTab && status && (
            <XRay
              key={`xray:${activeTab.fileId}:${activeSourceRefreshEpoch}`}
              size={activeTab.meta.size}
              regions={xrayRegions}
              vpStart={status.startByte}
              vpEnd={status.endByte}
              onSeek={seekTo}
            />
          )}

        {gridView && activeTab && ["csv", "tsv"].includes(activeTab.meta.detected.toLowerCase()) && (
          <PanelErrorBoundary
            key={`csv-grid:${activeTab.fileId}:${activeSourceRefreshEpoch}`}
            panelName="CSV grid"
            resetKey={`${activeTab.fileId}:${activeSourceRefreshEpoch}`}
            onClose={() => setGridView(false)}
            onRetry={() => retryLazyPanel("csv")}
            onError={activePanelError}
          >
            <Suspense fallback={<div className="q-panel-loading" role="status">Loading CSV grid…</div>}>
              <CsvGrid
                key={`${activeTab.fileId}:${activeSourceRefreshEpoch}`}
                fileId={activeTab.fileId}
                detected={activeTab.meta.detected}
                dialect={csvDialects[activeTab.fileId]}
                onDialectChange={(dialect) => updateCsvDialect(activeTab.fileId, dialect)}
                onError={activePanelError}
              />
            </Suspense>
          </PanelErrorBoundary>
        )}

        {hexView && activeTab && (
          <PanelErrorBoundary
            key={`hex-view:${activeTab.fileId}:${activeSourceRefreshEpoch}`}
            panelName="Hex view"
            resetKey={`${activeTab.fileId}:${activeSourceRefreshEpoch}`}
            onClose={() => setHexView(false)}
            onRetry={() => retryLazyPanel("hex")}
            onError={activePanelError}
          >
            <Suspense fallback={<div className="q-panel-loading" role="status">Loading hex view…</div>}>
              <HexView
                key={`${activeTab.fileId}:${activeSourceRefreshEpoch}`}
                fileId={activeTab.fileId}
                onError={activePanelError}
              />
            </Suspense>
          </PanelErrorBoundary>
        )}

        {searchOpen && hasFiles && (
          <div className="q-find" role="search" aria-label="Find in current file">
            <input
              className="q-find-input"
              autoFocus
              placeholder="Find…"
              aria-label="Find text"
              value={query}
              maxLength={regex ? SEARCH_REGEX_INPUT_MAX_BYTES : SEARCH_PLAIN_INPUT_MAX_BYTES}
              spellCheck={false}
              onChange={(e) => updateSearchQuery(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === "Enter") { e.preventDefault(); void runFind(e.shiftKey ? "prev" : "next"); }
                else if (e.key === "Escape") closeSearch();
              }}
            />
            <button type="button" className={"q-toggle" + (caseSensitive ? " on" : "")} aria-label="Match case" aria-pressed={caseSensitive} title="Match case (case-insensitive plain search supports ASCII terms only)" onClick={() => { invalidateSearch(); setCaseSensitive((v) => !v); }}>Aa</button>
            <button type="button" className={"q-toggle" + (wholeWord ? " on" : "")} aria-label="Match whole words" aria-pressed={wholeWord} title="Whole word (UTF-8 plain text only)" onClick={() => { invalidateSearch(); setWholeWord((v) => !v); }}>W</button>
            <button type="button" className={"q-toggle" + (regex ? " on" : "")} aria-label="Use regular expression" aria-pressed={regex} title="Regular expression" onClick={toggleSearchRegex}>.*</button>
            <button type="button" className="q-icon" aria-label="Find previous match" title="Previous (Shift+Enter)" disabled={searching} onClick={() => void runFind("prev")}>↑</button>
            <button type="button" className="q-icon" aria-label="Find next match" title="Next (Enter)" disabled={searching} onClick={() => void runFind("next")}>↓</button>
            <button type="button" className="q-icon" aria-label="List up to 1,000 matches" title="List up to 1,000 matches" disabled={searching} onClick={() => void runSearchAll()}>≡</button>
            {activeSearchRequestId && (
              <button type="button" className="q-icon" aria-label="Cancel active search" title="Cancel search" onClick={() => cancelActiveSearch(activeSearchRequestId)}>Stop</button>
            )}
            <button type="button" className="q-icon" aria-label="Extract all regular-expression matches to a new file" title={regex ? "Extract all regex matches → file" : "Turn on regular-expression search to extract matches"} disabled={busy || searching || !regex || !query.trim()} onClick={() => void harvest()}>⤓</button>
            <span className="q-find-info" role="status" aria-live="polite">{searchInfo}</span>
            <button type="button" className="q-icon" aria-label="Close find" title="Close (Esc)" onClick={closeSearch}>×</button>
          </div>
        )}

        {searchOpen && results && hasFiles && (
          <section className="q-results" role="region" aria-labelledby="q-search-results-title">
            <div className="q-results-head">
              <span id="q-search-results-title">{resultsInfo || "Search results"}</span>
              <span className="q-spacer" />
              <button type="button" className="q-icon" aria-label="Close search results" title="Close search results" onClick={invalidateSearch}>×</button>
            </div>
            <div className="q-results-body" role="list" aria-label="Search matches" data-action-list>
              {results.length === 0 && <div className="q-results-empty" role="listitem">{resultsInfo || "No matches"}</div>}
              {results.map((r, i) => (
                <div key={`${r.offset}:${i}`} role="listitem">
                  <button
                    type="button"
                    className="q-result q-result-button"
                    data-list-action="true"
                    aria-label={`Go to match on line ${r.line}, byte ${r.offset}: ${snippet(r.preview)}`}
                    title={`Byte offset 0x${r.offset.toString(16)}`}
                    onKeyDown={navigateActionList}
                    onClick={() => void showListedMatch(r)}
                  >
                    <span className="q-result-line">{r.line}</span>
                    <span className="q-result-text">{r.preview}</span>
                  </button>
                </div>
              ))}
            </div>
          </section>
        )}

        {bookmarksOpen && activeTab && (
          <section className="q-results q-bookmarks" role="region" aria-labelledby="q-bookmarks-title">
            <div className="q-results-head">
              <span id="q-bookmarks-title">Bookmarks · {(bookmarks[activeTab.meta.path] ?? []).length}</span>
              <span className="q-spacer" />
              <button type="button" className="q-icon" aria-label="Add bookmark at current position" title="Add current position" onClick={addBookmark}>＋</button>
              <button type="button" className="q-icon" aria-label="Close bookmarks" title="Close bookmarks" onClick={() => setBookmarksOpen(false)}>×</button>
            </div>
            <div className="q-results-body" role="list" aria-label="Bookmarks in current file" data-action-list>
              {(bookmarks[activeTab.meta.path] ?? []).length === 0 && (
                <div className="q-results-empty" role="listitem">No bookmarks. Use ＋ or “Add bookmark here”.</div>
              )}
              {(bookmarks[activeTab.meta.path] ?? []).map((b, i) => (
                <div key={`${b.offset}:${i}`} className="q-result" role="listitem" title={`0x${b.offset.toString(16)}`}>
                  <button
                    type="button"
                    className="q-result-text q-bookmark-seek"
                    data-list-action="true"
                    aria-label={`Go to bookmark ${b.label}, byte ${b.offset}`}
                    onKeyDown={navigateActionList}
                    onClick={() => void seekActiveFile(activeTab.fileId, b.offset)}
                  >
                    {b.label} · 0x{b.offset.toString(16)}
                  </button>
                  <button type="button" className="q-icon" aria-label={`Remove bookmark ${b.label}`} title="Remove bookmark" onClick={() => removeBookmark(activeTab.meta.path, b.offset)}>×</button>
                </div>
              ))}
            </div>
          </section>
        )}

        {gotoOpen && hasFiles && (
          <div className="q-find q-goto" role="region" aria-label="Go to file position">
            <input
              className="q-find-input"
              autoFocus
              placeholder="Go to line, 0xHEX byte offset, or NN%"
              aria-label="Line, byte offset, or percentage"
              value={gotoValue}
              maxLength={GOTO_POSITION_INPUT_MAX_CODE_UNITS}
              spellCheck={false}
              onChange={(e) => {
                requestGateRef.current.invalidateChannel("goto");
                if (e.target.value.length > GOTO_POSITION_INPUT_MAX_CODE_UNITS) {
                  setSearchInfo(`Position input is limited to ${GOTO_POSITION_INPUT_MAX_CODE_UNITS} characters`);
                  return;
                }
                setGotoValue(e.target.value);
                setSearchInfo("");
              }}
              onKeyDown={(e) => {
                if (e.key === "Enter") { e.preventDefault(); void doGoto(); }
                else if (e.key === "Escape") { requestGateRef.current.invalidateChannel("goto"); setGotoOpen(false); }
              }}
            />
            <button type="button" className="q-icon" aria-label="Go to position" onClick={() => void doGoto()}>Go</button>
            <span className="q-find-info" role="status" aria-live="polite">{searchInfo}</span>
            <button type="button" className="q-icon" aria-label="Close go to" onClick={() => { requestGateRef.current.invalidateChannel("goto"); setGotoOpen(false); }}>×</button>
          </div>
        )}

        {diffOpen && hasFiles && (
          <div className={"q-diff" + (diffMode === "side" ? " q-diff-wide" : "")}>
            <div className="q-diff-head">
              <span>Staged edits ({stagedEdits.length})</span>
              <span className="q-spacer" />
              <button type="button" className={"q-toggle" + (diffMode === "list" ? " on" : "")} aria-label="Show staged edits as a list" aria-pressed={diffMode === "list"} onClick={() => setDiffMode("list")}>List</button>
              <button type="button" className={"q-toggle" + (diffMode === "side" ? " on" : "")} aria-label="Show staged edits side by side" aria-pressed={diffMode === "side"} onClick={() => setDiffMode("side")}>Side</button>
              <button type="button" className="q-icon" aria-label="Close staged-edit diff" onClick={closeDiff}>×</button>
            </div>
            {diffMode === "side" ? (
              <PanelErrorBoundary
                panelName="Side-by-side diff"
                resetKey={`${activeId ?? ""}:${activeSourceRefreshEpoch}:${status?.startByte ?? 0}`}
                onClose={closeDiff}
                onRetry={() => retryLazyPanel("diff")}
                onError={activePanelError}
              >
                <Suspense fallback={<div className="q-panel-loading" role="status">Loading side-by-side diff…</div>}>
                  <DiffView
                    key={(activeId ?? "") + ":" + (status?.startByte ?? 0)}
                    fileId={activeId ?? ""}
                    startByte={status?.startByte ?? 0}
                    theme={theme}
                    onError={activePanelError}
                  />
                </Suspense>
              </PanelErrorBoundary>
            ) : (
              <div className="q-diff-body">
                {stagedEdits.length === 0 ? (
                  <div className="q-diff-empty">No staged edits yet. Turn on Edit and change the text.</div>
                ) : (
                  stagedEdits.map((e, i) => (
                    <div className="q-diff-item" key={i}>
                      <div className="q-diff-loc">line ~{e.line} · 0x{e.start.toString(16)}</div>
                      <div className="q-diff-old">- {snippet(e.old)}</div>
                      <div className="q-diff-new">+ {snippet(e.new)}</div>
                    </div>
                  ))
                )}
              </div>
            )}
          </div>
        )}

        {toolsOpen && activeTab && (isCsv || isSql) && (
          <PanelErrorBoundary
            key={`data-tools:${activeTab.fileId}:${detected}:${activeSourceRefreshEpoch}`}
            panelName="Data tools"
            resetKey={`${activeTab.fileId}:${detected}:${activeSourceRefreshEpoch}`}
            onClose={closeToolsPanel}
            onRetry={() => retryLazyPanel("tools")}
            onError={activePanelError}
          >
            <Suspense fallback={<div className="q-panel-loading q-panel-loading-tools" role="status">Loading data tools…</div>}>
              <Tools
                key={`${activeTab.fileId}:${detected}:${activeSourceRefreshEpoch}`}
                fileId={activeTab.fileId}
                detected={activeTab.meta.detected}
                dialect={csvDialects[activeTab.fileId]}
                onDialectChange={(dialect) => updateCsvDialect(activeTab.fileId, dialect)}
                analysis={activeId ? analyses[activeId] ?? null : null}
                otherFiles={tabs.filter((t) => t.fileId !== activeTab.fileId).map((t) => ({ id: t.fileId, name: t.meta.path.replace(/^.*[\\/]/, ""), detected: t.meta.detected }))}
                onAnalyze={analyzeActiveView}
                activeJob={job}
                onCancelJob={cancelJob}
                onOperationStart={activeViewStart}
                onOperationComplete={activeViewComplete}
                onWorkbenchBusyChange={setOperationBusy}
                onNotice={activeViewNotice}
                onError={activeViewOwnedError}
                onClose={closeToolsPanel}
              />
            </Suspense>
          </PanelErrorBoundary>
        )}

        {!hasFiles && (
          <div className="q-empty">
            <img className="q-empty-logo" src="/logos/symbol.png" alt="" />
            <h1 className="q-empty-title">Quarry</h1>
            <p className="q-empty-tag">Open and edit very large files — SQL dumps, CSVs, logs.</p>
            <button className="q-btn q-btn-primary q-empty-open" onClick={() => void openViaDialog()} disabled={busy}>
              Open file…
            </button>
            <div className="q-empty-path">
              <input
                className="q-path"
                placeholder="…or paste a path, e.g. E:\dumps\big.sql"
                value={path}
                maxLength={SOURCE_PATH_INPUT_MAX_CODE_UNITS}
                aria-describedby={pathInputInfo ? "q-path-input-info" : undefined}
                spellCheck={false}
                onChange={(e) => {
                  if (e.target.value.length > SOURCE_PATH_INPUT_MAX_CODE_UNITS) {
                    setPathInputInfo(`Source paths are limited to ${SOURCE_PATH_INPUT_MAX_CODE_UNITS} characters.`);
                    return;
                  }
                  setPath(e.target.value);
                  setPathInputInfo("");
                }}
                onKeyDown={(e) => {
                  if (e.key === "Enter") void openPath();
                }}
              />
              <button className="q-btn" onClick={() => void openPath()} disabled={busy || !hasSourcePathInput(path)}>
                Open
              </button>
            </div>
            {pathInputInfo && <p id="q-path-input-info" className="q-empty-hint" role="status">{pathInputInfo}</p>}
            <p className="q-empty-hint">The editor streams bounded windows as you scroll.</p>
          </div>
        )}
        </div>
      </div>

      <footer className="q-status">
        {status && activeTab ? (
          <>
            <span className="q-file" title={activeTab.meta.path}>{activeTab.meta.path}</span>
            <span className="q-sep" />
            <span>{fmtBytes(activeTab.meta.size)}</span>
            <span>{activeTab.meta.encoding || "?"}</span>
            <span>{activeTab.meta.detected || "plain"}</span>
            <span className="q-spacer" />
            {following && <span className="q-edit-flag" title="Following file tail">● live</span>}
            {editMode && <span className="q-edit-flag">{dirty ? "● editing" : "editing"}</span>}
            {hasEdits && (
              <span className="q-edit-flag">
                {editCount} staged · save copy{staging && staging.netDelta !== 0 ? " · len±" + staging.netDelta : ""}
              </span>
            )}
            <span>0x{status.startByte.toString(16)}–0x{status.endByte.toString(16)}</span>
            <span>lines {status.firstLine}–{status.lastLine}{status.approx ? " ~" : ""}</span>
            {status.atBof && <span className="q-edge">BOF</span>}
            {status.atEof && <span className="q-edge">EOF</span>}
          </>
        ) : (
          <span className="q-muted">Ready</span>
        )}
      </footer>
      </div>

      {paletteOpen && (
        <CommandPalette
          commands={commands}
          onClose={closeCommandPalette}
          returnFocusTo={paletteReturnFocusRef.current}
        />
      )}
      {helpOpen && (
        <PanelErrorBoundary
          panelName="Help"
          resetKey="help"
          onClose={closeHelp}
          onRetry={() => retryLazyPanel("help")}
          modal
          returnFocusTo={helpReturnFocusRef.current}
        >
          <Suspense fallback={(
            <LoadingHelpDialog
              onClose={closeHelp}
              returnFocusTo={helpReturnFocusRef.current}
            />
          )}>
            <Help onClose={closeHelp} returnFocusTo={helpReturnFocusRef.current} />
          </Suspense>
        </PanelErrorBoundary>
      )}
      {closeReview && !closeDecision && (
        <div className="q-close-backdrop" role="presentation">
          <section
            ref={closeReviewDialogRef}
            className="q-close-dialog q-close-progress"
            role="dialog"
            aria-modal="true"
            aria-labelledby="q-close-review-title"
            aria-describedby="q-close-review-description"
            aria-live="polite"
            tabIndex={-1}
          >
            <h2 id="q-close-review-title">{closeReview === "application" ? "Checking open files before exit" : "Checking tab before close"}</h2>
            <p id="q-close-review-description">Quarry is verifying pending editor and staging state. The close remains blocked until this check completes.</p>
          </section>
        </div>
      )}
      {closeDecision && <UnsavedChangesDialog context={closeDecision} onChoose={chooseCloseDecision} />}
    </div>
  );
}

export default App;
