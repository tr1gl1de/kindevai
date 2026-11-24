/** Date and time in the UI language, e.g. "24 Nov 2026, 15:00" / "24 нояб. 2026 г., 15:00". */
export function formatDateTime(iso: string, lng: string): string {
  return new Intl.DateTimeFormat(lng, { dateStyle: "medium", timeStyle: "short" }).format(new Date(iso));
}
