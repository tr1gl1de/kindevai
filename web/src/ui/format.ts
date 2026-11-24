/** Date and time in the UI language, e.g. "24 Nov 2026, 15:00" / "24 нояб. 2026 г., 15:00". */
export function formatDateTime(iso: string, lng: string): string {
  return new Intl.DateTimeFormat(lng, { dateStyle: "medium", timeStyle: "short" }).format(new Date(iso));
}

/** Bytes in decimal units of the UI language, e.g. "4 GB" / "4 ГБ". */
export function formatBytes(bytes: number, lng: string): string {
  const units = [
    ["terabyte", 1e12],
    ["gigabyte", 1e9],
    ["megabyte", 1e6],
  ] as const;
  const [unit, size] = units.find(([, s]) => bytes >= s) ?? units[2];
  return new Intl.NumberFormat(lng, { style: "unit", unit, maximumFractionDigits: 1 }).format(bytes / size);
}
