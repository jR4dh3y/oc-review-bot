// Single source for date/duration/count formatting. Hand-rolled with Intl and
// Date — no date dependency. All helpers tolerate invalid input with a stable
// fallback instead of throwing.

const ABSOLUTE_FORMAT = new Intl.DateTimeFormat(undefined, {
  dateStyle: "medium",
  timeStyle: "short",
});

function parseDate(iso: string | null | undefined): Date | null {
  if (!iso) return null;
  const date = new Date(iso);
  return Number.isNaN(date.getTime()) ? null : date;
}

function formatDate(date: Date): string {
  return ABSOLUTE_FORMAT.format(date);
}

/** Absolute local timestamp, e.g. "12 Sept 2026, 9:16 AM". */
export function formatDateTime(iso: string | null | undefined): string {
  const date = parseDate(iso);
  return date == null ? "unknown time" : formatDate(date);
}

/** Coarse relative age, e.g. "just now", "42s ago", "3m ago", "2h ago", "5d ago". */
export function formatRelative(iso: string | null | undefined): string {
  const date = parseDate(iso);
  if (date == null) return "unknown time";
  const seconds = Math.max(0, Math.floor((Date.now() - date.getTime()) / 1000));
  if (seconds < 15) return "just now";
  if (seconds < 60) return `${seconds}s ago`;
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes}m ago`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours}h ago`;
  return `${Math.floor(hours / 24)}d ago`;
}

/** Live duration, e.g. "42s", "1m23s", "2h5m3s". `now` defaults to the current
 *  time so ticking callers can pass a re-rendered timestamp. Returns null when
 *  there is no valid start date or the start is in the future (clock skew). */
export function formatElapsed(
  iso: string | null | undefined,
  now: number = Date.now(),
): string | null {
  const date = parseDate(iso);
  if (date == null) return null;
  const seconds = Math.floor((now - date.getTime()) / 1000);
  if (seconds < 0) return null;
  if (seconds < 60) return `${seconds}s`;
  const minutes = Math.floor(seconds / 60);
  const restSeconds = seconds % 60;
  if (minutes < 60) return `${minutes}m${restSeconds}s`;
  const hours = Math.floor(minutes / 60);
  return `${hours}h${minutes % 60}m${restSeconds}s`;
}

/** "1 finding" / "3 findings". */
export function formatFindingCount(count: number): string {
  return `${count} ${count === 1 ? "finding" : "findings"}`;
}
