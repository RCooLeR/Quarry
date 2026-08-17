/**
 * Global per-render ceiling for cosmetic syntax-highlight decorations.
 *
 * Editor windows are byte-bounded, but a delimiter/token-dense visible line
 * can still contain hundreds of thousands of ranges. Every custom highlighter
 * shares this ceiling so highlighting cannot amplify one window into an
 * unbounded RangeSet or DOM surface.
 */
export const MAX_EDITOR_HIGHLIGHT_RANGES = 4_096;
