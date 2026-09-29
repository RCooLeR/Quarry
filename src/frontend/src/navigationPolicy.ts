function parsedURL(value: string | URL, base: string): URL | null {
  try {
    return value instanceof URL ? value : new URL(value, base);
  } catch {
    return null;
  }
}

/**
 * Classifies navigation requested by rendered anchors. Only a fragment change
 * within the exact current document is accepted; captured anchor/form events
 * for remote URLs, dangerous schemes, other paths, downloads, and new windows
 * are blocked by installNavigationPolicy. This DOM helper does not intercept
 * script-set location, meta refresh, or native top-level WebView navigation.
 */
export function isAllowedDocumentNavigation(value: string | URL, currentURL: string): boolean {
  const current = parsedURL(currentURL, currentURL);
  const target = parsedURL(value, currentURL);
  if (!current || !target) return false;
  return target.protocol === current.protocol
    && target.origin === current.origin
    && target.host === current.host
    && target.username === current.username
    && target.password === current.password
    && target.pathname === current.pathname
    && target.search === current.search
    && target.hash !== "";
}

function closestAnchor(target: EventTarget | null): HTMLAnchorElement | null {
  return target instanceof Element ? target.closest<HTMLAnchorElement>("a[href]") : null;
}

/** Installs a capture-phase anchor/form-event backstop in addition to the CSP. */
export function installNavigationPolicy(
  documentTarget: Document = document,
  windowTarget: Window = window,
): () => void {
  const blockDisallowedAnchor = (event: MouseEvent) => {
    const anchor = closestAnchor(event.target);
    if (!anchor) return;
    const href = anchor.getAttribute("href");
    const unmodifiedPrimaryClick = event.type === "click"
      && event.button === 0
      && !event.altKey
      && !event.ctrlKey
      && !event.metaKey
      && !event.shiftKey;
    const allowed = unmodifiedPrimaryClick
      && href != null
      && !anchor.hasAttribute("download")
      && (anchor.target === "" || anchor.target === "_self")
      && isAllowedDocumentNavigation(href, windowTarget.location.href);
    if (allowed) return;
    event.preventDefault();
    event.stopPropagation();
  };
  const blockFormSubmission = (event: SubmitEvent) => {
    event.preventDefault();
    event.stopPropagation();
  };
  const blockBrowserContextMenu = (event: MouseEvent) => {
    event.preventDefault();
    event.stopPropagation();
  };

  documentTarget.addEventListener("click", blockDisallowedAnchor, true);
  documentTarget.addEventListener("auxclick", blockDisallowedAnchor, true);
  documentTarget.addEventListener("submit", blockFormSubmission, true);
  // Wails' native default-menu option is not implemented uniformly by every
  // platform backend. Enforce the privileged-renderer policy in the DOM too.
  documentTarget.addEventListener("contextmenu", blockBrowserContextMenu, true);
  return () => {
    documentTarget.removeEventListener("click", blockDisallowedAnchor, true);
    documentTarget.removeEventListener("auxclick", blockDisallowedAnchor, true);
    documentTarget.removeEventListener("submit", blockFormSubmission, true);
    documentTarget.removeEventListener("contextmenu", blockBrowserContextMenu, true);
  };
}
