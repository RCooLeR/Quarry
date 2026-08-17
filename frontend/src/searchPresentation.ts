export interface SearchPageLike<T> {
  hits?: T[] | null;
  truncated?: boolean;
  timedOut?: boolean;
}

export interface SearchPagePresentation<T> {
  hits: T[] | null;
  info: string;
}

// A timed-out whole-file scan is not a complete result set. Do not replace the
// timeout with an ordinary match count or render partial data as authoritative.
export function presentSearchPage<T>(page: SearchPageLike<T>): SearchPagePresentation<T> {
  const hits = page.hits ?? [];
  if (page.timedOut) {
    return {
      hits: null,
      info: hits.length > 0
        ? `Timed out — ${hits.length} partial matches were not shown`
        : "Timed out — narrow the query",
    };
  }
  return {
    hits,
    info: `${hits.length}${page.truncated ? "+" : ""} matches`,
  };
}
