import { afterEach, describe, expect, it } from "vitest";
import { installNavigationPolicy, isAllowedDocumentNavigation } from "../src/navigationPolicy";

describe("privileged WebView navigation policy", () => {
  const currentURL = "https://wails.localhost/workbench?mode=desktop";
  let uninstall: (() => void) | null = null;

  afterEach(() => {
    uninstall?.();
    uninstall = null;
    document.body.replaceChildren();
  });

  it("allows only fragments in the exact current document", () => {
    expect(isAllowedDocumentNavigation("#help", currentURL)).toBe(true);
    expect(isAllowedDocumentNavigation("/workbench?mode=desktop#help", currentURL)).toBe(true);
    expect(isAllowedDocumentNavigation("/other", currentURL)).toBe(false);
    expect(isAllowedDocumentNavigation("https://example.com/", currentURL)).toBe(false);
    expect(isAllowedDocumentNavigation("javascript:alert(1)", currentURL)).toBe(false);
    expect(isAllowedDocumentNavigation("data:text/html,unsafe", currentURL)).toBe(false);
    expect(isAllowedDocumentNavigation("not a valid % url", currentURL)).toBe(false);
  });

  it("blocks external, new-window, download, and form navigation in capture phase", () => {
    const windowTarget = { location: { href: currentURL } } as unknown as Window;
    uninstall = installNavigationPolicy(document, windowTarget);

    const external = document.createElement("a");
    external.href = "https://example.com/";
    external.textContent = "external";
    document.body.append(external);
    const externalClick = new MouseEvent("click", { bubbles: true, cancelable: true });
    expect(external.dispatchEvent(externalClick)).toBe(false);
    expect(externalClick.defaultPrevented).toBe(true);

    const fragment = document.createElement("a");
    fragment.href = "#help";
    document.body.append(fragment);
    const fragmentClick = new MouseEvent("click", { bubbles: true, cancelable: true });
    expect(fragment.dispatchEvent(fragmentClick)).toBe(true);
    expect(fragmentClick.defaultPrevented).toBe(false);

    const middleClick = new MouseEvent("auxclick", { bubbles: true, cancelable: true, button: 1 });
    expect(fragment.dispatchEvent(middleClick)).toBe(false);
    expect(middleClick.defaultPrevented).toBe(true);

    const modifiedClick = new MouseEvent("click", { bubbles: true, cancelable: true, ctrlKey: true });
    expect(fragment.dispatchEvent(modifiedClick)).toBe(false);
    expect(modifiedClick.defaultPrevented).toBe(true);

    fragment.target = "_blank";
    const newWindowClick = new MouseEvent("click", { bubbles: true, cancelable: true });
    expect(fragment.dispatchEvent(newWindowClick)).toBe(false);

    fragment.target = "";
    fragment.download = "export.txt";
    const downloadClick = new MouseEvent("click", { bubbles: true, cancelable: true });
    expect(fragment.dispatchEvent(downloadClick)).toBe(false);

    const form = document.createElement("form");
    document.body.append(form);
    const submit = new SubmitEvent("submit", { bubbles: true, cancelable: true });
    expect(form.dispatchEvent(submit)).toBe(false);
    expect(submit.defaultPrevented).toBe(true);

    const contextMenu = new MouseEvent("contextmenu", { bubbles: true, cancelable: true });
    expect(document.body.dispatchEvent(contextMenu)).toBe(false);
    expect(contextMenu.defaultPrevented).toBe(true);

    uninstall();
    uninstall = null;
    const afterUninstall = new MouseEvent("contextmenu", { bubbles: true, cancelable: true });
    expect(document.body.dispatchEvent(afterUninstall)).toBe(true);
    expect(afterUninstall.defaultPrevented).toBe(false);
  });
});
