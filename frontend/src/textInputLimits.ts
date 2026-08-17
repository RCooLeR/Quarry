/** Backend-aligned UTF-8 byte ceilings for user-supplied search text. */
export const SEARCH_PLAIN_INPUT_MAX_BYTES = 1024 * 1024;
export const SEARCH_REGEX_INPUT_MAX_BYTES = 64 * 1024;
export const SQL_FIND_INPUT_MAX_BYTES = 64 * 1024;
export const SQL_REPLACEMENT_INPUT_MAX_BYTES = 256 * 1024;

export interface BoundedUtf8Text {
  value: string;
  truncated: boolean;
}

/**
 * Keeps the longest scalar-aligned prefix whose UTF-8 representation fits in
 * maxBytes. The scan stops at the ceiling, so an oversized paste does not
 * require encoding or traversing the entire value.
 */
export function boundedUtf8Text(value: string, maxBytes: number): BoundedUtf8Text {
  if (!Number.isSafeInteger(maxBytes) || maxBytes < 0) {
    throw new RangeError("maxBytes must be a non-negative safe integer");
  }

  let bytes = 0;
  let index = 0;
  while (index < value.length) {
    const first = value.charCodeAt(index);
    let codeUnits = 1;
    let encodedBytes: number;

    if (first <= 0x7f) {
      encodedBytes = 1;
    } else if (first <= 0x7ff) {
      encodedBytes = 2;
    } else if (first >= 0xd800 && first <= 0xdbff) {
      const second = value.charCodeAt(index + 1);
      if (second >= 0xdc00 && second <= 0xdfff) {
        codeUnits = 2;
        encodedBytes = 4;
      } else {
        // TextEncoder and the backend JSON decoder both replace an unpaired
        // surrogate with U+FFFD, whose UTF-8 representation is three bytes.
        encodedBytes = 3;
      }
    } else {
      encodedBytes = 3;
    }

    if (bytes + encodedBytes > maxBytes) {
      return { value: value.slice(0, index), truncated: true };
    }
    bytes += encodedBytes;
    index += codeUnits;
  }

  return { value, truncated: false };
}
