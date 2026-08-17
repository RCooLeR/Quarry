export type CsvDialectOrigin = "fallback" | "detected" | "override";

export interface CsvDialect {
  delimiter: string;
  hasHeader: boolean;
  origin: CsvDialectOrigin;
}

export function isCsvType(detected: string): boolean {
  const value = detected.toLowerCase();
  return value === "csv" || value === "tsv";
}

export function fallbackCsvDialect(detected: string): CsvDialect {
  return {
    delimiter: detected.toLowerCase() === "tsv" ? "\t" : ",",
    hasHeader: false,
    origin: "fallback",
  };
}

export function isValidCsvDelimiter(delimiter: string): boolean {
  return Array.from(delimiter).length === 1 && delimiter !== "\0" && delimiter !== "\r" && delimiter !== "\n";
}

export function detectedCsvDialect(
  detected: string,
  delimiter: string,
  hasHeader: boolean,
): CsvDialect {
  const fallback = fallbackCsvDialect(detected);
  return {
    delimiter: isValidCsvDelimiter(delimiter) ? delimiter : fallback.delimiter,
    hasHeader,
    origin: "detected",
  };
}

export function overrideCsvDialect(delimiter: string, hasHeader: boolean): CsvDialect {
  if (!isValidCsvDelimiter(delimiter)) {
    throw new Error("CSV separator must be exactly one non-NUL, non-newline character");
  }
  return { delimiter, hasHeader, origin: "override" };
}

/** Keep an explicit user choice when a slower detector response arrives. */
export function mergeCsvDetection(current: CsvDialect | undefined, detected: CsvDialect): CsvDialect {
  return current?.origin === "override" ? current : detected;
}
