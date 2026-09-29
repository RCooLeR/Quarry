// Source paths are opaque filesystem identifiers. Whitespace can be a valid
// part of POSIX and Windows names, so UI validation must never normalize it.
export const SOURCE_PATH_INPUT_MAX_CODE_UNITS = 32_767;

export function hasSourcePathInput(value: string): boolean {
  return value.length > 0;
}

export function isBoundedSourcePathInput(value: string): boolean {
  return hasSourcePathInput(value) && value.length <= SOURCE_PATH_INPUT_MAX_CODE_UNITS;
}
