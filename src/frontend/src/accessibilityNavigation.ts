import type { KeyboardEvent as ReactKeyboardEvent } from "react";

export function primaryShortcut(key: string): string {
  return `Ctrl/Cmd+${key.toUpperCase()}`;
}

export function modalBackgroundAttributes(active: boolean): {
  inert?: true;
  "aria-hidden"?: true;
} {
  return active ? { inert: true, "aria-hidden": true } : {};
}

/** Roving focus for bounded search/bookmark action lists. */
export function navigateActionList(event: ReactKeyboardEvent<HTMLButtonElement>): void {
  let targetIndex: number | undefined;
  const list = event.currentTarget.closest<HTMLElement>("[data-action-list]");
  if (!list) return;
  const actions = Array.from(list.querySelectorAll<HTMLButtonElement>('button[data-list-action="true"]:not(:disabled)'));
  const currentIndex = actions.indexOf(event.currentTarget);
  if (currentIndex < 0 || actions.length === 0) return;

  switch (event.key) {
    case "ArrowDown":
      targetIndex = (currentIndex + 1) % actions.length;
      break;
    case "ArrowUp":
      targetIndex = (currentIndex - 1 + actions.length) % actions.length;
      break;
    case "Home":
      targetIndex = 0;
      break;
    case "End":
      targetIndex = actions.length - 1;
      break;
    default:
      return;
  }
  event.preventDefault();
  actions[targetIndex].focus();
}
