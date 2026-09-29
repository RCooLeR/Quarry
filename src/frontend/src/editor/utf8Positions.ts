/**
 * Maps exact UTF-8 byte boundaries in one bounded edit window to UTF-16.
 * A single pass avoids creating a TextEncoder buffer for every code point,
 * including the common case of a match near the end of a megabyte window.
 */
export function utf8ByteRangeToCodeUnits(
  text: string,
  offset: number,
  length: number,
): { from: number; to: number } | null {
  const end = offset + length;
  if (!Number.isSafeInteger(offset) || !Number.isSafeInteger(length)
      || !Number.isSafeInteger(end) || offset < 0 || length < 0) return null;

  let bytes = 0;
  let codeUnits = 0;
  let from: number | null = null;
  while (true) {
    if (bytes === offset) from = codeUnits;
    if (bytes === end) return from === null ? null : { from, to: codeUnits };
    if (bytes > end || (bytes > offset && from === null) || codeUnits === text.length) return null;

    const scalar = text.codePointAt(codeUnits)!;
    // Lone surrogates occupy three UTF-8 bytes, matching TextEncoder's U+FFFD.
    bytes += scalar <= 0x7f ? 1 : scalar <= 0x7ff ? 2 : scalar <= 0xffff ? 3 : 4;
    codeUnits += scalar > 0xffff ? 2 : 1;
  }
}
